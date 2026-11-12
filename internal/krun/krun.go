// Package krun is a thin Go wrapper around the libkrun / libkrun_init C API.
//
// Ownership rules (mirroring libkrun):
//   - Passing an object into another one (payload into a VMM builder, overlay
//     into an fs device, device into a device manager, …) transfers ownership.
//     The Go value must not be used afterwards.
//   - C strings passed to libkrun are allocated in the C heap and kept alive
//     by the Go object that created them, since libkrun may borrow them until
//     the VM exits. They are freed only by an explicit Destroy.
package krun

/*
#cgo CFLAGS: -I${SRCDIR}/../../deps/libkrun/include
#cgo pkg-config: libkrun
#cgo LDFLAGS: -lkrun_init

#include <stdlib.h>
#include <libkrun.h>
#include <libkrun_init.h>
*/
import "C"

import (
	"fmt"
	"unsafe"
)

// ---------------------------------------------------------------------------
// Errors
// ---------------------------------------------------------------------------

// Error is a failed libkrun call.
type Error struct {
	Op     string // the C function that failed
	Result uint64 // raw KrunResult code
	Name   string // symbolic name of the result, e.g. "VmmError::BootError"
}

func (e *Error) Error() string {
	return fmt.Sprintf("krun: %s: %s (0x%x)", e.Op, e.Name, e.Result)
}

// vmmError builds an Error from a libkrun result and optional error handle,
// destroying the handle.
func vmmError(op string, r C.KrunResult, e C.KrunError) error {
	if e != nil {
		r = C.KrunResult(C.krun_error_result(e))
		C.krun_error_destroy(e)
	}
	if r == C.KRUN_RESULT_SUCCESS {
		// Call signalled failure (nil handle) without an error object.
		return &Error{Op: op, Name: "unknown error"}
	}
	return &Error{Op: op, Result: uint64(r), Name: C.GoString(C.krun_result_name_cstr(r))}
}

// check returns nil on success, otherwise a typed *Error.
func check(op string, r C.KrunResult, e C.KrunError) error {
	if r == C.KRUN_RESULT_SUCCESS && e == nil {
		return nil
	}
	return vmmError(op, r, e)
}

// initError is the libkrun_init equivalent of vmmError.
func initError(op string, r C.KrunResult, e C.KrunInitError) error {
	if e != nil {
		r = C.KrunResult(C.krun_init_error_result(e))
		C.krun_init_error_destroy(e)
	}
	if r == C.KRUN_RESULT_SUCCESS {
		return &Error{Op: op, Name: "unknown error"}
	}
	return &Error{Op: op, Result: uint64(r), Name: C.GoString(C.krun_init_result_name_cstr(r))}
}

// ---------------------------------------------------------------------------
// C strings
// ---------------------------------------------------------------------------

// cstrs keeps C-heap strings alive for as long as libkrun may reference them.
type cstrs []*C.char

// str copies s into the C heap and returns a KrunStr pointing at it.
func (c *cstrs) str(s string) C.KrunStr {
	p := C.CString(s)
	*c = append(*c, p)
	return C.KrunStr{data: p, len: C.size_t(len(s))}
}

func (c *cstrs) free() {
	for _, p := range *c {
		C.free(unsafe.Pointer(p))
	}
	*c = nil
}

// ---------------------------------------------------------------------------
// Payload (guest kernel)
// ---------------------------------------------------------------------------

type Payload struct{ h C.KrunPayload }

// LoadKrunfw loads the kernel bundled in libkrunfw.
func LoadKrunfw() (*Payload, error) {
	var e C.KrunError
	h := C.krun_payload_load_krunfw(&e)
	if h == nil {
		return nil, vmmError("krun_payload_load_krunfw", 0, e)
	}
	return &Payload{h: h}, nil
}

// ---------------------------------------------------------------------------
// Filesystem overlay
// ---------------------------------------------------------------------------

// Overlay holds virtual files injected into a virtiofs device (used by init).
type Overlay struct{ h C.KrunFsOverlay }

func NewOverlay() *Overlay {
	return &Overlay{h: C.krun_fs_overlay_new()}
}

// ---------------------------------------------------------------------------
// Init configuration (libkrun_init)
// ---------------------------------------------------------------------------

// InitOptions describes the process the guest init will exec.
type InitOptions struct {
	Args    []string // argv; Args[0] is the program to run
	Env     []string // "KEY=value"
	Workdir string   // defaults to "/"
}

