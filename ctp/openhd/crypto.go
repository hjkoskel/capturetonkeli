package openhd

// Crypto primitives for the OpenHD receiver (docs/13 §4), port of js/ohdcrypto.js. The Go
// module is stdlib-only, so Salsa20/XSalsa20, ChaCha20 and Poly1305 are written here from
// the algorithm descriptions (as in libsodium) and checked against libsodium output
// (test/js/fixtures/ohd-sodium.json):
//   - secretbox open/seal (session key packet, crypto_box_*_afternm)
//   - auth-only packets   (Poly1305 with an HChaCha20 subkey)
//   - encrypted packets   (crypto_aead_chacha20poly1305, ORIGINAL 8-byte-nonce variant;
//     not RFC 8439, see docs/13 §4.6)

import (
	"crypto/subtle"
	"encoding/binary"
	"math/bits"
)

var le = binary.LittleEndian

var sigma = [4]uint32{0x61707865, 0x3320646e, 0x79622d32, 0x6b206574} // "expand 32-byte k"

// ---- Salsa20 ----

func salsaRounds(x *[16]uint32) {
	r := bits.RotateLeft32
	for i := 0; i < 20; i += 2 {
		x[4] ^= r(x[0]+x[12], 7)
		x[8] ^= r(x[4]+x[0], 9)
		x[12] ^= r(x[8]+x[4], 13)
		x[0] ^= r(x[12]+x[8], 18)
		x[9] ^= r(x[5]+x[1], 7)
		x[13] ^= r(x[9]+x[5], 9)
		x[1] ^= r(x[13]+x[9], 13)
		x[5] ^= r(x[1]+x[13], 18)
		x[14] ^= r(x[10]+x[6], 7)
		x[2] ^= r(x[14]+x[10], 9)
		x[6] ^= r(x[2]+x[14], 13)
		x[10] ^= r(x[6]+x[2], 18)
		x[3] ^= r(x[15]+x[11], 7)
		x[7] ^= r(x[3]+x[15], 9)
		x[11] ^= r(x[7]+x[3], 13)
		x[15] ^= r(x[11]+x[7], 18)
		x[1] ^= r(x[0]+x[3], 7)
		x[2] ^= r(x[1]+x[0], 9)
		x[3] ^= r(x[2]+x[1], 13)
		x[0] ^= r(x[3]+x[2], 18)
		x[6] ^= r(x[5]+x[4], 7)
		x[7] ^= r(x[6]+x[5], 9)
		x[4] ^= r(x[7]+x[6], 13)
		x[5] ^= r(x[4]+x[7], 18)
		x[11] ^= r(x[10]+x[9], 7)
		x[8] ^= r(x[11]+x[10], 9)
		x[9] ^= r(x[8]+x[11], 13)
		x[10] ^= r(x[9]+x[8], 18)
		x[12] ^= r(x[15]+x[14], 7)
		x[13] ^= r(x[12]+x[15], 9)
		x[14] ^= r(x[13]+x[12], 13)
		x[15] ^= r(x[14]+x[13], 18)
	}
}

func salsaInit(key []byte, n16 []byte) [16]uint32 {
	var s [16]uint32
	s[0], s[5], s[10], s[15] = sigma[0], sigma[1], sigma[2], sigma[3]
	for i := 0; i < 4; i++ {
		s[1+i] = le.Uint32(key[4*i:])
		s[11+i] = le.Uint32(key[16+4*i:])
		s[6+i] = le.Uint32(n16[4*i:])
	}
	return s
}

// HSalsa20 derives a 32-byte key from key and a 16-byte input.
func HSalsa20(key, in16 []byte) [32]byte {
	x := salsaInit(key, in16)
	salsaRounds(&x)
	var o [32]byte
	for i, j := range [8]int{0, 5, 10, 15, 6, 7, 8, 9} {
		le.PutUint32(o[4*i:], x[j])
	}
	return o
}

