// Package openhd is a passive OpenHD receiver (docs/13), the Go port of js/openhd.js,
// js/ohdcrypto.js, js/ohdkeys.js and js/ohdfec.js: 802.11 fingerprint → session key →
// auth/decrypt → Gen A/B framing → FEC (video) / dedupe (telemetry) / management.
// Receive-only. Air units are told apart by their session key (random per boot); several
// OpenHD systems on one channel work as long as their bind phrase is known.
//
// Also: Sender (simulators and tests), the capture filter preset and channel helpers.
// RTP depacketizing and MP4 recording live in package video, MAVLink in package mavlink.
package openhd

import (
	"bytes"
	"encoding/hex"
	"math"
	"sort"
	"time"

	"github.com/hjkoskel/capturetonkeli/ctp/fec"
	"github.com/hjkoskel/capturetonkeli/ctp/filter"
)

// Unit ids (addr2[0]) and radio ports.
const (
	UIDAir = 1
	UIDGnd = 2

	PortTeleGnd = 3
	PortTeleAir = 4
	PortVideo0  = 10
	PortVideo1  = 11
	PortMgmtAir = 20
	PortMgmtGnd = 21
	PortAudio   = 30
	PortSession = 127
)

func isVideo(p uint8) bool { return p == PortVideo0 || p == PortVideo1 }

func expectedType(p uint8) (uint8, bool) {
	switch p {
	case PortVideo0, PortVideo1:
		return 0, true
	case PortTeleAir, PortTeleGnd:
		return 1, true
	}
	return 0, false
}

func noFraming(p uint8) bool { return p == PortMgmtAir || p == PortMgmtGnd || p == PortSession }

// RaceBandMHz are OpenHD's 5.8 GHz race band channels and the 2.4 GHz default (docs/13 §3).
var RaceBandMHz = []int{5700, 5745, 5785, 5825, 5865, 5260, 5280}

// DefaultFreq is OpenHD's default frequency (MHz).
const DefaultFreq = 5745

// FreqToChannel converts MHz to a Wi-Fi channel (0 = not a channel centre).
func FreqToChannel(mhz int) int {
	switch {
	case mhz == 2484:
		return 14
	case mhz >= 2412 && mhz <= 2472 && (mhz-2407)%5 == 0:
		return (mhz - 2407) / 5
	case mhz >= 5160 && mhz <= 5885 && (mhz-5000)%5 == 0:
		return (mhz - 5000) / 5
	}
	return 0
}

// HT40Plus is the HT40 side OpenHD uses on a channel (true = HT40+).
func HT40Plus(ch int) bool {
	switch {
	case ch <= 14:
		return ch == 1 || ch == 9
	case ch >= 149:
		return ((ch-149)/4)%2 == 0
	}
	return ((ch-36)/4)%2 == 0
}

var portNames = map[uint8]string{3: "telemetry", 4: "telemetry", 10: "video", 11: "video 2", 20: "management", 21: "management", 30: "audio", 127: "session key"}

// PortName names a radio port.
func PortName(p uint8) string {
	if n, ok := portNames[p]; ok {
		return n
	}
	return "port " + itoa(int(p))
}

func itoa(v int) string { return string(appendInt(nil, v)) }

func appendInt(b []byte, v int) []byte {
	if v < 0 {
		b = append(b, '-')
		v = -v
	}
	var d [20]byte
	i := len(d)
	for {
		i--
		d[i] = byte('0' + v%10)
		v /= 10
		if v == 0 {
			break
		}
	}
	return append(b, d[i:]...)
}

// Header is the OpenHD part of the 802.11 header (§4.1).
type Header struct {
	UID     uint8
	Enc     bool
	Port    uint8
	Nonce   uint64
	Payload []byte
}

