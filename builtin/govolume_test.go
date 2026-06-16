package builtin

// govolume_test.go exercises the go-volumes-backed volume driver end to end:
//   - EnsureVolume → AttachVolume returns an nbd:// URI a minimal in-test NBD
//     client connects to, reads, and writes (round-tripping bytes through the
//     CoW pool); DetachVolume stops the export.
//   - CreateSnapshot / ListSnapshots over the pool's CoW branching.
//   - CreateBackup (oci.Freeze) → RestoreBackup (oci.OpenReadOnly + import)
//     round-trip against an httptest OCI Distribution v2 registry.
//
// Everything runs in-process (loopback NBD + httptest registry + t.TempDir
// pool files), so the suite is CGO-free, needs no external binaries, and runs
// from any CWD.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	drivers "github.com/openweft/weft-drivers"
)

func newTestGoVolume(t *testing.T) *GoVolume {
	t.Helper()
	return NewGoVolume(GoVolumeOptions{StateDir: t.TempDir()})
}

func TestGoVolumeNameLocal(t *testing.T) {
	v := newTestGoVolume(t)
	if v.Name() != "govolume" {
		t.Errorf("Name = %q, want govolume", v.Name())
	}
	if !v.Local() {
		t.Errorf("Local = false, want true (single-node niche)")
	}
	hi, err := v.HostInfo(context.Background())
	if err != nil {
		t.Fatalf("HostInfo: %v", err)
	}
	if hi.Hypervisor == "" {
		t.Errorf("HostInfo.Hypervisor empty")
	}
}

func TestGoVolumeEnsureIdempotentAndErrors(t *testing.T) {
	v := newTestGoVolume(t)
	ctx := context.Background()
	spec := drivers.VolumeSpec{UUID: "vol-a", SizeGiB: 1, Format: "raw"}

	if err := v.EnsureVolume(ctx, spec); err != nil {
		t.Fatalf("EnsureVolume: %v", err)
	}
	// Idempotent re-run.
	if err := v.EnsureVolume(ctx, spec); err != nil {
		t.Fatalf("EnsureVolume (2nd): %v", err)
	}
	// Same size re-run is fine; a smaller size is also fine (>= want check),
	// but a larger size than the fixed pool volume must be rejected.
	big := drivers.VolumeSpec{UUID: "vol-a", SizeGiB: 64}
	if err := v.EnsureVolume(ctx, big); err == nil {
		t.Errorf("EnsureVolume grow beyond fixed size: want error, got nil")
	}

	if err := v.EnsureVolume(ctx, drivers.VolumeSpec{UUID: "", SizeGiB: 1}); err == nil {
		t.Errorf("EnsureVolume empty uuid: want error")
	}
	if err := v.EnsureVolume(ctx, drivers.VolumeSpec{UUID: "x", SizeGiB: 0}); err == nil {
		t.Errorf("EnsureVolume zero size: want error")
	}
}

func TestGoVolumeDestroy(t *testing.T) {
	v := newTestGoVolume(t)
	ctx := context.Background()
	spec := drivers.VolumeSpec{UUID: "vol-d", SizeGiB: 1}
	if err := v.EnsureVolume(ctx, spec); err != nil {
		t.Fatalf("EnsureVolume: %v", err)
	}
	// Attach so Destroy must also tear down the running server.
	if _, err := v.AttachVolume(ctx, "vol-d", "host-1"); err != nil {
		t.Fatalf("AttachVolume: %v", err)
	}
	if err := v.DestroyVolume(ctx, "vol-d"); err != nil {
		t.Fatalf("DestroyVolume: %v", err)
	}
	if err := v.DestroyVolume(ctx, "vol-d"); err != nil {
		t.Fatalf("DestroyVolume (missing): %v", err)
	}
	if err := v.DestroyVolume(ctx, ""); err != nil {
		t.Fatalf("DestroyVolume empty: %v", err)
	}
}

