package fwver

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
)

// ManifestFromBuild builds a manifest from an ESP-IDF build directory
// (flasher_args.json + the app descriptor), docs/04 §11.1. File paths are
// relative to dir.
func ManifestFromBuild(dir string) (Manifest, error) {
	var m Manifest
	b, err := os.ReadFile(filepath.Join(dir, "flasher_args.json"))
	if err != nil {
		return m, err
	}
	var fa struct {
		FlashFiles    map[string]string `json:"flash_files"`
		FlashSettings struct {
			Mode string `json:"flash_mode"`
			Size string `json:"flash_size"`
			Freq string `json:"flash_freq"`
		} `json:"flash_settings"`
		App struct {
			Offset string `json:"offset"`
			File   string `json:"file"`
		} `json:"app"`
		Extra struct {
			Chip string `json:"chip"`
		} `json:"extra_esptool_args"`
	}
	if err := json.Unmarshal(b, &fa); err != nil {
		return m, fmt.Errorf("fwver: flasher_args.json: %w", err)
	}
	app, err := os.ReadFile(filepath.Join(dir, fa.App.File))
	if err != nil {
		return m, err
	}
	ad, err := ParseAppDesc(app)
	if err != nil {
		return m, err
	}
	bid := ad.BuildID()
	m = Manifest{Version: ad.Version, BuildID: hex.EncodeToString(bid[:]), ProtocolVer: 1, IDF: ad.IDF, Chip: fa.Extra.Chip,
		Flash: FlashParams{Mode: fa.FlashSettings.Mode, Freq: fa.FlashSettings.Freq, Size: fa.FlashSettings.Size}}
	offs := make([]string, 0, len(fa.FlashFiles))
	for o := range fa.FlashFiles {
		offs = append(offs, o)
	}
	sort.Slice(offs, func(i, j int) bool {
		a, _ := strconv.ParseUint(offs[i], 0, 32)
		b, _ := strconv.ParseUint(offs[j], 0, 32)
		return a < b
	})
	for _, o := range offs {
		f := fa.FlashFiles[o]
		data, err := os.ReadFile(filepath.Join(dir, f))
		if err != nil {
			return m, err
		}
		sum := sha256.Sum256(data)
		name := "app"
		switch {
		case f == fa.App.File:
		case filepath.Base(f) == "bootloader.bin":
			name = "bootloader"
		case filepath.Base(f) == "partition-table.bin":
			name = "partition-table"
		default:
			name = filepath.Base(f)
		}
		m.Parts = append(m.Parts, Part{Name: name, Offset: o, File: f, SHA256: hex.EncodeToString(sum[:])})
	}
	return m, nil
}

// WriteBundle copies the images of an IDF build into outDir with release
// names (bootloader.bin, partition-table.bin, ctk-VERSION.bin), removes older
// *.bin files there, and writes outDir/manifest.json (paths relative to it).
func WriteBundle(buildDir, outDir string) (Manifest, error) {
	m, err := ManifestFromBuild(buildDir)
	if err != nil {
		return m, err
	}
	if _, err := Parse(m.Version); err != nil {
		return m, fmt.Errorf("fwver: release builds need a strict MAJOR.MINOR.PATCH version, got %q (build with CTK_RELEASE_VERSION)", m.Version)
	}
	old, _ := filepath.Glob(filepath.Join(outDir, "*.bin"))
	for _, f := range old {
		if err := os.Remove(f); err != nil {
			return m, err
		}
	}
	for i, p := range m.Parts {
		name := p.Name + ".bin"
		if p.Name == "app" {
			name = "ctk-" + m.Version + ".bin"
		}
		data, err := os.ReadFile(filepath.Join(buildDir, p.File))
		if err != nil {
			return m, err
		}
		if err := os.WriteFile(filepath.Join(outDir, name), data, 0o644); err != nil {
			return m, err
		}
		m.Parts[i].File = name
	}
	b, _ := json.MarshalIndent(m, "", "  ")
	return m, os.WriteFile(filepath.Join(outDir, "manifest.json"), append(b, '\n'), 0o644)
}
