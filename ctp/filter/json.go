package filter

import (
	"encoding/json"
	"fmt"
	"net"
	"strings"
)

// JSON format = the web UI import/export format (docs/04 §6):
//
//	{"version":1,
//	 "rules":[{"name":"beacons","enabled":true,
//	           "terms":[{"offset":0,"value":"80","mask":"fc","negate":false}]}],
//	 "global":{"drop_fcs_err":true,"rssi_min":-70,"len_min":null,"len_max":null}}
//
// "value" uses the ParseTerm syntax ('?' wildcards allowed); "mask" is
// optional (default ff… with wildcard nibbles cleared).

type jsonTerm struct {
	Offset uint16 `json:"offset"`
	Value  string `json:"value"`
	Mask   string `json:"mask,omitempty"`
	Negate bool   `json:"negate"`
}

type jsonRule struct {
	Name    string     `json:"name"`
	Enabled *bool      `json:"enabled,omitempty"`
	Terms   []jsonTerm `json:"terms"`
}

type jsonGlobal struct {
	DropFCSErr bool    `json:"drop_fcs_err"`
	RSSIMin    *int8   `json:"rssi_min"`
	LenMin     *uint16 `json:"len_min"`
	LenMax     *uint16 `json:"len_max"`
}

type jsonSet struct {
	Version int        `json:"version"`
	Rules   []jsonRule `json:"rules"`
	Global  jsonGlobal `json:"global"`
}

// MarshalJSON implements json.Marshaler.
func (s Set) MarshalJSON() ([]byte, error) {
	j := jsonSet{Version: 1, Rules: []jsonRule{}, Global: jsonGlobal(s.Global)}
	for _, r := range s.Rules {
		en := r.Enabled
		jr := jsonRule{Name: r.Name, Enabled: &en, Terms: []jsonTerm{}}
		for _, t := range r.Terms {
			jr.Terms = append(jr.Terms, jsonTerm{Offset: t.Offset, Value: FormatHex(t.Value), Mask: FormatHex(t.Mask), Negate: t.Negate})
		}
		j.Rules = append(j.Rules, jr)
	}
	return json.Marshal(j)
}

// UnmarshalJSON implements json.Unmarshaler.
func (s *Set) UnmarshalJSON(b []byte) error {
	var j jsonSet
	if err := json.Unmarshal(b, &j); err != nil {
		return err
	}
	if j.Version != 0 && j.Version != 1 {
		return fmt.Errorf("filter: unsupported version %d", j.Version)
	}
	out := Set{Version: 1, Global: Global(j.Global)}
	for i, jr := range j.Rules {
		r := Rule{Name: jr.Name, Enabled: jr.Enabled == nil || *jr.Enabled}
		for k, jt := range jr.Terms {
			p := jt.Value
			if jt.Mask != "" {
				p += "/" + jt.Mask
			}
			t, err := ParseTerm(jt.Offset, p)
			if err != nil {
				return fmt.Errorf("filter: rule %d term %d: %w", i, k, err)
			}
			t.Negate = jt.Negate
			r.Terms = append(r.Terms, t)
		}
		out.Rules = append(out.Rules, r)
	}
	*s = out
	return nil
}

// ---- presets (same definitions as the web UI, docs/04 §6) ---------------

func mustTerm(off uint16, p string) Term {
	t, err := ParseTerm(off, p)
	if err != nil {
		panic(err)
	}
	return t
}

func rule(name string, terms ...Term) Rule { return Rule{Name: name, Enabled: true, Terms: terms} }

func Beacons() Rule   { return rule("Beacons", mustTerm(0, "80/fc")) }
func ProbeReq() Rule  { return rule("Probe requests", mustTerm(0, "40/fc")) }
func ProbeResp() Rule { return rule("Probe responses", mustTerm(0, "50/fc")) }
func Mgmt() Rule      { return rule("Management", mustTerm(0, "00/0c")) }
func Ctrl() Rule      { return rule("Control", mustTerm(0, "04/0c")) }
func Data() Rule      { return rule("Data", mustTerm(0, "08/0c")) }
func DropBeacons() Rule {
	t := mustTerm(0, "80/fc")
	t.Negate = true
	return rule("Drop beacons", t)
}

func macTerm(off uint16, mac net.HardwareAddr) Term {
	return Term{Offset: off, Value: append([]byte(nil), mac...), Mask: []byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff}}
}

func FromMAC(mac net.HardwareAddr) Rule { return rule("From "+mac.String(), macTerm(10, mac)) }
func ToMAC(mac net.HardwareAddr) Rule   { return rule("To "+mac.String(), macTerm(4, mac)) }
func BSSID(mac net.HardwareAddr) Rule   { return rule("BSSID "+mac.String(), macTerm(16, mac)) }

// DeauthDisassoc returns two rules (deauth, disassoc); one byte mask cannot express both.
func DeauthDisassoc() []Rule {
	return []Rule{rule("Deauth", mustTerm(0, "c0/fc")), rule("Disassoc", mustTerm(0, "a0/fc"))}
}

// EAPOL returns two rules: non-QoS data (LLC at 24) and QoS data (LLC at 26).
func EAPOL() []Rule {
	const llc = "aa aa 03 00 00 00 88 8e"
	return []Rule{
		rule("EAPOL", mustTerm(0, "08/fc"), mustTerm(24, llc)),
		rule("EAPOL (QoS)", mustTerm(0, "88/fc"), mustTerm(26, llc)),
	}
}

// Preset resolves "name[:arg]" as used by `ctpbench filter set --preset`.
func Preset(spec string) ([]Rule, error) {
	name, arg, _ := strings.Cut(spec, ":")
	mac := func() (net.HardwareAddr, error) {
		m, err := net.ParseMAC(arg)
		if err != nil || len(m) != 6 {
			return nil, fmt.Errorf("preset %s needs a MAC argument (%s:aa:bb:cc:dd:ee:ff)", name, name)
		}
		return m, nil
	}
	switch strings.ToLower(name) {
	case "beacons":
		return []Rule{Beacons()}, nil
	case "probereq", "probe-req":
		return []Rule{ProbeReq()}, nil
	case "proberesp", "probe-resp":
		return []Rule{ProbeResp()}, nil
	case "mgmt":
		return []Rule{Mgmt()}, nil
	case "ctrl":
		return []Rule{Ctrl()}, nil
	case "data":
		return []Rule{Data()}, nil
	case "dropbeacons", "drop-beacons":
		return []Rule{DropBeacons()}, nil
	case "deauth", "deauthdisassoc":
		return DeauthDisassoc(), nil
	case "eapol":
		return EAPOL(), nil
	case "from", "frommac":
		m, err := mac()
		if err != nil {
			return nil, err
		}
		return []Rule{FromMAC(m)}, nil
	case "to", "tomac":
		m, err := mac()
		if err != nil {
			return nil, err
		}
		return []Rule{ToMAC(m)}, nil
	case "bssid":
		m, err := mac()
		if err != nil {
			return nil, err
		}
		return []Rule{BSSID(m)}, nil
	}
	return nil, fmt.Errorf("unknown preset %q", name)
}
