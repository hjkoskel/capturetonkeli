// Package diversity merges the PKT streams of several dongles on the same channel into
// one (multi-dongle receive diversity, docs/14 §4). It is the Go port of js/diversity.js
// and follows the same algorithm and constants:
//
//   - First copy wins: it is emitted at once (no added latency); later copies of the same
//     air frame only add their RSSI (Entry.Copies).
//   - Dedup key: CRC-32 over the captured bytes + orig_len + channel + cap_len (docs/14 §4.3
//     fallback key; the ESP32-C5 delivers no FCS, docs/03 rev 3).
//   - Two copies with the same key are the same air frame only when their times, mapped to
//     the reference dongle's clock, agree: rough (each dongle's host − device offset, lower
//     envelope; tolerance 10 ms) until the fine model t_ref = a·t + b locks (≥ 8 anchors,
//     RMS < 50 µs; tolerance from the residual, ≥ 20 µs).
//   - The reference is the member with the lowest MAC.
//   - Copies with a bad FCS attach to a good copy that differs in ≤ 16 bits, or are held
//     HoldTime for one, then emitted as they are (Due).
//
// The Merger is not safe for concurrent use and takes the host time as an argument, so it
// is deterministic in tests; ctp.Group drives it from one goroutine.
package diversity

import (
	"fmt"
	"hash/crc32"
	"math"
	"math/bits"
	"sort"
	"time"

	"github.com/hjkoskel/capturetonkeli/ctp/wire"
)

// Limits and timing (same values as js/diversity.js).
const (
	MaxMembers    = 8
	DefaultWindow = 100 * time.Millisecond // a frame's copies must arrive within this (host time)
	HoldTime      = 15 * time.Millisecond  // bad copies wait this long for a good one

	maxBitDiff     = 16
	unlockedTolUS  = 10000.0 // ≥ USB delivery jitter between dongles (rough offsets)
	envelopeSlack  = 200e-6  // rough offset may rise 200 µs/s
	anchorSpanUS   = 2e6
	maxAnchors     = 400
	lockMin        = 8
	lockRMSUS      = 50.0
	refTolUS       = 20.0
	maxFineTolUS   = 500.0
	medianWindowUS = 2000.0
)

// Clock maps one dongle's device time (µs) to the reference dongle's.
type Clock struct {
	anchors  [][2]float64
	a, b     float64
	rms      float64
	locked   bool
	used     int
	sinceFit int
	env      float64
	hasEnv   bool
	envT     float64
}

// NewClock returns an unlocked clock model.
func NewClock() *Clock { c := &Clock{}; c.Reset(); return c }

// Reset forgets the fine model (the rough envelope is kept).
func (c *Clock) Reset() {
	c.anchors = c.anchors[:0]
	c.a, c.b, c.rms, c.locked, c.used, c.sinceFit = 1, 0, math.Inf(1), false, 0, 0
}

// Observe updates the rough axis: lower envelope of host − device time.
func (c *Clock) Observe(tsUS, hostUS float64) {
	s := hostUS - tsUS
	if !c.hasEnv {
		c.env, c.hasEnv = s, true
	} else {
		c.env = math.Min(c.env+envelopeSlack*math.Max(0, hostUS-c.envT), s)
	}
	c.envT = hostUS
}

// Anchor adds one frame heard by this dongle at x and by the reference at y.
func (c *Clock) Anchor(x, y float64) {
	c.anchors = append(c.anchors, [2]float64{x, y})
	drop := 0
	for len(c.anchors)-drop > maxAnchors || (len(c.anchors)-drop > 0 && x-c.anchors[drop][0] > anchorSpanUS) {
		drop++
	}
	if drop > 0 {
		c.anchors = append(c.anchors[:0], c.anchors[drop:]...)
	}
	c.sinceFit++
	every := 2
	if c.locked {
		every = 16
	}
	if c.sinceFit >= every {
		c.fit()
	}
}

