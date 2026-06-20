// Package builtin implements a QEMU HypervisorDriver. Unlike the Apple-VZ
// driver it shells out to a qemu-system process (no cgo, no framework
// objects), and defaults to the TCG accelerator — pure software emulation
// that needs no KVM/HVF and thus no nested virtualization. That makes it the
// driver to reach for in a non-nested dev VM (e.g. a macOS Tart guest) where
// Apple VZ can't create VMs: QEMU/TCG still boots a real Linux micro-VM,
// slowly but really, so the in-guest stack (weft-init, wg0, weft-microvm-agent,
// mesh) can be exercised end to end.
//
// VM lifetime follows the same transitional convention as weft-driver-vz:
// the vmUUID is the absolute path to the VM's state directory. By
// convention that directory holds: kernel, initrd (optional), disk.img
// (optional), config.json (cmdline), mac.txt, vm.pid, console.log.
package builtin

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	drivers "github.com/openweft/weft-drivers"
)

// Options configures a QEMU driver instance.
type Options struct {
	// QemuBinary is the qemu-system executable. Empty → "qemu-system-<arch>"
	// resolved on PATH (point it at a pkgx shim or absolute path otherwise).
	QemuBinary string
	// Arch is the qemu guest arch ("aarch64" | "x86_64"). Empty → host arch.
	Arch string
	// Accel is the qemu accelerator. Empty → "tcg" (no nested virt needed).
	Accel string
	// DefaultMemMiB is used when a VMSpec leaves MemoryMiB unset.
	DefaultMemMiB int
	// DefaultCPUs is used when a VMSpec leaves CPUCount unset. Useful
	// under TCG where 1 CPU + 9p I/O serialises crun-create syscalls
	// enough to make container startup feel hung; bumping the default
	// to 2-4 turns minutes into seconds without changing any callers.
	DefaultCPUs int

	HostUUID string
	Hostname string
}

// Hypervisor implements drivers.HypervisorDriver against qemu-system.
type Hypervisor struct {
	opts Options
}

// compile-time conformance.
var _ drivers.HypervisorDriver = (*Hypervisor)(nil)

// NewHypervisor constructs the driver, filling in host-derived defaults.
func NewHypervisor(o Options) *Hypervisor {
	if o.Arch == "" {
		o.Arch = hostArch(runtime.GOARCH)
	}
	if o.Accel == "" {
		o.Accel = "tcg"
	}
	if o.QemuBinary == "" {
		o.QemuBinary = qemuBinary(o.Arch)
	}
	if o.DefaultMemMiB == 0 {
		o.DefaultMemMiB = 512
	}
	if o.DefaultCPUs == 0 {
		o.DefaultCPUs = 1
	}
	return &Hypervisor{opts: o}
}

func (h *Hypervisor) HostInfo(context.Context) (drivers.HostInfo, error) {
	return hostInfoFor(h.opts), nil
}

// CreateVM provisions the VM's static state: its directory and a stable MAC.
// Idempotent — re-creating reuses existing files.
func (h *Hypervisor) CreateVM(_ context.Context, spec drivers.VMSpec) error {
	vmDir := spec.UUID
	if vmDir == "" {
		return fmt.Errorf("qemu CreateVM: empty VM uuid/dir")
	}
	if err := os.MkdirAll(vmDir, 0o755); err != nil {
		return fmt.Errorf("qemu CreateVM: %w", err)
	}
	if err := ensureMAC(vmDir); err != nil {
		return fmt.Errorf("qemu CreateVM: mac: %w", err)
	}
	return nil
}

