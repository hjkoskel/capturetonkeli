package openhd

// OpenHD keys (docs/13 §4.4), port of js/ohdkeys.js: bind phrase → Argon2id seeds →
// Curve25519 keypairs → beforenm keys K_air / K_gnd, or the same from a 128-byte txrx.key.
// Written from RFC 7748 (X25519), RFC 7693 (BLAKE2b), RFC 9106 (Argon2id) and libsodium's
// crypto_box_seed_keypair / beforenm (no golang.org/x/crypto: the module is stdlib-only).

import (
	"crypto/sha512"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"math/big"
	"math/bits"
)

// Keys are the beforenm keys that open each direction's session key packet.
type Keys struct {
	Air []byte // K_air (32 bytes): opens the air unit's session packets; nil = unknown
	Gnd []byte // K_gnd (32 bytes): the ground unit's
}

func mustHex(s string) []byte {
	b, err := hex.DecodeString(s)
	if err != nil {
		panic(err)
	}
	return b
}

// DefaultKeys are the keys of the default bind phrase "openhd" (public: the phrase is in
// OpenHD's source).
func DefaultKeys() Keys {
	return Keys{
		Air: mustHex("978a890d4b79f33f700fcdb7e4b91be6d6eb903a6638d2381d9b8e3ea9b20125"),
		Gnd: mustHex("5be985599ab9271ee02aef52532bf4f5bd83eba8915934373f0ef1468a33f003"),
	}
}

var (
	saltAir = []byte{192, 189, 216, 102, 56, 153, 154, 92, 228, 26, 49, 209, 157, 7, 128, 207}
	saltGnd = []byte{179, 30, 150, 20, 17, 200, 225, 82, 48, 64, 18, 130, 89, 62, 83, 234}
)

// ---- X25519 (math/big Montgomery ladder; used once per key) ----

var p25519 = new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 255), big.NewInt(19))

func leToBig(b []byte) *big.Int {
	r := make([]byte, len(b))
	for i := range b {
		r[len(b)-1-i] = b[i]
	}
	return new(big.Int).SetBytes(r)
}

func bigToLE(v *big.Int) []byte {
	be := v.FillBytes(make([]byte, 32))
	out := make([]byte, 32)
	for i := range be {
		out[31-i] = be[i]
	}
	return out
}

// X25519 returns scalar·u (u = nil: the base point 9).
func X25519(scalar, u []byte) []byte {
	k := append([]byte(nil), scalar[:32]...)
	k[0] &= 248
	k[31] &= 127
	k[31] |= 64
	kn := leToBig(k)
	var x1 *big.Int
	if u == nil {
		x1 = big.NewInt(9)
	} else {
		uu := append([]byte(nil), u[:32]...)
		uu[31] &= 127
		x1 = leToBig(uu)
	}
	P := p25519
	md := func(a *big.Int) *big.Int { return a.Mod(a, P) }
	x2, z2, x3, z3 := big.NewInt(1), big.NewInt(0), new(big.Int).Set(x1), big.NewInt(1)
	swap := uint(0)
	a24 := big.NewInt(121665)
	for t := 254; t >= 0; t-- {
		kt := kn.Bit(t)
		if swap^kt != 0 {
			x2, x3 = x3, x2
			z2, z3 = z3, z2
		}
		swap = kt
		A := md(new(big.Int).Add(x2, z2))
		AA := md(new(big.Int).Mul(A, A))
		B := md(new(big.Int).Sub(x2, z2))
		BB := md(new(big.Int).Mul(B, B))
		E := md(new(big.Int).Sub(AA, BB))
		C := md(new(big.Int).Add(x3, z3))
		D := md(new(big.Int).Sub(x3, z3))
		DA := md(new(big.Int).Mul(D, A))
		CB := md(new(big.Int).Mul(C, B))
		s := new(big.Int).Add(DA, CB)
		x3 = md(s.Mul(s, s))
		d := new(big.Int).Sub(DA, CB)
		d = md(d.Mul(d, d))
		z3 = md(d.Mul(d, x1))
		x2 = md(new(big.Int).Mul(AA, BB))
		e := new(big.Int).Mul(a24, E)
		e.Add(e, AA)
		z2 = md(e.Mul(e, E))
	}
	if swap != 0 {
		x2, z2 = x3, z3
	}
	inv := new(big.Int).Exp(z2, new(big.Int).Sub(P, big.NewInt(2)), P)
	return bigToLE(md(inv.Mul(inv, x2)))
}

