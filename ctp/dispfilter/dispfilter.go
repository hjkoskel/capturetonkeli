// Package dispfilter is the display filter language of the web UI (docs/04 §8), evaluated
// on the host (Go port of js/display-filter.js, same syntax and semantics):
//
//	type==beacon   ssid~"home"   rssi>-60   src==aa:bb:cc:dd:ee:ff   ch==6
//	len>200   fcs_err   combined with and/or/not/&&/||/! and parentheses
//
// Unlike the dongle filter (package filter) it never reaches the device: it selects
// frames that were already captured (CLI output, pcapng export).
package dispfilter

import (
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/hjkoskel/capturetonkeli/ctp/dot11"
	"github.com/hjkoskel/capturetonkeli/ctp/wire"
)

// Filter reports whether a frame matches.
type Filter func(m wire.PktMeta, data []byte) bool

// pkt is one frame being evaluated; the 802.11 header is parsed at most once.
type pkt struct {
	m    wire.PktMeta
	data []byte
	d    *dot11.Header
}

func (p *pkt) dot() *dot11.Header {
	if p.d == nil {
		h := dot11.Parse(p.data)
		p.d = &h
	}
	return p.d
}

// val is a field value: a number or a string.
type val struct {
	num   float64
	str   string
	isNum bool
}

func norm(s string) string {
	s = strings.ToLower(s)
	return strings.Map(func(r rune) rune {
		switch r {
		case ' ', '\t', '\n', '\r', '\f', '\v', '_', '-', '+':
			return -1
		}
		return r
	}, s)
}

var alias = map[string]string{"probereq": "proberequest", "proberesp": "proberesponse", "qos": "qosdata", "ba": "blockack", "bar": "blockackrequest",
	"assocreq": "assocrequest", "assocresp": "assocresponse", "deauthentication": "deauth", "disassociation": "disassoc"}

var bbNames = map[uint8]string{0: "11b", 1: "11a/g", 2: "HT", 3: "VHT", 4: "HE-SU", 5: "HE-MU", 6: "HE-ER-SU", 7: "HE-TB", 11: "VHT-MU"}

var typeClass = []string{"mgmt", "ctrl", "data", "ext"}

func num(v float64) val { return val{num: v, isNum: true} }
func str(s string) val  { return val{str: s} }
func addr(a []byte) string {
	if len(a) != 6 {
		return ""
	}
	return fmt.Sprintf("%02x:%02x:%02x:%02x:%02x:%02x", a[0], a[1], a[2], a[3], a[4], a[5])
}

var fields = map[string]func(*pkt) val{
	"ch":      func(p *pkt) val { return num(float64(p.m.Channel)) },
	"channel": func(p *pkt) val { return num(float64(p.m.Channel)) },
	"rssi":    func(p *pkt) val { return num(float64(p.m.RSSI)) },
	"noise":   func(p *pkt) val { return num(float64(p.m.Noise)) },
	"len":     func(p *pkt) val { return num(float64(p.m.OrigLen)) },
	"caplen":  func(p *pkt) val { return num(float64(p.m.CapLen)) },
	"rate":    func(p *pkt) val { return num(float64(p.m.Rate)) },
	"bb": func(p *pkt) val {
		if n, ok := bbNames[p.m.BBFormat]; ok {
			return str(norm(n))
		}
		return str("?")
	},
	"type":  func(p *pkt) val { return str(norm(p.dot().Name)) },
	"class": func(p *pkt) val { return str(typeClass[p.dot().Type&3]) },
	"ssid": func(p *pkt) val {
		s, _ := p.dot().SSID()
		return str(s)
	},
	"src":   func(p *pkt) val { return str(addr(p.dot().Addr2)) },
	"dst":   func(p *pkt) val { return str(addr(p.dot().Addr1)) },
	"bssid": func(p *pkt) val { return str(addr(p.dot().BSSID())) },
	"addr": func(p *pkt) val {
		var a []string
		for _, x := range [][]byte{p.dot().Addr1, p.dot().Addr2, p.dot().Addr3} {
			if s := addr(x); s != "" {
				a = append(a, s)
			}
		}
		return str(strings.Join(a, " "))
	},
	"seq": func(p *pkt) val {
		if d := p.dot(); d.HasSeq {
			return num(float64(d.Seq))
		}
		return num(-1)
	},
}

