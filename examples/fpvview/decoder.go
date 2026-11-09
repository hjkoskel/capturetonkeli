package main

import (
	"bytes"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"sync"

	"github.com/hjkoskel/capturetonkeli/ctp/video"
)

// decoder decodes H.264/H.265 access units with an ffmpeg subprocess (Annex B in, RGBA
// frames out). Low latency: no probing or input buffering, single-threaded decoding (frame
// threads add a frame of delay each), and an access unit delimiter after every unit so
// ffmpeg's parser emits a frame at once instead of waiting for the next one. When ffmpeg
// falls behind, units are dropped until the next key frame.
type decoder struct {
	bin     string
	sh      *shared
	verbose bool

	cur     *ffproc
	codec   string
	sps     []byte
	waitKey bool

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
	return &decoder{bin: bin, sh: sh, verbose: verbose}, nil
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
	if au.Key && au.HasParams {
		if sps := spsOf(au); sps != nil && (d.cur == nil || au.Codec != d.codec || !bytes.Equal(sps, d.sps)) {
			d.restart(au.Codec, sps)
		}
	}
	if d.cur == nil {
		return
	}
	if d.waitKey && !au.Key {
		return
	}
	d.waitKey = false
	select {
	case d.cur.q <- withAUD(au):
	default:
		d.waitKey = true // ffmpeg is behind: skip to the next key frame
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
		"-fflags", "nobuffer", "-flags", "low_delay", "-probesize", "32", "-analyzeduration", "0", "-threads", "1",
		"-f", format, "-i", "pipe:0", "-f", "rawvideo", "-pix_fmt", "rgba", "-vf", fmt.Sprintf("scale=%d:%d", info.Width, info.Height), "pipe:1")
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
	p := &ffproc{cmd: cmd, q: make(chan []byte, 8)}
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
