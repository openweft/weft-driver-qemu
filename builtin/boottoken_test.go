package builtin

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	drivers "github.com/openweft/weft-drivers"
)

// envFakeQemu switches the test binary into a fake qemu-system: it records
// its argv and, when asked for the fw_cfg token, what it reads from fd 3.
const envFakeQemu = "WEFT_FAKE_QEMU_OUT"

func TestMain(m *testing.M) {
	if out := os.Getenv(envFakeQemu); out != "" {
		os.Exit(fakeQemu(out))
	}
	os.Exit(m.Run())
}

func fakeQemu(out string) int {
	argv := strings.Join(os.Args[1:], "\x00")
	if err := os.WriteFile(filepath.Join(out, "argv.bin"), []byte(argv), 0o600); err != nil {
		return 2
	}
	if strings.Contains(argv, "file="+bootTokenFDPath) {
		got, err := io.ReadAll(os.NewFile(3, "fd3"))
		if err != nil {
			return 3
		}
		if err := os.WriteFile(filepath.Join(out, "token.bin"), got, 0o600); err != nil {
			return 4
		}
	}
	return 0
}

func testToken() []byte {
	t := make([]byte, drivers.BootTokenLen)
	for i := range t {
		t[i] = byte(0xA0 + i)
	}
	return t
}

