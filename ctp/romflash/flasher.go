package romflash

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"io"
	"time"
)

// ChipInfo describes the connected chip.
type ChipInfo struct {
	ChipID     uint32 `json:"chip_id"`
	Name       string `json:"name"`
	EcoVersion uint32 `json:"eco_version"`
	SecureBoot bool   `json:"secure_boot"`
	FlashCrypt bool   `json:"flash_encryption"`
}

// Image is one region to write.
type Image struct {
	Name   string
	Offset uint32
	Data   []byte
}

// WriteOptions control WriteImages.
type WriteOptions struct {
	Differential bool                  // compare 4 KB sectors by MD5, write only changed runs
	FlashSize    uint32                // bytes; default 8 MB (XIAO ESP32-C5)
	Progress     func(done, total int) // bytes
}

// PartResult reports one image.
type PartResult struct {
	Name           string `json:"name"`
	Offset         string `json:"offset"`
	Size           int    `json:"size"`
	SectorsWritten int    `json:"sectors_written"`
	SectorsSkipped int    `json:"sectors_skipped"`
	MD5            string `json:"md5"`
	Verified       bool   `json:"verified"`
	Retried        bool   `json:"retried,omitempty"`
}

// Report summarises a write.
type Report struct {
	SectorsWritten int          `json:"sectors_written"`
	SectorsSkipped int          `json:"sectors_skipped"`
	DurationMS     int64        `json:"duration_ms"`
	Parts          []PartResult `json:"parts"`
}

// Flasher is a connected ROM session.
type Flasher struct {
	L    *Loader
	chip ChipInfo
}

// Probe reports whether a ROM loader answers SYNC on rw within a few
// attempts. Safe on a port running CTP firmware (SLIP SYNC is just a bad
// frame to the CTP decoder). Never send CTP frames to the ROM: malformed SLIP
// input was observed to wedge the C5 ROM's USB-Serial/JTAG (M3).
func Probe(ctx context.Context, rw io.ReadWriter, attempts int) bool {
	return NewLoader(rw).Sync(ctx, attempts) == nil
}

// Connect syncs with the ROM, checks the chip and attaches the SPI flash.
// It refuses anything but an ESP32-C5 (nothing is written in that case).
// It never touches eFuses.
func Connect(ctx context.Context, rw io.ReadWriter) (*Flasher, error) {
	l := NewLoader(rw)
	if err := l.Sync(ctx, 10); err != nil {
		return nil, err
	}
	si, err := l.SecurityInfo(ctx)
	if err != nil {
		return nil, fmt.Errorf("romflash: chip detect: %w", err)
	}
	ci := ChipInfo{ChipID: si.ChipID, EcoVersion: si.EcoVersion, Name: chipName(si.ChipID),
		SecureBoot: si.Flags&1 != 0, FlashCrypt: si.FlashCrypt&1 != 0}
	if si.ChipID != ChipIDC5 {
		return nil, fmt.Errorf("%w: this is a %s. Nothing was written", ErrNotC5, ci.Name)
	}
	if err := l.SPIAttach(ctx); err != nil {
		return nil, err
	}
	return &Flasher{L: l, chip: ci}, nil
}

func chipName(id uint32) string {
	switch id {
	case 5:
		return "ESP32-C3"
	case 9:
		return "ESP32-S3"
	case 12:
		return "ESP32-C2"
	case 13:
		return "ESP32-C6"
	case 16:
		return "ESP32-H2"
	case 18:
		return "ESP32-P4"
	case 20:
		return "ESP32-C61"
	case ChipIDC5:
		return "ESP32-C5"
	}
	return fmt.Sprintf("chip id %d", id)
}

// Chip returns the detected chip.
func (f *Flasher) Chip() ChipInfo { return f.chip }

func md5hex(b []byte) string {
	s := md5.Sum(b)
	return hex.EncodeToString(s[:])
}