// ParseFrame checks the 802.11 fingerprint (§4.1) of f (frame without FCS).
func ParseFrame(f []byte) (Header, bool) {
	if len(f) < 25 || f[0] != 0x08 || f[1] != 0x01 {
		return Header{}, false
	}
	for i := 4; i < 10; i++ {
		if f[i] != 0xff {
			return Header{}, false
		}
	}
	if f[10] != f[16] || f[15] != f[21] {
		return Header{}, false
	}
	n := uint64(le.Uint32(f[11:])) | uint64(le.Uint32(f[17:]))<<32
	return Header{UID: f[10], Enc: f[15]&1 != 0, Port: f[15] >> 1, Nonce: n, Payload: f[24:]}, true
}

// LooksGenB is the Gen B WBPacketHeader heuristic (§2).
func LooksGenB(port uint8, body []byte) bool {
	t, ok := expectedType(port)
	return ok && len(body) >= 8 && (body[0] == 0 || body[0] == 1) && body[1] == t && int(le.Uint16(body[6:])) == len(body)
}

// ---------------------------------------------------------------------------------------------

// FragInfo describes a forwarded video packet.
type FragInfo struct {
	Block     uint32
	Recovered bool // rebuilt by FEC
	Gap       bool // packets before this one were lost
}

// FecStats are one video stream's counters.
type FecStats struct {
	Fragments     uint64 `json:"fragments"`
	Primaries     uint64 `json:"primaries"`
	Secondaries   uint64 `json:"secondaries"`
	SecUnneeded   uint64 `json:"sec_unneeded"`
	Duplicates    uint64 `json:"duplicates"`
	Late          uint64 `json:"late"`
	Bad           uint64 `json:"bad"`
	Recovered     uint64 `json:"recovered"`
	BlocksOK      uint64 `json:"blocks_ok"`
	BlocksFEC     uint64 `json:"blocks_fec"`
	BlocksLost    uint64 `json:"blocks_lost"`
	PrimariesLost uint64 `json:"primaries_lost"`
	Restarts      uint64 `json:"restarts"`
}

type fecBlock struct {
	idx       uint32
	k         int
	prim      map[int][]byte
	recovered map[int]bool
	sec       map[int][]byte
	fwd       int
	fec       bool
	t0        time.Time
}

// FecStream is one FEC video stream (§5.4). Primaries are forwarded in order, only from the
// oldest open block, as soon as they are available (no wait on a clean link). Queue = max
// open blocks (OpenHD ground default 1). Hold > 0 (several dongles, docs/14): an incomplete
// block is only given up once it is that old, because another dongle's copy of its missing
// packet can arrive after newer blocks.
type FecStream struct {
	Queue         int
	ForwardGapped bool
	Hold          time.Duration
	St            FecStats

	out        func(pkt []byte, info FragInfo)
	now        func() time.Time
	blocks     map[uint32]*fecBlock
	lastClosed int64
	gap        bool
}

// NewFecStream returns a stream that calls out for each RTP packet in order.
func NewFecStream(out func([]byte, FragInfo), queue int, now func() time.Time) *FecStream {
	if now == nil {
		now = time.Now
	}
	if queue <= 0 {
		queue = 1
	}
	s := &FecStream{Queue: queue, out: out, now: now}
	s.reset()
	return s
}

func (s *FecStream) reset() { s.blocks = map[uint32]*fecBlock{}; s.lastClosed = -1; s.gap = false }

func (s *FecStream) oldest() *fecBlock {
	var o *fecBlock
	for _, b := range s.blocks {
		if o == nil || b.idx < o.idx {
			o = b
		}
	}
	return o
}

