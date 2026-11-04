package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"sync/atomic"
	"time"

	"github.com/hjkoskel/capturetonkeli/ctp"
	"github.com/hjkoskel/capturetonkeli/ctp/codec"
	"github.com/hjkoskel/capturetonkeli/ctp/filter"
	"github.com/hjkoskel/capturetonkeli/ctp/wire"
)

// docs/06 §2 Testing: fuzz (malformed frames at a live device) and soak
// (long run with config changes and reconnects). Both fail (exit 1) on any
// anomaly and write their details to --out-dir.

func init() {
	var fzDur time.Duration
	register(command{name: "fuzz", summary: "malformed frames interleaved with PINGs; the device must stay alive and correct",
		example: "ctpbench fuzz --duration 30m --seed 42",
		flags:   func(fs *flag.FlagSet) { fs.DurationVar(&fzDur, "duration", time.Minute, "how long") },
		run:     func(ctx context.Context, e *env) (any, error) { return cmdFuzz(ctx, e, fzDur) }})
	var skDur, skEvery, skReopen time.Duration
	var skCountry string
	var skChannels string
	register(command{name: "soak", summary: "long run: capture + config changes + reconnects; detects resets, stalls, leaks, counter anomalies",
		example: "ctpbench soak --duration 24h --report-every 10m --country FI --channels 1,6,11,13,36",
		flags: func(fs *flag.FlagSet) {
			fs.DurationVar(&skDur, "duration", 10*time.Minute, "how long")
			fs.DurationVar(&skEvery, "report-every", time.Minute, "interval of report lines")
			fs.DurationVar(&skReopen, "reopen-every", 5*time.Minute, "close and reopen the port (host disappears) this often; 0 = never")
			fs.StringVar(&skCountry, "country", "FI", "country table to use")
			fs.StringVar(&skChannels, "channels", "1,6,11,13", "channels to cycle through")
		},
		run: func(ctx context.Context, e *env) (any, error) {
			return cmdSoak(ctx, e, skDur, skEvery, skReopen, skCountry, skChannels)
		}})
}

func extend(ctx context.Context, e *env, d time.Duration) (context.Context, context.CancelFunc) {
	if e.g.timeout < d {
		return context.WithTimeout(context.WithoutCancel(ctx), d)
	}
	return context.WithCancel(ctx)
}

// ---- fuzz -----------------------------------------------------------------------

