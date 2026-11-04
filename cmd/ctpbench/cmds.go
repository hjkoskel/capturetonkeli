package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hjkoskel/capturetonkeli/conformance"
	"github.com/hjkoskel/capturetonkeli/ctp"
	"github.com/hjkoskel/capturetonkeli/ctp/codec"
	"github.com/hjkoskel/capturetonkeli/ctp/dot11"
	"github.com/hjkoskel/capturetonkeli/ctp/filter"
	"github.com/hjkoskel/capturetonkeli/ctp/fwver"
	"github.com/hjkoskel/capturetonkeli/ctp/pcapng"
	"github.com/hjkoskel/capturetonkeli/ctp/serialport"
	"github.com/hjkoskel/capturetonkeli/ctp/wire"
	"github.com/hjkoskel/capturetonkeli/internal/vectorgen"
)

func init() {
	register(command{name: "list", summary: "enumerate serial ports and dongles", run: cmdList})
	register(command{name: "doctor", summary: "environment check: port, permissions, busy, HELLO", run: cmdDoctor})
	register(command{name: "info", summary: "HELLO + GET_CONFIG", example: "ctpbench --port sim info", run: cmdInfo})
	var pingN int
	var pingIv time.Duration
	register(command{name: "ping", summary: "RTT measurement", example: "ctpbench ping --count 50",
		flags: func(fs *flag.FlagSet) {
			fs.IntVar(&pingN, "count", 20, "pings")
			fs.DurationVar(&pingIv, "interval", 20*time.Millisecond, "interval")
		},
		run: func(ctx context.Context, e *env) (any, error) { return cmdPing(ctx, e, pingN, pingIv) }})
	register(command{name: "config get", summary: "GET_CONFIG", run: func(ctx context.Context, e *env) (any, error) {
		return configCmd(ctx, e, func(c *ctp.Client) (wire.Config, error) { return c.GetConfig(ctx) })
	}})
	var bw string
	register(command{name: "channel set", summary: "SET_CHANNEL", example: "ctpbench channel set 36 --bw 40+",
		flags: func(fs *flag.FlagSet) { fs.StringVar(&bw, "bw", "20", "20 | 40+ | 40-") },
		run: func(ctx context.Context, e *env) (any, error) {
			if len(e.args) != 1 {
				return nil, errf(exitUsage, "USAGE", "ctpbench channel set CH [--bw 20|40+|40-]", "need one channel")
			}
			ch, err := strconv.ParseUint(e.args[0], 10, 8)
			b, err2 := wire.ParseBandwidth(bw)
			if err != nil || err2 != nil {
				return nil, errf(exitUsage, "USAGE", "ctpbench channel set CH [--bw 20|40+|40-]", "bad channel/bw")
			}
			return configCmd(ctx, e, func(c *ctp.Client) (wire.Config, error) { return c.SetChannel(ctx, uint8(ch), b) })
		}})
	var manual bool
	var schan, nchan uint
	register(command{name: "country set", summary: "SET_COUNTRY", example: "ctpbench country set FI | country set JP --manual --schan 1 --nchan 14",
		flags: func(fs *flag.FlagSet) {
			fs.BoolVar(&manual, "manual", false, "legacy manual range")
			fs.UintVar(&schan, "schan", 1, "first 2.4 GHz channel (manual)")
			fs.UintVar(&nchan, "nchan", 13, "number of 2.4 GHz channels (manual)")
		},
		run: func(ctx context.Context, e *env) (any, error) {
			if len(e.args) != 1 || len(e.args[0]) != 2 {
				return nil, errf(exitUsage, "USAGE", "ctpbench country set CC", "need a 2-letter code")
			}
			mode := wire.CountryValidated
			if manual {
				mode = wire.CountryManual
			}
			cc := wire.MakeCountry(e.args[0], mode, uint8(schan), uint8(nchan))
			return configCmd(ctx, e, func(c *ctp.Client) (wire.Config, error) { return c.SetCountry(ctx, cc) })
		}})
	register(command{name: "snaplen set", summary: "SET_SNAPLEN (0 = unlimited)", run: func(ctx context.Context, e *env) (any, error) {
		if len(e.args) != 1 {
			return nil, errf(exitUsage, "USAGE", "ctpbench snaplen set N", "need N")
		}
		n, err := strconv.ParseUint(e.args[0], 0, 16)
		if err != nil {
			return nil, errf(exitUsage, "USAGE", "ctpbench snaplen set N", "%v", err)
		}
		return configCmd(ctx, e, func(c *ctp.Client) (wire.Config, error) { return c.SetSnaplen(ctx, uint16(n)) })
	}})
	var tmask, cmask string
	register(command{name: "drivermask set", summary: "SET_DRIVER_MASK",
		flags: func(fs *flag.FlagSet) {
			fs.StringVar(&tmask, "type", "0xffffffff", "type mask")
			fs.StringVar(&cmask, "ctrl", "0xff800000", "ctrl mask")
		},
		run: func(ctx context.Context, e *env) (any, error) {
			t, err1 := strconv.ParseUint(tmask, 0, 32)
			c2, err2 := strconv.ParseUint(cmask, 0, 32)
			if err1 != nil || err2 != nil {
				return nil, errf(exitUsage, "USAGE", "--type 0x7 --ctrl 0xff800000", "bad mask")
			}
			return configCmd(ctx, e, func(c *ctp.Client) (wire.Config, error) { return c.SetDriverMask(ctx, uint32(t), uint32(c2)) })
		}})
	var ffile string
	var presets multiFlag
	var fclear bool
	filterFlags := func(fs *flag.FlagSet) {
		fs.StringVar(&ffile, "file", "", "filter JSON (web UI format)")
		fs.Var(&presets, "preset", "preset NAME[:ARG] (repeatable): beacons, probereq, mgmt, ctrl, data, dropbeacons, deauth, eapol, from:MAC, to:MAC, bssid:MAC")
		fs.BoolVar(&fclear, "clear", false, "empty filter")
	}
	loadFilter := func() (filter.Set, error) {
		var s filter.Set
		switch {
		case fclear:
			return s, nil
		case ffile != "":
			b, err := os.ReadFile(ffile)
			if err != nil {
				return s, errf(exitUsage, "USAGE", "", "%v", err)
			}
			if err := json.Unmarshal(b, &s); err != nil {
				return s, errf(exitUsage, "BAD_FILTER", "see docs/04 §6 JSON format", "%v", err)
			}
		case len(presets) > 0:
			for _, p := range presets {
				r, err := filter.Preset(p)
				if err != nil {
					return s, errf(exitUsage, "BAD_FILTER", "", "%v", err)
				}
				s.Rules = append(s.Rules, r...)
			}
		default:
			return s, errf(exitUsage, "USAGE", "--file f.json | --preset NAME | --clear", "no filter given")
		}
		if err := s.Validate(0); err != nil {
			return s, errf(exitUsage, "BAD_FILTER", "", "%v", err)
		}
		return s, nil
	}
	register(command{name: "filter set", summary: "SET_FILTER (+ CRC verify)", example: "ctpbench filter set --preset beacons --preset eapol", flags: filterFlags,
		run: func(ctx context.Context, e *env) (any, error) {
			s, err := loadFilter()
			if err != nil {
				return nil, err
			}
			return configCmd(ctx, e, func(c *ctp.Client) (wire.Config, error) { return c.SetFilter(ctx, s) })
		}})
	register(command{name: "filter check", summary: "validate + encode locally (no device)", flags: filterFlags,
		run: func(ctx context.Context, e *env) (any, error) {
			s, err := loadFilter()
			if err != nil {
				return nil, err
			}
			b, _ := s.Encode()
			return map[string]any{"body_hex": hex.EncodeToString(b), "crc": fmt.Sprintf("%08X", codec.CRC32(b)), "n_rules": b[0], "len": len(b)}, nil
		}})
	register(command{name: "capture on", summary: "CAPTURE 1", run: func(ctx context.Context, e *env) (any, error) {
		return configCmd(ctx, e, func(c *ctp.Client) (wire.Config, error) { return c.Capture(ctx, true) })
	}})
	register(command{name: "capture off", summary: "CAPTURE 0", run: func(ctx context.Context, e *env) (any, error) {
		return configCmd(ctx, e, func(c *ctp.Client) (wire.Config, error) { return c.Capture(ctx, false) })
	}})
	var capDur time.Duration
	var capCount int
	var capPkts bool
	var capPcap string
	register(command{name: "capture run", summary: "capture for --duration/--count; summary with counts, drops, histogram",
		example: "ctpbench --port sim capture run --duration 3s",
		flags: func(fs *flag.FlagSet) {
			fs.DurationVar(&capDur, "duration", 0, "capture time")
			fs.IntVar(&capCount, "count", 0, "stop after N packets")
			fs.BoolVar(&capPkts, "jsonl-packets", false, "write packets.jsonl to --out-dir")
			fs.StringVar(&capPcap, "pcapng", "", "write a pcapng file (bare file names go to --out-dir)")
		},
		run: func(ctx context.Context, e *env) (any, error) {
			return cmdCaptureRun(ctx, e, capDur, capCount, capPkts, capPcap)
		}})
	var sreset bool
	register(command{name: "stats", summary: "GET_STATS", flags: func(fs *flag.FlagSet) { fs.BoolVar(&sreset, "reset", false, "reset after read") },
		run: func(ctx context.Context, e *env) (any, error) {
			return withClient(ctx, e, func(c *ctp.Client) (any, error) { return c.Stats(ctx, sreset) })
		}})
	var rbDownload, yes bool
	register(command{name: "reboot", summary: "REBOOT (needs --yes)", flags: func(fs *flag.FlagSet) {
		fs.BoolVar(&rbDownload, "download", false, "reboot into download mode")
		fs.BoolVar(&yes, "yes", false, "really reboot")
	}, run: func(ctx context.Context, e *env) (any, error) { return cmdReboot(ctx, e, rbDownload, yes) }})
	var monDur time.Duration
	register(command{name: "monitor", summary: "JSONL stream of events/stats/gaps (no packet bodies)",
		flags: func(fs *flag.FlagSet) { fs.DurationVar(&monDur, "duration", 5*time.Second, "how long") },
		run:   func(ctx context.Context, e *env) (any, error) { return cmdMonitor(ctx, e, monDur) }})
	var rawID uint
	register(command{name: "raw send", summary: "send TYPE BODYHEX, print responses", example: "ctpbench raw send 0x02 01000000",
		flags: func(fs *flag.FlagSet) { fs.UintVar(&rawID, "req-id", 0x7123, "req_id") },
		run:   func(ctx context.Context, e *env) (any, error) { return cmdRawSend(ctx, e, uint16(rawID)) }})
	var crun, cskip, tcc string
	var clist bool
	var tch uint
	register(command{name: "conformance", summary: "protocol conformance suite (docs/06 §4)",
		example: "ctpbench --port /dev/pts/5 conformance --skip 'CTP-(FLT-RF|LIVE-RTT)' --out-dir bench-out/last",
		flags: func(fs *flag.FlagSet) {
			fs.StringVar(&crun, "run", "", "regex of case IDs to run")
			fs.StringVar(&cskip, "skip", "", "regex of case IDs to skip")
			fs.BoolVar(&clist, "list", false, "list cases (no device)")
			fs.StringVar(&tcc, "traffic-country", "01", "country for traffic-dependent cases")
			fs.UintVar(&tch, "traffic-channel", 6, "busy channel for traffic-dependent cases")
		},
		run: func(ctx context.Context, e *env) (any, error) {
			return cmdConformance(ctx, e, crun, cskip, clist, tcc, uint8(tch))
		}})
	var simPTY bool
	var simTCP string
	var simLink string
	register(command{name: "sim serve", summary: "run the Go simulator on a PTY or TCP",
		example: "ctpbench sim serve --pty --timeout 1h",
		flags: func(fs *flag.FlagSet) {
			fs.BoolVar(&simPTY, "pty", false, "serve on a pseudo-terminal (prints its path)")
			fs.StringVar(&simTCP, "tcp", "", "listen address, e.g. 127.0.0.1:7777")
			fs.StringVar(&simLink, "link", "", "with --pty: also create this symlink to the PTY")
		},
		run: func(ctx context.Context, e *env) (any, error) { return cmdSimServe(ctx, e, simPTY, simTCP, simLink) }})
	var vdir string
	register(command{name: "vectors check", summary: "verify testvectors/ match the library",
		flags: func(fs *flag.FlagSet) { fs.StringVar(&vdir, "dir", "testvectors", "vector directory") },
		run: func(ctx context.Context, e *env) (any, error) {
			diff, err := vectorgen.Check(vdir)
			if err != nil {
				return nil, err
			}
			res := map[string]any{"dir": vdir, "stale": diff, "ok": len(diff) == 0}
			if len(diff) > 0 {
				return nil, testFailure{res}
			}
			return res, nil
		}})
	register(command{name: "verify-pcap", summary: "parse a pcapng file, check structure/radiotap", run: cmdVerifyPcap})
	register(command{name: "wait-device", summary: "wait until the device answers HELLO (use --timeout)",
		run: cmdWaitDevice})
	for _, n := range []string{
		"inject", "raw stream", "report merge", "schema"} {
		n := n
		register(command{name: n, summary: "not implemented yet", run: func(context.Context, *env) (any, error) {
			return nil, errf(exitUsage, "NOT_IMPLEMENTED", "planned for a later milestone (docs/07 §5)", "%s is not implemented yet", n)
		}})
	}
}

