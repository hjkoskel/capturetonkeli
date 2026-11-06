// Package fw embeds the current release firmware bundle (docs/11 §1):
// manifest.json + *.bin written by `make release`. ctpbench uses it as the
// expected firmware and flashes it with `fw update` / `fw ensure`.
package fw

import "embed"

// FS holds manifest.json and the images ("*" so the package also builds
// before the first release, when only this file and README.md exist).
//
//go:embed *
var FS embed.FS

// HasBundle reports whether a release bundle is embedded.
func HasBundle() bool {
	_, err := FS.Open("manifest.json")
	return err == nil
}
