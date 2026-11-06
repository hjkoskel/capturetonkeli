package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/hjkoskel/capturetonkeli/ctp"
	"github.com/hjkoskel/capturetonkeli/ctp/regdom"
	"github.com/hjkoskel/capturetonkeli/ctp/wire"
)

func init() {
	var tpSnap, tpSave, tpCompare, tpTol string
	var tpDur time.Duration
	register(command{name: "bench throughput", summary: "sustained PKT/s and B/s per snaplen; latency packet→host",
		example: "ctpbench bench throughput --snaplen 32,256,0 --duration 5s --save-baseline usb",
		flags: func(fs *flag.FlagSet) {
			fs.StringVar(&tpSnap, "snaplen", "32,128,512,0", "comma-separated snaplens (0 = full)")
			fs.DurationVar(&tpDur, "duration", 5*time.Second, "per snaplen")
			fs.StringVar(&tpSave, "save-baseline", "", "save result as bench-out/baseline/NAME.json")
			fs.StringVar(&tpCompare, "compare-baseline", "", "compare with bench-out/baseline/NAME.json")
			fs.StringVar(&tpTol, "tolerance", "10%", "allowed regression vs baseline")
		},
		run: func(ctx context.Context, e *env) (any, error) {
			return cmdThroughput(ctx, e, tpSnap, tpDur, tpSave, tpCompare, tpTol)
		}})
	var rtDur time.Duration
	register(command{name: "bench rtt-under-load", summary: "PING RTT distribution while capturing (doc 03 test 3)",
		flags: func(fs *flag.FlagSet) { fs.DurationVar(&rtDur, "duration", 10*time.Second, "how long") },
		run:   func(ctx context.Context, e *env) (any, error) { return cmdRTTLoad(ctx, e, rtDur) }})
	var csList string
	var csRepeat int
	register(command{name: "bench channel-switch", summary: "SET_CHANNEL latency, within-band vs cross-band",
		flags: func(fs *flag.FlagSet) {
			fs.StringVar(&csList, "channels", "1,6,11,36,44,149", "channel cycle")
			fs.IntVar(&csRepeat, "repeat", 5, "cycles")
		},
		run: func(ctx context.Context, e *env) (any, error) { return cmdChannelSwitch(ctx, e, csList, csRepeat) }})
	var cmCountries, cmOut string
	var cmManual bool
	register(command{name: "bench channel-map", summary: "accepted/rejected channels per country (answers doc 02 §7.1)",
		example: "ctpbench bench channel-map --country 01,FI,US,JP --manual --out docs/measurements/channel-map-v6.0.2.json",
		flags: func(fs *flag.FlagSet) {
			fs.StringVar(&cmCountries, "country", "01,FI,US,JP", "comma-separated country codes")
			fs.BoolVar(&cmManual, "manual", false, "also try manual range 1..14 per country")
			fs.StringVar(&cmOut, "out", "", "also write the map to this file")
		},
		run: func(ctx context.Context, e *env) (any, error) {
			return cmdChannelMap(ctx, e, cmCountries, cmManual, cmOut)
		}})
}

func durStats(ds []time.Duration) map[string]float64 {
	sort.Slice(ds, func(i, j int) bool { return ds[i] < ds[j] })
	return map[string]float64{"min": pct(ds, 0), "median": pct(ds, 0.5), "p95": pct(ds, 0.95), "p99": pct(ds, 0.99), "max": pct(ds, 1)}
}

// clockOffset estimates host_time - device_time (µs) from the fastest of n PINGs.
func clockOffset(ctx context.Context, c *ctp.Client, n int) (int64, error) {
	best := time.Duration(math.MaxInt64)
	var off int64
	for i := 0; i < n; i++ {
		t0 := time.Now()
		_, b, err := c.Do(ctx, wire.TypePing, wire.Ping{}.AppendBinary(nil))
		rtt := time.Since(t0)
		if err != nil {
			return 0, err
		}
		var p wire.Pong
		if p.UnmarshalBinary(b) != nil {
			continue
		}
		if rtt < best {
			best = rtt
			mid := t0.Add(rtt / 2)
			off = mid.UnixMicro() - int64(p.DeviceTimeUS)
		}
	}
	return off, nil
}

