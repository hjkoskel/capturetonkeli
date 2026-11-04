// Package sim is the simulated dongle: the device side of CTP v1 over any
// io.ReadWriter (docs/05 §8). Its behaviour is the reference behaviour that
// the firmware is tested against.
package sim

import (
	"context"
	"errors"
	"io"
	"math/rand"
	"net"
	"sync"
	"time"

	"github.com/hjkoskel/capturetonkeli/ctp/codec"
	"github.com/hjkoskel/capturetonkeli/ctp/filter"
	"github.com/hjkoskel/capturetonkeli/ctp/regdom"
	"github.com/hjkoskel/capturetonkeli/ctp/wire"
)

// Faults injects link problems (device → host direction).
type Faults struct {
	DropRate    float64       // probability to drop one encoded frame
	CorruptRate float64       // probability to flip a byte in one encoded frame
	BootGarbage bool          // emit ROM-like text before EVT_RESET (doc 08 B3)
	RebootAt    time.Duration // >0: simulated spontaneous reboot after this long
	StallAt     time.Duration // >0: stop writing for StallFor after this long
	StallFor    time.Duration
}

// SimOptions configures a Device.
type SimOptions struct {
	Caps           wire.Caps
	MaxRules       uint16
	FwVersion      uint32
	FwFlags        uint8
	FwBuildID      [8]byte
	MAC            [6]byte
	Traffic        TrafficModel
	USBBytesPerSec int // 0 = 1 MB/s
	RingBytes      int // 0 = 96 KiB
	Faults         Faults
	Seed           int64
	SafeMode       bool
	BootReason     wire.ResetReason // reason reported for the first boot (default POWER_ON)
}

// Defaults fills unset options.
func (o SimOptions) withDefaults() SimOptions {
	if o.Caps == 0 {
		o.Caps = wire.Cap5GHz | wire.CapHT40 | wire.CapManualCountry
	}
	if o.MaxRules == 0 {
		o.MaxRules = wire.MaxRules
	}
	if o.FwVersion == 0 {
		o.FwVersion = 0<<24 | 1<<16 | 0
	}
	if o.MAC == ([6]byte{}) {
		o.MAC = [6]byte{0x02, 0xc5, 0x00, 0x00, 0x00, 0x01}
	}
	if o.Traffic == (TrafficModel{}) {
		o.Traffic = DefaultTraffic
	}
	if o.USBBytesPerSec == 0 {
		o.USBBytesPerSec = 1_000_000
	}
	if o.RingBytes == 0 {
		o.RingBytes = 96 * 1024
	}
	if o.BootReason == 0 {
		o.BootReason = wire.ResetPowerOn
	}
	return o
}

const (
	watchdogTimeout = 2 * time.Second
	statsInterval   = time.Second
	dedupSize       = 4
	ringRecOverhead = 4
)

type outFrame struct {
	typ    uint8
	reqID  uint16
	cfgGen uint8
	body   []byte
}

type dedupEntry struct {
	valid bool
	reqID uint16
	typ   uint8
	crc   uint32
	rtyp  uint8
	rbody []byte
}

type counters struct {
	rxTotal, rxPassed, dropRing, dropFCS, txFrames, txBytes, hostCRC, hostOversize uint32
	highWater                                                                      uint16
}

// Device is a simulated dongle. Its state persists across Serve calls
// (like a real dongle across port reopen).
type Device struct {
	o     SimOptions
	start time.Time

	mu            sync.Mutex
	cond          *sync.Cond
	cfg           wire.Config
	region        regdom.Region
	flt           filter.Set
	session       bool
	lastRx        time.Time
	rxSeq         uint16
	cnt           counters
	noise         int8
	ctrl          []outFrame
	ring          []outFrame
	ringBytes     int
	dedup         [dedupSize]dedupEntry
	dedupNext     int
	pendingReboot bool
	bootReason    wire.ResetReason
	bootSent      bool
	gen           *generator
	lastGenAt     time.Time
	lastStats     time.Time
	seq           uint16
	lastGenTx     uint8
	firstTx       bool
	rng           *rand.Rand
	attached      bool
	epoch         int // increments on reboot
}

