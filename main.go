package main

import (
	"bytes"
	"io"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/giuliog97/temple-sbx/internal/krun"
)

// MicroVM config
type Config struct {
	Rootfs string
	vCPU   uint8
	RAM    uint8
}

// crlf rewrites "\n" as "\r\n". libkrun puts the terminal in raw mode during
// Build, where a bare "\n" moves down without returning to column 0.
type crlf struct{ w io.Writer }

func (c crlf) Write(p []byte) (int, error) {
	_, err := c.w.Write(bytes.ReplaceAll(p, []byte("\n"), []byte("\r\n")))
	return len(p), err
}

func main() {
	log.SetFlags(0) // no timestamps
	log.SetPrefix("[temple-sbx] ")

	// Parse CLI arguments: ./temple-sbx [rootfs] [command] [args...]

	rootfsPath := "./rootfs_fedora"
	guestArgs := []string{"/bin/sh"}

	if len(os.Args) > 1 {
		rootfsPath = os.Args[1]
	}
	if len(os.Args) > 2 {
		guestArgs = os.Args[2:]
	}

	log.Printf("Resolving rootfs path %s...", rootfsPath)
	absRoot, err := filepath.Abs(rootfsPath)
	if err != nil {
		log.Fatalf("Invalid rootfs path: %v", err)
	}

	// Raise file descriptor limit for virtiofs
	log.Printf("Raising open file limit...")
	var rlim syscall.Rlimit
	if sysErr := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &rlim); sysErr == nil {
		rlim.Cur = rlim.Max
		_ = syscall.Setrlimit(syscall.RLIMIT_NOFILE, &rlim)
	}

	// 1. Create filesystem overlay
	log.Printf("Creating filesystem overlay...")
	overlay := krun.NewOverlay()

	// 2. Load krunfw kernel payload
	log.Printf("Loading krunfw kernel payload...")
	payload, err := krun.LoadKrunfw()
	if err != nil {
		log.Fatalf("Failed to load krunfw payload: %v", err)
	}

	// 3. Build init configuration (identical to chroot_vm.c).
	// initConfig must stay alive until the VM exits.
	log.Printf("Configuring init to run %v...", guestArgs)
	initConfig := krun.NewInitConfig(krun.InitOptions{
		Args:    guestArgs,
		Workdir: "/",
	})
	if err := initConfig.Apply(overlay, payload); err != nil {
		log.Fatalf("Failed to apply init config: %v", err)
	}

	// 4. Create rootfs device and attach overlay
	log.Printf("Sharing rootfs %s as /dev/root...", absRoot)
	rootfs, err := krun.NewFsDevice("/dev/root", absRoot)
	if err != nil {
		log.Fatalf("Failed to create rootfs device for %s: %v", absRoot, err)
	}
	rootfs.SetOverlay(overlay)

	// 5. Device Manager: Console + Rootfs + RNG + Balloon (like chroot_vm)
	log.Printf("Creating console, rng and balloon devices...")
	console, err := krun.NewDefaultConsole(int(os.Stdin.Fd()), int(os.Stdout.Fd()), int(os.Stderr.Fd()))
	if err != nil {
		log.Fatalf("Failed to create console: %v", err)
	}
	rng, err := krun.NewRngDevice()
	if err != nil {
		log.Fatalf("Failed to create rng device: %v", err)
	}
	balloon, err := krun.NewBalloonDevice()
	if err != nil {
		log.Fatalf("Failed to create balloon device: %v", err)
	}

	log.Printf("Attaching devices...")
	devices := krun.NewMMIODeviceManager()
	devices.Add(console, rootfs, rng, balloon)

	// 6. Build VMM
	log.Printf("Configuring VMM (2 vCPUs, 1024 MiB RAM)...")
	builder := krun.NewVMMBuilder()
	if err := builder.VCPUs(2); err != nil {
		log.Fatalf("%v", err)
	}
	if err := builder.RAMMiB(1024); err != nil {
		log.Fatalf("%v", err)
	}
	builder.Payload(payload)
	builder.Devices(devices)
	if err := builder.SplitIRQChip(false); err != nil {
		log.Fatalf("%v", err)
	}

	log.Printf("Building VMM...")
	vmm, err := builder.Build()
	if err != nil {
		log.Fatalf("Failed to build VMM: %v", err)
	}
	log.SetOutput(crlf{os.Stderr})

	// 7. Acquire handle for Ctrl+C shutdown
	log.Printf("Installing Ctrl+C handler...")
	handle, err := vmm.Handle()
	if err != nil {
		log.Fatalf("Failed to get VMM handle: %v", err)
	}
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigChan
		if err := handle.Shutdown(); err != nil {
			log.Printf("Shutdown failed: %v", err)
		}
	}()

	log.Printf("Booting %s (Ctrl+C to exit)...", absRoot)

	// 8. Run the VMM
	vmm.Run()

	handle.Destroy()
	initConfig.Destroy()
}
