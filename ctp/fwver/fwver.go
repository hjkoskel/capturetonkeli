// Package fwver implements firmware semantic versions, the doc 03 §11
// compatibility rule, and the release manifest model (docs/04 §11.1).
package fwver

import (
	"encoding/binary"
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

// Manifest is fw/manifest.json. Version may carry a "-dev"/"-dirty" suffix
// for development builds (firmware/build/manifest.json).
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
	if _, err := VersionOf(m.Version); err != nil {
		return m, err
	}
	return m, nil
}

// SemVer returns the manifest's MAJOR.MINOR.PATCH (pre-release suffix dropped).
func (m Manifest) SemVer() Version {
	v, _ := VersionOf(m.Version)
	return v
}

// AppDesc is the esp_app_desc_t embedded at the start of an app image.
type AppDesc struct {
	Version   string   `json:"version"`
	Project   string   `json:"project"`
	Time      string   `json:"time"`
	Date      string   `json:"date"`
	IDF       string   `json:"idf"`
	ELFSHA256 [32]byte `json:"-"`
}

// BuildID is the first 8 bytes of the app ELF SHA-256 (HELLO fw_build_id).
func (a AppDesc) BuildID() [8]byte {
	var b [8]byte
	copy(b[:], a.ELFSHA256[:8])
	return b
}

const appDescMagic = 0xABCD5432

// ParseAppDesc reads esp_app_desc_t from an app image: it follows the
// 24-byte image header and the 8-byte first segment header.
func ParseAppDesc(app []byte) (AppDesc, error) {
	const off = 24 + 8
	var a AppDesc
	if len(app) < off+256 || app[0] != 0xE9 {
		return a, fmt.Errorf("fwver: not an ESP app image")
	}
	d := app[off:]
	if binary.LittleEndian.Uint32(d) != appDescMagic {
		return a, fmt.Errorf("fwver: app descriptor magic not found")
	}
	str := func(b []byte) string { return strings.TrimRight(string(b), "\x00") }
	a.Version = str(d[16:48])
	a.Project = str(d[48:80])
	a.Time = str(d[80:96])
	a.Date = str(d[96:112])
	a.IDF = str(d[112:144])
	copy(a.ELFSHA256[:], d[144:176])
	return a, nil
}

// VersionOf parses the semver prefix of an app descriptor version
// ("0.1.0-dev-dirty" → 0.1.0).
func VersionOf(s string) (Version, error) {
	core, _, _ := strings.Cut(s, "-")
	return Parse(core)
}
