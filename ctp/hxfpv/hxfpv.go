// Package hxfpv receives hx-esp32-cam-fpv video passively (docs/12), the Go port of
// js/hxfpv.js: 802.11 frame → Packet_Header → per-air-unit FEC blocks → Air2Ground packets
// → JPEG frames, OSD stats and config. Receive-only; Sender is for simulators and tests.
//
// Wire layout (little-endian, packed; hx-esp32-cam-fpv components/common):
//
//	802.11 data header (24 B, addr2 = 11:22:33:44:55:66)
//	Packet_Header (12 B): u8 version(2) u8 signature(56) u16 fromDeviceId
//	  u16 toDeviceId u16 size u24 block_index u8 packet_index
//	FEC slot (mtu B, default 1464): one Air2Ground packet, zero-padded
//	Air2Ground_Header (12 B): u8 type u32 size u8 pong u8 version u8 crc
//	  u16 airDeviceId u16 gsDeviceId; crc = CRC-8 (poly 0x07) over the type's header
//	  struct with the crc byte zeroed.
package hxfpv

import (
	"bytes"
	"encoding/binary"
	"math"
	"time"

	"github.com/hjkoskel/capturetonkeli/ctp/fec"
	"github.com/hjkoskel/capturetonkeli/ctp/filter"
)

// Protocol constants.
const (
	PacketVersion   = 2
	PacketSignature = 56
	WlanHdr         = 24
	PktHdr          = 12
	A2GHdr          = 12
	VideoHdr        = 18
	OSDSize         = 1244
	ConfigSize      = 53
	DefaultK        = 6
	MaxN            = 32

	TypeVideo     = 0
	TypeTelemetry = 1
	TypeOSD       = 2
	TypeConfig    = 3

	maxQueuedBlocks = 3   // like the native GS: give up on a block once 3 newer ones are waiting
	restartGap      = 100 // a block this far in the past means the air unit restarted
)

// AirMAC is addr2 of every air → ground frame.
var AirMAC = []byte{0x11, 0x22, 0x33, 0x44, 0x55, 0x66}

// Resolutions and Rates name the air unit's settings.
var (
	Resolutions = []string{"QVGA 320×240", "CIF 400×296", "HVGA 480×320", "VGA 640×480", "VGA16 640×360", "SVGA 800×600", "SVGA16 800×456",
		"XGA 1024×768", "XGA16 1024×576", "SXGA 1280×960", "HD 1280×720", "UXGA 1600×1200"}
	Rates = []string{"2M CCK", "2M CCK S", "5.5M CCK", "5.5M CCK S", "11M CCK", "11M CCK S", "6M", "9M", "12M", "18M", "24M", "36M", "48M", "54M",
		"MCS0 6.5M", "MCS0 7.2M S", "MCS1 13M", "MCS1 14.4M S", "MCS2 19.5M", "MCS2 21.7M S", "MCS3 26M", "MCS3 28.9M S", "MCS4 39M", "MCS4 43.3M S",
		"MCS5 52M", "MCS5 57.8M S", "MCS6 58M", "MCS6 65M S", "MCS7 65M", "MCS7 72M S"}
)

var le = binary.LittleEndian

// ---- CRC-8 (poly 0x07, init 0, no reflection) ----

var crc8Table = func() (t [256]byte) {
	for i := range t {
		c := byte(i)
		for j := 0; j < 8; j++ {
			if c&0x80 != 0 {
				c = c<<1 ^ 0x07
			} else {
				c <<= 1
			}
		}
		t[i] = c
	}
	return
}()

// CRC8 computes the CRC-8 of b.
func CRC8(b []byte) byte {
	var c byte
	for _, x := range b {
		c = crc8Table[c^x]
	}
	return c
}

// CRCOK checks an Air2Ground header struct of n bytes (crc byte at offset 7 counted as 0).
func CRCOK(slot []byte, n int) bool {
	if len(slot) < n {
		return false
	}
	var c byte
	for i := 0; i < n; i++ {
		x := slot[i]
		if i == 7 {
			x = 0
		}
		c = crc8Table[c^x]
	}
	return c == slot[7]
}

