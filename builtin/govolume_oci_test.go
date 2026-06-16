package builtin

// govolume_oci_test.go exercises the OCI-image-backed volume MODE end to end,
// reusing the in-test NBD client + httptest OCI registry harness from
// govolume_test.go:
//
//   - Freeze a base rootfs into the registry, EnsureVolume(Format="oci") over
//     it, AttachVolume → serves an nbd:// the in-test client reads (base
//     content) and writes through (in-memory overlay).
//   - CreateSnapshot/CreateBackup → overlay.Commit pushes a NEW versioned tag
//     with only the changed chunks (delta-deduped); the sidecar's Current
//     advances to it.
//   - RestoreBackup(Format="oci") branches a fresh overlay from the committed
//     tag and reads back the written data while the base stays unchanged.
//   - every error branch (bad ref, OpenReadOnly failure, commit/push failure,
//     listen failure, not-attached, sidecar IO).
//
// All in-process, CGO-free, t.TempDir-rooted; the overlay is in memory so no
// pool file is ever touched in this mode.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"testing"

	volume "github.com/go-volumes/interface"
	"github.com/go-volumes/oci"
	"github.com/go-volumes/oci/registry"
	drivers "github.com/openweft/weft-drivers"
)

// rawROVolume is a fixed-size in-memory volume.ReadOnly used as the source we
// freeze into the test registry to build a base rootfs image.
type rawROVolume struct{ data []byte }

func (r rawROVolume) ReadAt(p []byte, off int64) (int, error) {
	if off >= int64(len(r.data)) {
		return 0, fmt.Errorf("read past end")
	}
	n := copy(p, r.data[off:])
	return n, nil
}
func (r rawROVolume) Size() (int64, error) { return int64(len(r.data)), nil }
func (r rawROVolume) Close() error         { return nil }

var _ volume.ReadOnly = rawROVolume{}

// freezeBase freezes content into reg under repo:tag and returns the oci:// ref.
func freezeBase(t *testing.T, reg *testRegistry, repo, tag string, content []byte) string {
	t.Helper()
	regHost := strings.TrimPrefix(reg.URL, "http://")
	client := &registry.Client{BaseURL: reg.URL, Repository: repo}
	if _, err := oci.Freeze(context.Background(), rawROVolume{data: content}, client, tag, oci.Options{ChunkSize: 4096}); err != nil {
		t.Fatalf("freeze base: %v", err)
	}
	return fmt.Sprintf("oci://%s/%s:%s", regHost, repo, tag)
}

// readOCI reads length bytes at off from an oci:// ref via OpenReadOnly, so a
// test can assert what a committed tag actually contains.
func readOCI(t *testing.T, v *GoVolume, ref string, off int64, length int) []byte {
	t.Helper()
	client, r, err := v.registryFor(ref)
	if err != nil {
		t.Fatalf("registryFor %q: %v", ref, err)
	}
	img, err := oci.OpenReadOnly(context.Background(), client, r)
	if err != nil {
		t.Fatalf("OpenReadOnly %q: %v", ref, err)
	}
	defer img.Close()
	buf := make([]byte, length)
	if _, err := img.ReadAt(buf, off); err != nil {
		t.Fatalf("ReadAt %q: %v", ref, err)
	}
	return buf
}