// Beforenm is crypto_box_beforenm(pk, sk) = HSalsa20(X25519(sk, pk), 0).
func Beforenm(pk, sk []byte) []byte {
	k := HSalsa20(X25519(sk, pk), make([]byte, 16))
	return k[:]
}

// SeedKeypair is crypto_box_seed_keypair: sk = SHA-512(seed)[:32], pk = X25519(sk, 9).
func SeedKeypair(seed []byte) (pk, sk []byte) {
	h := sha512.Sum512(seed)
	sk = h[:32]
	return X25519(sk, nil), sk
}

// ---- BLAKE2b ----

var b2iv = [8]uint64{0x6a09e667f3bcc908, 0xbb67ae8584caa73b, 0x3c6ef372fe94f82b, 0xa54ff53a5f1d36f1,
	0x510e527fade682d1, 0x9b05688c2b3e6c1f, 0x1f83d9abfb41bd6b, 0x5be0cd19137e2179}

var b2sigma = [10][16]byte{
	{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15}, {14, 10, 4, 8, 9, 15, 13, 6, 1, 12, 0, 2, 11, 7, 5, 3},
	{11, 8, 12, 0, 5, 2, 15, 13, 10, 14, 3, 6, 7, 1, 9, 4}, {7, 9, 3, 1, 13, 12, 11, 14, 2, 6, 5, 10, 4, 0, 15, 8},
	{9, 0, 5, 7, 2, 4, 10, 15, 14, 1, 11, 12, 6, 8, 3, 13}, {2, 12, 6, 10, 0, 11, 8, 3, 4, 13, 7, 5, 15, 14, 1, 9},
	{12, 5, 1, 15, 14, 13, 4, 10, 0, 7, 6, 3, 9, 2, 8, 11}, {13, 11, 7, 14, 12, 1, 3, 9, 5, 0, 15, 4, 8, 6, 2, 10},
	{6, 15, 14, 9, 11, 3, 0, 8, 12, 2, 13, 7, 1, 4, 10, 5}, {10, 2, 8, 4, 7, 6, 1, 5, 15, 11, 9, 14, 3, 12, 13, 0},
}

func b2compress(h *[8]uint64, block []byte, t uint64, last bool) {
	var v [16]uint64
	copy(v[:8], h[:])
	copy(v[8:], b2iv[:])
	v[12] ^= t
	if last {
		v[14] = ^v[14]
	}
	var m [16]uint64
	for i := range m {
		m[i] = binary.LittleEndian.Uint64(block[8*i:])
	}
	g := func(a, b, c, d int, x, y uint64) {
		v[a] += v[b] + x
		v[d] = bits.RotateLeft64(v[d]^v[a], -32)
		v[c] += v[d]
		v[b] = bits.RotateLeft64(v[b]^v[c], -24)
		v[a] += v[b] + y
		v[d] = bits.RotateLeft64(v[d]^v[a], -16)
		v[c] += v[d]
		v[b] = bits.RotateLeft64(v[b]^v[c], -63)
	}
	for r := 0; r < 12; r++ {
		s := &b2sigma[r%10]
		g(0, 4, 8, 12, m[s[0]], m[s[1]])
		g(1, 5, 9, 13, m[s[2]], m[s[3]])
		g(2, 6, 10, 14, m[s[4]], m[s[5]])
		g(3, 7, 11, 15, m[s[6]], m[s[7]])
		g(0, 5, 10, 15, m[s[8]], m[s[9]])
		g(1, 6, 11, 12, m[s[10]], m[s[11]])
		g(2, 7, 8, 13, m[s[12]], m[s[13]])
		g(3, 4, 9, 14, m[s[14]], m[s[15]])
	}
	for i := 0; i < 8; i++ {
		h[i] ^= v[i] ^ v[i+8]
	}
}

