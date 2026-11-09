package openhd

import (
	"math"
	"math/rand"
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/hjkoskel/capturetonkeli/ctp/wire"
)

type clock struct{ t time.Time }

func (c *clock) now() time.Time      { return c.t }
func (c *clock) add(d time.Duration) { c.t = c.t.Add(d) }

func newClock() *clock { return &clock{t: time.Unix(1700000000, 0)} }

func rtpPkt(seq, n int) []byte {
	p := make([]byte, n)
	p[0], p[1], p[2], p[3] = 0x80, 96, byte(seq>>8), byte(seq)
	for i := 12; i < n; i++ {
		p[i] = byte(i * seq)
	}
	return p
}

func seqOf(p []byte) int { return int(p[2])<<8 | int(p[3]) }

// OpenHD-generated frames (wifibroadcast's own Encryptor, FECEncoder, ...), same fixtures
// as test/js/openhd.test.mjs.
func TestDecodesOpenHDFrames(t *testing.T) {
	for _, gen := range []string{"a", "b"} {
		var v genFixture
		fixture(t, "ohd-gen-"+gen+".json", &v)
		var rtp [2][]string
		var tele []string
		var teleDir []uint8
		var mgmt []Mgmt
		var sess []string
		c := newClock()
		rx := NewReceiver(Options{Now: c.now,
			OnRTP:       func(u *Unit, cam int, p []byte, _ FragInfo) { rtp[cam] = append(rtp[cam], toHex(p)) },
			OnTelemetry: func(u *Unit, d []byte, dir uint8) { tele = append(tele, toHex(d)); teleDir = append(teleDir, dir) },
			OnMgmt:      func(u *Unit, m Mgmt) { mgmt = append(mgmt, m) },
			OnSession:   func(u *Unit, kind string) { sess = append(sess, kind) }})
		i := 0
		for _, fr := range v.Frames {
			if fr.Kind == "session" {
				if i == 1 {
					c.add(2 * time.Second) // the restart happens 2 s later
				}
				i++
			}
			if !fr.Drop {
				rx.Push(hx(fr.Frame), RxMeta{RSSI: -60, HasRSSI: true, Channel: 149})
			}
		}
		if !reflect.DeepEqual(rtp[0], v.Expect.RTP0) || !reflect.DeepEqual(rtp[1], v.Expect.RTP1) {
			t.Fatalf("gen %s: rtp %d/%d packets, want %d/%d", gen, len(rtp[0]), len(rtp[1]), len(v.Expect.RTP0), len(v.Expect.RTP1))
		}
		if len(tele) != 2 || tele[0] != v.Expect.Telemetry[0] || teleDir[0] != 1 || teleDir[1] != 2 {
			t.Fatalf("gen %s: telemetry %v %v", gen, tele, teleDir)
		}
		if !reflect.DeepEqual(mgmt, []Mgmt{{5745, 20}}) || !reflect.DeepEqual(sess, []string{"new", "new", "restart"}) {
			t.Fatalf("gen %s: mgmt %v sessions %v", gen, mgmt, sess)
		}
		s := rx.Summary()
		var air *UnitSummary
		for i := range s.Units {
			if s.Units[i].Dir == UIDAir {
				air = &s.Units[i]
			}
		}
		if air == nil || air.Gen != map[string]string{"a": "A", "b": "B"}[gen] || air.St.Restarts != 1 || s.G.AuthFail != 1 || air.FEC[0].BlocksLost != 0 {
			t.Fatalf("gen %s: %+v auth fail %d", gen, air, s.G.AuthFail)
		}
	}
}

func TestWrongPhraseUnverifiedTwoSystems(t *testing.T) {
	var v genFixture
	fixture(t, "ohd-gen-a.json", &v)
	other := Keys{Air: hx("1111111111111111111111111111111111111111111111111111111111111111")}
	n := 0
	rx := NewReceiver(Options{Keys: other, OnRTP: func(*Unit, int, []byte, FragInfo) { n++ }})
	for _, fr := range v.Frames {
		if !fr.Drop {
			rx.Push(hx(fr.Frame), RxMeta{})
		}
	}
	if s := rx.Summary(); n != 0 || s.G.BadSession[1] < 2 || s.G.NoSession == 0 {
		t.Fatalf("n %d %+v", n, s.G)
	}
	// keyless: video can still be shown, marked unverified
	rx2 := NewReceiver(Options{Keys: other, Unverified: true, OnRTP: func(*Unit, int, []byte, FragInfo) { n++ }})
	for _, fr := range v.Frames {
		if !fr.Drop {
			rx2.Push(hx(fr.Frame), RxMeta{})
		}
	}
	if s := rx2.Summary(); n == 0 || len(s.Units) != 1 || s.Units[0].Verified || s.Units[0].Dir != 1 {
		t.Fatalf("unverified: n %d %+v", n, s.Units)
	}
	// two OpenHD systems with the default phrase on one channel: two units
	got := map[int]int{}
	rx3 := NewReceiver(Options{OnRTP: func(u *Unit, _ int, _ []byte, _ FragInfo) { got[u.ID]++ }})
	rng := rand.New(rand.NewSource(5))
	a, b := NewSender(rng, UIDAir, nil, 1000, false), NewSender(rng, UIDAir, nil, 7000000000, false)
	rx3.Push(a.Session(), RxMeta{})
	rx3.Push(b.Session(), RxMeta{})
	for i := 0; i < 5; i++ {
		for _, f := range a.Video([][]byte{rtpPkt(i, 300)}, 0, 0, false) {
			rx3.Push(f, RxMeta{})
		}
		for _, f := range b.Video([][]byte{rtpPkt(i, 400)}, 0, 0, false) {
			rx3.Push(f, RxMeta{})
		}
	}
	if !reflect.DeepEqual(got, map[int]int{1: 5, 2: 5}) {
		t.Fatalf("%v", got)
	}
}

