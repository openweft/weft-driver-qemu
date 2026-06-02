# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project aims to adhere to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

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