// xsalsa20XOR XORs data with the XSalsa20 keystream (24-byte nonce), starting at keystream
// byte skip (< 64).
func xsalsa20XOR(key, n24, data []byte, skip int) []byte {
	sub := HSalsa20(key, n24[:16])
	var n [16]byte
	copy(n[:], n24[16:24])
	s0 := salsaInit(sub[:], n[:])
	out := make([]byte, len(data))
	var ks [64]byte
	var ctr uint64
	pos, off := 0, skip
	for pos < len(data) {
		s0[8], s0[9] = uint32(ctr), uint32(ctr>>32)
		x := s0
		salsaRounds(&x)
		for i := 0; i < 16; i++ {
			le.PutUint32(ks[4*i:], x[i]+s0[i])
		}
		for i := off; i < 64 && pos < len(data); i++ {
			out[pos] = data[pos] ^ ks[i]
			pos++
		}
		off = 0
		ctr++
	}
	return out
}

// ---- ChaCha20 (original: 64-bit counter, 64-bit nonce) ----

func chachaRounds(x *[16]uint32) {
	qr := func(a, b, c, d int) {
		x[a] += x[b]
		x[d] = bits.RotateLeft32(x[d]^x[a], 16)
		x[c] += x[d]
		x[b] = bits.RotateLeft32(x[b]^x[c], 12)
		x[a] += x[b]
		x[d] = bits.RotateLeft32(x[d]^x[a], 8)
		x[c] += x[d]
		x[b] = bits.RotateLeft32(x[b]^x[c], 7)
	}
	for i := 0; i < 20; i += 2 {
		qr(0, 4, 8, 12)
		qr(1, 5, 9, 13)
		qr(2, 6, 10, 14)
		qr(3, 7, 11, 15)
		qr(0, 5, 10, 15)
		qr(1, 6, 11, 12)
		qr(2, 7, 8, 13)
		qr(3, 4, 9, 14)
	}
}

func chachaInit(key []byte) [16]uint32 {
	var s [16]uint32
	copy(s[:4], sigma[:])
	for i := 0; i < 8; i++ {
		s[4+i] = le.Uint32(key[4*i:])
	}
	return s
}

// HChaCha20 derives a 32-byte key (libsodium crypto_core_hchacha20 with c = NULL).
func HChaCha20(key, in16 []byte) [32]byte {
	x := chachaInit(key)
	for i := 0; i < 4; i++ {
		x[12+i] = le.Uint32(in16[4*i:])
	}
	chachaRounds(&x)
	var o [32]byte
	for i, j := range [8]int{0, 1, 2, 3, 12, 13, 14, 15} {
		le.PutUint32(o[4*i:], x[j])
	}
	return o
}

// ChaCha20XOR XORs data with the ChaCha20 keystream (8-byte nonce), starting at block ic.
func ChaCha20XOR(key, n8, data []byte, ic uint64) []byte {
	s0 := chachaInit(key)
	s0[14], s0[15] = le.Uint32(n8), le.Uint32(n8[4:])
	out := make([]byte, len(data))
	var ks [64]byte
	ctr, pos := ic, 0
	for pos < len(data) {
		s0[12], s0[13] = uint32(ctr), uint32(ctr>>32)
		x := s0
		chachaRounds(&x)
		for i := 0; i < 16; i++ {
			le.PutUint32(ks[4*i:], x[i]+s0[i])
		}
		for i := 0; i < 64 && pos < len(data); i++ {
			out[pos] = data[pos] ^ ks[i]
			pos++
		}
		ctr++
	}
	return out
}

// ---- Poly1305 (26-bit limbs) ----

type poly struct {
	r, s [5]uint32
	h    [5]uint32
	pad  [4]uint32
	buf  [16]byte
	fill int
}

func newPoly(key []byte) *poly {
	p := &poly{}
	p.r[0] = le.Uint32(key[0:]) & 0x3ffffff
	p.r[1] = (le.Uint32(key[3:]) >> 2) & 0x3ffff03
	p.r[2] = (le.Uint32(key[6:]) >> 4) & 0x3ffc0ff
	p.r[3] = (le.Uint32(key[9:]) >> 6) & 0x3f03fff
	p.r[4] = (le.Uint32(key[12:]) >> 8) & 0x00fffff
	for i := 1; i < 5; i++ {
		p.s[i] = p.r[i] * 5
	}
	for i := 0; i < 4; i++ {
		p.pad[i] = le.Uint32(key[16+4*i:])
	}
	return p
}

