// Package video turns OpenHD's RTP video into H.264/H.265 access units and MP4 files
// (docs/13 §6, §13.2). Port of js/rtp.js and js/mp4.js.
//
// RTP → access units in Annex B format: one Depacketizer per video stream. Access units end
// at the RTP marker bit or a timestamp change; a sequence gap marks the unit as damaged.
package video

import (
	"encoding/binary"
	"fmt"
	"math/bits"
	"strings"
)

// Codecs.
const (
	H264 = "h264"
	H265 = "h265"
	Auto = "auto"
)

var startCode = []byte{0, 0, 0, 1}

var h264Valid = map[byte]bool{1: true, 5: true, 6: true, 7: true, 8: true, 9: true, 10: true, 11: true, 12: true, 24: true, 28: true}
var h265Valid = map[byte]bool{0: true, 1: true, 2: true, 3: true, 8: true, 9: true, 16: true, 17: true, 18: true, 19: true, 20: true, 21: true,
	32: true, 33: true, 34: true, 35: true, 39: true, 40: true, 48: true, 49: true}

// GuessCodec guesses the codec of one RTP payload: H264, H265 or "" (ambiguous).
func GuessCodec(p []byte) string {
	if len(p) < 3 || p[0]&0x80 != 0 {
		return ""
	}
	t5 := (p[0] >> 1) & 0x3f
	h265 := p[0]&0x01 == 0 && p[1] >= 1 && p[1] <= 7 && h265Valid[t5]
	h264 := h264Valid[p[0]&0x1f] && p[0]&0x1f != 0
	switch {
	case h265 && !h264:
		return H265
	case h264 && !h265:
		return H264
	}
	return ""
}

// AU is one access unit.
type AU struct {
	Codec       string
	Data        []byte // Annex B (start codes); parameter sets are added before key frames
	Key         bool
	Damaged     bool   // an RTP packet of it was lost
	TS          uint32 // RTP timestamp (90 kHz)
	CodecString string // WebCodecs / RFC 6381 codec string, e.g. "avc1.42c014"
	HasParams   bool   // all parameter sets seen so far
}

// DepacketizerStats count packets and access units.
type DepacketizerStats struct {
	Packets uint64 `json:"packets"`
	NotRTP  uint64 `json:"not_rtp"`
	Gaps    uint64 `json:"gaps"`
	AUs     uint64 `json:"aus"`
	Keys    uint64 `json:"keys"`
	Damaged uint64 `json:"damaged"`
	Bytes   uint64 `json:"bytes"`
}

type pendingPkt struct {
	pl     []byte
	marker bool
	ts     uint32
}

// Depacketizer reassembles access units from RTP packets (RFC 6184 / RFC 7798).
type Depacketizer struct {
	St DepacketizerStats

	want    string
	codec   string
	onAU    func(AU)
	votes   map[string]int
	nals    [][]byte
	fu      [][]byte
	ts      uint32
	hasTS   bool
	lastSeq uint16
	hasSeq  bool
	damaged bool
	pending []pendingPkt
	params  map[string][]byte
}

// NewDepacketizer returns a depacketizer for codec (H264, H265 or Auto: guessed from the
// first packets). onAU gets each access unit; its Data is owned by the callee.
func NewDepacketizer(codec string, onAU func(AU)) *Depacketizer {
	if codec == "" {
		codec = Auto
	}
	d := &Depacketizer{want: codec, onAU: onAU}
	d.Reset()
	return d
}

// Codec is the codec in use ("" while still guessing).
func (d *Depacketizer) Codec() string { return d.codec }

// Reset forgets the stream state (not the statistics).
func (d *Depacketizer) Reset() {
	d.codec = ""
	if d.want != Auto {
		d.codec = d.want
	}
	d.votes = map[string]int{}
	d.nals, d.fu, d.hasTS, d.hasSeq, d.damaged, d.pending = nil, nil, false, false, false, nil
	d.params = map[string][]byte{}
}

// SetCodec changes the codec (H264, H265 or Auto) and resets the stream.
func (d *Depacketizer) SetCodec(c string) {
	if c != d.want {
		d.want = c
		d.Reset()
	}
}