// SealCRC sets the crc byte of a header struct of n bytes.
func SealCRC(slot []byte, n int) []byte {
	slot[7] = 0
	slot[7] = CRC8(slot[:n])
	return slot
}

// Packet is the hx Packet_Header of a frame.
type Packet struct {
	From, To, Size uint16
	Block          uint32 // 24 bits
	Index          uint8
	Payload        []byte
}

// ParseFrame parses an 802.11 frame (no FCS). ok = false: not hx. version != 0: hx, but
// another protocol version.
func ParseFrame(f []byte) (p Packet, version uint8, ok bool) {
	if len(f) < WlanHdr+PktHdr+1 || f[0]&0x0c != 0x08 || !bytes.Equal(f[10:16], AirMAC) {
		return Packet{}, 0, false
	}
	h := f[WlanHdr:]
	if h[1] != PacketSignature {
		return Packet{}, 0, false
	}
	if h[0] != PacketVersion {
		return Packet{}, h[0], true
	}
	return Packet{From: le.Uint16(h[2:]), To: le.Uint16(h[4:]), Size: le.Uint16(h[6:]),
		Block: uint32(h[8]) | uint32(h[9])<<8 | uint32(h[10])<<16, Index: h[11], Payload: h[PktHdr:]}, 0, true
}

// A2G is the Air2Ground header of a FEC slot.
type A2G struct {
	Type    uint8
	Size    uint32
	Pong    uint8
	Version uint8
	CRC     uint8
	Air, GS uint16
}

// ParseA2G parses the Air2Ground header (s must be ≥ 12 bytes).
func ParseA2G(s []byte) A2G {
	return A2G{Type: s[0], Size: le.Uint32(s[1:]), Pong: s[5], Version: s[6], CRC: s[7], Air: le.Uint16(s[8:]), GS: le.Uint16(s[10:])}
}

// AirStats is the OSD packet's statistics block.
type AirStats struct {
	SDDetected, SDSlow, SDError                 bool
	WifiRate                                    uint8
	WifiQueueMin                                uint8
	AirRecording                                bool
	WifiQueueMax                                uint8
	SDFreeGB, SDTotalGB                         float64
	Quality                                     uint8
	WifiOverflow, OV5640                        bool
	OutPacketRate, InPacketRate, InRejectedRate uint16
	RSSIDbm, NoiseFloorDbm                      int
	CaptureFPS, CamOverflows                    uint8
	FrameSizeMin, FrameSizeMax                  uint16
	MavlinkIn, MavlinkOut                       uint16
	RCPeriodMax                                 uint8
	WifiChannel, Resolution                     uint8
	Temperature                                 uint8
	Overheat, Suspended                         bool
	FecK                                        uint8
	InSession                                   bool
	Aspect                                      uint8
	Brightness, Contrast, Saturation, Sharpness int
	AELevel                                     int
	At                                          time.Time
}

func sb3(v uint16) int { return int(int8(byte(v&7)<<5) >> 5) }