// InitConfig must stay alive (not destroyed) until the VM exits: Apply makes
// the overlay and payload borrow its data.
type InitConfig struct {
	h    C.KrunInitConfig
	strs cstrs
}

func NewInitConfig(opts InitOptions) *InitConfig {
	cfg := &InitConfig{}
	b := C.krun_init_config_builder()

	for _, a := range opts.Args {
		C.krun_init_builder_arg(&b, cfg.strs.str(a))
	}
	for _, v := range opts.Env {
		C.krun_init_builder_env_var(&b, cfg.strs.str(v))
	}
	wd := opts.Workdir
	if wd == "" {
		wd = "/"
	}
	C.krun_init_builder_workdir(&b, cfg.strs.str(wd))

	cfg.h = C.krun_init_builder_build(&b) // consumes the builder
	return cfg
}

// Apply injects the init binary and its config into the overlay, and appends
// the init kernel command line argument to the payload.
func (c *InitConfig) Apply(o *Overlay, p *Payload) error {
	var e C.KrunInitError
	r := C.krun_init_config_apply(c.h, o.h, p.h, &e)
	if r != C.KRUN_RESULT_SUCCESS || e != nil {
		return initError("krun_init_config_apply", r, e)
	}
	return nil
}

// Destroy frees the config. Only call it after the VM has exited.
func (c *InitConfig) Destroy() {
	if c.h != nil {
		C.krun_init_config_destroy(c.h)
		c.h = nil
	}
	c.strs.free()
}

// ---------------------------------------------------------------------------
// Devices
// ---------------------------------------------------------------------------

// Device is anything that can be attached to a DeviceManager.
type Device interface {
	attachHandle() C.KrunAttachDevice
}

// FsDevice is a virtiofs device sharing a host directory.
type FsDevice struct {
	h    C.KrunFsDevice
	strs cstrs
}

// NewFsDevice shares hostPath with the guest under tag (use "/dev/root" for
// the root filesystem).
func NewFsDevice(tag, hostPath string) (*FsDevice, error) {
	d := &FsDevice{}
	var e C.KrunError
	h := C.krun_fs_device_new(d.strs.str(tag), d.strs.str(hostPath), &e)
	if h == nil {
		d.strs.free()
		return nil, vmmError("krun_fs_device_new", 0, e)
	}
	d.h = h
	return d, nil
}

// SetOverlay attaches an overlay. Ownership of o moves to the device.
func (d *FsDevice) SetOverlay(o *Overlay) {
	C.krun_fs_device_set_overlay(d.h, o.h)
	o.h = nil
}

func (d *FsDevice) attachHandle() C.KrunAttachDevice { return C.KrunAttachDevice(d.h) }

// ConsoleDevice is a virtio console.
type ConsoleDevice struct{ h C.KrunConsoleDevice }

// NewDefaultConsole creates the default console wired to the given host fds
// (typically 0, 1, 2). If one of them is a TTY it becomes a raw-mode terminal.
// The fds are borrowed and must stay open until the VM exits.
func NewDefaultConsole(stdin, stdout, stderr int) (*ConsoleDevice, error) {
	b := C.krun_console_device_builder()

	var e C.KrunError
	r := C.krun_console_builder_add_default_console(b, C.int(stdin), C.int(stdout), C.int(stderr), &e)
	if err := check("krun_console_builder_add_default_console", r, e); err != nil {
		C.krun_console_builder_destroy(b)
		return nil, err
	}

	e = nil
	h := C.krun_console_builder_build(b, &e) // consumes the builder
	if h == nil {
		return nil, vmmError("krun_console_builder_build", 0, e)
	}
	return &ConsoleDevice{h: h}, nil
}

func (d *ConsoleDevice) attachHandle() C.KrunAttachDevice { return C.KrunAttachDevice(d.h) }

// RngDevice is a virtio entropy source for the guest.
type RngDevice struct{ h C.KrunRngDevice }

func NewRngDevice() (*RngDevice, error) {
	var e C.KrunError
	h := C.krun_rng_device_new(&e)
	if h == nil {
		return nil, vmmError("krun_rng_device_new", 0, e)
	}
	return &RngDevice{h: h}, nil
}

func (d *RngDevice) attachHandle() C.KrunAttachDevice { return C.KrunAttachDevice(d.h) }

// BalloonDevice is a virtio memory balloon (returns free guest memory to host).
type BalloonDevice struct{ h C.KrunBalloonDevice }

func NewBalloonDevice() (*BalloonDevice, error) {
	var e C.KrunError
	h := C.krun_balloon_device_new(&e)
	if h == nil {
		return nil, vmmError("krun_balloon_device_new", 0, e)
	}
	return &BalloonDevice{h: h}, nil
}