func TestGoVolumeAttachReadWriteRoundTrip(t *testing.T) {
	v := newTestGoVolume(t)
	ctx := context.Background()
	if err := v.EnsureVolume(ctx, drivers.VolumeSpec{UUID: "vol-rw", SizeGiB: 1}); err != nil {
		t.Fatalf("EnsureVolume: %v", err)
	}

	att, err := v.AttachVolume(ctx, "vol-rw", "host-1")
	if err != nil {
		t.Fatalf("AttachVolume: %v", err)
	}
	if !strings.HasPrefix(att.BackingPath, "nbd://127.0.0.1:") {
		t.Fatalf("BackingPath = %q, want nbd://127.0.0.1:PORT", att.BackingPath)
	}
	// Idempotent attach returns the same address.
	att2, err := v.AttachVolume(ctx, "vol-rw", "host-1")
	if err != nil {
		t.Fatalf("AttachVolume (2nd): %v", err)
	}
	if att2.BackingPath != att.BackingPath {
		t.Errorf("re-attach addr %q != %q", att2.BackingPath, att.BackingPath)
	}

	addr := strings.TrimPrefix(att.BackingPath, "nbd://")

	// Write a pattern over NBD, then read it back over a fresh connection to
	// prove it persisted into the pool.
	want := []byte("go-volumes microVM disk over in-process NBD!")
	c := dialNBD(t, addr)
	c.write(t, 4096, want)
	c.disconnect(t)

	c2 := dialNBD(t, addr)
	got := c2.read(t, 4096, uint32(len(want)))
	c2.disconnect(t)
	if !bytes.Equal(got, want) {
		t.Errorf("NBD read mismatch: got %q want %q", got, want)
	}

	// Detach stops the server: a subsequent dial must fail.
	if err := v.DetachVolume(ctx, "vol-rw", "host-1"); err != nil {
		t.Fatalf("DetachVolume: %v", err)
	}
	if conn, derr := net.Dial("tcp", addr); derr == nil {
		conn.Close()
		t.Errorf("dial after detach succeeded, want refused")
	}
	// Idempotent detach.
	if err := v.DetachVolume(ctx, "vol-rw", "host-1"); err != nil {
		t.Fatalf("DetachVolume (2nd): %v", err)
	}
}

func TestGoVolumeAttachErrors(t *testing.T) {
	v := newTestGoVolume(t)
	ctx := context.Background()
	if _, err := v.AttachVolume(ctx, "", "h"); err == nil {
		t.Errorf("AttachVolume empty uuid: want error")
	}
	if _, err := v.AttachVolume(ctx, "missing", "h"); err == nil {
		t.Errorf("AttachVolume missing pool: want error")
	}
}

func TestGoVolumeSnapshots(t *testing.T) {
	v := newTestGoVolume(t)
	ctx := context.Background()
	if err := v.EnsureVolume(ctx, drivers.VolumeSpec{UUID: "vol-s", SizeGiB: 1}); err != nil {
		t.Fatalf("EnsureVolume: %v", err)
	}

	// No snapshots yet.
	snaps, err := v.ListSnapshots(ctx, "vol-s")
	if err != nil {
		t.Fatalf("ListSnapshots: %v", err)
	}
	if len(snaps) != 0 {
		t.Fatalf("ListSnapshots: got %d, want 0", len(snaps))
	}

	s1, err := v.CreateSnapshot(ctx, drivers.SnapshotSpec{VolumeUUID: "vol-s", Name: "snap1"})
	if err != nil {
		t.Fatalf("CreateSnapshot: %v", err)
	}
	if s1.Name != "snap1" || !s1.UserCreated {
		t.Errorf("snapshot descriptor = %+v", s1)
	}
	// Auto-named snapshot.
	s2, err := v.CreateSnapshot(ctx, drivers.SnapshotSpec{VolumeUUID: "vol-s"})
	if err != nil {
		t.Fatalf("CreateSnapshot auto-name: %v", err)
	}
	if !strings.HasPrefix(s2.Name, "snap-") {
		t.Errorf("auto name = %q, want snap-*", s2.Name)
	}

	snaps, err = v.ListSnapshots(ctx, "vol-s")
	if err != nil {
		t.Fatalf("ListSnapshots: %v", err)
	}
	if len(snaps) != 2 {
		t.Fatalf("ListSnapshots: got %d, want 2", len(snaps))
	}

	// Reserved name + missing-volume errors.
	if _, err := v.CreateSnapshot(ctx, drivers.SnapshotSpec{VolumeUUID: "vol-s", Name: diskVolumeName}); err == nil {
		t.Errorf("CreateSnapshot reserved name: want error")
	}
	if _, err := v.CreateSnapshot(ctx, drivers.SnapshotSpec{VolumeUUID: ""}); err == nil {
		t.Errorf("CreateSnapshot empty uuid: want error")
	}
	if _, err := v.ListSnapshots(ctx, ""); err == nil {
		t.Errorf("ListSnapshots empty uuid: want error")
	}

	// Delete/Revert are unsupported (documented primitive gap).
	if err := v.DeleteSnapshot(ctx, "vol-s", "snap1"); err != drivers.ErrUnsupported {
		t.Errorf("DeleteSnapshot = %v, want ErrUnsupported", err)
	}
	if err := v.RevertSnapshot(ctx, "vol-s", "snap1"); err != drivers.ErrUnsupported {
		t.Errorf("RevertSnapshot = %v, want ErrUnsupported", err)
	}
}