// ParseAirStats parses AirStats (32 B at offset 12 of an OSD packet).
func ParseAirStats(s []byte) AirStats {
	o := A2GHdr
	b3 := le.Uint32(s[o+3:])
	w := le.Uint16(s[o+30:])
	return AirStats{
		SDDetected: s[o]&1 != 0, SDSlow: s[o]&2 != 0, SDError: s[o]&4 != 0, WifiRate: s[o] >> 3,
		WifiQueueMin: s[o+1] & 0x7f, AirRecording: s[o+1]&0x80 != 0, WifiQueueMax: s[o+2],
		SDFreeGB: float64(b3&0xfff) / 16, SDTotalGB: float64((b3>>12)&0xfff) / 16, Quality: uint8((b3 >> 24) & 0x3f),
		WifiOverflow: (b3>>30)&1 != 0, OV5640: b3>>31 != 0,
		OutPacketRate: le.Uint16(s[o+7:]), InPacketRate: le.Uint16(s[o+9:]), InRejectedRate: le.Uint16(s[o+11:]),
		RSSIDbm: -int(s[o+13]), NoiseFloorDbm: -int(s[o+14]), CaptureFPS: s[o+15], CamOverflows: s[o+16],
		FrameSizeMin: le.Uint16(s[o+17:]), FrameSizeMax: le.Uint16(s[o+19:]), MavlinkIn: le.Uint16(s[o+21:]), MavlinkOut: le.Uint16(s[o+23:]),
		RCPeriodMax: s[o+25], WifiChannel: s[o+26] & 0x0f, Resolution: s[o+26] >> 4,
		Temperature: s[o+27] & 0x7f, Overheat: s[o+27]&0x80 != 0, Suspended: s[o+28]&0x80 != 0,
		FecK: s[o+29] & 0x0f, InSession: s[o+29]&0x10 != 0, Aspect: s[o+29] >> 5,
		Brightness: sb3(w), Contrast: sb3(w >> 3), Saturation: sb3(w >> 6), Sharpness: sb3(w >> 9), AELevel: sb3(w >> 12),
	}
}

// Config is the Air2Ground_Config_Packet: CameraConfig (at 12) + DataChannelConfig (at 42).
type Config struct {
	Resolution, FPSLimit, Quality uint8
	WifiPower                     int8
	WifiRate, WifiChannel         uint8
	FecK, FecN                    uint8
	MTU                           uint16
	At                            time.Time
}

// ParseConfig parses a config packet slot (≥ 53 bytes).
func ParseConfig(s []byte) Config {
	const d = 42
	return Config{Resolution: s[12], FPSLimit: s[13], Quality: s[14], WifiPower: int8(s[d]), WifiRate: s[d+1], WifiChannel: s[d+2],
		FecK: s[d+3], FecN: s[d+4], MTU: le.Uint16(s[d+5:])}
}

// JPEGSize returns the size from a JPEG SOFn marker (ok = false when not found).
func JPEGSize(j []byte) (width, height int, ok bool) {
	if len(j) < 4 || j[0] != 0xff || j[1] != 0xd8 {
		return 0, 0, false
	}
	p := 2
	for p+9 < len(j) {
		if j[p] != 0xff {
			p++
			continue
		}
		m := j[p+1]
		switch {
		case m == 0xff:
			p++
			continue
		case m == 0xd8 || m == 0x01 || (m >= 0xd0 && m <= 0xd7):
			p += 2
			continue
		case m == 0xd9 || m == 0xda:
			return 0, 0, false
		}
		n := int(j[p+2])<<8 | int(j[p+3])
		if m >= 0xc0 && m <= 0xcf && m != 0xc4 && m != 0xc8 && m != 0xcc {
			return int(j[p+7])<<8 | int(j[p+8]), int(j[p+5])<<8 | int(j[p+6]), true
		}
		p += 2 + n
	}
	return 0, 0, false
}

// Stats are one air unit's counters.
type Stats struct {
	Packets, DataPackets, FECPackets, FECUnneeded, Duplicates, Late, BadIndex uint64
	BlocksOK, BlocksFEC, BlocksLost, SlotsLost, CRCErrors, BadSize, Restarts  uint64
	FramesOK, FramesFEC, FramesLost, BadJPEG                                  uint64
	VideoBytes, TelemetryBytes, OSD, Configs, WireBytes                       uint64
}

// FrameInfo describes a delivered JPEG frame.
type FrameInfo struct {
	Index      uint32
	Size       int
	FEC        bool // a part was rebuilt by FEC
	Resolution uint8
	Parts      int
	At         time.Time
}

type block struct {
	have    map[int][]byte
	emitted int
}

type frameAsm struct {
	index    int64
	nextPart int
	parts    [][]byte
	size     int
	fec      bool
}

