// Package codec implements CTP framing (docs/03 §3): COBS, CRC-32 and a
// streaming, allocation-free decoder with the resync rule.
package codec

import (
	"encoding/binary"
	"errors"
	"hash/crc32"

	"github.com/hjkoskel/capturetonkeli/ctp/wire"
)

// MaxFrame is the largest decoded frame (header + body + crc).
const MaxFrame = wire.MaxFrame

// ErrCOBS reports an invalid COBS encoding.
var ErrCOBS = errors.New("codec: invalid COBS data")

// CRC32 is CRC-32 IEEE (reflected, zlib/Ethernet).
func CRC32(b []byte) uint32 { return crc32.ChecksumIEEE(b) }

// MaxEncodedLen returns the worst-case COBS length of n input bytes (no delimiter).
func MaxEncodedLen(n int) int { return n + n/254 + 2 }

// COBSEncode appends COBS(src) to dst (no trailing delimiter).
func COBSEncode(dst, src []byte) []byte {
	codeIdx := len(dst)
	dst = append(dst, 0)
	code := byte(1)
	for _, b := range src {
		if b == 0 {
			dst[codeIdx] = code
			codeIdx = len(dst)
			dst = append(dst, 0)
			code = 1
			continue
		}
		dst = append(dst, b)
		code++
		if code == 0xFF {
			dst[codeIdx] = code
			codeIdx = len(dst)
			dst = append(dst, 0)
			code = 1
		}
	}
	dst[codeIdx] = code
	return dst
}

// COBSDecode appends the decoding of one COBS frame (without delimiter) to dst.
func COBSDecode(dst, src []byte) ([]byte, error) {
	i := 0
	for i < len(src) {
		code := src[i]
		if code == 0 {
			return dst, ErrCOBS
		}
		i++
		n := int(code) - 1
		if i+n > len(src) {
			return dst, ErrCOBS
		}
		for _, b := range src[i : i+n] {
			if b == 0 {
				return dst, ErrCOBS
			}
		}
		dst = append(dst, src[i:i+n]...)
		i += n
		if code != 0xFF && i < len(src) {
			dst = append(dst, 0)
		}
	}
	return dst, nil
}

// EncodeFrame appends COBS(header|body|crc32) followed by 0x00 to dst.
func EncodeFrame(dst []byte, h wire.Header, body []byte) []byte {
	var raw [MaxFrame + 16]byte
	f := h.AppendBinary(raw[:0])
	f = append(f, body...)
	f = binary.LittleEndian.AppendUint32(f, CRC32(f))
	dst = COBSEncode(dst, f)
	return append(dst, 0)
}

// DecoderStats counts decoder events.
type DecoderStats struct {
	Frames       uint64 `json:"frames"`
	CRCErrors    uint64 `json:"crc_errors"`
	COBSErrors   uint64 `json:"cobs_errors"`
	Oversize     uint64 `json:"oversize"`
	Short        uint64 `json:"short"`
	SkippedBytes uint64 `json:"skipped_bytes"`
	Resyncs      uint64 `json:"resyncs"`
}

// Decoder is a streaming frame decoder. It discards bytes until the first
// 0x00 (resync rule), then decodes delimiter-terminated frames.
type Decoder struct {
	maxFrame int
	maxEnc   int
	enc      []byte
	dec      []byte
	synced   bool
	overrun  bool
	st       DecoderStats
	// RawFn, if set, is called for every delimited chunk with its decode status
	// (used by the bench's raw stream and tracing). ok=false for bad frames.
	RawFn func(raw []byte, ok bool)
}

// NewDecoder returns a decoder for decoded frames up to maxFrame bytes.
func NewDecoder(maxFrame int) *Decoder {
	if maxFrame <= 0 {
		maxFrame = MaxFrame
	}
	me := MaxEncodedLen(maxFrame)
	return &Decoder{maxFrame: maxFrame, maxEnc: me, enc: make([]byte, 0, me), dec: make([]byte, 0, maxFrame+8)}
}

// Synced reports whether the decoder has seen its first delimiter.
func (d *Decoder) Synced() bool { return d.synced }

// SetSynced marks the stream as starting on a frame boundary (e.g. a freshly
// opened in-process pipe). Normal serial use leaves this false.
func (d *Decoder) SetSynced() { d.synced = true }

// Reset drops partial state and requires a fresh delimiter.
func (d *Decoder) Reset() {
	d.enc = d.enc[:0]
	d.synced = false
	d.overrun = false
	d.st.Resyncs++
}

// Stats returns a copy of the counters.
func (d *Decoder) Stats() DecoderStats { return d.st }

// Feed consumes bytes and calls fn for each valid frame. body is valid only
// during the callback.
func (d *Decoder) Feed(p []byte, fn func(h wire.Header, body []byte)) {
	for _, b := range p {
		if !d.synced {
			if b == 0 {
				d.synced = true
			} else {
				d.st.SkippedBytes++
			}
			continue
		}
		if b != 0 {
			if d.overrun {
				continue
			}
			if len(d.enc) >= d.maxEnc {
				d.overrun = true
				d.st.Oversize++
				continue
			}
			d.enc = append(d.enc, b)
			continue
		}
		// delimiter
		if d.overrun {
			d.overrun = false
			d.enc = d.enc[:0]
			continue
		}
		if len(d.enc) == 0 {
			continue // empty frame ignored
		}
		d.handle(fn)
		d.enc = d.enc[:0]
	}
}

func (d *Decoder) handle(fn func(h wire.Header, body []byte)) {
	var err error
	d.dec, err = COBSDecode(d.dec[:0], d.enc)
	if err != nil {
		d.st.COBSErrors++
		d.raw(false)
		return
	}
	if len(d.dec) > d.maxFrame {
		d.st.Oversize++
		d.raw(false)
		return
	}
	if len(d.dec) < wire.HeaderLen+4 {
		d.st.Short++
		d.raw(false)
		return
	}
	n := len(d.dec) - 4
	if CRC32(d.dec[:n]) != binary.LittleEndian.Uint32(d.dec[n:]) {
		d.st.CRCErrors++
		d.raw(false)
		return
	}
	var h wire.Header
	_ = h.UnmarshalBinary(d.dec)
	d.st.Frames++
	d.raw(true)
	if fn != nil {
		fn(h, d.dec[wire.HeaderLen:n])
	}
}

func (d *Decoder) raw(ok bool) {
	if d.RawFn != nil {
		d.RawFn(d.enc, ok)
	}
}
