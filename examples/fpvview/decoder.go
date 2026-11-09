package main

import (
	"bytes"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hjkoskel/capturetonkeli/ctp/video"
)

// decoder decodes H.264/H.265 access units with an ffmpeg subprocess (Annex B in, RGBA
// frames out). Low latency: minimal probing, single-threaded decoding and output (frame
// threads add a frame of delay each), and an access unit delimiter after every unit so
// ffmpeg's parser emits a frame at once instead of waiting for the next one.
//
// Real OpenHD cameras send key frames rarely, or never (intra refresh), so the decoder must
// not depend on them: it starts at a key frame, or after keyStartWait with the parameter
// sets seen so far; when ffmpeg falls behind (queue full) units are dropped until the next
// key frame, but for at most keyResumeWait (the picture heals by itself with intra refresh).
type decoder struct {
	bin     string
	sh      *shared
	verbose bool

	cur     *ffproc
	codec   string
	sps     []byte
	waitKey bool
	waitT   time.Time // when waiting for a key frame started
	params  map[string][]byte
	first   time.Time // first access unit without a running decoder
	frames  uint64    // pictures out of ffmpeg
	dropped uint64

	mu sync.Mutex
	wh string
}

type ffproc struct {
	cmd *exec.Cmd
	q   chan []byte
}

func newDecoder(bin string, sh *shared, verbose bool) (*decoder, error) {
	if _, err := exec.LookPath(bin); err != nil {
		return nil, fmt.Errorf("OpenHD video is decoded with ffmpeg, which was not found (%v); install it or pass -ffmpeg", err)
	}
	return &decoder{bin: bin, sh: sh, verbose: verbose, params: map[string][]byte{}}, nil
}

const (
	queueLen      = 64 // ≈ 2 s at 30 fps: covers ffmpeg starting up
	keyStartWait  = 2 * time.Second
	keyResumeWait = time.Second
)

// state is a short decoder status for the overlay ("" when frames flow).
func (d *decoder) state() string {
	switch {
	case d.cur == nil && !d.first.IsZero():
		return fmt.Sprintf("decoder: waiting for a key frame or parameter sets (%.0f s)", time.Since(d.first).Seconds())
	case d.cur == nil:
		return ""
	case d.waitKey:
		return "decoder behind: skipping to the next key frame"
	}
	return ""
}

// remember keeps the latest parameter sets (VPS/SPS/PPS) of the stream.
func (d *decoder) remember(au video.AU) {
	for _, n := range video.SplitAnnexB(au.Data) {
		if len(n) == 0 {
			continue
		}
		var name string
		if au.Codec == video.H264 {
			name = map[byte]string{7: "sps", 8: "pps"}[n[0]&0x1f]
		} else {
			name = map[byte]string{32: "vps", 33: "sps", 34: "pps"}[(n[0]>>1)&0x3f]
		}
		if name != "" {
			d.params[name] = append([]byte(nil), n...)
		}
	}
}

func (d *decoder) haveParams(codec string) bool {
	if d.params["sps"] == nil || d.params["pps"] == nil {
		return false
	}
	return codec == video.H264 || d.params["vps"] != nil
}

// withParams puts the cached parameter sets in front of an access unit.
func (d *decoder) withParams(au video.AU) video.AU {
	var b []byte
	for _, k := range []string{"vps", "sps", "pps"} {
		if p := d.params[k]; p != nil {
			b = append(append(b, 0, 0, 0, 1), p...)
		}
	}
	au.Data = append(b, au.Data...)
	return au
}

func (d *decoder) size() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.wh
}

func spsOf(au video.AU) []byte {
	for _, n := range video.SplitAnnexB(au.Data) {
		if len(n) == 0 {
			continue
		}
		if (au.Codec == video.H264 && n[0]&0x1f == 7) || (au.Codec == video.H265 && (n[0]>>1)&0x3f == 33) {
			return n
		}
	}
	return nil
}