// Blake2b returns the unkeyed BLAKE2b hash (outLen 1…64) of the concatenated parts.
func Blake2b(outLen int, parts ...[]byte) []byte {
	h := b2iv
	h[0] ^= 0x01010000 ^ uint64(outLen)
	var buf [128]byte
	fill := 0
	var t uint64
	for _, p := range parts {
		for len(p) > 0 {
			if fill == 128 {
				t += 128
				b2compress(&h, buf[:], t, false)
				fill = 0
			}
			n := copy(buf[fill:], p)
			fill += n
			p = p[n:]
		}
	}
	t += uint64(fill)
	for i := fill; i < 128; i++ {
		buf[i] = 0
	}
	b2compress(&h, buf[:], t, true)
	var out [64]byte
	for i := 0; i < 8; i++ {
		binary.LittleEndian.PutUint64(out[8*i:], h[i])
	}
	return out[:outLen]
}

func le32b(n int) []byte { b := make([]byte, 4); binary.LittleEndian.PutUint32(b, uint32(n)); return b }

// hPrime is Argon2's variable-length hash H'.
func hPrime(T int, parts ...[]byte) []byte {
	if T <= 64 {
		return Blake2b(T, append([][]byte{le32b(T)}, parts...)...)
	}
	out := make([]byte, T)
	r := (T+31)/32 - 2
	v := Blake2b(64, append([][]byte{le32b(T)}, parts...)...)
	copy(out, v[:32])
	for i := 1; i < r; i++ {
		v = Blake2b(64, v)
		copy(out[32*i:], v[:32])
	}
	copy(out[32*r:], Blake2b(T-32*r, v))
	return out
}

// ---- Argon2id (version 0x13, one lane) ----

type block [128]uint64

func blamka(v *block, a, b, c, d int) {
	f := func(x, y uint64) uint64 { return x + y + 2*uint64(uint32(x))*uint64(uint32(y)) }
	v[a] = f(v[a], v[b])
	v[d] = bits.RotateLeft64(v[d]^v[a], -32)
	v[c] = f(v[c], v[d])
	v[b] = bits.RotateLeft64(v[b]^v[c], -24)
	v[a] = f(v[a], v[b])
	v[d] = bits.RotateLeft64(v[d]^v[a], -16)
	v[c] = f(v[c], v[d])
	v[b] = bits.RotateLeft64(v[b]^v[c], -63)
}

func permute(v *block, w *[16]int) {
	blamka(v, w[0], w[4], w[8], w[12])
	blamka(v, w[1], w[5], w[9], w[13])
	blamka(v, w[2], w[6], w[10], w[14])
	blamka(v, w[3], w[7], w[11], w[15])
	blamka(v, w[0], w[5], w[10], w[15])
	blamka(v, w[1], w[6], w[11], w[12])
	blamka(v, w[2], w[7], w[8], w[13])
	blamka(v, w[3], w[4], w[9], w[14])
}

var rowIdx, colIdx [8][16]int

func init() {
	for i := 0; i < 8; i++ {
		for j := 0; j < 16; j++ {
			rowIdx[i][j] = 16*i + j
			colIdx[i][j] = 2*i + (j & 1) + 16*(j>>1)
		}
	}
}

// gBlock computes dst = G(x, y) (dst ^= G(x, y) when xor).
func gBlock(dst, x, y *block, xor bool) {
	var r, q block
	for i := range r {
		r[i] = x[i] ^ y[i]
	}
	q = r
	for i := range rowIdx {
		permute(&q, &rowIdx[i])
	}
	for i := range colIdx {
		permute(&q, &colIdx[i])
	}
	if xor {
		for i := range dst {
			dst[i] ^= q[i] ^ r[i]
		}
	} else {
		for i := range dst {
			dst[i] = q[i] ^ r[i]
		}
	}
}

func bytesToBlock(b []byte) block {
	var o block
	for i := range o {
		o[i] = binary.LittleEndian.Uint64(b[8*i:])
	}
	return o
}