func TestGoVolumeBackupRestoreRoundTrip(t *testing.T) {
	reg := newTestRegistry(t)
	defer reg.Close()

	v := NewGoVolume(GoVolumeOptions{StateDir: t.TempDir()})
	ctx := context.Background()
	if err := v.EnsureVolume(ctx, drivers.VolumeSpec{UUID: "vol-b", SizeGiB: 1}); err != nil {
		t.Fatalf("EnsureVolume: %v", err)
	}

	// Write known content via NBD, then snapshot it (backups always come from a
	// snapshot, per the interface contract).
	att, err := v.AttachVolume(ctx, "vol-b", "host-1")
	if err != nil {
		t.Fatalf("AttachVolume: %v", err)
	}
	addr := strings.TrimPrefix(att.BackingPath, "nbd://")
	payload := bytes.Repeat([]byte("WEFT"), 4096) // 16 KiB non-zero
	c := dialNBD(t, addr)
	c.write(t, 0, payload)
	c.disconnect(t)
	if err := v.DetachVolume(ctx, "vol-b", "host-1"); err != nil {
		t.Fatalf("DetachVolume: %v", err)
	}

	if _, err := v.CreateSnapshot(ctx, drivers.SnapshotSpec{VolumeUUID: "vol-b", Name: "bk"}); err != nil {
		t.Fatalf("CreateSnapshot: %v", err)
	}

	regHost := strings.TrimPrefix(reg.URL, "http://")
	target := fmt.Sprintf("oci://%s/weft/backups:vol-b-bk", regHost)

	bk, err := v.CreateBackup(ctx, drivers.BackupSpec{
		VolumeUUID:   "vol-b",
		SnapshotName: "bk",
		Target:       target,
	})
	if err != nil {
		t.Fatalf("CreateBackup: %v", err)
	}
	if bk.URL != target || bk.State != "complete" {
		t.Errorf("backup descriptor = %+v", bk)
	}

	// Restore into a NEW volume and read its bytes back.
	if err := v.RestoreBackup(ctx, target, drivers.VolumeSpec{UUID: "vol-r", SizeGiB: 1}); err != nil {
		t.Fatalf("RestoreBackup: %v", err)
	}
	// Idempotent restore.
	if err := v.RestoreBackup(ctx, target, drivers.VolumeSpec{UUID: "vol-r", SizeGiB: 1}); err != nil {
		t.Fatalf("RestoreBackup (2nd): %v", err)
	}

	att2, err := v.AttachVolume(ctx, "vol-r", "host-2")
	if err != nil {
		t.Fatalf("AttachVolume restored: %v", err)
	}
	addr2 := strings.TrimPrefix(att2.BackingPath, "nbd://")
	c2 := dialNBD(t, addr2)
	got := c2.read(t, 0, uint32(len(payload)))
	c2.disconnect(t)
	if !bytes.Equal(got, payload) {
		t.Errorf("restored content mismatch (len got %d want %d)", len(got), len(payload))
	}
	_ = v.DetachVolume(ctx, "vol-r", "host-2")
}

func TestGoVolumeBackupRestoreErrors(t *testing.T) {
	v := NewGoVolume(GoVolumeOptions{StateDir: t.TempDir()})
	ctx := context.Background()

	if _, err := v.CreateBackup(ctx, drivers.BackupSpec{VolumeUUID: "", SnapshotName: "s", Target: "oci://h/r:t"}); err == nil {
		t.Errorf("CreateBackup empty uuid: want error")
	}
	if _, err := v.CreateBackup(ctx, drivers.BackupSpec{VolumeUUID: "v", SnapshotName: "s", Target: ""}); err == nil {
		t.Errorf("CreateBackup empty target: want error")
	}
	if err := v.RestoreBackup(ctx, "oci://h/r:t", drivers.VolumeSpec{UUID: ""}); err == nil {
		t.Errorf("RestoreBackup empty uuid: want error")
	}

	// List/Delete backups are unsupported.
	if _, err := v.ListBackups(ctx, "t", "v"); err != drivers.ErrUnsupported {
		t.Errorf("ListBackups = %v, want ErrUnsupported", err)
	}
	if err := v.DeleteBackup(ctx, "u"); err != drivers.ErrUnsupported {
		t.Errorf("DeleteBackup = %v, want ErrUnsupported", err)
	}
}