func (d *decoder) feed(au video.AU) {
	if au.Codec != d.codec && d.codec != "" {
		d.params = map[string][]byte{}
	}
	d.remember(au)
	sps := spsOf(au)
	switch {
	case sps != nil && d.cur != nil && (au.Codec != d.codec || !bytes.Equal(sps, d.sps)):
		d.restart(au.Codec, sps) // new resolution or codec
	case d.cur == nil && au.Key && sps != nil:
		if d.verbose {
			log.Printf("decoder: starting at a key frame")
		}
		d.restart(au.Codec, sps)
	case d.cur == nil:
		if d.first.IsZero() {
			d.first = time.Now()
		}
		// no key frame (intra refresh, or rare key frames): start with the parameter sets seen
		if time.Since(d.first) >= keyStartWait && d.haveParams(au.Codec) {
			if d.verbose {
				log.Printf("decoder: no key frame for %v, starting with the cached parameter sets", keyStartWait)
			}
			d.restart(au.Codec, d.params["sps"])
			if d.cur != nil && sps == nil {
				au = d.withParams(au)
			}
		}
	}
	if d.cur == nil {
		return
	}
	d.first = time.Time{}
	if d.waitKey && !au.Key && time.Since(d.waitT) < keyResumeWait {
		d.dropped++
		return
	}
	d.waitKey = false
	select {
	case d.cur.q <- withAUD(au):
	default: // ffmpeg is behind: skip to the next key frame (or resume after keyResumeWait)
		if d.verbose {
			log.Printf("decoder: ffmpeg is behind, skipping to the next key frame")
		}
		d.dropped++
		d.waitKey, d.waitT = true, time.Now()
	}
}

// withAUD drops access unit delimiters inside the unit and appends one after it.
func withAUD(au video.AU) []byte {
	aud, typ := []byte{0, 0, 0, 1, 0x09, 0xf0}, byte(9)
	if au.Codec == video.H265 {
		aud, typ = []byte{0, 0, 0, 1, 0x46, 0x01, 0x50}, 35
	}
	var out []byte
	for _, n := range video.SplitAnnexB(au.Data) {
		if len(n) == 0 {
			continue
		}
		t := n[0] & 0x1f
		if au.Codec == video.H265 {
			t = (n[0] >> 1) & 0x3f
		}
		if t == typ {
			continue
		}
		out = append(append(out, 0, 0, 0, 1), n...)
	}
	return append(out, aud...)
}

func (d *decoder) restart(codec string, sps []byte) {
	info, err := video.ParseSPS(codec, sps)
	if err != nil || info.Width <= 0 || info.Height <= 0 {
		return
	}
	d.stop()
	d.codec, d.sps = codec, append([]byte(nil), sps...)
	d.mu.Lock()
	d.wh = fmt.Sprintf("%dx%d", info.Width, info.Height)
	d.mu.Unlock()
	format := map[string]string{video.H264: "h264", video.H265: "hevc"}[codec]
	cmd := exec.Command(d.bin, "-hide_banner", "-loglevel", "error", "-nostdin",
		// no "-fflags nobuffer": it discards what is read while probing, i.e. the first key
		// frame, and with rare key frames the picture then stays broken for seconds
		"-flags", "low_delay", "-probesize", "32", "-analyzeduration", "0", "-threads", "1",
		"-flags2", "showall", // output frames before the first key frame (intra refresh streams have none)
		"-f", format, "-i", "pipe:0",
		"-threads", "1", // output side too: a frame-threaded rawvideo encoder holds ~4 frames
		"-f", "rawvideo", "-pix_fmt", "rgba", "-vf", fmt.Sprintf("scale=%d:%d", info.Width, info.Height), "pipe:1")
	if d.verbose {
		cmd.Stderr = os.Stderr
	}
	in, err := cmd.StdinPipe()
	if err != nil {
		return
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return
	}
	if err := cmd.Start(); err != nil {
		d.sh.setNote("ffmpeg: " + err.Error())
		return
	}
	if d.verbose {
		log.Printf("decoder: %s %dx%d", codec, info.Width, info.Height)
	}
	p := &ffproc{cmd: cmd, q: make(chan []byte, queueLen)}
	d.cur = p
	go func() { // writer
		defer in.Close()
		for b := range p.q {
			if _, err := in.Write(b); err != nil {
				for range p.q { // drain until stop
				}
				return
			}
		}
	}()
	w, h := info.Width, info.Height
	go func() { // reader: triple-buffered RGBA frames to the window
		var bufs [3][]byte
		for i := range bufs {
			bufs[i] = make([]byte, w*h*4)
		}
		for i := 0; ; i = (i + 1) % 3 {
			if _, err := io.ReadFull(out, bufs[i]); err != nil {
				return
			}
			d.sh.setPicture(w, h, bufs[i])
			atomic.AddUint64(&d.frames, 1)
		}
	}()
}

func (d *decoder) stop() {
	if d.cur == nil {
		return
	}
	close(d.cur.q)
	_ = d.cur.cmd.Process.Kill()
	go d.cur.cmd.Wait()
	d.cur = nil
}

func (d *decoder) close() { d.stop() }