type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }

func withClient(ctx context.Context, e *env, f func(c *ctp.Client) (any, error)) (any, error) {
	c, err := e.client(ctx, ctp.Options{PingInterval: -1})
	if err != nil {
		return nil, err
	}
	defer c.Abandon() // leave device state as configured (no CAPTURE 0)
	return f(c)
}

func configCmd(ctx context.Context, e *env, f func(c *ctp.Client) (wire.Config, error)) (any, error) {
	return withClient(ctx, e, func(c *ctp.Client) (any, error) {
		cfg, err := f(c)
		if err != nil {
			return nil, err
		}
		return map[string]any{"config": cfgJSON(cfg)}, nil
	})
}

func cfgJSON(c wire.Config) map[string]any {
	return map[string]any{"cfg_gen": c.CfgGen, "channel": c.Channel, "bw": c.BW.String(), "band": map[wire.Band]string{0: "2.4", 1: "5"}[c.Band],
		"country": c.Country.Code(), "country_mode": c.Country.Mode, "schan": c.Country.SChan, "nchan": c.Country.NChan,
		"snaplen": c.Snaplen, "type_mask": fmt.Sprintf("0x%08x", c.TypeMask), "ctrl_mask": fmt.Sprintf("0x%08x", c.CtrlMask),
		"capture_on": c.CaptureOn, "n_rules": c.NRules, "rules_crc32": fmt.Sprintf("%08X", c.RulesCRC32)}
}