func TestGoVolumeCreateBackupMissingSnapshotAndPool(t *testing.T) {
	v := NewGoVolume(GoVolumeOptions{StateDir: t.TempDir()})
	ctx := context.Background()

	// Pool does not exist → open error.
	if _, err := v.CreateBackup(ctx, drivers.BackupSpec{VolumeUUID: "nope", SnapshotName: "s", Target: "oci://127.0.0.1:1/r:t"}); err == nil {
		t.Errorf("CreateBackup missing pool: want error")
	}

	if err := v.EnsureVolume(ctx, drivers.VolumeSpec{UUID: "vol-x", SizeGiB: 1}); err != nil {
		t.Fatalf("EnsureVolume: %v", err)
	}
	// Snapshot does not exist → open-snapshot error.
	if _, err := v.CreateBackup(ctx, drivers.BackupSpec{VolumeUUID: "vol-x", SnapshotName: "ghost", Target: "oci://127.0.0.1:1/r:t"}); err == nil {
		t.Errorf("CreateBackup missing snapshot: want error")
	}
	// Valid snapshot but unreachable registry → freeze/push error.
	if _, err := v.CreateSnapshot(ctx, drivers.SnapshotSpec{VolumeUUID: "vol-x", Name: "s"}); err != nil {
		t.Fatalf("CreateSnapshot: %v", err)
	}
	if _, err := v.CreateBackup(ctx, drivers.BackupSpec{VolumeUUID: "vol-x", SnapshotName: "s", Target: "oci://127.0.0.1:1/r:t"}); err == nil {
		// Note: an all-zero snapshot pushes no chunk blobs, only the config +
		// manifest, which still require the registry — so this must error.
		t.Errorf("CreateBackup unreachable registry: want error")
	}
}

func TestGoVolumeRestoreErrors(t *testing.T) {
	v := NewGoVolume(GoVolumeOptions{StateDir: t.TempDir()})
	ctx := context.Background()
	// Unreachable registry → OpenReadOnly error.
	if err := v.RestoreBackup(ctx, "oci://127.0.0.1:1/r:t", drivers.VolumeSpec{UUID: "r1", SizeGiB: 1}); err == nil {
		t.Errorf("RestoreBackup unreachable registry: want error")
	}
	// Bad target schema → parse error.
	if err := v.RestoreBackup(ctx, "not-a-url", drivers.VolumeSpec{UUID: "r2", SizeGiB: 1}); err == nil {
		t.Errorf("RestoreBackup bad target: want error")
	}
}

func TestGoVolumeSnapshotMissingPool(t *testing.T) {
	v := NewGoVolume(GoVolumeOptions{StateDir: t.TempDir()})
	ctx := context.Background()
	if _, err := v.CreateSnapshot(ctx, drivers.SnapshotSpec{VolumeUUID: "ghost", Name: "s"}); err == nil {
		t.Errorf("CreateSnapshot missing pool: want error")
	}
	if _, err := v.ListSnapshots(ctx, "ghost"); err == nil {
		t.Errorf("ListSnapshots missing pool: want error")
	}
}

func TestParseOCITarget(t *testing.T) {
	cases := []struct {
		in      string
		base    string
		repo    string
		ref     string
		wantErr bool
	}{
		{"oci://reg.example.com/weft/backups:v1", "https://reg.example.com", "weft/backups", "v1", false},
		{"oci://127.0.0.1:5000/weft/b:v1", "http://127.0.0.1:5000", "weft/b", "v1", false},
		{"oci://localhost/r:t", "http://localhost", "r", "t", false},
		{"oci://host.local/r/s:t", "http://host.local", "r/s", "t", false},
		{"https://nope/r:t", "", "", "", true},
		{"oci://hostonly", "", "", "", true},
		{"oci://host/repo-no-tag", "", "", "", true},
		{"oci:///repo:tag", "", "", "", true},
	}
	for _, c := range cases {
		base, repo, ref, err := parseOCITarget(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("%q: want error, got base=%q repo=%q ref=%q", c.in, base, repo, ref)
			}
			continue
		}
		if err != nil {
			t.Errorf("%q: unexpected error %v", c.in, err)
			continue
		}
		if base != c.base || repo != c.repo || ref != c.ref {
			t.Errorf("%q: got (%q,%q,%q), want (%q,%q,%q)", c.in, base, repo, ref, c.base, c.repo, c.ref)
		}
	}
}