var flags = map[string]func(*pkt) bool{
	"fcs_err":   func(p *pkt) bool { return p.m.Flags&wire.PktFCSErr != 0 },
	"truncated": func(p *pkt) bool { return p.m.Flags&wire.PktTruncated != 0 },
	"ht40":      func(p *pkt) bool { return p.m.Flags&wire.PktHT40 != 0 },
	"retry":     func(p *pkt) bool { return p.dot().Retry },
	"protected": func(p *pkt) bool { return p.dot().Protected },
	"mgmt":      func(p *pkt) bool { return p.dot().Type == 0 },
	"ctrl":      func(p *pkt) bool { return p.dot().Type == 1 },
	"data":      func(p *pkt) bool { return p.dot().Type == 2 },
	"beacon":    func(p *pkt) bool { return norm(p.dot().Name) == "beacon" },
}

var numeric = map[string]bool{"ch": true, "channel": true, "rssi": true, "noise": true, "len": true, "caplen": true, "rate": true, "seq": true}

// order of field names in error messages (same as the web UI)
var fieldOrder = []string{"ch", "channel", "rssi", "noise", "len", "caplen", "rate", "bb", "type", "class", "ssid", "src", "dst", "bssid", "addr", "seq",
	"fcs_err", "truncated", "ht40", "retry", "protected", "mgmt", "ctrl", "data", "beacon"}

type tok struct {
	k  string // "(", ")", operator, "str", "word"
	v  string
	at int
}

