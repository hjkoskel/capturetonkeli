package wire

import (
	"encoding/binary"
	"strings"
)

// reader is a bounds-checked little-endian cursor with a sticky error,
// the Go twin of firmware rd.c.
type reader struct {
	b   []byte
	off int
	err bool
}

func (r *reader) left() int { return len(r.b) - r.off }

func (r *reader) take(n int) []byte {
	if r.err || r.left() < n {
		r.err = true
		r.off = len(r.b)
		return make([]byte, n)
	}
	s := r.b[r.off : r.off+n]
	r.off += n
	return s
}
func (r *reader) u8() uint8   { return r.take(1)[0] }
func (r *reader) i8() int8    { return int8(r.take(1)[0]) }
func (r *reader) u16() uint16 { return binary.LittleEndian.Uint16(r.take(2)) }
func (r *reader) u32() uint32 { return binary.LittleEndian.Uint32(r.take(4)) }
func (r *reader) i32() int32  { return int32(r.u32()) }
func (r *reader) u64() uint64 { return binary.LittleEndian.Uint64(r.take(8)) }

func (r *reader) done() error {
	if r.err {
		return ErrShortBody
	}
	return nil
}

var le = binary.LittleEndian

func appendStr8(dst []byte, s string, max int) []byte {
	if len(s) > max {
		s = s[:max]
	}
	dst = append(dst, uint8(len(s)))
	return append(dst, s...)
}

// ---- requests -------------------------------------------------------------

// Hello is the HELLO request body.
type Hello struct {
	HostNonce    uint32
	HostProtoVer uint8
}

func (m Hello) AppendBinary(dst []byte) []byte {
	return append(le.AppendUint32(dst, m.HostNonce), m.HostProtoVer)
}
func (m *Hello) UnmarshalBinary(b []byte) error {
	r := reader{b: b}
	m.HostNonce, m.HostProtoVer = r.u32(), r.u8()
	return r.done()
}

// Ping is the PING request body.
type Ping struct{ HostTimeMS uint32 }

func (m Ping) AppendBinary(dst []byte) []byte { return le.AppendUint32(dst, m.HostTimeMS) }
func (m *Ping) UnmarshalBinary(b []byte) error {
	r := reader{b: b}
	m.HostTimeMS = r.u32()
	return r.done()
}

// Pong is the RSP_OK body for PING.
type Pong struct {
	HostTimeMS   uint32
	DeviceTimeUS uint64
}

func (m Pong) AppendBinary(dst []byte) []byte {
	return le.AppendUint64(le.AppendUint32(dst, m.HostTimeMS), m.DeviceTimeUS)
}
func (m *Pong) UnmarshalBinary(b []byte) error {
	r := reader{b: b}
	m.HostTimeMS, m.DeviceTimeUS = r.u32(), r.u64()
	return r.done()
}

// SetChannel is the SET_CHANNEL body.
type SetChannel struct {
	Channel uint8
	BW      Bandwidth
}

func (m SetChannel) AppendBinary(dst []byte) []byte { return append(dst, m.Channel, uint8(m.BW)) }
func (m *SetChannel) UnmarshalBinary(b []byte) error {
	r := reader{b: b}
	m.Channel, m.BW = r.u8(), Bandwidth(r.u8())
	return r.done()
}

// Country is the SET_COUNTRY body and the country part of CONFIG.
type Country struct {
	CC    [3]byte
	Mode  CountryMode
	SChan uint8
	NChan uint8
}

// MakeCountry builds a Country from a 2-letter code ("FI", "01").
func MakeCountry(cc string, mode CountryMode, schan, nchan uint8) Country {
	var c Country
	copy(c.CC[:2], strings.ToUpper(cc))
	c.Mode, c.SChan, c.NChan = mode, schan, nchan
	return c
}

// Code returns the country code without trailing NUL/space.
func (c Country) Code() string { return strings.TrimRight(string(c.CC[:]), "\x00 ") }

func (m Country) AppendBinary(dst []byte) []byte {
	return append(dst, m.CC[0], m.CC[1], m.CC[2], uint8(m.Mode), m.SChan, m.NChan)
}
func (m *Country) UnmarshalBinary(b []byte) error {
	r := reader{b: b}
	copy(m.CC[:], r.take(3))
	m.Mode, m.SChan, m.NChan = CountryMode(r.u8()), r.u8(), r.u8()
	return r.done()
}

// SetSnaplen is the SET_SNAPLEN body.
type SetSnaplen struct{ Snaplen uint16 }

