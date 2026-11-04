// Package pcapng writes (and minimally reads) pcapng files with
// IEEE 802.11 + radiotap (LINKTYPE 127), docs/05 §7 and docs/04 §10.
// The byte layout is shared with the web UI writer via testvectors/pcapng.json.
package pcapng

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	"github.com/hjkoskel/capturetonkeli/ctp/wire"
)

// Block types.
const (
	BlockSHB uint32 = 0x0A0D0D0A
	BlockIDB uint32 = 0x00000001
	BlockEPB uint32 = 0x00000006

	byteOrderMagic   = 0x1A2B3C4D
	LinkTypeRadiotap = 127
	optEnd           = 0
	optComment       = 1
	optSHBHardware   = 2
	optSHBUserAppl   = 4
	optIfTsresol     = 9
	optIfName        = 2
)

// WriterOptions describe the capture.
type WriterOptions struct {
	UserAppl string // shb_userappl, e.g. "capturetonkeli ctpbench v1"
	Hardware string // shb_hardware, e.g. "XIAO ESP32-C5 fw 0.1.0"
	IfName   string
	Snaplen  uint32 // IDB snaplen; 0 = 65535
}

// Writer emits a pcapng stream.
type Writer struct {
	w   io.Writer
	buf []byte
}

var le = binary.LittleEndian

func pad4(n int) int { return (4 - n%4) % 4 }

func appendOpt(b []byte, code uint16, val []byte) []byte {
	b = le.AppendUint16(b, code)
	b = le.AppendUint16(b, uint16(len(val)))
	b = append(b, val...)
	return append(b, make([]byte, pad4(len(val)))...)
}

func block(typ uint32, body []byte) []byte {
	total := uint32(12 + len(body))
	b := le.AppendUint32(nil, typ)
	b = le.AppendUint32(b, total)
	b = append(b, body...)
	return le.AppendUint32(b, total)
}

// NewWriter writes the SHB and one IDB.
func NewWriter(w io.Writer, o WriterOptions) (*Writer, error) {
	var shb []byte
	shb = le.AppendUint32(shb, byteOrderMagic)
	shb = le.AppendUint16(shb, 1)
	shb = le.AppendUint16(shb, 0)
	shb = le.AppendUint64(shb, 0xFFFFFFFFFFFFFFFF) // section length unknown
	if o.Hardware != "" {
		shb = appendOpt(shb, optSHBHardware, []byte(o.Hardware))
	}
	if o.UserAppl != "" {
		shb = appendOpt(shb, optSHBUserAppl, []byte(o.UserAppl))
	}
	shb = appendOpt(shb, optEnd, nil)
	snap := o.Snaplen
	if snap == 0 {
		snap = 65535
	}
	var idb []byte
	idb = le.AppendUint16(idb, LinkTypeRadiotap)
	idb = le.AppendUint16(idb, 0)
	idb = le.AppendUint32(idb, snap)
	if o.IfName != "" {
		idb = appendOpt(idb, optIfName, []byte(o.IfName))
	}
	idb = appendOpt(idb, optIfTsresol, []byte{6}) // microseconds
	idb = appendOpt(idb, optEnd, nil)
	out := append(block(BlockSHB, shb), block(BlockIDB, idb)...)
	if _, err := w.Write(out); err != nil {
		return nil, err
	}
	return &Writer{w: w}, nil
}

// Radiotap present bits used.
const (
	rtTSFT    = 1 << 0
	rtFlags   = 1 << 1
	rtRate    = 1 << 2
	rtChannel = 1 << 3
	rtSignal  = 1 << 5
	rtNoise   = 1 << 6
	rtMCS     = 1 << 19

	rtFlagFCS    = 0x10
	rtFlagBadFCS = 0x40

	chanCCK  = 0x0020
	chanOFDM = 0x0040
	chan2G   = 0x0080
	chan5G   = 0x0100
)

// Radiotap builds the radiotap header for one packet's metadata.
func Radiotap(m wire.PktMeta) []byte {
	present := uint32(rtTSFT | rtFlags | rtChannel | rtSignal | rtNoise)
	legacy := m.Flags&wire.PktRateUnknown == 0 && (m.BBFormat == wire.BB11b || m.BBFormat == wire.BB11g)
	mcs := m.Flags&wire.PktRateUnknown == 0 && m.BBFormat == wire.BBHT
	if legacy {
		present |= rtRate
	}
	if mcs {
		present |= rtMCS
	}
	b := []byte{0, 0, 0, 0} // version, pad, length (patched)
	b = le.AppendUint32(b, present)
	b = le.AppendUint64(b, m.TsUS) // offset 8: 8-byte aligned
	var fl uint8
	if m.Flags&wire.PktNoFCS == 0 && m.CapLen == m.OrigLen && m.CapLen >= 4 {
		fl |= rtFlagFCS
	}
	if m.Flags&wire.PktFCSErr != 0 {
		fl |= rtFlagBadFCS
	}
	b = append(b, fl)
	if legacy {
		b = append(b, m.Rate)
	}
	if len(b)%2 != 0 {
		b = append(b, 0) // channel is 2-byte aligned
	}
	mhz, band, _ := wire.ChannelFreqMHz(m.Channel)
	var cf uint16
	if band == wire.Band5G {
		cf = chan5G | chanOFDM
	} else {
		cf = chan2G
		if m.BBFormat == wire.BB11b {
			cf |= chanCCK
		} else {
			cf |= chanOFDM
		}
	}
	b = le.AppendUint16(b, uint16(mhz))
	b = le.AppendUint16(b, cf)
	b = append(b, uint8(m.RSSI), uint8(m.Noise))
	if mcs {
		b = append(b, 0x02 /* known: MCS index */, 0, m.Rate)
	}
	le.PutUint16(b[2:], uint16(len(b)))
	return b
}