func infoJSON(i wire.DeviceInfo) map[string]any {
	v := fwver.FromWire(i.FwVersion)
	var flags []string
	if i.FwFlags&wire.FwFlagDirty != 0 {
		flags = append(flags, "dirty")
	}
	if i.FwFlags&wire.FwFlagIDFMismatch != 0 {
		flags = append(flags, "idf_mismatch")
	}
	if i.FwFlags&wire.FwFlagDev != 0 {
		flags = append(flags, "dev")
	}
	return map[string]any{"fw_version": v.String(), "fw_flags": flags, "build_id": hex.EncodeToString(i.FwBuildID[:]), "proto_ver": i.ProtoVer,
		"mac": net.HardwareAddr(i.MAC[:]).String(), "caps": i.Caps.Names(), "max_frame": i.MaxFrame, "max_rules": i.MaxRules,
		"device_time_us": i.DeviceTimeUS, "reset_reason": i.ResetReason.String(), "safe_mode": i.StateFlags&wire.StateSafeMode != 0}
}

func cmdList(ctx context.Context, e *env) (any, error) {
	ps, err := serialport.List()
	if err != nil {
		return nil, err
	}
	type p struct {
		serialport.Info
		IsDongle bool `json:"is_dongle"`
	}
	out := []p{}
	for _, x := range ps {
		out = append(out, p{x, x.IsDongle()})
	}
	return map[string]any{"ports": out}, nil
}