func (m SetSnaplen) AppendBinary(dst []byte) []byte { return le.AppendUint16(dst, m.Snaplen) }
func (m *SetSnaplen) UnmarshalBinary(b []byte) error {
	r := reader{b: b}
	m.Snaplen = r.u16()
	return r.done()
}

// SetDriverMask is the SET_DRIVER_MASK body.
type SetDriverMask struct{ TypeMask, CtrlMask uint32 }

func (m SetDriverMask) AppendBinary(dst []byte) []byte {
	return le.AppendUint32(le.AppendUint32(dst, m.TypeMask), m.CtrlMask)
}
func (m *SetDriverMask) UnmarshalBinary(b []byte) error {
	r := reader{b: b}
	m.TypeMask, m.CtrlMask = r.u32(), r.u32()
	return r.done()
}

// Capture is the CAPTURE body.
type Capture struct{ On bool }

func (m Capture) AppendBinary(dst []byte) []byte { return append(dst, b2u(m.On)) }
func (m *Capture) UnmarshalBinary(b []byte) error {
	r := reader{b: b}
	v := r.u8()
	m.On = v != 0
	return r.done()
}

// GetStats is the GET_STATS body.
type GetStats struct{ ResetAfterRead bool }

func (m GetStats) AppendBinary(dst []byte) []byte { return append(dst, b2u(m.ResetAfterRead)) }
func (m *GetStats) UnmarshalBinary(b []byte) error {
	r := reader{b: b}
	m.ResetAfterRead = r.u8() != 0
	return r.done()
}

// Reboot is the REBOOT body.
type Reboot struct{ Mode RebootMode }

func (m Reboot) AppendBinary(dst []byte) []byte { return append(dst, uint8(m.Mode)) }
func (m *Reboot) UnmarshalBinary(b []byte) error {
	r := reader{b: b}
	m.Mode = RebootMode(r.u8())
	return r.done()
}

func b2u(b bool) uint8 {
	if b {
		return 1
	}
	return 0
}

// ---- responses / events ---------------------------------------------------

// DeviceInfo is the RSP_OK body for HELLO (42 bytes).
type DeviceInfo struct {
	ProtoVer     uint8
	FwVersion    uint32
	FwFlags      uint8
	FwBuildID    [8]byte
	MAC          [6]byte
	Caps         Caps
	MaxFrame     uint16
	MaxRules     uint16
	DeviceTimeUS uint64
	HostNonce    uint32
	ResetReason  ResetReason // optional trailing (rev 2)
	StateFlags   uint8       // optional trailing (rev 2)
}

// DeviceInfoLen is the encoded size.
const DeviceInfoLen = 42

// Firmware flag bits (DeviceInfo.FwFlags).
const (
	FwFlagDirty       uint8 = 1 << 0
	FwFlagIDFMismatch uint8 = 1 << 1
	FwFlagDev         uint8 = 1 << 2
)

func (m DeviceInfo) AppendBinary(dst []byte) []byte {
	dst = append(dst, m.ProtoVer)
	dst = le.AppendUint32(dst, m.FwVersion)
	dst = append(dst, m.FwFlags)
	dst = append(dst, m.FwBuildID[:]...)
	dst = append(dst, m.MAC[:]...)
	dst = le.AppendUint32(dst, uint32(m.Caps))
	dst = le.AppendUint16(dst, m.MaxFrame)
	dst = le.AppendUint16(dst, m.MaxRules)
	dst = le.AppendUint64(dst, m.DeviceTimeUS)
	dst = le.AppendUint32(dst, m.HostNonce)
	return append(dst, uint8(m.ResetReason), m.StateFlags)
}

func (m *DeviceInfo) UnmarshalBinary(b []byte) error {
	r := reader{b: b}
	m.ProtoVer = r.u8()
	m.FwVersion = r.u32()
	m.FwFlags = r.u8()
	copy(m.FwBuildID[:], r.take(8))
	copy(m.MAC[:], r.take(6))
	m.Caps = Caps(r.u32())
	m.MaxFrame, m.MaxRules = r.u16(), r.u16()
	m.DeviceTimeUS = r.u64()
	m.HostNonce = r.u32()
	if err := r.done(); err != nil {
		return err
	}
	m.ResetReason, m.StateFlags = 0, 0
	if r.left() >= 2 {
		m.ResetReason, m.StateFlags = ResetReason(r.u8()), r.u8()
	}
	return nil
}

