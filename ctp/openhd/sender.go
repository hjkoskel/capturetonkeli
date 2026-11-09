package openhd

import (
	"math/rand"

	"github.com/hjkoskel/capturetonkeli/ctp/fec"
)

// Sender is one transmitting OpenHD instance, for simulators and tests (port of the
// js/openhd.js Sender). It builds the frames OpenHD would put on the air (without FCS).
type Sender struct {
	K          []byte // beforenm key of this direction
	UID        uint8
	Nonce      uint64
	GenB       bool
	SessionKey []byte

	sessionPacket []byte
	block         map[uint8]uint32
	seq           map[uint8]uint32
	bidx          map[uint16]uint32
}

// NewSender returns an air unit (uid UIDAir) or ground unit sender with a random session key.
// k nil = the default bind phrase's key for uid.
func NewSender(rng *rand.Rand, uid uint8, k []byte, nonce uint64, genB bool) *Sender {
	if k == nil {
		d := DefaultKeys()
		k = d.Air
		if uid == UIDGnd {
			k = d.Gnd
		}
	}
	s := &Sender{K: k, UID: uid, Nonce: nonce, GenB: genB, block: map[uint8]uint32{}, seq: map[uint8]uint32{}, bidx: map[uint16]uint32{}}
	s.SessionKey = make([]byte, 32)
	rng.Read(s.SessionKey)
	n24 := make([]byte, 24)
	rng.Read(n24)
	s.sessionPacket = append(n24, SecretboxSeal(s.SessionKey, n24, k)...)
	return s
}

func (s *Sender) header(port uint8, enc bool) ([]byte, uint64) {
	f := make([]byte, 24)
	n := s.Nonce
	s.Nonce++
	f[0], f[1] = 0x08, 0x01
	for i := 4; i < 10; i++ {
		f[i] = 0xff
	}
	f[10], f[16] = s.UID, s.UID
	le.PutUint32(f[11:], uint32(n))
	le.PutUint32(f[17:], uint32(n>>32))
	p := port << 1
	if enc {
		p |= 1
	}
	f[15], f[21] = p, p
	return f, n
}

func (s *Sender) frame(port uint8, enc bool, body []byte) []byte {
	f, n := s.header(port, enc)
	if enc {
		return append(f, AEADSeal(s.SessionKey, n, body)...)
	}
	return append(f, AuthSeal(s.SessionKey, n, body)...)
}

// Session returns the session key frame (sent about once a second by OpenHD).
func (s *Sender) Session() []byte {
	f, _ := s.header(PortSession, false)
	return append(f, s.sessionPacket...)
}

func (s *Sender) genBHdr(port, typ uint8, d []byte) []byte {
	if !s.GenB {
		return d
	}
	key := uint16(port)<<8 | uint16(typ)
	i := s.bidx[key]
	s.bidx[key] = i + 1
	o := make([]byte, 8+len(d))
	o[1] = typ
	le.PutUint32(o[2:], i)
	le.PutUint16(o[6:], uint16(len(o)))
	copy(o[8:], d)
	return o
}

// Video returns one FEC block from RTP packets (each ≤ 1441 B) with m secondaries (m ≤ 0:
// 20 %, at least 1), in send order.
func (s *Sender) Video(packets [][]byte, cam int, m int, enc bool) [][]byte {
	port := uint8(PortVideo0 + cam)
	blk := s.block[port]
	s.block[port] = blk + 1
	k := len(packets)
	if m <= 0 {
		m = max(1, int(float64(k)*0.2+0.5))
	}
	hdr := func(fi, size int) []byte {
		h := make([]byte, 8)
		le.PutUint32(h, blk)
		h[4], h[5] = byte(fi), byte(k)
		le.PutUint16(h[6:], uint16(size))
		return h
	}
	var out, syms [][]byte
	L := 0
	for j, p := range packets {
		b := append(hdr(j, len(p)), p...)
		syms = append(syms, b[6:])
		L = max(L, len(p)+2)
		out = append(out, s.frame(port, enc, s.genBHdr(port, 0, b)))
	}
	for r, sym := range fec.CauchyEncode(syms, m, L) {
		b := append(hdr(k+r, 0)[:6], sym...)
		out = append(out, s.frame(port, enc, s.genBHdr(port, 0, b)))
	}
	return out
}

// Telemetry returns n copies of one encrypted FECDisabled telemetry packet (OpenHD injects
// telemetry more than once).
func (s *Sender) Telemetry(data []byte, n int) [][]byte {
	port := uint8(PortTeleAir)
	if s.UID == UIDGnd {
		port = PortTeleGnd
	}
	seq := s.seq[port]
	s.seq[port] = seq + 1
	b := make([]byte, 8+len(data))
	le.PutUint32(b, seq)
	copy(b[8:], data)
	out := make([][]byte, n)
	for i := range out {
		out[i] = s.frame(port, true, s.genBHdr(port, 1, b))
	}
	return out
}

// Mgmt returns a management frame announcing frequency (MHz) and width.
func (s *Sender) Mgmt(freq uint32, bw uint8) []byte {
	b := make([]byte, 6)
	le.PutUint32(b[1:], freq)
	b[5] = bw
	return s.frame(PortMgmtAir, true, b)
}
