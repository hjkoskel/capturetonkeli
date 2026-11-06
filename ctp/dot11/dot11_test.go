package dot11

import (
	"testing"
)

func beacon(ssid string, ch uint8) []byte {
	b := []byte{0x80, 0, 0, 0, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 2, 1, 2, 3, 4, 5, 2, 1, 2, 3, 4, 5, 0x30, 0x01}
	b = append(b, make([]byte, 12)...)
	b = append(b, 0, byte(len(ssid)))
	b = append(b, ssid...)
	return append(b, 3, 1, ch, 48, 2, 1, 0)
}

func TestBeacon(t *testing.T) {
	h := Parse(beacon("home", 6))
	if h.Name != "Beacon" || h.Seq != 0x13 || h.Truncated {
		t.Fatalf("%+v", h)
	}
	if s, ok := h.SSID(); !ok || s != "home" {
		t.Fatalf("ssid %q %v", s, ok)
	}
	if c, ok := h.DSChannel(); !ok || c != 6 || !h.HasRSN() {
		t.Fatalf("ds %d %v rsn %v", c, ok, h.HasRSN())
	}
	if h.BSSID().String() != "02:01:02:03:04:05" {
		t.Fatal(h.BSSID())
	}
}

func TestControlAndQoS(t *testing.T) {
	ack := Parse([]byte{0xd4, 0, 0, 0, 1, 2, 3, 4, 5, 6})
	if ack.Name != "ACK" || ack.Addr2 != nil || ack.HeaderLen != 10 {
		t.Fatalf("%+v", ack)
	}
	q := make([]byte, 30)
	q[0], q[1], q[24] = 0x88, 0x01, 0x05
	h := Parse(q)
	if !h.QoS || h.TID != 5 || h.HeaderLen != 26 || h.BSSID() == nil {
		t.Fatalf("%+v", h)
	}
}

func FuzzParse(f *testing.F) {
	f.Add(beacon("x", 1))
	f.Add([]byte{0x88, 0x03})
	f.Fuzz(func(t *testing.T, b []byte) {
		h := Parse(b)
		_ = h.IEs()
		_, _ = h.SSID()
		_ = h.BSSID()
	})
}
