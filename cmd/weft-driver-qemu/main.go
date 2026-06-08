// Command weft-driver-qemu is the QEMU hypervisor driver as an external weft
// go-plugin. Unlike weft-driver-vz it is pure Go (it execs qemu-system; no
// cgo, no framework objects), so it builds and runs on every platform —
// KVM where the host has nested virt, TCG otherwise.
//
// It has a single mode: launched by weft with no arguments, it serves the
// four driver services over go-plugin. Launch-time config arrives via env
// (set by the launching weft process).
package main

import (
	"log/slog"
	"os"
	"strconv"

	weftplugin "github.com/openweft/weft-driver-plugin"
	qemudriver "github.com/openweft/weft-driver-qemu/builtin"
	weftslognats "github.com/openweft/weft-slognats"
)

// envInt reads an int env var, returning 0 when unset or malformed so the
// driver falls back to its built-in default.
func envInt(name string) int {
	if v := os.Getenv(name); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return 0
}

// Optional QEMU tuning passed through from the host. Empty values keep the
// driver's defaults (qemu-system-<arch> on PATH, host arch, TCG accel).
const (
	envQemuBinary = "WEFT_QEMU_BINARY"
	envQemuArch   = "WEFT_QEMU_ARCH"
	envQemuAccel  = "WEFT_QEMU_ACCEL" // "kvm" on nested-virt hosts, else "tcg"
	envQemuMemMiB = "WEFT_QEMU_DEFAULT_MEM_MIB"
	envQemuCPUs   = "WEFT_QEMU_DEFAULT_CPUS"
)

func main() {
	hostUUID := os.Getenv(weftplugin.EnvHostUUID)
	log, logCloser := weftslognats.SetupFromEnv("weft.driver.qemu." + hostUUID + ".log")
	defer logCloser.Close()
	slog.SetDefault(log)

	defMem := envInt(envQemuMemMiB)
	defCPUs := envInt(envQemuCPUs)
	b := qemudriver.New(qemudriver.BundleOptions{
		Options: qemudriver.Options{
			HostUUID:      hostUUID,
			Hostname:      os.Getenv(weftplugin.EnvHostname),
			QemuBinary:    os.Getenv(envQemuBinary),
			Arch:          os.Getenv(envQemuArch),
			Accel:         os.Getenv(envQemuAccel),
			DefaultMemMiB: defMem,
			DefaultCPUs:   defCPUs,
		},
		StateDir: os.Getenv(weftplugin.EnvStateDir),
	})
	weftplugin.Serve(weftplugin.DriverSet{
		Hypervisor: b.Hypervisor,
		Network:    b.Network,
		Volume:     b.Volume,
		Image:      b.Image,
	})
}