// TestGoVolumeOCIOverlayRoundTrip is the full golden-image lifecycle: freeze a
// base, ensure an overlay volume over it, attach + read the base + write a
// delta, snapshot (Commit a new tag), branch a new volume from that tag and
// confirm the write is present while the base is untouched.
func TestGoVolumeOCIOverlayRoundTrip(t *testing.T) {
	reg := newTestRegistry(t)
	defer reg.Close()

	v := NewGoVolume(GoVolumeOptions{StateDir: t.TempDir()})
	ctx := context.Background()

	// 8 KiB base rootfs: "GOLDEN" pattern in chunk 0, zeros in chunk 1.
	base := make([]byte, 8192)
	copy(base, bytes.Repeat([]byte("GOLDEN.."), 512)) // fills first 4 KiB
	baseRef := freezeBase(t, reg, "weft/rootfs", "v1", base)

	// EnsureVolume in OCI mode: source ref travels in spec.Name.
	spec := drivers.VolumeSpec{UUID: "oci-a", Name: baseRef, Format: "oci"}
	if err := v.EnsureVolume(ctx, spec); err != nil {
		t.Fatalf("EnsureVolume(oci): %v", err)
	}
	// Idempotent re-ensure.
	if err := v.EnsureVolume(ctx, spec); err != nil {
		t.Fatalf("EnsureVolume(oci) 2nd: %v", err)
	}
	if !v.isOCIVolume("oci-a") {
		t.Fatalf("isOCIVolume = false after ensure")
	}

	// Attach: the overlay serves the base read-write over nbd://.
	att, err := v.AttachVolume(ctx, "oci-a", "host-1")
	if err != nil {
		t.Fatalf("AttachVolume: %v", err)
	}
	if !strings.HasPrefix(att.BackingPath, "nbd://127.0.0.1:") {
		t.Fatalf("BackingPath = %q", att.BackingPath)
	}
	// Idempotent attach returns the same addr.
	att2, err := v.AttachVolume(ctx, "oci-a", "host-1")
	if err != nil || att2.BackingPath != att.BackingPath {
		t.Fatalf("re-attach = %q,%v want %q", att2.BackingPath, err, att.BackingPath)
	}
	addr := strings.TrimPrefix(att.BackingPath, "nbd://")

	// Read base content through the overlay.
	c := dialNBD(t, addr)
	got := c.read(t, 0, 8)
	if !bytes.Equal(got, []byte("GOLDEN..")) {
		t.Errorf("base read = %q, want GOLDEN..", got)
	}
	// Write a delta into chunk 1 (previously zero).
	delta := []byte("WRITTEN-DELTA-LAYER")
	c.write(t, 4096, delta)
	c.disconnect(t)

	// Snapshot: Commit the overlay to a new tag (delta only).
	snap, err := v.CreateSnapshot(ctx, drivers.SnapshotSpec{VolumeUUID: "oci-a", Name: "v2"})
	if err != nil {
		t.Fatalf("CreateSnapshot: %v", err)
	}
	if !strings.HasSuffix(snap.Name, ":v2") {
		t.Errorf("snapshot ref = %q, want …:v2", snap.Name)
	}

	// The new tag carries the written delta.
	gotDelta := readOCI(t, v, snap.Name, 4096, len(delta))
	if !bytes.Equal(gotDelta, delta) {
		t.Errorf("committed delta = %q, want %q", gotDelta, delta)
	}
	// The BASE tag is unchanged: chunk 1 still reads zero.
	baseChunk1 := readOCI(t, v, baseRef, 4096, len(delta))
	if !bytes.Equal(baseChunk1, make([]byte, len(delta))) {
		t.Errorf("base mutated: chunk1 = %q, want zeros", baseChunk1)
	}

	// Detach (drops the in-memory overlay; the commit persisted the version).
	if err := v.DetachVolume(ctx, "oci-a", "host-1"); err != nil {
		t.Fatalf("DetachVolume: %v", err)
	}

	// Branch a NEW volume from the committed tag and confirm the delta is there.
	if err := v.RestoreBackup(ctx, snap.Name, drivers.VolumeSpec{UUID: "oci-b", Format: "oci"}); err != nil {
		t.Fatalf("RestoreBackup(oci): %v", err)
	}
	// Idempotent restore.
	if err := v.RestoreBackup(ctx, snap.Name, drivers.VolumeSpec{UUID: "oci-b", Format: "oci"}); err != nil {
		t.Fatalf("RestoreBackup(oci) 2nd: %v", err)
	}
	attB, err := v.AttachVolume(ctx, "oci-b", "host-2")
	if err != nil {
		t.Fatalf("AttachVolume branched: %v", err)
	}
	addrB := strings.TrimPrefix(attB.BackingPath, "nbd://")
	cb := dialNBD(t, addrB)
	gotB := cb.read(t, 4096, uint32(len(delta)))
	cb.disconnect(t)
	if !bytes.Equal(gotB, delta) {
		t.Errorf("branched read = %q, want %q", gotB, delta)
	}
	_ = v.DetachVolume(ctx, "oci-b", "host-2")

	// Destroy tears down the OCI volume + its sidecar.
	if err := v.DestroyVolume(ctx, "oci-a"); err != nil {
		t.Fatalf("DestroyVolume: %v", err)
	}
	if v.isOCIVolume("oci-a") {
		t.Errorf("sidecar survived destroy")
	}
}