func cmdFuzz(ctx context.Context, e *env, dur time.Duration) (any, error) {
	ctx, cancel := extend(ctx, e, dur+30*time.Second)
	defer cancel()
	c, err := e.client(ctx, ctp.Options{PingInterval: -1, Retries: 3})
	if err != nil {
		return nil, err
	}
	defer c.Close()
	var resetsA atomic.Int64
	var armed atomic.Bool
	go func() {
		for ev := range c.Events() {
			if _, ok := ev.(ctp.EventDeviceReset); ok && armed.Load() {
				resetsA.Add(1)
			}
		}
	}()
	time.Sleep(300 * time.Millisecond) // let a pending boot EVT_RESET pass
	armed.Store(true)
	go func() {
		for range c.Packets() {
		}
	}()
	st0, err := c.Stats(ctx, false)
	if err != nil {
		return nil, err
	}
	rng := rand.New(rand.NewSource(e.g.seed))
	var rtts []time.Duration
	kinds := map[string]int{}
	var failures []string
	sent := 0
	end := time.Now().Add(dur)
	reqTypes := []uint8{wire.TypeHello, wire.TypePing, wire.TypeGetConfig, wire.TypeSetChannel, wire.TypeSetCountry, wire.TypeSetFilter,
		wire.TypeSetSnaplen, wire.TypeSetDriverMask, wire.TypeCapture, wire.TypeGetStats}
	for time.Now().Before(end) && ctx.Err() == nil && len(failures) < 20 {
		for i := 0; i < 20; i++ {
			var raw []byte
			kind := rng.Intn(7)
			switch kind {
			case 0: // random bytes, no delimiter inside
				raw = make([]byte, 1+rng.Intn(300))
				rng.Read(raw)
				for k := range raw {
					if raw[k] == 0 {
						raw[k] = 0x5a
					}
				}
				raw = append(raw, 0)
			case 1: // valid frame, random type and body (never REBOOT)
				t := uint8(rng.Intn(256))
				if t == wire.TypeReboot {
					t = 0x31
				}
				body := make([]byte, rng.Intn(120))
				rng.Read(body)
				raw = codec.EncodeFrame([]byte{0}, wire.Header{Ver: 1, Type: t, ReqID: 0}, body)
			case 2: // known request, truncated / extended body
				body := make([]byte, rng.Intn(12))
				rng.Read(body)
				raw = codec.EncodeFrame([]byte{0}, wire.Header{Ver: 1, Type: reqTypes[rng.Intn(len(reqTypes))], ReqID: 0}, body)
			case 3: // random SET_FILTER
				body := make([]byte, 8+rng.Intn(300))
				rng.Read(body)
				body[0] = byte(rng.Intn(18))
				body[1] &= 0x1f
				if rng.Intn(2) == 0 {
					body[3] = 0
				}
				raw = codec.EncodeFrame([]byte{0}, wire.Header{Ver: 1, Type: wire.TypeSetFilter, ReqID: 0}, body)
			case 4: // bad CRC
				raw = codec.EncodeFrame([]byte{0}, wire.Header{Ver: 1, Type: wire.TypePing, ReqID: 0}, []byte{1, 2, 3, 4})
				raw[len(raw)-3] ^= 0x10
				if raw[len(raw)-3] == 0 {
					raw[len(raw)-3] = 1
				}
			case 5: // oversize
				raw = make([]byte, 5000+rng.Intn(3000))
				for k := range raw {
					raw[k] = byte(1 + rng.Intn(255))
				}
				raw = append(append([]byte{0}, raw...), 0)
			case 6: // wrong protocol version
				raw = codec.EncodeFrame([]byte{0}, wire.Header{Ver: uint8(2 + rng.Intn(250)), Type: wire.TypePing, ReqID: 0}, []byte{0, 0, 0, 0})
			}
			kinds[[]string{"garbage", "random-type", "truncated", "filter", "bad-crc", "oversize", "bad-version"}[kind]]++
			if err := c.WriteRaw(raw); err != nil {
				return nil, err
			}
			sent++
		}
		d, err := c.Ping(ctx)
		if err != nil {
			failures = append(failures, fmt.Sprintf("after %d fuzz frames: PING failed: %v", sent, err))
			continue
		}
		rtts = append(rtts, d)
	}
	// the device must still behave correctly
	st1, err := c.Stats(ctx, false)
	if err != nil {
		failures = append(failures, "GET_STATS after fuzz failed: "+err.Error())
	}
	cfg, err := c.SetSnaplen(ctx, 123)
	if err != nil || cfg.Snaplen != 123 {
		failures = append(failures, fmt.Sprintf("SET_SNAPLEN after fuzz: %v", err))
	}
	if _, err := c.Push(ctx, defaultDesired()); err != nil {
		failures = append(failures, "restoring the default config after fuzz failed: "+err.Error())
	}
	resets := resetsA.Load()
	if resets > 0 {
		failures = append(failures, fmt.Sprintf("device reset %d time(s) during fuzzing", resets))
	}
	sort.Slice(rtts, func(i, j int) bool { return rtts[i] < rtts[j] })
	res := map[string]any{"frames_sent": sent, "kinds": kinds, "pings": len(rtts), "rtt_ms": durStats(rtts), "device_resets": resets,
		"device_counted_note": "fuzzed GET_STATS frames may reset these counters", "device_counted": map[string]uint32{"host_rx_crc_err": st1.HostRxCRCErr - st0.HostRxCRCErr, "host_rx_oversize": st1.HostRxOversize - st0.HostRxOversize},
		"seed":           e.g.seed, "failures": failures, "ok": len(failures) == 0,
		"repro": fmt.Sprintf("ctpbench --port %s --seed %d fuzz --duration %s", e.g.port, e.g.seed, dur)}
	if len(failures) > 0 {
		return nil, testFailure{res}
	}
	return res, nil
}

// ---- soak -------------------------------------------------------------------------

