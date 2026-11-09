package main

import (
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/hjkoskel/capturetonkeli/ctp/mavlink"
	"github.com/hjkoskel/capturetonkeli/ctp/openhd"
	"github.com/hjkoskel/capturetonkeli/ctp/video"
)

// pipeline turns captured frames into pictures and recordings. push and status run on the
// pipeline goroutine; close after it stopped.
type pipeline interface {
	push(f rxFrame)
	status() []string
	close() []string
}

type ohdPipe struct {
	cfg   *config
	sh    *shared
	rx    *openhd.Receiver
	dep   *video.Depacketizer
	dec   *decoder
	rec   *video.Recorder
	fs    *mavlink.FlightState
	unit  int // shown unit id (0 = not chosen yet)
	nth   int // units seen, for -unit
	ids   map[int]int
	auN   int
	auT   time.Time
	auFPS float64
	codec string
	size  string
}

func newOpenHD(cfg *config, sh *shared, hold time.Duration) (*ohdPipe, error) {
	keys := openhd.DefaultKeys()
	switch {
	case cfg.keyFile != "":
		b, err := os.ReadFile(cfg.keyFile)
		if err != nil {
			return nil, err
		}
		if keys, err = openhd.KeysFromKeyFile(b); err != nil {
			return nil, err
		}
	case cfg.phrase != "openhd":
		log.Printf("deriving the keys of the bind phrase (Argon2id)…")
		keys = openhd.KeysFromPhrase(cfg.phrase, nil)
	}
	p := &ohdPipe{cfg: cfg, sh: sh, fs: mavlink.NewFlightState(), ids: map[int]int{}, auT: time.Now()}
	dec, err := newDecoder(cfg.ffmpeg, sh, cfg.verbose)
	if err != nil {
		return nil, err
	}
	p.dec = dec
	if cfg.record != "" {
		if p.rec, err = video.NewRecorder(cfg.record); err != nil {
			dec.close()
			return nil, err
		}
	}
	p.dep = video.NewDepacketizer(video.Auto, p.onAU)
	p.rx = openhd.NewReceiver(openhd.Options{Keys: keys, Unverified: cfg.unverified, Hold: hold,
		OnRTP: p.onRTP,
		OnTelemetry: func(u *openhd.Unit, d []byte, dir uint8) {
			if dir == openhd.UIDAir && (p.unit == 0 || u.ID == p.unit) {
				p.fs.Add(mavlink.Parse(d), time.Now())
			}
		},
		OnSession: func(u *openhd.Unit, kind string) {
			if cfg.verbose {
				log.Printf("OpenHD %s unit %d: session %s", map[uint8]string{1: "air", 2: "ground"}[u.Dir], u.ID, kind)
			}
			if u.ID == p.unit && kind == "restart" {
				p.dep.Reset() // the air unit rebooted: new stream
			}
		}})
	return p, nil
}

func (p *ohdPipe) push(f rxFrame) {
	p.rx.Push(f.data, openhd.RxMeta{RSSI: f.rssi, HasRSSI: f.hasRSSI, Channel: f.channel})
}

func (p *ohdPipe) onRTP(u *openhd.Unit, cam int, pkt []byte, info openhd.FragInfo) {
	if u.Dir != openhd.UIDAir || cam != p.cfg.cam {
		return
	}
	if _, ok := p.ids[u.ID]; !ok {
		p.nth++
		p.ids[u.ID] = p.nth
	}
	if p.unit == 0 && (p.cfg.unit == 0 || p.ids[u.ID] == p.cfg.unit) {
		p.unit = u.ID
	}
	if u.ID != p.unit {
		return
	}
	p.dep.Push(pkt, info.Gap)
}

func (p *ohdPipe) onAU(au video.AU) {
	if p.cfg.verbose && (au.Key || au.Damaged) {
		log.Printf("access unit: key %v, damaged %v, parameter sets %v, %d bytes", au.Key, au.Damaged, au.HasParams, len(au.Data))
	}
	p.auN++
	p.codec = au.Codec
	if p.rec != nil {
		if err := p.rec.Add(au); err != nil {
			p.sh.setNote("recording stopped: " + err.Error())
			p.rec.Close()
			p.rec = nil
		}
	}
	p.dec.feed(au)
}

