// Package fwver implements firmware semantic versions, the doc 03 §11
// compatibility rule, and the release manifest model (docs/04 §11.1).
package fwver

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"strconv"
	"strings"
)

// Version is MAJOR.MINOR.PATCH, packed on the wire as major<<24|minor<<16|patch.
type Version struct {
	Major, Minor uint8
	Patch        uint16
}

// Parse accepts "1.3.0" or "v1.3.0".
func Parse(s string) (Version, error) {
	s = strings.TrimPrefix(strings.TrimSpace(s), "v")
	p := strings.Split(s, ".")
	if len(p) != 3 {
		return Version{}, fmt.Errorf("fwver: %q is not MAJOR.MINOR.PATCH", s)
	}
	ma, e1 := strconv.ParseUint(p[0], 10, 8)
	mi, e2 := strconv.ParseUint(p[1], 10, 8)
	pa, e3 := strconv.ParseUint(p[2], 10, 16)
	if e1 != nil || e2 != nil || e3 != nil {
		return Version{}, fmt.Errorf("fwver: %q is not strict semver", s)
	}
	return Version{uint8(ma), uint8(mi), uint16(pa)}, nil
}

// MustParse panics on error (constants, tests).
func MustParse(s string) Version {
	v, err := Parse(s)
	if err != nil {
		panic(err)
	}
	return v
}

// FromWire unpacks a HELLO fw_version.
func FromWire(u uint32) Version { return Version{uint8(u >> 24), uint8(u >> 16), uint16(u)} }

// Wire packs the version.
func (v Version) Wire() uint32 { return uint32(v.Major)<<24 | uint32(v.Minor)<<16 | uint32(v.Patch) }

func (v Version) String() string { return fmt.Sprintf("%d.%d.%d", v.Major, v.Minor, v.Patch) }

// Compatible implements docs/03 §11: MAJOR and MINOR must match.
func Compatible(host, dev Version) bool { return host.Major == dev.Major && host.Minor == dev.Minor }

// FlashParams of the manifest.
type FlashParams struct {
	Mode string `json:"mode"`
	Freq string `json:"freq"`
	Size string `json:"size"`
}

// Part is one flash region.
type Part struct {
	Name   string `json:"name"`
	Offset string `json:"offset"`
	File   string `json:"file"`
	SHA256 string `json:"sha256"`
}

// Manifest is fw/manifest.json.
type Manifest struct {
	Version     string      `json:"version"`
	BuildID     string      `json:"build_id"`
	ProtocolVer int         `json:"protocol_ver"`
	IDF         string      `json:"idf"`
	Chip        string      `json:"chip"`
	Flash       FlashParams `json:"flash"`
	Parts       []Part      `json:"parts"`
}

// LoadManifest reads and parses a manifest from fsys.
func LoadManifest(fsys fs.FS, path string) (Manifest, error) {
	var m Manifest
	b, err := fs.ReadFile(fsys, path)
	if err != nil {
		return m, err
	}
	if err := json.Unmarshal(b, &m); err != nil {
		return m, fmt.Errorf("fwver: %s: %w", path, err)
	}
	if _, err := Parse(m.Version); err != nil {
		return m, err
	}
	return m, nil
}
