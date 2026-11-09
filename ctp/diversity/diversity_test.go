package diversity

import (
	"math"
	"math/rand"
	"sort"
	"testing"

	"github.com/hjkoskel/capturetonkeli/ctp/wire"
)

var seqNo int

// dataFrame is a QoS data frame with a sequence number: unique content.
func dataFrame(r *rand.Rand, n int) []byte {
	seqNo++
	f := make([]byte, n)
	f[0], f[1] = 0x88, 0x02
	f[22], f[23] = byte(seqNo<<4), byte(seqNo>>4)
	for i := 24; i < n; i++ {
		f[i] = byte(r.Intn(256))
	}
	return f
}

var ack = []byte{0xd4, 0, 0, 0, 2, 0x11, 0x22, 0x33, 6, 1} // identical every time

func meta(ts float64, data []byte, flags uint8) wire.PktMeta {
	return wire.PktMeta{TsUS: uint64(math.Round(ts)), Channel: 6, Flags: flags, RSSI: -50, OrigLen: uint16(len(data)), CapLen: uint16(len(data))}
}

type airFrame struct {
	t          float64 // µs
	data       []byte
	badA, badB bool
}

type cp struct {
	id   int
	ts   float64
	host float64 // ms
	data []byte
	bad  bool
	air  int
}

// scenario: dongle A (ref, lower MAC) has device time = t; B has t·(1+ppm) + 5 s and ±jitter;
// each copy reaches the host 1–9 ms later (USB batching). Copies in host arrival order.
func scenario(r *rand.Rand, frames []airFrame, ppm, jitter, lossA, lossB float64) []cp {
	var out []cp
	for i, f := range frames {
		ta, tb := f.t, f.t*(1+ppm*1e-6)+5e6+(r.Float64()-0.5)*2*jitter
		if r.Float64() >= lossA {
			out = append(out, cp{0, ta, f.t/1000 + 1 + r.Float64()*8, f.data, f.badA, i})
		}
		if r.Float64() >= lossB {
			out = append(out, cp{1, tb, f.t/1000 + 1 + r.Float64()*8, f.data, f.badB, i})
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].host < out[j].host })
	return out
}

type emitted struct {
	data []byte
	bad  bool
	air  int
}

func run(t *testing.T, copies []cp, m *Merger) []emitted {
	t.Helper()
	if m.Size() == 0 {
		if err := m.SetMembers([]Member{{1, "02:c5:de:40:00:02"}, {0, "02:c5:de:40:00:01"}}); err != nil {
			t.Fatal(err)
		}
	}
	var out []emitted
	for _, c := range copies {
		data, fl := c.data, uint8(0)
		if c.bad {
			data = append([]byte(nil), c.data...)
			data[30] ^= 4
			fl = wire.PktFCSErr
		}
		host := int64(c.host * 1000)
		res, err := m.Offer(c.id, meta(c.ts, data, fl), data, host)
		if err != nil {
			t.Fatal(err)
		}
		if !res.Dup && !res.Held {
			out = append(out, emitted{data: data, air: c.air})
			m.Bind(res.Entry, len(out)-1)
		}
		for _, h := range m.Due(host) {
			out = append(out, emitted{data: h.Data, bad: true})
		}
	}
	m.Flush()
	for _, h := range m.Due(math.MaxInt64) {
		out = append(out, emitted{data: h.Data, bad: true})
	}
	return out
}

func memberOf(s Summary, id int) MemberSummary {
	for _, x := range s.Members {
		if x.ID == id {
			return x
		}
	}
	return MemberSummary{}
}

func TestMergeLockUnique(t *testing.T) {
	r := rand.New(rand.NewSource(1))
	var frames []airFrame
	for ts := 0.0; ts < 3e6; ts += 2000 + r.Float64()*2000 {
		frames = append(frames, airFrame{t: ts, data: dataFrame(r, 60)})
	}
	copies := scenario(r, frames, 23, 4, 0.1, 0.1)
	heard := map[int]bool{}
	for _, c := range copies {
		heard[c.air] = true
	}
	m := NewMerger(Options{})
	out := run(t, copies, m)
	if len(out) != len(heard) {
		t.Fatalf("emitted %d, want every air frame exactly once (%d)", len(out), len(heard))
	}
	s := m.Summary()
	b := memberOf(s, 1)
	if s.Ref != 0 {
		t.Fatalf("ref %d, want the lowest MAC (0)", s.Ref)
	}
	if !b.Clock.Locked || b.Clock.PPM == nil || math.Abs(*b.Clock.PPM-23) > 3 || *b.Clock.RMSUS > 6 {
		t.Fatalf("clock %+v", b.Clock)
	}
	a := memberOf(s, 0)
	if float64(a.Unique) < float64(len(heard))*0.05 || float64(b.Unique) < float64(len(heard))*0.05 {
		t.Fatalf("unique A %d B %d of %d", a.Unique, b.Unique, len(heard))
	}
	if s.Out != uint64(len(heard)) || s.Out+s.Dups != uint64(len(copies)) {
		t.Fatalf("out %d dups %d copies %d", s.Out, s.Dups, len(copies))
	}
}