// fakeVM prepares a VM dir (with a kernel) and an out dir, and returns a
// Hypervisor whose "qemu" is this test binary.
func fakeVM(t *testing.T) (h *Hypervisor, vmDir, out string) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	vmDir = t.TempDir()
	out = t.TempDir()
	if err := os.WriteFile(filepath.Join(vmDir, "kernel"), []byte("fake"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv(envFakeQemu, out)
	return NewHypervisor(Options{QemuBinary: exe, Arch: "aarch64"}), vmDir, out
}

func waitExit(t *testing.T, vmDir string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(filepath.Join(vmDir, "exit.json")); err == nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("fake qemu did not exit")
}

func TestStartVMWithBootToken_DeliversTokenOverFd3Only(t *testing.T) {
	h, vmDir, out := fakeVM(t)
	token := testToken()
	if err := h.StartVMWithBootToken(context.Background(), vmDir, token); err != nil {
		t.Fatalf("StartVMWithBootToken: %v", err)
	}
	waitExit(t, vmDir)

	got, err := os.ReadFile(filepath.Join(out, "token.bin"))
	if err != nil {
		t.Fatalf("child did not read a token: %v", err)
	}
	if !bytes.Equal(got, token) {
		t.Errorf("child read %x, want %x", got, token)
	}

	argv, _ := os.ReadFile(filepath.Join(out, "argv.bin"))
	if !bytes.Contains(argv, []byte("-fw_cfg\x00name=opt/org.weft/boot-token,file=/dev/fd/3")) {
		t.Errorf("fw_cfg pair missing from argv: %q", argv)
	}
	for _, form := range [][]byte{token, []byte(hex.EncodeToString(token))} {
		if bytes.Contains(argv, form) {
			t.Error("token appears in argv")
		}
	}

	// Nothing the driver wrote under vmDir may hold the token.
	err = filepath.WalkDir(vmDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, rerr := os.ReadFile(p)
		if rerr != nil {
			return rerr
		}
		if bytes.Contains(b, token) || bytes.Contains(b, []byte(hex.EncodeToString(token))) {
			t.Errorf("token written to %s", p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestStartVM_WithoutTokenHasNoFwCfg(t *testing.T) {
	h, vmDir, out := fakeVM(t)
	if err := h.StartVM(context.Background(), vmDir); err != nil {
		t.Fatal(err)
	}
	waitExit(t, vmDir)
	argv, _ := os.ReadFile(filepath.Join(out, "argv.bin"))
	if bytes.Contains(argv, []byte("fw_cfg")) {
		t.Errorf("plain StartVM must not add fw_cfg: %q", argv)
	}
	if _, err := os.Stat(filepath.Join(out, "token.bin")); err == nil {
		t.Error("plain StartVM delivered a token")
	}
}

func TestStartVMWithBootToken_RejectsBadLength(t *testing.T) {
	h, vmDir, out := fakeVM(t)
	for _, n := range []int{0, 31, 33} {
		err := h.StartVMWithBootToken(context.Background(), vmDir, make([]byte, n))
		if !errors.Is(err, drivers.ErrInvalidBootToken) {
			t.Errorf("len %d: err = %v", n, err)
		}
	}
	if _, err := os.Stat(filepath.Join(out, "argv.bin")); err == nil {
		t.Error("qemu was launched for an invalid token")
	}
}

func TestStartVMWithBootToken_IdempotentWhenRunning(t *testing.T) {
	h, vmDir, out := fakeVM(t)
	pid := strconv.Itoa(os.Getpid()) // this process is alive
	if err := os.WriteFile(filepath.Join(vmDir, "vm.pid"), []byte(pid), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := h.StartVMWithBootToken(context.Background(), vmDir, testToken()); err != nil {
		t.Fatalf("already running must be a no-op: %v", err)
	}
	if _, err := os.Stat(filepath.Join(out, "argv.bin")); err == nil {
		t.Error("a second qemu was launched")
	}
}

func TestStartVMWithBootToken_NoKernelErrorsWithoutLeakingToken(t *testing.T) {
	h := NewHypervisor(Options{Arch: "aarch64"})
	token := testToken()
	err := h.StartVMWithBootToken(context.Background(), t.TempDir(), token)
	if err == nil {
		t.Fatal("expected error when no kernel present")
	}
	if strings.Contains(err.Error(), hex.EncodeToString(token)) || strings.Contains(err.Error(), string(token)) {
		t.Errorf("error leaks token: %v", err)
	}
}

func TestStartVMWithBootToken_LaunchFailureClosesPipeAndHidesToken(t *testing.T) {
	vmDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(vmDir, "kernel"), []byte("fake"), 0o644); err != nil {
		t.Fatal(err)
	}
	h := NewHypervisor(Options{QemuBinary: filepath.Join(vmDir, "no-such-qemu"), Arch: "aarch64"})
	token := testToken()
	err := h.StartVMWithBootToken(context.Background(), vmDir, token)
	if err == nil {
		t.Fatal("expected launch error")
	}
	if strings.Contains(err.Error(), hex.EncodeToString(token)) || strings.Contains(err.Error(), string(token)) {
		t.Errorf("error leaks token: %v", err)
	}
	if _, serr := os.Stat(filepath.Join(vmDir, "vm.pid")); serr == nil {
		t.Error("vm.pid written for a failed launch")
	}
}

func TestFeedToken(t *testing.T) {
	r, err := feedToken(testToken())
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	got, err := io.ReadAll(r) // returns at EOF: the write end is closed
	if err != nil || !bytes.Equal(got, testToken()) {
		t.Errorf("read %x, %v", got, err)
	}
}

func TestBuildArgs_BootTokenFD(t *testing.T) {
	base := bootConfig{arch: "aarch64", accel: "tcg", cpus: 1, memMiB: 256, kernel: "/k"}
	args, err := buildArgs(base)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.Join(args, " "), "fw_cfg") {
		t.Error("fw_cfg present without bootTokenFD")
	}
	base.bootTokenFD = true
	args, err = buildArgs(base)
	if err != nil {
		t.Fatal(err)
	}
	want := "-fw_cfg name=opt/org.weft/boot-token,file=/dev/fd/3"
	if !strings.Contains(strings.Join(args, " "), want) {
		t.Errorf("missing %q in %v", want, args)
	}
}

func TestFeedToken_PipeFailure(t *testing.T) {
	boom := errors.New("pipe boom")
	osPipe = func() (*os.File, *os.File, error) { return nil, nil, boom }
	defer func() { osPipe = os.Pipe }()
	if _, err := feedToken(testToken()); !errors.Is(err, boom) {
		t.Errorf("err = %v", err)
	}
	// And through the launcher: no qemu may start.
	h, vmDir, out := fakeVM(t)
	if err := h.StartVMWithBootToken(context.Background(), vmDir, testToken()); !errors.Is(err, boom) {
		t.Errorf("launcher err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(out, "argv.bin")); err == nil {
		t.Error("qemu started without its token pipe")
	}
}

func TestFeedToken_WriteFailure(t *testing.T) {
	osPipe = func() (*os.File, *os.File, error) {
		r, w, err := os.Pipe()
		if err == nil {
			_ = w.Close() // writing to a closed file fails
		}
		return r, w, err
	}
	defer func() { osPipe = os.Pipe }()
	r, err := feedToken(testToken())
	if err == nil || r != nil {
		t.Fatalf("got %v, %v; want an error and no reader", r, err)
	}
}
