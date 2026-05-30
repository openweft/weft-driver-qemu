package builtin

// image.go is the QEMU ImageDriver scaffold. Image caching (OCI / kernel+
// initrd artifacts) is shared platform logic that will be wrapped here once
// the imagestore is factored out of weft; for now it reports nothing cached.

import (
	"context"

	drivers "github.com/openweft/weft-drivers"
)

// ImageOptions configures the image cache driver.
type ImageOptions struct {
	Options
	CacheDir string
}

// Image implements drivers.ImageDriver for QEMU hosts.
type Image struct {
	opts     Options
	cacheDir string
}

func NewImage(o ImageOptions) *Image { return &Image{opts: o.Options, cacheDir: o.CacheDir} }

var _ drivers.ImageDriver = (*Image)(nil)

func (i *Image) HostInfo(context.Context) (drivers.HostInfo, error) {
	return hostInfoFor(i.opts), nil
}
func (i *Image) Pull(context.Context, string) error                { return drivers.ErrUnsupported }
func (i *Image) LocalPath(context.Context, string) (string, error) { return "", drivers.ErrUnsupported }
func (i *Image) Delete(context.Context, string) error              { return drivers.ErrUnsupported }
func (i *Image) InCache(context.Context, string) (bool, error)     { return false, nil }
