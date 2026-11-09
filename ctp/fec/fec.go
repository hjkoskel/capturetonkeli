// Package fec holds the erasure codes over GF(2^8) (poly 0x11d) used by the FPV receivers:
//
//   - Vandermonde (Rizzo/zfec construction): hx-esp32-cam-fpv (docs/12 §3), port of js/hxfec.js.
//   - Cauchy-style rows of wifibroadcast's fec.c: OpenHD video (docs/13 §5.3), port of js/ohdfec.js.
//
// Both are systematic: the k data packets are sent as they are, followed by FEC packets.
package fec

import (
	"errors"
	"fmt"
	"sort"
	"sync"
)

var (
	exp [510]byte
	log [256]int
	mul [256][256]byte
)

func init() {
	x := 1
	for i := 0; i < 255; i++ {
		exp[i] = byte(x)
		log[x] = i
		x <<= 1
		if x&0x100 != 0 {
			x ^= 0x11d // x^8 + x^4 + x^3 + x^2 + 1
		}
	}
	for i := 255; i < 510; i++ {
		exp[i] = exp[i-255]
	}
	log[0] = -1
	for a := 1; a < 256; a++ {
		for b := 1; b < 256; b++ {
			mul[a][b] = exp[log[a]+log[b]]
		}
	}
}

// ErrSingular is returned when a decoding matrix cannot be inverted.
var ErrSingular = errors.New("fec: singular matrix")

// Mul returns a·b in GF(2^8).
func Mul(a, b byte) byte { return mul[a][b] }

// Inv returns 1/a in GF(2^8); a must not be 0.
func Inv(a byte) byte {
	if a == 0 {
		panic("fec: inverse of 0")
	}
	return exp[255-log[a]]
}

// AddMul computes dst[:n] ^= c·src[:n].
func AddMul(dst, src []byte, c byte, n int) {
	if c == 0 {
		return
	}
	t := &mul[c]
	for i := 0; i < n; i++ {
		dst[i] ^= t[src[i]]
	}
}

// Invert returns the inverse of the k×k row-major matrix m (Gauss-Jordan).
func Invert(m []byte, k int) ([]byte, error) {
	a := append([]byte(nil), m...)
	r := make([]byte, k*k)
	for i := 0; i < k; i++ {
		r[i*k+i] = 1
	}
	for c := 0; c < k; c++ {
		p := c
		for p < k && a[p*k+c] == 0 {
			p++
		}
		if p == k {
			return nil, ErrSingular
		}
		if p != c {
			for j := 0; j < k; j++ {
				a[p*k+j], a[c*k+j] = a[c*k+j], a[p*k+j]
				r[p*k+j], r[c*k+j] = r[c*k+j], r[p*k+j]
			}
		}
		s := Inv(a[c*k+c])
		for j := 0; j < k; j++ {
			a[c*k+j] = mul[a[c*k+j]][s]
			r[c*k+j] = mul[r[c*k+j]][s]
		}
		for i := 0; i < k; i++ {
			f := a[i*k+c]
			if i == c || f == 0 {
				continue
			}
			for j := 0; j < k; j++ {
				a[i*k+j] ^= mul[f][a[c*k+j]]
				r[i*k+j] ^= mul[f][r[c*k+j]]
			}
		}
	}
	return r, nil
}

// ---- Vandermonde (hx-esp32-cam-fpv) -----------------------------------------------------

var rowCache sync.Map // [2]int{k, maxN} → []byte

// VandermondeRows returns encoding rows k…maxN-1 (each k bytes) for k data packets: the
// bottom of the Vandermonde matrix V (row 0 = e0, row i+1 = [α^(i·j)]) times inv(V top k×k).
// Row r does not depend on n, so one table serves any n ≤ maxN.
func VandermondeRows(k, maxN int) ([]byte, error) {
	key := [2]int{k, maxN}
	if v, ok := rowCache.Load(key); ok {
		return v.([]byte), nil
	}
	if !(k >= 1 && k <= 16 && maxN > k && maxN <= 32) {
		return nil, fmt.Errorf("fec: unsupported k=%d n=%d", k, maxN)
	}
	v := make([]byte, maxN*k)
	v[0] = 1
	for r := 0; r < maxN-1; r++ {
		for c := 0; c < k; c++ {
			v[(r+1)*k+c] = exp[(r*c)%255]
		}
	}
	top, err := Invert(v[:k*k], k)
	if err != nil {
		return nil, err
	}
	rows := make([]byte, (maxN-k)*k)
	for r := k; r < maxN; r++ {
		for c := 0; c < k; c++ {
			var s byte
			for j := 0; j < k; j++ {
				s ^= mul[v[r*k+j]][top[j*k+c]]
			}
			rows[(r-k)*k+c] = s
		}
	}
	rowCache.Store(key, rows)
	return rows, nil
}