func (p *poly) block(m []byte, hibit uint32) {
	const mask = 0x3ffffff
	r0, r1, r2, r3, r4 := uint64(p.r[0]), uint64(p.r[1]), uint64(p.r[2]), uint64(p.r[3]), uint64(p.r[4])
	s1, s2, s3, s4 := uint64(p.s[1]), uint64(p.s[2]), uint64(p.s[3]), uint64(p.s[4])
	h0 := uint64(p.h[0] + le.Uint32(m[0:])&mask)
	h1 := uint64(p.h[1] + (le.Uint32(m[3:])>>2)&mask)
	h2 := uint64(p.h[2] + (le.Uint32(m[6:])>>4)&mask)
	h3 := uint64(p.h[3] + (le.Uint32(m[9:])>>6)&mask)
	h4 := uint64(p.h[4] + (le.Uint32(m[12:])>>8 | hibit))
	d0 := h0*r0 + h1*s4 + h2*s3 + h3*s2 + h4*s1
	d1 := h0*r1 + h1*r0 + h2*s4 + h3*s3 + h4*s2
	d2 := h0*r2 + h1*r1 + h2*r0 + h3*s4 + h4*s3
	d3 := h0*r3 + h1*r2 + h2*r1 + h3*r0 + h4*s4
	d4 := h0*r4 + h1*r3 + h2*r2 + h3*r1 + h4*r0
	c := d0 >> 26
	p.h[0] = uint32(d0) & mask
	d1 += c
	c = d1 >> 26
	p.h[1] = uint32(d1) & mask
	d2 += c
	c = d2 >> 26
	p.h[2] = uint32(d2) & mask
	d3 += c
	c = d3 >> 26
	p.h[3] = uint32(d3) & mask
	d4 += c
	c = d4 >> 26
	p.h[4] = uint32(d4) & mask
	p.h[0] += uint32(c) * 5
	p.h[1] += p.h[0] >> 26
	p.h[0] &= mask
}

func (p *poly) write(b []byte) {
	for len(b) > 0 {
		if p.fill == 0 && len(b) >= 16 {
			p.block(b[:16], 1<<24)
			b = b[16:]
			continue
		}
		n := copy(p.buf[p.fill:], b)
		p.fill += n
		b = b[n:]
		if p.fill == 16 {
			p.block(p.buf[:], 1<<24)
			p.fill = 0
		}
	}
}

func (p *poly) sum() [16]byte {
	const mask = 0x3ffffff
	if p.fill > 0 {
		p.buf[p.fill] = 1
		for i := p.fill + 1; i < 16; i++ {
			p.buf[i] = 0
		}
		p.block(p.buf[:], 0)
	}
	h := p.h
	c := h[1] >> 26
	h[1] &= mask
	h[2] += c
	c = h[2] >> 26
	h[2] &= mask
	h[3] += c
	c = h[3] >> 26
	h[3] &= mask
	h[4] += c
	c = h[4] >> 26
	h[4] &= mask
	h[0] += c * 5
	c = h[0] >> 26
	h[0] &= mask
	h[1] += c
	// g = h + 5 - 2^130; use g when h ≥ p
	var g [5]uint32
	g[0] = h[0] + 5
	c = g[0] >> 26
	g[0] &= mask
	g[1] = h[1] + c
	c = g[1] >> 26
	g[1] &= mask
	g[2] = h[2] + c
	c = g[2] >> 26
	g[2] &= mask
	g[3] = h[3] + c
	c = g[3] >> 26
	g[3] &= mask
	g[4] = h[4] + c - (1 << 26)
	sel := (g[4] >> 31) - 1 // all ones when g4 did not underflow (h ≥ p)
	for i := range h {
		h[i] = (h[i] &^ sel) | (g[i] & sel)
	}
	w0 := h[0] | h[1]<<26
	w1 := h[1]>>6 | h[2]<<20
	w2 := h[2]>>12 | h[3]<<14
	w3 := h[3]>>18 | h[4]<<8
	var out [16]byte
	f := uint64(w0) + uint64(p.pad[0])
	le.PutUint32(out[0:], uint32(f))
	f = uint64(w1) + uint64(p.pad[1]) + f>>32
	le.PutUint32(out[4:], uint32(f))
	f = uint64(w2) + uint64(p.pad[2]) + f>>32
	le.PutUint32(out[8:], uint32(f))
	f = uint64(w3) + uint64(p.pad[3]) + f>>32
	le.PutUint32(out[12:], uint32(f))
	return out
}

