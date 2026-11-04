// Package wire defines the CTP v1 message set (docs/03 §4, §6, §6.1, §9):
// header, message type IDs, error codes and one struct per message body.
//
// Every body struct has AppendBinary (encode) and UnmarshalBinary (decode).
// Decoders accept longer bodies (trailing bytes ignored), accept shorter
// bodies when only optional trailing fields are missing, and return
// ErrShortBody when a mandatory field is truncated (docs/03 §11).
package wire

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// ProtoVer is the protocol version carried in every header (docs/03 §4).
const ProtoVer = 1

// HeaderLen is the size of the common frame header.
const HeaderLen = 8

// MaxCapLen is the largest PKT data length (docs/03 §9).
const MaxCapLen = 4096

// MaxFrame is the largest decoded frame (header+body+crc) (docs/03 §3).
const MaxFrame = 4096 + 32

// MaxRules is the device rule limit reported in HELLO.
const MaxRules = 16

// MaxDetail is the longest RSP_ERR / EVT_RESET detail string.
const MaxDetail = 64

// MaxLogText is the longest EVT_LOG text.
const MaxLogText = 200

// Message types (docs/03 §6).
const (
	TypeHello         uint8 = 0x01
	TypePing          uint8 = 0x02
	TypeGetConfig     uint8 = 0x10
	TypeSetChannel    uint8 = 0x11
	TypeSetCountry    uint8 = 0x12
	TypeSetFilter     uint8 = 0x13
	TypeSetSnaplen    uint8 = 0x14
	TypeSetDriverMask uint8 = 0x15
	TypeCapture       uint8 = 0x20
	TypeGetStats      uint8 = 0x21
	TypeReboot        uint8 = 0x30

	TypeRspOK    uint8 = 0x81
	TypeRspErr   uint8 = 0x82
	TypeEvtStats uint8 = 0x90
	TypeEvtLog   uint8 = 0x91
	TypeEvtReset uint8 = 0x92
	TypePkt      uint8 = 0xA0
)

// TypeName returns the doc 03 name of a message type.
func TypeName(t uint8) string {
	switch t {
	case TypeHello:
		return "HELLO"
	case TypePing:
		return "PING"
	case TypeGetConfig:
		return "GET_CONFIG"
	case TypeSetChannel:
		return "SET_CHANNEL"
	case TypeSetCountry:
		return "SET_COUNTRY"
	case TypeSetFilter:
		return "SET_FILTER"
	case TypeSetSnaplen:
		return "SET_SNAPLEN"
	case TypeSetDriverMask:
		return "SET_DRIVER_MASK"
	case TypeCapture:
		return "CAPTURE"
	case TypeGetStats:
		return "GET_STATS"
	case TypeReboot:
		return "REBOOT"
	case TypeRspOK:
		return "RSP_OK"
	case TypeRspErr:
		return "RSP_ERR"
	case TypeEvtStats:
		return "EVT_STATS"
	case TypeEvtLog:
		return "EVT_LOG"
	case TypeEvtReset:
		return "EVT_RESET"
	case TypePkt:
		return "PKT"
	}
	return fmt.Sprintf("0x%02X", t)
}

// Header flag bits.
const (
	FlagMore       uint8 = 1 << 0
	FlagCfgChanged uint8 = 1 << 1
)

// Header is the 8-byte common header (docs/03 §4).
type Header struct {
	Ver, Type  uint8
	Seq, ReqID uint16
	Flags      uint8
	CfgGen     uint8
}

// AppendBinary appends the 8 header bytes.
func (h Header) AppendBinary(dst []byte) []byte {
	dst = append(dst, h.Ver, h.Type)
	dst = binary.LittleEndian.AppendUint16(dst, h.Seq)
	dst = binary.LittleEndian.AppendUint16(dst, h.ReqID)
	return append(dst, h.Flags, h.CfgGen)
}

// UnmarshalBinary decodes the first 8 bytes.
func (h *Header) UnmarshalBinary(b []byte) error {
	if len(b) < HeaderLen {
		return ErrShortBody
	}
	h.Ver, h.Type = b[0], b[1]
	h.Seq = binary.LittleEndian.Uint16(b[2:])
	h.ReqID = binary.LittleEndian.Uint16(b[4:])
	h.Flags, h.CfgGen = b[6], b[7]
	return nil
}

// ErrShortBody is returned when a mandatory field is truncated.
var ErrShortBody = errors.New("ctp: body too short")

// Bandwidth (SET_CHANNEL bw).
type Bandwidth uint8

const (
	BW20      Bandwidth = 0
	BW40Plus  Bandwidth = 1
	BW40Minus Bandwidth = 2
)

func (b Bandwidth) String() string {
	switch b {
	case BW20:
		return "20"
	case BW40Plus:
		return "40+"
	case BW40Minus:
		return "40-"
	}
	return fmt.Sprintf("bw%d", uint8(b))
}

