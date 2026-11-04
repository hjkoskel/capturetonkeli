package filter

import (
	"encoding/json"
	"testing"

	"github.com/hjkoskel/capturetonkeli/ctp/codec"
	"github.com/hjkoskel/capturetonkeli/ctp/wire"
)

func TestEmptyCRC(t *testing.T) {
	c, err := Set{}.CRC32()
	if err != nil || c != wire.EmptyFilterCRC {
		t.Fatalf("crc %08x err %v", c, err)
	}
}

func TestParseTerm(t *testing.T) {
	tm, err := ParseTerm(0, "8?")
	if err != nil || tm.Value[0] != 0x80 || tm.Mask[0] != 0xF0 {
		t.Fatalf("%+v %v", tm, err)
	}
	tm, err = ParseTerm(10, "aa:bb:cc:dd:ee:ff")
	if err != nil || len(tm.Value) != 6 || tm.Mask[5] != 0xff {
		t.Fatalf("%+v %v", tm, err)
	}
	tm, err = ParseTerm(24, "88-8e ?? 03")
	if err != nil || len(tm.Value) != 4 || tm.Mask[2] != 0 || tm.Value[3] != 3 {
		t.Fatalf("%+v %v", tm, err)
	}
	if tm, err = ParseTerm(0, "80/fc"); err != nil || tm.Mask[0] != 0xfc {
		t.Fatalf("%+v %v", tm, err)
	}
	for _, bad := range []string{"", "8", "zz", "80/f", "00112233445566778899aabbccddeeff00"} {
		if _, err := ParseTerm(0, bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestEncodeDecodeJSON(t *testing.T) {
	rm := int8(-70)
	s := Set{Version: 1, Rules: append([]Rule{Beacons(), DropBeacons()}, EAPOL()...), Global: Global{DropFCSErr: true, RSSIMin: &rm}}
	s.Rules[1].Enabled = false
	b, err := s.Encode()
	if err != nil {
		t.Fatal(err)
	}
	d, err := Decode(b)
	if err != nil || len(d.Rules) != 3 {
		t.Fatalf("decode %v rules=%d", err, len(d.Rules))
	}
	b2, _ := d.Encode()
	if codec.CRC32(b) != codec.CRC32(b2) {
		t.Fatal("re-encode differs")
	}
	js, _ := json.Marshal(s)
	var s2 Set
	if err := json.Unmarshal(js, &s2); err != nil {
		t.Fatal(err)
	}
	c1, _ := s.CRC32()
	c2, _ := s2.CRC32()
	if c1 != c2 {
		t.Fatalf("json roundtrip crc %08x != %08x\n%s", c1, c2, js)
	}
	// strictness
	for _, mut := range []func([]byte) []byte{
		func(b []byte) []byte { return append(b, 0) },
		func(b []byte) []byte { b[3] = 1; return b },
		func(b []byte) []byte { b[1] |= 0x80; return b },
		func(b []byte) []byte { return b[:len(b)-1] },
	} {
		c := mut(append([]byte(nil), b...))
		if _, err := Decode(c); err == nil {
			t.Errorf("mutated body accepted: % x", c)
		}
	}
}

func TestMatchSemantics(t *testing.T) {
	beacon := []byte{0x80, 0, 0, 0}
	data := []byte{0x88, 0x02}
	s := Set{Rules: []Rule{Beacons()}}
	m := wire.PktMeta{OrigLen: 4}
	if !s.Match(beacon, m) || s.Match(data, m) {
		t.Fatal("beacon rule")
	}
	if !(Set{}).Match(data, m) {
		t.Fatal("empty set must pass")
	}
	neg := Set{Rules: []Rule{DropBeacons()}}
	if neg.Match(beacon, m) || !neg.Match(data, m) {
		t.Fatal("negate")
	}
	far := Set{Rules: []Rule{{Enabled: true, Terms: []Term{{Offset: 100, Value: []byte{1}, Mask: []byte{0xff}}}}}}
	if far.Match(beacon, m) {
		t.Fatal("out of range must not match")
	}
	far.Rules[0].Terms[0].Negate = true
	if !far.Match(beacon, m) {
		t.Fatal("negated out of range matches (App. A XOR rule)")
	}
	fcs := Set{Global: Global{DropFCSErr: true}}
	if fcs.Match(beacon, wire.PktMeta{Flags: wire.PktFCSErr}) {
		t.Fatal("fcs drop")
	}
}

func FuzzDecode(f *testing.F) {
	b, _ := Set{Rules: EAPOL()}.Encode()
	f.Add(b)
	f.Fuzz(func(t *testing.T, b []byte) {
		s, err := Decode(b)
		if err != nil {
			return
		}
		b2, err := s.Encode()
		if err != nil {
			// offsets > MaxOffset are legal on the wire but not in the editor model
			return
		}
		if string(b2) != string(b) {
			t.Fatalf("roundtrip mismatch\n% x\n% x", b, b2)
		}
	})
}
