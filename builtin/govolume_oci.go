package builtin

// govolume_oci.go adds an OCI-image-backed volume MODE to the GoVolume driver,
// complementing the copy-on-write pool mode in govolume.go. It targets the
// read-mostly golden-image / rootfs use case — the container-image model —
// rather than write-heavy data volumes.
//
// Shape of one OCI-overlay volume, rooted at <StateDir>/<uuid>/:
//
//	oci.json   — {"base":"oci://…:tag","current":"oci://…:tag"} sidecar
//
// There is NO pool.bin: the volume is a frozen, content-addressed OCI base
// image (oci.OpenReadOnly → an immutable volume.Device) wrapped in an in-memory
// read-write overlay (oci.NewOverlay → a pool.Backing whose writes buffer in
// RAM). AttachVolume serves that overlay straight over the in-process NBD
// export, so QEMU boots the golden image read-write. CreateSnapshot /
// CreateBackup call overlay.Commit, snapshotting the live overlay into a NEW
// immutable OCI tag that pushes ONLY the changed chunks (a content-addressed
// delta); the new tag is recorded as the volume's "current" version.
// RestoreBackup branches a fresh overlay volume from any committed tag.
//
// IN-MEMORY / READ-MOSTLY CAVEAT. The overlay buffers every write in memory:
// written bytes are ephemeral until an explicit Commit and RAM grows with the
// WRITTEN delta (not the base image size). That makes this mode ideal for an
// immutable base + a small writable layer + versioned snapshots (boot a golden
// rootfs, tweak it, commit a new version), and UNSUITABLE for write-heavy data
// volumes — those keep the pool path (govolume.go) with its on-disk CoW +
// freeze-backup. Detaching/destroying an OCI-overlay volume drops any
// uncommitted writes by design: durability is the explicit Commit.

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/go-volumes/nbd"
	"github.com/go-volumes/oci"
	"github.com/go-volumes/oci/registry"
	drivers "github.com/openweft/weft-drivers"
)

// ociFormat is the VolumeSpec.Format value that selects OCI-overlay mode. The
// base image ref travels in VolumeSpec.Name as an "oci://host/repo:tag" URL.
const ociFormat = "oci"

// ociSidecarName is the per-volume metadata file recording the base + current
// OCI refs, so a reopen (after a driver restart) can find the right tag.
const ociSidecarName = "oci.json"

// Test seams over the OCI library calls whose failure paths are otherwise
// unreachable through real injection. Production wiring is the plain library
// call; tests override these to drive the error branches deterministically.
var (
	ociOpenReadOnly = oci.OpenReadOnly
	ociNewOverlay   = oci.NewOverlay
	ociCommit       = func(ctx context.Context, o *oci.Overlay, dst *registry.Client, ref string) (string, error) {
		return o.Commit(ctx, dst, ref)
	}
)

// ociSidecar is the JSON persisted at <volDir>/oci.json. Base never changes;
// Current advances to the newest committed tag on each snapshot/backup so a
// reopen attaches the latest version.
type ociSidecar struct {
	Base    string `json:"base"`    // oci://host/repo:tag of the frozen base image
	Current string `json:"current"` // oci://host/repo:tag currently materialised
}

// isOCISpec reports whether a VolumeSpec asks for OCI-overlay mode.
func isOCISpec(spec drivers.VolumeSpec) bool {
	return spec.Format == ociFormat
}

// ociSidecarPath is the per-volume sidecar path.
func (v *GoVolume) ociSidecarPath(uuid string) string {
	return filepath.Join(v.volDir(uuid), ociSidecarName)
}

// isOCIVolume reports whether the on-host volume is OCI-overlay-backed, by the
// presence of its sidecar — the only volume-UUID-keyed signal available to the
// attach / snapshot / destroy paths that never see the original VolumeSpec.
func (v *GoVolume) isOCIVolume(uuid string) bool {
	return fileExists(v.ociSidecarPath(uuid))
}

// readOCISidecar loads a volume's OCI sidecar.
func (v *GoVolume) readOCISidecar(uuid string) (ociSidecar, error) {
	var sc ociSidecar
	b, err := os.ReadFile(v.ociSidecarPath(uuid))
	if err != nil {
		return sc, fmt.Errorf("govolume oci: read sidecar: %w", err)
	}
	if err := json.Unmarshal(b, &sc); err != nil {
		return sc, fmt.Errorf("govolume oci: parse sidecar: %w", err)
	}
	return sc, nil
}

// writeOCISidecar atomically persists a volume's OCI sidecar.
func (v *GoVolume) writeOCISidecar(uuid string, sc ociSidecar) error {
	b, _ := json.Marshal(sc) // a struct of two strings always marshals cleanly
	tmp := v.ociSidecarPath(uuid) + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return fmt.Errorf("govolume oci: write sidecar: %w", err)
	}
	if err := os.Rename(tmp, v.ociSidecarPath(uuid)); err != nil {
		return fmt.Errorf("govolume oci: commit sidecar: %w", err)
	}
	return nil
}

