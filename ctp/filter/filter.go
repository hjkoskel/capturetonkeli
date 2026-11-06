// Package filter is the reference implementation of CTP user filters
// (docs/02 §2, docs/04 §6 and Appendix A). Match is the oracle that the
// firmware C matcher and the JS matcher must agree with.
package filter

import (
	"encoding/binary"
	"errors"
	"fmt"
	"strings"

	"github.com/hjkoskel/capturetonkeli/ctp/codec"
	"github.com/hjkoskel/capturetonkeli/ctp/wire"
)

// Limits (docs/04 §6).
const (
	MaxRules   = 16
	MaxTerms   = 4
	MaxTermLen = 16
	MaxOffset  = 4095
)

// gflags / tflags bits (Appendix A).
const (
	GDropFCSErr uint8 = 1 << 0
	GRSSIMinEn  uint8 = 1 << 1
	GLenMinEn   uint8 = 1 << 2
	GLenMaxEn   uint8 = 1 << 3
	gKnown            = GDropFCSErr | GRSSIMinEn | GLenMinEn | GLenMaxEn

	TNegate uint8 = 1 << 0
	tKnown        = TNegate
)

// HeaderLen is the fixed SET_FILTER prefix.
const HeaderLen = 8

// ErrInvalid wraps every validation/decoding failure.
var ErrInvalid = errors.New("filter: invalid")

func invalid(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, a...))
}

// Term is one byte-pattern comparison.
type Term struct {
	Offset uint16
	Value  []byte
	Mask   []byte
	Negate bool
}

// Rule is an AND of terms.
type Rule struct {
	Name    string
	Enabled bool
	Terms   []Term
}

// Global holds the meta conditions applied to every frame.
type Global struct {
	DropFCSErr bool
	RSSIMin    *int8
	LenMin     *uint16
	LenMax     *uint16
}

// Set is an OR of rules plus global conditions.
type Set struct {
	Version int
	Rules   []Rule
	Global  Global
}

// Validate checks all limits. maxRules ≤ 0 means MaxRules.
func (s Set) Validate(maxRules int) error {
	if maxRules <= 0 || maxRules > MaxRules {
		maxRules = MaxRules
	}
	n := 0
	for i, r := range s.Rules {
		if !r.Enabled {
			continue
		}
		n++
		if len(r.Terms) < 1 || len(r.Terms) > MaxTerms {
			return invalid("rule %d: %d terms (want 1..%d)", i, len(r.Terms), MaxTerms)
		}
		for j, t := range r.Terms {
			if err := t.validate(); err != nil {
				return invalid("rule %d term %d: %v", i, j, err)
			}
		}
	}
	if n > maxRules {
		return invalid("%d enabled rules (max %d)", n, maxRules)
	}
	return nil
}

func (t Term) validate() error {
	if len(t.Value) < 1 || len(t.Value) > MaxTermLen {
		return fmt.Errorf("length %d (want 1..%d)", len(t.Value), MaxTermLen)
	}
	if len(t.Mask) != len(t.Value) {
		return fmt.Errorf("mask length %d != value length %d", len(t.Mask), len(t.Value))
	}
	if t.Offset > MaxOffset {
		return fmt.Errorf("offset %d > %d", t.Offset, MaxOffset)
	}
	return nil
}

// Encode returns the SET_FILTER body (enabled rules only).
func (s Set) Encode() ([]byte, error) {
	if err := s.Validate(MaxRules); err != nil {
		return nil, err
	}
	var g uint8
	var rssi int8
	var lmin, lmax uint16
	if s.Global.DropFCSErr {
		g |= GDropFCSErr
	}
	if s.Global.RSSIMin != nil {
		g |= GRSSIMinEn
		rssi = *s.Global.RSSIMin
	}
	if s.Global.LenMin != nil {
		g |= GLenMinEn
		lmin = *s.Global.LenMin
	}
	if s.Global.LenMax != nil {
		g |= GLenMaxEn
		lmax = *s.Global.LenMax
	}
	n := 0
	for _, r := range s.Rules {
		if r.Enabled {
			n++
		}
	}
	b := []byte{uint8(n), g, uint8(rssi), 0}
	b = binary.LittleEndian.AppendUint16(b, lmin)
	b = binary.LittleEndian.AppendUint16(b, lmax)
	for _, r := range s.Rules {
		if !r.Enabled {
			continue
		}
		b = append(b, uint8(len(r.Terms)))
		for _, t := range r.Terms {
			var tf uint8
			if t.Negate {
				tf |= TNegate
			}
			b = binary.LittleEndian.AppendUint16(b, t.Offset)
			b = append(b, uint8(len(t.Value)), tf)
			b = append(b, t.Value...)
			b = append(b, t.Mask...)
		}
	}
	return b, nil
}