type soakLine struct {
	T          string  `json:"t"`
	Elapsed    string  `json:"elapsed"`
	Packets    uint64  `json:"packets"`
	PktPerS    float64 `json:"pkt_per_s"`
	DevRxTotal uint32  `json:"dev_rx_total"`
	DropRing   uint32  `json:"dev_drop_ring"`
	HostLost   uint64  `json:"host_link_lost"`
	DevGaps    uint64  `json:"device_gaps"`
	RTTp99     float64 `json:"rtt_p99_ms"`
	Resets     int     `json:"device_resets"`
	Reopens    int     `json:"reopens"`
	CfgChanges int     `json:"config_changes"`
	UptimeS    float64 `json:"device_uptime_s"`
	Anomalies  int     `json:"anomalies"`
}

func cmdSoak(ctx context.Context, e *env, dur, every, reopen time.Duration, country, chans string) (any, error) {
	ctx, cancel := extend(ctx, e, dur+time.Minute)
	defer cancel()
	var chList []uint8
	for _, s := range splitComma(chans) {
		var v int
		if _, err := fmt.Sscan(s, &v); err != nil || v < 1 || v > 177 {
			return nil, errf(exitUsage, "USAGE", "--channels 1,6,11", "bad channel %q", s)
		}
		chList = append(chList, uint8(v))
	}
	dir := e.outDir()
	_ = os.MkdirAll(dir, 0o755)
	lf, err := os.Create(filepath.Join(dir, "soak.jsonl"))
	if err != nil {
		return nil, err
	}
	defer lf.Close()
	enc := json.NewEncoder(lf)
	rng := rand.New(rand.NewSource(e.g.seed))
	var anomalies []string
	anomaly := func(f string, a ...any) {
		s := time.Now().Format(time.RFC3339) + " " + fmt.Sprintf(f, a...)
		anomalies = append(anomalies, s)
		e.logf(0, "soak: ANOMALY %s", s)
	}
	desired := ptr(defaultDesired())
	desired.Country = ptr(wire.MakeCountry(country, 0, 0, 0))
	desired.Channel = &chList[0]
	desired.Capture = ptr(true)
	var (
		c                        *ctp.Client
		evCh                     <-chan ctp.Event
		pktsA                    atomic.Uint64
		resets, reopens, changes int
		lastUptime               float64
		rtts                     []time.Duration
		lastStats                *wire.Stats
	)
	open := func() error {
		var err error
		for i := 0; i < 20; i++ {
			c, err = e.client(ctx, ctp.Options{Desired: desired, PacketQueue: 1 << 18})
			if err == nil {
				break
			}
			time.Sleep(time.Second)
			_ = e.refind(ctx, e.g.port, 3*time.Second)
		}
		if err != nil {
			return err
		}
		go func(cl *ctp.Client) {
			for range cl.Packets() {
				pktsA.Add(1)
			}
		}(c)
		evCh = c.Events()
		return nil
	}
	if err := open(); err != nil {
		return nil, err
	}
	t0 := time.Now()
	report := time.NewTicker(every)
	change := time.NewTicker(20 * time.Second)
	ping := time.NewTicker(time.Second)
	defer report.Stop()
	defer change.Stop()
	defer ping.Stop()
	nextReopen := t0.Add(reopen)
	lastPk, lastT := uint64(0), t0
	end := t0.Add(dur)
	for time.Now().Before(end) && ctx.Err() == nil {
		select {
		case <-ctx.Done():
		case ev, ok := <-evCh:
			if !ok {
				evCh = nil
				break
			}
			switch v := ev.(type) {
			case ctp.EventDeviceReset:
				resets++
				anomaly("device reset: %s %s", v.Reason, v.Detail)
			case ctp.EventStats:
				up := float64(v.DeviceTimeUS) / 1e6
				if up+1 < lastUptime {
					anomaly("device uptime went backwards %.0f → %.0f s (unreported reboot?)", lastUptime, up)
				}
				lastUptime = up
				if lastStats != nil && v.RxTotal < lastStats.RxTotal {
					anomaly("rx_total went backwards %d → %d", lastStats.RxTotal, v.RxTotal)
				}
				if v.FlashWrites != 0 {
					anomaly("flash_writes = %d (must stay 0)", v.FlashWrites)
				}
				st := v.Stats
				lastStats = &st
			case ctp.EventLink:
				if v.State == ctp.LinkDown {
					anomaly("link down")
				}
			}
		case <-ping.C:
			if d, err := c.Ping(ctx); err == nil {
				rtts = append(rtts, d)
				if len(rtts) > 600 {
					rtts = rtts[1:]
				}
			}
		case <-change.C:
			// random config change: channel / snaplen / filter
			var err error
			switch rng.Intn(3) {
			case 0:
				ch := chList[rng.Intn(len(chList))]
				desired.Channel = &ch
				_, err = c.SetChannel(ctx, ch, wire.BW20)
			case 1:
				n := []uint16{0, 32, 64, 256}[rng.Intn(4)]
				desired.Snaplen = &n
				_, err = c.SetSnaplen(ctx, n)
			case 2:
				sets := []filter.Set{{}, {Rules: []filter.Rule{filter.Beacons()}}, {Rules: []filter.Rule{filter.DropBeacons()}}, {Rules: filter.EAPOL()}}
				f := sets[rng.Intn(len(sets))]
				desired.Filter = &f
				_, err = c.SetFilter(ctx, f)
			}
			changes++
			if err != nil {
				anomaly("config change failed: %v", err)
			}
		case now := <-report.C:
			sorted := append([]time.Duration(nil), rtts...)
			sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
			hc := c.Counters()
			pkts := pktsA.Load()
			l := soakLine{T: now.Format(time.RFC3339), Elapsed: now.Sub(t0).Round(time.Second).String(), Packets: pkts,
				PktPerS: float64(pkts-lastPk) / now.Sub(lastT).Seconds(), HostLost: hc.LinkLost, DevGaps: hc.DeviceGaps,
				RTTp99: pct(sorted, 0.99), Resets: resets, Reopens: reopens, CfgChanges: changes, UptimeS: lastUptime, Anomalies: len(anomalies)}
			if lastStats != nil {
				l.DevRxTotal, l.DropRing = lastStats.RxTotal, lastStats.DropRing
			}
			lastPk, lastT = pkts, now
			_ = enc.Encode(l)
			e.logf(1, "soak %s: %d pkts (%.0f/s), rtt p99 %.1f ms, resets %d, reopens %d, anomalies %d", l.Elapsed, l.Packets, l.PktPerS, l.RTTp99, resets, reopens, len(anomalies))
			if l.RTTp99 > 50 {
				anomaly("PING RTT p99 %.1f ms > 50 ms", l.RTTp99)
			}
		}
		if reopen > 0 && time.Now().After(nextReopen) {
			// host disappears without CAPTURE 0, then comes back (docs/06 CTP-ROB-002 at scale)
			_ = c.Abandon()
			time.Sleep(time.Duration(500+rng.Intn(2500)) * time.Millisecond)
			if err := open(); err != nil {
				anomaly("reopen failed: %v", err)
				break
			}
			reopens++
			nextReopen = time.Now().Add(reopen)
		}
	}
	_ = c.Close()
	res := map[string]any{"duration": time.Since(t0).Round(time.Second).String(), "packets": pktsA.Load(), "config_changes": changes, "reopens": reopens,
		"device_resets": resets, "anomalies": anomalies, "ok": len(anomalies) == 0, "report": lf.Name(), "seed": e.g.seed}
	if len(anomalies) > 0 {
		return nil, testFailure{res}
	}
	return res, nil
}

func ptr[T any](v T) *T { return &v }

func splitComma(s string) []string {
	var out []string
	cur := ""
	for _, r := range s {
		if r == ',' {
			out = append(out, cur)
			cur = ""
			continue
		}
		cur += string(r)
	}
	return append(out, cur)
}

// defaultDesired is the complete boot-equivalent configuration (every field
// set, so nothing a previous session changed survives).
func defaultDesired() ctp.DesiredConfig {
	return ctp.DesiredConfig{Country: ptr(wire.MakeCountry("01", 0, 0, 0)), Channel: ptr(uint8(1)), BW: ptr(wire.BW20), Snaplen: ptr(uint16(0)),
		TypeMask: ptr(wire.DefaultTypeMask), CtrlMask: ptr(wire.DefaultCtrlMask), Filter: &filter.Set{}, Capture: ptr(false)}
}
