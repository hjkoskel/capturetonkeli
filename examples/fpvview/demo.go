package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"io"
	"math"
	"math/rand"
	"os/exec"
	"time"

	"github.com/hjkoskel/capturetonkeli/ctp/hxfpv"
	"github.com/hjkoskel/capturetonkeli/ctp/mavlink"
	"github.com/hjkoskel/capturetonkeli/ctp/openhd"
	"github.com/hjkoskel/capturetonkeli/ctp/video"
)

// Demo sources: a simulated air unit in this process, built with the library's senders
// (the same frames a dongle would capture), with random loss on the "air".

type demo struct {
	out  chan rxFrame
	txt  string
	stop func()
}

func (d *demo) frames() <-chan rxFrame { return d.out }
func (d *demo) describe() string       { return d.txt }
func (d *demo) close()                 { d.stop() }

func (d *demo) send(ctx context.Context, r *rand.Rand, loss float64, ch uint8, frames ...[]byte) bool {
	for _, f := range frames {
		if r.Float64() < loss {
			continue
		}
		rssi := int8(-55 - r.Intn(6))
		select {
		case d.out <- rxFrame{data: f, rssi: rssi, hasRSSI: true, channel: ch}:
		case <-ctx.Done():
			return false
		}
	}
	return true
}

// ---- hx-esp32-cam-fpv: an animated test picture as JPEG, 30 fps ----

func newHXDemo(ctx context.Context, cfg *config) *demo {
	ctx, cancel := context.WithCancel(ctx)
	d := &demo{out: make(chan rxFrame, 1024), txt: fmt.Sprintf("demo hx air unit · ch %d · loss %.0f %%", cfg.channel, 100*cfg.demoLoss), stop: cancel}
	go func() {
		defer close(d.out)
		r := rand.New(rand.NewSource(1))
		tx := hxfpv.NewSender(0x1234, 0)
		const w, h = 640, 480
		img := image.NewRGBA(image.Rect(0, 0, w, h))
		tick := time.NewTicker(time.Second / 30)
		defer tick.Stop()
		for i := 0; ; i++ {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
			}
			testPattern(img, i)
			var b bytes.Buffer
			_ = jpeg.Encode(&b, img, &jpeg.Options{Quality: 60})
			slots := hxfpv.VideoSlots(b.Bytes(), uint32(i), 3, tx.Air, tx.GS, tx.MTU)
			if i%10 == 0 {
				o := hxfpv.DefaultOSD
				o.WifiChannel = uint8(cfg.channel)
				o.Resolution = 3
				slots = append(slots, hxfpv.OSDSlot(tx.Air, tx.GS, tx.MTU, o))
			}
			if !d.send(ctx, r, cfg.demoLoss, uint8(cfg.channel), tx.Push(slots...)...) {
				return
			}
		}
	}()
	return d
}

func testPattern(img *image.RGBA, i int) {
	b := img.Bounds()
	t := float64(i) / 30
	for y := 0; y < b.Dy(); y++ {
		for x := 0; x < b.Dx(); x++ {
			bar := (x*8/b.Dx() + i/15) % 8
			c := color.RGBA{uint8(255 * (bar & 1)), uint8(255 * (bar >> 1 & 1)), uint8(255 * (bar >> 2 & 1)), 255}
			if y > b.Dy()*2/3 {
				v := uint8(x * 255 / b.Dx())
				c = color.RGBA{v, v, v, 255}
			}
			img.SetRGBA(x, y, c)
		}
	}
	// a ball bouncing around shows motion and latency
	cx := int(float64(b.Dx()/2) + float64(b.Dx()/3)*math.Sin(t*1.3))
	cy := int(float64(b.Dy()/3) + float64(b.Dy()/5)*math.Cos(t*2.1))
	for y := -30; y <= 30; y++ {
		for x := -30; x <= 30; x++ {
			if x*x+y*y <= 900 {
				img.SetRGBA(cx+x, cy+y, color.RGBA{255, 140, 0, 255})
			}
		}
	}
}

// ---- OpenHD: ffmpeg's test source encoded live, RTP → FEC → session-keyed frames ----