// Poly1305 returns the tag of the concatenated parts under a 32-byte one-time key.
func Poly1305(key []byte, parts ...[]byte) [16]byte {
	p := newPoly(key)
	for _, b := range parts {
		p.write(b)
	}
	return p.sum()
}

func tagOK(want []byte, got [16]byte) bool { return subtle.ConstantTimeCompare(want, got[:]) == 1 }

// ---- secretbox (XSalsa20-Poly1305) ----

// SecretboxOpen opens box = tag16 ‖ ciphertext (crypto_box_open_easy_afternm). ok = false
// when the tag does not verify.
func SecretboxOpen(box, n24, k []byte) ([]byte, bool) {
	if len(box) < 16 {
		return nil, false
	}
	c := box[16:]
	polyKey := xsalsa20XOR(k, n24, make([]byte, 32), 0)
	if !tagOK(box[:16], Poly1305(polyKey, c)) {
		return nil, false
	}
	return xsalsa20XOR(k, n24, c, 32), true
}

// SecretboxSeal is the inverse of SecretboxOpen (demo sender, tests).
func SecretboxSeal(m, n24, k []byte) []byte {
	c := xsalsa20XOR(k, n24, m, 32)
	tag := Poly1305(xsalsa20XOR(k, n24, make([]byte, 32), 0), c)
	return append(tag[:], c...)
}

// ---- OpenHD per-packet protection (docs/13 §4.6) ----

func nonce8(n uint64) []byte { b := make([]byte, 8); le.PutUint64(b, n); return b }

func authKey(sessionKey []byte, nonce uint64) [32]byte {
	in := make([]byte, 16)
	le.PutUint64(in, nonce)
	return HChaCha20(sessionKey, in)
}

// AuthOpen checks an auth-only packet (radio port bit0 = 0): body ‖ tag. Returns the body.
func AuthOpen(sessionKey []byte, nonce uint64, payload []byte) ([]byte, bool) {
	if len(payload) < 17 {
		return nil, false
	}
	body := payload[:len(payload)-16]
	k := authKey(sessionKey, nonce)
	if !tagOK(payload[len(payload)-16:], Poly1305(k[:], body)) {
		return nil, false
	}
	return body, true
}

// AuthSeal appends the tag to body (demo sender, tests).
func AuthSeal(sessionKey []byte, nonce uint64, body []byte) []byte {
	k := authKey(sessionKey, nonce)
	tag := Poly1305(k[:], body)
	return append(append([]byte(nil), body...), tag[:]...)
}

func lenLE64(n int) []byte { return nonce8(uint64(n)) }

// AEADOpen decrypts crypto_aead_chacha20poly1305 (original construction, no AD).
func AEADOpen(sessionKey []byte, nonce uint64, payload []byte) ([]byte, bool) {
	if len(payload) < 17 {
		return nil, false
	}
	n := nonce8(nonce)
	c := payload[:len(payload)-16]
	polyKey := ChaCha20XOR(sessionKey, n, make([]byte, 32), 0)
	if !tagOK(payload[len(payload)-16:], Poly1305(polyKey, lenLE64(0), c, lenLE64(len(c)))) {
		return nil, false
	}
	return ChaCha20XOR(sessionKey, n, c, 1), true
}

// AEADSeal encrypts (demo sender, tests).
func AEADSeal(sessionKey []byte, nonce uint64, m []byte) []byte {
	n := nonce8(nonce)
	c := ChaCha20XOR(sessionKey, n, m, 1)
	polyKey := ChaCha20XOR(sessionKey, n, make([]byte, 32), 0)
	tag := Poly1305(polyKey, lenLE64(0), c, lenLE64(len(c)))
	return append(c, tag[:]...)
}