func (c *Clock) fit() {
	c.sinceFit = 0
	if len(c.anchors) < 2 {
		return
	}
	// robust start: drop pairs far from the median offset
	off := make([]float64, len(c.anchors))
	for i, p := range c.anchors {
		off[i] = p[1] - p[0]
	}
	sort.Float64s(off)
	med := off[len(off)>>1]
	pts := make([][2]float64, 0, len(c.anchors))
	for _, p := range c.anchors {
		if math.Abs(p[1]-p[0]-med) < medianWindowUS {
			pts = append(pts, p)
		}
	}
	if len(pts) < 2 {
		return
	}
	for pass := 0; pass < 2; pass++ {
		n := float64(len(pts))
		var mx, my float64
		for _, p := range pts {
			mx += p[0]
			my += p[1]
		}
		mx /= n
		my /= n
		var sxx, sxy float64
		for _, p := range pts {
			sxx += (p[0] - mx) * (p[0] - mx)
			sxy += (p[0] - mx) * (p[1] - my)
		}
		a := 1.0 // with less than 0.2 s of anchors the slope is noise: offset only
		if sxx > n*1e10 {
			a = sxy / sxx
		}
		b := my - a*mx
		res := make([]float64, len(pts))
		var ss float64
		for i, p := range pts {
			res[i] = p[1] - (a*p[0] + b)
			ss += res[i] * res[i]
		}
		rms := math.Sqrt(ss / n)
		c.a, c.b, c.rms, c.used = a, b, rms, len(pts)
		lim := math.Max(50, 3*rms)
		keep := pts[:0:0]
		for i, p := range pts {
			if math.Abs(res[i]) <= lim {
				keep = append(keep, p)
			}
		}
		if len(keep) == len(pts) || len(keep) < 2 {
			break
		}
		pts = keep
	}
	c.locked = c.used >= lockMin && c.rms < lockRMSUS
}

// Fine maps a device time to the reference clock.
func (c *Clock) Fine(ts float64) float64 { return c.a*ts + c.b }

// PPM is this dongle's clock rate error vs the reference (+ = runs fast).
func (c *Clock) PPM() float64 { return (1/c.a - 1) * 1e6 }

// Locked reports whether the fine model is usable.
func (c *Clock) Locked() bool { return c.locked }

// TolUS is the time tolerance for matching copies from this dongle.
func (c *Clock) TolUS() float64 {
	if c.locked {
		return math.Min(maxFineTolUS, math.Max(refTolUS, 4*c.rms+10))
	}
	return unlockedTolUS
}

// Copy is one dongle's reception of a frame.
type Copy struct {
	Member int
	RSSI   int8
	TsUS   uint64
	Bad    bool // fcs_err copy that only adds its RSSI
}

type times struct {
	rough   float64
	fine    float64
	hasFine bool
}

// Entry is one air frame: the first copy's bytes and metadata, plus every copy seen.
type Entry struct {
	Meta    wire.PktMeta // the first copy's metadata
	Data    []byte       // the first copy's bytes (nil after the window closed)
	GroupTs float64      // reference-clock time (µs)
	Copies  []Copy
	First   int    // member that delivered the first copy
	Mask    uint32 // members that delivered a copy
	Tag     any    // caller's handle (e.g. index in a packet store), set with Bind

	key    uint64
	t      times
	host   int64
	anchor bool
}

// Members returns how many dongles delivered this frame.
func (e *Entry) Members() int { return bits.OnesCount32(e.Mask) }

// Held is a bad-FCS copy that no good copy claimed within HoldTime.
type Held struct {
	Member  int
	Meta    wire.PktMeta
	Data    []byte
	GroupTs float64

	t    times
	host int64
}

// Result of Offer.
type Result struct {
	Dup     bool   // a later copy of Entry (only its RSSI was added)
	Held    bool   // a bad copy waiting for a good one (see Due)
	Entry   *Entry // the frame (nil when Held)
	GroupTs float64
}

// MemberStats are one dongle's counters.
type MemberStats struct {
	RX     uint64 `json:"rx"`     // copies received
	First  uint64 `json:"first"`  // copies that were the first of their frame
	Unique uint64 `json:"unique"` // frames only this dongle delivered
	Copies uint64 `json:"copies"` // later copies
	Bad    uint64 `json:"bad"`    // fcs_err copies
}

// Stats are the group counters.
type Stats struct {
	Out     uint64 `json:"out"`     // frames whose window closed
	Dups    uint64 `json:"dups"`    // extra copies merged away
	Unique  uint64 `json:"unique"`  // frames delivered by one dongle only
	Rescued uint64 `json:"rescued"` // bad copies replaced by a good one
	BadOut  uint64 `json:"bad_out"` // bad copies emitted as they are
}