// Config is the CONFIG struct (26 bytes).
type Config struct {
	CfgGen     uint8
	Channel    uint8
	BW         Bandwidth
	Band       Band
	Country    Country
	Snaplen    uint16
	TypeMask   uint32
	CtrlMask   uint32
	CaptureOn  bool
	NRules     uint8
	RulesCRC32 uint32
}

// ConfigLen is the encoded size.
const ConfigLen = 26

func (m Config) AppendBinary(dst []byte) []byte {
	dst = append(dst, m.CfgGen, m.Channel, uint8(m.BW), uint8(m.Band))
	dst = m.Country.AppendBinary(dst)
	dst = le.AppendUint16(dst, m.Snaplen)
	dst = le.AppendUint32(dst, m.TypeMask)
	dst = le.AppendUint32(dst, m.CtrlMask)
	dst = append(dst, b2u(m.CaptureOn), m.NRules)
	return le.AppendUint32(dst, m.RulesCRC32)
}

func (m *Config) UnmarshalBinary(b []byte) error {
	r := reader{b: b}
	m.CfgGen, m.Channel, m.BW, m.Band = r.u8(), r.u8(), Bandwidth(r.u8()), Band(r.u8())
	copy(m.Country.CC[:], r.take(3))
	m.Country.Mode, m.Country.SChan, m.Country.NChan = CountryMode(r.u8()), r.u8(), r.u8()
	m.Snaplen = r.u16()
	m.TypeMask, m.CtrlMask = r.u32(), r.u32()
	m.CaptureOn = r.u8() != 0
	m.NRules = r.u8()
	m.RulesCRC32 = r.u32()
	return r.done()
}

// SameSettings reports whether two configs hold the same settings (ignoring CfgGen).
func (m Config) SameSettings(o Config) bool {
	m.CfgGen, o.CfgGen = 0, 0
	return m == o
}

// BootConfig is the device configuration after boot (docs/03 §6.1).
func BootConfig() Config {
	return Config{
		Channel: 1, BW: BW20, Band: Band2G4,
		Country:  Country{CC: [3]byte{'0', '1', 0}, Mode: CountryValidated, SChan: 1, NChan: 11},
		TypeMask: DefaultTypeMask, CtrlMask: DefaultCtrlMask,
		RulesCRC32: EmptyFilterCRC,
	}
}

// RspErr is the RSP_ERR body.
type RspErr struct {
	Code   ErrCode
	EspErr int32
	Detail string
}

func (m RspErr) AppendBinary(dst []byte) []byte {
	dst = le.AppendUint16(dst, uint16(m.Code))
	dst = le.AppendUint32(dst, uint32(m.EspErr))
	return appendStr8(dst, m.Detail, MaxDetail)
}

func (m *RspErr) UnmarshalBinary(b []byte) error {
	r := reader{b: b}
	m.Code, m.EspErr = ErrCode(r.u16()), r.i32()
	if err := r.done(); err != nil {
		return err
	}
	m.Detail = ""
	if r.left() >= 1 {
		n := int(r.u8())
		if n > r.left() {
			n = r.left()
		}
		m.Detail = string(r.take(n))
	}
	return nil
}

func (m RspErr) Error() string {
	s := m.Code.String()
	if m.EspErr != 0 {
		s += " (" + EspErrName(m.EspErr) + ")"
	}
	if m.Detail != "" {
		s += ": " + m.Detail
	}
	return s
}

// Stats is the EVT_STATS / GET_STATS body (48 bytes).
type Stats struct {
	DeviceTimeUS   uint64
	RxTotal        uint32
	RxPassed       uint32
	DropRing       uint32
	DropFCS        uint32
	TxFrames       uint32
	TxBytes        uint32
	HostRxCRCErr   uint32
	HostRxOversize uint32
	RingHighWater  uint16
	Channel        uint8
	NoiseFloor     int8
	FlashWrites    uint32 // optional trailing (rev 2)
}

// StatsLen is the encoded size.
const StatsLen = 48

func (m Stats) AppendBinary(dst []byte) []byte {
	dst = le.AppendUint64(dst, m.DeviceTimeUS)
	for _, v := range []uint32{m.RxTotal, m.RxPassed, m.DropRing, m.DropFCS, m.TxFrames, m.TxBytes, m.HostRxCRCErr, m.HostRxOversize} {
		dst = le.AppendUint32(dst, v)
	}
	dst = le.AppendUint16(dst, m.RingHighWater)
	dst = append(dst, m.Channel, uint8(m.NoiseFloor))
	return le.AppendUint32(dst, m.FlashWrites)
}