func TestFecStreamOrderQueueLoss(t *testing.T) {
	tx := NewSender(rand.New(rand.NewSource(9)), UIDAir, nil, 0x10000000, false)
	type o struct {
		seq int
		FragInfo
	}
	var out []o
	rx := NewReceiver(Options{OnRTP: func(_ *Unit, _ int, p []byte, info FragInfo) { out = append(out, o{seqOf(p), info}) }})
	rx.Push(tx.Session(), RxMeta{})
	seq := 0
	var blocks [][][]byte
	for b := 0; b < 6; b++ {
		var ps [][]byte
		for i := 0; i < 5; i++ {
			seq++
			ps = append(ps, rtpPkt(seq-1, 200+(seq*37)%900))
		}
		blocks = append(blocks, tx.Video(ps, 0, 2, false))
	}
	push := func(f []byte) { rx.Push(f, RxMeta{}) }
	for _, f := range blocks[0] {
		push(f)
	}
	for i, f := range blocks[1] { // loses 2 primaries: recovered
		if i != 1 && i != 3 {
			push(f)
		}
	}
	for i, f := range blocks[2] { // loses 3: lost
		if i > 2 {
			push(f)
		}
	}
	for _, f := range blocks[3] { // duplicated
		push(f)
		push(f)
	}
	for _, f := range blocks[4] {
		push(f)
	}
	var seqs []int
	for _, x := range out {
		seqs = append(seqs, x.seq)
	}
	if !reflect.DeepEqual(seqs, []int{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 15, 16, 17, 18, 19, 20, 21, 22, 23, 24}) {
		t.Fatalf("%v", seqs)
	}
	for _, x := range out {
		if x.seq == 6 && !x.Recovered || x.seq == 15 && !x.Gap {
			t.Fatalf("%+v", x)
		}
	}
	st := rx.Summary().Units[0].FEC[0]
	if st.BlocksOK != 3 || st.BlocksFEC != 1 || st.BlocksLost != 1 || st.Duplicates == 0 || st.SecUnneeded == 0 {
		t.Fatalf("%+v", st)
	}
	// ForwardGapped: primaries after the gap of a lost block are delivered too
	var out2 []int
	rx2 := NewReceiver(Options{ForwardGapped: true, OnRTP: func(_ *Unit, _ int, p []byte, _ FragInfo) { out2 = append(out2, seqOf(p)) }})
	rx2.Push(tx.Session(), RxMeta{})
	for i, f := range blocks[2] {
		if i != 1 && i != 2 && i != 3 {
			rx2.Push(f, RxMeta{})
		}
	}
	for _, f := range blocks[3] {
		rx2.Push(f, RxMeta{})
	}
	if !reflect.DeepEqual(out2, []int{10, 14, 15, 16, 17, 18, 19}) {
		t.Fatalf("%v", out2)
	}
}

func TestSenderReceiverGenB(t *testing.T) {
	tx := NewSender(rand.New(rand.NewSource(3)), UIDAir, nil, 0x10000000, true)
	rtp := 0
	var tele []string
	var mg *Mgmt
	rx := NewReceiver(Options{OnRTP: func(*Unit, int, []byte, FragInfo) { rtp++ },
		OnTelemetry: func(_ *Unit, d []byte, _ uint8) { tele = append(tele, toHex(d)) },
		OnMgmt:      func(_ *Unit, m Mgmt) { mg = &m }})
	rx.Push(tx.Session(), RxMeta{})
	for _, f := range tx.Video([][]byte{rtpPkt(1, 1441), rtpPkt(2, 10)}, 0, 0, true) {
		rx.Push(f, RxMeta{})
	}
	for _, f := range tx.Telemetry([]byte{1, 2, 3}, 2) {
		rx.Push(f, RxMeta{})
	}
	rx.Push(tx.Mgmt(5785, 40), RxMeta{})
	if rtp != 2 || !reflect.DeepEqual(tele, []string{"010203"}) || mg == nil || *mg != (Mgmt{5785, 40}) || rx.Summary().Units[0].Gen != "B" {
		t.Fatalf("rtp %d tele %v mgmt %v", rtp, tele, mg)
	}
}

