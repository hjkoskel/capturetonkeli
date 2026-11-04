package conformance

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math/rand"
	"sort"
	"time"

	"github.com/hjkoskel/capturetonkeli/ctp/codec"
	"github.com/hjkoskel/capturetonkeli/ctp/filter"
	"github.com/hjkoskel/capturetonkeli/ctp/wire"
)

func le32(v uint32) []byte { return binary.LittleEndian.AppendUint32(nil, v) }

func expectErr(t *T, l *Link, typ uint8, body []byte, want wire.ErrCode, hint string) {
	f, ok := l.Req(t.Ctx(), typ, body)
	if !ok {
		t.Fatalf("RSP_ERR "+want.String(), hint, "no response")
	}
	if f.H.Type != wire.TypeRspErr {
		t.Fatalf("RSP_ERR "+want.String(), hint, "%s", wire.TypeName(f.H.Type))
	}
	var e wire.RspErr
	if err := e.UnmarshalBinary(f.Body); err != nil {
		t.Fatalf("well-formed RSP_ERR body", "wire.c RSP_ERR encoder", "%v (body % x)", err, f.Body)
	}
	if e.Code != want {
		t.Fatalf("err_code "+want.String(), hint, "%s", e.Error())
	}
}

func mustCfg(t *T, l *Link, typ uint8, body []byte, hint string) wire.Config {
	c, code, ok := l.Config(t.Ctx(), typ, body)
	if !ok {
		t.Fatalf(wire.TypeName(typ)+" answered with CONFIG", hint, "no/invalid response")
	}
	if code != 0 {
		t.Fatalf(wire.TypeName(typ)+" succeeds", hint, "%s", code)
	}
	return c
}

func pkts(fs []Frame) []Frame {
	var out []Frame
	for _, f := range fs {
		if f.H.Type == wire.TypePkt {
			out = append(out, f)
		}
	}
	return out
}

// captureFor turns capture on, collects frames for d while keeping the link alive, then turns capture off.
func captureFor(t *T, l *Link, d time.Duration) []Frame {
	mustCfg(t, l, wire.TypeCapture, []byte{1}, "CAPTURE handler")
	stop := make(chan struct{})
	go l.KeepAlive(stop)
	fs := l.Collect(t.Ctx(), d)
	close(stop)
	mustCfg(t, l, wire.TypeCapture, []byte{0}, "CAPTURE handler")
	return fs
}

func needCap(t *T, c wire.Caps, name string) {
	if t.Info().Caps&c == 0 {
		t.Skip("device lacks cap " + name)
	}
}

