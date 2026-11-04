// Package difftest runs the C firmware filter against the Go reference on
// random (filter, frame, meta) cases (docs/07 M1 differential test).
// Set CTP_FILTER_DIFF to the filter_diff binary (make diff-test does).
package difftest

import (
	"bufio"
	"bytes"
	"encoding/hex"
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"

	"github.com/hjkoskel/capturetonkeli/ctp/filter"
	"github.com/hjkoskel/capturetonkeli/ctp/wire"
)

func randTerm(rng *rand.Rand, frameHint []byte) filter.Term {
	n := 1 + rng.Intn(filter.MaxTermLen)
	off := uint16(rng.Intn(64))
	if rng.Intn(8) == 0 {
		off = uint16(rng.Intn(filter.MaxOffset + 1))
	}
	v, m := make([]byte, n), make([]byte, n)
	for i := range v {
		m[i] = []byte{0xff, 0xf0, 0x0f, 0xfc, 0x0c, 0x00, byte(rng.Intn(256))}[rng.Intn(7)]
		if int(off)+i < len(frameHint) && rng.Intn(3) > 0 {
			v[i] = frameHint[int(off)+i] // likely matches
		} else {
			v[i] = byte(rng.Intn(256))
		}
	}
	return filter.Term{Offset: off, Value: v, Mask: m, Negate: rng.Intn(5) == 0}
}

func randSet(rng *rand.Rand, hint []byte) filter.Set {
	var s filter.Set
	for r := rng.Intn(5); r > 0; r-- {
		ru := filter.Rule{Enabled: rng.Intn(6) > 0}
		for t := 1 + rng.Intn(filter.MaxTerms); t > 0; t-- {
			ru.Terms = append(ru.Terms, randTerm(rng, hint))
		}
		s.Rules = append(s.Rules, ru)
	}
	if rng.Intn(3) == 0 {
		s.Global.DropFCSErr = true
	}
	if rng.Intn(4) == 0 {
		v := int8(-100 + rng.Intn(70))
		s.Global.RSSIMin = &v
	}
	if rng.Intn(5) == 0 {
		v := uint16(rng.Intn(200))
		s.Global.LenMin = &v
	}
	if rng.Intn(5) == 0 {
		v := uint16(rng.Intn(2000))
		s.Global.LenMax = &v
	}
	return s
}

func TestFilterDifferential(t *testing.T) {
	bin := os.Getenv("CTP_FILTER_DIFF")
	if bin == "" {
		t.Skip("set CTP_FILTER_DIFF to the filter_diff binary (make diff-test)")
	}
	n := 100000
	if v, err := strconv.Atoi(os.Getenv("CTP_DIFF_N")); err == nil {
		n = v
	}
	seed := int64(1)
	if v, err := strconv.ParseInt(os.Getenv("CTP_DIFF_SEED"), 10, 64); err == nil {
		seed = v
	}
	rng := rand.New(rand.NewSource(seed))
	type tc struct {
		body, frame []byte
		meta        wire.PktMeta
		want        string
	}
	cases := make([]tc, 0, n)
	var in bytes.Buffer
	for i := 0; i < n; i++ {
		fr := make([]byte, rng.Intn(300))
		rng.Read(fr)
		if len(fr) > 0 && rng.Intn(2) == 0 {
			fr[0] = []byte{0x80, 0x40, 0x88, 0x08, 0xd4, 0xb4}[rng.Intn(6)]
		}
		s := randSet(rng, fr)
		body, err := s.Encode()
		if err != nil {
			t.Fatal(err)
		}
		m := wire.PktMeta{RSSI: int8(-100 + rng.Intn(90)), OrigLen: uint16(len(fr) + rng.Intn(3))}
		if rng.Intn(4) == 0 {
			m.Flags |= wire.PktFCSErr
		}
		want := "0"
		if s.Match(fr, m) {
			want = "1"
		}
		fh, xh := hex.EncodeToString(body), hex.EncodeToString(fr)
		if xh == "" {
			xh = "-"
		}
		fmt.Fprintf(&in, "%s %s %d %d %d\n", fh, xh, m.Flags&1, m.RSSI, m.OrigLen)
		cases = append(cases, tc{body, fr, m, want})
	}
	cmd := exec.Command(bin)
	cmd.Stdin = &in
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("filter_diff: %v", err)
	}
	sc := bufio.NewScanner(bytes.NewReader(out))
	i := 0
	for sc.Scan() {
		got := strings.TrimSpace(sc.Text())
		c := cases[i]
		if got != c.want {
			t.Fatalf("case %d (seed %d) mismatch: C=%s Go=%s\nfilter %x\nframe %x\nmeta %+v\n(add as a vector to testvectors via internal/vectorgen)",
				i, seed, got, c.want, c.body, c.frame, c.meta)
		}
		i++
	}
	if i != n {
		t.Fatalf("filter_diff answered %d of %d cases", i, n)
	}
	t.Logf("%d cases agree (seed %d)", n, seed)
}
