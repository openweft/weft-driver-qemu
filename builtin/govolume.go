package builtin

// govolume.go is a pure-Go (CGO=0) drivers.VolumeDriver backed by the
// go-volumes storage stack. It is the LOCAL microVM-disk backend: a volume is
// a copy-on-write pool carved out of a single host file under the driver state
// dir, attached to a QEMU VM over an in-process NBD export (no qemu-nbd, no
// kernel nbd module) so the bundle can wire it as
// `-drive file=nbd://127.0.0.1:PORT,if=virtio`.
//
// It is COMPLEMENTARY to — not a replacement for — Longhorn-backed distributed
// persistent volumes: Local() is true, so the scheduler binds these volumes to
// the host that created them. Snapshots use the pool's CoW branching; backups
// freeze a volume to an OCI registry (immutable, content-deduplicated,
// distributable) and restore re-materialises it into a fresh pool.
//
// Lifecycle of the on-host layout, rooted at <StateDir>:
//
//	<StateDir>/<volumeUUID>/pool.bin   — the go-volumes pool file
//
// Each pool holds exactly one logical volume named diskVolumeName ("disk").
// Snapshots are extra read-only volumes in the same pool, keyed by name.

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	volume "github.com/go-volumes/interface"
	"github.com/go-volumes/nbd"
	"github.com/go-volumes/oci"
	"github.com/go-volumes/oci/registry"
	"github.com/go-volumes/pool"
	drivers "github.com/openweft/weft-drivers"
)

// diskVolumeName is the single logical volume every per-UUID pool carries. The
// snapshot namespace is the rest of the pool's volume list.
const diskVolumeName = "disk"

// poolOverheadGiB is extra capacity reserved on top of the requested size so
// copy-on-write writes (after a snapshot diverges the block map) and snapshot
// metadata have room to allocate without hitting ErrPoolFull. The pool file is
// sparse on creation, so this costs no real disk until blocks are touched.
const poolOverheadGiB = 1

// gib is one GiB in bytes.
const gib = int64(1) << 30

// GoVolumeOptions configures the go-volumes-backed volume driver.
type GoVolumeOptions struct {
	Options
	// StateDir is the on-host root for <StateDir>/<uuid>/pool.bin.
	StateDir string
	// BackupRegistry is the OCI Distribution v2 registry root used as the
	// default backup store when a BackupSpec.Target is empty (e.g.
	// "https://registry.example.com"). Empty disables the default; a backup
	// then requires a target encoded in BackupSpec.Target.
	BackupRegistry string
}

// GoVolume implements drivers.VolumeDriver against the go-volumes stack. It is
// registered alongside the "file" driver and only used when explicitly
// selected (Name() == "govolume"), so the existing file backend is untouched.
type GoVolume struct {
	opts      Options
	stateDir  string
	backupReg string

	mu      sync.Mutex
	servers map[string]*nbdServer // key: volumeUUID|hostUUID
}

// nbdServer tracks one running in-process NBD export for an attached volume.
type nbdServer struct {
	ln   net.Listener
	pool *pool.Pool
	addr string        // "nbd://127.0.0.1:PORT"
	done chan struct{} // closed when the Serve goroutine has fully drained
}

// NewGoVolume constructs the go-volumes volume driver.
func NewGoVolume(o GoVolumeOptions) *GoVolume {
	return &GoVolume{
		opts:      o.Options,
		stateDir:  o.StateDir,
		backupReg: o.BackupRegistry,
		servers:   map[string]*nbdServer{},
	}
}

var _ drivers.VolumeDriver = (*GoVolume)(nil)

func (v *GoVolume) Name() string { return "govolume" }

// Local reports true: a pool file lives on this host, so the volume can only
// attach to VMs scheduled here. This is the deliberate single-node niche.
func (v *GoVolume) Local() bool { return true }

func (v *GoVolume) HostInfo(context.Context) (drivers.HostInfo, error) {
	return hostInfoFor(v.opts), nil
}

