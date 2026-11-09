// Package mavlink parses MAVLink v1/v2 as carried in the OpenHD telemetry stream (docs/13 §7):
// framing, CRC with CRC_EXTRA (common + ardupilotmega + OpenHD dialect), decoders for the
// messages the web UI shows, a flight-state summary and .tlog records. Port of js/mavlink.js.
// Field offsets are MAVLink wire order (fields sorted by type size).
package mavlink

import (
	"encoding/binary"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Info is the CRC_EXTRA and payload length range of a message id.
type Info struct {
	CRC      uint8
	Min, Max int
}

var table = func() map[uint32]Info {
	t := map[uint32]Info{}
	for _, e := range strings.Fields(crcTable) {
		f := strings.Split(e, ":")
		id, _ := strconv.Atoi(f[0])
		c, _ := strconv.Atoi(f[1])
		mn, _ := strconv.Atoi(f[2])
		mx, _ := strconv.Atoi(f[3])
		t[uint32(id)] = Info{CRC: uint8(c), Min: mn, Max: mx}
	}
	return t
}()

// MsgInfo returns the table entry of a message id.
func MsgInfo(id uint32) (Info, bool) { i, ok := table[id]; return i, ok }

// CRCX25 is the X.25 / MCRF4XX CRC used by MAVLink (start with 0xffff).
func CRCX25(b []byte, crc uint16) uint16 {
	for _, c := range b {
		t := (c ^ byte(crc)) & 0xff
		t ^= t << 4
		crc = crc>>8 ^ uint16(t)<<8 ^ uint16(t)<<3 ^ uint16(t)>>4
	}
	return crc
}

// CRC states.
const (
	CRCUnknown = 0 // message id not in the table
	CRCOK      = 1
	CRCBad     = 2
)

// Message is one parsed frame.
type Message struct {
	V       int // 1 or 2
	ID      uint32
	Sys     uint8
	Comp    uint8
	Seq     uint8
	Len     int
	Signed  bool
	Payload []byte // zero-extended to the full length when the id is known
	CRC     int    // CRCOK, CRCBad or CRCUnknown
	Raw     []byte // the frame bytes
}

// Parse splits a buffer of concatenated MAVLink frames; garbage between frames is skipped.
func Parse(buf []byte) []Message {
	var out []Message
	for i := 0; i < len(buf); {
		switch stx := buf[i]; {
		case stx == 0xfd && i+12 <= len(buf):
			n := int(buf[i+1])
			signed := buf[i+2]&1 != 0
			total := 12 + n
			if signed {
				total += 13
			}
			if i+total > len(buf) {
				return out
			}
			id := uint32(buf[i+7]) | uint32(buf[i+8])<<8 | uint32(buf[i+9])<<16
			out = append(out, finish(Message{V: 2, ID: id, Seq: buf[i+4], Sys: buf[i+5], Comp: buf[i+6], Len: n, Signed: signed}, buf, i, 10, n, total))
			i += total
		case stx == 0xfe && i+8 <= len(buf):
			n := int(buf[i+1])
			total := 8 + n
			if i+total > len(buf) {
				return out
			}
			out = append(out, finish(Message{V: 1, ID: uint32(buf[i+5]), Seq: buf[i+2], Sys: buf[i+3], Comp: buf[i+4], Len: n}, buf, i, 6, n, total))
			i += total
		default:
			i++
		}
	}
	return out
}

func finish(m Message, buf []byte, i, hdr, n, total int) Message {
	pl := buf[i+hdr : i+hdr+n]
	m.Raw = append([]byte(nil), buf[i:i+total]...)
	if info, ok := table[m.ID]; ok {
		crc := CRCX25([]byte{info.CRC}, CRCX25(buf[i+1:i+hdr+n], 0xffff))
		if crc == binary.LittleEndian.Uint16(buf[i+hdr+n:]) {
			m.CRC = CRCOK
		} else {
			m.CRC = CRCBad
		}
		m.Payload = make([]byte, max(info.Max, n))
		copy(m.Payload, pl) // v2 truncates trailing zero bytes
	} else {
		m.Payload = append([]byte(nil), pl...)
	}
	return m
}

// Encode builds a MAVLink v2 frame (trailing zeros truncated), for simulators and tests.
func Encode(id uint32, sys, comp, seq uint8, payload []byte) ([]byte, error) {
	info, ok := table[id]
	if !ok {
		return nil, fmt.Errorf("mavlink: unknown message %d", id)
	}
	n := len(payload)
	for n > 1 && payload[n-1] == 0 {
		n--
	}
	f := []byte{0xfd, byte(n), 0, 0, seq, sys, comp, byte(id), byte(id >> 8), byte(id >> 16)}
	f = append(f, payload[:n]...)
	crc := CRCX25([]byte{info.CRC}, CRCX25(f[1:10+n], 0xffff))
	return binary.LittleEndian.AppendUint16(f, crc), nil
}

// ---- decoders ----

type rd []byte

func (d rd) u8(o int) uint8   { return d[o] }
func (d rd) i8(o int) int8    { return int8(d[o]) }
func (d rd) u16(o int) uint16 { return binary.LittleEndian.Uint16(d[o:]) }
func (d rd) i16(o int) int16  { return int16(binary.LittleEndian.Uint16(d[o:])) }
func (d rd) u32(o int) uint32 { return binary.LittleEndian.Uint32(d[o:]) }
func (d rd) i32(o int) int32  { return int32(binary.LittleEndian.Uint32(d[o:])) }
func (d rd) f32(o int) float64 {
	return float64(math.Float32frombits(binary.LittleEndian.Uint32(d[o:])))
}

// Decoded messages.
type (
	Heartbeat struct {
		CustomMode   uint32 `json:"custom_mode"`
		Type         uint8  `json:"type"`
		Autopilot    uint8  `json:"autopilot"`
		BaseMode     uint8  `json:"base_mode"`
		SystemStatus uint8  `json:"system_status"`
	}
	SysStatus struct {
		Load             float64 `json:"load"`
		Voltage          float64 `json:"voltage"`
		Current          float64 `json:"current"`
		DropRate         float64 `json:"drop_rate"`
		BatteryRemaining int8    `json:"battery_remaining"`
	}
	GPSRawInt struct {
		Lat        float64 `json:"lat"`
		Lon        float64 `json:"lon"`
		Alt        float64 `json:"alt"`
		EPH        uint16  `json:"eph"`
		Vel        float64 `json:"vel"`
		COG        float64 `json:"cog"`
		FixType    uint8   `json:"fix_type"`
		Satellites uint8   `json:"satellites"`
	}
	Attitude struct {
		Roll  float64 `json:"roll"`
		Pitch float64 `json:"pitch"`
		Yaw   float64 `json:"yaw"`
	}
	GlobalPositionInt struct {
		Lat    float64  `json:"lat"`
		Lon    float64  `json:"lon"`
		Alt    float64  `json:"alt"`
		RelAlt float64  `json:"rel_alt"`
		VX     float64  `json:"vx"`
		VY     float64  `json:"vy"`
		VZ     float64  `json:"vz"`
		Hdg    *float64 `json:"hdg"`
	}
	RCChannels struct {
		ChanCount uint8 `json:"chancount"`
		RSSI      uint8 `json:"rssi"`
	}
	VFRHUD struct {
		Airspeed    float64 `json:"airspeed"`
		Groundspeed float64 `json:"groundspeed"`
		Alt         float64 `json:"alt"`
		Climb       float64 `json:"climb"`
		Heading     int16   `json:"heading"`
		Throttle    uint16  `json:"throttle"`
	}
	RadioStatus struct {
		RSSI, RemRSSI, Noise, RemNoise uint8
	}
	BatteryStatus struct {
		Consumed  int32   `json:"consumed"`
		Voltage   float64 `json:"voltage"`
		Current   float64 `json:"current"`
		Remaining int8    `json:"remaining"`
	}
	StatusText struct {
		Severity uint8  `json:"severity"`
		Text     string `json:"text"`
	}
	OpenHDWifiLink struct {
		TxBps, RxBps   int32
		TxPps, RxPps   int16
		Freq, RateKbit uint16
		LossPerc       int8
		Width, MCS     uint8
		Pollution      uint8
		CardTemp       int8
	}
	OpenHDVideoAir struct {
		EncoderBitrate, InjectedBitrate, InjectedPps, DroppedFrames int32
		RecommendedBitrate, FECPerc                                 int16
		Link                                                        uint8
	}
	OpenHDComputerStatusExt struct {
		CPUCoreMv   uint16
		OverCurrent uint8
	}
	OpenHDCameraStatusAir struct {
		BitrateKbit, Width, Height, FPS                           uint16
		CamType, CamStatus, Recording, Encoding, KeyframeInterval uint8
	}
	OpenHDVersion struct {
		Major, Minor, Patch, ReleaseType uint8
	}
	OpenHDCoreStatus struct {
		StorageLeftMb                            uint32
		CPUClock                                 uint16
		CPUTemp, Wifi0Temp                       int8
		Undervolt, Platform, Encryption, FCSysID uint8
		Mode                                     uint8
	}
	OpenHDPowerStatus struct {
		Voltage, Current  float64
		Charging, Percent uint8
	}
	Empty struct{}
)

type decoder struct {
	name string
	fn   func(d rd) any
}

var decoders = map[uint32]decoder{
	0: {"HEARTBEAT", func(d rd) any {
		return Heartbeat{CustomMode: d.u32(0), Type: d.u8(4), Autopilot: d.u8(5), BaseMode: d.u8(6), SystemStatus: d.u8(7)}
	}},
	1: {"SYS_STATUS", func(d rd) any {
		return SysStatus{Load: float64(d.u16(12)) / 10, Voltage: float64(d.u16(14)) / 1000, Current: float64(d.i16(16)) / 100, DropRate: float64(d.u16(18)) / 100, BatteryRemaining: d.i8(30)}
	}},
	24: {"GPS_RAW_INT", func(d rd) any {
		return GPSRawInt{Lat: float64(d.i32(8)) / 1e7, Lon: float64(d.i32(12)) / 1e7, Alt: float64(d.i32(16)) / 1000, EPH: d.u16(20), Vel: float64(d.u16(24)) / 100,
			COG: float64(d.u16(26)) / 100, FixType: d.u8(28), Satellites: d.u8(29)}
	}},
	30: {"ATTITUDE", func(d rd) any { return Attitude{Roll: d.f32(4), Pitch: d.f32(8), Yaw: d.f32(12)} }},
	33: {"GLOBAL_POSITION_INT", func(d rd) any {
		g := GlobalPositionInt{Lat: float64(d.i32(4)) / 1e7, Lon: float64(d.i32(8)) / 1e7, Alt: float64(d.i32(12)) / 1000, RelAlt: float64(d.i32(16)) / 1000,
			VX: float64(d.i16(20)) / 100, VY: float64(d.i16(22)) / 100, VZ: float64(d.i16(24)) / 100}
		if h := d.u16(26); h != 65535 {
			v := float64(h) / 100
			g.Hdg = &v
		}
		return g
	}},
	65: {"RC_CHANNELS", func(d rd) any { return RCChannels{ChanCount: d.u8(40), RSSI: d.u8(41)} }},
	74: {"VFR_HUD", func(d rd) any {
		return VFRHUD{Airspeed: d.f32(0), Groundspeed: d.f32(4), Alt: d.f32(8), Climb: d.f32(12), Heading: d.i16(16), Throttle: d.u16(18)}
	}},
	109: {"RADIO_STATUS", func(d rd) any { return RadioStatus{RSSI: d.u8(4), RemRSSI: d.u8(5), Noise: d.u8(7), RemNoise: d.u8(8)} }},
	147: {"BATTERY_STATUS", func(d rd) any {
		mv := 0
		for i := 0; i < 10; i++ {
			c := d.u16(10 + 2*i)
			if c == 0xffff {
				break
			}
			mv += int(c)
		}
		return BatteryStatus{Consumed: d.i32(0), Voltage: float64(mv) / 1000, Current: float64(d.i16(30)) / 100, Remaining: d.i8(35)}
	}},
	253: {"STATUSTEXT", func(d rd) any {
		t := d[1:min(51, len(d))]
		if i := strings.IndexByte(string(t), 0); i >= 0 {
			t = t[:i]
		}
		return StatusText{Severity: d[0], Text: string(t)}
	}},
	1211: {"OPENHD_STATS_MONITOR_MODE_WIFI_LINK", func(d rd) any {
		return OpenHDWifiLink{TxBps: d.i32(0), RxBps: d.i32(4), TxPps: d.i16(20), RxPps: d.i16(22), Freq: d.u16(26), RateKbit: d.u16(28), LossPerc: d.i8(32),
			Width: d.u8(33), MCS: d.u8(34), Pollution: d.u8(37), CardTemp: d.i8(40)}
	}},
	1213: {"OPENHD_STATS_TELEMETRY", func(rd) any { return Empty{} }},
	1214: {"OPENHD_STATS_WB_VIDEO_AIR", func(d rd) any {
		return OpenHDVideoAir{EncoderBitrate: d.i32(0), InjectedBitrate: d.i32(4), InjectedPps: d.i32(8), DroppedFrames: d.i32(12), RecommendedBitrate: d.i16(20),
			FECPerc: d.i16(22), Link: d.u8(26)}
	}},
	1215: {"OPENHD_STATS_WB_VIDEO_AIR_FEC_PERFORMANCE", func(rd) any { return Empty{} }},
	1218: {"OPENHD_ONBOARD_COMPUTER_STATUS_EXTENSION", func(d rd) any { return OpenHDComputerStatusExt{CPUCoreMv: d.u16(0), OverCurrent: d.u8(10)} }},
	1219: {"OPENHD_CAMERA_STATUS_AIR", func(d rd) any {
		return OpenHDCameraStatusAir{BitrateKbit: d.u16(4), Width: d.u16(6), Height: d.u16(8), FPS: d.u16(10), CamType: d.u8(14), CamStatus: d.u8(15),
			Recording: d.u8(17), Encoding: d.u8(18), KeyframeInterval: d.u8(19)}
	}},
	1221: {"OPENHD_VERSION_MESSAGE", func(d rd) any {
		return OpenHDVersion{Major: d.u8(0), Minor: d.u8(1), Patch: d.u8(2), ReleaseType: d.u8(3)}
	}},
	1226: {"OPENHD_SYS_STATUS1", func(rd) any { return Empty{} }},
	1227: {"OPENHD_CORE_STATUS", func(d rd) any {
		return OpenHDCoreStatus{StorageLeftMb: d.u32(0), CPUClock: d.u16(4), CPUTemp: d.i8(14), Wifi0Temp: d.i8(15), Undervolt: d.u8(17), Platform: d.u8(18),
			Encryption: d.u8(19), FCSysID: d.u8(20), Mode: d.u8(21)}
	}},
	1228: {"OPENHD_POWER_STATUS", func(d rd) any {
		return OpenHDPowerStatus{Voltage: float64(d.u16(0)) / 1000, Current: float64(d.i16(2)) / 1000, Charging: d.u8(4), Percent: d.u8(5)}
	}},
}

// MsgName is the message name ("HEARTBEAT") or "#id" for messages not decoded here.
func MsgName(id uint32) string {
	if d, ok := decoders[id]; ok {
		return d.name
	}
	return "#" + strconv.Itoa(int(id))
}

// Decode returns the decoded fields of m (one of the types above), or nil when the message
// is not decoded here or its CRC is not OK.
func Decode(m Message) any {
	d, ok := decoders[m.ID]
	if !ok || m.CRC != CRCOK {
		return nil
	}
	info := table[m.ID]
	if len(m.Payload) < info.Max {
		return nil
	}
	return d.fn(rd(m.Payload))
}

// ---- flight modes (ArduPilot copter/plane, PX4) ----

var (
	copter = []string{"Stabilize", "Acro", "AltHold", "Auto", "Guided", "Loiter", "RTL", "Circle", "", "Land", "", "Drift", "", "Sport", "Flip", "AutoTune",
		"PosHold", "Brake", "Throw", "Avoid_ADSB", "Guided_NoGPS", "Smart_RTL", "FlowHold", "Follow", "ZigZag", "SystemID", "Autorotate", "Auto RTL"}
	plane = []string{"Manual", "Circle", "Stabilize", "Training", "Acro", "FBWA", "FBWB", "Cruise", "Autotune", "", "Auto", "RTL", "Loiter", "Takeoff",
		"Avoid_ADSB", "Guided", "", "QStabilize", "QHover", "QLoiter", "QLand", "QRTL", "QAutotune", "QAcro", "Thermal", "Loiter2QLand"}
	px4        = []string{"", "Manual", "Altitude", "Position", "Auto", "Acro", "Offboard", "Stabilized", "Rattitude"}
	px4Auto    = []string{"", "Ready", "Takeoff", "Hold", "Mission", "RTL", "Land", "", "Follow", "Precland"}
	planeTypes = map[uint8]bool{1: true, 16: true, 19: true, 20: true, 21: true, 22: true, 23: true, 24: true, 25: true}
)

// Autopilot names (MAV_AUTOPILOT).
var Autopilot = map[uint8]string{0: "generic", 3: "ArduPilot", 8: "none", 12: "PX4"}

// MavType names (MAV_TYPE).
var MavType = map[uint8]string{1: "fixed wing", 2: "quadcopter", 3: "coaxial", 4: "helicopter", 6: "GCS", 10: "rover", 13: "hexacopter", 14: "octocopter",
	15: "tricopter", 18: "onboard computer", 19: "VTOL", 20: "VTOL", 21: "VTOL", 22: "VTOL", 26: "gimbal", 27: "ADSB"}

func pick(list []string, i int) string {
	if i >= 0 && i < len(list) {
		return list[i]
	}
	return ""
}

// FlightMode names the heartbeat's mode ("" when custom mode is not enabled).
func FlightMode(hb Heartbeat) string {
	if hb.BaseMode&1 == 0 {
		return ""
	}
	switch hb.Autopilot {
	case 3:
		list := copter
		if planeTypes[hb.Type] {
			list = plane
		}
		if s := pick(list, int(hb.CustomMode)); s != "" {
			return s
		}
		return fmt.Sprintf("mode %d", hb.CustomMode)
	case 12:
		main, sub := int(hb.CustomMode>>16&0xff), int(hb.CustomMode>>24&0xff)
		if main == 4 {
			if s := pick(px4Auto, sub); s != "" {
				return "Auto " + s
			}
			return fmt.Sprintf("Auto %d", sub)
		}
		if s := pick(px4, main); s != "" {
			return s
		}
		return fmt.Sprintf("mode %d", main)
	}
	return fmt.Sprintf("mode %d", hb.CustomMode)
}

// ---- flight state ----

type system struct {
	sys    uint8
	comps  map[uint8]bool
	counts map[uint32]uint64
	last   time.Time
	d      map[string]any
}

// Text is a STATUSTEXT message.
type Text struct {
	T        time.Time `json:"t"`
	Sys      uint8     `json:"sys"`
	Severity uint8     `json:"severity"`
	Text     string    `json:"text"`
}

// FlightState keeps the latest state per MAVLink system; feed it Parse output.
type FlightState struct {
	sys     map[uint8]*system
	total   uint64
	badCRC  uint64
	unknown uint64
	texts   []Text
}

// NewFlightState returns an empty state.
func NewFlightState() *FlightState { return &FlightState{sys: map[uint8]*system{}} }

// Add takes parsed messages received at now.
func (f *FlightState) Add(msgs []Message, now time.Time) {
	for _, m := range msgs {
		f.total++
		switch m.CRC {
		case CRCBad:
			f.badCRC++
			continue
		case CRCUnknown:
			f.unknown++
		}
		s := f.sys[m.Sys]
		if s == nil {
			s = &system{sys: m.Sys, comps: map[uint8]bool{}, counts: map[uint32]uint64{}, d: map[string]any{}}
			f.sys[m.Sys] = s
		}
		s.comps[m.Comp] = true
		s.last = now
		s.counts[m.ID]++
		v := Decode(m)
		if v == nil {
			continue
		}
		s.d[MsgName(m.ID)] = v
		if t, ok := v.(StatusText); ok {
			f.texts = append(f.texts, Text{T: now, Sys: m.Sys, Severity: t.Severity, Text: t.Text})
			if len(f.texts) > 30 {
				f.texts = f.texts[1:]
			}
		}
	}
}

// SystemSummary is one MAVLink system.
type SystemSummary struct {
	Sys    uint8             `json:"sys"`
	Comps  []uint8           `json:"comps"`
	Age    time.Duration     `json:"age"`
	Counts map[uint32]uint64 `json:"counts"`
	D      map[string]any    `json:"d"` // latest decoded message by name
}

// Battery is the flight controller's battery.
type Battery struct {
	Voltage   float64 `json:"voltage"`
	Current   float64 `json:"current"`
	Remaining int8    `json:"remaining"`
}

// Flight summarises the flight controller.
type Flight struct {
	Sys       uint8              `json:"sys"`
	Age       time.Duration      `json:"age"`
	Autopilot string             `json:"autopilot,omitempty"`
	Vehicle   string             `json:"vehicle,omitempty"`
	Armed     *bool              `json:"armed,omitempty"`
	Mode      string             `json:"mode,omitempty"`
	GPS       *GPSRawInt         `json:"gps,omitempty"`
	Pos       *GlobalPositionInt `json:"pos,omitempty"`
	Att       *Attitude          `json:"att,omitempty"`
	HUD       *VFRHUD            `json:"hud,omitempty"`
	Battery   *Battery           `json:"battery,omitempty"`
	RCRSSI    *uint8             `json:"rc_rssi,omitempty"`
}

// Summary is a snapshot of the flight state.
type Summary struct {
	Total   uint64          `json:"total"`
	BadCRC  uint64          `json:"bad_crc"`
	Unknown uint64          `json:"unknown"`
	Systems []SystemSummary `json:"systems"`
	Flight  *Flight         `json:"flight,omitempty"`
	OpenHD  map[string]any  `json:"openhd,omitempty"` // system 101 (the OpenHD air unit)
	Texts   []Text          `json:"texts"`
}

func ptr[T any](v T) *T { return &v }

// Summary returns per-system messages and counts, plus a flight summary for the flight
// controller (the system with a non-GCS, non-companion heartbeat heard most recently).
func (f *FlightState) Summary(now time.Time) Summary {
	s := Summary{Total: f.total, BadCRC: f.badCRC, Unknown: f.unknown}
	ids := make([]int, 0, len(f.sys))
	for id := range f.sys {
		ids = append(ids, int(id))
	}
	sort.Ints(ids)
	for _, id := range ids {
		x := f.sys[uint8(id)]
		ss := SystemSummary{Sys: x.sys, Age: now.Sub(x.last), Counts: map[uint32]uint64{}, D: map[string]any{}}
		for c := range x.comps {
			ss.Comps = append(ss.Comps, c)
		}
		sort.Slice(ss.Comps, func(i, j int) bool { return ss.Comps[i] < ss.Comps[j] })
		for k, v := range x.counts {
			ss.Counts[k] = v
		}
		for k, v := range x.d {
			ss.D[k] = v
		}
		s.Systems = append(s.Systems, ss)
	}
	var fc *SystemSummary
	for i := range s.Systems {
		x := &s.Systems[i]
		hb, ok := x.D["HEARTBEAT"].(Heartbeat)
		if !ok || hb.Type == 18 || hb.Type == 6 || x.Sys == 100 || x.Sys == 101 || x.Sys == 255 {
			continue
		}
		if fc == nil || x.Age < fc.Age {
			fc = x
		}
	}
	if fc == nil {
		for i := range s.Systems {
			x := &s.Systems[i]
			if x.D["GPS_RAW_INT"] != nil || x.D["ATTITUDE"] != nil {
				fc = x
				break
			}
		}
	}
	if fc != nil {
		d := fc.D
		fl := &Flight{Sys: fc.Sys, Age: fc.Age}
		if hb, ok := d["HEARTBEAT"].(Heartbeat); ok {
			fl.Autopilot = Autopilot[hb.Autopilot]
			if fl.Autopilot == "" {
				fl.Autopilot = fmt.Sprintf("autopilot %d", hb.Autopilot)
			}
			fl.Vehicle = MavType[hb.Type]
			if fl.Vehicle == "" {
				fl.Vehicle = fmt.Sprintf("type %d", hb.Type)
			}
			fl.Armed = ptr(hb.BaseMode&128 != 0)
			fl.Mode = FlightMode(hb)
		}
		if v, ok := d["GPS_RAW_INT"].(GPSRawInt); ok {
			fl.GPS = &v
		}
		if v, ok := d["GLOBAL_POSITION_INT"].(GlobalPositionInt); ok {
			fl.Pos = &v
		}
		if v, ok := d["ATTITUDE"].(Attitude); ok {
			fl.Att = &v
		}
		if v, ok := d["VFR_HUD"].(VFRHUD); ok {
			fl.HUD = &v
		}
		if v, ok := d["SYS_STATUS"].(SysStatus); ok {
			fl.Battery = &Battery{Voltage: v.Voltage, Current: v.Current, Remaining: v.BatteryRemaining}
		} else if v, ok := d["BATTERY_STATUS"].(BatteryStatus); ok {
			fl.Battery = &Battery{Voltage: v.Voltage, Current: v.Current, Remaining: v.Remaining}
		}
		if v, ok := d["RC_CHANNELS"].(RCChannels); ok {
			fl.RCRSSI = ptr(v.RSSI)
		}
		s.Flight = fl
	}
	for _, x := range s.Systems {
		if x.Sys == 101 {
			s.OpenHD = x.D
		}
	}
	n := len(f.texts)
	s.Texts = append([]Text(nil), f.texts[max(0, n-10):]...)
	return s
}

// TlogRecord is one .tlog record (Mission Planner / QGroundControl): u64 big-endian µs
// since the epoch + the frame.
func TlogRecord(raw []byte, t time.Time) []byte {
	r := binary.BigEndian.AppendUint64(nil, uint64(t.UnixMicro()))
	return append(r, raw...)
}