// NewDevice creates a device in boot state.
func NewDevice(o SimOptions) *Device {
	o = o.withDefaults()
	d := &Device{o: o, start: time.Now(), rng: rand.New(rand.NewSource(o.Seed ^ 0x5eed))}
	d.cond = sync.NewCond(&d.mu)
	d.gen = newGenerator(o.Seed, o.Traffic)
	d.bootLocked(o.BootReason)
	return d
}

func (d *Device) nowUS() uint64 { return uint64(time.Since(d.start) / time.Microsecond) }

func (d *Device) bootLocked(reason wire.ResetReason) {
	d.cfg = wire.BootConfig()
	d.region = regdom.Countries["01"]
	d.flt = filter.Set{Version: 1}
	d.session = false
	d.cnt = counters{}
	d.ctrl = nil
	d.ring = nil
	d.ringBytes = 0
	d.dedup = [dedupSize]dedupEntry{}
	d.pendingReboot = false
	d.bootReason = reason
	d.bootSent = false
	d.seq = 0
	d.firstTx = true
	d.noise = -95
	d.epoch++
	d.ctrl = append(d.ctrl, outFrame{typ: wire.TypeEvtReset, body: wire.Reset{Reason: reason, FwVersion: d.o.FwVersion, StateFlags: d.stateFlags()}.AppendBinary(nil)})
}

func (d *Device) stateFlags() uint8 {
	if d.o.SafeMode {
		return wire.StateSafeMode
	}
	return 0
}

// Reboot simulates a spontaneous reset with the given reason.
func (d *Device) Reboot(reason wire.ResetReason) {
	d.mu.Lock()
	d.bootLocked(reason)
	d.cond.Broadcast()
	d.mu.Unlock()
}

// Config returns the current configuration (for tests).
func (d *Device) Config() wire.Config {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.cfg
}

// Serve speaks CTP on rw until ctx is done or rw fails. Only one Serve may run at a time.
func (d *Device) Serve(ctx context.Context, rw io.ReadWriter) error {
	d.mu.Lock()
	if d.attached {
		d.mu.Unlock()
		return errors.New("sim: device already attached")
	}
	d.attached = true
	d.mu.Unlock()
	defer func() {
		d.mu.Lock()
		d.attached = false
		d.mu.Unlock()
	}()

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var wg sync.WaitGroup
	errc := make(chan error, 3)
	// The reader is not waited for: it unblocks when the caller closes rw.
	go func() { errc <- d.readLoop(ctx, rw) }()
	wg.Add(2)
	go func() { defer wg.Done(); errc <- d.writeLoop(ctx, rw) }()
	go func() { defer wg.Done(); d.timerLoop(ctx) }()
	err := <-errc
	cancel()
	d.mu.Lock()
	d.cond.Broadcast()
	d.mu.Unlock()
	wg.Wait()
	if errors.Is(err, context.Canceled) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrClosedPipe) {
		return nil
	}
	return err
}

func (d *Device) readLoop(ctx context.Context, r io.Reader) error {
	dec := codec.NewDecoder(codec.MaxFrame)
	var last codec.DecoderStats
	buf := make([]byte, 4096)
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		n, err := r.Read(buf)
		if n > 0 {
			d.mu.Lock()
			dec.Feed(buf[:n], func(h wire.Header, body []byte) {
				if h.Ver != wire.ProtoVer {
					d.cnt.hostCRC++
					return
				}
				d.lastRx = time.Now()
				d.handleLocked(h, body)
			})
			st := dec.Stats()
			d.cnt.hostCRC += uint32(st.CRCErrors-last.CRCErrors) + uint32(st.COBSErrors-last.COBSErrors) + uint32(st.Short-last.Short)
			d.cnt.hostOversize += uint32(st.Oversize - last.Oversize)
			last = st
			d.cond.Broadcast()
			d.mu.Unlock()
		}
		if err != nil {
			return err
		}
	}
}

// ---- request handling ------------------------------------------------------

