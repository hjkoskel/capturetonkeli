package mavlink

import (
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"os"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"
)

func TestTableMatchesJS(t *testing.T) {
	b, err := os.ReadFile("../../js/mavlink-crc.js")
	if err != nil {
		t.Fatal(err)
	}
	js := regexp.MustCompile("(?s)CRC_TABLE = `(.*)`").FindSubmatch(b)
	if js == nil || strings.Join(strings.Fields(string(js[1])), " ") != strings.Join(strings.Fields(crcTable), " ") {
		t.Fatal("ctp/mavlink/crctable.go differs from js/mavlink-crc.js: regenerate both with tools/mavlink_crc_table.py")
	}
}

func TestFramingCRCv1Tlog(t *testing.T) {
	hb := make([]byte, 9)
	binary.LittleEndian.PutUint32(hb, 3)
	hb[4], hb[5], hb[6] = 1, 3, 129
	f, err := Encode(0, 1, 1, 7, hb)
	if err != nil {
		t.Fatal(err)
	}
	m := Parse(f)
	if len(m) != 1 || m[0].CRC != CRCOK {
		t.Fatalf("%+v", m)
	}
	if mode := FlightMode(Decode(m[0]).(Heartbeat)); mode != "Training" { // plane, custom mode 3
		t.Fatal(mode)
	}
	bad := append([]byte(nil), f...)
	bad[12] ^= 1
	if Parse(bad)[0].CRC != CRCBad {
		t.Fatal("bad CRC accepted")
	}
	if Parse([]byte{0xfd, 1, 0, 0, 0, 1, 1, 0x39, 0x30, 0, 5, 0, 0})[0].CRC != CRCUnknown {
		t.Fatal("unknown id 12345")
	}
	v1 := append([]byte{0xfe, 9, 0, 1, 1, 0}, hb...)
	v1 = append(v1, 0, 0)
	crc := CRCX25([]byte{50}, CRCX25(v1[1:15], 0xffff))
	binary.LittleEndian.PutUint16(v1[15:], crc)
	if Parse(v1)[0].CRC != CRCOK {
		t.Fatal("v1")
	}
	junk := append(append(append([]byte{1, 2}, f...), 3), f...)
	if len(Parse(junk)) != 2 {
		t.Fatal("garbage between frames")
	}
	rec := TlogRecord(f, time.UnixMilli(1700000000000))
	if binary.BigEndian.Uint64(rec) != 1700000000000000 {
		t.Fatal("tlog time")
	}
}

// The telemetry of the OpenHD fixture: ArduPilot quad in Loiter, armed; OpenHD air unit 2.6.4.
func TestFlightStateFromOpenHDTelemetry(t *testing.T) {
	b, err := os.ReadFile("../../test/js/fixtures/ohd-gen-a.json")
	if err != nil {
		t.Fatal(err)
	}
	var v struct {
		Expect struct{ Telemetry []string }
	}
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
	raw, _ := hex.DecodeString(v.Expect.Telemetry[0])
	fs := NewFlightState()
	now := time.Unix(0, 0)
	fs.Add(Parse(raw), now)
	s := fs.Summary(now)
	fl := s.Flight
	if s.BadCRC != 0 || fl == nil || fl.Autopilot != "ArduPilot" || fl.Vehicle != "quadcopter" || fl.Armed == nil || !*fl.Armed || fl.Mode != "Loiter" || fl.GPS.Satellites != 14 {
		t.Fatalf("%+v", fl)
	}
	if !reflect.DeepEqual(*fl.HUD, VFRHUD{Airspeed: 12.5, Groundspeed: 11, Alt: 42, Climb: -0.5, Heading: 270, Throttle: 55}) || fl.Battery.Voltage != 15.8 {
		t.Fatalf("hud %+v battery %+v", fl.HUD, fl.Battery)
	}
	if s.OpenHD["OPENHD_VERSION_MESSAGE"] != (OpenHDVersion{2, 6, 4, 0}) || s.OpenHD["OPENHD_CAMERA_STATUS_AIR"].(OpenHDCameraStatusAir).Width != 1280 ||
		s.OpenHD["OPENHD_CORE_STATUS"].(OpenHDCoreStatus).CPUTemp != 63 {
		t.Fatalf("%+v", s.OpenHD)
	}
	if len(s.Texts) == 0 || s.Texts[0].Text != "Hello from the air" {
		t.Fatalf("%+v", s.Texts)
	}
}

func FuzzParse(f *testing.F) {
	hb, _ := Encode(0, 1, 1, 0, make([]byte, 9))
	f.Add(hb)
	f.Fuzz(func(t *testing.T, b []byte) {
		for _, m := range Parse(b) {
			Decode(m)
		}
	})
}