// WriteImages writes every image, MD5-verifies it, and retries a failed
// image once. The ROM loader stays active afterwards (call HardReset).
func (f *Flasher) WriteImages(ctx context.Context, imgs []Image, o WriteOptions) (Report, error) {
	t0 := time.Now()
	if o.FlashSize == 0 {
		o.FlashSize = 8 << 20
	}
	var rep Report
	if err := f.L.SPISetParams(ctx, o.FlashSize); err != nil {
		return rep, err
	}
	total := 0
	for _, im := range imgs {
		total += len(im.Data)
		if im.Offset%SectorSize != 0 {
			return rep, fmt.Errorf("romflash: %s offset 0x%x not sector aligned", im.Name, im.Offset)
		}
		if uint64(im.Offset)+uint64(len(im.Data)) > uint64(o.FlashSize) {
			return rep, fmt.Errorf("romflash: %s does not fit in %d MB flash", im.Name, o.FlashSize>>20)
		}
	}
	done := 0
	for _, im := range imgs {
		pr := PartResult{Name: im.Name, Offset: fmt.Sprintf("0x%x", im.Offset), Size: len(im.Data), MD5: md5hex(im.Data)}
		var err error
		for attempt := 0; attempt < 2; attempt++ {
			pr.SectorsWritten, pr.SectorsSkipped, err = f.writeOne(ctx, im, o.Differential && attempt == 0, func(n int) {
				if o.Progress != nil {
					o.Progress(done+n, total)
				}
			})
			if err == nil {
				var got string
				got, err = f.L.MD5(ctx, im.Offset, uint32(len(im.Data)))
				if err == nil && got != pr.MD5 {
					err = fmt.Errorf("romflash: %s: MD5 mismatch (flash %s, image %s)", im.Name, got, pr.MD5)
				}
			}
			if err == nil {
				pr.Verified = true
				break
			}
			if ctx.Err() != nil {
				break
			}
			pr.Retried = true
		}
		rep.Parts = append(rep.Parts, pr)
		rep.SectorsWritten += pr.SectorsWritten
		rep.SectorsSkipped += pr.SectorsSkipped
		if err != nil {
			rep.DurationMS = time.Since(t0).Milliseconds()
			return rep, fmt.Errorf("flashing failed, dongle is safe to retry: %w", err)
		}
		done += len(im.Data)
	}
	// No FLASH_END: the ROM acknowledges each block after writing it, rejects
	// FLASH_END once another command (MD5) ran in between (status 1/0x06, M3),
	// and we leave the ROM with a watchdog reset anyway.
	rep.DurationMS = time.Since(t0).Milliseconds()
	return rep, nil
}

// writeOne writes one image, optionally only its changed sectors.
func (f *Flasher) writeOne(ctx context.Context, im Image, diff bool, progress func(int)) (written, skipped int, err error) {
	n := (len(im.Data) + SectorSize - 1) / SectorSize
	changed := make([]bool, n)
	for i := range changed {
		changed[i] = true
	}
	if diff {
		for i := 0; i < n; i++ {
			lo, hi := i*SectorSize, (i+1)*SectorSize
			if hi > len(im.Data) {
				hi = len(im.Data)
			}
			got, err := f.L.MD5(ctx, im.Offset+uint32(lo), uint32(hi-lo))
			if err != nil {
				return 0, 0, err
			}
			changed[i] = got != md5hex(im.Data[lo:hi])
		}
	}
	for i := 0; i < n; {
		if !changed[i] {
			skipped++
			i++
			continue
		}
		j := i
		for j < n && changed[j] {
			j++
		}
		lo, hi := i*SectorSize, j*SectorSize
		if hi > len(im.Data) {
			hi = len(im.Data)
		}
		if err := f.writeRun(ctx, im.Offset+uint32(lo), im.Data[lo:hi], func(k int) { progress(lo + k) }); err != nil {
			return written, skipped, err
		}
		written += j - i
		i = j
	}
	progress(len(im.Data))
	return written, skipped, nil
}

func (f *Flasher) writeRun(ctx context.Context, off uint32, data []byte, progress func(int)) error {
	blocks, err := f.L.FlashBegin(ctx, off, uint32(len(data)))
	if err != nil {
		return err
	}
	for seq := uint32(0); seq < blocks; seq++ {
		lo := int(seq) * FlashWriteSize
		hi := lo + FlashWriteSize
		if hi > len(data) {
			hi = len(data)
		}
		if err := f.L.FlashData(ctx, seq, data[lo:hi]); err != nil {
			return err
		}
		progress(hi)
	}
	return nil
}

// HardReset leaves the ROM via the LP watchdog (docs/10 §3).
func (f *Flasher) HardReset(ctx context.Context) error { return f.L.WatchdogReset(ctx) }