// volDir is the per-volume state directory.
func (v *GoVolume) volDir(uuid string) string { return filepath.Join(v.stateDir, uuid) }

// poolPath is the per-volume pool file path.
func (v *GoVolume) poolPath(uuid string) string { return filepath.Join(v.volDir(uuid), "pool.bin") }

// attachKey keys the running-server map by (volume, host).
func attachKey(volumeUUID, hostUUID string) string { return volumeUUID + "|" + hostUUID }

// EnsureVolume creates the backing pool + its single "disk" volume sized per
// the spec. Idempotent: an existing pool that already carries "disk" is a
// no-op (the grow-only contract is enforced upstream by the registry; the pool
// volume size is fixed at creation in this version, so a re-Ensure with a
// larger size that the pool can't satisfy is reported rather than silently
// ignored).
func (v *GoVolume) EnsureVolume(_ context.Context, spec drivers.VolumeSpec) error {
	if spec.UUID == "" {
		return fmt.Errorf("govolume EnsureVolume: empty volume uuid")
	}
	if spec.SizeGiB <= 0 {
		return fmt.Errorf("govolume EnsureVolume: size must be > 0 GiB")
	}
	path := v.poolPath(spec.UUID)
	wantBytes := int64(spec.SizeGiB) * gib

	// Already provisioned? Open and verify the disk volume is present + sized.
	if fileExists(path) {
		p, err := pool.Open(path)
		if err != nil {
			return fmt.Errorf("govolume EnsureVolume: open existing pool %s: %w", path, err)
		}
		defer p.Close()
		vol, err := p.OpenVolume(diskVolumeName)
		if err != nil {
			return fmt.Errorf("govolume EnsureVolume: open disk volume: %w", err)
		}
		have, _ := vol.Size()
		if have < wantBytes {
			return fmt.Errorf("govolume EnsureVolume: cannot grow %s from %d to %d bytes (pool volumes are fixed-size)", spec.UUID, have, wantBytes)
		}
		return p.Sync()
	}

	if err := os.MkdirAll(v.volDir(spec.UUID), 0o755); err != nil {
		return fmt.Errorf("govolume EnsureVolume: mkdir: %w", err)
	}
	// Reserve the requested size plus CoW/snapshot headroom. The pool file is
	// sparse, so this is logical, not physical, allocation.
	capBytes := wantBytes + poolOverheadGiB*gib
	p, err := pool.Create(path, capBytes)
	if err != nil {
		return fmt.Errorf("govolume EnsureVolume: create pool %s: %w", path, err)
	}
	defer p.Close()
	if _, err := p.CreateVolume(diskVolumeName, wantBytes); err != nil {
		return fmt.Errorf("govolume EnsureVolume: create disk volume: %w", err)
	}
	return p.Sync()
}

// DestroyVolume removes the backing pool directory. No-op if missing. Any
// running attachment for the volume is stopped first.
func (v *GoVolume) DestroyVolume(_ context.Context, volumeUUID string) error {
	if volumeUUID == "" {
		return nil
	}
	v.mu.Lock()
	for key, srv := range v.servers {
		if srv != nil && hasVolumePrefix(key, volumeUUID) {
			_ = srv.close()
			delete(v.servers, key)
		}
	}
	v.mu.Unlock()
	return os.RemoveAll(v.volDir(volumeUUID))
}

// hasVolumePrefix reports whether an attachKey belongs to volumeUUID.
func hasVolumePrefix(key, volumeUUID string) bool {
	return len(key) > len(volumeUUID) && key[:len(volumeUUID)] == volumeUUID && key[len(volumeUUID)] == '|'
}

