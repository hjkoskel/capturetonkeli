# fw/ – current release firmware bundle (generated)

**Do not edit by hand.** `make release VERSION=X.Y.Z` writes this directory (docs/11 §4):

- `manifest.json`: version, build_id, protocol_ver, idf, chip, flash params, parts with offsets and sha256 (format: docs/04 §11.1)
- `bootloader.bin`, `partition-table.bin`, `ctk-X.Y.Z.bin`
- `embed.go`: package `fw`, `//go:embed *`, so ctpbench ships the same firmware (expected version + `fw update`/`fw ensure`)

This directory is served by GitHub Pages (the web UI flashes and updates dongles from it) and embedded into ctpbench. CI rebuilds the firmware from source and checks these files byte-for-byte.

Verify: `make bundle-check` rebuilds the version in manifest.json from source and compares byte-for-byte.