// TestGoVolumeOCIBackupToTarget commits an attached overlay to an explicit
// backup target (a different repo), the CreateBackup path.
func TestGoVolumeOCIBackupToTarget(t *testing.T) {
	reg := newTestRegistry(t)
	defer reg.Close()
	v := NewGoVolume(GoVolumeOptions{StateDir: t.TempDir()})
	ctx := context.Background()

	base := bytes.Repeat([]byte("B"), 4096)
	baseRef := freezeBase(t, reg, "weft/img", "base", base)
	if err := v.EnsureVolume(ctx, drivers.VolumeSpec{UUID: "oci-c", Name: baseRef, Format: "oci"}); err != nil {
		t.Fatalf("EnsureVolume: %v", err)
	}
	att, err := v.AttachVolume(ctx, "oci-c", "h")
	if err != nil {
		t.Fatalf("AttachVolume: %v", err)
	}
	addr := strings.TrimPrefix(att.BackingPath, "nbd://")
	c := dialNBD(t, addr)
	c.write(t, 0, []byte("PATCHED"))
	c.disconnect(t)

	regHost := strings.TrimPrefix(reg.URL, "http://")
	target := fmt.Sprintf("oci://%s/weft/backups:oci-c-bk", regHost)
	bk, err := v.CreateBackup(ctx, drivers.BackupSpec{VolumeUUID: "oci-c", SnapshotName: "bk", Target: target})
	if err != nil {
		t.Fatalf("CreateBackup: %v", err)
	}
	if bk.URL != target || bk.State != "complete" {
		t.Errorf("backup descriptor = %+v", bk)
	}
	got := readOCI(t, v, target, 0, 7)
	if !bytes.Equal(got, []byte("PATCHED")) {
		t.Errorf("backup content = %q, want PATCHED", got)
	}
	_ = v.DetachVolume(ctx, "oci-c", "h")
}

// TestGoVolumeOCIEnsureErrors covers EnsureVolume's OCI-mode error branches.
func TestGoVolumeOCIEnsureErrors(t *testing.T) {
	reg := newTestRegistry(t)
	defer reg.Close()
	v := NewGoVolume(GoVolumeOptions{StateDir: t.TempDir()})
	ctx := context.Background()

	// Empty base ref in spec.Name.
	if err := v.EnsureVolume(ctx, drivers.VolumeSpec{UUID: "e1", Format: "oci"}); err == nil {
		t.Errorf("ensure empty base ref: want error")
	}
	// Bad ref schema → registryFor parse error.
	if err := v.EnsureVolume(ctx, drivers.VolumeSpec{UUID: "e2", Name: "not-a-url", Format: "oci"}); err == nil {
		t.Errorf("ensure bad ref: want error")
	}
	// Unreachable / missing base manifest → OpenReadOnly error.
	missing := fmt.Sprintf("oci://%s/no/such:tag", strings.TrimPrefix(reg.URL, "http://"))
	if err := v.EnsureVolume(ctx, drivers.VolumeSpec{UUID: "e3", Name: missing, Format: "oci"}); err == nil {
		t.Errorf("ensure missing base: want error")
	}
	// mkdir fails: a file sits where the per-UUID dir must go (after a valid base
	// open, so the mkdir branch is reached).
	base := bytes.Repeat([]byte("X"), 4096)
	baseRef := freezeBase(t, reg, "weft/img", "ok", base)
	writeFile(t, v.volDir("e4"), []byte("i am a file"))
	if err := v.EnsureVolume(ctx, drivers.VolumeSpec{UUID: "e4", Name: baseRef, Format: "oci"}); err == nil {
		t.Errorf("ensure mkdir collision: want error")
	}
}