func cmdDoctor(ctx context.Context, e *env) (any, error) {
	type check struct {
		Name   string `json:"name"`
		OK     bool   `json:"ok"`
		Detail string `json:"detail,omitempty"`
		Hint   string `json:"hint,omitempty"`
	}
	var cs []check
	if e.g.port != "sim" {
		ds, err := serialport.FindDongles()
		cs = append(cs, check{"dongle present", err == nil && len(ds) > 0, fmt.Sprintf("%d found", len(ds)), "plug in the dongle; check `ctpbench list`"})
	}
	rw, err := e.openRaw(ctx)
	if err != nil {
		be := classify(err)
		var b *benchErr
		if errors.As(err, &b) {
			be = b
		}
		cs = append(cs, check{"open port", false, err.Error(), be.Hint})
	} else {
		cs = append(cs, check{"open port", true, e.g.port, ""})
		c, err := ctp.New(ctx, rw, ctp.Options{PingInterval: -1})
		if err != nil {
			cs = append(cs, check{"HELLO", false, err.Error(), "firmware not running CTP? try `ctpbench fw flash-bare`"})
		} else {
			cs = append(cs, check{"HELLO", true, fwver.FromWire(c.Info().FwVersion).String(), ""})
			c.Abandon()
		}
	}
	ok := true
	for _, c := range cs {
		ok = ok && c.OK
	}
	res := map[string]any{"checks": cs, "ok": ok}
	if !ok {
		return nil, testFailure{res}
	}
	return res, nil
}

func cmdInfo(ctx context.Context, e *env) (any, error) {
	return withClient(ctx, e, func(c *ctp.Client) (any, error) {
		res := map[string]any{"device": infoJSON(c.Info()), "config": cfgJSON(c.LastConfig()), "host": hostInfo()}
		if c.Unsupported() {
			res["unsupported_fw"] = true
		}
		return res, nil
	})
}

func pct(ds []time.Duration, p float64) float64 {
	if len(ds) == 0 {
		return 0
	}
	i := int(float64(len(ds)-1) * p)
	return float64(ds[i].Microseconds()) / 1000
}

func cmdPing(ctx context.Context, e *env, n int, iv time.Duration) (any, error) {
	return withClient(ctx, e, func(c *ctp.Client) (any, error) {
		var rtts []time.Duration
		lost := 0
		for i := 0; i < n && ctx.Err() == nil; i++ {
			d, err := c.Ping(ctx)
			if err != nil {
				lost++
			} else {
				rtts = append(rtts, d)
			}
			time.Sleep(iv)
		}
		sort.Slice(rtts, func(i, j int) bool { return rtts[i] < rtts[j] })
		return map[string]any{"rtt_ms": map[string]float64{"min": pct(rtts, 0), "median": pct(rtts, 0.5), "p95": pct(rtts, 0.95), "p99": pct(rtts, 0.99), "max": pct(rtts, 1)},
			"count": n, "lost": lost}, nil
	})
}

