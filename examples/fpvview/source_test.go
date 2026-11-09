package main

import (
	"testing"

	"github.com/hjkoskel/capturetonkeli/ctp/pcapng"
	"github.com/hjkoskel/capturetonkeli/ctp/wire"
)

// The signal is found in the radiotap headers capturetonkeli writes (one dongle and a group).
func TestRadiotapSignal(t *testing.T) {
	for _, bb := range []uint8{wire.BB11b, wire.BB11g, wire.BBHT, wire.BBHESU} {
		m := wire.PktMeta{TsUS: 123, Channel: 149, RSSI: -61, Noise: -95, Rate: 12, BBFormat: bb, OrigLen: 30, CapLen: 30}
		for _, ants := range [][]pcapng.Antenna{nil, {{Index: 0, RSSI: -61}, {Index: 1, RSSI: -70}}} {
			if v, ok := radiotapSignal(pcapng.RadiotapAntennas(m, ants)); !ok || v != -61 {
				t.Fatalf("bb %d antennas %v: %d %v", bb, ants, v, ok)
			}
		}
	}
	if _, ok := radiotapSignal([]byte{0, 0, 8, 0}); ok {
		t.Fatal("short header")
	}
}

func TestFreqToChannel(t *testing.T) {
	for mhz, ch := range map[int]int{5700: 140, 5745: 149, 2442: 7, 2484: 14, 5001: 0} {
		if freqToChannel(mhz) != ch {
			t.Fatalf("%d MHz", mhz)
		}
	}
}