type tpRow struct {
	Snaplen     uint16             `json:"snaplen"`
	PktPerS     float64            `json:"pkt_per_s"`
	PayloadBps  float64            `json:"payload_Bps"`
	WireBps     float64            `json:"wire_Bps"`
	RxTotal     uint32             `json:"rx_total"`
	DropRing    uint32             `json:"drop_ring"`
	DropPct     float64            `json:"drop_ring_pct"`
	HostLost    uint64             `json:"host_lost"`
	HighWater   uint16             `json:"ring_high_water_max"`
	LatencyMS   map[string]float64 `json:"latency_ms"`
	Packets     int                `json:"packets"`
	DurationSec float64            `json:"duration_s"`
}

func cmdThroughput(ctx context.Context, e *env, snaps string, dur time.Duration, save, compare, tol string) (any, error) {
	var list []uint16
	for _, s := range strings.Split(snaps, ",") {
		v, err := strconv.ParseUint(strings.TrimSpace(s), 10, 16)
		if err != nil {
			return nil, errf(exitUsage, "USAGE", "--snaplen 32,256,0", "bad snaplen %q", s)
		}
		list = append(list, uint16(v))
	}
	need := time.Duration(len(list))*(dur+3*time.Second) + 10*time.Second
	if e.g.timeout < need {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(context.WithoutCancel(ctx), need)
		defer cancel()
	}
	c, err := e.client(ctx, ctp.Options{PacketQueue: 1 << 18})
	if err != nil {
		return nil, err
	}
	defer c.Close()
	var rows []tpRow
	for _, sl := range list {
		if _, err := c.SetSnaplen(ctx, sl); err != nil {
			return nil, err
		}
		off, err := clockOffset(ctx, c, 10)
		if err != nil {
			return nil, err
		}
		if _, err := c.Stats(ctx, true); err != nil {
			return nil, err
		}
		lost0 := c.Counters().LinkLost
		if _, err := c.Capture(ctx, true); err != nil {
			return nil, err
		}
		end := time.After(dur)
		t0 := time.Now()
		n, payload, hw := 0, 0, uint16(0)
		var lat []time.Duration
	loop:
		for {
			select {
			case p, ok := <-c.Packets():
				if !ok {
					return nil, errf(exitTimeout, "LINK_DEAD", "", "link closed during bench")
				}
				n++
				payload += int(p.CapLen)
				if n%16 == 0 {
					lat = append(lat, time.Duration(p.HostRx.UnixMicro()-off-int64(p.TsUS))*time.Microsecond)
				}
			case ev := <-c.Events():
				if s, ok := ev.(ctp.EventStats); ok && s.RingHighWater > hw {
					hw = s.RingHighWater
				}
			case <-end:
				break loop
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		el := time.Since(t0).Seconds()
		if _, err := c.Capture(ctx, false); err != nil {
			return nil, err
		}
		st, err := c.Stats(ctx, false)
		if err != nil {
			return nil, err
		}
		if st.RingHighWater > hw {
			hw = st.RingHighWater
		}
		drain := time.After(300 * time.Millisecond) // packets still in flight
	drainLoop:
		for {
			select {
			case p := <-c.Packets():
				if p != nil {
					n++
					payload += int(p.CapLen)
				}
			case <-drain:
				break drainLoop
			}
		}
		r := tpRow{Snaplen: sl, PktPerS: float64(n) / el, PayloadBps: float64(payload) / el, WireBps: float64(st.TxBytes) / el,
			RxTotal: st.RxTotal, DropRing: st.DropRing, HostLost: c.Counters().LinkLost - lost0, HighWater: hw,
			LatencyMS: durStats(lat), Packets: n, DurationSec: el}
		if st.RxTotal > 0 {
			r.DropPct = 100 * float64(st.DropRing) / float64(st.RxTotal)
		}
		e.logf(1, "snaplen %5d: %8.0f pkt/s %9.0f B/s wire, drop %.1f%%, latency p50 %.1f ms", sl, r.PktPerS, r.WireBps, r.DropPct, r.LatencyMS["median"])
		rows = append(rows, r)
	}
	res := map[string]any{"rows": rows, "device": infoJSON(c.Info()), "host": hostInfo()}
	if save != "" {
		p := filepath.Join("bench-out", "baseline", save+".json")
		_ = os.MkdirAll(filepath.Dir(p), 0o755)
		b, _ := json.MarshalIndent(res, "", "  ")
		if err := os.WriteFile(p, b, 0o644); err != nil {
			return nil, err
		}
		res["saved"] = p
	}
	if compare != "" {
		cmp, regressed, err := compareBaseline(compare, tol, rows)
		if err != nil {
			return nil, err
		}
		res["compare"] = cmp
		if regressed {
			return nil, testFailure{res}
		}
	}
	return res, nil
}

func compareBaseline(name, tol string, rows []tpRow) (any, bool, error) {
	b, err := os.ReadFile(filepath.Join("bench-out", "baseline", name+".json"))
	if err != nil {
		return nil, false, errf(exitUsage, "NO_BASELINE", "record one with --save-baseline "+name, "%v", err)
	}
	var base struct{ Rows []tpRow }
	if err := json.Unmarshal(b, &base); err != nil {
		return nil, false, err
	}
	t, err := strconv.ParseFloat(strings.TrimSuffix(tol, "%"), 64)
	if err != nil {
		return nil, false, errf(exitUsage, "USAGE", "--tolerance 10%", "bad tolerance")
	}
	type diff struct {
		Snaplen  uint16  `json:"snaplen"`
		Metric   string  `json:"metric"`
		Baseline float64 `json:"baseline"`
		Now      float64 `json:"now"`
		ChangePc float64 `json:"change_pct"`
		Regress  bool    `json:"regression"`
	}
	var out []diff
	bad := false
	for _, r := range rows {
		for _, br := range base.Rows {
			if br.Snaplen != r.Snaplen {
				continue
			}
			for _, m := range []struct {
				n    string
				b, v float64
			}{{"pkt_per_s", br.PktPerS, r.PktPerS}, {"wire_Bps", br.WireBps, r.WireBps}} {
				ch := 100 * (m.v - m.b) / math.Max(m.b, 1e-9)
				d := diff{r.Snaplen, m.n, m.b, m.v, ch, ch < -t}
				bad = bad || d.Regress
				out = append(out, d)
			}
		}
	}
	return out, bad, nil
}

func cmdRTTLoad(ctx context.Context, e *env, dur time.Duration) (any, error) {
	if e.g.timeout < dur+10*time.Second {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(context.WithoutCancel(ctx), dur+10*time.Second)
		defer cancel()
	}
	c, err := e.client(ctx, ctp.Options{PingInterval: -1, PacketQueue: 1 << 18})
	if err != nil {
		return nil, err
	}
	defer c.Close()
	if _, err := c.Capture(ctx, true); err != nil {
		return nil, err
	}
	go func() {
		for range c.Packets() {
		}
	}()
	var rtts []time.Duration
	lost := 0
	end := time.Now().Add(dur)
	for time.Now().Before(end) && ctx.Err() == nil {
		d, err := c.Ping(ctx)
		if err != nil {
			lost++
		} else {
			rtts = append(rtts, d)
		}
		time.Sleep(10 * time.Millisecond)
	}
	st, _ := c.Stats(ctx, false)
	return map[string]any{"rtt_ms": durStats(rtts), "pings": len(rtts) + lost, "lost": lost,
		"device": map[string]uint32{"rx_total": st.RxTotal, "tx_frames": st.TxFrames, "drop_ring": st.DropRing}}, nil
}

func cmdChannelSwitch(ctx context.Context, e *env, list string, repeat int) (any, error) {
	var chans []uint8
	for _, s := range strings.Split(list, ",") {
		v, err := strconv.ParseUint(strings.TrimSpace(s), 10, 8)
		if err != nil {
			return nil, errf(exitUsage, "USAGE", "--channels 1,6,36", "bad channel %q", s)
		}
		chans = append(chans, uint8(v))
	}
	c, err := e.client(ctx, ctp.Options{PingInterval: -1})
	if err != nil {
		return nil, err
	}
	defer c.Abandon()
	if _, err := c.SetCountry(ctx, wire.MakeCountry("FI", 0, 0, 0)); err != nil {
		return nil, err
	}
	var within, cross []time.Duration
	var errs []string
	prev := c.LastConfig().Channel
	for r := 0; r < repeat; r++ {
		for _, ch := range chans {
			t0 := time.Now()
			_, err := c.SetChannel(ctx, ch, wire.BW20)
			d := time.Since(t0)
			if err != nil {
				errs = append(errs, fmt.Sprintf("ch %d: %v", ch, err))
				continue
			}
			if (prev > 14) == (ch > 14) {
				within = append(within, d)
			} else {
				cross = append(cross, d)
			}
			prev = ch
		}
	}
	return map[string]any{"within_band_ms": durStats(within), "cross_band_ms": durStats(cross), "errors": errs,
		"note": "round-trip time of SET_CHANNEL incl. USB (~1 ms)"}, nil
}

func cmdChannelMap(ctx context.Context, e *env, countries string, manual bool, out string) (any, error) {
	var chans []uint8
	for ch := uint8(1); ch <= 14; ch++ {
		chans = append(chans, ch)
	}
	for ch := 32; ch <= 144; ch += 2 { // UNII-1..2C: even numbers
		chans = append(chans, uint8(ch))
	}
	for ch := 149; ch <= 177; ch += 2 { // UNII-3/4: odd numbers
		chans = append(chans, uint8(ch))
	}
	if e.g.timeout == 30*time.Second {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(context.WithoutCancel(ctx), 5*time.Minute)
		defer cancel()
	}
	c, err := e.client(ctx, ctp.Options{})
	if err != nil {
		return nil, err
	}
	defer c.Abandon()
	type entry struct {
		Country  string           `json:"country"`
		Mode     string           `json:"mode"`
		SChan    uint8            `json:"schan"`
		NChan    uint8            `json:"nchan"`
		Accepted []int            `json:"accepted"`
		Rejected map[string][]int `json:"rejected"`
		Model    []int            `json:"model_accepted,omitempty"`
		Differs  bool             `json:"differs_from_model"`
	}
	var res []entry
	run := func(cc wire.Country, mode string) error {
		cfg, err := c.SetCountry(ctx, cc)
		if err != nil {
			var de *ctp.DeviceError
			if errors.As(err, &de) {
				res = append(res, entry{Country: cc.Code(), Mode: mode, Rejected: map[string][]int{"country: " + de.RspErr.Error(): nil}})
				return nil
			}
			return err
		}
		en := entry{Country: cc.Code(), Mode: mode, SChan: cfg.Country.SChan, NChan: cfg.Country.NChan, Rejected: map[string][]int{}}
		for _, ch := range chans {
			_, err := c.SetChannel(ctx, ch, wire.BW20)
			if err == nil {
				en.Accepted = append(en.Accepted, int(ch))
				continue
			}
			var de *ctp.DeviceError
			if !errors.As(err, &de) {
				return err
			}
			k := de.RspErr.Error()
			en.Rejected[k] = append(en.Rejected[k], int(ch))
		}
		if mode == "validated" {
			if r, ok := regdom.Countries[cc.Code()]; ok {
				for _, ch := range chans {
					if r.Allowed(ch) {
						en.Model = append(en.Model, int(ch))
					}
				}
				en.Differs = fmt.Sprint(en.Model) != fmt.Sprint(en.Accepted)
			}
		}
		res = append(res, en)
		e.logf(1, "%s %s: %d accepted", cc.Code(), mode, len(en.Accepted))
		return nil
	}
	for _, cc := range strings.Split(countries, ",") {
		cc = strings.ToUpper(strings.TrimSpace(cc))
		if err := run(wire.MakeCountry(cc, wire.CountryValidated, 0, 0), "validated"); err != nil {
			return nil, err
		}
		if manual {
			if err := run(wire.MakeCountry(cc, wire.CountryManual, 1, 14), "manual 1..14"); err != nil {
				return nil, err
			}
		}
	}
	_, _ = c.SetCountry(ctx, wire.MakeCountry("01", 0, 0, 0))
	info := c.Info()
	doc := map[string]any{"device": infoJSON(info), "date": time.Now().Format("2006-01-02"), "map": res}
	if out != "" {
		_ = os.MkdirAll(filepath.Dir(out), 0o755)
		b, _ := json.MarshalIndent(doc, "", "  ")
		if err := os.WriteFile(out, append(b, '\n'), 0o644); err != nil {
			return nil, err
		}
		doc["written"] = out
	}
	return doc, nil
}