// Push takes one FEC fragment (the stream payload after Gen B framing).
func (s *FecStream) Push(body []byte) {
	st := &s.St
	if len(body) < 8 {
		st.Bad++
		return
	}
	blk, fi, k := le.Uint32(body), int(body[4]), int(body[5])
	if k == 0 || k > 128 || fi == 255 {
		st.Bad++
		return
	}
	st.Fragments++
	if fi < k {
		st.Primaries++
	} else {
		st.Secondaries++
	}
	if int64(blk) <= s.lastClosed {
		if s.lastClosed-int64(blk) > 1000 { // sender restarted without a new session
			st.Restarts++
			s.reset()
		} else {
			if fi >= k {
				st.SecUnneeded++
			} else {
				st.Late++
			}
			return
		}
	}
	b := s.blocks[blk]
	if b == nil {
		b = &fecBlock{idx: blk, k: k, prim: map[int][]byte{}, recovered: map[int]bool{}, sec: map[int][]byte{}}
		if s.Hold > 0 {
			b.t0 = s.now()
		}
		s.blocks[blk] = b
	}
	if b.k != k {
		st.Bad++
		return
	}
	sym := body[6:]
	if fi < k {
		if _, dup := b.prim[fi]; dup {
			st.Duplicates++
			return
		}
		if int(le.Uint16(body[6:]))+2 > len(sym) {
			st.Bad++
			return
		}
		b.prim[fi] = sym
	} else {
		if _, dup := b.sec[fi-k]; dup {
			st.Duplicates++
			return
		}
		if len(b.prim) == k {
			st.SecUnneeded++
			return
		}
		b.sec[fi-k] = sym
	}
	if len(b.prim) < k && len(b.prim)+len(b.sec) >= k {
		s.recover(b)
	}
	if len(b.prim) == k { // a newer block is complete: older ones that are ripe are given up
		var older []*fecBlock
		for _, x := range s.blocks {
			if x.idx < b.idx {
				older = append(older, x)
			}
		}
		sort.Slice(older, func(i, j int) bool { return older[i].idx < older[j].idx })
		for _, o := range older {
			if s.ripe(o) {
				s.giveUp(o)
			}
		}
	}
	s.drain()
	s.trim()
}

func (s *FecStream) ripe(b *fecBlock) bool { return s.Hold <= 0 || s.now().Sub(b.t0) >= s.Hold }

func (s *FecStream) trim() {
	for len(s.blocks) > s.Queue {
		o := s.oldest()
		if !s.ripe(o) && len(s.blocks) <= 64 {
			return
		}
		s.giveUp(o)
		s.drain()
	}
}

func (s *FecStream) emit(b *fecBlock, j int) {
	sym := b.prim[j]
	s.out(sym[2:2+int(le.Uint16(sym))], FragInfo{Block: b.idx, Recovered: b.recovered[j], Gap: s.gap})
	s.gap = false
}

// drain forwards what the oldest blocks have in order and finishes complete ones.
func (s *FecStream) drain() {
	for o := s.oldest(); o != nil; o = s.oldest() {
		for o.fwd < o.k {
			if _, ok := o.prim[o.fwd]; !ok {
				break
			}
			s.emit(o, o.fwd)
			o.fwd++
		}
		if o.fwd < o.k {
			return
		}
		if o.fec {
			s.St.BlocksFEC++
		} else {
			s.St.BlocksOK++
		}
		delete(s.blocks, o.idx)
		s.lastClosed = max(s.lastClosed, int64(o.idx))
	}
}

func (s *FecStream) recover(b *fecBlock) {
	L := 0
	for _, x := range b.sec {
		L = max(L, len(x))
	}
	rec, err := fec.CauchyDecode(b.k, b.prim, b.sec, L)
	if err != nil {
		s.St.Bad++
		return
	}
	for _, x := range rec {
		if int(le.Uint16(x))+2 > L {
			s.St.Bad++
			return
		}
	}
	for j, x := range rec {
		b.prim[j] = x
		b.recovered[j] = true
		s.St.Recovered++
	}
	b.fec = true
}

func (s *FecStream) giveUp(b *fecBlock) {
	// what is in order was forwarded already (b was not the oldest, or drain stopped at a gap)
	if b == s.oldest() {
		for b.fwd < b.k {
			if _, ok := b.prim[b.fwd]; !ok {
				break
			}
			s.emit(b, b.fwd)
			b.fwd++
		}
	}
	delete(s.blocks, b.idx)
	s.lastClosed = max(s.lastClosed, int64(b.idx))
	if len(b.prim) == b.k && b.fwd == b.k {
		if b.fec {
			s.St.BlocksFEC++
		} else {
			s.St.BlocksOK++
		}
		return
	}
	s.St.BlocksLost++
	s.St.PrimariesLost += uint64(b.k - len(b.prim))
	s.gap = true
	if s.ForwardGapped {
		for j := b.fwd; j < b.k; j++ {
			if _, ok := b.prim[j]; ok {
				s.emit(b, j)
			} else {
				s.gap = true
			}
		}
	}
}