func TestRegistryFor(t *testing.T) {
	v := NewGoVolume(GoVolumeOptions{StateDir: t.TempDir()})
	if _, _, err := v.registryFor(""); err == nil {
		t.Errorf("registryFor empty: want error")
	}
	cl, ref, err := v.registryFor("oci://127.0.0.1:5000/weft/b:tag")
	if err != nil {
		t.Fatalf("registryFor: %v", err)
	}
	if cl.BaseURL != "http://127.0.0.1:5000" || cl.Repository != "weft/b" || ref != "tag" {
		t.Errorf("registryFor = %+v ref=%q", cl, ref)
	}
}

// ── minimal in-test NBD client (export-name flow) ───────────────────────────

const (
	tNBDMAGIC       = 0x4e42444d41474943 // "NBDMAGIC"
	tIHAVEOPT       = 0x49484156454f5054 // "IHAVEOPT"
	tClientFixed    = 1 << 0
	tOptExportName  = 1
	tCmdRead        = 0
	tCmdWrite       = 1
	tCmdDisc        = 2
	tRequestMagic   = 0x25609513
	tSimpleReplyMag = 0x67446698
)

type nbdTestClient struct {
	conn net.Conn
}

// dialNBD connects to addr and completes the fixed-newstyle handshake selecting
// the default export via NBD_OPT_EXPORT_NAME.
func dialNBD(t *testing.T, addr string) *nbdTestClient {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial nbd %s: %v", addr, err)
	}
	c := &nbdTestClient{conn: conn}

	// Server greeting: NBDMAGIC(8) IHAVEOPT(8) flags(2).
	greeting := make([]byte, 18)
	if _, err := io.ReadFull(conn, greeting); err != nil {
		t.Fatalf("read greeting: %v", err)
	}
	if binary.BigEndian.Uint64(greeting[0:8]) != tNBDMAGIC ||
		binary.BigEndian.Uint64(greeting[8:16]) != tIHAVEOPT {
		t.Fatalf("bad greeting magic")
	}
	// Client flags (FIXED_NEWSTYLE; zeroes allowed so the 124-byte pad is sent).
	if err := binary.Write(conn, binary.BigEndian, uint32(tClientFixed)); err != nil {
		t.Fatalf("write client flags: %v", err)
	}
	// NBD_OPT_EXPORT_NAME with empty name → default export.
	hdr := make([]byte, 16)
	binary.BigEndian.PutUint64(hdr[0:8], tIHAVEOPT)
	binary.BigEndian.PutUint32(hdr[8:12], tOptExportName)
	binary.BigEndian.PutUint32(hdr[12:16], 0) // name length 0
	if _, err := conn.Write(hdr); err != nil {
		t.Fatalf("write opt: %v", err)
	}
	// Reply: size(8) flags(2) + 124 zero pad.
	reply := make([]byte, 10+124)
	if _, err := io.ReadFull(conn, reply); err != nil {
		t.Fatalf("read export reply: %v", err)
	}
	return c
}

func (c *nbdTestClient) write(t *testing.T, off uint64, data []byte) {
	t.Helper()
	c.sendRequest(t, tCmdWrite, 0x1111, off, uint32(len(data)), data)
	c.expectReply(t, 0x1111)
}

func (c *nbdTestClient) read(t *testing.T, off uint64, length uint32) []byte {
	t.Helper()
	c.sendRequest(t, tCmdRead, 0x2222, off, length, nil)
	errno, data := c.recvReply(t, 0x2222, int(length))
	if errno != 0 {
		t.Fatalf("nbd read errno %d", errno)
	}
	return data
}

func (c *nbdTestClient) sendRequest(t *testing.T, cmd uint32, handle, off uint64, length uint32, body []byte) {
	t.Helper()
	req := make([]byte, 28)
	binary.BigEndian.PutUint32(req[0:4], tRequestMagic)
	binary.BigEndian.PutUint32(req[4:8], cmd)
	binary.BigEndian.PutUint64(req[8:16], handle)
	binary.BigEndian.PutUint64(req[16:24], off)
	binary.BigEndian.PutUint32(req[24:28], length)
	if _, err := c.conn.Write(req); err != nil {
		t.Fatalf("write request: %v", err)
	}
	if len(body) > 0 {
		if _, err := c.conn.Write(body); err != nil {
			t.Fatalf("write body: %v", err)
		}
	}
}

