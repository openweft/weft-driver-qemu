package builtin

import (
	"fmt"
	"strconv"
	"strings"
)

// bootConfig is the resolved description buildArgs turns into a
// qemu-system argv. It is hypervisor-agnostic of the *host*: with
// accel "tcg" QEMU emulates the guest CPU purely in software, needing
// no KVM/HVF and therefore no nested-virtualization support — which is
// exactly why this driver works inside a non-nested dev VM where the
// Apple-VZ driver can't create VMs.
type bootConfig struct {
	arch        string // "aarch64" | "x86_64"
	accel       string // "tcg" (default, no nested virt) | "hvf" | "kvm"
	cpu         string // "-cpu" model; default per arch
	cpus        int
	memMiB      int
	kernel      string // -kernel (direct Linux boot)
	initrd      string // -initrd (optional)
	cmdline     string // -append
	disks       []diskArg
	consolePath string // -serial file:<path>; empty → serial discarded
	netUser     bool   // user-mode (SLIRP) NIC — no root/tap needed
	fwd         []portForward
	// shares are virtio-9p directory shares exposed to the guest. The QEMU
	// backend uses 9p (not virtio-fs) because virtio-fs in QEMU needs the
	// external virtiofsd vhost-user daemon, which is Linux-only — so it
	// won't run when the host is macOS. 9p is QEMU-built-in (in-process)
	// so it works the same on Linux and macOS hosts. See
	// [[qemu-microvm-9p]] in the project memory.
	shares []shareArg
}

type diskArg struct {
	path     string
	readOnly bool
}

// shareArg is one virtio-9p host-directory share. tag is the mount_tag the
// guest sees (matches a weft-init Share.Tag); path is the absolute host dir.
type shareArg struct {
	tag      string
	path     string
	readOnly bool
}

// portForward maps a host port onto a guest port over user-mode networking,
// so an operator on the host can reach the guest (e.g. its WireGuard UDP
// listener) without bridging.
type portForward struct {
	proto     string // "tcp" | "udp"
	hostPort  int
	guestPort int
}

// machineType is the qemu -machine for an arch.
func machineType(arch string) string {
	switch arch {
	case "aarch64":
		return "virt" // the generic AArch64 virtual platform
	default: // x86_64
		return "q35"
	}
}

// defaultCPU is the qemu -cpu when none is given. "max" gives the richest
// emulated CPU TCG can offer for the arch.
func defaultCPU(string) string { return "max" }

// buildArgs renders the qemu-system argv (excluding argv[0]) for a direct
// Linux-kernel boot. Pure and deterministic so the wiring is unit-testable
// without QEMU installed.
func buildArgs(b bootConfig) ([]string, error) {
	if b.kernel == "" {
		return nil, fmt.Errorf("qemu: kernel is required (direct_linux boot)")
	}
	if b.arch == "" {
		return nil, fmt.Errorf("qemu: arch is required")
	}
	accel := b.accel
	if accel == "" {
		accel = "tcg"
	}
	cpu := b.cpu
	if cpu == "" {
		cpu = defaultCPU(b.arch)
	}
	cpus := b.cpus
	if cpus < 1 {
		cpus = 1
	}
	mem := b.memMiB
	if mem < 1 {
		mem = 512
	}

	args := []string{
		"-machine", machineType(b.arch),
		"-accel", accel,
		"-cpu", cpu,
		"-smp", strconv.Itoa(cpus),
		"-m", strconv.Itoa(mem),
		"-no-reboot",
		"-display", "none",
		"-kernel", b.kernel,
	}
	if b.initrd != "" {
		args = append(args, "-initrd", b.initrd)
	}
	if b.cmdline != "" {
		args = append(args, "-append", b.cmdline)
	}

	for i, d := range b.disks {
		spec := fmt.Sprintf("file=%s,if=virtio,format=raw,id=disk%d", d.path, i)
		if d.readOnly {
			spec += ",readonly=on"
		}
		args = append(args, "-drive", spec)
	}

	if b.netUser {
		netdev := "user,id=net0"
		for _, f := range b.fwd {
			proto := f.proto
			if proto == "" {
				proto = "tcp"
			}
			netdev += fmt.Sprintf(",hostfwd=%s::%d-:%d", proto, f.hostPort, f.guestPort)
		}
		args = append(args, "-netdev", netdev, "-device", "virtio-net-pci,netdev=net0")
	}

	// virtio-9p directory shares. One -fsdev/-device pair per share; the
	// guest sees them as mount_tag=<tag> and weft-init mounts them with
	// fs type "9p" + "trans=virtio,version=9p2000.L".
	//
	// security_model=none stores guest-side uid/gid/mode/symlinks
	// as host xattrs instead of requiring chown privileges; APFS supports
	// xattrs, so this works without running qemu as root. fsdev id is the
	// stable bridge between -fsdev and -device.
	for i, s := range b.shares {
		if s.tag == "" || s.path == "" {
			return nil, fmt.Errorf("qemu: share #%d needs both tag and path", i)
		}
		fsID := fmt.Sprintf("fs%d", i)
		fsdev := fmt.Sprintf("local,id=%s,path=%s,security_model=none", fsID, s.path)
		if s.readOnly {
			fsdev += ",readonly=on"
		}
		dev := fmt.Sprintf("virtio-9p-pci,fsdev=%s,mount_tag=%s", fsID, s.tag)
		args = append(args, "-fsdev", fsdev, "-device", dev)
	}

	if b.consolePath != "" {
		args = append(args, "-serial", "file:"+b.consolePath)
	} else {
		args = append(args, "-serial", "null")
	}

	return args, nil
}

// qemuBinary returns the qemu-system binary name for an arch.
func qemuBinary(arch string) string { return "qemu-system-" + arch }

// hostArch maps a Go GOARCH to the qemu arch name.
func hostArch(goarch string) string {
	switch goarch {
	case "arm64":
		return "aarch64"
	case "amd64":
		return "x86_64"
	default:
		return goarch
	}
}

// driverArch maps a qemu arch name back to the platform's Architecture label.
func driverArch(arch string) string {
	switch arch {
	case "aarch64":
		return "arm64"
	case "x86_64":
		return "amd64"
	default:
		return arch
	}
}

// joinCmdline is a small helper kept for callers assembling cmdlines from
// tokens (e.g. weft.config=virtiofs:<tag> plus console=…).
func joinCmdline(tokens ...string) string {
	return strings.Join(tokens, " ")
}