// dedupe drops repeated FECDisabled sequence numbers (§5.1), remembering the last 100.
type dedupe struct {
	set  map[uint64]bool
	ring []uint64
}

func (d *dedupe) seen(seq uint64) bool {
	if d.set[seq] {
		return true
	}
	d.set[seq] = true
	d.ring = append(d.ring, seq)
	if len(d.ring) > 100 {
		delete(d.set, d.ring[0])
		d.ring = d.ring[1:]
	}
	return false
}

// ---------------------------------------------------------------------------------------------

// Mgmt is the air unit's management announcement (frequency in MHz, width in MHz).
type Mgmt struct {
	Freq uint32 `json:"freq"`
	BW   uint8  `json:"bw"`
}

// UnitStats are one OpenHD unit's counters.
type UnitStats struct {
	Packets       uint64           `json:"packets"`
	Bytes         uint64           `json:"bytes"`
	NonceGaps     uint64           `json:"nonce_gaps"`
	NonceSpan     uint64           `json:"nonce_span"`
	LateFilled    uint64           `json:"late_filled"`
	Video         [2]uint64        `json:"video"`
	VideoBytes    [2]uint64        `json:"video_bytes"`
	Telemetry     uint64           `json:"telemetry"`
	TeleDup       uint64           `json:"tele_dup"`
	Mgmt          uint64           `json:"mgmt"`
	Audio         uint64           `json:"audio"`
	Retransmitted uint64           `json:"retransmitted"`
	ARQRequests   uint64           `json:"arq_requests"`
	Restarts      uint64           `json:"restarts"`
	AuthFail      uint64           `json:"auth_fail"`
	Ports         map[uint8]uint64 `json:"ports"`
}

// Unit is one OpenHD transmitter (air or ground unit) identified by its session key.
type Unit struct {
	ID       int
	Dir      uint8 // UIDAir or UIDGnd
	Verified bool  // false: keyless video shown without authentication (Options.Unverified)
	Gen      byte  // 'A', 'B' or 0 (not detected yet)
	Created  time.Time
	Last     time.Time
	RSSI     *int // smoothed dBm
	Channel  uint8
	Mgmt     *Mgmt
	St       UnitStats

	key       []byte
	sessPkt   []byte
	nonceLast uint64
	hasNonce  bool
	genMiss   int
	fec       map[uint8]*FecStream
	dedupe    map[uint8]*dedupe
	seen      map[uint32]bool
	seenOrder []uint32
	prevSpan  uint64
	prevGaps  uint64
	hasPrev   bool
}

func (u *Unit) resetStreams() {
	u.fec = map[uint8]*FecStream{}
	u.dedupe = map[uint8]*dedupe{}
	u.Gen, u.genMiss, u.hasNonce = 0, 0, false
	u.seen, u.seenOrder = nil, nil
}

// RxMeta is the dongle metadata of a frame (optional).
type RxMeta struct {
	RSSI    int8
	HasRSSI bool
	Channel uint8
}

// GlobalStats count frames before they are attributed to a unit.
type GlobalStats struct {
	Frames         uint64            `json:"frames"`
	Foreign        uint64            `json:"foreign"`
	OhdFrames      [3]uint64         `json:"ohd_frames"`      // by dir (1 air, 2 ground)
	SessionPackets [3]uint64         `json:"session_packets"` // by dir
	BadSession     [3]uint64         `json:"bad_session"`     // by dir
	NoSession      uint64            `json:"no_session"`
	AuthFail       uint64            `json:"auth_fail"`
	Unverified     uint64            `json:"unverified"`
	Ports          map[string]uint64 `json:"ports"` // "dir:port"
	LastOhd        time.Time         `json:"last_ohd"`
}