func TestIdenticalAcksNotMergedWhenLocked(t *testing.T) {
	r := rand.New(rand.NewSource(2))
	var frames []airFrame
	for ts := 0.0; ts < 3e6; ts += 3000 {
		frames = append(frames, airFrame{t: ts, data: dataFrame(r, 60)})
		if ts > 2e6 {
			for k := 1; k <= 3; k++ {
				frames = append(frames, airFrame{t: ts + 300*float64(k), data: ack})
			}
		}
	}
	copies := scenario(r, frames, 23, 4, 0, 0)
	acks := 0
	for _, f := range frames {
		if &f.data[0] == &ack[0] {
			acks++
		}
	}
	out := run(t, copies, NewMerger(Options{}))
	got := 0
	for _, e := range out {
		if len(e.data) == len(ack) && e.data[0] == 0xd4 {
			got++
		}
	}
	if got != acks {
		t.Fatalf("ACKs out %d, in air %d", got, acks)
	}
}

func TestBadCopies(t *testing.T) {
	r := rand.New(rand.NewSource(3))
	var frames []airFrame
	for ts := 0.0; ts < 2e6; ts += 4000 {
		frames = append(frames, airFrame{t: ts, data: dataFrame(r, 60), badA: r.Float64() < 0.2})
	}
	copies := scenario(r, frames, 23, 4, 0, 0)
	bad := 0
	for _, c := range copies {
		if c.bad {
			bad++
		}
	}
	m := NewMerger(Options{})
	out := run(t, copies, m)
	for _, e := range out {
		if e.bad {
			t.Fatal("a bad frame was emitted although B had a good copy")
		}
	}
	if len(out) != len(copies)/2 || m.Summary().Rescued != uint64(bad) {
		t.Fatalf("out %d of %d, rescued %d of %d", len(out), len(copies)/2, m.Summary().Rescued, bad)
	}
	// bad and alone: emitted after the hold
	m2 := NewMerger(Options{})
	_ = m2.SetMembers([]Member{{0, "a"}, {1, "b"}})
	d := dataFrame(r, 60)
	if res, _ := m2.Offer(0, meta(1000, d, wire.PktFCSErr), d, 0); !res.Held {
		t.Fatalf("%+v, want held", res)
	}
	if n := len(m2.Due(HoldTime.Microseconds() - 1)); n != 0 {
		t.Fatal("released before the hold")
	}
	if n := len(m2.Due(HoldTime.Microseconds())); n != 1 {
		t.Fatal("not released after the hold")
	}
}

func TestStaleAnchorsAndMemberLeaving(t *testing.T) {
	r := rand.New(rand.NewSource(9))
	m := NewMerger(Options{})
	_ = m.SetMembers([]Member{{0, "a"}, {1, "b"}})
	var host float64
	for ts := 0.0; ts < 3e6; ts += 3000 {
		d := dataFrame(r, 60)
		host = ts/1000 + 2
		mustOffer(t, m, 0, meta(ts, d, 0), d, host)
		mustOffer(t, m, 1, meta(ts*(1+20e-6)+7e6, d, 0), d, host+1)
		if math.Mod(ts, 300000) == 0 {
			m.members[1].clock.Anchor(ts*(1+20e-6)+7e6, ts+3e5) // 300 ms wrong (stale backlog)
		}
	}
	c := memberOf(m.Summary(), 1).Clock
	if !c.Locked || c.PPM == nil || math.Abs(*c.PPM-20) > 3 {
		t.Fatalf("clock %+v", c)
	}
	// B's first copy is open, B leaves, A's copy of the same frame arrives: no crash, one frame
	d := dataFrame(r, 60)
	mustOffer(t, m, 1, meta(3.1e6*(1+20e-6)+7e6, d, 0), d, host+5)
	_ = m.SetMembers([]Member{{0, "a"}, {2, "c"}})
	if res := mustOffer(t, m, 0, meta(3.1e6, d, 0), d, host+6); !res.Dup {
		t.Fatal("A's copy not merged with the departed member's")
	}
}

func TestSetMembersRange(t *testing.T) {
	if err := NewMerger(Options{}).SetMembers([]Member{{MaxMembers, "x"}}); err == nil {
		t.Fatal("out-of-range id accepted")
	}
	if _, err := NewMerger(Options{}).Offer(0, wire.PktMeta{}, nil, 0); err == nil {
		t.Fatal("unknown member accepted")
	}
}

func TestOnFinalHasAllCopies(t *testing.T) {
	var finals []*Entry
	m := NewMerger(Options{OnFinal: func(e *Entry) {
		if len(e.Data) == 0 {
			t.Error("data released before OnFinal")
		}
		finals = append(finals, e)
	}})
	_ = m.SetMembers([]Member{{0, "a"}, {1, "b"}, {2, "c"}})
	d := dataFrame(rand.New(rand.NewSource(4)), 40)
	for i := 0; i < 3; i++ {
		mm := meta(1000, d, 0)
		mm.RSSI = int8(-50 - 5*i)
		mustOffer(t, m, i, mm, d, 1+float64(i)*0.5)
	}
	m.Flush()
	if len(finals) != 1 || len(finals[0].Copies) != 3 || finals[0].Copies[2].RSSI != -60 || finals[0].Members() != 3 {
		t.Fatalf("%+v", finals)
	}
}

func mustOffer(t *testing.T, m *Merger, id int, mm wire.PktMeta, d []byte, hostMs float64) Result {
	t.Helper()
	res, err := m.Offer(id, mm, d, int64(hostMs*1000))
	if err != nil {
		t.Fatal(err)
	}
	return res
}