type member struct {
	mac   string
	clock *Clock
	st    MemberStats
}

// Options for NewMerger.
type Options struct {
	Window  time.Duration // copy window (default 100 ms)
	OnFinal func(*Entry)  // called when a frame's copy window closes (archive, stats)
}

// Merger merges copies from up to MaxMembers dongles.
type Merger struct {
	window  int64
	onFinal func(*Entry)
	members map[int]*member
	ref     int
	hasRef  bool
	table   map[uint64][]*Entry
	byLen   map[uint32][]*Entry
	held    []*Held
	queue   []*Entry
	st      Stats
}

// NewMerger returns an empty merger; call SetMembers before Offer.
func NewMerger(o Options) *Merger {
	if o.Window <= 0 {
		o.Window = DefaultWindow
	}
	return &Merger{window: o.Window.Microseconds(), onFinal: o.OnFinal, members: map[int]*member{},
		table: map[uint64][]*Entry{}, byLen: map[uint32][]*Entry{}}
}

// Member identifies one dongle: ID is the caller's index (0..MaxMembers-1), MAC its HELLO MAC.
type Member struct {
	ID  int
	MAC string // "aa:bb:cc:dd:ee:ff"; the lowest is the clock reference
}

// SetMembers sets the active members. Members not listed are dropped; when the reference
// changes, every fine clock model restarts.
func (m *Merger) SetMembers(list []Member) error {
	ids := map[int]bool{}
	for _, x := range list {
		if x.ID < 0 || x.ID >= MaxMembers {
			return fmt.Errorf("diversity: member id %d out of range", x.ID)
		}
		ids[x.ID] = true
	}
	for id := range m.members {
		if !ids[id] {
			delete(m.members, id)
		}
	}
	for _, x := range list {
		if mm, ok := m.members[x.ID]; ok {
			mm.mac = x.MAC
		} else {
			m.members[x.ID] = &member{mac: x.MAC, clock: NewClock()}
		}
	}
	ref, has := 0, false
	for id, mm := range m.members {
		if !has || mm.mac < m.members[ref].mac || (mm.mac == m.members[ref].mac && id < ref) {
			ref, has = id, true
		}
	}
	if has != m.hasRef || ref != m.ref {
		m.ref, m.hasRef = ref, has
		for _, mm := range m.members {
			mm.clock.Reset()
		}
	}
	return nil
}

// Size is the number of members.
func (m *Merger) Size() int { return len(m.members) }

// Ref returns the reference member (ok = false with no members).
func (m *Merger) Ref() (int, bool) { return m.ref, m.hasRef }

func (m *Merger) times(id int, ts float64) times {
	mm := m.members[id]
	t := times{rough: ts + mm.clock.env}
	if id == m.ref {
		t.fine, t.hasFine = ts, true
	} else if mm.clock.locked {
		t.fine, t.hasFine = mm.clock.Fine(ts), true
	}
	return t
}

func (m *Merger) groupTs(t times) float64 {
	if t.hasFine {
		return t.fine
	}
	if r := m.members[m.ref]; r != nil && r.clock.hasEnv {
		return t.rough - r.clock.env
	}
	return t.rough
}

func (m *Merger) tol(id int) float64 {
	if id == m.ref {
		return refTolUS
	}
	if mm := m.members[id]; mm != nil {
		return mm.clock.TolUS()
	}
	return unlockedTolUS // a member may have left
}

func (m *Merger) near(id int, t times, e *Entry) bool {
	if t.hasFine && e.t.hasFine {
		return math.Abs(t.fine-e.t.fine) <= math.Max(m.tol(id), m.tol(e.First))
	}
	return math.Abs(t.rough-e.t.rough) <= unlockedTolUS
}

func lenKey(ch uint8, origLen uint16) uint32 { return uint32(ch)<<16 | uint32(origLen) }

func isAnchor(meta wire.PktMeta, data []byte) bool {
	return meta.Flags&wire.PktFCSErr == 0 && len(data) >= 24 && (data[0]>>2)&3 != 1
}

