package hxfpv

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"os"
	"sort"
	"testing"

	"github.com/hjkoskel/capturetonkeli/ctp/wire"
)

func hx(s string) []byte { b, _ := hex.DecodeString(s); return b }

// C structs of hx-esp32-cam-fpv (same fixture as test/js/hxfpv.test.mjs).
func TestPacketsMatchC(t *testing.T) {
	b, err := os.ReadFile("../../test/js/fixtures/hxfpv-packets.json")
	if err != nil {
		t.Fatal(err)
	}
	var v struct {
		CRC                int `json:"crc8_123456789"`
		Video, OSD, Config string
	}
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
	if int(CRC8([]byte("123456789"))) != v.CRC {
		t.Fatal("crc8")
	}
	video := hx(v.Video)
	if !CRCOK(video, VideoHdr) || ParseA2G(video) != (A2G{Type: 0, Size: 23, Pong: 7, Version: 2, CRC: video[7], Air: 0xa1b2, GS: 0xc3d4}) {
		t.Fatalf("%+v", ParseA2G(video))
	}
	if video[13]&0x7f != 3 || video[13]&0x80 == 0 {
		t.Fatal("video part")
	}
	osd := hx(v.OSD)
	if len(osd) != OSDSize || !CRCOK(osd, OSDSize) {
		t.Fatal("osd crc")
	}
	s := ParseAirStats(osd)
	got := []any{s.WifiRate, s.Quality, s.OV5640, s.SDFreeGB, s.SDTotalGB, s.RSSIDbm, s.NoiseFloorDbm, s.CaptureFPS, s.FrameSizeMin, s.FrameSizeMax,
		s.WifiChannel, s.Resolution, s.Temperature, s.FecK, s.InSession, s.Aspect, s.Brightness, s.Contrast, s.Saturation, s.Sharpness, s.AELevel, s.AirRecording, s.WifiQueueMax}
	want := []any{uint8(20), uint8(17), true, 20.0, 32.0, -63, -92, uint8(30), uint16(9000), uint16(21000),
		uint8(7), uint8(5), uint8(51), uint8(6), true, uint8(2), -2, 1, -1, 3, 2, true, uint8(33)}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("field %d: %v, want %v", i, got[i], want[i])
		}
	}
	cfg := hx(v.Config)
	c := ParseConfig(cfg)
	c.At = c.At.UTC()
	if len(cfg) != ConfigSize || !CRCOK(cfg, ConfigSize) || c != (Config{Resolution: 10, FPSLimit: 50, Quality: 12, WifiPower: -3, WifiRate: 22, WifiChannel: 11, FecK: 4, FecN: 8, MTU: 1464, At: c.At}) {
		t.Fatalf("%+v", c)
	}
	osd[20] ^= 1
	if CRCOK(osd, OSDSize) {
		t.Fatal("corrupted OSD passed")
	}
}

func fakeJPEG(n int, seed uint32) []byte {
	j := make([]byte, n)
	x := seed
	for i := range j {
		x = x*1103515245 + 12345
		j[i] = byte(x >> 24)
	}
	copy(j, []byte{0xff, 0xd8, 0xff, 0xc0, 0, 11, 8, 0x01, 0xe0, 0x02, 0x80}) // SOF0 480×640
	copy(j[n-2:], []byte{0xff, 0xd9})
	return j
}

type got struct {
	jpeg []byte
	info FrameInfo
}

func run(t *testing.T, frames, k, n int, loss func(i int) bool) ([]got, [][]byte, *Receiver) {
	t.Helper()
	tx := NewSender(0xbeef, 0x4242)
	tx.K, tx.N = k, n
	var out []got
	rx := NewReceiver(Options{OnFrame: func(_ *Unit, j []byte, info FrameInfo) { out = append(out, got{j, info}) }})
	var sent [][]byte
	seq := 0
	for i := 0; i < frames; i++ {
		j := fakeJPEG(9000+i*37, uint32(i+1))
		sent = append(sent, j)
		slots := VideoSlots(j, uint32(i), 3, 0xbeef, 0x4242, 1464)
		if i%5 == 0 {
			f := DefaultOSD
			f.FecK = uint8(k)
			slots = append(slots, OSDSlot(0xbeef, 0x4242, 1464, f))
		}
		for _, f := range tx.Push(slots...) {
			if !loss(seq) {
				rx.Push(f, RxMeta{RSSI: -50, HasRSSI: true, Channel: 7})
			}
			seq++
		}
	}
	for _, g := range out {
		if !bytes.Equal(g.jpeg, sent[g.info.Index]) {
			t.Fatalf("frame %d differs", g.info.Index)
		}
	}
	return out, sent, rx
}

func TestCleanLink(t *testing.T) {
	out, _, rx := run(t, 20, 6, 12, func(int) bool { return false })
	if len(out) < 18 {
		t.Fatalf("%d frames", len(out))
	}
	u := rx.Summary()[0]
	if u.ID != 0xbeef || u.GS != 0x4242 || u.K != 6 || u.KSource != "OSD" || u.St.FramesFEC != 0 || u.St.CRCErrors != 0 || u.St.FramesLost != 0 || u.OSD.CaptureFPS != 30 {
		t.Fatalf("%+v", u)
	}
	if w, h, ok := JPEGSize(out[0].jpeg); !ok || w != 640 || h != 480 {
		t.Fatal("jpeg size")
	}
}

