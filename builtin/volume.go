package builtin

// volume.go is the QEMU VolumeDriver scaffold. It declares the file backend
// (host-local, so volumes are host-bound) but defers actual create/attach to
// the transitional HypervisorDriver.AttachDisk path — full VolumeDriver
// wiring lands post-Phase-F, same as weft-driver-vz.

import (
	"context"

	drivers "github.com/openweft/weft-drivers"
)

// VolumeOptions configures the file volume driver.
type VolumeOptions struct {
	Options
	StateDir string // root for <StateDir>/<uuid>/disk.<fmt>
}

// Volume implements drivers.VolumeDriver (file backend) for QEMU hosts.
type Volume struct {
	opts     Options
	stateDir string
}

func NewVolume(o VolumeOptions) *Volume { return &Volume{opts: o.Options, stateDir: o.StateDir} }

var _ drivers.VolumeDriver = (*Volume)(nil)

func (v *Volume) Name() string { return "file" }
func (v *Volume) Local() bool  { return true } // file-backed → host-bound
func (v *Volume) HostInfo(context.Context) (drivers.HostInfo, error) {
	return hostInfoFor(v.opts), nil
}
func (v *Volume) EnsureVolume(context.Context, drivers.VolumeSpec) error {
	return drivers.ErrUnsupported
}
func (v *Volume) DestroyVolume(context.Context, string) error { return drivers.ErrUnsupported }
func (v *Volume) AttachVolume(context.Context, string, string) (drivers.AttachedVolume, error) {
	return drivers.AttachedVolume{}, drivers.ErrUnsupported
}
func (v *Volume) DetachVolume(context.Context, string, string) error { return drivers.ErrUnsupported }