func (p *ohdPipe) status() []string {
	if el := time.Since(p.auT).Seconds(); el > 0 {
		p.auFPS = float64(p.auN) / el
	}
	p.auN, p.auT = 0, time.Now()
	s := p.rx.Summary()
	var lines []string
	var shown *openhd.UnitSummary
	for i := range s.Units {
		if s.Units[i].ID == p.unit {
			shown = &s.Units[i]
		}
	}
	switch {
	case shown != nil:
		l := fmt.Sprintf("OpenHD air unit %d · Gen %s · ch %d", p.ids[shown.ID], shown.Gen, shown.Channel)
		if shown.RSSI != nil {
			l += fmt.Sprintf(" · %d dBm", *shown.RSSI)
		}
		if shown.Loss != nil {
			l += fmt.Sprintf(" · loss %.1f %%", 100**shown.Loss)
		}
		if !shown.Verified {
			l += " · NOT VERIFIED (no key)"
		}
		lines = append(lines, l)
		if f, ok := shown.FEC[p.cfg.cam]; ok {
			lines = append(lines, fmt.Sprintf("video %s %s · %.0f frames/s · FEC blocks ok %d, repaired %d, lost %d",
				p.codec, p.dec.size(), p.auFPS, f.BlocksOK, f.BlocksFEC, f.BlocksLost))
		}
		if st := p.dec.state(); st != "" {
			lines = append(lines, st)
		}
	case s.G.OhdFrames[1] > 0 && !s.Keys.Air:
		lines = append(lines, "OpenHD frames heard, but no key for the air unit")
	case s.G.BadSession[1] > 0:
		lines = append(lines, "OpenHD session key does not open: wrong bind phrase? (-bind-phrase, -key-file, or -unverified)")
	case s.G.OhdFrames[1] > 0:
		lines = append(lines, "OpenHD frames heard, waiting for the session key (sent once a second)")
	default:
		lines = append(lines, fmt.Sprintf("no OpenHD frames yet (%d frames captured)", s.G.Frames))
	}
	if fl := p.fs.Summary(time.Now()).Flight; fl != nil {
		var parts []string
		if fl.Mode != "" {
			parts = append(parts, fl.Mode)
		}
		if fl.Armed != nil && *fl.Armed {
			parts = append(parts, "ARMED")
		}
		if fl.Battery != nil {
			parts = append(parts, fmt.Sprintf("%.1f V", fl.Battery.Voltage))
		}
		if fl.HUD != nil {
			parts = append(parts, fmt.Sprintf("alt %.0f m · %.0f m/s · hdg %d°", fl.HUD.Alt, fl.HUD.Groundspeed, fl.HUD.Heading))
		}
		if fl.GPS != nil {
			parts = append(parts, fmt.Sprintf("%d sats", fl.GPS.Satellites))
		}
		if len(parts) > 0 {
			lines = append(lines, strings.Join(parts, " · "))
		}
	}
	if p.rec != nil {
		lines = append(lines, "REC "+p.cfg.record)
	}
	return lines
}

func (p *ohdPipe) close() []string {
	p.dec.close()
	if p.rec == nil {
		return nil
	}
	files, err := p.rec.Close()
	var out []string
	for _, f := range files {
		if f.Frames > 0 {
			out = append(out, fmt.Sprintf("recorded %s: %dx%d, %d frames, %.1f s", f.Path, f.Width, f.Height, f.Frames, float64(f.Ms)/1000))
		} else {
			out = append(out, "recorded "+f.Path)
		}
	}
	if len(files) == 0 {
		out = append(out, "nothing recorded (no key frame received)")
	}
	if err != nil {
		out = append(out, "recording: "+err.Error())
	}
	return out
}