// VandermondeEncode returns FEC packet number idx (k ≤ idx < 32) from the k data packets
// (each size bytes).
func VandermondeEncode(k int, data [][]byte, idx, size int) ([]byte, error) {
	rows, err := VandermondeRows(k, 32)
	if err != nil {
		return nil, err
	}
	if idx < k || idx >= 32 {
		return nil, fmt.Errorf("fec: FEC index %d out of range", idx)
	}
	row := rows[(idx-k)*k : (idx-k+1)*k]
	out := make([]byte, size)
	for j := 0; j < k; j++ {
		AddMul(out, data[j], row[j], size)
	}
	return out, nil
}

// VandermondeDecode recovers missing data packets. have maps packet index → bytes (size
// each) with at least k entries (data and/or FEC). It returns the missing data packets 0…k-1.
func VandermondeDecode(k int, have map[int][]byte, size int) (map[int][]byte, error) {
	rows, err := VandermondeRows(k, 32)
	if err != nil {
		return nil, err
	}
	out := map[int][]byte{}
	var missing []int
	for i := 0; i < k; i++ {
		if _, ok := have[i]; !ok {
			missing = append(missing, i)
		}
	}
	if len(missing) == 0 {
		return out, nil
	}
	var fecIdx []int
	for i := range have {
		if i >= k {
			fecIdx = append(fecIdx, i)
		}
	}
	sort.Ints(fecIdx)
	if len(fecIdx) < len(missing) {
		return nil, fmt.Errorf("fec: need %d packets, have %d", k, k-len(missing)+len(fecIdx))
	}
	m := make([]byte, k*k)
	src := make([][]byte, k)
	f := 0
	for i := 0; i < k; i++ {
		if p, ok := have[i]; ok {
			m[i*k+i] = 1
			src[i] = p
			continue
		}
		idx := fecIdx[f]
		f++
		if idx >= 32 {
			return nil, fmt.Errorf("fec: FEC index %d out of range", idx)
		}
		copy(m[i*k:], rows[(idx-k)*k:(idx-k+1)*k])
		src[i] = have[idx]
	}
	d, err := Invert(m, k)
	if err != nil {
		return nil, err
	}
	for _, i := range missing {
		buf := make([]byte, size)
		for j := 0; j < k; j++ {
			AddMul(buf, src[j], d[i*k+j], min(size, len(src[j])))
		}
		out[i] = buf
	}
	return out, nil
}

// ---- Cauchy rows (OpenHD / wifibroadcast) -----------------------------------------------

// CauchyCoef is the coefficient of primary j in secondary r: inv((128 + r) XOR j).
func CauchyCoef(r, j int) byte { return Inv(byte((128 + r) ^ j)) }

// CauchyEncode returns m secondaries of length L from k symbols (each ≤ L, zero padded).
// A symbol is LE16(size) ‖ data in OpenHD.
func CauchyEncode(symbols [][]byte, m, L int) [][]byte {
	out := make([][]byte, m)
	for r := 0; r < m; r++ {
		s := make([]byte, L)
		for j, p := range symbols {
			AddMul(s, p, CauchyCoef(r, j), min(len(p), L))
		}
		out[r] = s
	}
	return out
}

// CauchyDecode recovers missing primaries: prim maps j → symbol (≤ L bytes), sec maps
// r → secondary (L bytes). It returns every missing j (L bytes each).
func CauchyDecode(k int, prim, sec map[int][]byte, L int) (map[int][]byte, error) {
	var missing []int
	for j := 0; j < k; j++ {
		if _, ok := prim[j]; !ok {
			missing = append(missing, j)
		}
	}
	e := len(missing)
	res := map[int][]byte{}
	if e == 0 {
		return res, nil
	}
	rows := make([]int, 0, len(sec))
	for r := range sec {
		rows = append(rows, r)
	}
	sort.Ints(rows)
	if len(rows) < e {
		return nil, fmt.Errorf("fec: need %d secondaries, have %d", e, len(rows))
	}
	rows = rows[:e]
	rhs := make([][]byte, e)
	for a, r := range rows {
		acc := make([]byte, L)
		copy(acc, sec[r])
		for j, p := range prim {
			AddMul(acc, p, CauchyCoef(r, j), min(len(p), L))
		}
		rhs[a] = acc
	}
	m := make([]byte, e*e)
	for a, r := range rows {
		for b, j := range missing {
			m[a*e+b] = CauchyCoef(r, j)
		}
	}
	mi, err := Invert(m, e)
	if err != nil {
		return nil, err
	}
	for b, j := range missing {
		s := make([]byte, L)
		for a := 0; a < e; a++ {
			AddMul(s, rhs[a], mi[b*e+a], L)
		}
		res[j] = s
	}
	return res, nil
}