// Push takes one RTP packet (copied as needed). gap: packets before it were lost upstream.
func (d *Depacketizer) Push(p []byte, gap bool) {
	d.St.Packets++
	if len(p) < 12 || p[0]>>6 != 2 {
		d.St.NotRTP++
		return
	}
	cc, marker, seq := int(p[0]&0x0f), p[1]&0x80 != 0, binary.BigEndian.Uint16(p[2:])
	ts := binary.BigEndian.Uint32(p[4:])
	off, end := 12+4*cc, len(p)
	if p[0]&0x10 != 0 {
		if off+4 > end {
			return
		}
		off += 4 + 4*int(binary.BigEndian.Uint16(p[off+2:]))
	}
	if p[0]&0x20 != 0 {
		end -= int(p[end-1])
	}
	if off >= end {
		return
	}
	pl := append([]byte(nil), p[off:end]...)
	if (d.hasSeq && seq != d.lastSeq+1) || gap {
		d.St.Gaps++
		d.damaged = true
		d.fu = nil
	}
	d.lastSeq, d.hasSeq = seq, true
	if d.hasTS && ts != d.ts && (len(d.nals) > 0 || d.fu != nil) {
		d.flush()
	}
	d.ts, d.hasTS = ts, true
	if d.codec == "" {
		// undecided: keep the packets and replay them once the codec is known
		d.pending = append(d.pending, pendingPkt{pl, marker, ts})
		if g := GuessCodec(pl); g != "" {
			d.votes[g]++
			other := H264
			if g == H264 {
				other = H265
			}
			if d.votes[g] >= 3 && d.votes[g] > 2*d.votes[other] {
				d.codec = g
			}
		}
		if d.codec == "" {
			if len(d.pending) > 200 {
				d.pending = d.pending[1:]
			}
			return
		}
		pend := d.pending
		d.pending = nil
		d.ts = pend[0].ts
		for _, q := range pend {
			if q.ts != d.ts && len(d.nals) > 0 {
				d.flush()
			}
			d.ts = q.ts
			d.payload(q.pl)
			if q.marker {
				d.flush()
			}
		}
		return
	}
	d.payload(pl)
	if marker {
		d.flush()
	}
}

func (d *Depacketizer) payload(p []byte) {
	if d.codec == H264 {
		d.h264(p)
	} else {
		d.h265(p)
	}
}

func (d *Depacketizer) h264(p []byte) {
	t := p[0] & 0x1f
	switch {
	case t >= 1 && t <= 23:
		d.nals = append(d.nals, p)
	case t == 24:
		d.aggregate(p, 1)
	case t == 28 && len(p) > 2:
		h := p[1]
		if h&0x80 != 0 {
			d.fu = [][]byte{{(p[0] & 0xe0) | (h & 0x1f)}, p[2:]}
		} else if d.fu != nil {
			d.fu = append(d.fu, p[2:])
		}
		if h&0x40 != 0 && d.fu != nil {
			d.nals = append(d.nals, concat(d.fu))
			d.fu = nil
		}
	}
}

func (d *Depacketizer) h265(p []byte) {
	t := (p[0] >> 1) & 0x3f
	switch {
	case t < 48:
		d.nals = append(d.nals, p)
	case t == 48:
		d.aggregate(p, 2)
	case t == 49 && len(p) > 3:
		h := p[2]
		if h&0x80 != 0 {
			d.fu = [][]byte{{(p[0] & 0x81) | ((h & 0x3f) << 1), p[1]}, p[3:]}
		} else if d.fu != nil {
			d.fu = append(d.fu, p[3:])
		}
		if h&0x40 != 0 && d.fu != nil {
			d.nals = append(d.nals, concat(d.fu))
			d.fu = nil
		}
	}
}

func (d *Depacketizer) aggregate(p []byte, skip int) {
	for i := skip; i+2 <= len(p); {
		n := int(binary.BigEndian.Uint16(p[i:]))
		if n == 0 || i+2+n > len(p) {
			break
		}
		d.nals = append(d.nals, p[i+2:i+2+n])
		i += 2 + n
	}
}

func nalType(codec string, n []byte) byte {
	if codec == H265 {
		return (n[0] >> 1) & 0x3f
	}
	return n[0] & 0x1f
}

func paramName(codec string, t byte) string {
	if codec == H265 {
		return map[byte]string{32: "vps", 33: "sps", 34: "pps"}[t]
	}
	return map[byte]string{7: "sps", 8: "pps"}[t]
}

func needParams(codec string) []string {
	if codec == H265 {
		return []string{"vps", "sps", "pps"}
	}
	return []string{"sps", "pps"}
}

func (d *Depacketizer) flush() {
	nals := d.nals
	d.nals, d.fu = nil, nil
	damaged := d.damaged
	d.damaged = false
	if len(nals) == 0 {
		return
	}
	h265 := d.codec == H265
	key := false
	have := map[string]bool{}
	for _, n := range nals {
		if len(n) == 0 {
			continue
		}
		t := nalType(d.codec, n)
		if name := paramName(d.codec, t); name != "" {
			d.params[name] = append([]byte(nil), n...)
			have[name] = true
		}
		if (h265 && t >= 16 && t <= 21) || (!h265 && t == 5) {
			key = true
		}
	}
	var parts [][]byte
	if key {
		for _, k := range needParams(d.codec) {
			if !have[k] && d.params[k] != nil {
				parts = append(parts, startCode, d.params[k])
			}
		}
	}
	for _, n := range nals {
		parts = append(parts, startCode, n)
	}
	data := concat(parts)
	d.St.AUs++
	d.St.Bytes += uint64(len(data))
	if key {
		d.St.Keys++
	}
	if damaged {
		d.St.Damaged++
	}
	hasParams := true
	for _, k := range needParams(d.codec) {
		if d.params[k] == nil {
			hasParams = false
		}
	}
	if d.onAU != nil {
		d.onAU(AU{Codec: d.codec, Data: data, Key: key, Damaged: damaged, TS: d.ts, CodecString: CodecString(d.codec, d.params["sps"]), HasParams: hasParams})
	}
}