func (d *Device) queueCtrl(typ uint8, reqID uint16, body []byte) {
	d.ctrl = append(d.ctrl, outFrame{typ: typ, reqID: reqID, cfgGen: d.cfg.CfgGen, body: body})
}

func rspErr(code wire.ErrCode, detail string) (uint8, []byte) {
	return wire.TypeRspErr, wire.RspErr{Code: code, Detail: detail}.AppendBinary(nil)
}

func (d *Device) handleLocked(h wire.Header, body []byte) {
	crc := codec.CRC32(body)
	for _, e := range d.dedup {
		if e.valid && e.reqID == h.ReqID && e.typ == h.Type && e.crc == crc {
			d.queueCtrl(e.rtyp, h.ReqID, e.rbody)
			return
		}
	}
	rtyp, rbody := d.execLocked(h.Type, body)
	d.dedup[d.dedupNext] = dedupEntry{valid: true, reqID: h.ReqID, typ: h.Type, crc: crc, rtyp: rtyp, rbody: rbody}
	d.dedupNext = (d.dedupNext + 1) % dedupSize
	d.queueCtrl(rtyp, h.ReqID, rbody)
}

func (d *Device) configRsp() (uint8, []byte) {
	return wire.TypeRspOK, d.cfg.AppendBinary(nil)
}

func (d *Device) execLocked(typ uint8, body []byte) (uint8, []byte) {
	if d.o.SafeMode {
		switch typ {
		case wire.TypeHello, wire.TypePing, wire.TypeGetConfig, wire.TypeGetStats, wire.TypeReboot:
		default:
			if typ < 0x80 && knownReq(typ) {
				return rspErr(wire.ErrSafeMode, "safe mode")
			}
		}
	}
	switch typ {
	case wire.TypeHello:
		var m wire.Hello
		if m.UnmarshalBinary(body) != nil {
			return rspErr(wire.ErrBadBody, "")
		}
		d.session = true
		info := wire.DeviceInfo{ProtoVer: wire.ProtoVer, FwVersion: d.o.FwVersion, FwFlags: d.o.FwFlags, FwBuildID: d.o.FwBuildID,
			MAC: d.o.MAC, Caps: d.o.Caps, MaxFrame: wire.MaxFrame, MaxRules: d.o.MaxRules, DeviceTimeUS: d.nowUS(),
			HostNonce: m.HostNonce, ResetReason: d.bootReason, StateFlags: d.stateFlags()}
		d.lastStats = time.Now()
		return wire.TypeRspOK, info.AppendBinary(nil)
	case wire.TypePing:
		var m wire.Ping
		if m.UnmarshalBinary(body) != nil {
			return rspErr(wire.ErrBadBody, "")
		}
		return wire.TypeRspOK, wire.Pong{HostTimeMS: m.HostTimeMS, DeviceTimeUS: d.nowUS()}.AppendBinary(nil)
	case wire.TypeGetConfig:
		return d.configRsp()
	case wire.TypeSetChannel:
		var m wire.SetChannel
		if m.UnmarshalBinary(body) != nil {
			return rspErr(wire.ErrBadBody, "")
		}
		if code, det := d.setChannelLocked(m.Channel, m.BW); code != 0 {
			return rspErr(code, det)
		}
		return d.configRsp()
	case wire.TypeSetCountry:
		var m wire.Country
		if m.UnmarshalBinary(body) != nil {
			return rspErr(wire.ErrBadBody, "")
		}
		if code, det := d.setCountryLocked(m); code != 0 {
			return rspErr(code, det)
		}
		return d.configRsp()
	case wire.TypeSetFilter:
		s, err := filter.Decode(body)
		if err != nil || len(s.Rules) > int(d.o.MaxRules) {
			det := "too many rules"
			if err != nil {
				det = err.Error()
			}
			return rspErr(wire.ErrFilterInvalid, trunc(det))
		}
		d.flt = s
		d.applyLocked(func(c *wire.Config) { c.NRules = uint8(len(s.Rules)); c.RulesCRC32 = codec.CRC32(body) })
		return d.configRsp()
	case wire.TypeSetSnaplen:
		var m wire.SetSnaplen
		if m.UnmarshalBinary(body) != nil {
			return rspErr(wire.ErrBadBody, "")
		}
		d.applyLocked(func(c *wire.Config) { c.Snaplen = m.Snaplen })
		return d.configRsp()
	case wire.TypeSetDriverMask:
		var m wire.SetDriverMask
		if m.UnmarshalBinary(body) != nil {
			return rspErr(wire.ErrBadBody, "")
		}
		d.applyLocked(func(c *wire.Config) { c.TypeMask, c.CtrlMask = m.TypeMask, m.CtrlMask })
		return d.configRsp()
	case wire.TypeCapture:
		var m wire.Capture
		if m.UnmarshalBinary(body) != nil {
			return rspErr(wire.ErrBadBody, "")
		}
		if m.On && !d.session {
			return rspErr(wire.ErrNoSession, "HELLO first")
		}
		if m.On && !d.cfg.CaptureOn {
			d.rxSeq = 0
			d.lastGenAt = time.Now()
		}
		d.applyLocked(func(c *wire.Config) { c.CaptureOn = m.On })
		return d.configRsp()
	case wire.TypeGetStats:
		var m wire.GetStats
		if m.UnmarshalBinary(body) != nil {
			return rspErr(wire.ErrBadBody, "")
		}
		b := d.statsLocked().AppendBinary(nil)
		if m.ResetAfterRead {
			hw := d.cnt.highWater
			d.cnt = counters{highWater: hw}
		}
		d.cnt.highWater = 0
		return wire.TypeRspOK, b
	case wire.TypeReboot:
		var m wire.Reboot
		if m.UnmarshalBinary(body) != nil {
			return rspErr(wire.ErrBadBody, "")
		}
		if m.Mode > wire.RebootDownload {
			return rspErr(wire.ErrInvalidArg, "mode")
		}
		d.pendingReboot = true
		return wire.TypeRspOK, nil
	}
	return rspErr(wire.ErrUnknownType, "")
}