func tokenize(s string) ([]tok, error) {
	var out []tok
	i := 0
	isSpace := func(c byte) bool { return c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f' || c == '\v' }
	for {
		for i < len(s) && isSpace(s[i]) {
			i++
		}
		if i >= len(s) {
			return out, nil
		}
		at := i
		c := s[i]
		switch {
		case c == '(' || c == ')':
			out = append(out, tok{k: string(c), at: at})
			i++
		case strings.HasPrefix(s[i:], "==") || strings.HasPrefix(s[i:], "!=") || strings.HasPrefix(s[i:], "<=") || strings.HasPrefix(s[i:], ">=") ||
			strings.HasPrefix(s[i:], "&&") || strings.HasPrefix(s[i:], "||"):
			out = append(out, tok{k: s[i : i+2], at: at})
			i += 2
		case c == '<' || c == '>' || c == '~' || c == '!':
			out = append(out, tok{k: string(c), at: at})
			i++
		case c == '"':
			var b strings.Builder
			j := i + 1
			for ; j < len(s) && s[j] != '"'; j++ {
				if s[j] == '\\' && j+1 < len(s) {
					j++
				}
				b.WriteByte(s[j])
			}
			if j >= len(s) {
				return nil, fmt.Errorf("unexpected character at %d: %q", at+1, clip(s[at:]))
			}
			out = append(out, tok{k: "str", v: b.String(), at: at})
			i = j + 1
		case strings.IndexByte("=&|", c) >= 0:
			return nil, fmt.Errorf("unexpected character at %d: %q", at+1, clip(s[at:]))
		default:
			j := i
			for j < len(s) && !isSpace(s[j]) && strings.IndexByte("()<>=!~\"&|", s[j]) < 0 {
				j++
			}
			w := s[i:j]
			switch strings.ToLower(w) {
			case "and":
				out = append(out, tok{k: "&&", at: at})
			case "or":
				out = append(out, tok{k: "||", at: at})
			case "not":
				out = append(out, tok{k: "!", at: at})
			default:
				out = append(out, tok{k: "word", v: w, at: at})
			}
			i = j
		}
	}
}

func clip(s string) string {
	if len(s) > 8 {
		return s[:8]
	}
	return s
}

type parseErr struct{ msg string }

type parser struct {
	toks []tok
	i    int
}

func (p *parser) peek() *tok {
	if p.i < len(p.toks) {
		return &p.toks[p.i]
	}
	return nil
}

func (p *parser) take() *tok {
	t := p.peek()
	if t != nil {
		p.i++
	}
	return t
}

func (p *parser) fail(msg string, t *tok) {
	if t != nil {
		panic(parseErr{fmt.Sprintf("%s at position %d", msg, t.at+1)})
	}
	panic(parseErr{msg + " at the end"})
}

type pred func(*pkt) bool

func (p *parser) orE() pred {
	l := p.andE()
	for t := p.peek(); t != nil && t.k == "||"; t = p.peek() {
		p.take()
		a, r := l, p.andE()
		l = func(x *pkt) bool { return a(x) || r(x) }
	}
	return l
}

func (p *parser) andE() pred {
	l := p.notE()
	for t := p.peek(); t != nil && t.k == "&&"; t = p.peek() {
		p.take()
		a, r := l, p.notE()
		l = func(x *pkt) bool { return a(x) && r(x) }
	}
	return l
}

func (p *parser) notE() pred {
	if t := p.peek(); t != nil && t.k == "!" {
		p.take()
		e := p.notE()
		return func(x *pkt) bool { return !e(x) }
	}
	return p.prim()
}

var cmpOps = map[string]bool{"==": true, "!=": true, "<": true, ">": true, "<=": true, ">=": true, "~": true}

func (p *parser) prim() pred {
	t := p.take()
	if t == nil {
		p.fail("expression expected", nil)
	}
	if t.k == "(" {
		e := p.orE()
		if c := p.take(); c == nil || c.k != ")" {
			p.fail(`")" expected`, p.peek())
		}
		return e
	}
	if t.k != "word" {
		p.fail(fmt.Sprintf("unexpected %q", t.k), t)
	}
	name := strings.ToLower(t.v)
	op := p.peek()
	if op == nil || !cmpOps[op.k] {
		if f := flags[name]; f != nil {
			return f
		}
		if fields[name] != nil {
			p.fail(fmt.Sprintf("%q needs a comparison, e.g. %s==…", name, name), op)
		}
		p.fail(fmt.Sprintf("unknown field %q (known: %s)", t.v, strings.Join(fieldOrder, ", ")), t)
	}
	p.take()
	get := fields[name]
	if get == nil {
		p.fail(fmt.Sprintf("unknown field %q", t.v), t)
	}
	vt := p.take()
	if vt == nil || (vt.k != "word" && vt.k != "str") {
		p.fail("value expected", vt)
	}
	var want val
	switch {
	case numeric[name]:
		f, err := strconv.ParseFloat(vt.v, 64)
		if err != nil || math.IsInf(f, 0) || math.IsNaN(f) {
			p.fail(fmt.Sprintf("%q is not a number", vt.v), vt)
		}
		want = num(f)
	case name == "type":
		n := norm(vt.v)
		if a, ok := alias[n]; ok {
			n = a
		}
		want = str(n)
	case name == "src" || name == "dst" || name == "bssid" || name == "addr":
		want = str(strings.ReplaceAll(strings.ToLower(vt.v), "-", ":"))
	case name == "ssid":
		want = str(vt.v)
	default:
		want = str(norm(vt.v))
	}
	if !numeric[name] && op.k != "==" && op.k != "!=" && op.k != "~" {
		p.fail(fmt.Sprintf("%q only works on numbers", op.k), op)
	}
	if name == "addr" && op.k == "==" {
		return func(x *pkt) bool {
			for _, a := range strings.Fields(get(x).str) {
				if a == want.str {
					return true
				}
			}
			return false
		}
	}
	return compare(op.k, get, want)
}

func (v val) String() string {
	if v.isNum {
		return strconv.FormatFloat(v.num, 'f', -1, 64)
	}
	return v.str
}

func compare(op string, get func(*pkt) val, want val) pred {
	if op == "~" {
		w := strings.ToLower(want.String())
		return func(x *pkt) bool { return strings.Contains(strings.ToLower(get(x).String()), w) }
	}
	if want.isNum {
		return func(x *pkt) bool {
			a := get(x).num
			switch op {
			case "==":
				return a == want.num
			case "!=":
				return a != want.num
			case "<":
				return a < want.num
			case ">":
				return a > want.num
			case "<=":
				return a <= want.num
			}
			return a >= want.num
		}
	}
	if op == "==" {
		return func(x *pkt) bool { return get(x).str == want.str }
	}
	return func(x *pkt) bool { return get(x).str != want.str }
}

// Compile parses a display filter expression. An empty expression matches everything.
// Errors name the position, like the web UI's.
func Compile(expr string) (f Filter, err error) {
	if strings.TrimSpace(expr) == "" {
		return func(wire.PktMeta, []byte) bool { return true }, nil
	}
	toks, err := tokenize(expr)
	if err != nil {
		return nil, err
	}
	defer func() {
		if r := recover(); r != nil {
			pe, ok := r.(parseErr)
			if !ok {
				panic(r)
			}
			f, err = nil, fmt.Errorf("%s", pe.msg)
		}
	}()
	p := &parser{toks: toks}
	e := p.orE()
	if p.i < len(p.toks) {
		t := p.toks[p.i]
		v := t.v
		if v == "" {
			v = t.k
		}
		p.fail(fmt.Sprintf("unexpected %q", v), &t)
	}
	return func(m wire.PktMeta, data []byte) bool { return e(&pkt{m: m, data: data}) }, nil
}