// StartVM assembles the qemu argv from the VM directory and launches a
// qemu-system process, recording its PID. Idempotent: an already-running VM
// (live vm.pid) is a no-op.
func (h *Hypervisor) StartVM(_ context.Context, vmUUID string) error {
	vmDir := vmUUID
	if alive, _ := pidAlive(vmDir); alive {
		return nil
	}
	// Clear any stale exit.json from a previous run before we relaunch ;
	// otherwise a restart after a crash would leave the old descriptor
	// in place and downstream status readers would mis-report the new
	// process as already exited.
	_ = os.Remove(filepath.Join(vmDir, "exit.json"))

	kernel := filepath.Join(vmDir, "kernel")
	if _, err := os.Stat(kernel); err != nil {
		return fmt.Errorf("qemu StartVM: kernel not found at %s: %w", kernel, err)
	}

	cfg := readConfig(vmDir)
	bc := bootConfig{
		arch:        h.opts.Arch,
		accel:       h.opts.Accel,
		cpus:        readCPUs(vmDir, h.opts.DefaultCPUs),
		memMiB:      readMem(vmDir, h.opts.DefaultMemMiB),
		kernel:      kernel,
		cmdline:     pickCmdline(cfg.Cmdline, defaultCmdline(h.opts.Arch)),
		consolePath: filepath.Join(vmDir, "console.log"),
		netUser:     true,
		vsockCID:    cfg.VsockCID,
	}
	if initrd := filepath.Join(vmDir, "initrd"); fileExists(initrd) {
		bc.initrd = initrd
	}
	if disk := filepath.Join(vmDir, "disk.img"); fileExists(disk) {
		bc.disks = append(bc.disks, diskArg{path: disk})
	}
	// microVM mode: wire each declared share over virtio-9p. weft's
	// RegisterMicroVM writes config.json with {microvm:true, shares:[…]}
	// — the Apple-VZ backend uses virtio-fs natively; here we surface
	// the same directories over QEMU 9p (in-process, no virtiofsd).
	// Also patch the cmdline: weft sets `console=hvc0` (matches Apple
	// VZ's serial-over-virtio-console port) and `…transport=virtiofs`
	// — neither fits QEMU's wiring, which captures ttyAMA0 to
	// console.log and mounts via the 9p kernel client. The rewrite is
	// scoped to microVM mode so a non-microVM direct-Linux boot keeps
	// the original cmdline verbatim.
	if cfg.MicroVM {
		for _, s := range cfg.Shares {
			bc.shares = append(bc.shares, shareArg{tag: s.Tag, path: s.Path, readOnly: s.ReadOnly})
		}
		bc.cmdline = rewriteCmdlineForQEMU(bc.cmdline)
	}

	args, err := buildArgs(bc)
	if err != nil {
		return fmt.Errorf("qemu StartVM: %w", err)
	}

	cmd := exec.Command(h.opts.QemuBinary, args...)
	// Detach from the agent's session so the VM outlives a single RPC and
	// isn't killed by a SIGHUP to the agent.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("qemu StartVM: launch %s: %w", h.opts.QemuBinary, err)
	}
	pid := cmd.Process.Pid
	if err := os.WriteFile(filepath.Join(vmDir, "vm.pid"), []byte(strconv.Itoa(pid)), 0o600); err != nil {
		return fmt.Errorf("qemu StartVM: write pid: %w", err)
	}
	// Reap the qemu child when it exits. Without this, a microVM whose
	// guest powers itself off (init-failure, normal `poweroff`, panic, ...)
	// becomes a zombie in the process table AND the next StatusVM call
	// still reads vm.pid → os.FindProcess succeeds → reported "running"
	// even though the guest is gone. Writing exit.json once Wait returns
	// lets readPID + statusFromExitFile flip the reported state on the
	// next probe without requiring the agent to poll qemu directly.
	go func() {
		err := cmd.Wait()
		out := map[string]any{
			"pid":          pid,
			"exited_at_ns": time.Now().UnixNano(),
		}
		if err != nil {
			out["error"] = err.Error()
		}
		if ee, ok := err.(*exec.ExitError); ok {
			out["exit_code"] = ee.ExitCode()
		} else if err == nil {
			out["exit_code"] = 0
		}
		blob, _ := json.Marshal(out)
		// Best-effort write — if the vmDir is gone (DeleteVM raced) we
		// just stop here ; the parent's reap is what mattered. Write
		// atomically (tmp + rename) so a concurrent status reader can
		// never observe a half-written JSON document.
		tmp := filepath.Join(vmDir, "exit.json.tmp")
		final := filepath.Join(vmDir, "exit.json")
		if werr := os.WriteFile(tmp, blob, 0o600); werr == nil {
			_ = os.Rename(tmp, final)
		}
	}()
	return nil
}