func knownReq(t uint8) bool {
	switch t {
	case wire.TypeHello, wire.TypePing, wire.TypeGetConfig, wire.TypeSetChannel, wire.TypeSetCountry, wire.TypeSetFilter,
		wire.TypeSetSnaplen, wire.TypeSetDriverMask, wire.TypeCapture, wire.TypeGetStats, wire.TypeReboot:
		return true
	}
	return false
}

func trunc(s string) string {
	if len(s) > wire.MaxDetail {
		return s[:wire.MaxDetail]
	}
	return s
}

// applyLocked mutates the config and bumps cfg_gen only if something changed.
func (d *Device) applyLocked(f func(c *wire.Config)) {
	old := d.cfg
	f(&d.cfg)
	if !old.SameSettings(d.cfg) {
		d.cfg.CfgGen = old.CfgGen + 1
		if old.CaptureOn && !d.cfg.CaptureOn {
			d.flushRingLocked()
		}
	}
}

func (d *Device) flushRingLocked() { d.ring, d.ringBytes = nil, 0 }

// resolveChannel validates (ch,bw) under region r and returns the applied bw and band.
func (d *Device) resolveChannel(r regdom.Region, ch uint8, bw wire.Bandwidth) (wire.Bandwidth, wire.Band, wire.ErrCode, string) {
	if bw > wire.BW40Minus {
		return 0, 0, wire.ErrInvalidArg, "bw"
	}
	_, band, ok := wire.ChannelFreqMHz(ch)
	if !ok {
		return 0, 0, wire.ErrChannelNotAllowed, "no such channel"
	}
	if band == wire.Band5G && d.o.Caps&wire.Cap5GHz == 0 {
		return 0, 0, wire.ErrNotSupported, "5 GHz"
	}
	if !r.Allowed(ch) {
		return 0, 0, wire.ErrChannelNotAllowed, "not in country table"
	}
	if bw == wire.BW20 {
		return wire.BW20, band, 0, ""
	}
	if d.o.Caps&wire.CapHT40 == 0 {
		return 0, 0, wire.ErrNotSupported, "HT40"
	}
	p, plus, ok := regdom.HT40Partner(ch, bw == wire.BW40Plus)
	if !ok || !r.Allowed(p) {
		return 0, 0, wire.ErrChannelNotAllowed, "HT40 partner channel"
	}
	if plus {
		return wire.BW40Plus, band, 0, ""
	}
	return wire.BW40Minus, band, 0, ""
}

