package wire

import (
	"encoding/hex"
	"encoding/json"
	"os"
	"testing"
)

type binMsg interface {
	AppendBinary([]byte) []byte
	UnmarshalBinary([]byte) error
}

func TestMessageVectors(t *testing.T) {
	b, err := os.ReadFile("../../testvectors/messages.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		Name   string `json:"name"`
		Type   uint8  `json:"type"`
		Body   string `json:"body_hex"`
		Longer bool   `json:"longer_body"`
	}
	if err := json.Unmarshal(b, &cases); err != nil {
		t.Fatal(err)
	}
	byName := map[string]func() binMsg{
		"HELLO": func() binMsg { return &Hello{} }, "PING": func() binMsg { return &Ping{} },
		"SET_CHANNEL": func() binMsg { return &SetChannel{} }, "SET_COUNTRY": func() binMsg { return &Country{} },
		"SET_SNAPLEN": func() binMsg { return &SetSnaplen{} }, "SET_DRIVER_MASK": func() binMsg { return &SetDriverMask{} },
		"CAPTURE": func() binMsg { return &Capture{} }, "GET_STATS": func() binMsg { return &GetStats{} },
		"REBOOT": func() binMsg { return &Reboot{} }, "RSP_OK DeviceInfo": func() binMsg { return &DeviceInfo{} },
		"RSP_OK Pong": func() binMsg { return &Pong{} }, "RSP_OK Config": func() binMsg { return &Config{} },
		"RSP_OK boot Config": func() binMsg { return &Config{} }, "EVT_STATS": func() binMsg { return &Stats{} },
		"RSP_ERR": func() binMsg { return &RspErr{} }, "EVT_RESET": func() binMsg { return &Reset{} },
	}
	for _, c := range cases {
		name := c.Name
		if c.Longer {
			name = name[:len(name)-len(" (+trailing)")]
		}
		mk, ok := byName[name]
		if !ok {
			continue // PKT has its own decoder
		}
		body, _ := hex.DecodeString(c.Body)
		m := mk()
		if err := m.UnmarshalBinary(body); err != nil {
			t.Errorf("%s: %v", c.Name, err)
			continue
		}
		if got := m.AppendBinary(nil); !c.Longer && hex.EncodeToString(got) != c.Body {
			t.Errorf("%s: re-encode\n got %x\nwant %s", c.Name, got, c.Body)
		}
		if len(body) > 1 {
			short := mk()
			if err := short.UnmarshalBinary(body[:1]); err == nil && name != "CAPTURE" && name != "GET_STATS" && name != "REBOOT" {
				t.Errorf("%s: 1-byte body accepted", c.Name)
			}
		}
	}
}
