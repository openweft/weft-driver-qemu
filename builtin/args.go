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
	// pciPassthrough is the list of host PCI BDFs the operator wants
	// passed through to the guest via VFIO. Resolved upstream (the
	// scheduler picks a host carrying the requested vendor:device
	// tuples, then weft-agent passes the concrete BDFs down).
	//
	// VFIO requires the host kernel to have unbound the native driver
	// and bound vfio-pci first — weft does NOT do that today, it's
	// part of the host's day-0 setup (see
	// docs/operations/pci-passthrough.md). buildArgs just appends one
	// -device vfio-pci,host=<BDF> per entry ; QEMU surfaces the
	// "device or resource busy" error if the BDF isn't actually
	// available for passthrough.
	pciPassthrough []string
	// migPassthrough is the list of NVIDIA MIG-instance mediated-device
	// UUIDs to pass through, one -device vfio-pci,sysfsdev=<path> per
	// entry. A MIG slice is NOT a whole PCI function, so it can't be
	// addressed by BDF like pciPassthrough ; it is a mediated device
	// exposed under /sys/bus/mdev/devices/<uuid>, and QEMU's vfio-pci
	// takes that sysfs path via `sysfsdev=`. The UUIDs come from the
	// host's GPU inventory (weft's detectGPUs enumerates MIG instances
	// on MIG-enabled cards) ; the scheduler claims a specific instance
	// and weft-agent passes its UUID down. As with pciPassthrough the
	// host's day-0 setup must have created the mdev + bound it to vfio ;
	// buildArgs only renders the argv. See docs/operations/gpu-sharing.md.
	migPassthrough []string
	// vsockCID is the AF_VSOCK guest CID the weft agent allocated for
	// the VM. 0 = unassigned (legacy VM ; skip the vsock device, fall
	// back to the agent's permissive Hello-CID guard). Non-zero adds
	// `-device vhost-vsock-pci,guest-cid=<vsockCID>` so the guest's
	// GuestPodPlane bidi stream comes in over the expected CID and
	// GuestPodPlane.Attach's strict-when-known check accepts it.
	vsockCID uint32
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

	// virtio-vsock device. Bound to the agent-allocated guest CID so
	// GuestPodPlane.Attach can verify the announced pod_id matches
	// this exact CID (strict-when-known peer check). Skipped when
	// vsockCID == 0 (legacy VMs ; the agent's permissive guard runs
	// instead). The host kernel must have the vhost_vsock module
	// loaded ; absent that, QEMU returns an init failure that
	// surfaces in the VM's monitor.log.
	if b.vsockCID != 0 {
		args = append(args, "-device",
			fmt.Sprintf("vhost-vsock-pci,guest-cid=%d", b.vsockCID))
	}

	// PCI passthrough : one -device vfio-pci per resolved BDF. Order
	// follows the input slice (the scheduler emits BDFs sorted by
	// the host's inventory, which is itself sorted by BDF — so this
	// is deterministic across runs). QEMU's vfio-pci device takes a
	// `host=<BDF>` argument in the canonical 0000:bb:dd.f form,
	// matching what /sys/bus/pci/devices uses.
	for _, bdf := range b.pciPassthrough {
		if bdf == "" {
			continue
		}
		if err := validVFIOToken(bdf); err != nil {
			return nil, fmt.Errorf("qemu: pci passthrough %q: %w", bdf, err)
		}
		args = append(args, "-device", fmt.Sprintf("vfio-pci,host=%s", bdf))
	}

	// MIG passthrough : one -device vfio-pci,sysfsdev=<mdev path> per
	// instance UUID. Emitted after the whole-card BDFs so a VM that
	// somehow requested both keeps a stable, deterministic device order.
	for _, uuid := range b.migPassthrough {
		if uuid == "" {
			continue
		}
		if err := validVFIOToken(uuid); err != nil {
			return nil, fmt.Errorf("qemu: mig passthrough %q: %w", uuid, err)
		}
		args = append(args, "-device",
			fmt.Sprintf("vfio-pci,sysfsdev=/sys/bus/mdev/devices/%s", uuid))
	}

	if b.consolePath != "" {
		args = append(args, "-serial", "file:"+b.consolePath)
	} else {
		args = append(args, "-serial", "null")
	}

	return args, nil
}

// validVFIOToken rejects a host= / sysfsdev= token that could break out
// of its qemu -device option or its sysfs path. These values are
// host-derived (nvidia-smi / sysfs walk → GPUClaim → config.json), not
// tenant-controlled, so this is defence in depth : even a malformed mdev
// label or a tampered config.json then can't inject extra qemu device
// properties (a `,key=val` rides the comma separator), corrupt the argv
// (whitespace), or path-traverse out of /sys/bus/mdev/devices (`/` or
// `..`). A canonical BDF ("0000:65:00.0") and a clean mdev UUID pass; a
// malformed entry fails the VM start loudly rather than booting wrong or
// extra hardware.
func validVFIOToken(s string) error {
	if strings.ContainsAny(s, ", \t\n\r=/") {
		return fmt.Errorf("forbidden character (comma, whitespace, '=' or '/')")
	}
	if strings.Contains(s, "..") {
		return fmt.Errorf("contains '..'")
	}
	return nil
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