func newOpenHDDemo(ctx context.Context, cfg *config) (*demo, error) {
	codec := cfg.demoCodec
	enc := []string{"-c:v", "libx264", "-preset", "ultrafast", "-tune", "zerolatency", "-bsf:v", "h264_metadata=aud=insert", "-f", "h264"}
	if codec == "h265" {
		enc = []string{"-c:v", "libx265", "-preset", "ultrafast", "-tune", "zerolatency", "-x265-params", "log-level=error", "-bsf:v", "hevc_metadata=aud=insert", "-f", "hevc"}
	}
	args := append([]string{"-hide_banner", "-loglevel", "error", "-nostdin", "-re", "-f", "lavfi", "-i", "testsrc2=size=1280x720:rate=30",
		"-g", fmt.Sprint(max(1, cfg.demoKeyint)), "-bf", "0", "-pix_fmt", "yuv420p"}, enc...)
	cmd := exec.CommandContext(ctx, cfg.ffmpeg, append(args, "pipe:1")...)
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("demo encoder (ffmpeg with lib%s): %w", map[string]string{"h264": "x264", "h265": "x265"}[codec], err)
	}
	ctx, cancel := context.WithCancel(ctx)
	d := &demo{out: make(chan rxFrame, 4096), txt: fmt.Sprintf("demo OpenHD drone (%s) · ch %d · loss %.0f %%", codec, cfg.channel, 100*cfg.demoLoss),
		stop: func() { cancel(); _ = cmd.Process.Kill(); go cmd.Wait() }}
	aus := make(chan []byte, 64)
	go splitAUs(out, codec, aus)
	go func() {
		defer close(d.out)
		r := rand.New(rand.NewSource(2))
		tx := openhd.NewSender(r, openhd.UIDAir, nil, 0x10000000, false)
		pk := video.NewPacketizer(codec)
		lastSession, lastTele := time.Time{}, time.Time{}
		var ts uint32
		start := time.Now()
		for {
			var au []byte
			select {
			case <-ctx.Done():
				return
			case a, ok := <-aus:
				if !ok {
					return
				}
				au = a
			}
			ch := uint8(cfg.channel)
			if time.Since(lastSession) >= time.Second {
				lastSession = time.Now()
				mhz := uint32(5000 + 5*cfg.channel)
				if cfg.channel <= 14 {
					mhz = uint32(2407 + 5*cfg.channel)
				}
				if !d.send(ctx, r, 0, ch, tx.Session(), tx.Mgmt(mhz, 20)) {
					return
				}
			}
			if time.Since(lastTele) >= 200*time.Millisecond {
				lastTele = time.Now()
				if !d.send(ctx, r, cfg.demoLoss, ch, tx.Telemetry(demoTelemetry(time.Since(start)), 2)...) {
					return
				}
			}
			pkts := pk.Packetize(au, ts)
			ts += 3000
			for len(pkts) > 0 { // FEC blocks of up to 32 primaries, +20 %
				n := min(32, len(pkts))
				if !d.send(ctx, r, cfg.demoLoss, ch, tx.Video(pkts[:n], 0, 0, false)...) {
					return
				}
				pkts = pkts[n:]
			}
		}
	}()
	return d, nil
}

// splitAUs cuts an Annex B byte stream into access units at the access unit delimiters.
func splitAUs(r io.Reader, codec string, out chan<- []byte) {
	defer close(out)
	br := bufio.NewReaderSize(r, 1<<20)
	isAUD := func(b []byte) bool {
		if codec == "h265" {
			return (b[0]>>1)&0x3f == 35
		}
		return b[0]&0x1f == 9
	}
	var buf []byte
	chunk := make([]byte, 64<<10)
	scan := 0
	for {
		n, err := br.Read(chunk)
		buf = append(buf, chunk[:n]...)
		for i := max(scan, 1); i+3 < len(buf); i++ {
			if buf[i] == 0 && buf[i+1] == 0 && buf[i+2] == 1 && isAUD(buf[i+3:]) {
				start := i
				if start > 0 && buf[start-1] == 0 {
					start--
				}
				if start > 0 {
					out <- append([]byte(nil), buf[:start]...)
					buf = buf[start:]
					i = 3
				}
			}
		}
		scan = max(0, len(buf)-4)
		if err != nil {
			return
		}
	}
}

// demoTelemetry is a quadcopter in Loiter circling slowly.
func demoTelemetry(t time.Duration) []byte {
	s := t.Seconds()
	hb := make([]byte, 9)
	binary.LittleEndian.PutUint32(hb, 5) // Loiter
	hb[4], hb[5], hb[6], hb[7], hb[8] = 2, 3, 128|1, 4, 3
	hud := make([]byte, 20)
	binary.LittleEndian.PutUint32(hud[0:], math.Float32bits(float32(10+2*math.Sin(s/3))))
	binary.LittleEndian.PutUint32(hud[4:], math.Float32bits(float32(9+2*math.Sin(s/3))))
	binary.LittleEndian.PutUint32(hud[8:], math.Float32bits(float32(40+5*math.Sin(s/7))))
	binary.LittleEndian.PutUint32(hud[12:], math.Float32bits(float32(math.Cos(s/7))))
	binary.LittleEndian.PutUint16(hud[16:], uint16(int(s*6)%360))
	binary.LittleEndian.PutUint16(hud[18:], 55)
	sys := make([]byte, 31)
	binary.LittleEndian.PutUint16(sys[14:], uint16(16200-int(s*2)))
	binary.LittleEndian.PutUint16(sys[16:], 1250)
	sys[30] = byte(max(0, 90-int(s/20)))
	gps := make([]byte, 30)
	gps[28], gps[29] = 3, 14
	var out []byte
	for i, m := range []struct {
		id uint32
		p  []byte
	}{{0, hb}, {74, hud}, {1, sys}, {24, gps}} {
		f, err := mavlink.Encode(m.id, 1, 1, uint8(i), m.p)
		if err == nil {
			out = append(out, f...)
		}
	}
	return out
}
