package pcapng

import (
	"bytes"
	"testing"

	"github.com/hjkoskel/capturetonkeli/ctp/wire"
)

func TestWriteRead(t *testing.T) {
	var buf bytes.Buffer
	w, err := NewWriter(&buf, WriterOptions{UserAppl: "test", Hardware: "XIAO ESP32-C5", Snaplen: 256})
	if err != nil {
		t.Fatal(err)
	}
	frames := []struct {
		m       wire.PktMeta
		data    []byte
		comment string
	}{
		{wire.PktMeta{TsUS: 1 << 33, Channel: 6, RSSI: -50, Noise: -95, Rate: 12, BBFormat: wire.BB11g, OrigLen: 5, CapLen: 5}, []byte{0x80, 0, 0, 0, 1}, "cfg_gen 3"},
		{wire.PktMeta{TsUS: 5, Channel: 36, RSSI: -70, Noise: -96, Rate: 7, BBFormat: wire.BBHT, OrigLen: 1500, CapLen: 3, Flags: wire.PktTruncated | wire.Pkt5GHz}, []byte{0x88, 1, 2}, ""},
		{wire.PktMeta{TsUS: 6, Channel: 1, Flags: wire.PktRateUnknown | wire.PktNoFCS | wire.PktFCSErr, OrigLen: 2, CapLen: 2}, []byte{0xd4, 0}, ""},
	}
	for _, f := range frames {
		if err := w.WritePacket(f.m, f.data, f.comment); err != nil {
			t.Fatal(err)
		}
	}
	f, err := Read(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if f.LinkType != LinkTypeRadiotap || f.TsResol != 6 || f.UserAppl != "test" || len(f.Packets) != 3 {
		t.Fatalf("%+v", f)
	}
	for i, p := range f.Packets {
		if p.TsUS != frames[i].m.TsUS || !bytes.Equal(p.Frame, frames[i].data) || p.Comment != frames[i].comment {
			t.Fatalf("packet %d: %+v", i, p)
		}
		if int(p.OrigLen)-len(p.Radiotap) != int(frames[i].m.OrigLen) {
			t.Fatalf("packet %d orig len", i)
		}
		if len(p.Radiotap)%1 != 0 || p.Radiotap[0] != 0 {
			t.Fatalf("radiotap version")
		}
	}
	// HT packet carries the MCS field (27-byte radiotap), legacy one the rate (24)
	if len(f.Packets[0].Radiotap) != 24 || len(f.Packets[1].Radiotap) != 27 || len(f.Packets[2].Radiotap) != 24 {
		t.Fatalf("radiotap lengths %d %d %d", len(f.Packets[0].Radiotap), len(f.Packets[1].Radiotap), len(f.Packets[2].Radiotap))
	}
}
