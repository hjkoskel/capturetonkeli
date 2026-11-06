// Package dot11 is a minimal 802.11 header / IE decoder (docs/05 §7):
// frame control, type/subtype names, addresses, seq/frag, QoS and the IEs of
// management frames. It never panics on truncated input.
package dot11

import (
	"fmt"
	"net"
)

// Header is the decoded MAC header.
type Header struct {
	Type, Subtype uint8
	Name          string
	ToDS, FromDS  bool
	Retry         bool
	Protected     bool
	MoreData      bool
	Duration      uint16
	Addr1, Addr2  net.HardwareAddr // receiver, transmitter
	Addr3, Addr4  net.HardwareAddr
	Seq, Frag     uint16
	HasSeq        bool
	QoS           bool
	TID           uint8
	HeaderLen     int // bytes of MAC header present (0 if truncated)
	Truncated     bool
	body          []byte
}

var names = map[[2]uint8]string{
	{0, 0}: "Assoc Request", {0, 1}: "Assoc Response", {0, 2}: "Reassoc Request", {0, 3}: "Reassoc Response",
	{0, 4}: "Probe Request", {0, 5}: "Probe Response", {0, 6}: "Timing Advert", {0, 8}: "Beacon", {0, 9}: "ATIM",
	{0, 10}: "Disassoc", {0, 11}: "Auth", {0, 12}: "Deauth", {0, 13}: "Action", {0, 14}: "Action No Ack",
	{1, 2}: "Trigger", {1, 4}: "Beamforming Poll", {1, 5}: "NDP Announcement", {1, 7}: "Control Wrapper",
	{1, 8}: "Block Ack Request", {1, 9}: "Block Ack", {1, 10}: "PS-Poll", {1, 11}: "RTS", {1, 12}: "CTS",
	{1, 13}: "ACK", {1, 14}: "CF-End", {1, 15}: "CF-End+CF-Ack",
	{2, 0}: "Data", {2, 4}: "Null", {2, 8}: "QoS Data", {2, 12}: "QoS Null",
	{3, 0}: "DMG Beacon",
}

// TypeName returns the type/subtype name ("Beacon", "QoS Data", "t3/s5").
func TypeName(t, s uint8) string {
	if n, ok := names[[2]uint8{t, s}]; ok {
		return n
	}
	return fmt.Sprintf("t%d/s%d", t, s)
}

func mac(b []byte, off int) net.HardwareAddr {
	if off+6 > len(b) {
		return nil
	}
	return net.HardwareAddr(append([]byte(nil), b[off:off+6]...))
}

// Parse decodes the header of frame b (FC byte 0 onward).
func Parse(b []byte) Header {
	var h Header
	if len(b) < 2 {
		h.Truncated = true
		h.Name = "runt"
		return h
	}
	fc0, fc1 := b[0], b[1]
	h.Type, h.Subtype = (fc0>>2)&3, fc0>>4
	h.Name = TypeName(h.Type, h.Subtype)
	h.ToDS, h.FromDS = fc1&1 != 0, fc1&2 != 0
	h.Retry, h.MoreData, h.Protected = fc1&0x08 != 0, fc1&0x20 != 0, fc1&0x40 != 0
	if len(b) >= 4 {
		h.Duration = uint16(b[2]) | uint16(b[3])<<8
	}
	h.Addr1 = mac(b, 4)
	need := 10
	switch h.Type {
	case 1: // control: addr2 for RTS, PS-Poll, BA/BAR, CF-End, beamforming, trigger
		switch h.Subtype {
		case 2, 4, 5, 8, 9, 10, 11, 14, 15:
			h.Addr2 = mac(b, 10)
			need = 16
		}
	default:
		h.Addr2, h.Addr3 = mac(b, 10), mac(b, 16)
		need = 24
		if len(b) >= 24 {
			sc := uint16(b[22]) | uint16(b[23])<<8
			h.Seq, h.Frag, h.HasSeq = sc>>4, sc&0xF, true
		}
		if h.Type == 2 && h.ToDS && h.FromDS {
			h.Addr4 = mac(b, 24)
			need = 30
		}
		if h.Type == 2 && h.Subtype&0x8 != 0 {
			h.QoS = true
			if len(b) >= need+2 {
				h.TID = b[need] & 0x0F
			}
			need += 2
		}
	}
	if len(b) < need {
		h.Truncated = true
		return h
	}
	h.HeaderLen = need
	h.body = b[need:]
	return h
}

// IE is one information element.
type IE struct {
	ID   uint8
	Data []byte
}

// IEs returns the information elements of a management frame body
// (skipping the fixed fields of beacons/probe responses/assoc frames).
func (h Header) IEs() []IE {
	if h.Type != 0 || h.Protected {
		return nil
	}
	b := h.body
	switch h.Subtype {
	case 8, 5: // beacon, probe resp: timestamp(8) interval(2) capability(2)
		b = skip(b, 12)
	case 0: // assoc req: capability(2) listen interval(2)
		b = skip(b, 4)
	case 1, 3: // assoc/reassoc resp: capability, status, AID
		b = skip(b, 6)
	case 2: // reassoc req: capability, listen interval, current AP
		b = skip(b, 10)
	case 4: // probe request: IEs only
	default:
		return nil
	}
	var out []IE
	for len(b) >= 2 {
		n := int(b[1])
		if 2+n > len(b) {
			break // truncated IE
		}
		out = append(out, IE{ID: b[0], Data: b[2 : 2+n]})
		b = b[2+n:]
	}
	return out
}

func skip(b []byte, n int) []byte {
	if len(b) < n {
		return nil
	}
	return b[n:]
}

// SSID returns the SSID IE (ok=false if absent).
func (h Header) SSID() (string, bool) {
	for _, ie := range h.IEs() {
		if ie.ID == 0 {
			return string(ie.Data), true
		}
	}
	return "", false
}

// DSChannel returns the DS Parameter Set channel.
func (h Header) DSChannel() (uint8, bool) {
	for _, ie := range h.IEs() {
		if ie.ID == 3 && len(ie.Data) == 1 {
			return ie.Data[0], true
		}
	}
	return 0, false
}

// HasRSN reports an RSN IE (WPA2/3).
func (h Header) HasRSN() bool {
	for _, ie := range h.IEs() {
		if ie.ID == 48 {
			return true
		}
	}
	return false
}

// BSSID returns addr3 for mgmt frames, the AP side address for data frames.
func (h Header) BSSID() net.HardwareAddr {
	switch {
	case h.Type == 0:
		return h.Addr3
	case h.Type == 2 && h.ToDS && !h.FromDS:
		return h.Addr1
	case h.Type == 2 && !h.ToDS && h.FromDS:
		return h.Addr2
	case h.Type == 2 && !h.ToDS && !h.FromDS:
		return h.Addr3
	}
	return nil
}
