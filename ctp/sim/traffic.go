package sim

import (
	"encoding/binary"
	"hash/crc32"
	"math/rand"

	"github.com/hjkoskel/capturetonkeli/ctp/wire"
)

// TrafficModel describes the synthetic RF environment.
type TrafficModel struct {
	BSSPerChannel int     // access points per channel (beacons 10/s each)
	FramesPerSec  int     // non-beacon frames per second per channel
	FCSErrorRate  float64 // fraction of frames with a bad FCS
}

// DefaultTraffic is a moderately busy channel.
var DefaultTraffic = TrafficModel{BSSPerChannel: 3, FramesPerSec: 400, FCSErrorRate: 0.01}

// RxFrame is one synthetic received frame (full bytes incl. FCS).
type RxFrame struct {
	Data     []byte
	RSSI     int8
	Noise    int8
	Rate     uint8
	BBFormat uint8
	FCSErr   bool
}

type bss struct {
	mac  [6]byte
	ssid string
	rssi int8
}

type generator struct {
	rng     *rand.Rand
	model   TrafficModel
	bssByCh map[uint8][]bss
	seqNo   uint16
	carry   float64
}

func newGenerator(seed int64, m TrafficModel) *generator {
	return &generator{rng: rand.New(rand.NewSource(seed)), model: m, bssByCh: map[uint8][]bss{}}
}

func (g *generator) bsss(ch uint8) []bss {
	if b, ok := g.bssByCh[ch]; ok {
		return b
	}
	var out []bss
	for i := 0; i < g.model.BSSPerChannel; i++ {
		var mac [6]byte
		g.rng.Read(mac[:])
		mac[0] = (mac[0] &^ 1) | 2 // unicast, locally administered
		out = append(out, bss{mac: mac, ssid: ssidName(ch, i), rssi: int8(-40 - g.rng.Intn(45))})
	}
	g.bssByCh[ch] = out
	return out
}

func ssidName(ch uint8, i int) string {
	return "sim-ch" + itoa(int(ch)) + "-" + string(rune('a'+i))
}

func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	var b [8]byte
	i := len(b)
	for v > 0 {
		i--
		b[i] = byte('0' + v%10)
		v /= 10
	}
	return string(b[i:])
}

// tick returns the frames received on channel ch during dtSec seconds.
func (g *generator) tick(ch uint8, dtSec float64) []RxFrame {
	aps := g.bsss(ch)
	rate := float64(len(aps))*10 + float64(g.model.FramesPerSec)
	g.carry += rate * dtSec
	n := int(g.carry)
	g.carry -= float64(n)
	out := make([]RxFrame, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, g.frame(ch, aps))
	}
	return out
}

func (g *generator) frame(ch uint8, aps []bss) RxFrame {
	g.seqNo++
	five := ch > 14
	var ap bss
	if len(aps) > 0 {
		ap = aps[g.rng.Intn(len(aps))]
	}
	sta := [6]byte{0x02, 0x11, 0x22, 0x33, 0x44, byte(g.rng.Intn(8))}
	bcast := [6]byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff}
	var f []byte
	bb, rate := wire.BB11g, uint8(12) // 6 Mb/s
	if !five && g.rng.Intn(4) == 0 {
		bb, rate = wire.BB11b, 2
	}
	r := g.rng.Intn(100)
	switch {
	case r < 12: // beacon
		f = mgmt(0x80, bcast, ap.mac, ap.mac, g.seqNo)
		f = append(f, make([]byte, 8)...)     // timestamp
		f = append(f, 0x64, 0x00, 0x11, 0x04) // interval, capability
		f = append(f, 0, byte(len(ap.ssid)))
		f = append(f, ap.ssid...)
		f = append(f, 1, 4, 0x8c, 0x12, 0x98, 0x24) // rates
		f = append(f, 3, 1, ch)                     // DS param
	case r < 18: // probe request
		f = mgmt(0x40, bcast, sta, bcast, g.seqNo)
		f = append(f, 0, 0, 1, 4, 0x8c, 0x12, 0x98, 0x24)
	case r < 22: // probe response
		f = mgmt(0x50, sta, ap.mac, ap.mac, g.seqNo)
		f = append(f, make([]byte, 8)...)
		f = append(f, 0x64, 0x00, 0x11, 0x04, 0, byte(len(ap.ssid)))
		f = append(f, ap.ssid...)
	case r < 40: // ACK
		f = []byte{0xd4, 0x00, 0x00, 0x00}
		f = append(f, sta[:]...)
	case r < 46: // RTS
		f = []byte{0xb4, 0x00, 0x20, 0x00}
		f = append(f, ap.mac[:]...)
		f = append(f, sta[:]...)
	case r < 48: // EAPOL (QoS data)
		f = dataHdr(0x88, ap.mac, sta, ap.mac, g.seqNo)
		f = append(f, 0x00, 0x00) // QoS
		f = append(f, 0xaa, 0xaa, 0x03, 0, 0, 0, 0x88, 0x8e)
		f = append(f, make([]byte, 95)...)
	default: // QoS data
		f = dataHdr(0x88, ap.mac, sta, ap.mac, g.seqNo)
		f = append(f, 0x00, 0x00)
		f = append(f, 0xaa, 0xaa, 0x03, 0, 0, 0, 0x08, 0x00)
		pl := 20 + g.rng.Intn(1400)
		start := len(f)
		f = append(f, make([]byte, pl)...)
		g.rng.Read(f[start:])
		if g.rng.Intn(3) > 0 {
			bb, rate = wire.BBHT, uint8(g.rng.Intn(8))
		}
	}
	fcs := crc32.ChecksumIEEE(f)
	f = binary.LittleEndian.AppendUint32(f, fcs)
	bad := g.rng.Float64() < g.model.FCSErrorRate
	if bad {
		f[len(f)-1] ^= 0x5a
	}
	rssi := ap.rssi + int8(g.rng.Intn(7)-3)
	if rssi > -10 {
		rssi = -10
	}
	return RxFrame{Data: f, RSSI: rssi, Noise: -95, Rate: rate, BBFormat: bb, FCSErr: bad}
}

func mgmt(fc0 byte, a1, a2, a3 [6]byte, seq uint16) []byte {
	f := []byte{fc0, 0x00, 0x00, 0x00}
	f = append(f, a1[:]...)
	f = append(f, a2[:]...)
	f = append(f, a3[:]...)
	return binary.LittleEndian.AppendUint16(f, seq<<4)
}

func dataHdr(fc0 byte, a1, a2, a3 [6]byte, seq uint16) []byte {
	f := []byte{fc0, 0x02, 0x2c, 0x00} // FromDS
	f = append(f, a1[:]...)
	f = append(f, a2[:]...)
	f = append(f, a3[:]...)
	return binary.LittleEndian.AppendUint16(f, seq<<4)
}