// Unit is one air unit (by its device id).
type Unit struct {
	ID, GS    uint16
	K         int
	KSource   string // "default", "OSD" or "config"
	MTU       int
	RSSI      *float64
	Channel   uint8
	St        Stats
	OSD       *AirStats
	Config    *Config
	LastFrame *FrameInfo
	Pong      *uint8
	First     time.Time
	Last      time.Time

	next   int64
	blocks map[uint32]*block
	frame  frameAsm
}

// Options for NewReceiver.
type Options struct {
	OnFrame func(u *Unit, jpeg []byte, info FrameInfo) // every complete JPEG of every unit
	Now     func() time.Time
}

// Receiver handles every air unit in range. Not safe for concurrent use.
type Receiver struct {
	o                Options
	Units            map[uint16]*Unit
	WrongVersion     uint64
	LastWrongVersion uint8
}

// NewReceiver returns a receiver.
func NewReceiver(o Options) *Receiver {
	if o.Now == nil {
		o.Now = time.Now
	}
	return &Receiver{o: o, Units: map[uint16]*Unit{}}
}

// Reset forgets every unit.
func (r *Receiver) Reset() { r.Units = map[uint16]*Unit{}; r.WrongVersion = 0 }

// RxMeta is the dongle metadata of a frame.
type RxMeta struct {
	RSSI    int8
	HasRSSI bool
	Channel uint8
}

// Push takes one captured 802.11 frame (without FCS) and reports whether it was an hx packet.
func (r *Receiver) Push(f []byte, m RxMeta) bool {
	p, ver, ok := ParseFrame(f)
	if !ok {
		return false
	}
	if ver != 0 {
		r.WrongVersion++
		r.LastWrongVersion = ver
		return true
	}
	u := r.Units[p.From]
	if u == nil {
		u = &Unit{ID: p.From, GS: p.To, K: DefaultK, KSource: "default", MTU: len(p.Payload), next: -1, blocks: map[uint32]*block{},
			First: r.o.Now(), Channel: m.Channel, frame: frameAsm{index: -1}}
		r.Units[p.From] = u
	}
	st := &u.St
	st.Packets++
	st.WireBytes += uint64(len(f))
	u.Last, u.GS = r.o.Now(), p.To
	if m.HasRSSI {
		v := float64(m.RSSI)
		if u.RSSI != nil {
			v = *u.RSSI*0.9 + v*0.1
		}
		u.RSSI = &v
	}
	if m.Channel != 0 {
		u.Channel = m.Channel
	}
	if len(p.Payload) != u.MTU { // MTU changed (air unit reconfigured): restart decoding
		u.MTU = len(p.Payload)
		u.restart()
	}
	if p.Index >= MaxN {
		st.BadIndex++
		return true
	}
	if int(p.Index) < u.K {
		st.DataPackets++
	} else {
		st.FECPackets++
	}
	if u.next < 0 {
		u.next = int64(p.Block)
	}
	if int64(p.Block) < u.next {
		if u.next-int64(p.Block) > restartGap {
			st.Restarts++
			u.restart()
			u.next = int64(p.Block)
		} else {
			if int(p.Index) < u.K {
				st.Late++
			} else {
				st.FECUnneeded++
			}
			return true
		}
	}
	b := u.blocks[p.Block]
	if b == nil {
		b = &block{have: map[int][]byte{}}
		u.blocks[p.Block] = b
	}
	if _, dup := b.have[int(p.Index)]; dup {
		st.Duplicates++
		return true
	}
	b.have[int(p.Index)] = append([]byte(nil), p.Payload...)
	r.process(u)
	return true
}

func (u *Unit) restart() {
	u.blocks = map[uint32]*block{}
	u.next = -1
	u.frame = frameAsm{index: -1}
}

func (u *Unit) setK(k int, source string) {
	if k == u.K {
		u.KSource = source
		return
	}
	if k < 1 || k > 16 {
		return
	}
	u.K, u.KSource = k, source
	next := u.next
	u.restart()
	u.next = next
}

func (u *Unit) oldest() (uint32, *block) {
	var idx uint32
	var ob *block
	for i, b := range u.blocks {
		if ob == nil || i < idx {
			idx, ob = i, b
		}
	}
	return idx, ob
}

