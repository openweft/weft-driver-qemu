package builtin

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	drivers "github.com/openweft/weft-drivers"
)

func TestPickCmdline(t *testing.T) {
	if got := pickCmdline("", "console=ttyAMA0"); got != "console=ttyAMA0" {
		t.Errorf("empty supplied: got %q", got)
	}
	if got := pickCmdline("custom", "def"); got != "custom" {
		t.Errorf("non-empty supplied: got %q", got)
	}
}

func TestRewriteCmdlineForQEMU(t *testing.T) {
	in := "weft.rootfs=virtiofs:rootfs0 console=hvc0 weft.env=WEFT_PROJECT_UUID=abc"
	out := rewriteCmdlineForQEMU(in)
	// hvc0 (Apple-VZ virtio-console) → ttyAMA0 (qemu -serial file:) so the
	// kernel log actually lands in console.log on the QEMU backend.
	if strings.Contains(out, "console=hvc0") {
		t.Errorf("console=hvc0 should be rewritten away: %q", out)
	}
	if !strings.Contains(out, "console=ttyAMA0") {
		t.Errorf("missing console=ttyAMA0: %q", out)
	}
	// Other tokens pass through verbatim.
	if !strings.Contains(out, "weft.rootfs=virtiofs:rootfs0") {
		t.Errorf("other tokens dropped: %q", out)
	}
	if !strings.Contains(out, "weft.env=WEFT_PROJECT_UUID=abc") {
		t.Errorf("env token dropped: %q", out)
	}
	// Transport hint appended so weft-init switches its mount-type branch.
	if !strings.Contains(out, "weft.transport=9p") {
		t.Errorf("missing weft.transport=9p: %q", out)
	}
	// Idempotent — re-running doesn't duplicate the hint.
	out2 := rewriteCmdlineForQEMU(out)
	if strings.Count(out2, "weft.transport=9p") != 1 {
		t.Errorf("weft.transport=9p not deduped: %q", out2)
	}
}

func TestCreateVM_MakesDirAndMAC(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "vm1")
	h := NewHypervisor(Options{})
	if err := h.CreateVM(context.Background(), drivers.VMSpec{UUID: dir}); err != nil {
		t.Fatalf("CreateVM: %v", err)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Errorf("vmDir not created: %v", err)
	}
	mac, err := os.ReadFile(filepath.Join(dir, "mac.txt"))
	if err != nil {
		t.Fatalf("mac.txt: %v", err)
	}
	if len(mac) != len("02:00:00:00:00:00") {
		t.Errorf("mac.txt malformed: %q", mac)
	}
	// Idempotent: second call keeps the same MAC.
	_ = h.CreateVM(context.Background(), drivers.VMSpec{UUID: dir})
	mac2, _ := os.ReadFile(filepath.Join(dir, "mac.txt"))
	if string(mac) != string(mac2) {
		t.Error("CreateVM regenerated the MAC (not idempotent)")
	}
}

func TestHostInfo_ReportsQemuTCG(t *testing.T) {
	h := NewHypervisor(Options{Arch: "aarch64", HostUUID: "h1"})
	hi, _ := h.HostInfo(context.Background())
	if hi.Hypervisor != "qemu-tcg" {
		t.Errorf("Hypervisor = %q, want qemu-tcg", hi.Hypervisor)
	}
	if hi.Architecture != "arm64" {
		t.Errorf("Architecture = %q, want arm64", hi.Architecture)
	}
}

func TestStopVM_MissingPIDIsNoOp(t *testing.T) {
	h := NewHypervisor(Options{})
	if err := h.StopVM(context.Background(), t.TempDir()); err != nil {
		t.Errorf("StopVM on missing pid = %v, want nil", err)
	}
}

func TestAttachDisk_CreatesBackingFile(t *testing.T) {
	dir := t.TempDir()
	h := NewHypervisor(Options{})
	path := filepath.Join(dir, "disk.img")

	if err := h.AttachDisk(context.Background(), dir, drivers.DiskSpec{BackingPath: path, SizeGiB: 1}); err != nil {
		t.Fatalf("AttachDisk: %v", err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("backing file: %v", err)
	}
	if fi.Size() != 1<<30 {
		t.Errorf("size = %d, want %d", fi.Size(), 1<<30)
	}
	// Idempotent: existing file → no error, size unchanged.
	if err := h.AttachDisk(context.Background(), dir, drivers.DiskSpec{BackingPath: path, SizeGiB: 1}); err != nil {
		t.Errorf("AttachDisk (existing) = %v", err)
	}
}

func TestAttachDisk_MissingZeroSizeErrors(t *testing.T) {
	h := NewHypervisor(Options{})
	err := h.AttachDisk(context.Background(), t.TempDir(), drivers.DiskSpec{BackingPath: filepath.Join(t.TempDir(), "x.img"), SizeGiB: 0})
	if err == nil {
		t.Error("expected error for missing file with SizeGiB==0")
	}
}

func TestDeleteVM_Idempotent(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "vm")
	os.MkdirAll(dir, 0o755)
	h := NewHypervisor(Options{})
	if err := h.DeleteVM(context.Background(), dir); err != nil {
		t.Fatalf("DeleteVM: %v", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Error("vmDir still present after DeleteVM")
	}
	if err := h.DeleteVM(context.Background(), dir); err != nil {
		t.Errorf("DeleteVM (missing) = %v, want nil", err)
	}
}

// TestStartVM_SpawnsAndRecordsPID exercises the launch path with a harmless
// stub binary standing in for qemu-system: it must build args, spawn, and
// write vm.pid. (A real boot needs a guest kernel; covered separately.)
func TestStartVM_SpawnsAndRecordsPID(t *testing.T) {
	stub, err := exec.LookPath("true")
	if err != nil {
		t.Skip("no 'true' binary to stand in for qemu")
	}
	dir := t.TempDir()
	// Convention: a kernel file must exist for StartVM to proceed.
	if err := os.WriteFile(filepath.Join(dir, "kernel"), []byte("fake"), 0o644); err != nil {
		t.Fatal(err)
	}
	h := NewHypervisor(Options{QemuBinary: stub, Arch: "aarch64"})
	if err := h.StartVM(context.Background(), dir); err != nil {
		t.Fatalf("StartVM: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "vm.pid")); err != nil {
		t.Errorf("vm.pid not written: %v", err)
	}
	// Stop must be a no-op-safe call even though the stub already exited.
	if err := h.StopVM(context.Background(), dir); err != nil {
		t.Errorf("StopVM = %v", err)
	}
}

func TestStartVM_NoKernelErrors(t *testing.T) {
	h := NewHypervisor(Options{Arch: "aarch64"})
	if err := h.StartVM(context.Background(), t.TempDir()); err == nil {
		t.Error("expected error when no kernel present")
	}
}
