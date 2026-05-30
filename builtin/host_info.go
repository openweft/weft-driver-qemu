package builtin

import (
	"runtime"

	drivers "github.com/openweft/weft-drivers"
)

// hostInfoFor builds the HostInfo all four QEMU drivers report, applying the
// same defaults NewHypervisor does so a bare Options is still consistent.
func hostInfoFor(o Options) drivers.HostInfo {
	arch := o.Arch
	if arch == "" {
		arch = hostArch(runtime.GOARCH)
	}
	accel := o.Accel
	if accel == "" {
		accel = "tcg"
	}
	return drivers.HostInfo{
		UUID:         o.HostUUID,
		Hostname:     o.Hostname,
		Hypervisor:   "qemu-" + accel,
		Architecture: driverArch(arch),
	}
}
