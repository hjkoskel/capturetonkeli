package dispfilter

import (
	"encoding/hex"
	"regexp"
	"testing"

	"github.com/hjkoskel/capturetonkeli/ctp/wire"
)

func unhex(s string) []byte { b, _ := hex.DecodeString(s); return b }

type frame struct {
	m    wire.PktMeta
	data []byte
}

// Same frames and expressions as test/js/display-filter.test.mjs.
var (
	beacon = frame{wire.PktMeta{Channel: 6, RSSI: -50, Noise: -95, OrigLen: 60, CapLen: 60, Rate: 12, BBFormat: 1},
		unhex("80000000ffffffffffff020102030405020102030405300100000000000000006400110400046b6f746903010630020100")}
	ack      = frame{wire.PktMeta{Channel: 6, RSSI: -80, Noise: -95, OrigLen: 14, CapLen: 14, Flags: 1, Rate: 2}, unhex("d4000000aabbccddeeff")}
	probeReq = frame{beacon.m, unhex("40000000ffffffffffff021122334455ffffffffffff1000")}
)

func run(t *testing.T, expr string, f frame) bool {
	t.Helper()
	c, err := Compile(expr)
	if err != nil {
		t.Fatalf("%s: %v", expr, err)
	}
	return c(f.m, f.data)
}

func TestExpressions(t *testing.T) {
	for _, c := range []struct {
		expr string
		f    frame
		want bool
	}{
		{"type==beacon", beacon, true},
		{"type==beacon", ack, false},
		{`ssid~"KO"`, beacon, true},
		{"rssi>-60 and ch==6", beacon, true},
		{"fcs_err || type==ack", ack, true},
		{"not mgmt", ack, true},
		{"src==02-01-02-03-04-05", beacon, true},
		{`(len>200 or ssid=="koti") && !fcs_err`, beacon, true},
		{"type==probe-req", probeReq, true},
		{"bb==11a/g", beacon, true},
		{"bb==ht", beacon, false},
		{"class==ctrl", ack, true},
		{"addr==ff:ff:ff:ff:ff:ff", beacon, true},
		{"bssid==02:01:02:03:04:05 and seq==19", beacon, true},
		{"seq==-1", ack, true},
		{"beacon and !retry and !protected", beacon, true},
		{"", ack, true},
	} {
		if got := run(t, c.expr, c.f); got != c.want {
			t.Errorf("%q = %v, want %v", c.expr, got, c.want)
		}
	}
}

func TestErrorsAreExplained(t *testing.T) {
	for expr, re := range map[string]string{
		"rssi>":         `value expected`,
		"bogus==1":      `unknown field "bogus"`,
		"rssi>abc":      `not a number`,
		"(type==beacon": `"\)" expected`,
		"ssid<3":        `only works on numbers`,
		"rssi":          `needs a comparison`,
		"ch==6 )":       `unexpected "\)" at position 7`,
		"ch=6":          `unexpected character at 3`,
	} {
		_, err := Compile(expr)
		if err == nil || !regexp.MustCompile(re).MatchString(err.Error()) {
			t.Errorf("%q: %v, want /%s/", expr, err, re)
		}
	}
}

func FuzzCompile(f *testing.F) {
	for _, s := range []string{"type==beacon", `ssid~"a\"b"`, "!(a||b)&&c", "rssi>=-1e3"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		if c, err := Compile(s); err == nil {
			c(beacon.m, beacon.data)
			c(ack.m, ack.data[:3])
		}
	})
}