// ParseBandwidth accepts "20", "40+", "40-".
func ParseBandwidth(s string) (Bandwidth, error) {
	switch s {
	case "20", "HT20", "ht20":
		return BW20, nil
	case "40+", "HT40+", "ht40+":
		return BW40Plus, nil
	case "40-", "HT40-", "ht40-":
		return BW40Minus, nil
	}
	return 0, fmt.Errorf("unknown bandwidth %q (want 20, 40+ or 40-)", s)
}

// Band of the configured channel.
type Band uint8

const (
	Band2G4 Band = 0
	Band5G  Band = 1
)

// CountryMode (SET_COUNTRY mode).
type CountryMode uint8

const (
	CountryValidated CountryMode = 0
	CountryManual    CountryMode = 1
)

// RebootMode (REBOOT mode).
type RebootMode uint8

const (
	RebootNormal   RebootMode = 0
	RebootDownload RebootMode = 1
)

// Caps bits (HELLO caps).
type Caps uint32

const (
	Cap5GHz          Caps = 1 << 0
	CapHT40          Caps = 1 << 1
	CapManualCountry Caps = 1 << 2
	CapInjector      Caps = 1 << 31
)

// Names returns the set cap names.
func (c Caps) Names() []string {
	var out []string
	if c&Cap5GHz != 0 {
		out = append(out, "5GHZ")
	}
	if c&CapHT40 != 0 {
		out = append(out, "HT40")
	}
	if c&CapManualCountry != 0 {
		out = append(out, "MANUAL_COUNTRY")
	}
	if c&CapInjector != 0 {
		out = append(out, "INJECTOR")
	}
	return out
}

// State flags (DeviceInfo / EVT_RESET).
const StateSafeMode uint8 = 1 << 0

// PKT flags (docs/03 §9).
const (
	PktFCSErr      uint8 = 1 << 0
	PktTruncated   uint8 = 1 << 1
	Pkt5GHz        uint8 = 1 << 2
	PktHT40        uint8 = 1 << 3
	PktAMPDU       uint8 = 1 << 4
	PktRateUnknown uint8 = 1 << 5
	PktNoFCS       uint8 = 1 << 6 // data/orig_len exclude the FCS (docs/03 §9 rev 3)
)

// bb_format values (C5 rx_ctrl.cur_bb_format).
const (
	BB11b     uint8 = 0
	BB11g     uint8 = 1
	BBHT      uint8 = 2
	BBVHT     uint8 = 3
	BBHESU    uint8 = 4
	BBHEMU    uint8 = 5
	BBHEERSU  uint8 = 6
	BBHETB    uint8 = 7
	BBVHTMU   uint8 = 11
	BBUnknown uint8 = 0xFF
)

// BBFormatName returns a short name for a bb_format value.
func BBFormatName(f uint8) string {
	switch f {
	case BB11b:
		return "11b"
	case BB11g:
		return "11ag"
	case BBHT:
		return "HT"
	case BBVHT:
		return "VHT"
	case BBHESU:
		return "HE-SU"
	case BBHEMU:
		return "HE-MU"
	case BBHEERSU:
		return "HE-ER-SU"
	case BBHETB:
		return "HE-TB"
	case BBVHTMU:
		return "VHT-MU"
	}
	return "unknown"
}

// Driver mask defaults (ESP-IDF WIFI_PROMIS_FILTER_MASK_ALL / CTRL_FILTER_MASK_ALL).
const (
	DefaultTypeMask uint32 = 0xFFFFFFFF
	DefaultCtrlMask uint32 = 0xFF800000
)

// EmptyFilterCRC is the CRC-32 of the 8-byte empty SET_FILTER body.
const EmptyFilterCRC uint32 = 0x6522DF69

// ChannelFreqMHz maps a channel number to its centre frequency (docs/04 App. B).
func ChannelFreqMHz(ch uint8) (mhz int, band Band, ok bool) {
	switch {
	case ch >= 1 && ch <= 13:
		return 2407 + 5*int(ch), Band2G4, true
	case ch == 14:
		return 2484, Band2G4, true
	case ch >= 32 && ch <= 177:
		return 5000 + 5*int(ch), Band5G, true
	}
	return 0, 0, false
}

// FreqChannel is the inverse of ChannelFreqMHz.
func FreqChannel(mhz int) (uint8, bool) {
	switch {
	case mhz == 2484:
		return 14, true
	case mhz >= 2412 && mhz <= 2472 && (mhz-2407)%5 == 0:
		return uint8((mhz - 2407) / 5), true
	case mhz >= 5160 && mhz <= 5885 && mhz%5 == 0:
		return uint8((mhz - 5000) / 5), true
	}
	return 0, false
}