// CRC32 is the CRC of Encode(); compare with Config.RulesCRC32.
func (s Set) CRC32() (uint32, error) {
	b, err := s.Encode()
	if err != nil {
		return 0, err
	}
	return codec.CRC32(b), nil
}

// Decode parses a SET_FILTER body with the device's strict rules
// (docs/03 §6.1 ERR_FILTER_INVALID): limits, known flag bits, zero
// reserved byte, no trailing bytes. Offsets above MaxOffset are accepted on
// the wire (u16) and simply never match long frames.
func Decode(body []byte) (Set, error) {
	var s Set
	s.Version = 1
	if len(body) < HeaderLen {
		return s, invalid("body %d bytes < %d", len(body), HeaderLen)
	}
	n, g := body[0], body[1]
	if n > MaxRules {
		return s, invalid("%d rules > %d", n, MaxRules)
	}
	if g&^gKnown != 0 {
		return s, invalid("unknown gflags 0x%02x", g)
	}
	if body[3] != 0 {
		return s, invalid("reserved byte 0x%02x", body[3])
	}
	s.Global.DropFCSErr = g&GDropFCSErr != 0
	if g&GRSSIMinEn != 0 {
		v := int8(body[2])
		s.Global.RSSIMin = &v
	}
	if g&GLenMinEn != 0 {
		v := binary.LittleEndian.Uint16(body[4:])
		s.Global.LenMin = &v
	}
	if g&GLenMaxEn != 0 {
		v := binary.LittleEndian.Uint16(body[6:])
		s.Global.LenMax = &v
	}
	p := body[HeaderLen:]
	for i := 0; i < int(n); i++ {
		if len(p) < 1 {
			return s, invalid("rule %d truncated", i)
		}
		nt := int(p[0])
		p = p[1:]
		if nt < 1 || nt > MaxTerms {
			return s, invalid("rule %d: %d terms", i, nt)
		}
		r := Rule{Enabled: true}
		for j := 0; j < nt; j++ {
			if len(p) < 4 {
				return s, invalid("rule %d term %d truncated", i, j)
			}
			off := binary.LittleEndian.Uint16(p)
			l, tf := int(p[2]), p[3]
			p = p[4:]
			if l < 1 || l > MaxTermLen {
				return s, invalid("rule %d term %d: len %d", i, j, l)
			}
			if tf&^tKnown != 0 {
				return s, invalid("rule %d term %d: unknown tflags 0x%02x", i, j, tf)
			}
			if len(p) < 2*l {
				return s, invalid("rule %d term %d: value/mask truncated", i, j)
			}
			t := Term{Offset: off, Negate: tf&TNegate != 0,
				Value: append([]byte(nil), p[:l]...), Mask: append([]byte(nil), p[l:2*l]...)}
			p = p[2*l:]
			r.Terms = append(r.Terms, t)
		}
		s.Rules = append(s.Rules, r)
	}
	if len(p) != 0 {
		return s, invalid("%d trailing bytes", len(p))
	}
	return s, nil
}

