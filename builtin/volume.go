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

// Snapshot + backup ops are unsupported for the file backend ; weft-block
// is the documented home for that surface. These stubs land here so the
// driver still satisfies the drivers.VolumeDriver interface as it grows.
func (v *Volume) CreateSnapshot(context.Context, drivers.SnapshotSpec) (drivers.Snapshot, error) {
	return drivers.Snapshot{}, drivers.ErrUnsupported
}
func (v *Volume) ListSnapshots(context.Context, string) ([]drivers.Snapshot, error) {
	return nil, drivers.ErrUnsupported
}
func (v *Volume) DeleteSnapshot(context.Context, string, string) error {
	return drivers.ErrUnsupported
}
func (v *Volume) RevertSnapshot(context.Context, string, string) error {
	return drivers.ErrUnsupported
}
func (v *Volume) CreateBackup(context.Context, drivers.BackupSpec) (drivers.Backup, error) {
	return drivers.Backup{}, drivers.ErrUnsupported
}
func (v *Volume) ListBackups(context.Context, string, string) ([]drivers.Backup, error) {
	return nil, drivers.ErrUnsupported
}
func (v *Volume) DeleteBackup(context.Context, string) error {
	return drivers.ErrUnsupported
}
func (v *Volume) RestoreBackup(context.Context, string, drivers.VolumeSpec) error {
	return drivers.ErrUnsupported
}