func (d *Device) setChannelLocked(ch uint8, bw wire.Bandwidth) (wire.ErrCode, string) {
	abw, band, code, det := d.resolveChannel(d.region, ch, bw)
	if code != 0 {
		return code, det
	}
	d.applyLocked(func(c *wire.Config) { c.Channel, c.BW, c.Band = ch, abw, band })
	return 0, ""
}

func (d *Device) setCountryLocked(m wire.Country) (wire.ErrCode, string) {
	cc := string(m.CC[:2])
	var r regdom.Region
	switch m.Mode {
	case wire.CountryValidated:
		var ok bool
		if r, ok = regdom.Countries[cc]; !ok {
			return wire.ErrCountryInvalid, "unknown country " + printable(cc)
		}
	case wire.CountryManual:
		if d.o.Caps&wire.CapManualCountry == 0 {
			return wire.ErrNotSupported, "manual country"
		}
		if !isPrintable(cc) {
			return wire.ErrCountryInvalid, "bad code"
		}
		var ok bool
		if r, ok = regdom.Manual(cc, m.SChan, m.NChan); !ok {
			return wire.ErrCountryInvalid, "bad manual range"
		}
	default:
		return wire.ErrInvalidArg, "mode"
	}
	d.region = r
	ch, bw := d.cfg.Channel, d.cfg.BW
	abw, band, code, _ := d.resolveChannel(r, ch, bw)
	if code != 0 {
		abw, code = wire.BW20, 0
		if !r.Allowed(ch) {
			ch, band = r.G24[0], wire.Band2G4
		} else {
			_, band, _ = wire.ChannelFreqMHz(ch)
		}
	}
	d.applyLocked(func(c *wire.Config) {
		c.Country = wire.Country{CC: [3]byte{m.CC[0], m.CC[1], 0}, Mode: m.Mode, SChan: r.G24[0], NChan: r.G24[1]}
		c.Channel, c.BW, c.Band = ch, abw, band
	})
	return 0, ""
}

func isPrintable(s string) bool {
	for _, c := range []byte(s) {
		if c < 0x21 || c > 0x7e {
			return false
		}
	}
	return true
}

func printable(s string) string {
	if isPrintable(s) {
		return s
	}
	return "?"
}

func (d *Device) statsLocked() wire.Stats {
	return wire.Stats{DeviceTimeUS: d.nowUS(), RxTotal: d.cnt.rxTotal, RxPassed: d.cnt.rxPassed, DropRing: d.cnt.dropRing,
		DropFCS: d.cnt.dropFCS, TxFrames: d.cnt.txFrames, TxBytes: d.cnt.txBytes, HostRxCRCErr: d.cnt.hostCRC,
		HostRxOversize: d.cnt.hostOversize, RingHighWater: d.cnt.highWater, Channel: d.cfg.Channel, NoiseFloor: d.noise}
}

// ---- capture path ------------------------------------------------------------

func driverPass(c wire.Config, f []byte) bool {
	if len(f) < 1 {
		return false
	}
	t := (f[0] >> 2) & 3
	sub := f[0] >> 4
	switch t {
	case 0:
		return c.TypeMask&1 != 0
	case 1:
		if c.TypeMask&2 == 0 {
			return false
		}
		if sub >= 7 {
			return c.CtrlMask&(1<<(16+uint32(sub))) != 0
		}
		return true
	case 2:
		return c.TypeMask&4 != 0
	}
	return c.TypeMask&8 != 0
}

