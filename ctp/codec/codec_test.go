package codec

import (
	"bytes"
	"math/rand"
	"testing"

	"github.com/hjkoskel/capturetonkeli/ctp/wire"
)

func TestCOBSRoundTrip(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	for n := 0; n < 1100; n += 1 + n/7 {
		for _, zeroP := range []int{0, 2, 50, 100} {
			src := make([]byte, n)
			for i := range src {
				if rng.Intn(100) < zeroP {
					src[i] = 0
				} else {
					src[i] = byte(1 + rng.Intn(255))
				}
			}
			enc := COBSEncode(nil, src)
			if bytes.IndexByte(enc, 0) >= 0 {
				t.Fatalf("zero in encoding n=%d", n)
			}
			if len(enc) > MaxEncodedLen(n) {
				t.Fatalf("n=%d enc %d > max %d", n, len(enc), MaxEncodedLen(n))
			}
			dec, err := COBSDecode(nil, enc)
			if err != nil || !bytes.Equal(dec, src) {
				t.Fatalf("n=%d roundtrip failed: %v", n, err)
			}
		}
	}
}

func TestDecoderResync(t *testing.T) {
	var stream []byte
	stream = append(stream, "garbage before sync"...)
	h := wire.Header{Ver: 1, Type: wire.TypePing, Seq: 1, ReqID: 7}
	stream = EncodeFrame(append(stream, 0), h, []byte{1, 2, 3, 4})
	bad := EncodeFrame(nil, h, []byte{9, 9})
	bad[2] ^= 0x01
	stream = append(stream, bad...)
	stream = append(stream, 0, 0, 0)
	stream = EncodeFrame(stream, wire.Header{Ver: 1, Type: wire.TypeRspOK, ReqID: 8}, nil)
	d := NewDecoder(MaxFrame)
	var got []uint16
	for _, b := range stream { // byte-at-a-time
		d.Feed([]byte{b}, func(h wire.Header, body []byte) { got = append(got, h.ReqID) })
	}
	if len(got) != 2 || got[0] != 7 || got[1] != 8 {
		t.Fatalf("got %v", got)
	}
	st := d.Stats()
	if st.CRCErrors+st.COBSErrors != 1 || st.SkippedBytes != uint64(len("garbage before sync")) {
		t.Fatalf("stats %+v", st)
	}
}

func TestDecoderOversize(t *testing.T) {
	d := NewDecoder(MaxFrame)
	d.SetSynced()
	big := bytes.Repeat([]byte{0x55}, MaxFrame*2)
	n := 0
	d.Feed(append(big, 0), func(wire.Header, []byte) { n++ })
	d.Feed(EncodeFrame(nil, wire.Header{Ver: 1, Type: 2}, nil), func(wire.Header, []byte) { n++ })
	if n != 1 || d.Stats().Oversize != 1 {
		t.Fatalf("n=%d stats %+v", n, d.Stats())
	}
}

func FuzzDecoder(f *testing.F) {
	f.Add([]byte{0, 1, 2, 0})
	f.Add(EncodeFrame([]byte{0}, wire.Header{Ver: 1, Type: 1}, []byte{1, 2, 3}))
	f.Fuzz(func(t *testing.T, b []byte) {
		d := NewDecoder(MaxFrame)
		d.Feed(b, func(h wire.Header, body []byte) {
			if len(body) > MaxFrame {
				t.Fatal("body too large")
			}
		})
		// recovery: a valid frame after a delimiter is always decoded
		ok := false
		d.Feed(EncodeFrame([]byte{0}, wire.Header{Ver: 1, Type: 2, ReqID: 0xBEEF}, nil), func(h wire.Header, _ []byte) { ok = ok || h.ReqID == 0xBEEF })
		if !ok {
			t.Fatal("no recovery")
		}
	})
}
