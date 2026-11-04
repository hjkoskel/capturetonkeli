package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/hjkoskel/capturetonkeli/ctp"
	"github.com/hjkoskel/capturetonkeli/ctp/fwver"
	"github.com/hjkoskel/capturetonkeli/ctp/romflash"
	"github.com/hjkoskel/capturetonkeli/ctp/serialport"
	"github.com/hjkoskel/capturetonkeli/ctp/wire"
)

func init() {
	var mBuild string
	register(command{name: "fw manifest", summary: "write manifest.json for an IDF build dir (docs/04 §11.1)",
		example: "ctpbench fw manifest --build firmware/build",
		flags:   func(fs *flag.FlagSet) { fs.StringVar(&mBuild, "build", "firmware/build", "IDF build directory") },
		run: func(ctx context.Context, e *env) (any, error) {
			m, err := fwver.ManifestFromBuild(mBuild)
			if err != nil {
				return nil, err
			}
			b, _ := json.MarshalIndent(m, "", "  ")
			p := filepath.Join(mBuild, "manifest.json")
			if err := os.WriteFile(p, append(b, '\n'), 0o644); err != nil {
				return nil, err
			}
			return map[string]any{"written": p, "manifest": m}, nil
		}})
	register(command{name: "fw info", summary: "HELLO only (allowed on mismatch): version, flags, build_id vs manifest",
		run: cmdFwInfo})
	var fManifest, fImage, fOffset string
	var fYes, fFull bool
	flashFlags := func(fs *flag.FlagSet) {
		fs.StringVar(&fManifest, "manifest", "", "manifest.json (default: --fw-manifest)")
		fs.StringVar(&fImage, "image", "", "single image file (with --offset)")
		fs.StringVar(&fOffset, "offset", "0x10000", "offset for --image")
		fs.BoolVar(&fYes, "yes", false, "really write flash (without: print the plan)")
		fs.BoolVar(&fFull, "full", false, "write every sector (no differential compare)")
	}
	register(command{name: "fw flash", summary: "flash a manifest or image (dev/downgrade), differential by default",
		example: "ctpbench fw flash --manifest firmware/build/manifest.json --yes", flags: flashFlags,
		run: func(ctx context.Context, e *env) (any, error) {
			return cmdFwFlash(ctx, e, fwPlan{manifest: fManifest, image: fImage, offset: fOffset, yes: fYes, full: fFull})
		}})
	register(command{name: "fw update", summary: "update a CTP dongle to the expected (--fw-manifest) firmware", flags: flashFlags,
		run: func(ctx context.Context, e *env) (any, error) {
			return cmdFwFlash(ctx, e, fwPlan{manifest: fManifest, yes: fYes, full: fFull, expect: true})
		}})
	register(command{name: "fw ensure", summary: "no-op if the exact build is running, else fw update", flags: flashFlags,
		run: func(ctx context.Context, e *env) (any, error) {
			return cmdFwFlash(ctx, e, fwPlan{manifest: fManifest, yes: fYes, full: fFull, expect: true, ensure: true})
		}})
	register(command{name: "fw flash-bare", summary: "full flash of a bare chip in ROM download mode (hold BOOT while plugging in)", flags: flashFlags,
		run: func(ctx context.Context, e *env) (any, error) {
			return cmdFwFlash(ctx, e, fwPlan{manifest: fManifest, image: fImage, offset: fOffset, yes: fYes, full: true, bare: true})
		}})
}

type fwPlan struct {
	manifest, image, offset string
	yes, full               bool
	expect, ensure, bare    bool
}

func loadImages(manifestPath, image, offset string) (*fwver.Manifest, []romflash.Image, error) {
	if image != "" {
		data, err := os.ReadFile(image)
		if err != nil {
			return nil, nil, err
		}
		off, err := strconv.ParseUint(offset, 0, 32)
		if err != nil {
			return nil, nil, errf(exitUsage, "USAGE", "--offset 0x10000", "bad offset")
		}
		return nil, []romflash.Image{{Name: filepath.Base(image), Offset: uint32(off), Data: data}}, nil
	}
	if manifestPath == "" {
		return nil, nil, errf(exitUsage, "NO_MANIFEST", "pass --manifest PATH or --fw-manifest PATH (fw/ bundle not built yet: make release)", "no firmware manifest")
	}
	dir, file := filepath.Split(manifestPath)
	if dir == "" {
		dir = "."
	}
	m, err := fwver.LoadManifest(os.DirFS(dir), file)
	if err != nil {
		return nil, nil, errf(exitUsage, "BAD_MANIFEST", "regenerate with `ctpbench fw manifest`", "%v", err)
	}
	if m.Chip != "" && m.Chip != "esp32c5" {
		return nil, nil, errf(exitUsage, "BAD_MANIFEST", "", "manifest is for %s, not esp32c5", m.Chip)
	}
	var imgs []romflash.Image
	for _, p := range m.Parts {
		data, err := os.ReadFile(filepath.Join(dir, p.File))
		if err != nil {
			return nil, nil, err
		}
		if sum := sha256.Sum256(data); p.SHA256 != "" && hex.EncodeToString(sum[:]) != p.SHA256 {
			return nil, nil, errf(exitUsage, "BAD_MANIFEST", "rebuild and regenerate the manifest", "%s: sha256 mismatch", p.File)
		}
		off, err := strconv.ParseUint(p.Offset, 0, 32)
		if err != nil {
			return nil, nil, errf(exitUsage, "BAD_MANIFEST", "", "%s: bad offset %q", p.Name, p.Offset)
		}
		imgs = append(imgs, romflash.Image{Name: p.Name, Offset: uint32(off), Data: data})
	}
	return &m, imgs, nil
}