// Options for NewReceiver. Callbacks get the unit; pkt/data slices are only valid during
// the call.
type Options struct {
	Keys          Keys                                              // zero value: DefaultKeys
	OnRTP         func(u *Unit, cam int, pkt []byte, info FragInfo) // cam 0/1
	OnTelemetry   func(u *Unit, data []byte, dir uint8)             // aggregated MAVLink frames
	OnMgmt        func(u *Unit, m Mgmt)
	OnSession     func(u *Unit, kind string) // "new" | "restart"
	Now           func() time.Time
	Queue         int           // FEC blocks kept open (default 1, OpenHD's ground default)
	ForwardGapped bool          // forward primaries after a lost block's gap too
	Unverified    bool          // show keyless video (auth-only) without verifying it
	Hold          time.Duration // see FecStream.Hold (set with several dongles, docs/14 §9.3)
}

// Receiver decodes OpenHD frames. Not safe for concurrent use.
type Receiver struct {
	o      Options
	k      [3][]byte
	units  []*Unit
	nextID int
	G      GlobalStats
}

// NewReceiver returns a receiver; Options.Keys zero = the default bind phrase.
func NewReceiver(o Options) *Receiver {
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Queue <= 0 {
		o.Queue = 1
	}
	if o.Keys.Air == nil && o.Keys.Gnd == nil {
		o.Keys = DefaultKeys()
	}
	r := &Receiver{o: o, nextID: 1}
	r.SetKeys(o.Keys)
	return r
}

// SetKeys replaces the keys (and forgets every unit).
func (r *Receiver) SetKeys(k Keys) {
	r.o.Keys = k
	r.k = [3][]byte{nil, k.Air, k.Gnd}
	r.Reset()
}

// SetStreamOptions changes the FEC options of every stream.
func (r *Receiver) SetStreamOptions(queue int, forwardGapped bool, hold time.Duration) {
	if queue <= 0 {
		queue = 1
	}
	r.o.Queue, r.o.ForwardGapped, r.o.Hold = queue, forwardGapped, hold
	for _, u := range r.units {
		for _, s := range u.fec {
			s.Queue, s.ForwardGapped, s.Hold = queue, forwardGapped, hold
		}
	}
}

// Reset forgets every unit and counter.
func (r *Receiver) Reset() {
	r.units = nil
	r.G = GlobalStats{Ports: map[string]uint64{}}
}

func (r *Receiver) newUnit(dir uint8, key, sess []byte) *Unit {
	now := r.o.Now()
	u := &Unit{ID: r.nextID, Dir: dir, key: key, sessPkt: sess, Created: now, Last: now, Verified: key != nil,
		St: UnitStats{Ports: map[uint8]uint64{}}}
	u.resetStreams()
	r.nextID++
	r.units = append(r.units, u)
	return u
}

func (r *Receiver) deleteUnit(u *Unit) {
	for i, x := range r.units {
		if x == u {
			r.units = append(r.units[:i], r.units[i+1:]...)
			return
		}
	}
}

func dist(u *Unit, n uint64) uint64 {
	if !u.hasNonce {
		return math.MaxUint64
	}
	d := int64(n - u.nonceLast)
	if d < 0 {
		d = -d
	}
	return uint64(d)
}