// ensureOCIVolume provisions an OCI-overlay volume: it validates the base ref
// (by opening the frozen image once) and writes the sidecar. Idempotent: an
// existing sidecar is a no-op. The actual overlay is created lazily at attach
// time so the in-memory write buffer only lives while the VM is attached.
func (v *GoVolume) ensureOCIVolume(ctx context.Context, spec drivers.VolumeSpec) error {
	baseRef := strings.TrimSpace(spec.Name)
	if baseRef == "" {
		return fmt.Errorf("govolume EnsureVolume: oci format requires a base image ref in spec.Name (oci://host/repo:tag)")
	}
	if v.isOCIVolume(spec.UUID) {
		return nil // already provisioned
	}
	// Validate the base ref is parseable + resolvable before we record it.
	client, ref, err := v.registryFor(baseRef)
	if err != nil {
		return err
	}
	img, err := ociOpenReadOnly(ctx, client, ref)
	if err != nil {
		return fmt.Errorf("govolume EnsureVolume: open oci base %q: %w", baseRef, err)
	}
	_ = img.Close()

	if err := os.MkdirAll(v.volDir(spec.UUID), 0o755); err != nil {
		return fmt.Errorf("govolume EnsureVolume: mkdir: %w", err)
	}
	return v.writeOCISidecar(spec.UUID, ociSidecar{Base: baseRef, Current: baseRef})
}

// openOverlay opens the volume's current OCI version read-only and wraps it in a
// fresh in-memory read-write overlay. The caller owns img.Close (the overlay's
// own Close is a no-op); both are tracked on the nbdServer and released on close.
func (v *GoVolume) openOverlay(ctx context.Context, uuid string) (*oci.Image, *oci.Overlay, error) {
	sc, err := v.readOCISidecar(uuid)
	if err != nil {
		return nil, nil, err
	}
	client, ref, err := v.registryFor(sc.Current)
	if err != nil {
		return nil, nil, err
	}
	img, err := ociOpenReadOnly(ctx, client, ref)
	if err != nil {
		return nil, nil, fmt.Errorf("govolume oci: open current version %q: %w", sc.Current, err)
	}
	return img, ociNewOverlay(img), nil
}