func TestFECAndLoss(t *testing.T) {
	out, _, rx := run(t, 20, 6, 12, func(i int) bool { return i%12 < 4 || i%12 == 7 })
	u := rx.Summary()[0]
	if len(out) < 18 || u.St.BlocksFEC == 0 || u.St.FramesFEC == 0 || u.St.BlocksLost != 0 {
		t.Fatalf("%d frames %+v", len(out), u.St)
	}
	out, _, rx = run(t, 20, 6, 12, func(i int) bool { return i/12 == 3 && i%12 < 7 })
	u = rx.Summary()[0]
	later := false
	for _, g := range out {
		later = later || g.info.Index > 6
	}
	if u.St.BlocksLost < 1 || u.St.FramesLost < 1 || !later {
		t.Fatalf("%+v", u.St)
	}
}

func TestDuplicatesForeignVersionTwoUnits(t *testing.T) {
	rx := NewReceiver(Options{})
	tx1, tx2 := NewSender(1, 0), NewSender(2, 0)
	for _, f := range tx1.Push(VideoSlots(fakeJPEG(3000, 1), 0, 0, 1, 0, 1464)...) {
		rx.Push(f, RxMeta{})
		rx.Push(f, RxMeta{})
	}
	for _, f := range tx2.Push(VideoSlots(fakeJPEG(3000, 2), 0, 0, 2, 0, 1464)...) {
		rx.Push(f, RxMeta{})
	}
	s := rx.Summary()
	if len(s) != 2 || s[0].ID != 1 || s[1].ID != 2 || s[0].St.Duplicates == 0 {
		t.Fatalf("%+v", s)
	}
	beacon := make([]byte, 60)
	beacon[0] = 0x80
	if rx.Push(beacon, RxMeta{}) {
		t.Fatal("beacon taken")
	}
	old := tx1.Push(VideoSlots(fakeJPEG(100, 3), 1, 0, 1, 0, 1464)...)[0]
	old[24] = 1
	if !rx.Push(old, RxMeta{}) || rx.WrongVersion != 1 {
		t.Fatal("wrong version not counted")
	}
}

func TestKFollowsAirUnitAndRestart(t *testing.T) {
	var idx []uint32
	rx := NewReceiver(Options{OnFrame: func(_ *Unit, _ []byte, info FrameInfo) { idx = append(idx, info.Index) }})
	tx := NewSender(7, 0)
	tx.K, tx.N = 4, 8
	f := DefaultOSD
	f.FecK = 4
	for _, fr := range tx.Push(OSDSlot(7, 0, 1464, f)) {
		rx.Push(fr, RxMeta{})
	}
	for i := 0; i < 6; i++ {
		for _, fr := range tx.Push(VideoSlots(fakeJPEG(5000, uint32(i+1)), uint32(i), 0, 7, 0, 1464)...) {
			rx.Push(fr, RxMeta{})
		}
	}
	if rx.Summary()[0].K != 4 || len(idx) < 5 {
		t.Fatalf("k %d frames %v", rx.Summary()[0].K, idx)
	}
	tx2 := NewSender(7, 0)
	tx2.K, tx2.N = 4, 8
	tx.Block += 300
	before := len(idx)
	for i := 0; i < 400; i++ {
		for _, fr := range tx.Push(VideoSlots(fakeJPEG(200, uint32(i)), uint32(10+i), 0, 7, 0, 1464)...) {
			rx.Push(fr, RxMeta{})
		}
	}
	for i := 0; i < 5; i++ {
		for _, fr := range tx2.Push(VideoSlots(fakeJPEG(5000, uint32(i+9)), uint32(i), 0, 7, 0, 1464)...) {
			rx.Push(fr, RxMeta{})
		}
	}
	after := idx[before:]
	sort.Slice(after, func(i, j int) bool { return after[i] < after[j] })
	has3 := false
	for _, x := range after {
		has3 = has3 || x == 3
	}
	if rx.Summary()[0].St.Restarts < 1 || !has3 {
		t.Fatalf("restarts %d, frames after restart %v", rx.Summary()[0].St.Restarts, after[:min(10, len(after))])
	}
}

func TestCaptureFilter(t *testing.T) {
	for _, keep := range []int{0, 8, 16} {
		set := CaptureFilter(keep)
		if err := set.Validate(0); err != nil {
			t.Fatal(err)
		}
		tx := NewSender(1, 0)
		var slots [][]byte
		for i := 0; i < 6; i++ {
			slots = append(slots, make([]byte, 1464))
		}
		passed := 0
		for _, f := range tx.Push(slots...) {
			if set.Match(f, wire.PktMeta{RSSI: -50, OrigLen: uint16(len(f)), CapLen: uint16(len(f))}) {
				passed++
			}
		}
		want := 12
		if keep == 8 {
			want = 8
		}
		if passed != want {
			t.Fatalf("keep %d: %d passed", keep, passed)
		}
	}
	beacon := make([]byte, 60)
	beacon[0] = 0x80
	if CaptureFilter(8).Match(beacon, wire.PktMeta{OrigLen: 60, CapLen: 60}) {
		t.Fatal("beacon matched")
	}
}