func (d *Device) captureLocked(fr RxFrame, now time.Time) {
	if !driverPass(d.cfg, fr.Data) {
		return
	}
	d.cnt.rxTotal++
	d.noise = fr.Noise
	ol := len(fr.Data)
	var flags uint8
	if fr.FCSErr {
		flags |= wire.PktFCSErr
	}
	if d.cfg.Band == wire.Band5G {
		flags |= wire.Pkt5GHz
	}
	if d.cfg.BW != wire.BW20 {
		flags |= wire.PktHT40
	}
	meta := wire.PktMeta{TsUS: uint64(now.Sub(d.start) / time.Microsecond), Channel: d.cfg.Channel, Flags: flags,
		RSSI: fr.RSSI, Noise: fr.Noise, Rate: fr.Rate, BBFormat: fr.BBFormat, OrigLen: uint16(ol)}
	if !d.flt.Match(fr.Data, meta) {
		if fr.FCSErr && d.flt.Global.DropFCSErr {
			d.cnt.dropFCS++
		}
		return
	}
	d.cnt.rxPassed++
	capLen := ol
	if d.cfg.Snaplen != 0 && int(d.cfg.Snaplen) < capLen {
		capLen = int(d.cfg.Snaplen)
	}
	if capLen > wire.MaxCapLen {
		capLen = wire.MaxCapLen
	}
	if capLen < ol {
		meta.Flags |= wire.PktTruncated
	}
	meta.CapLen = uint16(capLen)
	meta.RxSeq = d.rxSeq
	d.rxSeq++
	rec := ringRecOverhead + wire.PktMetaLen + capLen
	if d.ringBytes+rec > d.o.RingBytes {
		d.cnt.dropRing++
		return
	}
	body := meta.AppendBinary(make([]byte, 0, wire.PktMetaLen+capLen))
	body = append(body, fr.Data[:capLen]...)
	d.ring = append(d.ring, outFrame{typ: wire.TypePkt, cfgGen: d.cfg.CfgGen, body: body})
	d.ringBytes += rec
	if hw := uint16(d.ringBytes * 100 / d.o.RingBytes); hw > d.cnt.highWater {
		d.cnt.highWater = hw
	}
}

// ---- timers: traffic, stats, watchdog ------------------------------------------

func (d *Device) timerLoop(ctx context.Context) {
	t := time.NewTicker(5 * time.Millisecond)
	defer t.Stop()
	serveStart := time.Now()
	rebooted := false
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			d.mu.Lock()
			if f := d.o.Faults; f.RebootAt > 0 && !rebooted && now.Sub(serveStart) >= f.RebootAt {
				rebooted = true
				d.bootLocked(wire.ResetWatchdog)
			}
			if d.cfg.CaptureOn && d.session {
				dt := now.Sub(d.lastGenAt).Seconds()
				if dt > 0.1 {
					dt = 0.1
				}
				d.lastGenAt = now
				for _, fr := range d.gen.tick(d.cfg.Channel, dt) {
					d.captureLocked(fr, now)
				}
			}
			if d.session && now.Sub(d.lastRx) > watchdogTimeout {
				d.session = false
				d.applyLocked(func(c *wire.Config) { c.CaptureOn = false })
				d.flushRingLocked()
			}
			if d.session && now.Sub(d.lastStats) >= statsInterval {
				d.lastStats = d.lastStats.Add(statsInterval)
				if now.Sub(d.lastStats) > statsInterval {
					d.lastStats = now
				}
				d.queueCtrl(wire.TypeEvtStats, 0, d.statsLocked().AppendBinary(nil))
				d.cnt.highWater = 0
			}
			d.cond.Broadcast()
			d.mu.Unlock()
		}
	}
}

// ---- link writer ---------------------------------------------------------------

