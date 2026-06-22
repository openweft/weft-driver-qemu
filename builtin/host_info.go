package builtin

import (
	"runtime"

	drivers "github.com/openweft/weft-drivers"
)

// Version is the compile-time build version of weft-driver-qemu.
// Set via -ldflags "-X github.com/openweft/weft-driver-qemu/builtin.Version=vX.Y.Z"
// at link time ; "dev" for un-stamped builds. Reported in HostInfo
// so weft can surface it in the TUI / webui chrome.
var Version = "dev"

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
		Version:      Version,
	}
}