func flashSizeOf(m *fwver.Manifest) uint32 {
	if m != nil {
		if mb, err := strconv.Atoi(strings.TrimSuffix(m.Flash.Size, "MB")); err == nil && mb > 0 {
			return uint32(mb) << 20
		}
	}
	return 8 << 20
}

// helloQuick returns device info if CTP firmware answers within d.
func (e *env) helloQuick(ctx context.Context, d time.Duration) (*wire.DeviceInfo, error) {
	rw, err := e.openRaw(ctx)
	if err != nil {
		return nil, err
	}
	cctx, cancel := context.WithTimeout(ctx, d)
	defer cancel()
	c, err := ctp.New(cctx, rw, ctp.Options{PingInterval: -1, RequestTimeout: 150 * time.Millisecond, Retries: 2})
	if err != nil {
		rw.Close()
		return nil, err
	}
	info := c.Info()
	c.Abandon()
	return &info, nil
}

// refind waits for the dongle to (re)appear after a reset and updates e.g.port.
func (e *env) refind(ctx context.Context, explicit string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) && ctx.Err() == nil {
		if explicit != "" && explicit != "auto" {
			if _, err := os.Stat(explicit); err == nil {
				e.g.port = explicit
				return nil
			}
		}
		if ds, err := serialport.FindDongles(); err == nil && len(ds) == 1 {
			e.g.port = ds[0].Path
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return errf(exitNoDevice, "NO_DEVICE", "replug the dongle", "dongle did not re-appear within %v", timeout)
}

func cmdFwInfo(ctx context.Context, e *env) (any, error) {
	info, err := e.helloQuick(ctx, 2*time.Second)
	if err != nil {
		return nil, err
	}
	res := map[string]any{"device": infoJSON(*info)}
	if e.g.fwManifest != "" {
		m, _, err := loadImages(e.g.fwManifest, "", "")
		if err != nil {
			return nil, err
		}
		dv := fwver.FromWire(info.FwVersion)
		res["expected"] = map[string]any{"version": m.Version, "build_id": m.BuildID}
		res["compatible"] = fwver.Compatible(m.SemVer(), dv)
		res["exact_build_match"] = hex.EncodeToString(info.FwBuildID[:]) == m.BuildID
	}
	return res, nil
}

func cmdFwFlash(ctx context.Context, e *env, p fwPlan) (any, error) {
	mp := p.manifest
	if mp == "" {
		mp = e.g.fwManifest
	}
	m, imgs, err := loadImages(mp, p.image, p.offset)
	if err != nil {
		return nil, err
	}
	if e.g.timeout == 30*time.Second {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(context.WithoutCancel(ctx), 5*time.Minute)
		defer cancel()
	}
	explicit := e.g.port
	// ROM first (SLIP only), CTP second: CTP bytes must never reach the ROM.
	inROM := false
	if e.g.port != "sim" {
		if err := e.refind(ctx, explicit, 2*time.Second); err == nil {
			if rw, err := openROM(e.g.port); err == nil {
				pctx, cancel := context.WithTimeout(ctx, 2*time.Second)
				inROM = romflash.Probe(pctx, rw, 3)
				cancel()
				rw.Close()
			}
		}
	}
	var before *wire.DeviceInfo
	if !p.bare && !inROM {
		before, _ = e.helloQuick(ctx, 1500*time.Millisecond)
	}
	if p.ensure && before != nil && m != nil && hex.EncodeToString(before.FwBuildID[:]) == m.BuildID {
		return map[string]any{"action": "none", "version": m.Version, "build_id": m.BuildID}, nil
	}
	if p.expect && !p.yes && before == nil && !inROM {
		return nil, errf(exitNoDevice, "NO_DEVICE", "fw update needs running CTP firmware; use fw flash-bare for a bare chip", "no CTP answer")
	}
	plan := map[string]any{}
	var parts []map[string]any
	total := 0
	for _, im := range imgs {
		parts = append(parts, map[string]any{"name": im.Name, "offset": fmt.Sprintf("0x%x", im.Offset), "size": len(im.Data)})
		total += len(im.Data)
	}
	plan["parts"], plan["bytes"], plan["differential"] = parts, total, !p.full
	switch {
	case inROM:
		plan["enter_rom"] = "already in ROM download mode"
	case before != nil:
		plan["running"] = infoJSON(*before)
		plan["enter_rom"] = "REBOOT 1"
	default:
		return nil, errf(exitNoDevice, "NO_DEVICE", "neither CTP firmware nor the ROM answers: hold BOOT while plugging the dongle in, then retry", "no device answers on %s", e.g.port)
	}
	if m != nil {
		plan["version"], plan["build_id"] = m.Version, m.BuildID
	}
	if !p.yes {
		plan["hint"] = "dry run: pass --yes to write flash"
		return map[string]any{"plan": plan}, nil
	}
	t0 := time.Now()
	// 1. enter ROM download mode
	if before != nil {
		c, err := e.client(ctx, ctp.Options{PingInterval: -1, VersionPolicy: ctp.AllowMismatch})
		if err != nil {
			return nil, err
		}
		if err := c.Reboot(ctx, wire.RebootDownload); err != nil {
			c.Abandon()
			return nil, err
		}
		c.Abandon()
		time.Sleep(700 * time.Millisecond) // re-enumeration
	}
	if err := e.refind(ctx, explicit, 8*time.Second); err != nil {
		return nil, err
	}
	// 2. connect to the ROM
	rw, err := openROM(e.g.port)
	if err != nil {
		return nil, err
	}
	f, err := romflash.Connect(ctx, rw)
	if err != nil {
		rw.Close()
		if errors.Is(err, romflash.ErrNotC5) {
			return nil, errf(exitFlashFail, "WRONG_CHIP", "", "%v", err)
		}
		return nil, errf(exitFlashFail, "NO_ROM", "hold BOOT while plugging the dongle in, then retry", "%v", err)
	}
	last := -1
	rep, err := f.WriteImages(ctx, imgs, romflash.WriteOptions{Differential: !p.full, FlashSize: flashSizeOf(m), Progress: func(d, t int) {
		if pc := d * 100 / max(t, 1); pc/10 != last/10 {
			last = pc
			e.logf(1, "flash: %3d%%", pc)
		}
	}})
	if err != nil {
		rw.Close()
		res := errf(exitFlashFail, "FLASH_FAILED", "the dongle stays in download mode; retry the same command", "%v", err)
		res.extra = map[string]any{"report": rep}
		return nil, res
	}
	// 3. leave ROM and verify the new firmware answers
	_ = f.HardReset(ctx)
	rw.Close()
	time.Sleep(1500 * time.Millisecond)
	res := map[string]any{"action": "flashed", "chip": f.Chip(), "sectors_written": rep.SectorsWritten, "sectors_skipped": rep.SectorsSkipped,
		"report": rep, "duration_ms": time.Since(t0).Milliseconds()}
	if err := e.refind(ctx, explicit, 10*time.Second); err != nil {
		return nil, err
	}
	var after *wire.DeviceInfo
	for i := 0; i < 20 && after == nil && ctx.Err() == nil; i++ {
		after, _ = e.helloQuick(ctx, time.Second)
		if after == nil {
			time.Sleep(300 * time.Millisecond)
			_ = e.refind(ctx, explicit, 2*time.Second)
		}
	}
	if after == nil {
		res["warning"] = "no HELLO after flashing (bare image or non-CTP firmware?)"
		return res, nil
	}
	res["new_version"] = fwver.FromWire(after.FwVersion).String()
	res["build_id"] = hex.EncodeToString(after.FwBuildID[:])
	res["port"] = e.g.port
	if m != nil && res["build_id"] != m.BuildID {
		return nil, errf(exitFlashFail, "BUILD_MISMATCH", "", "device runs build %s, manifest says %s", res["build_id"], m.BuildID)
	}
	return res, nil
}

// openROM opens the port for the ROM loader WITHOUT changing DTR/RTS: the
// kernel raises both on open and esptool keeps them that way. Toggling RTS
// while the ROM runs is suspected to wedge its USB-Serial/JTAG (M3).
func openROM(path string) (io.ReadWriteCloser, error) {
	return serialport.Open(path)
}