func cmdReboot(ctx context.Context, e *env, download, yes bool) (any, error) {
	mode := wire.RebootNormal
	if download {
		mode = wire.RebootDownload
	}
	if !yes {
		return map[string]any{"plan": fmt.Sprintf("REBOOT mode %d", mode), "hint": "pass --yes to execute"}, nil
	}
	c, err := e.client(ctx, ctp.Options{PingInterval: -1})
	if err != nil {
		return nil, err
	}
	defer c.Abandon()
	if err := c.Reboot(ctx, mode); err != nil {
		return nil, err
	}
	if mode == wire.RebootDownload {
		return map[string]any{"rebooted": true, "mode": "download"}, nil
	}
	for {
		select {
		case ev, ok := <-c.Events():
			if !ok {
				return map[string]any{"rebooted": true, "note": "link dropped (re-enumeration); use wait-device"}, nil
			}
			if r, ok := ev.(ctp.EventDeviceReset); ok {
				return map[string]any{"rebooted": true, "reset_reason": r.Reason.String()}, nil
			}
		case <-ctx.Done():
			return nil, errf(exitTimeout, "TIMEOUT", "", "no EVT_RESET after reboot")
		}
	}
}

func cmdCaptureRun(ctx context.Context, e *env, dur time.Duration, count int, pktsOut bool, pcapPath string) (any, error) {
	if dur == 0 && count == 0 {
		return nil, errf(exitUsage, "USAGE", "capture run --duration 5s | --count N", "need --duration or --count")
	}
	c, err := e.client(ctx, ctp.Options{})
	if err != nil {
		return nil, err
	}
	defer c.Close()
	var pf *os.File
	if pktsOut {
		_ = os.MkdirAll(e.outDir(), 0o755)
		if pf, err = os.Create(filepath.Join(e.outDir(), "packets.jsonl")); err != nil {
			return nil, err
		}
		defer pf.Close()
	}
	var pw *pcapng.Writer
	var pcapFile *os.File
	if pcapPath != "" {
		if !filepath.IsAbs(pcapPath) && !strings.Contains(pcapPath, "/") {
			_ = os.MkdirAll(e.outDir(), 0o755)
			pcapPath = filepath.Join(e.outDir(), pcapPath)
		}
		if pcapFile, err = os.Create(pcapPath); err != nil {
			return nil, err
		}
		defer pcapFile.Close()
		v := fwver.FromWire(c.Info().FwVersion)
		if pw, err = pcapng.NewWriter(pcapFile, pcapng.WriterOptions{UserAppl: "capturetonkeli ctpbench", Hardware: "XIAO ESP32-C5 fw " + v.String(), IfName: e.g.port}); err != nil {
			return nil, err
		}
	}
	st0, err := c.Stats(ctx, false)
	if err != nil {
		return nil, err
	}
	cfg, err := c.Capture(ctx, true)
	if err != nil {
		return nil, err
	}
	lastGen := cfg.CfgGen
	var devLogs []string
	pendingComment := fmt.Sprintf("cfg_gen %d: ch %d bw %s, country %s, snaplen %d, filter crc %08X", cfg.CfgGen, cfg.Channel, cfg.BW, cfg.Country.Code(), cfg.Snaplen, cfg.RulesCRC32)
	var deadline <-chan time.Time
	if dur > 0 {
		deadline = time.After(dur)
	}
	hist := map[string]int{}
	bssid := map[string]int{}
	n, bytesIn := 0, 0
	t0 := time.Now()
loop:
	for count == 0 || n < count {
		select {
		case p, ok := <-c.Packets():
			if !ok {
				break loop
			}
			n++
			bytesIn += int(p.CapLen)
			hist[frameType(p.Data)]++
			if pw != nil {
				if p.CfgGen != lastGen {
					pendingComment = fmt.Sprintf("cfg_gen %d", p.CfgGen)
					lastGen = p.CfgGen
				}
				if err := pw.WritePacket(p.PktMeta, p.Data, pendingComment); err != nil {
					return nil, err
				}
				pendingComment = ""
			}
			if len(p.Data) >= 22 && p.Data[0]&0x0c == 0 {
				bssid[net.HardwareAddr(p.Data[16:22]).String()]++
			}
			if pf != nil {
				json.NewEncoder(pf).Encode(map[string]any{"ts_us": p.TsUS, "ch": p.Channel, "rssi": p.RSSI, "orig_len": p.OrigLen, "cap_len": p.CapLen,
					"flags": p.Flags, "rate": p.Rate, "bb_format": p.BBFormat, "noise": p.Noise, "rx_seq": p.RxSeq, "cfg_gen": p.CfgGen, "type": frameType(p.Data), "hex": hex.EncodeToString(p.Data)})
			}
		case ev := <-c.Events():
			if l, ok := ev.(ctp.EventLog); ok {
				devLogs = append(devLogs, l.Text)
			}
			if g, ok := ev.(ctp.EventGap); ok && pw != nil {
				where := "host (link)"
				if g.Where == ctp.GapDevice {
					where = "dongle"
				}
				pendingComment = fmt.Sprintf("gap: %d dropped on %s", g.N, where)
			}
		case <-deadline:
			break loop
		case <-ctx.Done():
			break loop
		}
	}
	el := time.Since(t0)
	cctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, _ = c.Capture(cctx, false)
	st1, _ := c.Stats(cctx, false)
	type kv struct {
		K string `json:"bssid"`
		V int    `json:"frames"`
	}
	var top []kv
	for k, v := range bssid {
		top = append(top, kv{k, v})
	}
	sort.Slice(top, func(i, j int) bool { return top[i].V > top[j].V })
	if len(top) > 10 {
		top = top[:10]
	}
	hc := c.Counters()
	res := map[string]any{"packets": n, "duration_ms": el.Milliseconds(), "pkt_per_s": float64(n) / el.Seconds(), "bytes": bytesIn,
		"device": map[string]uint32{"rx_total": st1.RxTotal - st0.RxTotal, "rx_passed": st1.RxPassed - st0.RxPassed, "drop_ring": st1.DropRing - st0.DropRing, "drop_fcs": st1.DropFCS - st0.DropFCS},
		"host":   hc, "types": hist, "top_bssids": top}
	if devLogs != nil {
		res["device_logs"] = devLogs
	}
	var arts []string
	if pf != nil {
		arts = append(arts, pf.Name())
	}
	if pcapFile != nil {
		arts = append(arts, pcapFile.Name())
	}
	if arts != nil {
		res["artifacts"] = arts
	}
	return res, nil
}