// StopVM sends SIGTERM to the qemu process. Idempotent: a missing or
// already-dead VM is a no-op.
func (h *Hypervisor) StopVM(_ context.Context, vmUUID string) error {
	vmDir := vmUUID
	pid, err := readPID(vmDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("qemu StopVM: %w", err)
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return nil
	}
	if err := proc.Signal(syscall.SIGTERM); err != nil {
		// ESRCH / "process already finished" → already stopped.
		return nil
	}
	return nil
}

// DeleteVM removes the VM's state directory. Idempotent.
func (h *Hypervisor) DeleteVM(_ context.Context, vmUUID string) error {
	return os.RemoveAll(vmUUID)
}

// AttachDisk ensures a backing file exists for the disk (transitional mode,
// matching weft-driver-vz): when missing and SizeGiB > 0 it lazily creates a
// raw image; SizeGiB == 0 with a missing file is an error. The boot disk is
// the file named disk.img in the VM directory.
func (h *Hypervisor) AttachDisk(_ context.Context, vmUUID string, disk drivers.DiskSpec) (err error) {
	path := disk.BackingPath
	if path == "" {
		path = filepath.Join(vmUUID, "disk.img")
	}
	if fileExists(path) {
		return nil
	}
	if disk.SizeGiB <= 0 {
		return fmt.Errorf("qemu AttachDisk: %s missing and SizeGiB==0", path)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("qemu AttachDisk: create %s: %w", path, err)
	}
	defer func() {
		if cerr := f.Close(); cerr != nil && err == nil {
			err = fmt.Errorf("qemu AttachDisk: close %s: %w", path, cerr)
		}
	}()
	if err := f.Truncate(int64(disk.SizeGiB) << 30); err != nil {
		return fmt.Errorf("qemu AttachDisk: size %s: %w", path, err)
	}
	return nil
}

// DetachDisk is a no-op in the transitional file model (the backing file is
// owned by the VolumeDriver once that path lands). Idempotent.
func (h *Hypervisor) DetachDisk(context.Context, string, string) error { return nil }

// AttachNIC / DetachNIC are unsupported: the QEMU driver wires user-mode
// (SLIRP) networking at StartVM rather than via hot-plug taps.
func (h *Hypervisor) AttachNIC(context.Context, string, drivers.NICHandle) error {
	return drivers.ErrUnsupported
}
func (h *Hypervisor) DetachNIC(context.Context, string, string) error {
	return drivers.ErrUnsupported
}

// ValidatePCIPassthrough accepts any BDF list — QEMU supports VFIO
// passthrough through `-device vfio-pci,host=<BDF>` (wired in
// builtin/args.go's buildArgs). The helper is symmetric with the
// VZ driver's same-named method so the upper layer can call it
// uniformly without driver-kind switches ; on QEMU it's always
// nil because args.go does the actual attach.
//
// Returns nil even for nil / empty input — the "no passthrough"
// case is the common one (every non-passthrough VM).
//
// The host kernel still needs vfio-pci bound to each BDF for the
// run to succeed ; weft does NOT manage that today (see
// docs/operations/pci-passthrough.md "vfio prep on the host").
func (h *Hypervisor) ValidatePCIPassthrough(bdfs []string) error {
	return nil
}

// ── helpers ────────────────────────────────────────────────────────────────

type vmConfig struct {
	Cmdline string         `json:"cmdline"`
	CPUs    int            `json:"cpus"`
	MemMiB  int            `json:"mem_mib"`
	MicroVM bool           `json:"microvm,omitempty"`
	Shares  []shareEntryJS `json:"shares,omitempty"`
	// VsockCID is the AF_VSOCK guest CID weft-agent allocated for
	// this VM (deterministic hash, range [0x10000, 0xfffefffe]).
	// When non-zero we append `-device vhost-vsock-pci,guest-cid=N`
	// to the qemu argv so the guest's GuestPodPlane bidi stream
	// surfaces with the exact CID GuestPodPlane.Attach's strict-
	// when-known check expects. 0 = legacy VM ; no device.
	VsockCID uint32 `json:"vsock_cid,omitempty"`
}