// TestGoVolumeOCIAttachErrors covers attach failures in OCI mode.
func TestGoVolumeOCIAttachErrors(t *testing.T) {
	reg := newTestRegistry(t)
	defer reg.Close()
	v := NewGoVolume(GoVolumeOptions{StateDir: t.TempDir()})
	ctx := context.Background()

	base := bytes.Repeat([]byte("Y"), 4096)
	baseRef := freezeBase(t, reg, "weft/img", "v1", base)
	if err := v.EnsureVolume(ctx, drivers.VolumeSpec{UUID: "a1", Name: baseRef, Format: "oci"}); err != nil {
		t.Fatalf("EnsureVolume: %v", err)
	}

	// listen seam fails → AttachVolume listen error (img is opened then closed).
	t.Run("listen-fails", func(t *testing.T) {
		withSeams(t, func() {
			listenTCP = func(string) (net.Listener, error) { return nil, errors.New("listen boom") }
			if _, err := v.AttachVolume(ctx, "a1", "h"); err == nil {
				t.Errorf("want listen error")
			}
		})
	})

	// openOverlay fails: the recorded current ref is unresolvable. Corrupt the
	// sidecar to point at a missing tag.
	t.Run("open-current-fails", func(t *testing.T) {
		if err := v.writeOCISidecar("a1", ociSidecar{Base: baseRef, Current: "oci://" + strings.TrimPrefix(reg.URL, "http://") + "/no/such:tag"}); err != nil {
			t.Fatalf("writeOCISidecar: %v", err)
		}
		if _, err := v.AttachVolume(ctx, "a1", "h2"); err == nil {
			t.Errorf("want open-current error")
		}
	})

	// Lost-the-attach-race in OCI mode: a competing server is planted under the
	// key before the listen seam returns; AttachVolume closes its own and returns
	// the winner.
	t.Run("lost-attach-race", func(t *testing.T) {
		// Restore a valid sidecar first.
		if err := v.writeOCISidecar("a1", ociSidecar{Base: baseRef, Current: baseRef}); err != nil {
			t.Fatalf("writeOCISidecar: %v", err)
		}
		key := attachKey("a1", "race")
		withSeams(t, func() {
			listenTCP = func(addr string) (net.Listener, error) {
				ln, err := net.Listen("tcp", addr)
				if err != nil {
					return nil, err
				}
				v.mu.Lock()
				v.servers[key] = &nbdServer{addr: "nbd://winner:1"}
				v.mu.Unlock()
				return ln, nil
			}
			att, err := v.AttachVolume(ctx, "a1", "race")
			if err != nil {
				t.Fatalf("AttachVolume: %v", err)
			}
			if att.BackingPath != "nbd://winner:1" {
				t.Errorf("BackingPath = %q, want race winner", att.BackingPath)
			}
		})
		v.mu.Lock()
		delete(v.servers, key)
		v.mu.Unlock()
	})
}

