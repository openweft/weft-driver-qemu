package builtin

// bundle.go assembles the four driver instances a weft agent needs to run on
// a QEMU host — mirroring weft-driver-vz's Bundle so the agent's dispatch
// wiring is identical regardless of backend. Unlike the vz bundle this one is
// cross-platform (pure Go, no cgo), so a weft agent can drive QEMU/TCG on a
// macOS dev host where Apple VZ can't nest.

import (
	"path/filepath"

	drivers "github.com/openweft/weft-drivers"
)

// Bundle holds the QEMU-host driver instances. Volume is the active volume
// driver the agent serves; File and GoVolume are the two concrete backends it
// is selected from (so a caller can swap backends without re-plumbing the
// bundle). VolumeDriver implements drivers.VolumeDriver regardless of backend.
type Bundle struct {
	Hypervisor *Hypervisor
	Network    *Network
	Volume     drivers.VolumeDriver // active backend (== File or GoVolume)
	File       *Volume              // "file" backend (legacy/default)
	GoVolume   *GoVolume            // "govolume" backend (go-volumes, CGO=0)
	Image      *Image
}

// BundleOptions wraps construction inputs for all drivers. StateDir is the
// on-host root for per-volume + image-cache directories.
type BundleOptions struct {
	Options
	StateDir string
	// VolumeBackend selects the active volume driver: "" / "file" keeps the
	// legacy host-file backend (default, unchanged behaviour); "govolume"
	// activates the pure-Go go-volumes backend (CoW pool + in-process NBD
	// attach + OCI freeze/restore) for the local microVM-disk niche.
	VolumeBackend string
	// BackupRegistry is the default OCI registry root for the govolume
	// backend's backups (see GoVolumeOptions.BackupRegistry).
	BackupRegistry string
	// RegistryUsername and RegistryPassword authenticate the govolume backend to
	// the OCI registry on freeze / restore and OCI-overlay open / commit. Empty
	// → anonymous (see GoVolumeOptions.RegistryUsername).
	RegistryUsername string
	RegistryPassword string
}

// New returns the driver bundle for one QEMU host; all drivers share the same
// HostInfo. Both volume backends are constructed; VolumeBackend picks which one
// is exposed as Bundle.Volume.
func New(o BundleOptions) *Bundle {
	stateDir := o.StateDir
	if stateDir == "" {
		stateDir = ".weft-agent"
	}
	volRoot := filepath.Join(stateDir, "volumes")
	file := NewVolume(VolumeOptions{Options: o.Options, StateDir: volRoot})
	gov := NewGoVolume(GoVolumeOptions{
		Options:          o.Options,
		StateDir:         filepath.Join(stateDir, "govolumes"),
		BackupRegistry:   o.BackupRegistry,
		RegistryUsername: o.RegistryUsername,
		RegistryPassword: o.RegistryPassword,
	})
	b := &Bundle{
		Hypervisor: NewHypervisor(o.Options),
		Network:    NewNetwork(o.Options),
		File:       file,
		GoVolume:   gov,
		Image:      NewImage(ImageOptions{Options: o.Options, CacheDir: filepath.Join(stateDir, "cache")}),
	}
	switch o.VolumeBackend {
	case "govolume":
		b.Volume = gov
	default: // "" | "file"
		b.Volume = file
	}
	return b
}