func (c *nbdTestClient) expectReply(t *testing.T, handle uint64) {
	t.Helper()
	errno, _ := c.recvReply(t, handle, 0)
	if errno != 0 {
		t.Fatalf("nbd reply errno %d", errno)
	}
}

func (c *nbdTestClient) recvReply(t *testing.T, wantHandle uint64, dataLen int) (uint32, []byte) {
	t.Helper()
	hdr := make([]byte, 16)
	if _, err := io.ReadFull(c.conn, hdr); err != nil {
		t.Fatalf("read reply header: %v", err)
	}
	if binary.BigEndian.Uint32(hdr[0:4]) != tSimpleReplyMag {
		t.Fatalf("bad reply magic")
	}
	errno := binary.BigEndian.Uint32(hdr[4:8])
	if h := binary.BigEndian.Uint64(hdr[8:16]); h != wantHandle {
		t.Fatalf("reply handle %#x != %#x", h, wantHandle)
	}
	var data []byte
	if dataLen > 0 && errno == 0 {
		data = make([]byte, dataLen)
		if _, err := io.ReadFull(c.conn, data); err != nil {
			t.Fatalf("read reply data: %v", err)
		}
	}
	return errno, data
}

func (c *nbdTestClient) disconnect(t *testing.T) {
	t.Helper()
	req := make([]byte, 28)
	binary.BigEndian.PutUint32(req[0:4], tRequestMagic)
	binary.BigEndian.PutUint32(req[4:8], tCmdDisc)
	_, _ = c.conn.Write(req)
	c.conn.Close()
}

// ── minimal in-memory OCI Distribution v2 registry (httptest) ───────────────

type testRegistry struct {
	*httptest.Server
	mu        sync.Mutex
	blobs     map[string][]byte // digest -> bytes
	manifests map[string][]byte // ref -> manifest bytes
}

func newTestRegistry(t *testing.T) *testRegistry {
	t.Helper()
	r := &testRegistry{blobs: map[string][]byte{}, manifests: map[string][]byte{}}
	r.Server = httptest.NewServer(http.HandlerFunc(r.handle))
	return r
}

func digestOf(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// handle implements just enough of the v2 API for go-volumes/oci: HEAD/GET
// blob, POST+PUT monolithic blob upload, GET/PUT manifest.
func (r *testRegistry) handle(w http.ResponseWriter, req *http.Request) {
	p := req.URL.Path
	switch {
	case p == "/v2/":
		w.WriteHeader(http.StatusOK)

	case strings.Contains(p, "/blobs/uploads/"):
		// POST initiates an upload; respond with a Location to PUT to.
		if req.Method == http.MethodPost {
			w.Header().Set("Location", "/v2/upload/session")
			w.WriteHeader(http.StatusAccepted)
			return
		}
		fallthrough

	case strings.HasPrefix(p, "/v2/upload/"):
		// PUT finalises: body is the blob, ?digest=... names it.
		body, _ := io.ReadAll(req.Body)
		dg := req.URL.Query().Get("digest")
		if dg == "" {
			dg = digestOf(body)
		}
		r.mu.Lock()
		r.blobs[dg] = body
		r.mu.Unlock()
		w.Header().Set("Docker-Content-Digest", dg)
		w.WriteHeader(http.StatusCreated)

	case strings.Contains(p, "/blobs/"):
		dg := p[strings.LastIndex(p, "/blobs/")+len("/blobs/"):]
		r.mu.Lock()
		b, ok := r.blobs[dg]
		r.mu.Unlock()
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if req.Method == http.MethodHead {
			w.Header().Set("Docker-Content-Digest", dg)
			w.WriteHeader(http.StatusOK)
			return
		}
		w.Write(b)

	case strings.Contains(p, "/manifests/"):
		ref := p[strings.LastIndex(p, "/manifests/")+len("/manifests/"):]
		if req.Method == http.MethodPut {
			body, _ := io.ReadAll(req.Body)
			r.mu.Lock()
			r.manifests[ref] = body
			dg := digestOf(body)
			r.manifests[dg] = body
			r.mu.Unlock()
			w.Header().Set("Docker-Content-Digest", dg)
			w.WriteHeader(http.StatusCreated)
			return
		}
		r.mu.Lock()
		b, ok := r.manifests[ref]
		r.mu.Unlock()
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
		w.Header().Set("Docker-Content-Digest", digestOf(b))
		w.Write(b)

	default:
		w.WriteHeader(http.StatusNotFound)
	}
}