func (d *Device) writeLoop(ctx context.Context, w io.Writer) error {
	go func() {
		<-ctx.Done()
		d.mu.Lock()
		d.cond.Broadcast()
		d.mu.Unlock()
	}()
	budget := float64(d.o.USBBytesPerSec)
	tokens := 4096.0
	last := time.Now()
	serveStart := time.Now()
	buf := make([]byte, 0, 8192)
	for {
		d.mu.Lock()
		for len(d.ctrl) == 0 && (len(d.ring) == 0 || !d.session) && !d.pendingReboot && ctx.Err() == nil {
			d.cond.Wait()
		}
		if ctx.Err() != nil {
			d.mu.Unlock()
			return ctx.Err()
		}
		garbage := false
		if !d.bootSent {
			d.bootSent = true
			garbage = d.o.Faults.BootGarbage
		}
		buf = buf[:0]
		if garbage {
			buf = append(buf, "ESP-ROM:esp32c5\r\nrst:0x1 (POWERON),boot:0x1c\r\n"...)
		}
		buf = append(buf, 0) // leading delimiter after any stall is cheap insurance
		n := 0
		// ctrl first, then packets, up to ~2 KB per write
		for len(d.ctrl) > 0 && len(buf) < 2048 {
			f := d.ctrl[0]
			d.ctrl = d.ctrl[1:]
			buf = d.encodeLocked(buf, f)
			n++
		}
		for d.session && len(d.ring) > 0 && len(buf) < 2048 {
			f := d.ring[0]
			d.ring = d.ring[1:]
			d.ringBytes -= ringRecOverhead + len(f.body)
			buf = d.encodeLocked(buf, f)
			n++
		}
		reboot := d.pendingReboot && len(d.ctrl) == 0
		d.mu.Unlock()

		if fl := d.o.Faults; fl.StallAt > 0 && time.Since(serveStart) >= fl.StallAt && time.Since(serveStart) < fl.StallAt+fl.StallFor {
			time.Sleep(fl.StallAt + fl.StallFor - time.Since(serveStart))
		}
		// token-bucket USB model
		now := time.Now()
		tokens += now.Sub(last).Seconds() * budget
		last = now
		if tokens > 8192 {
			tokens = 8192
		}
		if need := float64(len(buf)); need > tokens {
			time.Sleep(time.Duration((need - tokens) / budget * float64(time.Second)))
			tokens = 0
			last = time.Now()
		} else {
			tokens -= need
		}
		if len(buf) > 1 {
			if _, err := w.Write(buf); err != nil {
				return err
			}
		}
		if reboot {
			d.mu.Lock()
			d.bootLocked(wire.ResetSoftware)
			d.mu.Unlock()
		}
	}
}

func (d *Device) encodeLocked(dst []byte, f outFrame) []byte {
	var flags uint8
	if !d.firstTx && f.cfgGen != d.lastGenTx {
		flags |= wire.FlagCfgChanged
	}
	d.firstTx = false
	d.lastGenTx = f.cfgGen
	h := wire.Header{Ver: wire.ProtoVer, Type: f.typ, Seq: d.seq, ReqID: f.reqID, Flags: flags, CfgGen: f.cfgGen}
	d.seq++
	start := len(dst)
	dst = codec.EncodeFrame(dst, h, f.body)
	if fl := d.o.Faults; fl.DropRate > 0 || fl.CorruptRate > 0 {
		x := d.rng.Float64()
		switch {
		case x < fl.DropRate:
			dst = dst[:start]
		case x < fl.DropRate+fl.CorruptRate:
			i := start + 1 + d.rng.Intn(len(dst)-start-2)
			if dst[i] ^= 0x10; dst[i] == 0 {
				dst[i] = 0x01
			}
		}
	}
	if f.typ == wire.TypePkt {
		d.cnt.txFrames++
		d.cnt.txBytes += uint32(len(dst) - start)
	}
	return dst
}

// Pipe returns an in-memory host endpoint connected to a new Device.
// Closing host stops the device's Serve.
func Pipe(o SimOptions) (host io.ReadWriteCloser, dev *Device) {
	dev = NewDevice(o)
	return dev.Attach(), dev
}

// Attach connects a new in-memory host endpoint to an existing device
// (like re-opening the serial port). Closing it detaches the device.
func (d *Device) Attach() io.ReadWriteCloser {
	h, s := net.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		_ = d.Serve(ctx, s)
		_ = s.Close()
		close(done)
	}()
	return &pipeEnd{Conn: h, cancel: cancel, done: done}
}

type pipeEnd struct {
	net.Conn
	cancel context.CancelFunc
	done   chan struct{}
}

func (p *pipeEnd) Close() error {
	p.cancel()
	err := p.Conn.Close()
	<-p.done
	return err
}