var cases = []Case{
	// ---- framing -----------------------------------------------------------
	{ID: "CTP-FRM-001", Title: "resync after garbage: HELLO after random bytes is answered", Spec: "doc03 §3",
		Hint: "frame decoder resync (frame.c): discard until first 0x00",
		Run: func(t *T) {
			l := t.Open()
			g := make([]byte, 300)
			rand.New(rand.NewSource(1)).Read(g)
			for i := range g {
				if g[i] == 0 {
					g[i] = 0x5a
				}
			}
			_ = l.WriteRaw(g)
			if _, ok := l.Hello(t.Ctx()); !ok {
				t.Fatalf("HELLO answered after garbage", "", "no response")
			}
		}},
	{ID: "CTP-FRM-002", Title: "frame with bad CRC is never answered and is counted", Spec: "doc03 §3",
		Hint: "frame.c CRC check / stats host_rx_crc_err",
		Run: func(t *T) {
			l := t.Session()
			st0, _ := l.Stats(t.Ctx(), false)
			id := l.ID()
			raw := make([]byte, 0, 32)
			h := wire.Header{Ver: 1, Type: wire.TypePing, ReqID: id}
			f := h.AppendBinary(nil)
			f = append(f, le32(1)...)
			f = append(f, le32(codec.CRC32(f)^0xFFFF)...)
			raw = append(codec.COBSEncode(append(raw, 0), f), 0)
			_ = l.WriteRaw(raw)
			if r, ok := l.Await(t.Ctx(), id, 300*time.Millisecond, nil); ok {
				t.Fatalf("no response", "", "got %s", wire.TypeName(r.H.Type))
			}
			st1, ok := l.Stats(t.Ctx(), false)
			if !ok || st1.HostRxCRCErr <= st0.HostRxCRCErr {
				t.Fatalf("host_rx_crc_err increments", "", "%d -> %d", st0.HostRxCRCErr, st1.HostRxCRCErr)
			}
		}},
	{ID: "CTP-FRM-003", Title: "oversize frame is dropped, counted, and the link recovers", Spec: "doc03 §3",
		Hint: "frame.c oversize handling (MaxFrame 4128)",
		Run: func(t *T) {
			l := t.Session()
			st0, _ := l.Stats(t.Ctx(), false)
			big := bytes.Repeat([]byte{0x33}, 6000)
			_ = l.WriteRaw(append(append([]byte{0}, big...), 0))
			if _, ok := l.Ping(t.Ctx(), 500*time.Millisecond); !ok {
				t.Fatalf("PING answered after oversize frame", "", "no response")
			}
			st1, _ := l.Stats(t.Ctx(), false)
			if st1.HostRxOversize <= st0.HostRxOversize {
				t.Fatalf("host_rx_oversize increments", "", "%d -> %d", st0.HostRxOversize, st1.HostRxOversize)
			}
		}},
	{ID: "CTP-FRM-004", Title: "empty frames are ignored", Spec: "doc03 §3",
		Run: func(t *T) {
			l := t.Session()
			_ = l.WriteRaw([]byte{0, 0, 0, 0})
			st0, _ := l.Stats(t.Ctx(), false)
			_ = l.WriteRaw([]byte{0, 0, 0})
			if _, ok := l.Ping(t.Ctx(), 300*time.Millisecond); !ok {
				t.Fatalf("PING answered", "frame.c empty-frame handling", "no response")
			}
			st1, _ := l.Stats(t.Ctx(), false)
			if st1.HostRxCRCErr != st0.HostRxCRCErr {
				t.Fatalf("empty frames not counted as errors", "frame.c must skip zero-length frames", "crc_err %d -> %d", st0.HostRxCRCErr, st1.HostRxCRCErr)
			}
		}},
	// ---- header ------------------------------------------------------------
	{ID: "CTP-HDR-001", Title: "device frames carry ver 1, seq +1 per frame", Spec: "doc03 §4",
		Hint: "link task header fill: one seq counter for all device→host frames",
		Run: func(t *T) {
			l := t.Session()
			mustCfg(t, l, wire.TypeSetSnaplen, []byte{32, 0}, "")
			fs := captureFor(t, l, 1200*time.Millisecond)
			if len(fs) < 3 {
				t.Fatalf("≥3 frames during 1.2 s capture", "", "%d", len(fs))
			}
			for i, f := range fs {
				if f.H.Ver != 1 {
					t.Fatalf("ver 1", "", "frame %d ver %d", i, f.H.Ver)
				}
				if i > 0 && f.H.Seq != fs[i-1].H.Seq+1 {
					t.Fatalf("seq +1", "", "frame %d seq %d after %d", i, f.H.Seq, fs[i-1].H.Seq)
				}
			}
		}},
	{ID: "CTP-HDR-002", Title: "req_id is echoed", Spec: "doc03 §4",
		Run: func(t *T) {
			l := t.Session()
			for _, id := range []uint16{1, 0x1234, 0xFFFF} {
				_ = l.Send(wire.TypePing, id, le32(5))
				f, ok := l.Await(t.Ctx(), id, 300*time.Millisecond, nil)
				if !ok || f.H.Type != wire.TypeRspOK {
					t.Fatalf(fmt.Sprintf("RSP_OK with req_id %d", id), "proto.c response header", "ok=%v", ok)
				}
			}
		}},
	{ID: "CTP-HDR-003", Title: "cfg_gen increments only on applied change", Spec: "doc03 §6.1, §7",
		Hint: "config.c: compare old/new CONFIG before bumping cfg_gen",
		Run: func(t *T) {
			l := t.Session()
			a := mustCfg(t, l, wire.TypeSetSnaplen, []byte{100, 0}, "")
			b := mustCfg(t, l, wire.TypeSetSnaplen, []byte{100, 0}, "")
			if a.CfgGen != b.CfgGen {
				t.Fatalf("same value: cfg_gen unchanged", "", "%d -> %d", a.CfgGen, b.CfgGen)
			}
			c := mustCfg(t, l, wire.TypeSetSnaplen, []byte{101, 0}, "")
			if c.CfgGen != b.CfgGen+1 {
				t.Fatalf("new value: cfg_gen +1", "", "%d -> %d", b.CfgGen, c.CfgGen)
			}
		}},
	{ID: "CTP-HDR-004", Title: "flags bit1 marks the first frame after a config change", Spec: "doc03 §4",
		Hint: "link task: set FlagCfgChanged when cfg_gen differs from the previous frame",
		Run: func(t *T) {
			l := t.Session()
			mustCfg(t, l, wire.TypeSetSnaplen, []byte{50, 0}, "")
			f, ok := l.Req(t.Ctx(), wire.TypeSetSnaplen, []byte{51, 0})
			if !ok || f.H.Flags&wire.FlagCfgChanged == 0 {
				t.Fatalf("response to changing SET has flags bit1", "", "ok=%v flags=%02x", ok, f.H.Flags)
			}
			f, ok = l.Req(t.Ctx(), wire.TypePing, le32(1))
			if !ok || f.H.Flags&wire.FlagCfgChanged != 0 {
				t.Fatalf("next frame has bit1 clear", "", "ok=%v flags=%02x", ok, f.H.Flags)
			}
		}},
	{ID: "CTP-HDR-005", Title: "frame with ver != 1 is dropped silently", Spec: "doc03 §6.1",
		Hint: "proto.c: drop frames whose header ver != 1",
		Run: func(t *T) {
			l := t.Session()
			id := l.ID()
			_ = l.WriteRaw(l.Encode(wire.Header{Ver: 2, Type: wire.TypePing, ReqID: id}, le32(1)))
			if _, ok := l.Await(t.Ctx(), id, 300*time.Millisecond, nil); ok {
				t.Fatalf("no response", "", "answered")
			}
		}},
	// ---- HELLO -------------------------------------------------------------
	{ID: "CTP-HELLO-001", Title: "HELLO: nonce echo, proto_ver 1, sane limits", Spec: "doc03 §6, §6.1",
		Hint: "proto.c HELLO response (DeviceInfo)",
		Run: func(t *T) {
			l := t.Open()
			f, ok := l.Req(t.Ctx(), wire.TypeHello, wire.Hello{HostNonce: 0xA5A55A5A, HostProtoVer: 1}.AppendBinary(nil))
			var info wire.DeviceInfo
			if !ok || f.H.Type != wire.TypeRspOK || info.UnmarshalBinary(f.Body) != nil {
				t.Fatalf("RSP_OK DeviceInfo", "", "ok=%v", ok)
			}
			if len(f.Body) < wire.DeviceInfoLen {
				t.Fatalf(fmt.Sprintf("body ≥ %d bytes", wire.DeviceInfoLen), "", "%d", len(f.Body))
			}
			if info.HostNonce != 0xA5A55A5A || info.ProtoVer != 1 {
				t.Fatalf("nonce echo + proto 1", "", "nonce %08x proto %d", info.HostNonce, info.ProtoVer)
			}
			if info.MaxFrame != wire.MaxFrame || info.MaxRules < 1 || info.MaxRules > 16 {
				t.Fatalf("max_frame 4128, max_rules 1..16", "", "max_frame %d max_rules %d", info.MaxFrame, info.MaxRules)
			}
		}},
	{ID: "CTP-HELLO-002", Title: "no PKT/EVT_STATS without a session; CAPTURE 1 before HELLO refused", Spec: "doc03 §6.1, §7",
		Hint: "proto.c session gating + host watchdog",
		Run: func(t *T) {
			l := t.Session()
			mustCfg(t, l, wire.TypeCapture, []byte{1}, "")
			// go silent: watchdog must end the session within 2 s
			time.Sleep(2300 * time.Millisecond)
			l.Drain()
			fs := l.Collect(t.Ctx(), 1300*time.Millisecond)
			for _, f := range fs {
				if f.H.Type == wire.TypePkt || f.H.Type == wire.TypeEvtStats {
					t.Fatalf("silence after watchdog", "watchdog must stop PKT and EVT_STATS", "got %s", wire.TypeName(f.H.Type))
				}
			}
			expectErr(t, l, wire.TypeCapture, []byte{1}, wire.ErrNoSession, "CAPTURE 1 needs a HELLO session")
			c := mustCfg(t, l, wire.TypeGetConfig, nil, "")
			if c.CaptureOn {
				t.Fatalf("capture off after watchdog", "", "capture_on=1")
			}
		}},
	// ---- requests ----------------------------------------------------------
	{ID: "CTP-REQ-001", Title: "exactly one response per request", Spec: "doc03 §5",
		Run: func(t *T) {
			l := t.Session()
			ids := map[uint16]int{}
			var all []byte
			for i := 0; i < 20; i++ {
				id := l.ID()
				ids[id] = 0
				all = append(all, l.Encode(wire.Header{Type: wire.TypePing, ReqID: id}, le32(uint32(i)))...)
			}
			_ = l.WriteRaw(all)
			for _, f := range l.Collect(t.Ctx(), 600*time.Millisecond) {
				if _, ok := ids[f.H.ReqID]; ok && (f.H.Type == wire.TypeRspOK || f.H.Type == wire.TypeRspErr) {
					ids[f.H.ReqID]++
				}
			}
			for id, n := range ids {
				if n != 1 {
					t.Fatalf("1 response each", "proto.c dispatcher / ctrl queue overflow", "req_id %d got %d", id, n)
				}
			}
		}},
	{ID: "CTP-REQ-002", Title: "duplicate req_id is answered from cache, not re-applied", Spec: "doc03 §5",
		Hint: "proto.c dedup cache (last 4) must be consulted before apply",
		Run: func(t *T) {
			l := t.Session()
			id := l.ID()
			first := l.Encode(wire.Header{Type: wire.TypeSetSnaplen, ReqID: id}, []byte{77, 0})
			_ = l.WriteRaw(first)
			r1, ok := l.Await(t.Ctx(), id, 300*time.Millisecond, nil)
			if !ok {
				t.Fatalf("response", "", "none")
			}
			mustCfg(t, l, wire.TypeSetSnaplen, []byte{88, 0}, "")
			_ = l.WriteRaw(first) // exact retry of the first request
			r2, ok := l.Await(t.Ctx(), id, 300*time.Millisecond, nil)
			if !ok || !bytes.Equal(r1.Body, r2.Body) {
				t.Fatalf("cached response re-sent unchanged", "", "ok=%v\n% x\n% x", ok, r1.Body, r2.Body)
			}
			c := mustCfg(t, l, wire.TypeGetConfig, nil, "")
			if c.Snaplen != 88 {
				t.Fatalf("snaplen stays 88", "", "snaplen %d (duplicate re-applied)", c.Snaplen)
			}
		}},
	{ID: "CTP-REQ-003", Title: "invalid argument → RSP_ERR ERR_INVALID_ARG", Spec: "doc03 §6.1",
		Run: func(t *T) {
			l := t.Session()
			expectErr(t, l, wire.TypeSetChannel, []byte{6, 7}, wire.ErrInvalidArg, "SET_CHANNEL bw > 2")
			expectErr(t, l, wire.TypeReboot, []byte{9}, wire.ErrInvalidArg, "REBOOT mode > 1")
		}},
	{ID: "CTP-REQ-004", Title: "unknown request type → ERR_UNKNOWN_TYPE (never silence)", Spec: "doc07 §4",
		Run: func(t *T) {
			l := t.Session()
			expectErr(t, l, 0x7E, []byte{1, 2, 3}, wire.ErrUnknownType, "proto.c default case")
			expectErr(t, l, wire.TypePkt, nil, wire.ErrUnknownType, "device→host types are not requests")
		}},
	{ID: "CTP-REQ-005", Title: "longer bodies are accepted (forward compat)", Spec: "doc03 §11",
		Run: func(t *T) {
			l := t.Session()
			f, ok := l.Req(t.Ctx(), wire.TypePing, append(le32(9), 1, 2, 3, 4, 5, 6, 7, 8))
			if !ok || f.H.Type != wire.TypeRspOK {
				t.Fatalf("RSP_OK", "wire.c decoders must ignore trailing bytes", "ok=%v type=%s", ok, wire.TypeName(f.H.Type))
			}
			mustCfg(t, l, wire.TypeSetSnaplen, []byte{64, 0, 0xAA, 0xBB}, "wire.c decoders must ignore trailing bytes")
		}},
	{ID: "CTP-REQ-006", Title: "short body → ERR_BAD_BODY", Spec: "doc03 §6.1",
		Run: func(t *T) {
			l := t.Session()
			expectErr(t, l, wire.TypeSetDriverMask, []byte{1, 2, 3}, wire.ErrBadBody, "rd.c sticky error → ERR_BAD_BODY")
			expectErr(t, l, wire.TypeHello, []byte{1}, wire.ErrBadBody, "rd.c sticky error → ERR_BAD_BODY")
		}},
	// ---- configuration -----------------------------------------------------
	{ID: "CTP-CFG-001", Title: "SET_CHANNEL returns the readback", Spec: "doc03 §5",
		Run: func(t *T) {
			l := t.Session()
			c := mustCfg(t, l, wire.TypeSetChannel, []byte{6, 0}, "")
			if c.Channel != 6 || c.BW != wire.BW20 || c.Band != wire.Band2G4 {
				t.Fatalf("ch 6 HT20 band 2.4", "config.c readback", "ch %d bw %s band %d", c.Channel, c.BW, c.Band)
			}
		}},
	{ID: "CTP-CFG-002", Title: "country table is enforced (01 rejects ch 13; FI allows it)", Spec: "doc02 §5, doc03 §5",
		Hint: "radio port must map driver rejection to ERR_CHANNEL_NOT_ALLOWED",
		Run: func(t *T) {
			l := t.Session()
			expectErr(t, l, wire.TypeSetChannel, []byte{13, 0}, wire.ErrChannelNotAllowed, "")
			c := mustCfg(t, l, wire.TypeSetCountry, wire.MakeCountry("FI", 0, 0, 0).AppendBinary(nil), "")
			if c.Country.Code() != "FI" || c.Country.SChan != 1 || c.Country.NChan != 13 {
				t.Fatalf("FI 1/13", "", "%q %d/%d", c.Country.Code(), c.Country.SChan, c.Country.NChan)
			}
			c = mustCfg(t, l, wire.TypeSetChannel, []byte{13, 0}, "")
			if c.Channel != 13 {
				t.Fatalf("ch 13", "", "ch %d", c.Channel)
			}
			// back to 01: ch 13 no longer allowed → device moves to schan
			c = mustCfg(t, l, wire.TypeSetCountry, wire.MakeCountry("01", 0, 0, 0).AppendBinary(nil), "")
			if c.Channel != 1 {
				t.Fatalf("fallback to ch 1 after country change", "config.c country apply fallback (doc03 §6.1)", "ch %d", c.Channel)
			}
		}},
	{ID: "CTP-CFG-003", Title: "band follows the channel (2.4 ↔ 5 GHz)", Spec: "doc02 §4",
		Run: func(t *T) {
			l := t.Session()
			needCap(t, wire.Cap5GHz, "5GHZ")
			c := mustCfg(t, l, wire.TypeSetChannel, []byte{36, 0}, "radio port band switch")
			if c.Band != wire.Band5G || c.Channel != 36 {
				t.Fatalf("ch 36 band 5", "", "ch %d band %d", c.Channel, c.Band)
			}
			c = mustCfg(t, l, wire.TypeSetChannel, []byte{1, 0}, "")
			if c.Band != wire.Band2G4 {
				t.Fatalf("band 2.4", "", "band %d", c.Band)
			}
		}},
	{ID: "CTP-CFG-004", Title: "HT40 on 5 GHz reports the derived direction", Spec: "doc03 §6.1",
		Run: func(t *T) {
			l := t.Session()
			needCap(t, wire.Cap5GHz, "5GHZ")
			if t.Info().Caps&wire.CapHT40 == 0 {
				expectErr(t, l, wire.TypeSetChannel, []byte{40, 1}, wire.ErrNotSupported, "")
				return
			}
			c := mustCfg(t, l, wire.TypeSetChannel, []byte{40, 1}, "")
			if c.BW != wire.BW40Minus {
				t.Fatalf("ch 40 HT40 → 40-", "config.c 5 GHz pair rule", "bw %s", c.BW)
			}
			c = mustCfg(t, l, wire.TypeSetChannel, []byte{6, 1}, "")
			if c.BW != wire.BW40Plus {
				t.Fatalf("ch 6 HT40+ stays 40+", "", "bw %s", c.BW)
			}
			expectErr(t, l, wire.TypeSetChannel, []byte{11, 1}, wire.ErrChannelNotAllowed, "HT40+ partner 15 does not exist")
		}},
	{ID: "CTP-CFG-005", Title: "snaplen 0 and N read back", Spec: "doc02 §3",
		Run: func(t *T) {
			l := t.Session()
			for _, n := range []uint16{0, 1, 32, 4096, 65535} {
				c := mustCfg(t, l, wire.TypeSetSnaplen, binary.LittleEndian.AppendUint16(nil, n), "")
				if c.Snaplen != n {
					t.Fatalf(fmt.Sprintf("snaplen %d", n), "", "%d", c.Snaplen)
				}
			}
		}},
	{ID: "CTP-CFG-006", Title: "SET_FILTER: rules_crc32 equals host CRC", Spec: "doc04 App. A",
		Hint: "filter.c must store CRC of the exact body; n_rules = enabled rules",
		Run: func(t *T) {
			l := t.Session()
			rm := int8(-80)
			s := filter.Set{Rules: append([]filter.Rule{filter.Beacons(), filter.DropBeacons()}, filter.EAPOL()...), Global: filter.Global{DropFCSErr: true, RSSIMin: &rm}}
			body, _ := s.Encode()
			c := mustCfg(t, l, wire.TypeSetFilter, body, "")
			if c.RulesCRC32 != codec.CRC32(body) || c.NRules != 4 {
				t.Fatalf(fmt.Sprintf("crc %08x n 4", codec.CRC32(body)), "", "crc %08x n %d", c.RulesCRC32, c.NRules)
			}
			c = mustCfg(t, l, wire.TypeSetFilter, make([]byte, 8), "")
			if c.RulesCRC32 != wire.EmptyFilterCRC || c.NRules != 0 {
				t.Fatalf("empty filter crc 6522DF69", "", "%08x", c.RulesCRC32)
			}
		}},
	{ID: "CTP-CFG-007", Title: "invalid SET_FILTER → ERR_FILTER_INVALID, config unchanged", Spec: "doc03 §6.1",
		Hint: "filter.c decode/validate before publishing",
		Run: func(t *T) {
			l := t.Session()
			before := mustCfg(t, l, wire.TypeGetConfig, nil, "")
			bad := [][]byte{
				{1, 0, 0, 0, 0, 0, 0, 0, 0},                         // rule with 0 terms
				{0, 0x80, 0, 0, 0, 0, 0, 0},                         // unknown gflags
				{0, 0, 0, 1, 0, 0, 0, 0},                            // reserved != 0
				{0, 0, 0, 0, 0, 0, 0, 0, 0xEE},                      // trailing
				{1, 0, 0, 0, 0, 0, 0, 0, 1, 0, 0, 17, 0},            // len 17
				{1, 0, 0, 0, 0, 0, 0, 0, 1, 0, 0, 1, 2, 0x80, 0xff}, // unknown tflags
				{17, 0, 0, 0, 0, 0, 0, 0},                           // too many rules
			}
			for _, b := range bad {
				expectErr(t, l, wire.TypeSetFilter, b, wire.ErrFilterInvalid, fmt.Sprintf("body % x", b))
			}
			after := mustCfg(t, l, wire.TypeGetConfig, nil, "")
			if after != before {
				t.Fatalf("config unchanged", "", "%+v -> %+v", before, after)
			}
		}},
	{ID: "CTP-CFG-008", Title: "driver mask readback", Spec: "doc03 §6",
		Run: func(t *T) {
			l := t.Session()
			c := mustCfg(t, l, wire.TypeSetDriverMask, wire.SetDriverMask{TypeMask: 0x7, CtrlMask: 0x20000000}.AppendBinary(nil), "")
			if c.TypeMask != 7 || c.CtrlMask != 0x20000000 {
				t.Fatalf("masks 7 / 20000000", "", "%x / %x", c.TypeMask, c.CtrlMask)
			}
		}},
	{ID: "CTP-CFG-009", Title: "unknown country → ERR_COUNTRY_INVALID", Spec: "doc03 §6.1",
		Run: func(t *T) {
			l := t.Session()
			expectErr(t, l, wire.TypeSetCountry, wire.MakeCountry("ZZ", 0, 0, 0).AppendBinary(nil), wire.ErrCountryInvalid, "")
			expectErr(t, l, wire.TypeSetCountry, wire.MakeCountry("FI", 3, 0, 0).AppendBinary(nil), wire.ErrInvalidArg, "mode > 1")
		}},
	{ID: "CTP-CFG-010", Title: "manual country range unlocks ch 14", Spec: "doc02 §5, doc10 §2",
		Run: func(t *T) {
			l := t.Session()
			if t.Info().Caps&wire.CapManualCountry == 0 {
				expectErr(t, l, wire.TypeSetCountry, wire.MakeCountry("JP", 1, 1, 14).AppendBinary(nil), wire.ErrNotSupported, "")
				return
			}
			c := mustCfg(t, l, wire.TypeSetCountry, wire.MakeCountry("JP", 1, 1, 14).AppendBinary(nil), "")
			if c.Country.Mode != 1 || c.Country.NChan != 14 {
				t.Fatalf("mode 1 1/14", "", "%+v", c.Country)
			}
			if c = mustCfg(t, l, wire.TypeSetChannel, []byte{14, 0}, ""); c.Channel != 14 {
				t.Fatalf("ch 14", "", "%d", c.Channel)
			}
			expectErr(t, l, wire.TypeSetCountry, wire.MakeCountry("JP", 1, 10, 10).AppendBinary(nil), wire.ErrCountryInvalid, "range past ch 14")
		}},
	// ---- filter on device ----------------------------------------------------
	{ID: "CTP-FLT-001", Title: "every captured frame satisfies the active filter (filter.Match)", Spec: "doc04 App. A",
		Hint: "filter.c match() disagrees with the Go reference",
		Run: func(t *T) {
			l := t.Session()
			s := filter.Set{Rules: []filter.Rule{filter.Beacons(), filter.Ctrl()}, Global: filter.Global{DropFCSErr: true}}
			body, _ := s.Encode()
			mustCfg(t, l, wire.TypeSetFilter, body, "")
			fs := pkts(captureFor(t, l, 1500*time.Millisecond))
			if len(fs) == 0 {
				t.Skip("no traffic captured (quiet channel)")
			}
			for _, f := range fs {
				m, data, err := wire.DecodePkt(f.Body)
				if err != nil {
					t.Fatalf("valid PKT", "", "%v", err)
				}
				if m.CapLen == m.OrigLen && !s.Match(data, m) {
					t.Fatalf("frame passes filter.Match", "", "frame % x flags %02x", data, m.Flags)
				}
			}
		}},
	{ID: "CTP-FLT-RF-001", Title: "filter decisions on injected reference traffic (needs injector)", Spec: "doc06 §5",
		Run: func(t *T) {
			if !t.env.Injector {
				t.Skip("needs injector dongle")
			}
			t.Skip("injector scenarios not implemented yet")
		}},
	// ---- packets -------------------------------------------------------------
	{ID: "CTP-PKT-001", Title: "PKT field sanity with snaplen 64", Spec: "doc03 §9",
		Hint: "capture.c snaplen/truncation/rx_seq",
		Run: func(t *T) {
			l := t.Session()
			mustCfg(t, l, wire.TypeSetChannel, []byte{6, 0}, "")
			mustCfg(t, l, wire.TypeSetSnaplen, []byte{64, 0}, "")
			fs := pkts(captureFor(t, l, 1500*time.Millisecond))
			if len(fs) == 0 {
				t.Skip("no traffic captured")
			}
			st, _ := l.Stats(t.Ctx(), false)
			var last uint16
			for i, f := range fs {
				m, _, err := wire.DecodePkt(f.Body)
				if err != nil {
					t.Fatalf("valid PKT", "", "%v", err)
				}
				switch {
				case m.CapLen > 64:
					t.Fatalf("cap_len ≤ 64", "", "%d", m.CapLen)
				case m.OrigLen < m.CapLen:
					t.Fatalf("orig_len ≥ cap_len", "", "%d < %d", m.OrigLen, m.CapLen)
				case (m.Flags&wire.PktTruncated != 0) != (m.CapLen < m.OrigLen):
					t.Fatalf("truncated iff cap_len < orig_len", "", "flags %02x cap %d orig %d", m.Flags, m.CapLen, m.OrigLen)
				case m.Channel != 6:
					t.Fatalf("channel 6", "", "%d", m.Channel)
				}
				if i == 0 && m.RxSeq != 0 {
					t.Fatalf("rx_seq restarts at 0 on CAPTURE 1", "", "first rx_seq %d", m.RxSeq)
				}
				if i > 0 && m.RxSeq != last+1 && st.DropRing == 0 {
					t.Fatalf("rx_seq continuous when drop_ring == 0", "", "%d after %d", m.RxSeq, last)
				}
				last = m.RxSeq
			}
		}},
	{ID: "CTP-PKT-002", Title: "snaplen 0 delivers full frames incl. FCS", Spec: "doc03 §9",
		Run: func(t *T) {
			l := t.Session()
			fs := pkts(captureFor(t, l, 1000*time.Millisecond))
			if len(fs) == 0 {
				t.Skip("no traffic captured")
			}
			for _, f := range fs {
				m, data, _ := wire.DecodePkt(f.Body)
				if m.OrigLen <= wire.MaxCapLen && m.CapLen != m.OrigLen {
					t.Fatalf("cap_len == orig_len", "", "%d != %d", m.CapLen, m.OrigLen)
				}
				if m.Flags&wire.PktFCSErr == 0 && m.CapLen == m.OrigLen && m.CapLen >= 4 {
					n := len(data) - 4
					if codec.CRC32(data[:n]) != binary.LittleEndian.Uint32(data[n:]) {
						t.Fatalf("FCS valid when fcs_err clear", "capture.c must copy sig_len bytes incl. FCS", "frame % x", data[:min(len(data), 32)])
					}
				}
			}
		}},
	// ---- liveness --------------------------------------------------------------
	{ID: "CTP-LIVE-RTT-001", Title: "PING RTT p99 < 5 ms under capture load", Spec: "doc03 §12.3, doc07 M6",
		Hint: "link task must drain ctrl queue before packets",
		Run: func(t *T) {
			l := t.Session()
			mustCfg(t, l, wire.TypeCapture, []byte{1}, "")
			var rtts []time.Duration
			for i := 0; i < 100; i++ {
				d, ok := l.Ping(t.Ctx(), 300*time.Millisecond)
				if !ok {
					t.Fatalf("all PINGs answered", "", "ping %d lost", i)
				}
				rtts = append(rtts, d)
				time.Sleep(5 * time.Millisecond)
			}
			sort.Slice(rtts, func(i, j int) bool { return rtts[i] < rtts[j] })
			if p99 := rtts[98]; p99 > 5*time.Millisecond {
				t.Fatalf("p99 < 5 ms", "", "p99 %v (median %v)", p99, rtts[50])
			}
		}},
	{ID: "CTP-LIVE-002", Title: "ctrl frames are not starved by packets (RTT < 50 ms at full load)", Spec: "doc03 §5",
		Hint: "link task priority: ctrl-out queue first",
		Run: func(t *T) {
			l := t.Session()
			mustCfg(t, l, wire.TypeCapture, []byte{1}, "")
			for i := 0; i < 20; i++ {
				d, ok := l.Ping(t.Ctx(), 300*time.Millisecond)
				if !ok || d > 50*time.Millisecond {
					t.Fatalf("RTT < 50 ms", "", "ping %d ok=%v rtt %v", i, ok, d)
				}
				time.Sleep(20 * time.Millisecond)
			}
		}},
	{ID: "CTP-LIVE-003", Title: "host watchdog: 2 s silence stops capture", Spec: "doc03 §7.4",
		Hint: "ctrl task 2 s host watchdog",
		Run: func(t *T) {
			l := t.Session()
			mustCfg(t, l, wire.TypeCapture, []byte{1}, "")
			time.Sleep(2500 * time.Millisecond)
			l.Drain()
			fs := l.Collect(t.Ctx(), 500*time.Millisecond)
			if n := len(pkts(fs)); n > 0 {
				t.Fatalf("no PKT after watchdog", "", "%d PKT", n)
			}
			l2 := l
			if _, ok := l2.Hello(t.Ctx()); !ok {
				t.Fatalf("HELLO after watchdog", "", "none")
			}
			if c := mustCfg(t, l2, wire.TypeGetConfig, nil, ""); c.CaptureOn {
				t.Fatalf("capture_on 0", "", "1")
			}
		}},
	{ID: "CTP-LIVE-004", Title: "EVT_STATS every 1 s ± 100 ms while host alive", Spec: "doc03 §6",
		Hint: "ctrl task 1 s timer",
		Run: func(t *T) {
			l := t.Session()
			stop := make(chan struct{})
			go l.KeepAlive(stop)
			fs := l.Collect(t.Ctx(), 3600*time.Millisecond)
			close(stop)
			var ts []time.Time
			for _, f := range fs {
				if f.H.Type == wire.TypeEvtStats {
					var st wire.Stats
					if err := st.UnmarshalBinary(f.Body); err != nil {
						t.Fatalf("valid Stats body", "", "%v", err)
					}
					ts = append(ts, f.At)
				}
			}
			if len(ts) < 3 {
				t.Fatalf("≥3 EVT_STATS in 3.6 s", "", "%d", len(ts))
			}
			for i := 1; i < len(ts); i++ {
				if d := ts[i].Sub(ts[i-1]); d < 900*time.Millisecond || d > 1100*time.Millisecond {
					t.Fatalf("interval 1 s ± 100 ms", "", "interval %v", d)
				}
			}
		}},
	// ---- reset ---------------------------------------------------------------
	{ID: "CTP-RST-001", Title: "REBOOT → RSP_OK, EVT_RESET SOFTWARE, boot config", Spec: "doc03 §6, §6.1",
		Hint: "REBOOT must answer first, then reboot; boot state per doc03 §6.1",
		Run: func(t *T) {
			l := t.Session()
			mustCfg(t, l, wire.TypeSetSnaplen, []byte{99, 0}, "")
			f, ok := l.Req(t.Ctx(), wire.TypeReboot, []byte{0})
			if !ok || f.H.Type != wire.TypeRspOK {
				t.Fatalf("RSP_OK", "", "ok=%v", ok)
			}
			l = waitReset(t, l)
			if _, ok := l.Hello(t.Ctx()); !ok {
				t.Fatalf("HELLO after reboot", "", "none")
			}
			c := mustCfg(t, l, wire.TypeGetConfig, nil, "")
			want := wire.BootConfig()
			if !c.SameSettings(want) || c.CfgGen != 0 {
				t.Fatalf(fmt.Sprintf("boot config %+v", want), "", "%+v", c)
			}
		}},
	{ID: "CTP-RST-002", Title: "desired-config re-push after reboot restores identical CONFIG", Spec: "doc03 §7.6",
		Run: func(t *T) {
			l := t.Session()
			steps := []struct {
				typ  uint8
				body []byte
			}{
				{wire.TypeSetCountry, wire.MakeCountry("FI", 0, 0, 0).AppendBinary(nil)},
				{wire.TypeSetChannel, []byte{11, 0}},
				{wire.TypeSetSnaplen, []byte{128, 0}},
			}
			fb, _ := filter.Set{Rules: []filter.Rule{filter.Mgmt()}}.Encode()
			steps = append(steps, struct {
				typ  uint8
				body []byte
			}{wire.TypeSetFilter, fb})
			var before wire.Config
			for _, s := range steps {
				before = mustCfg(t, l, s.typ, s.body, "")
			}
			if _, ok := l.Req(t.Ctx(), wire.TypeReboot, []byte{0}); !ok {
				t.Fatalf("RSP_OK", "", "none")
			}
			l = waitReset(t, l)
			l.Hello(t.Ctx())
			var after wire.Config
			for _, s := range steps {
				after = mustCfg(t, l, s.typ, s.body, "")
			}
			if !after.SameSettings(before) {
				t.Fatalf(fmt.Sprintf("%+v", before), "", "%+v", after)
			}
		}},
	// ---- robustness ------------------------------------------------------------
	{ID: "CTP-ROB-001", Title: "fuzz: garbage frames interleaved with PINGs, device stays alive", Spec: "doc03 §12.1",
		Hint: "a decoder crashed or stopped answering",
		Run: func(t *T) {
			l := t.Session()
			rng := rand.New(rand.NewSource(42))
			for i := 0; i < 150; i++ {
				var b []byte
				switch rng.Intn(5) {
				case 0: // random bytes
					b = make([]byte, rng.Intn(200))
					rng.Read(b)
				case 1: // valid CRC, random type/body
					body := make([]byte, rng.Intn(64))
					rng.Read(body)
					typ := uint8(rng.Intn(256))
					if typ == wire.TypeReboot {
						typ = 0x31 // never reboot from the fuzzer
					}
					b = l.Encode(wire.Header{Type: typ, ReqID: 0x6000 + uint16(i)}, body)
				case 2: // truncated bodies of known types
					types := []uint8{1, 2, 0x11, 0x12, 0x13, 0x14, 0x15, 0x20, 0x21}
					b = l.Encode(wire.Header{Type: types[rng.Intn(len(types))], ReqID: 0x6000 + uint16(i)}, []byte{byte(rng.Intn(256))})
				case 3: // random SET_FILTER
					body := make([]byte, 8+rng.Intn(80))
					rng.Read(body)
					body[0] = byte(rng.Intn(4))
					b = l.Encode(wire.Header{Type: wire.TypeSetFilter, ReqID: 0x6000 + uint16(i)}, body)
				default:
					b = []byte{0}
				}
				_ = l.WriteRaw(b)
				if i%10 == 0 {
					_ = l.WriteRaw([]byte{0})
					if _, ok := l.Ping(t.Ctx(), 500*time.Millisecond); !ok {
						t.Fatalf("PING answered", "", "after %d fuzz frames", i)
					}
				}
			}
			_ = l.WriteRaw([]byte{0})
			if _, ok := l.Hello(t.Ctx()); !ok {
				t.Fatalf("HELLO after fuzz", "", "none")
			}
		}},
	{ID: "CTP-ROB-002", Title: "host vanishes mid-stream; next open recovers", Spec: "doc03 §7.6",
		Run: func(t *T) {
			l := t.Session()
			mustCfg(t, l, wire.TypeCapture, []byte{1}, "")
			time.Sleep(300 * time.Millisecond)
			_ = l.Close()
			l2 := t.Open()
			if _, ok := l2.Hello(t.Ctx()); !ok {
				t.Fatalf("HELLO on reopen", "", "none")
			}
			if _, ok := l2.Ping(t.Ctx(), 300*time.Millisecond); !ok {
				t.Fatalf("PING on reopen", "", "none")
			}
		}},
}

// waitReset waits for EVT_RESET (reason SOFTWARE). If the transport dropped
// (real USB re-enumeration), it reopens.
func waitReset(t *T, l *Link) *Link {
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case f := <-l.frames:
			if f.H.Type == wire.TypeEvtReset {
				var r wire.Reset
				if err := r.UnmarshalBinary(f.Body); err != nil || r.Reason != wire.ResetSoftware {
					t.Fatalf("EVT_RESET reason SOFTWARE", "", "%v reason %v", err, r.Reason)
				}
				return l
			}
		case <-l.done:
			time.Sleep(500 * time.Millisecond)
			l = t.Open()
		case <-time.After(200 * time.Millisecond):
		case <-t.Ctx().Done():
			t.Fatalf("EVT_RESET", "", "timeout")
		}
	}
	t.Fatalf("EVT_RESET within 8 s", "", "none")
	return nil
}
