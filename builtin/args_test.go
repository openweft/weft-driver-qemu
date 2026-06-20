package builtin

import (
	"strings"
	"testing"
)

// argpair finds the value following the first occurrence of flag in args.
func argpair(args []string, flag string) (string, bool) {
	for i := 0; i < len(args)-1; i++ {
		if args[i] == flag {
			return args[i+1], true
		}
	}
	return "", false
}

func has(args []string, flag string) bool {
	for _, a := range args {
		if a == flag {
			return true
		}
	}
	return false
}

func TestBuildArgs_AArch64TCGDirectLinux(t *testing.T) {
	args, err := buildArgs(bootConfig{
		arch:        "aarch64",
		cpus:        2,
		memMiB:      1024,
		kernel:      "/v/kernel",
		initrd:      "/v/initrd",
		cmdline:     "console=ttyAMA0 weft.config=virtiofs:cfg",
		consolePath: "/v/console.log",
		netUser:     true,
		fwd:         []portForward{{proto: "udp", hostPort: 51820, guestPort: 51820}},
		disks:       []diskArg{{path: "/v/disk.img"}},
	})
	if err != nil {
		t.Fatalf("buildArgs: %v", err)
	}

	if m, _ := argpair(args, "-machine"); m != "virt" {
		t.Errorf("-machine = %q, want virt", m)
	}
	if a, _ := argpair(args, "-accel"); a != "tcg" {
		t.Errorf("-accel = %q, want tcg (default, no nested virt)", a)
	}
	if c, _ := argpair(args, "-cpu"); c != "max" {
		t.Errorf("-cpu = %q, want max", c)
	}
	if s, _ := argpair(args, "-smp"); s != "2" {
		t.Errorf("-smp = %q, want 2", s)
	}
	if m, _ := argpair(args, "-m"); m != "1024" {
		t.Errorf("-m = %q, want 1024", m)
	}
	if k, _ := argpair(args, "-kernel"); k != "/v/kernel" {
		t.Errorf("-kernel = %q", k)
	}
	if i, _ := argpair(args, "-initrd"); i != "/v/initrd" {
		t.Errorf("-initrd = %q", i)
	}
	if ap, _ := argpair(args, "-append"); !strings.Contains(ap, "weft.config=virtiofs:cfg") {
		t.Errorf("-append = %q", ap)
	}
	if !has(args, "-no-reboot") {
		t.Error("missing -no-reboot")
	}

	netdev, ok := argpair(args, "-netdev")
	if !ok || !strings.HasPrefix(netdev, "user,id=net0") {
		t.Errorf("-netdev = %q", netdev)
	}
	if !strings.Contains(netdev, "hostfwd=udp::51820-:51820") {
		t.Errorf("-netdev missing udp hostfwd: %q", netdev)
	}
	if dev, _ := argpair(args, "-device"); dev != "virtio-net-pci,netdev=net0" {
		t.Errorf("-device = %q", dev)
	}

	drive, _ := argpair(args, "-drive")
	if !strings.Contains(drive, "file=/v/disk.img") || !strings.Contains(drive, "if=virtio") {
		t.Errorf("-drive = %q", drive)
	}
	if ser, _ := argpair(args, "-serial"); ser != "file:/v/console.log" {
		t.Errorf("-serial = %q", ser)
	}
}

func TestBuildArgs_ReadOnlyDisk(t *testing.T) {
	args, _ := buildArgs(bootConfig{arch: "aarch64", kernel: "/k", disks: []diskArg{{path: "/r", readOnly: true}}})
	d, _ := argpair(args, "-drive")
	if !strings.Contains(d, "readonly=on") {
		t.Errorf("read-only disk missing readonly=on: %q", d)
	}
}

func TestBuildArgs_X86Machine(t *testing.T) {
	args, _ := buildArgs(bootConfig{arch: "x86_64", kernel: "/k"})
	if m, _ := argpair(args, "-machine"); m != "q35" {
		t.Errorf("x86_64 -machine = %q, want q35", m)
	}
}

func TestBuildArgs_RequiresKernel(t *testing.T) {
	if _, err := buildArgs(bootConfig{arch: "aarch64"}); err == nil {
		t.Error("expected error when kernel is empty")
	}
}

// argpairs returns every value following the given flag, in argv order, so
// tests can assert on multi-occurrence flags like -fsdev and -device.
func argpairs(args []string, flag string) []string {
	var out []string
	for i := 0; i < len(args)-1; i++ {
		if args[i] == flag {
			out = append(out, args[i+1])
		}
	}
	return out
}

