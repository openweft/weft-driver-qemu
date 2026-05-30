package main

// plugin_handshake_test.go is the end-to-end smoke for the external-driver
// path: it builds this plugin and drives it through the real go-plugin
// transport — process spawn → handshake → Dispense → a live HostInfo RPC.
// (The bufconn test in weft-driver-plugin covers the conversions in-process;
// this covers the parts only a real subprocess exercises.) Reusable harness:
// point Launch at any weft-driver-* binary to smoke its handshake.

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	weftplugin "github.com/openweft/weft-driver-plugin"
)

func TestPluginHandshake_EndToEnd(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "weft-driver-qemu")
	build := exec.Command("go", "build", "-o", bin, ".")
	build.Stderr = os.Stderr
	if err := build.Run(); err != nil {
		t.Skipf("cannot build plugin under test: %v", err)
	}

	set, client, err := weftplugin.Launch(weftplugin.LaunchOptions{
		Executable: bin,
		HostUUID:   "host-smoke",
		Hostname:   "smoke-host",
		StateDir:   dir,
	})
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	defer client.Kill()

	hi, err := set.Hypervisor.HostInfo(context.Background())
	if err != nil {
		t.Fatalf("HostInfo over plugin: %v", err)
	}
	if hi.UUID != "host-smoke" {
		t.Errorf("HostInfo.UUID = %q, want host-smoke (env not threaded to plugin)", hi.UUID)
	}
	if hi.Hypervisor == "" {
		t.Errorf("HostInfo.Hypervisor empty over plugin")
	}
	// A second service on the same connection, plus a non-context method.
	if set.Volume.Name() == "" {
		t.Errorf("Volume.Name empty over plugin")
	}
}
