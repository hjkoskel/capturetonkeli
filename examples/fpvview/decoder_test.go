package main

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hjkoskel/capturetonkeli/ctp/video"
)

func fixtureAUs(t *testing.T) []video.AU {
	b, err := os.ReadFile("../../test/js/fixtures/ohd-rtp.json")
	if err != nil {
		t.Fatal(err)
	}
	var v map[string]json.RawMessage
	_ = json.Unmarshal(b, &v)
	var h struct{ RTP []string }
	_ = json.Unmarshal(v["h264"], &h)
	var aus []video.AU
	d := video.NewDepacketizer(video.H264, func(a video.AU) { aus = append(aus, a) })
	for _, p := range h.RTP {
		x, _ := hex.DecodeString(p)
		d.Push(x, false)
	}
	return aus
}

func waitFrames(d *decoder, n uint64) bool {
	for end := time.Now().Add(5 * time.Second); time.Now().Before(end); time.Sleep(20 * time.Millisecond) {
		if atomic.LoadUint64(&d.frames) >= n {
			return true
		}
	}
	return false
}

// Real OpenHD cameras: rare key frames or intra refresh. The decoder must keep showing
// frames after its queue overflowed, and start without ever seeing a key frame.
func TestDecoderWithoutKeyFrames(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	aus := fixtureAUs(t)
	if !aus[0].Key {
		t.Fatal("fixture should start with a key frame")
	}
	// 1. never a key frame: only the parameter sets (as an intra refresh stream repeats them)
	d, _ := newDecoder("ffmpeg", newShared(), false)
	defer d.close()
	d.remember(aus[0])
	d.first = time.Now().Add(-keyStartWait)
	for _, a := range aus[1:] {
		d.feed(a)
	}
	if d.cur == nil || !waitFrames(d, 1) {
		t.Fatalf("no picture without a key frame (state %q)", d.state())
	}
	// 2. overflow: units are skipped, then resume without a key frame after keyResumeWait
	d2, _ := newDecoder("ffmpeg", newShared(), false)
	defer d2.close()
	d2.feed(aus[0])
	d2.waitKey, d2.waitT = true, time.Now().Add(-keyResumeWait)
	for _, a := range aus[1:] {
		d2.feed(a)
	}
	if !waitFrames(d2, uint64(len(aus)/2)) {
		t.Fatalf("decoder did not resume: %d frames", atomic.LoadUint64(&d2.frames))
	}
}

// The picture must match a plain ffmpeg decode of the same stream (the first key frame
// is not lost to stream probing).
func TestDecoderPictureMatchesFFmpeg(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	aus := fixtureAUs(t)
	var es []byte
	for _, a := range aus {
		es = append(es, a.Data...)
	}
	cmd := exec.Command("ffmpeg", "-v", "error", "-f", "h264", "-i", "pipe:0", "-f", "rawvideo", "-pix_fmt", "rgba", "pipe:1")
	cmd.Stdin = bytes.NewReader(es)
	ref, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	sh := newShared()
	d, _ := newDecoder("ffmpeg", sh, false)
	defer d.close()
	for _, a := range aus {
		d.feed(a)
	}
	if !waitFrames(d, uint64(len(aus))) {
		t.Fatalf("%d of %d frames", atomic.LoadUint64(&d.frames), len(aus))
	}
	pic, _, _, _ := sh.snapshot()
	last := ref[len(ref)-len(pic.pix):]
	var diff int
	for i := range last {
		diff += absInt(int(last[i]) - int(pic.pix[i]))
	}
	if mean := float64(diff) / float64(len(last)); mean > 1 {
		t.Fatalf("last picture differs from ffmpeg's by %.1f per byte", mean)
	}
}

func absInt(v int) int {
	if v < 0 {
		return -v
	}
	return v
}
