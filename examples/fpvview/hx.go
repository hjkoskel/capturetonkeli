package main

import (
	"bytes"
	"fmt"
	"image"
	"image/draw"
	"image/jpeg"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/hjkoskel/capturetonkeli/ctp/hxfpv"
)

type hxPipe struct {
	cfg   *config
	sh    *shared
	rx    *hxfpv.Receiver
	unit  uint16
	have  bool
	rec   *os.File // .mjpeg: JPEGs back to back
	dir   string   // or numbered .jpg files
	saved int
	n     int
	t     time.Time
	fps   float64
	wh    string
	bad   int
}

func newHX(cfg *config, sh *shared) (*hxPipe, error) {
	p := &hxPipe{cfg: cfg, sh: sh, t: time.Now()}
	if cfg.unit != 0 {
		p.unit, p.have = uint16(cfg.unit), true
	}
	if r := cfg.record; r != "" {
		if st, err := os.Stat(r); (err == nil && st.IsDir()) || strings.HasSuffix(r, "/") {
			if err := os.MkdirAll(r, 0o755); err != nil {
				return nil, err
			}
			p.dir = r
		} else {
			f, err := os.Create(r)
			if err != nil {
				return nil, err
			}
			p.rec = f
		}
	}
	p.rx = hxfpv.NewReceiver(hxfpv.Options{OnFrame: p.onFrame})
	return p, nil
}

func (p *hxPipe) push(f rxFrame) {
	p.rx.Push(f.data, hxfpv.RxMeta{RSSI: f.rssi, HasRSSI: f.hasRSSI, Channel: f.channel})
}

func (p *hxPipe) onFrame(u *hxfpv.Unit, j []byte, info hxfpv.FrameInfo) {
	if !p.have {
		p.unit, p.have = u.ID, true
	}
	if u.ID != p.unit {
		return
	}
	p.n++
	switch {
	case p.rec != nil:
		if _, err := p.rec.Write(j); err != nil {
			p.sh.setNote("recording stopped: " + err.Error())
			p.rec.Close()
			p.rec = nil
		}
		p.saved++
	case p.dir != "":
		if err := os.WriteFile(filepath.Join(p.dir, fmt.Sprintf("frame-%06d.jpg", p.saved)), j, 0o644); err == nil {
			p.saved++
		}
	}
	img, err := jpeg.Decode(bytes.NewReader(j))
	if err != nil {
		p.bad++
		return
	}
	b := img.Bounds()
	rgba := image.NewRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
	draw.Draw(rgba, rgba.Bounds(), img, b.Min, draw.Src)
	p.wh = fmt.Sprintf("%dx%d", b.Dx(), b.Dy())
	p.sh.setPicture(b.Dx(), b.Dy(), rgba.Pix)
}

func (p *hxPipe) status() []string {
	if el := time.Since(p.t).Seconds(); el > 0 {
		p.fps = float64(p.n) / el
	}
	p.n, p.t = 0, time.Now()
	var u *hxfpv.UnitSummary
	units := p.rx.Summary()
	for i := range units {
		if units[i].ID == p.unit || (!p.have && i == 0) {
			u = &units[i]
		}
	}
	if u == nil {
		if p.rx.WrongVersion > 0 {
			return []string{fmt.Sprintf("hx packets of protocol version %d (this viewer speaks 2)", p.rx.LastWrongVersion)}
		}
		return []string{"no hx-esp32-cam-fpv air unit heard yet"}
	}
	l := fmt.Sprintf("hx air unit 0x%04x · ch %d · FEC k %d (%s)", u.ID, u.Channel, u.K, u.KSource)
	if u.RSSI != nil {
		l += fmt.Sprintf(" · %d dBm", *u.RSSI)
	}
	if len(units) > 1 {
		l += fmt.Sprintf(" · %d units in range (-unit)", len(units))
	}
	lines := []string{l, fmt.Sprintf("JPEG %s · %.0f frames/s · ok %d, repaired %d, lost %d", p.wh, p.fps, u.St.FramesOK, u.St.FramesFEC, u.St.FramesLost)}
	if o := u.OSD; o != nil {
		res := ""
		if int(o.Resolution) < len(hxfpv.Resolutions) {
			res = hxfpv.Resolutions[o.Resolution] + " · "
		}
		lines = append(lines, fmt.Sprintf("air: %scamera %d fps · %d °C · air RSSI %d dBm · quality %d", res, o.CaptureFPS, o.Temperature, o.RSSIDbm, o.Quality))
	}
	if p.rec != nil || p.dir != "" {
		lines = append(lines, fmt.Sprintf("REC %s (%d frames)", p.cfg.record, p.saved))
	}
	return lines
}

func (p *hxPipe) close() []string {
	switch {
	case p.rec != nil:
		p.rec.Close()
		return []string{fmt.Sprintf("recorded %s: %d JPEG frames (play with: ffplay -f mjpeg %s)", p.cfg.record, p.saved, p.cfg.record)}
	case p.dir != "":
		return []string{fmt.Sprintf("recorded %d frames into %s", p.saved, p.dir)}
	}
	return nil
}