// TestGoVolumeOCISnapshotErrors covers the snapshot/commit error branches.
func TestGoVolumeOCISnapshotErrors(t *testing.T) {
	reg := newTestRegistry(t)
	defer reg.Close()
	v := NewGoVolume(GoVolumeOptions{StateDir: t.TempDir()})
	ctx := context.Background()

	base := bytes.Repeat([]byte("Z"), 4096)
	baseRef := freezeBase(t, reg, "weft/img", "v1", base)
	if err := v.EnsureVolume(ctx, drivers.VolumeSpec{UUID: "s1", Name: baseRef, Format: "oci"}); err != nil {
		t.Fatalf("EnsureVolume: %v", err)
	}

	// Snapshot a DETACHED volume → not-attached error (no live overlay).
	if _, err := v.CreateSnapshot(ctx, drivers.SnapshotSpec{VolumeUUID: "s1", Name: "v2"}); err == nil {
		t.Errorf("snapshot detached: want error")
	}

	// Attach so an overlay is live for the remaining branches.
	if _, err := v.AttachVolume(ctx, "s1", "h"); err != nil {
		t.Fatalf("AttachVolume: %v", err)
	}

	// Auto-named snapshot (empty Name) → timestamped tag.
	auto, err := v.CreateSnapshot(ctx, drivers.SnapshotSpec{VolumeUUID: "s1"})
	if err != nil {
		t.Fatalf("CreateSnapshot auto: %v", err)
	}
	if !strings.Contains(auto.Name, ":snap-") {
		t.Errorf("auto snapshot ref = %q, want …:snap-*", auto.Name)
	}

	// Commit failure (seam) → snapshot error.
	t.Run("commit-fails", func(t *testing.T) {
		origCommit := ociCommit
		t.Cleanup(func() { ociCommit = origCommit })
		ociCommit = func(context.Context, *oci.Overlay, *registry.Client, string) (string, error) {
			return "", errors.New("push boom")
		}
		if _, err := v.CreateSnapshot(ctx, drivers.SnapshotSpec{VolumeUUID: "s1", Name: "v3"}); err == nil {
			t.Errorf("commit-fails: want error")
		}
	})

	// retagOCIRef failure: a base whose ref has no tag colon. Plant such a base in
	// the sidecar, keep the overlay live.
	t.Run("retag-fails", func(t *testing.T) {
		if err := v.writeOCISidecar("s1", ociSidecar{Base: "oci://host/repo-no-tag", Current: baseRef}); err != nil {
			t.Fatalf("writeOCISidecar: %v", err)
		}
		if _, err := v.CreateSnapshot(ctx, drivers.SnapshotSpec{VolumeUUID: "s1", Name: "v4"}); err == nil {
			t.Errorf("retag-fails: want error")
		}
		// Restore a sane base for cleanup.
		_ = v.writeOCISidecar("s1", ociSidecar{Base: baseRef, Current: baseRef})
	})

	_ = v.DetachVolume(ctx, "s1", "h")
}

// TestGoVolumeOCIBackupErrors covers CreateBackup's OCI-mode error branches.
func TestGoVolumeOCIBackupErrors(t *testing.T) {
	reg := newTestRegistry(t)
	defer reg.Close()
	v := NewGoVolume(GoVolumeOptions{StateDir: t.TempDir()})
	ctx := context.Background()

	base := bytes.Repeat([]byte("Q"), 4096)
	baseRef := freezeBase(t, reg, "weft/img", "v1", base)
	if err := v.EnsureVolume(ctx, drivers.VolumeSpec{UUID: "b1", Name: baseRef, Format: "oci"}); err != nil {
		t.Fatalf("EnsureVolume: %v", err)
	}

	// Empty target → error (before the live-overlay check).
	if _, err := v.CreateBackup(ctx, drivers.BackupSpec{VolumeUUID: "b1", SnapshotName: "s"}); err == nil {
		t.Errorf("backup empty target: want error")
	}
	// Non-empty target but detached → not-attached error.
	if _, err := v.CreateBackup(ctx, drivers.BackupSpec{VolumeUUID: "b1", SnapshotName: "s", Target: "oci://h/r:t"}); err == nil {
		t.Errorf("backup detached: want error")
	}
	// Attached but bad target schema → registryFor error inside commitOverlay.
	if _, err := v.AttachVolume(ctx, "b1", "h"); err != nil {
		t.Fatalf("AttachVolume: %v", err)
	}
	if _, err := v.CreateBackup(ctx, drivers.BackupSpec{VolumeUUID: "b1", SnapshotName: "s", Target: "bad-url"}); err == nil {
		t.Errorf("backup bad target: want error")
	}
	_ = v.DetachVolume(ctx, "b1", "h")
}

// TestGoVolumeOCIRestoreErrors covers restore-at-version error branches.
func TestGoVolumeOCIRestoreErrors(t *testing.T) {
	reg := newTestRegistry(t)
	defer reg.Close()
	v := NewGoVolume(GoVolumeOptions{StateDir: t.TempDir()})
	ctx := context.Background()

	// Bad ref schema → registryFor parse error.
	if err := v.RestoreBackup(ctx, "not-a-url", drivers.VolumeSpec{UUID: "r1", Format: "oci"}); err == nil {
		t.Errorf("restore bad ref: want error")
	}
	// Resolvable schema but missing manifest → OpenReadOnly error.
	missing := fmt.Sprintf("oci://%s/no/such:tag", strings.TrimPrefix(reg.URL, "http://"))
	if err := v.RestoreBackup(ctx, missing, drivers.VolumeSpec{UUID: "r2", Format: "oci"}); err == nil {
		t.Errorf("restore missing manifest: want error")
	}
	// mkdir collision after a valid open.
	base := bytes.Repeat([]byte("R"), 4096)
	baseRef := freezeBase(t, reg, "weft/img", "v1", base)
	writeFile(t, v.volDir("r3"), []byte("file"))
	if err := v.RestoreBackup(ctx, baseRef, drivers.VolumeSpec{UUID: "r3", Format: "oci"}); err == nil {
		t.Errorf("restore mkdir collision: want error")
	}
}