// attachOCIVolume serves the in-memory overlay over the frozen base via an
// in-process NBD export and returns its nbd:// URI, mirroring the pool-mode
// AttachVolume (idempotency + lost-race handling included). The overlay is the
// served volume.Device, so QEMU boots the golden image read-write; uncommitted
// writes live only in this overlay until a snapshot Commit.
func (v *GoVolume) attachOCIVolume(ctx context.Context, volumeUUID, hostUUID, key string) (drivers.AttachedVolume, error) {
	img, overlay, err := v.openOverlay(ctx, volumeUUID)
	if err != nil {
		return drivers.AttachedVolume{}, fmt.Errorf("govolume AttachVolume: %w", err)
	}

	ln, err := listenTCP("127.0.0.1:0")
	if err != nil {
		_ = img.Close()
		return drivers.AttachedVolume{}, fmt.Errorf("govolume AttachVolume: listen: %w", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	addr := fmt.Sprintf("nbd://127.0.0.1:%d", port)

	srv := &nbdServer{ln: ln, overlay: overlay, img: img, addr: addr, done: make(chan struct{})}
	server := &nbd.Server{Exports: []nbd.Export{{Name: "", Device: overlay}}}
	go func() {
		_ = server.Serve(ln)
		close(srv.done)
	}()

	v.mu.Lock()
	if existing, ok := v.servers[key]; ok && existing != nil {
		v.mu.Unlock()
		_ = srv.close()
		return drivers.AttachedVolume{BackingPath: existing.addr}, nil
	}
	v.servers[key] = srv
	v.mu.Unlock()

	return drivers.AttachedVolume{BackingPath: addr}, nil
}

// liveOverlay returns the overlay of a currently-attached OCI volume, so a
// snapshot/backup commits the live (written-through) state. It returns false
// when the volume is not attached anywhere — committing a detached overlay would
// snapshot an empty layer, which is never what the caller means.
func (v *GoVolume) liveOverlay(volumeUUID string) (*oci.Overlay, bool) {
	v.mu.Lock()
	defer v.mu.Unlock()
	for key, srv := range v.servers {
		if srv != nil && srv.overlay != nil && hasVolumePrefix(key, volumeUUID) {
			return srv.overlay, true
		}
	}
	return nil, false
}

// commitOverlay is the shared snapshot/backup primitive: it commits the live
// overlay to ref on the resolved registry, advances the sidecar's Current to the
// new tag, and returns the new manifest digest. dst is "" to commit to the
// volume's own base registry under a new tag, or an explicit oci:// target.
func (v *GoVolume) commitOverlay(ctx context.Context, volumeUUID, dst string) (newRef, digest string, err error) {
	overlay, ok := v.liveOverlay(volumeUUID)
	if !ok {
		return "", "", fmt.Errorf("govolume oci: volume %q is not attached; attach it before snapshot/backup", volumeUUID)
	}
	client, ref, err := v.registryFor(dst)
	if err != nil {
		return "", "", err
	}
	digest, err = ociCommit(ctx, overlay, client, ref)
	if err != nil {
		return "", "", fmt.Errorf("govolume oci: commit: %w", err)
	}
	if err := v.writeOCISidecar(volumeUUID, ociSidecar{Base: v.baseRef(volumeUUID), Current: dst}); err != nil {
		return "", "", err
	}
	return dst, digest, nil
}

// baseRef returns the recorded base ref for a volume, or "" if the sidecar is
// unreadable (the caller has already opened it successfully on the commit path,
// so this only re-reads the immutable Base field).
func (v *GoVolume) baseRef(volumeUUID string) string {
	sc, err := v.readOCISidecar(volumeUUID)
	if err != nil {
		return ""
	}
	return sc.Base
}

// snapshotOCIVolume commits the live overlay to a NEW versioned tag in the
// volume's own repo. The tag is spec.Name, or a timestamped one when empty. The
// returned Snapshot.Name is the full oci:// ref of the new immutable version.
func (v *GoVolume) snapshotOCIVolume(ctx context.Context, spec drivers.SnapshotSpec) (drivers.Snapshot, error) {
	base := v.baseRef(spec.VolumeUUID)
	tag := spec.Name
	if tag == "" {
		tag = fmt.Sprintf("snap-%d", time.Now().UnixNano())
	}
	dst, err := retagOCIRef(base, tag)
	if err != nil {
		return drivers.Snapshot{}, fmt.Errorf("govolume CreateSnapshot: %w", err)
	}
	newRef, _, err := v.commitOverlay(ctx, spec.VolumeUUID, dst)
	if err != nil {
		return drivers.Snapshot{}, err
	}
	return drivers.Snapshot{
		VolumeUUID:      spec.VolumeUUID,
		Name:            newRef,
		CreatedAtUnixNs: time.Now().UnixNano(),
		Labels:          spec.Labels,
		UserCreated:     true,
	}, nil
}

// backupOCIVolume commits the live overlay to the explicit BackupSpec.Target
// tag (which may live in a different repo / registry), advancing the sidecar's
// Current to it. Same Commit primitive as a snapshot, different destination.
func (v *GoVolume) backupOCIVolume(ctx context.Context, spec drivers.BackupSpec) (drivers.Backup, error) {
	if spec.Target == "" {
		return drivers.Backup{}, fmt.Errorf("govolume CreateBackup: oci-overlay backup requires a target (oci://host/repo:tag)")
	}
	newRef, _, err := v.commitOverlay(ctx, spec.VolumeUUID, spec.Target)
	if err != nil {
		return drivers.Backup{}, err
	}
	return drivers.Backup{
		VolumeUUID:      spec.VolumeUUID,
		SnapshotName:    spec.SnapshotName,
		URL:             newRef,
		CreatedAtUnixNs: time.Now().UnixNano(),
		Labels:          spec.Labels,
		State:           "complete",
	}, nil
}

// restoreOCIVolume branches a fresh OCI-overlay volume (spec.UUID) from any
// committed tag (backupURL): it validates the ref and writes a sidecar whose
// base AND current point at that version, so a subsequent attach materialises a
// new in-memory overlay over it. Idempotent: an existing sidecar is a no-op.
func (v *GoVolume) restoreOCIVolume(ctx context.Context, backupURL string, spec drivers.VolumeSpec) error {
	if v.isOCIVolume(spec.UUID) {
		return nil // already restored
	}
	client, ref, err := v.registryFor(backupURL)
	if err != nil {
		return err
	}
	img, err := ociOpenReadOnly(ctx, client, ref)
	if err != nil {
		return fmt.Errorf("govolume RestoreBackup: open oci version %q: %w", backupURL, err)
	}
	_ = img.Close()

	if err := os.MkdirAll(v.volDir(spec.UUID), 0o755); err != nil {
		return fmt.Errorf("govolume RestoreBackup: mkdir: %w", err)
	}
	return v.writeOCISidecar(spec.UUID, ociSidecar{Base: backupURL, Current: backupURL})
}

// retagOCIRef rewrites the :tag of an "oci://host/repo:tag" ref, keeping host +
// repo, so a snapshot lands in the same repository under a new version tag.
func retagOCIRef(ref, newTag string) (string, error) {
	if newTag == "" {
		return "", fmt.Errorf("govolume oci: empty snapshot tag")
	}
	if !strings.HasPrefix(ref, "oci://") {
		return "", fmt.Errorf("govolume oci: base ref %q must be an oci:// URL", ref)
	}
	colon := strings.LastIndexByte(ref, ':')
	// The scheme's "oci:" colon is at index 3; a real tag colon is after the
	// host/repo path, which always contains a '/'. Require the colon to follow it.
	slash := strings.LastIndexByte(ref, '/')
	if colon <= slash {
		return "", fmt.Errorf("govolume oci: base ref %q missing :tag", ref)
	}
	return ref[:colon+1] + newTag, nil
}