// Argon2id(pw, salt, t passes, m KiB, one lane) → tagLen bytes. progress (may be nil) is
// called after each slice with 0…1.
func Argon2id(pw, salt []byte, t, m, tagLen int, progress func(float64)) []byte {
	const lanes = 1
	mm := 4 * (m / 4)
	q := mm
	sl := q / 4
	h0 := Blake2b(64, le32b(lanes), le32b(tagLen), le32b(m), le32b(t), le32b(0x13), le32b(2), le32b(len(pw)), pw,
		le32b(len(salt)), salt, le32b(0), le32b(0))
	mem := make([]block, mm)
	mem[0] = bytesToBlock(hPrime(1024, h0, le32b(0), le32b(0)))
	mem[1] = bytesToBlock(hPrime(1024, h0, le32b(1), le32b(0)))
	var addr, input, zero block
	nextAddresses := func() {
		input[6]++
		var tmp block
		gBlock(&tmp, &zero, &input, false)
		gBlock(&addr, &zero, &tmp, false)
	}
	for r := 0; r < t; r++ {
		for s := 0; s < 4; s++ {
			indep := r == 0 && s < 2
			if indep {
				input = block{}
				input[0], input[1], input[2], input[3], input[4], input[5] = uint64(r), 0, uint64(s), uint64(mm), uint64(t), 2
			}
			start := 0
			if r == 0 && s == 0 {
				start = 2
			}
			if indep && start != 0 {
				nextAddresses()
			}
			for i := start; i < sl; i++ {
				cur := s*sl + i
				prev := cur - 1
				if cur == 0 {
					prev = q - 1
				}
				var j1 uint64
				if indep {
					if i%128 == 0 {
						nextAddresses()
					}
					j1 = addr[i%128] & 0xffffffff
				} else {
					j1 = mem[prev][0] & 0xffffffff
				}
				var W int
				if r == 0 {
					W = s*sl + i - 1
				} else {
					W = q - sl + i - 1
				}
				x := (j1 * j1) >> 32
				y := (uint64(W) * x) >> 32
				zz := uint64(W) - 1 - y
				st0 := 0
				if r != 0 {
					st0 = ((s + 1) * sl) % q
				}
				ref := (uint64(st0) + zz) % uint64(q)
				gBlock(&mem[cur], &mem[prev], &mem[ref], r > 0)
			}
			if progress != nil {
				progress(float64(r*4+s+1) / float64(4*t))
			}
		}
	}
	last := make([]byte, 1024)
	for i, w := range mem[q-1] {
		binary.LittleEndian.PutUint64(last[8*i:], w)
	}
	return hPrime(tagLen, last)
}

// KeysFromPhrase derives the keys of a bind phrase (2 × Argon2id with 64 MiB: a few
// seconds). "openhd" returns DefaultKeys at once. progress may be nil.
func KeysFromPhrase(phrase string, progress func(float64)) Keys {
	if phrase == "openhd" {
		return DefaultKeys()
	}
	half := func(off float64) func(float64) {
		if progress == nil {
			return nil
		}
		return func(p float64) { progress(off + p/2) }
	}
	pw := []byte(phrase)
	seedAir := Argon2id(pw, saltAir, 2, 65536, 32, half(0))
	seedGnd := Argon2id(pw, saltGnd, 2, 65536, 32, half(0.5))
	apk, ask := SeedKeypair(seedAir)
	gpk, gsk := SeedKeypair(seedGnd)
	return Keys{Air: Beforenm(apk, ask), Gnd: Beforenm(gpk, gsk)}
}

// KeysFromKeyFile reads a 128-byte txrx.key (key_1.pub ‖ key_1.sec ‖ key_2.pub ‖ key_2.sec).
func KeysFromKeyFile(b []byte) (Keys, error) {
	if len(b) != 128 {
		return Keys{}, fmt.Errorf("openhd: txrx.key must be 128 bytes (this file has %d)", len(b))
	}
	return Keys{Air: Beforenm(b[0:32], b[32:64]), Gnd: Beforenm(b[64:96], b[96:128])}, nil
}