// TestGoVolumeOCISidecarIOErrors covers the sidecar read/parse and baseRef
// fallbacks directly (paths a happy-path flow never trips).
func TestGoVolumeOCISidecarIOErrors(t *testing.T) {
	v := NewGoVolume(GoVolumeOptions{StateDir: t.TempDir()})

	// readOCISidecar on a missing file → read error.
	if _, err := v.readOCISidecar("ghost"); err == nil {
		t.Errorf("readOCISidecar missing: want error")
	}
	// baseRef on a missing sidecar → "".
	if ref := v.baseRef("ghost"); ref != "" {
		t.Errorf("baseRef missing = %q, want empty", ref)
	}
	// Corrupt sidecar JSON → parse error.
	writeFile(t, v.ociSidecarPath("bad"), []byte("{not json"))
	if _, err := v.readOCISidecar("bad"); err == nil {
		t.Errorf("readOCISidecar corrupt: want error")
	}

	// writeOCISidecar mkdir/rename failure: a file occupies the volume dir, so the
	// temp-file write fails.
	writeFile(t, v.volDir("nodir"), []byte("file"))
	if err := v.writeOCISidecar("nodir", ociSidecar{Base: "x", Current: "x"}); err == nil {
		t.Errorf("writeOCISidecar into file path: want error")
	}
}

// TestGoVolumeOCIOpenOverlayErrors covers openOverlay's pre-OpenReadOnly error
// branches directly (a happy attach never trips them).
func TestGoVolumeOCIOpenOverlayErrors(t *testing.T) {
	v := NewGoVolume(GoVolumeOptions{StateDir: t.TempDir()})
	ctx := context.Background()

	// No sidecar → readOCISidecar error.
	if _, _, err := v.openOverlay(ctx, "ghost"); err == nil {
		t.Errorf("openOverlay no sidecar: want error")
	}
	// Sidecar with an unparseable Current ref → registryFor error.
	writeFile(t, v.ociSidecarPath("badref"), mustJSON(ociSidecar{Base: "x", Current: "not-a-url"}))
	if _, _, err := v.openOverlay(ctx, "badref"); err == nil {
		t.Errorf("openOverlay bad current ref: want error")
	}
}

// TestGoVolumeOCIWriteSidecarRenameFails drives writeOCISidecar's rename-failure
// branch: a directory occupies the final sidecar path, so renaming the temp file
// onto it fails after the temp write succeeds.
func TestGoVolumeOCIWriteSidecarRenameFails(t *testing.T) {
	v := NewGoVolume(GoVolumeOptions{StateDir: t.TempDir()})
	if err := os.MkdirAll(v.ociSidecarPath("ren"), 0o755); err != nil {
		t.Fatalf("mkdir sidecar-as-dir: %v", err)
	}
	if err := v.writeOCISidecar("ren", ociSidecar{Base: "b", Current: "c"}); err == nil {
		t.Errorf("writeOCISidecar onto a directory: want rename error")
	}
}