// AttachVolume starts an in-process NBD server exporting the volume's "disk"
// over a loopback TCP port and returns its nbd:// URI so the bundle attaches it
// as `-drive file=nbd://127.0.0.1:PORT,if=virtio`. Idempotent per (volume,
// host): a second attach returns the already-running server's address.
func (v *GoVolume) AttachVolume(_ context.Context, volumeUUID, hostUUID string) (drivers.AttachedVolume, error) {
	if volumeUUID == "" {
		return drivers.AttachedVolume{}, fmt.Errorf("govolume AttachVolume: empty volume uuid")
	}
	key := attachKey(volumeUUID, hostUUID)

	v.mu.Lock()
	if srv, ok := v.servers[key]; ok && srv != nil {
		addr := srv.addr
		v.mu.Unlock()
		return drivers.AttachedVolume{BackingPath: addr}, nil
	}
	v.mu.Unlock()

	path := v.poolPath(volumeUUID)
	p, err := pool.Open(path)
	if err != nil {
		return drivers.AttachedVolume{}, fmt.Errorf("govolume AttachVolume: open pool %s: %w", path, err)
	}
	vol, err := p.OpenVolume(diskVolumeName)
	if err != nil {
		p.Close()
		return drivers.AttachedVolume{}, fmt.Errorf("govolume AttachVolume: open disk volume: %w", err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		p.Close()
		return drivers.AttachedVolume{}, fmt.Errorf("govolume AttachVolume: listen: %w", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	addr := fmt.Sprintf("nbd://127.0.0.1:%d", port)

	srv := &nbdServer{ln: ln, pool: p, addr: addr, done: make(chan struct{})}
	// One default export (empty name) carrying the pool volume as a
	// volume.Device — qemu's nbd:// client requests the default export.
	server := &nbd.Server{Exports: []nbd.Export{{Name: "", Device: vol}}}
	go func() {
		// Serve returns net.ErrClosed once the listener is closed AND every
		// in-flight connection goroutine has drained (its internal wg.Wait).
		// Closing done only then lets close() release the pool without racing
		// a live connection's Sync.
		_ = server.Serve(ln)
		close(srv.done)
	}()

	v.mu.Lock()
	// Lost a race? Close ours, return the winner.
	if existing, ok := v.servers[key]; ok && existing != nil {
		v.mu.Unlock()
		_ = srv.close()
		return drivers.AttachedVolume{BackingPath: existing.addr}, nil
	}
	v.servers[key] = srv
	v.mu.Unlock()

	return drivers.AttachedVolume{BackingPath: addr}, nil
}

// close stops the NBD server and releases the pool. It closes the listener
// (unblocking Serve with net.ErrClosed), waits for every connection goroutine
// to drain, and only THEN closes the pool — so a client's last NBD_CMD_DISC,
// which triggers Device.Sync on the pool, can never race the pool's Close.
func (s *nbdServer) close() error {
	err := s.ln.Close()
	<-s.done // Serve has returned: all connection handlers are done.
	if s.pool != nil {
		if perr := s.pool.Close(); perr != nil && err == nil {
			err = perr
		}
	}
	return err
}

// DetachVolume stops the per-(volume,host) NBD server. Idempotent: detaching an
// already-detached volume is a no-op.
func (v *GoVolume) DetachVolume(_ context.Context, volumeUUID, hostUUID string) error {
	key := attachKey(volumeUUID, hostUUID)
	v.mu.Lock()
	srv, ok := v.servers[key]
	if ok {
		delete(v.servers, key)
	}
	v.mu.Unlock()
	if !ok || srv == nil {
		return nil
	}
	return srv.close()
}

// CreateSnapshot freezes the live "disk" under spec.Name (or a timestamped
// name if empty) via the pool's copy-on-write branching. The snapshot is a
// read-only volume sharing the disk's blocks; subsequent writes copy-on-write.
func (v *GoVolume) CreateSnapshot(_ context.Context, spec drivers.SnapshotSpec) (drivers.Snapshot, error) {
	if spec.VolumeUUID == "" {
		return drivers.Snapshot{}, fmt.Errorf("govolume CreateSnapshot: empty volume uuid")
	}
	name := spec.Name
	if name == "" {
		name = fmt.Sprintf("snap-%d", time.Now().UnixNano())
	}
	if name == diskVolumeName {
		return drivers.Snapshot{}, fmt.Errorf("govolume CreateSnapshot: %q is reserved", diskVolumeName)
	}
	p, err := pool.Open(v.poolPath(spec.VolumeUUID))
	if err != nil {
		return drivers.Snapshot{}, fmt.Errorf("govolume CreateSnapshot: open pool: %w", err)
	}
	defer p.Close()

	snap, err := p.Snapshot(diskVolumeName, name)
	if err != nil {
		return drivers.Snapshot{}, fmt.Errorf("govolume CreateSnapshot: %w", err)
	}
	size, _ := snap.Size()
	return drivers.Snapshot{
		VolumeUUID:      spec.VolumeUUID,
		Name:            name,
		SizeBytes:       size,
		CreatedAtUnixNs: time.Now().UnixNano(),
		Labels:          spec.Labels,
		UserCreated:     true,
	}, nil
}

// ListSnapshots returns every snapshot in the volume's pool (every pool volume
// except the live "disk"), name-sorted for a stable order. Empty list + nil
// error means "no snapshots".
func (v *GoVolume) ListSnapshots(_ context.Context, volumeUUID string) ([]drivers.Snapshot, error) {
	if volumeUUID == "" {
		return nil, fmt.Errorf("govolume ListSnapshots: empty volume uuid")
	}
	p, err := pool.Open(v.poolPath(volumeUUID))
	if err != nil {
		return nil, fmt.Errorf("govolume ListSnapshots: open pool: %w", err)
	}
	defer p.Close()

	names := p.Volumes()
	sort.Strings(names)
	out := make([]drivers.Snapshot, 0, len(names))
	for _, n := range names {
		if n == diskVolumeName {
			continue
		}
		vol, err := p.OpenVolume(n)
		if err != nil {
			continue
		}
		size, _ := vol.Size()
		out = append(out, drivers.Snapshot{
			VolumeUUID:  volumeUUID,
			Name:        n,
			SizeBytes:   size,
			UserCreated: true,
		})
	}
	return out, nil
}

// DeleteSnapshot and RevertSnapshot are unsupported: the published go-volumes
// pool API offers CoW branch creation (Snapshot/Clone) but no in-place delete,
// rename, or rollback primitive, so there is no correct way to drop a
// snapshot's delta or swing the live volume back onto it without those. Distinct
// from "no snapshots" — see the ErrUnsupported contract in drivers.VolumeDriver.
func (v *GoVolume) DeleteSnapshot(context.Context, string, string) error {
	return drivers.ErrUnsupported
}

func (v *GoVolume) RevertSnapshot(context.Context, string, string) error {
	return drivers.ErrUnsupported
}

// CreateBackup freezes the named snapshot's contents to an OCI registry: each
// unique chunk is content-addressed and pushed once (holes collapse to
// nothing), yielding an immutable, deduplicated, distributable artifact tagged
// at spec.Target. The snapshot referenced by spec.SnapshotName must already
// exist (callers snapshot first, per the interface contract).
func (v *GoVolume) CreateBackup(ctx context.Context, spec drivers.BackupSpec) (drivers.Backup, error) {
	if spec.VolumeUUID == "" || spec.SnapshotName == "" {
		return drivers.Backup{}, fmt.Errorf("govolume CreateBackup: volume uuid and snapshot name are required")
	}
	client, ref, err := v.registryFor(spec.Target)
	if err != nil {
		return drivers.Backup{}, err
	}
	p, err := pool.Open(v.poolPath(spec.VolumeUUID))
	if err != nil {
		return drivers.Backup{}, fmt.Errorf("govolume CreateBackup: open pool: %w", err)
	}
	defer p.Close()

	snap, err := p.OpenVolume(spec.SnapshotName)
	if err != nil {
		return drivers.Backup{}, fmt.Errorf("govolume CreateBackup: open snapshot %q: %w", spec.SnapshotName, err)
	}
	size, _ := snap.Size()
	if _, err := oci.Freeze(ctx, snap, client, ref, oci.Options{}); err != nil {
		return drivers.Backup{}, fmt.Errorf("govolume CreateBackup: freeze: %w", err)
	}
	return drivers.Backup{
		VolumeUUID:      spec.VolumeUUID,
		SnapshotName:    spec.SnapshotName,
		URL:             spec.Target,
		SizeBytes:       size,
		CreatedAtUnixNs: time.Now().UnixNano(),
		Labels:          spec.Labels,
		State:           "complete",
	}, nil
}

// RestoreBackup materialises backupURL into a NEW pool volume (spec.UUID). The
// frozen OCI artifact is opened read-only and its bytes are imported into a
// fresh pool's "disk" — a genuine, writable copy (all-zero chunks stay sparse).
// Idempotent: if spec.UUID is already provisioned the restore is a no-op.
func (v *GoVolume) RestoreBackup(ctx context.Context, backupURL string, spec drivers.VolumeSpec) error {
	if spec.UUID == "" {
		return fmt.Errorf("govolume RestoreBackup: empty target volume uuid")
	}
	if fileExists(v.poolPath(spec.UUID)) {
		return nil // already restored
	}
	client, ref, err := v.registryFor(backupURL)
	if err != nil {
		return err
	}
	img, err := oci.OpenReadOnly(ctx, client, ref)
	if err != nil {
		return fmt.Errorf("govolume RestoreBackup: open frozen image: %w", err)
	}
	defer img.Close()

	imgSize, err := img.Size()
	if err != nil {
		return fmt.Errorf("govolume RestoreBackup: image size: %w", err)
	}
	// Honour a larger requested size (restore can grow, never shrink).
	size := imgSize
	if want := int64(spec.SizeGiB) * gib; want > size {
		size = want
	}

	if err := os.MkdirAll(v.volDir(spec.UUID), 0o755); err != nil {
		return fmt.Errorf("govolume RestoreBackup: mkdir: %w", err)
	}
	p, err := pool.Create(v.poolPath(spec.UUID), size+poolOverheadGiB*gib)
	if err != nil {
		return fmt.Errorf("govolume RestoreBackup: create pool: %w", err)
	}
	defer p.Close()

	// Create the empty disk at the requested size, then overlay the frozen
	// image bytes onto it. (ImportRaw can't be reused directly because it sizes
	// the volume to the image; we may need a larger disk than the image.)
	disk, err := p.CreateVolume(diskVolumeName, size)
	if err != nil {
		return fmt.Errorf("govolume RestoreBackup: create disk volume: %w", err)
	}
	if err := copyDevice(disk, img, imgSize); err != nil {
		return fmt.Errorf("govolume RestoreBackup: materialise: %w", err)
	}
	return p.Sync()
}

// copyDevice streams src (length n) into dst.WriteAt, skipping all-zero windows
// so a sparse frozen image stays thin in the destination pool.
func copyDevice(dst volume.Device, src volume.ReadOnly, n int64) error {
	const win = 1 << 20 // 1 MiB windows
	buf := make([]byte, win)
	zero := make([]byte, win)
	for off := int64(0); off < n; off += win {
		m := int64(win)
		if rem := n - off; rem < m {
			m = rem
		}
		w := buf[:m]
		if err := readFullAt(src, w, off); err != nil {
			return err
		}
		if equalBytes(w, zero[:m]) {
			continue // hole → leave the destination sparse
		}
		if _, err := dst.WriteAt(w, off); err != nil {
			return err
		}
	}
	return nil
}

// readFullAt fills p from r at off, tolerating a tail EOF once p is full.
func readFullAt(r io.ReaderAt, p []byte, off int64) error {
	n := 0
	for n < len(p) {
		m, err := r.ReadAt(p[n:], off+int64(n))
		n += m
		if err != nil {
			if err == io.EOF && n == len(p) {
				return nil
			}
			return err
		}
	}
	return nil
}

// equalBytes reports whether a and b are byte-identical.
func equalBytes(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// ListBackups and DeleteBackup are unsupported: the published go-volumes OCI
// registry client speaks blob/manifest push+pull but exposes no tag-listing or
// manifest-delete surface, so neither enumeration nor removal can be done
// correctly here. weft-block is the documented home for the full backupstore
// lifecycle.
func (v *GoVolume) ListBackups(context.Context, string, string) ([]drivers.Backup, error) {
	return nil, drivers.ErrUnsupported
}

func (v *GoVolume) DeleteBackup(context.Context, string) error {
	return drivers.ErrUnsupported
}

// registryFor resolves a backup target into a registry client + image ref. The
// target schema is "oci://<host>[:port]/<repo>:<tag>". An empty target falls
// back to the driver's configured BackupRegistry, which then requires the
// target to still carry the repo:tag — so the empty case is only valid when the
// caller passes a bare "oci://…" minus host. To keep the common path simple we
// require an explicit oci:// URL here.
func (v *GoVolume) registryFor(target string) (*registry.Client, string, error) {
	if target == "" {
		return nil, "", fmt.Errorf("govolume: backup target is required (oci://host/repo:tag)")
	}
	base, repo, ref, err := parseOCITarget(target)
	if err != nil {
		return nil, "", err
	}
	return &registry.Client{BaseURL: base, Repository: repo}, ref, nil
}

// parseOCITarget splits an "oci://host[:port]/repo[/sub...]:tag" backup target
// into a registry base URL, repository path, and image ref (tag). The host is
// reached over http:// for loopback / *.local (test registries) and https://
// otherwise — matching the convention that production registries are TLS while
// an in-process httptest registry is plain HTTP.
//
//	oci://reg.example.com/weft/backups:vol-snap
//	  → base "https://reg.example.com", repo "weft/backups", ref "vol-snap"
//	oci://127.0.0.1:5000/weft/backups:vol-snap
//	  → base "http://127.0.0.1:5000",  repo "weft/backups", ref "vol-snap"
func parseOCITarget(target string) (base, repo, ref string, err error) {
	if !strings.HasPrefix(target, "oci://") {
		return "", "", "", fmt.Errorf("govolume: backup target %q must be an oci:// URL", target)
	}
	rest := strings.TrimPrefix(target, "oci://")
	slash := strings.IndexByte(rest, '/')
	if slash < 0 {
		return "", "", "", fmt.Errorf("govolume: backup target %q missing repository path", target)
	}
	host := rest[:slash]
	repoAndTag := rest[slash+1:]
	colon := strings.LastIndexByte(repoAndTag, ':')
	if colon < 0 {
		return "", "", "", fmt.Errorf("govolume: backup target %q missing :tag", target)
	}
	repo = repoAndTag[:colon]
	ref = repoAndTag[colon+1:]
	if host == "" || repo == "" || ref == "" {
		return "", "", "", fmt.Errorf("govolume: backup target %q must be oci://host/repo:tag", target)
	}
	scheme := "https"
	if hostIsLocal(host) {
		scheme = "http"
	}
	u := url.URL{Scheme: scheme, Host: host}
	return u.String(), repo, ref, nil
}

// hostIsLocal reports whether host is loopback or a *.local mDNS name, for which
// a plain-HTTP registry (e.g. an httptest server) is expected rather than TLS.
func hostIsLocal(host string) bool {
	h := host
	if i := strings.LastIndexByte(h, ':'); i >= 0 {
		h = h[:i]
	}
	switch h {
	case "127.0.0.1", "::1", "localhost":
		return true
	}
	return strings.HasSuffix(h, ".local")
}