func frameType(d []byte) string { return dot11.Parse(d).Name }

func cmdMonitor(ctx context.Context, e *env, dur time.Duration) (any, error) {
	c, err := e.client(ctx, ctp.Options{})
	if err != nil {
		return nil, err
	}
	defer c.Abandon()
	enc := json.NewEncoder(os.Stdout)
	counts := map[string]int{}
	end := time.After(dur)
	for {
		select {
		case ev, ok := <-c.Events():
			if !ok {
				return map[string]any{"type": "summary", "events": counts, "link": "closed"}, nil
			}
			name := fmt.Sprintf("%T", ev)
			name = strings.TrimPrefix(name, "ctp.")
			counts[name]++
			if e.g.jsonl {
				_ = enc.Encode(map[string]any{"type": name, "t": time.Now().Format(time.RFC3339Nano), "event": ev})
			}
		case <-c.Packets():
			counts["Packet"]++
		case <-end:
			return map[string]any{"type": "summary", "events": counts, "host": c.Counters()}, nil
		case <-ctx.Done():
			return map[string]any{"type": "summary", "events": counts, "host": c.Counters()}, nil
		}
	}
}

func cmdRawSend(ctx context.Context, e *env, id uint16) (any, error) {
	if len(e.args) < 1 {
		return nil, errf(exitUsage, "USAGE", "ctpbench raw send TYPE [BODYHEX]", "need TYPE")
	}
	typ, err := strconv.ParseUint(e.args[0], 0, 8)
	if err != nil {
		return nil, errf(exitUsage, "USAGE", "", "bad type: %v", err)
	}
	var body []byte
	if len(e.args) > 1 {
		if body, err = hex.DecodeString(strings.Join(e.args[1:], "")); err != nil {
			return nil, errf(exitUsage, "USAGE", "", "bad hex: %v", err)
		}
	}
	rw, err := e.openRaw(ctx)
	if err != nil {
		return nil, err
	}
	defer rw.Close()
	var mu sync.Mutex
	var frames []map[string]any
	dec := codec.NewDecoder(codec.MaxFrame)
	go func() {
		buf := make([]byte, 8192)
		for {
			n, err := rw.Read(buf)
			dec.Feed(buf[:n], func(h wire.Header, b []byte) {
				if h.Type == wire.TypePkt {
					return
				}
				mu.Lock()
				frames = append(frames, map[string]any{"type": wire.TypeName(h.Type), "seq": h.Seq, "req_id": h.ReqID, "cfg_gen": h.CfgGen, "flags": h.Flags, "body_hex": hex.EncodeToString(b)})
				mu.Unlock()
			})
			if err != nil {
				return
			}
		}
	}()
	if _, err := rw.Write(codec.EncodeFrame([]byte{0}, wire.Header{Ver: 1, Type: uint8(typ), ReqID: id}, body)); err != nil {
		return nil, err
	}
	time.Sleep(300 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	return map[string]any{"sent": map[string]any{"type": wire.TypeName(uint8(typ)), "req_id": id, "body_hex": hex.EncodeToString(body)}, "received": frames}, nil
}

// ---- conformance ---------------------------------------------------------------

type junitSuite struct {
	XMLName  xml.Name    `xml:"testsuite"`
	Name     string      `xml:"name,attr"`
	Tests    int         `xml:"tests,attr"`
	Failures int         `xml:"failures,attr"`
	Skipped  int         `xml:"skipped,attr"`
	Cases    []junitCase `xml:"testcase"`
}
type junitCase struct {
	Name    string    `xml:"name,attr"`
	Class   string    `xml:"classname,attr"`
	Time    float64   `xml:"time,attr"`
	Failure *junitMsg `xml:"failure,omitempty"`
	Skipped *junitMsg `xml:"skipped,omitempty"`
}
type junitMsg struct {
	Message string `xml:"message,attr"`
}

func cmdConformance(ctx context.Context, e *env, runRe, skipRe string, list bool, tcc string, tch uint8) (any, error) {
	if list {
		type item struct{ ID, Title, Spec string }
		var out []item
		for _, c := range conformance.Cases() {
			out = append(out, item{c.ID, c.Title, c.Spec})
		}
		return map[string]any{"cases": out}, nil
	}
	var o conformance.Options
	var err error
	if runRe != "" {
		if o.Run, err = regexp.Compile(runRe); err != nil {
			return nil, errf(exitUsage, "USAGE", "", "--run: %v", err)
		}
	}
	if skipRe != "" {
		if o.Skip, err = regexp.Compile(skipRe); err != nil {
			return nil, errf(exitUsage, "USAGE", "", "--skip: %v", err)
		}
	}
	// version check once, before the suite
	if c, err := e.client(ctx, ctp.Options{PingInterval: -1}); err != nil {
		return nil, err
	} else {
		info := c.Info()
		c.Abandon()
		e.logf(1, "device fw %s", fwver.FromWire(info.FwVersion))
	}
	if e.g.timeout == 30*time.Second {
		// the default cap is too small for the suite; per-case timeouts still apply
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(context.WithoutCancel(ctx), 10*time.Minute)
		defer cancel()
	}
	dir := e.outDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	var traceMu sync.Mutex
	env := conformance.Env{
		Open:           e.openRaw,
		TrafficCountry: tcc,
		TrafficChannel: tch,
		Trace: func(id string) func(string, wire.Header, []byte) {
			f, err := os.OpenFile(filepath.Join(dir, id+".trace.jsonl"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
			if err != nil {
				return nil
			}
			enc := json.NewEncoder(f)
			return func(d string, h wire.Header, b []byte) {
				traceMu.Lock()
				defer traceMu.Unlock()
				if h.Type == wire.TypePkt && len(b) > 40 {
					b = b[:40]
				}
				_ = enc.Encode(map[string]any{"t": time.Now().Format("15:04:05.000000"), "dir": d, "type": wire.TypeName(h.Type), "seq": h.Seq, "req_id": h.ReqID, "flags": h.Flags, "cfg_gen": h.CfgGen, "body": hex.EncodeToString(b)})
			}
		},
	}
	o.OnResult = func(r conformance.Result) {
		e.logf(1, "%-16s %-4s %dms %s", r.ID, r.Status, r.DurationMS, r.Actual)
	}
	t0 := time.Now()
	res := conformance.Run(ctx, env, o)
	n := map[conformance.Status]int{}
	js := junitSuite{Name: "ctp-conformance"}
	for i := range res {
		r := &res[i]
		n[r.Status]++
		trace := filepath.Join(dir, r.ID+".trace.jsonl")
		if r.Status == conformance.Fail {
			r.Trace = trace
		} else {
			_ = os.Remove(trace)
		}
		jc := junitCase{Name: r.ID + " " + r.Title, Class: "ctp", Time: float64(r.DurationMS) / 1000}
		switch r.Status {
		case conformance.Fail:
			jc.Failure = &junitMsg{"expected " + r.Expected + ", actual " + r.Actual}
			js.Failures++
		case conformance.Skip:
			jc.Skipped = &junitMsg{r.Actual}
			js.Skipped++
		}
		js.Cases = append(js.Cases, jc)
	}
	js.Tests = len(res)
	repro := "ctpbench --port " + e.g.port + " conformance"
	var failed []string
	for _, r := range res {
		if r.Status == conformance.Fail {
			failed = append(failed, regexp.QuoteMeta(r.ID))
		}
	}
	if len(failed) > 0 {
		repro += " --run '^(" + strings.Join(failed, "|") + ")$'"
	}
	report := map[string]any{"summary": map[string]any{"passed": n[conformance.Pass], "failed": n[conformance.Fail], "skipped": n[conformance.Skip],
		"duration_ms": time.Since(t0).Milliseconds()}, "port": e.g.port, "seed": e.g.seed, "host": hostInfo(), "repro": repro, "results": res,
		"artifacts": []string{filepath.Join(dir, "report.json"), filepath.Join(dir, "junit.xml")}}
	b, _ := json.MarshalIndent(report, "", "  ")
	_ = os.WriteFile(filepath.Join(dir, "report.json"), b, 0o644)
	x, _ := xml.MarshalIndent(js, "", "  ")
	_ = os.WriteFile(filepath.Join(dir, "junit.xml"), append([]byte(xml.Header), x...), 0o644)
	if base := filepath.Dir(dir); base != "" {
		latest := filepath.Join(base, "latest")
		_ = os.Remove(latest)
		_ = os.Symlink(filepath.Base(dir), latest)
	}
	fmt.Fprintf(e.stderr, "conformance: %d passed, %d failed, %d skipped in %.1fs\n", n[conformance.Pass], n[conformance.Fail], n[conformance.Skip], time.Since(t0).Seconds())
	// stdout stays small: summary + failures only
	var fails []conformance.Result
	for _, r := range res {
		if r.Status == conformance.Fail {
			fails = append(fails, r)
		}
	}
	small := map[string]any{"summary": report["summary"], "failures": fails, "repro": repro, "report": filepath.Join(dir, "report.json")}
	if n[conformance.Fail] > 0 {
		return nil, testFailure{small}
	}
	return small, nil
}

// ---- sim serve ------------------------------------------------------------------

func cmdSimServe(ctx context.Context, e *env, pty bool, tcp, link string) (any, error) {
	if e.g.timeout == 30*time.Second {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(context.WithoutCancel(ctx), 24*time.Hour)
		defer cancel()
	}
	dev := e.sim()
	switch {
	case pty:
		m, path, hold, err := serialport.OpenPTY()
		if err != nil {
			return nil, err
		}
		defer m.Close()
		defer hold.Close()
		if link != "" {
			_ = os.Remove(link)
			if err := os.Symlink(path, link); err != nil {
				return nil, err
			}
			defer os.Remove(link)
		}
		fmt.Fprintf(e.stderr, "sim: serving on %s\n", path)
		fmt.Fprintln(os.Stdout, path)
		go func() { <-ctx.Done(); m.Close() }()
		err = dev.Serve(ctx, m)
		return map[string]any{"pty": path, "stopped": true, "err": fmt.Sprint(err)}, nil
	case tcp != "":
		ln, err := net.Listen("tcp", tcp)
		if err != nil {
			return nil, err
		}
		go func() { <-ctx.Done(); ln.Close() }()
		fmt.Fprintf(e.stderr, "sim: listening on %s\n", ln.Addr())
		for {
			cn, err := ln.Accept()
			if err != nil {
				return map[string]any{"tcp": tcp, "stopped": true}, nil
			}
			_ = dev.Serve(ctx, cn)
			cn.Close()
		}
	}
	return nil, errf(exitUsage, "USAGE", "sim serve --pty | --tcp ADDR", "need --pty or --tcp")
}

func cmdWaitDevice(ctx context.Context, e *env) (any, error) {
	t0 := time.Now()
	var last error
	for ctx.Err() == nil {
		rw, err := e.openRaw(ctx)
		if err == nil {
			cctx, cancel := context.WithTimeout(ctx, 2*time.Second)
			c, err2 := ctp.New(cctx, rw, ctp.Options{PingInterval: -1})
			cancel()
			if err2 == nil {
				info := c.Info()
				c.Abandon()
				return map[string]any{"ready": true, "port": e.g.port, "waited_ms": time.Since(t0).Milliseconds(), "device": infoJSON(info)}, nil
			}
			last = err2
		} else {
			last = err
		}
		time.Sleep(250 * time.Millisecond)
	}
	return nil, errf(exitTimeout, "TIMEOUT", "device did not answer HELLO; check `ctpbench doctor`", "%v", last)
}

var _ = io.EOF

func cmdVerifyPcap(ctx context.Context, e *env) (any, error) {
	if len(e.args) != 1 {
		return nil, errf(exitUsage, "USAGE", "ctpbench verify-pcap FILE", "need one file")
	}
	fh, err := os.Open(e.args[0])
	if err != nil {
		return nil, err
	}
	defer fh.Close()
	f, err := pcapng.Read(fh)
	if err != nil {
		return nil, testFailure{map[string]any{"ok": false, "error": err.Error()}}
	}
	var anomalies []string
	if f.LinkType != pcapng.LinkTypeRadiotap {
		anomalies = append(anomalies, fmt.Sprintf("linktype %d != 127", f.LinkType))
	}
	var lastTs uint64
	comments := 0
	hist := map[string]int{}
	for i, p := range f.Packets {
		if p.Radiotap == nil || len(p.Radiotap) < 8 || p.Radiotap[0] != 0 {
			anomalies = append(anomalies, fmt.Sprintf("packet %d: bad radiotap", i))
			continue
		}
		if p.OrigLen < p.CapLen {
			anomalies = append(anomalies, fmt.Sprintf("packet %d: orig_len < cap_len", i))
		}
		if p.TsUS < lastTs {
			anomalies = append(anomalies, fmt.Sprintf("packet %d: timestamp goes back", i))
		}
		lastTs = p.TsUS
		if p.Comment != "" {
			comments++
		}
		hist[dot11.Parse(p.Frame).Name]++
		if len(anomalies) > 20 {
			break
		}
	}
	res := map[string]any{"ok": len(anomalies) == 0, "packets": len(f.Packets), "comments": comments, "linktype": f.LinkType,
		"tsresol": f.TsResol, "userappl": f.UserAppl, "hardware": f.Hardware, "types": hist, "anomalies": anomalies}
	if len(anomalies) > 0 {
		return nil, testFailure{res}
	}
	return res, nil
}
