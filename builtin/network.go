package builtin

// network.go is the QEMU NetworkDriver scaffold. For now the QEMU driver
// wires user-mode (SLIRP) networking directly at StartVM (see args.go), which
// needs no host-side construct — so the explicit network lifecycle returns
// ErrUnsupported until TAP/bridge + the shared wireguard sub-driver land.

import (
	"context"

	drivers "github.com/openweft/weft-drivers"
)

// Network implements drivers.NetworkDriver for QEMU hosts.
type Network struct {
	opts Options
}

func NewNetwork(o Options) *Network { return &Network{opts: o} }

var _ drivers.NetworkDriver = (*Network)(nil)

func (n *Network) HostInfo(context.Context) (drivers.HostInfo, error) {
	return hostInfoFor(n.opts), nil
}
func (n *Network) EnsureNetwork(context.Context, drivers.NetworkSpec) error {
	return drivers.ErrUnsupported
}
func (n *Network) DestroyNetwork(context.Context, string) error { return drivers.ErrUnsupported }
func (n *Network) AttachPort(context.Context, drivers.PortSpec) (drivers.NICHandle, error) {
	return drivers.NICHandle{}, drivers.ErrUnsupported
}
func (n *Network) DetachPort(context.Context, string) error { return drivers.ErrUnsupported }
func (n *Network) RotateMeshPeer(context.Context, drivers.PortSpec) error {
	return drivers.ErrUnsupported
}