// Push decodes one captured 802.11 frame (without FCS). It reports whether it was an
// OpenHD frame.
func (r *Receiver) Push(f []byte, m RxMeta) bool {
	r.G.Frames++
	h, ok := ParseFrame(f)
	if !ok || (h.UID != UIDAir && h.UID != UIDGnd) {
		r.G.Foreign++
		return false
	}
	dir, now := h.UID, r.o.Now()
	r.G.OhdFrames[dir]++
	r.G.LastOhd = now
	r.G.Ports[itoa(int(dir))+":"+itoa(int(h.Port))]++
	if h.Port == PortSession {
		r.session(dir, h, m, now)
		return true
	}
	// find the unit whose session key verifies this packet (closest nonce first)
	var cands []*Unit
	for _, u := range r.units {
		if u.Dir == dir && u.key != nil {
			cands = append(cands, u)
		}
	}
	if len(cands) > 1 {
		sort.SliceStable(cands, func(i, j int) bool { return dist(cands[i], h.Nonce) < dist(cands[j], h.Nonce) })
	}
	var unit *Unit
	var body []byte
	for _, u := range cands {
		var ok bool
		if h.Enc {
			body, ok = AEADOpen(u.key, h.Nonce, h.Payload)
		} else {
			body, ok = AuthOpen(u.key, h.Nonce, h.Payload)
		}
		if ok {
			unit = u
			break
		}
	}
	if unit == nil {
		if len(cands) > 0 {
			r.G.AuthFail++
			return true
		}
		if r.o.Unverified && !h.Enc && isVideo(h.Port) && len(h.Payload) > 16 {
			for _, u := range r.units {
				if u.Dir == dir && u.key == nil {
					unit = u
					break
				}
			}
			if unit == nil {
				unit = r.newUnit(dir, nil, nil)
			}
			body = h.Payload[:len(h.Payload)-16]
			r.G.Unverified++
		} else {
			r.G.NoSession++
			return true
		}
	}
	r.track(unit, h, len(f), m, now)
	r.payload(unit, h, body)
	return true
}

func (r *Receiver) session(dir uint8, h Header, m RxMeta, now time.Time) {
	r.G.SessionPackets[dir]++
	p := h.Payload
	if h.Enc || len(p) != 72 {
		r.G.BadSession[dir]++
		return
	}
	for _, u := range r.units {
		if u.Dir == dir && u.sessPkt != nil && bytes.Equal(u.sessPkt, p) {
			u.Last = now
			trackRF(u, m)
			return
		}
	}
	if r.k[dir] == nil {
		return
	}
	key, ok := SecretboxOpen(p[24:], p[:24], r.k[dir])
	if !ok {
		r.G.BadSession[dir]++
		return
	}
	for _, u := range r.units {
		if u.Dir == dir && u.key != nil && bytes.Equal(u.key, key) {
			u.sessPkt = append([]byte(nil), p...)
			u.Last = now
			return
		}
	}
	// a new session: the same unit restarted (old one silent), or another OpenHD system
	var u *Unit
	for _, x := range r.units {
		if x.Dir == dir && x.key != nil && now.Sub(x.Last) > 500*time.Millisecond && (u == nil || x.Last.After(u.Last)) {
			u = x
		}
	}
	if u != nil {
		u.key, u.sessPkt, u.Last = key, append([]byte(nil), p...), now
		u.St.Restarts++
		u.resetStreams()
		if r.o.OnSession != nil {
			r.o.OnSession(u, "restart")
		}
	} else {
		u = r.newUnit(dir, key, append([]byte(nil), p...))
		if r.o.OnSession != nil {
			r.o.OnSession(u, "new")
		}
	}
	// a verified session replaces the "unverified" pseudo unit of that direction
	for i := 0; i < len(r.units); i++ {
		if x := r.units[i]; x.Dir == dir && x.key == nil {
			r.deleteUnit(x)
			i--
		}
	}
	trackRF(u, m)
}

func (r *Receiver) track(u *Unit, h Header, n int, m RxMeta, now time.Time) {
	st := &u.St
	st.Packets++
	st.Bytes += uint64(n)
	st.Ports[h.Port]++
	// loss = nonces skipped; a late packet (several dongles: another one's copy arrives after
	// newer packets, docs/14) fills its gap again, so it is subtracted
	if u.seen == nil {
		u.seen = map[uint32]bool{}
	}
	n32 := uint32(h.Nonce)
	if u.hasNonce {
		d := int64(h.Nonce - u.nonceLast)
		switch {
		case d > 0 && d < 100000:
			st.NonceSpan += uint64(d)
			if d > 1 {
				st.NonceGaps += uint64(d - 1)
			}
		case d < 0 && d > -4096 && !u.seen[n32] && st.NonceGaps > 0:
			st.NonceGaps--
			st.LateFilled++
		}
	} else {
		st.NonceSpan++
	}
	if !u.seen[n32] {
		u.seen[n32] = true
		u.seenOrder = append(u.seenOrder, n32)
		if len(u.seenOrder) > 4096 {
			delete(u.seen, u.seenOrder[0])
			u.seenOrder = u.seenOrder[1:]
		}
	}
	if !u.hasNonce || h.Nonce > u.nonceLast {
		u.nonceLast, u.hasNonce = h.Nonce, true
	}
	u.Last = now
	trackRF(u, m)
}