func (d *BalloonDevice) attachHandle() C.KrunAttachDevice { return C.KrunAttachDevice(d.h) }

// ---------------------------------------------------------------------------
// Device manager
// ---------------------------------------------------------------------------

// DeviceManager collects devices on the virtio-mmio transport.
type DeviceManager struct{ h C.KrunMmioDeviceManager }

func NewMMIODeviceManager() *DeviceManager {
	return &DeviceManager{h: C.krun_mmio_device_manager_new()}
}

// Add attaches devices in order. Ownership of each device moves to the manager.
func (m *DeviceManager) Add(devs ...Device) {
	for _, d := range devs {
		C.krun_mmio_device_manager_add(m.h, d.attachHandle())
	}
}

// ---------------------------------------------------------------------------
// VMM builder
// ---------------------------------------------------------------------------

type VMMBuilder struct{ h C.KrunVmmBuilder }

func NewVMMBuilder() *VMMBuilder {
	return &VMMBuilder{h: C.krun_vmm_builder_new()}
}

func (b *VMMBuilder) VCPUs(n uint8) error {
	var e C.KrunError
	return check("krun_vmm_builder_vcpus", C.krun_vmm_builder_vcpus(&b.h, C.uint8_t(n), &e), e)
}

func (b *VMMBuilder) RAMMiB(mib uint32) error {
	var e C.KrunError
	return check("krun_vmm_builder_ram_mib", C.krun_vmm_builder_ram_mib(&b.h, C.uint32_t(mib), &e), e)
}

func (b *VMMBuilder) SplitIRQChip(enabled bool) error {
	var e C.KrunError
	return check("krun_vmm_builder_split_irqchip", C.krun_vmm_builder_split_irqchip(&b.h, C.bool(enabled), &e), e)
}

// Payload sets the guest kernel. Ownership of p moves to the builder.
func (b *VMMBuilder) Payload(p *Payload) {
	C.krun_vmm_builder_payload(&b.h, p.h)
	p.h = nil
}

// Devices adds a device manager. Ownership of m moves to the builder.
func (b *VMMBuilder) Devices(m *DeviceManager) {
	C.krun_vmm_builder_devices(&b.h, C.KrunDeviceManager(m.h))
	m.h = nil
}

// Build consumes the builder and returns a ready-to-run VMM.
func (b *VMMBuilder) Build() (*VMM, error) {
	var e C.KrunError
	h := C.krun_vmm_builder_build(&b.h, &e)
	if h == nil {
		return nil, vmmError("krun_vmm_builder_build", 0, e)
	}
	return &VMM{h: h}, nil
}

// ---------------------------------------------------------------------------
// VMM
// ---------------------------------------------------------------------------

type VMM struct{ h C.KrunVmm }

// Handle returns a thread-safe control handle. Must be called before Run.
func (v *VMM) Handle() (*Handle, error) {
	var e C.KrunError
	h := C.krun_vmm_handle(v.h, &e)
	if h == nil {
		return nil, vmmError("krun_vmm_handle", 0, e)
	}
	return &Handle{h: h}, nil
}

// Run boots the guest and blocks. It consumes the VMM; when the guest powers
// off libkrun normally exits the process with the guest's exit code.
func (v *VMM) Run() {
	C.krun_vmm_run(v.h)
	v.h = nil
}

// Destroy frees a VMM that was never run.
func (v *VMM) Destroy() {
	if v.h != nil {
		C.krun_vmm_destroy(v.h)
		v.h = nil
	}
}

// Handle controls a running VMM from another goroutine.
type Handle struct{ h C.KrunVmmHandle }

func (h *Handle) Pause() error {
	var e C.KrunError
	return check("krun_vmm_handle_pause", C.krun_vmm_handle_pause(h.h, &e), e)
}

func (h *Handle) Resume() error {
	var e C.KrunError
	return check("krun_vmm_handle_resume", C.krun_vmm_handle_resume(h.h, &e), e)
}

// Shutdown requests an orderly guest shutdown. Only supported on aarch64 macOS
// with shutdown support enabled; elsewhere it returns a FeatureDisabled error.
func (h *Handle) Shutdown() error {
	var e C.KrunError
	return check("krun_vmm_handle_shutdown", C.krun_vmm_handle_shutdown(h.h, &e), e)
}

func (h *Handle) Destroy() {
	if h.h != nil {
		C.krun_vmm_handle_destroy(h.h)
		h.h = nil
	}
}