func TestBuildArgs_Virtio9PShares(t *testing.T) {
	args, err := buildArgs(bootConfig{
		arch:   "aarch64",
		kernel: "/k",
		shares: []shareArg{
			{tag: "rootfs0", path: "/rootfs"},
			{tag: "weft-nats", path: "/nats", readOnly: true},
		},
	})
	if err != nil {
		t.Fatalf("buildArgs: %v", err)
	}
	fsdevs := argpairs(args, "-fsdev")
	if len(fsdevs) != 2 {
		t.Fatalf("-fsdev count = %d, want 2: %v", len(fsdevs), fsdevs)
	}
	// fsdev id must bind to a matching -device — assert the (fsdev, device)
	// pair lines up by index so a future args refactor that tries to share
	// one fsdev across devices stays detectable.
	devs := argpairs(args, "-device")
	if len(devs) < 2 {
		t.Fatalf("-device count = %d, want >=2 (one per share): %v", len(devs), devs)
	}
	if !strings.Contains(fsdevs[0], "id=fs0") || !strings.Contains(fsdevs[0], "path=/rootfs") {
		t.Errorf("first -fsdev = %q", fsdevs[0])
	}
	if !strings.Contains(fsdevs[0], "security_model=none") {
		t.Errorf("first -fsdev missing security_model=none: %q", fsdevs[0])
	}
	if strings.Contains(fsdevs[0], "readonly=on") {
		t.Errorf("first share (RW) should not be read-only: %q", fsdevs[0])
	}
	if !strings.Contains(fsdevs[1], "readonly=on") {
		t.Errorf("second share (RO) missing readonly=on: %q", fsdevs[1])
	}
	// devs slice includes the virtio-net-pci entry too when netUser=true;
	// here netUser is false so it should be just the two 9p devices.
	if !strings.Contains(devs[0], "virtio-9p-pci") || !strings.Contains(devs[0], "fsdev=fs0") || !strings.Contains(devs[0], "mount_tag=rootfs0") {
		t.Errorf("first -device = %q", devs[0])
	}
	if !strings.Contains(devs[1], "mount_tag=weft-nats") {
		t.Errorf("second -device tag = %q", devs[1])
	}
}

func TestBuildArgs_ShareRequiresTagAndPath(t *testing.T) {
	if _, err := buildArgs(bootConfig{arch: "aarch64", kernel: "/k", shares: []shareArg{{path: "/p"}}}); err == nil {
		t.Error("expected error when share tag is empty")
	}
	if _, err := buildArgs(bootConfig{arch: "aarch64", kernel: "/k", shares: []shareArg{{tag: "t"}}}); err == nil {
		t.Error("expected error when share path is empty")
	}
}

// TestBuildArgs_VsockDeviceWhenCIDSet asserts the virtio-vsock
// argument is emitted iff bootConfig.vsockCID != 0 and carries the
// exact CID weft-agent allocated — the GuestPodPlane strict-when-
// known peer check on the agent side compares against this number,
// so any drift here silently breaks every microVM Attach.
func TestBuildArgs_VsockDeviceWhenCIDSet(t *testing.T) {
	args, err := buildArgs(bootConfig{
		arch:     "aarch64",
		kernel:   "/k",
		vsockCID: 4242,
	})
	if err != nil {
		t.Fatalf("buildArgs: %v", err)
	}
	devs := argpairs(args, "-device")
	found := false
	for _, d := range devs {
		if strings.Contains(d, "vhost-vsock-pci") {
			if !strings.Contains(d, "guest-cid=4242") {
				t.Errorf("vsock device missing guest-cid=4242: %q", d)
			}
			found = true
		}
	}
	if !found {
		t.Errorf("expected vhost-vsock-pci -device entry, got: %v", devs)
	}
}

// TestBuildArgs_NoVsockDeviceWhenCIDZero pins the legacy-VM
// permissive path : VsockCID=0 means "no vsock", no -device,
// agent-side falls back to the non-reserved CID guard.
func TestBuildArgs_NoVsockDeviceWhenCIDZero(t *testing.T) {
	args, err := buildArgs(bootConfig{arch: "aarch64", kernel: "/k", vsockCID: 0})
	if err != nil {
		t.Fatalf("buildArgs: %v", err)
	}
	for _, d := range argpairs(args, "-device") {
		if strings.Contains(d, "vhost-vsock-pci") {
			t.Errorf("unexpected vhost-vsock-pci with CID=0: %q", d)
		}
	}
}

func TestArchMapping(t *testing.T) {
	if hostArch("arm64") != "aarch64" || hostArch("amd64") != "x86_64" {
		t.Error("hostArch mapping wrong")
	}
	if driverArch("aarch64") != "arm64" || driverArch("x86_64") != "amd64" {
		t.Error("driverArch mapping wrong")
	}
	if qemuBinary("aarch64") != "qemu-system-aarch64" {
		t.Error("qemuBinary wrong")
	}
}