func trackRF(u *Unit, m RxMeta) {
	if m.HasRSSI {
		v := int(m.RSSI)
		if u.RSSI != nil {
			v = int(math.Round(0.8*float64(*u.RSSI) + 0.2*float64(m.RSSI)))
		}
		u.RSSI = &v
	}
	if m.Channel != 0 {
		u.Channel = m.Channel
	}
}

func (r *Receiver) payload(u *Unit, h Header, body []byte) {
	port := h.Port
	if port == PortMgmtAir || port == PortMgmtGnd {
		u.St.Mgmt++
		if port == PortMgmtAir && len(body) >= 6 && body[0] == 0 {
			m := Mgmt{Freq: le.Uint32(body[1:]), BW: body[5]}
			u.Mgmt = &m
			if r.o.OnMgmt != nil {
				r.o.OnMgmt(u, m)
			}
		}
		return
	}
	if !noFraming(port) {
		if _, ok := expectedType(port); ok && u.Gen == 0 {
			if LooksGenB(port, body) {
				u.Gen = 'B'
			} else {
				u.Gen = 'A'
			}
		}
		switch u.Gen {
		case 'B':
			if len(body) < 8 || int(le.Uint16(body[6:])) != len(body) {
				if u.genMiss++; u.genMiss >= 5 { // re-detect
					u.Gen, u.genMiss = 0, 0
				}
				return
			}
			u.genMiss = 0
			if body[0]&1 != 0 {
				u.St.Retransmitted++
			}
			if body[1] == 3 {
				u.St.ARQRequests++
				return
			}
			body = body[8:]
		case 0:
			if port != PortAudio {
				return // framing unknown yet (only audio before any video/telemetry)
			}
		}
	}
	if isVideo(port) {
		cam := int(port - PortVideo0)
		s := u.fec[port]
		if s == nil {
			s = NewFecStream(func(pkt []byte, info FragInfo) {
				u.St.Video[cam]++
				u.St.VideoBytes[cam] += uint64(len(pkt))
				if r.o.OnRTP != nil {
					r.o.OnRTP(u, cam, pkt, info)
				}
			}, r.o.Queue, r.o.Now)
			s.ForwardGapped, s.Hold = r.o.ForwardGapped, r.o.Hold
			u.fec[port] = s
		}
		s.Push(body)
		return
	}
	if port == PortTeleAir || port == PortTeleGnd || port == PortAudio {
		if len(body) < 9 {
			return
		}
		seq := le.Uint64(body)
		d := u.dedupe[port]
		if d == nil {
			d = &dedupe{set: map[uint64]bool{}}
			u.dedupe[port] = d
		}
		if d.seen(seq) {
			u.St.TeleDup++
			return
		}
		if port == PortAudio {
			u.St.Audio++
			return
		}
		u.St.Telemetry++
		if r.o.OnTelemetry != nil {
			r.o.OnTelemetry(u, body[8:], u.Dir)
		}
	}
}

// FecSummary is one video stream in UnitSummary.
type FecSummary struct {
	FecStats
	Queue int `json:"queue"`
}

// UnitSummary is a snapshot of one unit.
type UnitSummary struct {
	ID        int                `json:"id"`
	Dir       uint8              `json:"dir"`
	Verified  bool               `json:"verified"`
	Gen       string             `json:"gen,omitempty"`
	Age       time.Duration      `json:"age"`
	Up        time.Duration      `json:"up"`
	RSSI      *int               `json:"rssi,omitempty"`
	Channel   uint8              `json:"channel,omitempty"`
	Mgmt      *Mgmt              `json:"mgmt,omitempty"`
	St        UnitStats          `json:"st"`
	Loss      *float64           `json:"loss,omitempty"` // since the previous Summary
	FEC       map[int]FecSummary `json:"fec"`            // by camera
	SessionID string             `json:"session_id,omitempty"`
}