func (r *Receiver) process(u *Unit) {
	st := &u.St
	for len(u.blocks) > 0 {
		idx, b := u.oldest()
		k := u.K
		// data packets in order, as soon as they arrive (low latency)
		for b.emitted < k {
			d, ok := b.have[b.emitted]
			if !ok {
				break
			}
			r.slot(u, d, false)
			b.emitted++
			if u.K != k {
				return
			}
		}
		nData := 0
		for i := range b.have {
			if i < k {
				nData++
			}
		}
		if nData >= k {
			st.BlocksOK++
			u.done(idx)
			continue
		}
		if len(b.have) >= k {
			if rec, err := fec.VandermondeDecode(k, b.have, u.MTU); err == nil {
				st.BlocksFEC++
				for i := b.emitted; i < k; i++ {
					d, had := b.have[i]
					if !had {
						d = rec[i]
					}
					r.slot(u, d, !had)
					if u.K != k {
						return
					}
				}
				u.done(idx)
				continue
			}
		}
		if len(u.blocks) > maxQueuedBlocks {
			st.BlocksLost++
			st.SlotsLost += uint64(k - b.emitted)
			u.done(idx)
			continue
		}
		break
	}
}

func (u *Unit) done(idx uint32) { delete(u.blocks, idx); u.next = int64(idx) + 1 }

// slot handles one decoded FEC slot = one Air2Ground packet.
func (r *Receiver) slot(u *Unit, s []byte, recovered bool) {
	st := &u.St
	if len(s) < A2GHdr {
		st.BadSize++
		return
	}
	h := ParseA2G(s)
	sizeOK := func(min int) bool { return h.Size >= uint32(min) && h.Size <= uint32(len(s)) }
	switch h.Type {
	case TypeVideo:
		if !sizeOK(VideoHdr) {
			st.BadSize++
			return
		}
		if !CRCOK(s, VideoHdr) {
			st.CRCErrors++
			return
		}
		pong := h.Pong
		u.Pong = &pong
		r.video(u, s, h, recovered)
	case TypeOSD:
		if !sizeOK(OSDSize) {
			st.BadSize++
			return
		}
		if !CRCOK(s, OSDSize) {
			st.CRCErrors++
			return
		}
		st.OSD++
		a := ParseAirStats(s)
		a.At = r.o.Now()
		u.OSD = &a
		if a.FecK != 0 {
			u.setK(int(a.FecK), "OSD")
		}
	case TypeConfig:
		if !sizeOK(ConfigSize) {
			st.BadSize++
			return
		}
		if !CRCOK(s, ConfigSize) {
			st.CRCErrors++
			return
		}
		st.Configs++
		c := ParseConfig(s)
		c.At = r.o.Now()
		u.Config = &c
		if c.FecK != 0 {
			u.setK(int(c.FecK), "config")
		}
	case TypeTelemetry:
		if !sizeOK(A2GHdr) {
			st.BadSize++
			return
		}
		if !CRCOK(s, A2GHdr) {
			st.CRCErrors++
			return
		}
		st.TelemetryBytes += uint64(h.Size) - A2GHdr
	default:
		st.BadSize++
	}
}

