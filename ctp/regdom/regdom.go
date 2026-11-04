// Package regdom models the ESP-IDF country → channel whitelist for the
// simulator and the firmware's host fake radio (docs/02 §5).
//
// This is a MODEL. The real accepted channel map is measured on hardware in
// M4 (`ctpbench bench channel-map`, docs/07 §5) and this table is then
// corrected to match.
package regdom

import "sort"

// Region groups countries that share a channel plan in the model.
type Region struct {
	Name string
	G24  [2]uint8 // schan, nchan for 2.4 GHz
	G5   []uint8  // allowed 5 GHz channels
}

var (
	ch5World = []uint8{36, 40, 44, 48, 52, 56, 60, 64, 100, 104, 108, 112, 116, 120, 124, 128, 132, 136, 140, 144, 149, 153, 157, 161, 165}
	ch5EU    = []uint8{36, 40, 44, 48, 52, 56, 60, 64, 100, 104, 108, 112, 116, 120, 124, 128, 132, 136, 140}
	ch5US    = []uint8{36, 40, 44, 48, 52, 56, 60, 64, 100, 104, 108, 112, 116, 120, 124, 128, 132, 136, 140, 144, 149, 153, 157, 161, 165, 169, 173, 177}
	ch5JP    = []uint8{36, 40, 44, 48, 52, 56, 60, 64, 100, 104, 108, 112, 116, 120, 124, 128, 132, 136, 140, 144}
	ch5CN    = []uint8{36, 40, 44, 48, 52, 56, 60, 64, 149, 153, 157, 161, 165}

	world = Region{"01", [2]uint8{1, 11}, ch5World}
	eu    = Region{"EU", [2]uint8{1, 13}, ch5EU}
	us    = Region{"US", [2]uint8{1, 11}, ch5US}
	jp    = Region{"JP", [2]uint8{1, 14}, ch5JP}
	cn    = Region{"CN", [2]uint8{1, 13}, ch5CN}
	wide  = Region{"AU", [2]uint8{1, 13}, ch5World}
)

// Countries accepted by esp_wifi_set_country_code (docs/02 §5).
var Countries = map[string]Region{
	"01": world,
	"AT": eu, "BE": eu, "BG": eu, "CH": eu, "CY": eu, "CZ": eu, "DE": eu, "DK": eu, "EE": eu,
	"ES": eu, "FI": eu, "FR": eu, "GB": eu, "GR": eu, "HR": eu, "HU": eu, "IE": eu, "IS": eu,
	"IT": eu, "LI": eu, "LT": eu, "LU": eu, "LV": eu, "MT": eu, "NL": eu, "NO": eu, "PL": eu,
	"PT": eu, "RO": eu, "SE": eu, "SI": eu, "SK": eu,
	"US": us, "CA": us, "MX": us, "TW": us, "BR": us,
	"JP": jp,
	"CN": cn,
	"AU": wide, "NZ": wide, "HK": wide, "IN": wide, "KR": wide,
}

// Codes returns all supported codes, sorted.
func Codes() []string {
	out := make([]string, 0, len(Countries))
	for k := range Countries {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Allowed reports whether ch is accepted under a validated country code.
func (r Region) Allowed(ch uint8) bool {
	if ch >= r.G24[0] && int(ch) < int(r.G24[0])+int(r.G24[1]) && ch <= 14 {
		return true
	}
	for _, c := range r.G5 {
		if c == ch {
			return true
		}
	}
	return false
}

// Manual builds the region for a manual range (legacy esp_wifi_set_country):
// 2.4 GHz = schan..schan+nchan-1, 5 GHz = the named country's set (or "01").
func Manual(cc string, schan, nchan uint8) (Region, bool) {
	if schan < 1 || nchan < 1 || int(schan)+int(nchan)-1 > 14 {
		return Region{}, false
	}
	base, ok := Countries[cc]
	if !ok {
		base = world
	}
	return Region{Name: cc, G24: [2]uint8{schan, nchan}, G5: base.G5}, true
}

// HT40Partner returns the secondary channel for an HT40 request and the
// direction actually applied. On 5 GHz the direction comes from the 40 MHz
// pair (36/40, 44/48, …), not from the request (docs/03 §6.1).
// plus=true means secondary above. ok=false if no partner exists.
func HT40Partner(ch uint8, wantPlus bool) (partner uint8, plus bool, ok bool) {
	if ch >= 1 && ch <= 14 {
		if wantPlus {
			if ch+4 > 13 {
				return 0, true, false
			}
			return ch + 4, true, true
		}
		if ch < 5 {
			return 0, false, false
		}
		return ch - 4, false, true
	}
	if ch < 36 || ch%4 != 0 && ch < 149 {
		return 0, false, false
	}
	var idx int
	if ch >= 149 {
		if (ch-149)%4 != 0 {
			return 0, false, false
		}
		idx = int(ch-149) / 4
	} else {
		idx = int(ch-36) / 4
	}
	if idx%2 == 0 {
		return ch + 4, true, true
	}
	return ch - 4, false, true
}
