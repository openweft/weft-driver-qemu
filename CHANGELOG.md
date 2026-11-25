# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project aims to adhere to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- **Per-boot token delivery** : `Hypervisor.StartVMWithBootToken`
  (`drivers.BootTokenStarter`, weft-drivers v0.7.0 / weft-driver-plugin
  v0.7.0). The 32-byte token goes to the guest as a QEMU fw_cfg blob,
  `-fw_cfg name=opt/org.weft/boot-token,file=/dev/fd/3`, where fd 3 is the
  read end of an anonymous pipe passed through `exec.Cmd.ExtraFiles`: the
  argv carries only the fd path, nothing touches a disk, and the token is
  never logged nor put in an error. In the guest it is readable (root only)
  at `/sys/firmware/qemu_fw_cfg/by_name/opt/org.weft/boot-token/raw`, which
  needs `CONFIG_FW_CFG_SYSFS`. `StartVM` is unchanged (no token); both share
  one launcher, and an already-running VM makes the call a no-op (token
  ignored).

- **NVIDIA MIG passthrough** : `bootConfig.migPassthrough` renders one
  `-device vfio-pci,sysfsdev=/sys/bus/mdev/devices/<uuid>` per MIG-instance
  mdev UUID — the mediated-device path a MIG slice needs (it isn't a whole
  PCI function addressable by BDF like `pciPassthrough`). `StartVM` now
  populates both `pciPassthrough` and `migPassthrough` from the VM
  `config.json` (`pci_passthrough` / `mig_devices`), so host-resolved GPU
  claims flow through to the qemu argv. Whole-card `-device`s are emitted
  before MIG ones for a deterministic order. See weft's
  `docs/operations/gpu-sharing.md`.
- **`govolume` OCI-image-backed volume mode** : a second mode of
  the pure-Go `govolume` backend for the read-mostly golden-image
  / rootfs use case (the container-image model). A volume whose
  `VolumeSpec.Format` is `"oci"` carries a frozen OCI base ref in
  `VolumeSpec.Name` (`oci://host/repo:tag`); `EnsureVolume` opens
  it read-only (`oci.OpenReadOnly`) and records a `base` + `current`
  sidecar. `AttachVolume` wraps the frozen base in an in-memory
  read-write `oci.Overlay` and serves THAT overlay over the
  in-process NBD export, so QEMU boots the golden image
  read-write. `CreateSnapshot` / `CreateBackup` call
  `Overlay.Commit`, snapshotting the live overlay into a NEW
  immutable, delta-deduped OCI tag (only changed chunks are
  pushed) and advancing `current`; `RestoreBackup` branches a
  fresh overlay volume from any committed tag. Reuses
  `WEFT_QEMU_BACKUP_REGISTRY` (+ new optional `_USERNAME` /
  `_PASSWORD` creds). The overlay is in memory — written bytes are
  ephemeral until Commit and RAM grows with the WRITTEN delta, not
  the base size — so this mode targets immutable base + small
  writable layer + versioned snapshots, NOT write-heavy data
  volumes (those keep the existing CoW-pool + freeze-backup path,
  untouched). 100 % covered.

### Fixed

- **Atomic `exit.json` write + stale clear on restart** : the
  reaper goroutine landed in `9cc7333` wrote the descriptor with
  a plain `os.WriteFile`, leaving a small window in which a
  concurrent status reader could see a half-written JSON
  document. `StartVM` now removes any stale `exit.json` before
  relaunching, and the reaper writes via `tmp + rename`. Commit
  `18cffe6`.

## [0.2.0] - 2026-06-02

### Added

- **PCI passthrough** via `-device vfio-pci` on the builtin QEMU
  command-line builder. Driver consumes the `RequestedPCI` slice
  from the start request and emits one `vfio-pci,host=<bdf>` per
  requested device. Pairs with the host-side cordon / inventory
  surface in `weft` (`67fd017b1`) and the admission shape in
  `weft-proto` v0.3.0 (`CreateVMRequest.requested_pci`). Commit
  `3c6b0cb`.

### Fixed

- **`AttachDisk`** (real bug) : the file `Close` error after the
  attach call was previously swallowed. Surfaced via the error
  return so the agent learns about a half-attached disk. Commit
  `f0ad4b4`.

## [0.1.0] - 2026-05-31

Initial release. QEMU/KVM driver plugin for `weft agent` on Linux.
Implements the `weft-driver-plugin` gRPC contract over go-plugin
stdio. BSD 3-Clause LICENSE (`4f45871`).