func concat(parts [][]byte) []byte {
	n := 0
	for _, p := range parts {
		n += len(p)
	}
	o := make([]byte, 0, n)
	for _, p := range parts {
		o = append(o, p...)
	}
	return o
}

// UnescapeRBSP removes emulation prevention bytes (00 00 03 → 00 00).
func UnescapeRBSP(b []byte) []byte {
	o := make([]byte, 0, len(b))
	z := 0
	for _, c := range b {
		if z >= 2 && c == 3 {
			z = 0
			continue
		}
		if c == 0 {
			z++
		} else {
			z = 0
		}
		o = append(o, c)
	}
	return o
}

// CodecString returns the WebCodecs / RFC 6381 codec string from the SPS NAL (with its NAL
// header), "" when unknown.
func CodecString(codec string, sps []byte) string {
	if sps == nil {
		return ""
	}
	if codec == H264 {
		if len(sps) < 4 {
			return ""
		}
		return fmt.Sprintf("avc1.%02x%02x%02x", sps[1], sps[2], sps[3])
	}
	if len(sps) < 3 {
		return ""
	}
	r := UnescapeRBSP(sps[2:min(len(sps), 40)])
	if len(r) < 13 {
		return ""
	}
	space, tier, profile := r[1]>>6, (r[1]>>5)&1, r[1]&0x1f
	rev := bits.Reverse32(binary.BigEndian.Uint32(r[2:]))
	cons := r[6:12]
	for len(cons) > 0 && cons[len(cons)-1] == 0 {
		cons = cons[:len(cons)-1]
	}
	var b strings.Builder
	fmt.Fprintf(&b, "hev1.%s%d.%x.", []string{"", "A", "B", "C"}[space], profile, rev)
	if tier != 0 {
		b.WriteString("H")
	} else {
		b.WriteString("L")
	}
	fmt.Fprintf(&b, "%d", r[12])
	for _, c := range cons {
		fmt.Fprintf(&b, ".%X", c)
	}
	return b.String()
}

// SplitAnnexB splits an Annex B buffer into NAL units (without start codes).
func SplitAnnexB(b []byte) [][]byte {
	var out [][]byte
	start := -1
	for i := 0; i+2 < len(b); i++ {
		if b[i] == 0 && b[i+1] == 0 && b[i+2] == 1 {
			if start >= 0 {
				e := i
				for e > start && b[e-1] == 0 {
					e--
				}
				out = append(out, b[start:e])
			}
			start = i + 3
			i += 2
		}
	}
	if start >= 0 && start < len(b) {
		out = append(out, b[start:])
	}
	return out
}

// Packetizer packs access units into RTP (single NAL / FU-A / H.265 FU), like GStreamer
// rtph26xpay (simulators and tests).
type Packetizer struct {
	Codec string
	MTU   int
	PT    uint8
	SSRC  uint32
	Seq   uint16
}

// NewPacketizer returns a packetizer with GStreamer's defaults (MTU 1440, PT 96).
func NewPacketizer(codec string) *Packetizer {
	return &Packetizer{Codec: codec, MTU: 1440, PT: 96, SSRC: 0x0ffd0ffd}
}

// Packetize returns the RTP packets of one access unit (Annex B); the last has the marker.
func (k *Packetizer) Packetize(annexB []byte, ts uint32) [][]byte {
	nals := SplitAnnexB(annexB)
	var out [][]byte
	max := k.MTU - 12
	h265 := k.Codec == H265
	pkt := func(payload []byte, marker bool) {
		p := make([]byte, 12+len(payload))
		p[0], p[1] = 0x80, k.PT
		if marker {
			p[1] |= 0x80
		}
		binary.BigEndian.PutUint16(p[2:], k.Seq)
		k.Seq++
		binary.BigEndian.PutUint32(p[4:], ts)
		binary.BigEndian.PutUint32(p[8:], k.SSRC)
		copy(p[12:], payload)
		out = append(out, p)
	}
	for ni, n := range nals {
		last := ni == len(nals)-1
		if len(n) <= max {
			pkt(n, last)
			continue
		}
		hl, typ := 1, n[0]&0x1f
		ind := []byte{(n[0] & 0xe0) | 28}
		if h265 {
			hl, typ = 2, (n[0]>>1)&0x3f
			ind = []byte{(n[0] & 0x81) | (49 << 1), n[1]}
		}
		room := max - hl - 1
		for i := hl; i < len(n); i += room {
			chunk := n[i:min(len(n), i+room)]
			s, e := i == hl, i+room >= len(n)
			p := append(append([]byte(nil), ind...), typ)
			if s {
				p[hl] |= 0x80
			}
			if e {
				p[hl] |= 0x40
			}
			pkt(append(p, chunk...), last && e)
		}
	}
	return out
}