// Offer passes one received copy (hostUS: host receive time in µs, monotonic). For the
// first copy of a frame the caller emits it now (Result.Dup == false, Result.Held == false).
// data is copied when kept.
func (m *Merger) Offer(id int, meta wire.PktMeta, data []byte, hostUS int64) (Result, error) {
	mem := m.members[id]
	if mem == nil {
		return Result{}, fmt.Errorf("diversity: unknown member %d", id)
	}
	m.Expire(hostUS)
	mem.st.RX++
	mem.clock.Observe(float64(meta.TsUS), float64(hostUS))
	t := m.times(id, float64(meta.TsUS))
	if meta.Flags&wire.PktFCSErr != 0 { // only ever an extra copy
		mem.st.Bad++
		if e := m.similar(id, meta, data, t); e != nil {
			m.attach(e, id, meta, true)
			m.st.Rescued++
			return Result{Dup: true, Entry: e}, nil
		}
		m.held = append(m.held, &Held{Member: id, Meta: meta, Data: append([]byte(nil), data...), GroupTs: m.groupTs(t), t: t, host: hostUS})
		return Result{Held: true}, nil
	}
	key := uint64(crc32.ChecksumIEEE(data))<<12 | uint64(meta.OrigLen&0xfff)
	bit := uint32(1) << id
	for _, e := range m.table[key] {
		if e.Mask&bit != 0 || e.Meta.Channel != meta.Channel || len(e.Data) != len(data) || !m.near(id, t, e) {
			continue
		}
		m.attach(e, id, meta, false)
		if e.anchor {
			m.addAnchors(e, id, float64(meta.TsUS))
		}
		return Result{Dup: true, Entry: e}, nil
	}
	g := m.groupTs(t)
	e := &Entry{Meta: meta, Data: append([]byte(nil), data...), GroupTs: g, First: id, Mask: bit,
		Copies: []Copy{{Member: id, RSSI: meta.RSSI, TsUS: meta.TsUS}}, key: key, t: t, host: hostUS, anchor: isAnchor(meta, data)}
	e.Meta.CapLen = uint16(len(data))
	m.table[key] = append(m.table[key], e)
	lk := lenKey(meta.Channel, meta.OrigLen)
	m.byLen[lk] = append(m.byLen[lk], e)
	m.queue = append(m.queue, e)
	mem.st.First++
	// bad copies of this frame that arrived first now only add their RSSI
	for i := 0; i < len(m.held); i++ {
		h := m.held[i]
		if e.Mask&(1<<h.Member) == 0 && m.sameFrame(e, h.Member, h.Meta, h.Data, h.t) {
			m.attach(e, h.Member, h.Meta, true)
			m.st.Rescued++
			m.held = append(m.held[:i], m.held[i+1:]...)
			i--
		}
	}
	return Result{Entry: e, GroupTs: g}, nil
}

// Bind stores the caller's handle for an entry (returned again via OnFinal).
func (m *Merger) Bind(e *Entry, tag any) { e.Tag = tag }

func (m *Merger) attach(e *Entry, id int, meta wire.PktMeta, bad bool) {
	e.Mask |= 1 << id
	e.Copies = append(e.Copies, Copy{Member: id, RSSI: meta.RSSI, TsUS: meta.TsUS, Bad: bad})
	if mm := m.members[id]; mm != nil {
		mm.st.Copies++
	}
}

func bitDiff(a, b []byte, max int) int {
	d := 0
	for i := range a {
		if x := a[i] ^ b[i]; x != 0 {
			if d += bits.OnesCount8(x); d > max {
				return d
			}
		}
	}
	return d
}

func (m *Merger) sameFrame(e *Entry, id int, meta wire.PktMeta, data []byte, t times) bool {
	if e.Meta.Channel != meta.Channel || e.Meta.OrigLen != meta.OrigLen || len(e.Data) != len(data) || !m.near(id, t, e) {
		return false
	}
	return bitDiff(e.Data, data, maxBitDiff) <= maxBitDiff
}

func (m *Merger) similar(id int, meta wire.PktMeta, data []byte, t times) *Entry {
	for _, e := range m.byLen[lenKey(meta.Channel, meta.OrigLen)] {
		if e.Mask&(1<<id) == 0 && m.sameFrame(e, id, meta, data, t) {
			return e
		}
	}
	return nil
}

// Due returns the bad copies whose wait is over and that no good copy claimed: emit them
// as they are.
func (m *Merger) Due(hostUS int64) []*Held {
	var out []*Held
	hold := HoldTime.Microseconds()
	for len(m.held) > 0 && hostUS-m.held[0].host >= hold {
		out = append(out, m.held[0])
		m.held = m.held[1:]
		m.st.BadOut++
	}
	return out
}