// shareEntryJS mirrors the (intentionally minimal) JSON shape weft's
// RegisterMicroVM writes — keep these field tags in lockstep with the
// adapter side or the schema mismatch will be silent.
type shareEntryJS struct {
	Tag      string `json:"tag"`
	Path     string `json:"path"`
	ReadOnly bool   `json:"read_only,omitempty"`
}

func readConfig(vmDir string) vmConfig {
	var c vmConfig
	if b, err := os.ReadFile(filepath.Join(vmDir, "config.json")); err == nil {
		_ = json.Unmarshal(b, &c)
	}
	return c
}

func readCmdline(vmDir, def string) string {
	if c := readConfig(vmDir).Cmdline; c != "" {
		return c
	}
	return def
}

func readCPUs(vmDir string, def int) int {
	if n := readConfig(vmDir).CPUs; n > 0 {
		return n
	}
	if def > 0 {
		return def
	}
	return 1
}

func readMem(vmDir string, def int) int {
	if n := readConfig(vmDir).MemMiB; n > 0 {
		return n
	}
	return def
}

// defaultCmdline routes the kernel console to the serial port qemu captures
// into console.log, per arch.
func defaultCmdline(arch string) string {
	switch arch {
	case "aarch64":
		return joinCmdline("console=ttyAMA0", "panic=-1")
	default:
		return joinCmdline("console=ttyS0", "panic=-1")
	}
}

// pickCmdline returns the caller-supplied cmdline when non-empty, else the
// per-arch default. Tiny helper kept so the StartVM body reads as a flat
// bootConfig assembly instead of an inline conditional.
func pickCmdline(supplied, def string) string {
	if supplied != "" {
		return supplied
	}
	return def
}

// rewriteCmdlineForQEMU adapts a microVM cmdline minted for the Apple-VZ
// backend so it works under QEMU. Two concerns:
//
//   - Console: weft sets `console=hvc0` (Apple VZ's virtio-console port).
//     QEMU here captures ttyAMA0 to console.log via `-serial file:` and
//     wires no virtio-console device, so a hvc0-routed kernel log would
//     vanish. Rewrite to ttyAMA0 (or ttyS0 on x86_64).
//   - Rootfs transport hint: append `weft.transport=9p` so weft-init knows
//     to mount the share with fs type "9p" + "trans=virtio,version=…"
//     rather than "virtiofs". Idempotent — appending twice is a no-op.
//
// Pure / deterministic so it's unit-testable without a running VM.
func rewriteCmdlineForQEMU(cmdline string) string {
	tokens := strings.Fields(cmdline)
	out := tokens[:0]
	for _, t := range tokens {
		switch t {
		case "console=hvc0":
			t = "console=ttyAMA0"
		case "weft.transport=9p":
			continue // re-added below; dedup
		}
		out = append(out, t)
	}
	out = append(out, "weft.transport=9p")
	return strings.Join(out, " ")
}

func ensureMAC(vmDir string) error {
	p := filepath.Join(vmDir, "mac.txt")
	if fileExists(p) {
		return nil
	}
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		return err
	}
	b[0] = (b[0] | 0x02) & 0xfe // locally administered, unicast
	mac := fmt.Sprintf("%02x:%02x:%02x:%02x:%02x:%02x", b[0], b[1], b[2], b[3], b[4], b[5])
	return os.WriteFile(p, []byte(mac), 0o600)
}

func readPID(vmDir string) (int, error) {
	b, err := os.ReadFile(filepath.Join(vmDir, "vm.pid"))
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(strings.TrimSpace(string(b)))
}

func pidAlive(vmDir string) (bool, error) {
	pid, err := readPID(vmDir)
	if err != nil {
		return false, err
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false, err
	}
	return proc.Signal(syscall.Signal(0)) == nil, nil
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
