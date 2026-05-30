package builtin

// bundle.go assembles the four driver instances a weft agent needs to run on
// a QEMU host — mirroring weft-driver-vz's Bundle so the agent's dispatch
// wiring is identical regardless of backend. Unlike the vz bundle this one is
// cross-platform (pure Go, no cgo), so a weft agent can drive QEMU/TCG on a
// macOS dev host where Apple VZ can't nest.

import "path/filepath"

// Bundle holds the four QEMU-host driver instances.
type Bundle struct {
	Hypervisor *Hypervisor
	Network    *Network
	Volume     *Volume
	Image      *Image
}

// BundleOptions wraps construction inputs for all four drivers. StateDir is
// the on-host root for per-volume + image-cache directories.
type BundleOptions struct {
	Options
	StateDir string
}

// New returns the driver bundle for one QEMU host; all four drivers share the
// same HostInfo.
func New(o BundleOptions) *Bundle {
	stateDir := o.StateDir
	if stateDir == "" {
		stateDir = ".weft-agent"
	}
	return &Bundle{
		Hypervisor: NewHypervisor(o.Options),
		Network:    NewNetwork(o.Options),
		Volume:     NewVolume(VolumeOptions{Options: o.Options, StateDir: filepath.Join(stateDir, "volumes")}),
		Image:      NewImage(ImageOptions{Options: o.Options, CacheDir: filepath.Join(stateDir, "cache")}),
	}
}