func TestCaptureFilterAndChannels(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	tx, gnd := NewSender(rng, UIDAir, nil, 1, false), NewSender(rng, UIDGnd, nil, 1, false)
	frames := [][]byte{tx.Session()}
	frames = append(frames, tx.Video([][]byte{rtpPkt(1, 100)}, 0, 0, false)...)
	frames = append(frames, tx.Video([][]byte{rtpPkt(2, 100)}, 0, 0, true)...)
	frames = append(frames, tx.Telemetry([]byte{1}, 2)...)
	frames = append(frames, tx.Mgmt(5745, 20))
	gf := append([][]byte{gnd.Session()}, gnd.Telemetry([]byte{1}, 2)...)
	meta := func(f []byte) wire.PktMeta {
		return wire.PktMeta{RSSI: -50, OrigLen: uint16(len(f)), CapLen: uint16(len(f))}
	}
	for _, ground := range []bool{false, true} {
		set := CaptureFilter(ground)
		if err := set.Validate(0); err != nil {
			t.Fatal(err)
		}
		if _, err := set.Encode(); err != nil {
			t.Fatal(err)
		}
		for _, f := range frames {
			if !set.Match(f, meta(f)) {
				t.Fatalf("air frame not matched (port %d)", f[15]>>1)
			}
		}
		n := 0
		for _, f := range gf {
			if set.Match(f, meta(f)) {
				n++
			}
		}
		if (ground && n != len(gf)) || (!ground && n != 0) {
			t.Fatalf("ground %v: %d of %d", ground, n, len(gf))
		}
		beacon := make([]byte, 60)
		beacon[0] = 0x80
		if set.Match(beacon, meta(beacon)) {
			t.Fatal("beacon matched")
		}
	}
	var chs []int
	for _, f := range RaceBandMHz {
		chs = append(chs, FreqToChannel(f))
	}
	if !reflect.DeepEqual(chs, []int{140, 149, 157, 165, 173, 52, 56}) || FreqToChannel(2452) != 9 || FreqToChannel(5000) != 0 {
		t.Fatalf("%v", chs)
	}
}

// docs/14 §9.3: with several dongles a missing packet's copy from another dongle arrives a
// few ms later, after newer packets. Holding incomplete blocks lets it complete them.
func TestHoldLetsLateCopiesCompleteBlocks(t *testing.T) {
	r := rand.New(rand.NewSource(5))
	tx := NewSender(r, UIDAir, nil, 0x10000000, false)
	type air struct {
		t   float64 // ms
		f   []byte
		blk int
	}
	frames := []air{{t: -50, f: tx.Session(), blk: -1}}
	seq := 0
	for i := 0; i < 300; i++ {
		for b := 0; b < 3; b++ {
			var ps [][]byte
			for j := 0; j < 8; j++ {
				ps = append(ps, rtpPkt(seq, 1000))
				seq++
			}
			for _, f := range tx.Video(ps, 0, 0, false) {
				frames = append(frames, air{t: float64(i*33) + float64(b)*0.5 + float64(len(frames))*0.01, f: f, blk: i*3 + b})
			}
		}
	}
	type cp struct {
		host float64
		id   int
	}
	var copies []cp
	for id, a := range frames {
		for d := 0; d < 2; d++ {
			if a.t < 0 || r.Float64() >= 0.15 {
				copies = append(copies, cp{a.t + 1 + r.Float64()*8, id})
			}
		}
	}
	sort.SliceStable(copies, func(i, j int) bool { return copies[i].host < copies[j].host })
	got := map[int]bool{}
	for _, c := range copies {
		got[c.id] = true
	}
	missing := map[int]int{}
	for id, a := range frames {
		if a.blk >= 0 && !got[id] {
			missing[a.blk]++
		}
	}
	hopeless := 0
	for _, n := range missing {
		if n > 2 { // more than m = 2 of 10 fragments heard by nobody
			hopeless++
		}
	}
	unionLoss := 1 - float64(len(got))/float64(len(frames))
	result := map[time.Duration][2]float64{}
	for _, hold := range []time.Duration{0, 15 * time.Millisecond} {
		c := newClock()
		base := c.t
		rx := NewReceiver(Options{Now: c.now, Hold: hold})
		seen := map[int]bool{}
		for _, x := range copies {
			if seen[x.id] { // first copy wins (the merger drops later ones)
				continue
			}
			seen[x.id] = true
			c.t = base.Add(time.Duration(x.host * float64(time.Millisecond)))
			rx.Push(frames[x.id].f, RxMeta{})
		}
		u := rx.Summary().Units[0]
		result[hold] = [2]float64{float64(u.FEC[0].BlocksLost), float64(u.St.NonceGaps) / float64(u.St.NonceSpan)}
	}
	if result[0][0] < 100 {
		t.Fatalf("without hold the late copies should be useless: %v", result)
	}
	if int(result[15*time.Millisecond][0]) != hopeless {
		t.Fatalf("with hold only blocks both dongles lost beyond FEC are lost: %v, hopeless %d", result, hopeless)
	}
	if math.Abs(result[15*time.Millisecond][1]-unionLoss) > 0.005 {
		t.Fatalf("loss %v vs union %v", result[15*time.Millisecond][1], unionLoss)
	}
}