// video assembles frames with the same rules as the native GS (main.cpp).
func (r *Receiver) video(u *Unit, s []byte, h A2G, recovered bool) {
	st, f := &u.St, &u.frame
	resolution, part, last, index := s[12], int(s[13]&0x7f), s[13]&0x80 != 0, int64(le.Uint32(s[14:]))
	if index+200 < f.index || index > f.index {
		if f.nextPart != 0 {
			st.FramesLost++
		}
		if f.index >= 0 && index > f.index+1 {
			st.FramesLost += uint64(index - f.index - 1)
		}
		*f = frameAsm{index: index}
	}
	if index != f.index || part != f.nextPart {
		return
	}
	data := s[VideoHdr:h.Size]
	f.parts = append(f.parts, data)
	f.size += len(data)
	f.nextPart++
	f.fec = f.fec || recovered
	if !last {
		return
	}
	jpeg := make([]byte, 0, f.size)
	for _, d := range f.parts {
		jpeg = append(jpeg, d...)
	}
	fecUsed := f.fec
	f.nextPart, f.parts, f.size, f.fec = 0, nil, 0, false
	st.VideoBytes += uint64(len(jpeg))
	if len(jpeg) < 2 || jpeg[0] != 0xff || jpeg[1] != 0xd8 {
		st.BadJPEG++
		return
	}
	if fecUsed {
		st.FramesFEC++
	} else {
		st.FramesOK++
	}
	info := FrameInfo{Index: uint32(index), Size: len(jpeg), FEC: fecUsed, Resolution: resolution, Parts: part + 1, At: r.o.Now()}
	u.LastFrame = &info
	if r.o.OnFrame != nil {
		r.o.OnFrame(u, jpeg, info)
	}
}

// UnitSummary is a snapshot of one unit.
type UnitSummary struct {
	ID, GS    uint16
	K         int
	KSource   string
	MTU       int
	RSSI      *int
	Channel   uint8
	Age       time.Duration
	St        Stats
	OSD       *AirStats
	Config    *Config
	LastFrame *FrameInfo
	Queued    int
}