func (m *Stats) UnmarshalBinary(b []byte) error {
	r := reader{b: b}
	m.DeviceTimeUS = r.u64()
	m.RxTotal, m.RxPassed, m.DropRing, m.DropFCS = r.u32(), r.u32(), r.u32(), r.u32()
	m.TxFrames, m.TxBytes, m.HostRxCRCErr, m.HostRxOversize = r.u32(), r.u32(), r.u32(), r.u32()
	m.RingHighWater = r.u16()
	m.Channel, m.NoiseFloor = r.u8(), r.i8()
	if err := r.done(); err != nil {
		return err
	}
	m.FlashWrites = 0
	if r.left() >= 4 {
		m.FlashWrites = r.u32()
	}
	return nil
}

// Log is the EVT_LOG body.
type Log struct {
	Level uint8
	Text  string
}

func (m Log) AppendBinary(dst []byte) []byte {
	t := m.Text
	if len(t) > MaxLogText {
		t = t[:MaxLogText]
	}
	return append(append(dst, m.Level), t...)
}
func (m *Log) UnmarshalBinary(b []byte) error {
	r := reader{b: b}
	m.Level = r.u8()
	if err := r.done(); err != nil {
		return err
	}
	m.Text = string(b[1:])
	return nil
}

// Reset is the EVT_RESET body.
type Reset struct {
	Reason     ResetReason
	FwVersion  uint32
	StateFlags uint8  // optional trailing
	Detail     string // optional trailing
}

func (m Reset) AppendBinary(dst []byte) []byte {
	dst = append(dst, uint8(m.Reason))
	dst = le.AppendUint32(dst, m.FwVersion)
	dst = append(dst, m.StateFlags)
	return appendStr8(dst, m.Detail, MaxDetail)
}
func (m *Reset) UnmarshalBinary(b []byte) error {
	r := reader{b: b}
	m.Reason, m.FwVersion = ResetReason(r.u8()), r.u32()
	if err := r.done(); err != nil {
		return err
	}
	m.StateFlags, m.Detail = 0, ""
	if r.left() >= 1 {
		m.StateFlags = r.u8()
	}
	if r.left() >= 1 {
		n := int(r.u8())
		if n > r.left() {
			n = r.left()
		}
		m.Detail = string(r.take(n))
	}
	return nil
}

// PktMeta is the fixed 20-byte PKT header (docs/03 §9).
type PktMeta struct {
	TsUS     uint64
	Channel  uint8
	Flags    uint8
	RSSI     int8
	Noise    int8
	Rate     uint8
	BBFormat uint8
	OrigLen  uint16
	CapLen   uint16
	RxSeq    uint16
}

// PktMetaLen is the encoded size.
const PktMetaLen = 20

func (m PktMeta) AppendBinary(dst []byte) []byte {
	dst = le.AppendUint64(dst, m.TsUS)
	dst = append(dst, m.Channel, m.Flags, uint8(m.RSSI), uint8(m.Noise), m.Rate, m.BBFormat)
	dst = le.AppendUint16(dst, m.OrigLen)
	dst = le.AppendUint16(dst, m.CapLen)
	return le.AppendUint16(dst, m.RxSeq)
}

func (m *PktMeta) UnmarshalBinary(b []byte) error {
	r := reader{b: b}
	m.TsUS = r.u64()
	m.Channel, m.Flags, m.RSSI, m.Noise, m.Rate, m.BBFormat = r.u8(), r.u8(), r.i8(), r.i8(), r.u8(), r.u8()
	m.OrigLen, m.CapLen, m.RxSeq = r.u16(), r.u16(), r.u16()
	return r.done()
}

// DecodePkt splits a PKT body into meta and data. Data aliases b.
func DecodePkt(b []byte) (PktMeta, []byte, error) {
	var m PktMeta
	if err := m.UnmarshalBinary(b); err != nil {
		return m, nil, err
	}
	if int(m.CapLen) > len(b)-PktMetaLen {
		return m, nil, ErrShortBody
	}
	return m, b[PktMetaLen : PktMetaLen+int(m.CapLen)], nil
}

// LegacyRateMbps500 converts a 500 kb/s rate unit value to Mb/s×10 for display.
func LegacyRateKbps(rate500k uint8) int { return int(rate500k) * 500 }