func (m *Merger) addAnchors(e *Entry, id int, ts float64) {
	if id == m.ref {
		for _, c := range e.Copies {
			if c.Member != id {
				if mm := m.members[c.Member]; mm != nil {
					mm.clock.Anchor(float64(c.TsUS), ts)
				}
			}
		}
		return
	}
	for _, c := range e.Copies {
		if c.Member == m.ref {
			m.members[id].clock.Anchor(ts, float64(c.TsUS))
			return
		}
	}
}

func remove(list []*Entry, e *Entry) []*Entry {
	for i, x := range list {
		if x == e {
			return append(list[:i], list[i+1:]...)
		}
	}
	return list
}

// Expire closes the copy window of frames older than the window (host time).
func (m *Merger) Expire(hostUS int64) {
	n := 0
	for n < len(m.queue) && hostUS-m.queue[n].host > m.window {
		e := m.queue[n]
		n++
		if l := remove(m.table[e.key], e); len(l) > 0 {
			m.table[e.key] = l
		} else {
			delete(m.table, e.key)
		}
		lk := lenKey(e.Meta.Channel, e.Meta.OrigLen)
		if l := remove(m.byLen[lk], e); len(l) > 0 {
			m.byLen[lk] = l
		} else {
			delete(m.byLen, lk)
		}
		k := e.Members()
		m.st.Out++
		m.st.Dups += uint64(k - 1)
		if k == 1 && len(m.members) > 1 {
			m.st.Unique++
			if mm := m.members[e.First]; mm != nil {
				mm.st.Unique++
			}
		}
		if m.onFinal != nil {
			m.onFinal(e)
		}
		e.Data = nil
	}
	if n > 0 {
		m.queue = append(m.queue[:0], m.queue[n:]...)
	}
}

// Flush closes every open window (stop, member change).
func (m *Merger) Flush() { m.Expire(math.MaxInt64) }

// ClockStats describe one member's clock model.
type ClockStats struct {
	Ref      bool     `json:"ref,omitempty"`
	Locked   bool     `json:"locked"`
	PPM      *float64 `json:"ppm,omitempty"`
	RMSUS    *float64 `json:"rms_us,omitempty"`
	Anchors  int      `json:"anchors"`
	OffsetUS *float64 `json:"offset_us,omitempty"`
	TolUS    float64  `json:"tol_us"`
}

// MemberSummary is one member in Summary.
type MemberSummary struct {
	ID  int    `json:"id"`
	MAC string `json:"mac"`
	MemberStats
	Ref   bool       `json:"ref"`
	Clock ClockStats `json:"clock"`
}

// Summary is the merger state for statistics.
type Summary struct {
	Ref int `json:"ref"`
	Stats
	Open    int             `json:"open"`
	Held    int             `json:"held"`
	Members []MemberSummary `json:"members"`
}

// Summary returns the group and per-member counters and clock state, members sorted by id.
func (m *Merger) Summary() Summary {
	s := Summary{Ref: m.ref, Stats: m.st, Open: len(m.queue), Held: len(m.held)}
	if !m.hasRef {
		s.Ref = -1
	}
	ids := make([]int, 0, len(m.members))
	for id := range m.members {
		ids = append(ids, id)
	}
	sort.Ints(ids)
	for _, id := range ids {
		mm := m.members[id]
		ms := MemberSummary{ID: id, MAC: mm.mac, MemberStats: mm.st, Ref: id == m.ref}
		if ms.Ref {
			ms.Clock = ClockStats{Ref: true, Locked: true, TolUS: refTolUS}
		} else {
			c := mm.clock
			ms.Clock = ClockStats{Locked: c.locked, Anchors: c.used, TolUS: c.TolUS()}
			if c.used >= lockMin {
				p := c.PPM()
				ms.Clock.PPM = &p
			}
			if !math.IsInf(c.rms, 0) {
				r := c.rms
				ms.Clock.RMSUS = &r
			}
			if c.locked {
				last := 0.0
				if len(c.anchors) > 0 {
					last = c.anchors[len(c.anchors)-1][0]
				}
				o := c.b + (c.a-1)*last
				ms.Clock.OffsetUS = &o
			}
		}
		s.Members = append(s.Members, ms)
	}
	return s
}