// Summary returns a snapshot of every unit (sorted by id).
func (r *Receiver) Summary() []UnitSummary {
	now := r.o.Now()
	var out []UnitSummary
	for _, u := range r.Units {
		s := UnitSummary{ID: u.ID, GS: u.GS, K: u.K, KSource: u.KSource, MTU: u.MTU, Channel: u.Channel, Age: now.Sub(u.Last), St: u.St,
			OSD: u.OSD, Config: u.Config, LastFrame: u.LastFrame, Queued: len(u.blocks)}
		if u.RSSI != nil {
			v := int(math.Round(*u.RSSI))
			s.RSSI = &v
		}
		out = append(out, s)
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].ID < out[j-1].ID; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// ---- sender side (simulators and tests) ----

// VideoSlots returns the Air2Ground video packets of one JPEG (each one FEC slot of mtu bytes).
func VideoSlots(jpeg []byte, frameIndex uint32, resolution uint8, air, gs uint16, mtu int) [][]byte {
	max := mtu - VideoHdr
	parts := (len(jpeg) + max - 1) / max
	if parts < 1 {
		parts = 1
	}
	var out [][]byte
	for i := 0; i < parts; i++ {
		data := jpeg[i*max : min(len(jpeg), (i+1)*max)]
		s := make([]byte, mtu)
		s[0] = TypeVideo
		le.PutUint32(s[1:], uint32(VideoHdr+len(data)))
		s[6] = PacketVersion
		le.PutUint16(s[8:], air)
		le.PutUint16(s[10:], gs)
		s[12] = resolution
		s[13] = byte(i & 0x7f)
		if i == parts-1 {
			s[13] |= 0x80
		}
		le.PutUint32(s[14:], frameIndex)
		copy(s[VideoHdr:], data)
		out = append(out, SealCRC(s, VideoHdr))
	}
	return out
}

// OSDFields are the AirStats fields OSDSlot sets.
type OSDFields struct {
	FecK, CaptureFPS, Temperature, Quality, Resolution, WifiChannel, WifiRate uint8
	RSSIDbm                                                                   int
}

// DefaultOSD are the demo air unit's values.
var DefaultOSD = OSDFields{FecK: DefaultK, RSSIDbm: -60, CaptureFPS: 30, Temperature: 45, Quality: 20, WifiChannel: 7, WifiRate: 20}

// OSDSlot returns an OSD packet slot.
func OSDSlot(air, gs uint16, mtu int, f OSDFields) []byte {
	s := make([]byte, mtu)
	o := A2GHdr
	s[0] = TypeOSD
	le.PutUint32(s[1:], OSDSize)
	s[6] = PacketVersion
	le.PutUint16(s[8:], air)
	le.PutUint16(s[10:], gs)
	s[o] = (f.WifiRate & 0x1f) << 3
	le.PutUint32(s[o+3:], uint32(f.Quality&0x3f)<<24)
	s[o+13], s[o+14], s[o+15] = byte(-f.RSSIDbm), 95, f.CaptureFPS
	s[o+26] = f.WifiChannel&0x0f | (f.Resolution&0x0f)<<4
	s[o+27] = f.Temperature & 0x7f
	s[o+29] = f.FecK&0x0f | 0x10
	return SealCRC(s, OSDSize)
}

// Sender wraps FEC slots into 802.11 frames: k data packets per block plus n-k FEC packets.
type Sender struct {
	Air, GS uint16
	K, N    int
	MTU     int
	Block   uint32
	pending [][]byte
	seq     uint16
}

// NewSender returns a sender with the hx defaults (k 6, n 12, MTU 1464).
func NewSender(air, gs uint16) *Sender {
	return &Sender{Air: air, GS: gs, K: DefaultK, N: 12, MTU: 1464}
}

// Push adds slots and returns the 802.11 frames (without FCS) of every completed block.
func (s *Sender) Push(slots ...[]byte) [][]byte {
	var frames [][]byte
	for _, sl := range slots {
		s.pending = append(s.pending, sl)
		frames = append(frames, s.frame(sl, len(s.pending)-1))
		if len(s.pending) == s.K {
			for i := s.K; i < s.N; i++ {
				p, err := fec.VandermondeEncode(s.K, s.pending, i, s.MTU)
				if err != nil {
					panic(err) // K/N validated by the caller
				}
				frames = append(frames, s.frame(p, i))
			}
			s.pending = nil
			s.Block = (s.Block + 1) & 0xffffff
		}
	}
	return frames
}

func (s *Sender) frame(payload []byte, index int) []byte {
	f := make([]byte, WlanHdr+PktHdr+s.MTU)
	copy(f, []byte{0x08, 0, 0, 0, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff})
	copy(f[10:], AirMAC)
	copy(f[16:], AirMAC)
	s.seq = (s.seq + 1) & 0xfff
	le.PutUint16(f[22:], s.seq<<4)
	p := f[WlanHdr:]
	p[0], p[1] = PacketVersion, PacketSignature
	le.PutUint16(p[2:], s.Air)
	le.PutUint16(p[4:], s.GS)
	le.PutUint16(p[6:], uint16(s.MTU))
	p[8], p[9], p[10], p[11] = byte(s.Block), byte(s.Block>>8), byte(s.Block>>16), byte(index)
	copy(p[PktHdr:], payload[:min(len(payload), s.MTU)])
	return f
}

// CaptureFilter is the dongle filter for hx air→ground traffic (docs/12 §5). keepBelow:
// keep packet_index < 8 or < 16; 0 = keep all.
func CaptureFilter(keepBelow int) filter.Set {
	terms := []filter.Term{
		{Offset: 0, Value: []byte{0x08}, Mask: []byte{0x0c}},
		{Offset: 10, Value: AirMAC, Mask: bytes.Repeat([]byte{0xff}, 6)},
		{Offset: 24, Value: []byte{0x02, 0x38}, Mask: []byte{0xff, 0xff}},
	}
	name := "hx-esp32-cam-fpv air→ground"
	if keepBelow == 8 || keepBelow == 16 {
		mask := byte(0xf8)
		if keepBelow == 16 {
			mask = 0xf0
		}
		terms = append(terms, filter.Term{Offset: 35, Value: []byte{0}, Mask: []byte{mask}})
		if keepBelow == 8 {
			name = "hx-esp32-cam-fpv air→ground (packets 0…7 of each block)"
		} else {
			name = "hx-esp32-cam-fpv air→ground (packets 0…15 of each block)"
		}
	}
	return filter.Set{Version: 1, Rules: []filter.Rule{{Name: name, Enabled: true, Terms: terms}}, Global: filter.Global{DropFCSErr: true}}
}