// TestGoVolumeOCICommitSidecarWriteFails drives commitOverlay's final branch: the
// Commit succeeds (stub) but advancing the sidecar fails (its path is a dir).
func TestGoVolumeOCICommitSidecarWriteFails(t *testing.T) {
	reg := newTestRegistry(t)
	defer reg.Close()
	v := NewGoVolume(GoVolumeOptions{StateDir: t.TempDir()})
	ctx := context.Background()

	base := bytes.Repeat([]byte("W"), 4096)
	baseRef := freezeBase(t, reg, "weft/img", "v1", base)
	if err := v.EnsureVolume(ctx, drivers.VolumeSpec{UUID: "cw", Name: baseRef, Format: "oci"}); err != nil {
		t.Fatalf("EnsureVolume: %v", err)
	}
	if _, err := v.AttachVolume(ctx, "cw", "h"); err != nil {
		t.Fatalf("AttachVolume: %v", err)
	}
	// Replace the sidecar file with a directory so the post-commit write fails.
	if err := os.Remove(v.ociSidecarPath("cw")); err != nil {
		t.Fatalf("rm sidecar: %v", err)
	}
	if err := os.MkdirAll(v.ociSidecarPath("cw"), 0o755); err != nil {
		t.Fatalf("mkdir sidecar-as-dir: %v", err)
	}
	// Stub Commit to succeed so the only failure is the sidecar write.
	origCommit := ociCommit
	t.Cleanup(func() { ociCommit = origCommit })
	ociCommit = func(context.Context, *oci.Overlay, *registry.Client, string) (string, error) {
		return "sha256:deadbeef", nil
	}
	regHost := strings.TrimPrefix(reg.URL, "http://")
	target := fmt.Sprintf("oci://%s/weft/img:v2", regHost)
	if _, _, err := v.commitOverlay(ctx, "cw", target); err == nil {
		t.Errorf("commitOverlay sidecar-write-fails: want error")
	}
	_ = v.DetachVolume(ctx, "cw", "h")
}

// mustJSON marshals v or fails the test.
func mustJSON(v ociSidecar) []byte {
	b, _ := json.Marshal(v)
	return b
}

// TestRetagOCIRef covers the tag-rewriting helper directly.
func TestRetagOCIRef(t *testing.T) {
	cases := []struct {
		ref, tag, want string
		wantErr        bool
	}{
		{"oci://reg/weft/img:v1", "v2", "oci://reg/weft/img:v2", false},
		{"oci://127.0.0.1:5000/r:old", "new", "oci://127.0.0.1:5000/r:new", false},
		{"oci://reg/weft/img:v1", "", "", true},    // empty new tag
		{"https://reg/r:t", "v2", "", true},        // not oci://
		{"oci://host/repo-no-tag", "v2", "", true}, // no tag colon after path
	}
	for _, c := range cases {
		got, err := retagOCIRef(c.ref, c.tag)
		if c.wantErr {
			if err == nil {
				t.Errorf("retag(%q,%q): want error, got %q", c.ref, c.tag, got)
			}
			continue
		}
		if err != nil || got != c.want {
			t.Errorf("retag(%q,%q) = %q,%v want %q", c.ref, c.tag, got, err, c.want)
		}
	}
}

// TestIsOCISpec covers the mode predicate.
func TestIsOCISpec(t *testing.T) {
	if !isOCISpec(drivers.VolumeSpec{Format: "oci"}) {
		t.Errorf("oci format: want true")
	}
	if isOCISpec(drivers.VolumeSpec{Format: "raw"}) {
		t.Errorf("raw format: want false")
	}
}

// TestGoVolumeOCICloseReleasesImage exercises nbdServer.close's overlay/img
// release path directly with a synthesized server (no listener traffic).
func TestGoVolumeOCICloseReleasesImage(t *testing.T) {
	reg := newTestRegistry(t)
	defer reg.Close()
	v := NewGoVolume(GoVolumeOptions{StateDir: t.TempDir()})

	base := bytes.Repeat([]byte("C"), 4096)
	baseRef := freezeBase(t, reg, "weft/img", "v1", base)
	if err := v.EnsureVolume(context.Background(), drivers.VolumeSpec{UUID: "cl", Name: baseRef, Format: "oci"}); err != nil {
		t.Fatalf("EnsureVolume: %v", err)
	}
	img, overlay, err := v.openOverlay(context.Background(), "cl")
	if err != nil {
		t.Fatalf("openOverlay: %v", err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	done := make(chan struct{})
	close(done)
	s := &nbdServer{ln: ln, overlay: overlay, img: img, done: done}
	if err := s.close(); err != nil {
		t.Errorf("close: %v", err)
	}
}