// Summary is a snapshot of the receiver.
type Summary struct {
	Units []UnitSummary `json:"units"`
	G     GlobalStats   `json:"g"`
	Keys  struct {
		Air bool `json:"air"`
		Gnd bool `json:"gnd"`
	} `json:"keys"`
}

// Summary returns a snapshot; units silent for 15 s are forgotten. Loss is measured since
// the previous call.
func (r *Receiver) Summary() Summary {
	now := r.o.Now()
	var s Summary
	for i := 0; i < len(r.units); i++ {
		u := r.units[i]
		if now.Sub(u.Last) > 15*time.Second {
			r.deleteUnit(u)
			i--
			continue
		}
		st := u.St
		us := UnitSummary{ID: u.ID, Dir: u.Dir, Verified: u.Verified, Age: now.Sub(u.Last), Up: now.Sub(u.Created), RSSI: u.RSSI,
			Channel: u.Channel, Mgmt: u.Mgmt, FEC: map[int]FecSummary{}}
		if u.Gen != 0 {
			us.Gen = string(u.Gen)
		}
		if u.hasPrev && st.NonceSpan > u.prevSpan {
			l := math.Max(0, math.Min(1, (float64(st.NonceGaps)-float64(u.prevGaps))/float64(st.NonceSpan-u.prevSpan)))
			us.Loss = &l
		}
		u.prevSpan, u.prevGaps, u.hasPrev = st.NonceSpan, st.NonceGaps, true
		st.Ports = map[uint8]uint64{}
		for k, v := range u.St.Ports {
			st.Ports[k] = v
		}
		us.St = st
		for port, f := range u.fec {
			us.FEC[int(port-PortVideo0)] = FecSummary{FecStats: f.St, Queue: f.Queue}
		}
		if u.sessPkt != nil {
			us.SessionID = hex.EncodeToString(u.sessPkt[:4])
		}
		s.Units = append(s.Units, us)
	}
	s.G = r.G
	s.G.Ports = map[string]uint64{}
	for k, v := range r.G.Ports {
		s.G.Ports[k] = v
	}
	s.Keys.Air, s.Keys.Gnd = r.k[1] != nil, r.k[2] != nil
	return s
}

// ---------------------------------------------------------------------------------------------

// CaptureFilter is the dongle filter preset (docs/13 §10.1): OpenHD frames from the air unit
// (session, video 10/11 plain or encrypted, telemetry, management, audio), and with ground
// also the ground unit's session/telemetry/management. Same rules as the web UI preset.
func CaptureFilter(ground bool) filter.Set {
	ff := func(n int) []byte { return bytes.Repeat([]byte{0xff}, n) }
	rule := func(name string, uid, port, pmask byte) filter.Rule {
		return filter.Rule{Name: "OpenHD " + name, Enabled: true, Terms: []filter.Term{
			{Offset: 0, Value: []byte{0x08, 0x01}, Mask: ff(2)},
			{Offset: 4, Value: []byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, uid}, Mask: ff(7)},
			{Offset: 15, Value: []byte{port, uid}, Mask: []byte{pmask, 0xff}},
		}}
	}
	rules := []filter.Rule{
		rule("session (air)", 1, 0xfe, 0xff), rule("video (air)", 1, 0x14, 0xfc), rule("telemetry (air)", 1, 0x08, 0xfe),
		rule("management (air)", 1, 0x28, 0xfe), rule("audio (air)", 1, 0x3c, 0xfe),
	}
	if ground {
		rules = append(rules, rule("session (ground)", 2, 0xfe, 0xff), rule("telemetry (ground)", 2, 0x06, 0xfe), rule("management (ground)", 2, 0x2a, 0xfe))
	}
	return filter.Set{Version: 1, Rules: rules, Global: filter.Global{DropFCSErr: true}}
}
