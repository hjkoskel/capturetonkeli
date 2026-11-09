package fec

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"os"
	"testing"
)

func fixture(t *testing.T, name string, v any) {
	t.Helper()
	b, err := os.ReadFile("../../test/js/fixtures/" + name)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, v); err != nil {
		t.Fatal(err)
	}
}

func unhex(s string) []byte { b, _ := hex.DecodeString(s); return b }

// hx-esp32-cam-fpv C reference (same fixture as test/js/hxfpv.test.mjs).
func TestVandermondeMatchesC(t *testing.T) {
	var v struct {
		Matrices []struct {
			K, N int
			Rows string
		}
		Blocks []struct {
			K, N, Size int
			Packets    []string
		}
	}
	fixture(t, "hxfpv-fec.json", &v)
	for _, m := range v.Matrices {
		rows, err := VandermondeRows(m.K, m.N)
		if err != nil || hex.EncodeToString(rows) != m.Rows {
			t.Fatalf("k=%d: %v %x", m.K, err, rows)
		}
	}
	for _, b := range v.Blocks {
		p := make([][]byte, len(b.Packets))
		for i, s := range b.Packets {
			p[i] = unhex(s)
		}
		for i := b.K; i < b.N; i++ {
			got, err := VandermondeEncode(b.K, p[:b.K], i, b.Size)
			if err != nil || !bytes.Equal(got, p[i]) {
				t.Fatalf("k=%d fec %d: %v", b.K, i, err)
			}
		}
		for drop := 1; drop <= min(b.K, b.N-b.K); drop++ {
			have := map[int][]byte{}
			for i := drop; i < b.N; i++ {
				have[i] = p[i]
			}
			rec, err := VandermondeDecode(b.K, have, b.Size)
			if err != nil {
				t.Fatal(err)
			}
			for i := 0; i < drop; i++ {
				if !bytes.Equal(rec[i], p[i]) {
					t.Fatalf("k=%d lost %d, packet %d", b.K, drop, i)
				}
			}
		}
	}
	if _, err := VandermondeDecode(6, map[int][]byte{0: make([]byte, 4)}, 4); err == nil {
		t.Fatal("decoded without enough packets")
	}
}

func TestCauchyAnyLossPattern(t *testing.T) {
	const k, m, L = 6, 3, 300
	syms := make([][]byte, k)
	for j := range syms {
		syms[j] = make([]byte, 50+j*40)
		for i := range syms[j] {
			syms[j][i] = byte(i*7 + j*13)
		}
	}
	sec := CauchyEncode(syms, m, L)
	secMap := map[int][]byte{}
	for r, s := range sec {
		secMap[r] = s
	}
	for mask := 0; mask < 1<<k; mask++ {
		prim := map[int][]byte{}
		lost := 0
		for j := 0; j < k; j++ {
			if mask&(1<<j) != 0 {
				lost++
			} else {
				prim[j] = syms[j]
			}
		}
		if lost > m {
			continue
		}
		rec, err := CauchyDecode(k, prim, secMap, L)
		if err != nil {
			t.Fatal(err)
		}
		for j, s := range rec {
			if !bytes.Equal(s[:len(syms[j])], syms[j]) {
				t.Fatalf("mask %b: symbol %d", mask, j)
			}
		}
	}
}