// EPB builds one Enhanced Packet Block (radiotap + data).
func EPB(m wire.PktMeta, data []byte, comment string) []byte {
	rt := Radiotap(m)
	pkt := append(rt, data...)
	orig := uint32(len(rt)) + uint32(m.OrigLen)
	var body []byte
	body = le.AppendUint32(body, 0) // interface id
	body = le.AppendUint32(body, uint32(m.TsUS>>32))
	body = le.AppendUint32(body, uint32(m.TsUS))
	body = le.AppendUint32(body, uint32(len(pkt)))
	body = le.AppendUint32(body, orig)
	body = append(body, pkt...)
	body = append(body, make([]byte, pad4(len(pkt)))...)
	if comment != "" {
		body = appendOpt(body, optComment, []byte(comment))
		body = appendOpt(body, optEnd, nil)
	}
	return block(BlockEPB, body)
}

// WritePacket writes one packet; comment may be empty.
func (w *Writer) WritePacket(m wire.PktMeta, data []byte, comment string) error {
	w.buf = EPB(m, data, comment)
	_, err := w.w.Write(w.buf)
	return err
}

// ---- minimal reader -----------------------------------------------------------

// Packet is one EPB as read back.
type Packet struct {
	TsUS     uint64
	CapLen   uint32
	OrigLen  uint32
	Data     []byte // radiotap + 802.11
	Comment  string
	Radiotap []byte
	Frame    []byte
}

// File is a parsed pcapng file.
type File struct {
	LinkType uint16
	TsResol  uint8
	UserAppl string
	Hardware string
	Packets  []Packet
}

// ErrFormat reports malformed input.
var ErrFormat = errors.New("pcapng: malformed file")

// Read parses a little-endian pcapng stream with one interface.
func Read(r io.Reader) (*File, error) {
	all, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	f := &File{TsResol: 6}
	for off := 0; off < len(all); {
		if len(all)-off < 12 {
			return f, fmt.Errorf("%w: truncated block at %d", ErrFormat, off)
		}
		typ, total := le.Uint32(all[off:]), int(le.Uint32(all[off+4:]))
		if total < 12 || total%4 != 0 || off+total > len(all) || le.Uint32(all[off+total-4:]) != uint32(total) {
			return f, fmt.Errorf("%w: bad block length at %d", ErrFormat, off)
		}
		body := all[off+8 : off+total-4]
		switch typ {
		case BlockSHB:
			if len(body) < 16 || le.Uint32(body) != byteOrderMagic {
				return f, fmt.Errorf("%w: bad SHB", ErrFormat)
			}
			opts(body[16:], func(c uint16, v []byte) {
				switch c {
				case optSHBUserAppl:
					f.UserAppl = string(v)
				case optSHBHardware:
					f.Hardware = string(v)
				}
			})
		case BlockIDB:
			if len(body) < 8 {
				return f, fmt.Errorf("%w: bad IDB", ErrFormat)
			}
			f.LinkType = le.Uint16(body)
			opts(body[8:], func(c uint16, v []byte) {
				if c == optIfTsresol && len(v) == 1 {
					f.TsResol = v[0]
				}
			})
		case BlockEPB:
			if len(body) < 20 {
				return f, fmt.Errorf("%w: bad EPB", ErrFormat)
			}
			p := Packet{TsUS: uint64(le.Uint32(body[4:]))<<32 | uint64(le.Uint32(body[8:])), CapLen: le.Uint32(body[12:]), OrigLen: le.Uint32(body[16:])}
			if int(p.CapLen) > len(body)-20 {
				return f, fmt.Errorf("%w: EPB caplen", ErrFormat)
			}
			p.Data = body[20 : 20+p.CapLen]
			if len(p.Data) >= 4 {
				rl := int(le.Uint16(p.Data[2:]))
				if rl <= len(p.Data) {
					p.Radiotap, p.Frame = p.Data[:rl], p.Data[rl:]
				}
			}
			o := 20 + int(p.CapLen) + pad4(int(p.CapLen))
			if o <= len(body) {
				opts(body[o:], func(c uint16, v []byte) {
					if c == optComment {
						p.Comment = string(v)
					}
				})
			}
			f.Packets = append(f.Packets, p)
		}
		off += total
	}
	return f, nil
}

func opts(b []byte, fn func(code uint16, val []byte)) {
	for len(b) >= 4 {
		c, n := le.Uint16(b), int(le.Uint16(b[2:]))
		if c == optEnd || 4+n > len(b) {
			return
		}
		fn(c, b[4:4+n])
		b = b[4+n+pad4(n):]
	}
}