// Match is the reference evaluator (docs/04 Appendix A).
// frame is the full received frame (before snaplen); m supplies Flags
// (fcs_err), RSSI and OrigLen.
//
// Pass = global conditions AND (no enabled rules OR any enabled rule whose
// terms all match). A term matches iff (offset+len ≤ len(frame) AND every
// masked byte equals) XOR negate; so a negated term on an out-of-range
// offset matches.
func (s Set) Match(frame []byte, m wire.PktMeta) bool {
	g := s.Global
	if g.DropFCSErr && m.Flags&wire.PktFCSErr != 0 {
		return false
	}
	if g.RSSIMin != nil && m.RSSI < *g.RSSIMin {
		return false
	}
	if g.LenMin != nil && m.OrigLen < *g.LenMin {
		return false
	}
	if g.LenMax != nil && m.OrigLen > *g.LenMax {
		return false
	}
	any := false
	for _, r := range s.Rules {
		if !r.Enabled {
			continue
		}
		any = true
		if r.match(frame) {
			return true
		}
	}
	return !any
}

func (r Rule) match(frame []byte) bool {
	for _, t := range r.Terms {
		if !t.Match(frame) {
			return false
		}
	}
	return true
}

// Match evaluates one term.
func (t Term) Match(frame []byte) bool {
	end := int(t.Offset) + len(t.Value)
	eq := end <= len(frame)
	if eq {
		for i := range t.Value {
			if frame[int(t.Offset)+i]&t.Mask[i] != t.Value[i]&t.Mask[i] {
				eq = false
				break
			}
		}
	}
	return eq != t.Negate
}

// ParseTerm parses a value pattern: hex bytes optionally separated by
// spaces, ':' or '-', with '?' as a nibble wildcard ("8?", "aa:bb:cc:dd:ee:ff",
// "88-8e ?? 03"). An optional "/mask" suffix gives an explicit mask of equal
// length ("80/fc"); it is ANDed with the wildcard mask.
func ParseTerm(offset uint16, pattern string) (Term, error) {
	vp, mp, hasMask := strings.Cut(pattern, "/")
	v, m, err := parseHex(vp, true)
	if err != nil {
		return Term{}, err
	}
	if hasMask {
		mm, _, err := parseHex(mp, false)
		if err != nil {
			return Term{}, fmt.Errorf("mask: %w", err)
		}
		if len(mm) != len(m) {
			return Term{}, fmt.Errorf("mask length %d != value length %d", len(mm), len(m))
		}
		for i := range m {
			m[i] &= mm[i]
		}
	}
	for i := range v {
		v[i] &= m[i]
	}
	t := Term{Offset: offset, Value: v, Mask: m}
	return t, t.validate()
}

// ParseMask parses a plain mask string (no wildcards).
func ParseMask(s string) ([]byte, error) {
	b, _, err := parseHex(s, false)
	return b, err
}

func parseHex(s string, allowWild bool) (val, mask []byte, err error) {
	var digits []byte
	for _, c := range strings.ToLower(s) {
		switch {
		case c == ' ' || c == ':' || c == '-' || c == '\t' || c == ',':
		case c >= '0' && c <= '9', c >= 'a' && c <= 'f':
			digits = append(digits, byte(c))
		case c == '?' && allowWild:
			digits = append(digits, '?')
		case c == 'x' && len(digits) == 1 && digits[0] == '0':
			digits = digits[:0] // allow 0x prefix
		default:
			return nil, nil, fmt.Errorf("bad character %q in %q", c, s)
		}
	}
	if len(digits) == 0 || len(digits)%2 != 0 {
		return nil, nil, fmt.Errorf("pattern %q: need an even, non-zero number of hex digits", s)
	}
	for i := 0; i < len(digits); i += 2 {
		var v, m byte
		for k := 0; k < 2; k++ {
			c := digits[i+k]
			v <<= 4
			m <<= 4
			if c == '?' {
				continue
			}
			m |= 0xF
			if c <= '9' {
				v |= c - '0'
			} else {
				v |= c - 'a' + 10
			}
		}
		val = append(val, v)
		mask = append(mask, m)
	}
	return val, mask, nil
}

// FormatHex renders bytes as "aa bb cc".
func FormatHex(b []byte) string {
	var sb strings.Builder
	for i, x := range b {
		if i > 0 {
			sb.WriteByte(' ')
		}
		fmt.Fprintf(&sb, "%02x", x)
	}
	return sb.String()
}
