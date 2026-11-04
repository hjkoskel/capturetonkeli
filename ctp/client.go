// Package ctp is the host-side CTP v1 client (docs/05 §6): session setup,
// request/response with retry and dedup-safe req_ids, keepalive, cfg_gen
// drift tracking, events and captured packets.
package ctp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hjkoskel/capturetonkeli/ctp/codec"
	"github.com/hjkoskel/capturetonkeli/ctp/filter"
	"github.com/hjkoskel/capturetonkeli/ctp/fwver"
	"github.com/hjkoskel/capturetonkeli/ctp/serialport"
	"github.com/hjkoskel/capturetonkeli/ctp/wire"
)

// Errors.
var (
	ErrTimeout        = errors.New("ctp: request timed out")
	ErrLinkDead       = errors.New("ctp: link dead")
	ErrClosed         = errors.New("ctp: client closed")
	ErrPortBusy       = serialport.ErrPortBusy
	ErrShortBody      = wire.ErrShortBody
	ErrFilterMismatch = errors.New("ctp: device rules_crc32 differs from local filter")
)

// DeviceError is returned for RSP_ERR (errors.As target).
type DeviceError struct {
	wire.RspErr
	Request uint8
}

func (e *DeviceError) Error() string {
	return fmt.Sprintf("ctp: %s: %s", wire.TypeName(e.Request), e.RspErr.Error())
}

// ProtocolVersionError: header/HELLO protocol version mismatch.
type ProtocolVersionError struct{ Device, Host uint8 }

func (e *ProtocolVersionError) Error() string {
	return fmt.Sprintf("ctp: device speaks protocol v%d, host v%d", e.Device, e.Host)
}

// FirmwareMismatchError: MAJOR.MINOR mismatch under VersionPolicy Strict.
type FirmwareMismatchError struct {
	Device, Host  fwver.Version
	DeviceBuildID [8]byte
	DeviceFlags   uint8
}

func (e *FirmwareMismatchError) Error() string {
	return fmt.Sprintf("ctp: firmware %s not compatible with expected %s", e.Device, e.Host)
}

// FullPolicy for the packet queue.
type FullPolicy int

const (
	DropNewest FullPolicy = iota
	Block
)

// VersionPolicy for firmware mismatches.
type VersionPolicy int

const (
	Strict VersionPolicy = iota
	AllowMismatch
)

// LinkState of the client.
type LinkState int

const (
	LinkUp LinkState = iota
	LinkDegraded
	LinkDown
)

func (s LinkState) String() string {
	return [...]string{"up", "degraded", "down"}[s]
}

// DesiredConfig is pushed after HELLO and re-pushed on reset/drift.
type DesiredConfig struct {
	Country  *wire.Country
	Channel  *uint8
	BW       *wire.Bandwidth
	Snaplen  *uint16
	TypeMask *uint32
	CtrlMask *uint32
	Filter   *filter.Set
	Capture  *bool
}

// Options for New/Dial.
type Options struct {
	Logger         *slog.Logger
	RequestTimeout time.Duration
	Retries        int
	PingInterval   time.Duration // 0 = default 500 ms; <0 disables
	PacketQueue    int
	OnFull         FullPolicy
	Desired        *DesiredConfig
	HostProtoVer   uint8
	HostFwVersion  *fwver.Version // nil = no firmware check
	VersionPolicy  VersionPolicy
	// Trace, if set, receives every frame sent and received.
	Trace func(dir string, h wire.Header, body []byte)
}

func (o *Options) defaults() {
	if o.RequestTimeout == 0 {
		o.RequestTimeout = 200 * time.Millisecond
	}
	if o.Retries == 0 {
		o.Retries = 3
	}
	if o.PingInterval == 0 {
		o.PingInterval = 500 * time.Millisecond
	}
	if o.PacketQueue == 0 {
		o.PacketQueue = 65536
	}
	if o.HostProtoVer == 0 {
		o.HostProtoVer = wire.ProtoVer
	}
	if o.Logger == nil {
		o.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
}

// Packet is one captured frame.
type Packet struct {
	wire.PktMeta
	Data   []byte
	CfgGen uint8
	HostRx time.Time
}

// HostCounters are host-side link statistics.
type HostCounters struct {
	Frames       uint64             `json:"frames"`
	LinkLost     uint64             `json:"link_lost"`
	DeviceGaps   uint64             `json:"device_gaps"`
	QueueDrops   uint64             `json:"queue_drops"`
	UnknownTypes uint64             `json:"unknown_types"`
	Retries      uint64             `json:"retries"`
	Decoder      codec.DecoderStats `json:"decoder"`
}

// Events.
type Event interface{ isEvent() }

type (
	EventStats       struct{ wire.Stats }
	EventLog         struct{ wire.Log }
	EventDeviceReset struct{ wire.Reset }
	EventLink        struct{ State LinkState }
	EventGap         struct {
		Where GapWhere
		N     uint32
	}
	EventConfigDrift struct{ Old, New wire.Config }
	EventRepushed    struct{ Config wire.Config }
	EventVersionInfo struct {
		Device, Host fwver.Version
		Note         string
	}
)

// GapWhere tells where frames were lost.
type GapWhere int

const (
	GapHost GapWhere = iota
	GapDevice
)

func (EventStats) isEvent()       {}
func (EventLog) isEvent()         {}
func (EventDeviceReset) isEvent() {}
func (EventLink) isEvent()        {}
func (EventGap) isEvent()         {}
func (EventConfigDrift) isEvent() {}
func (EventRepushed) isEvent()    {}
func (EventVersionInfo) isEvent() {}

type response struct {
	h    wire.Header
	body []byte
}

// Client is a CTP session over one transport.
type Client struct {
	o   Options
	rw  io.ReadWriteCloser
	log *slog.Logger

	wmu   sync.Mutex
	seq   uint16
	reqID atomic.Uint32

	mu          sync.Mutex
	pending     map[uint16]chan response
	info        wire.DeviceInfo
	cfg         wire.Config
	haveCfg     bool
	knownGen    uint8
	lastSeq     uint16
	haveSeq     bool
	lastRxSeq   uint16
	haveRxSeq   bool
	counters    HostCounters
	state       LinkState
	unsupported bool

	packets chan *Packet
	events  chan Event
	dec     *codec.Decoder
	closed  chan struct{}
	emitMu  sync.RWMutex
	chDone  bool
	once    sync.Once
	readErr error
	wg      sync.WaitGroup
}

// Dial opens a serial port ("auto" = the only dongle found) and calls New.
func Dial(ctx context.Context, path string, o Options) (*Client, error) {
	if path == "" || path == "auto" {
		ds, err := serialport.FindDongles()
		if err != nil {
			return nil, err
		}
		if len(ds) != 1 {
			return nil, fmt.Errorf("ctp: %d dongles found (want exactly 1); pass a port", len(ds))
		}
		path = ds[0].Path
	}
	p, err := serialport.Open(path)
	if err != nil {
		return nil, err
	}
	serialport.ApplySafeOpenSignals(p)
	return New(ctx, p, o)
}

// New starts a session on rw: HELLO, version checks, optional config push.
func New(ctx context.Context, rw io.ReadWriteCloser, o Options) (*Client, error) {
	o.defaults()
	c := &Client{o: o, rw: rw, log: o.Logger, pending: map[uint16]chan response{},
		packets: make(chan *Packet, o.PacketQueue), events: make(chan Event, 1024),
		dec: codec.NewDecoder(codec.MaxFrame), closed: make(chan struct{})}
	c.reqID.Store(uint32(rand.Intn(0x8000)))
	c.wg.Add(1)
	go c.readLoop()
	if err := c.handshake(ctx); err != nil {
		c.shutdown()
		return nil, err
	}
	if o.PingInterval > 0 {
		c.wg.Add(1)
		go c.keepalive()
	}
	return c, nil
}

func (c *Client) handshake(ctx context.Context) error {
	nonce := rand.Uint32()
	_, body, err := c.Do(ctx, wire.TypeHello, wire.Hello{HostNonce: nonce, HostProtoVer: c.o.HostProtoVer}.AppendBinary(nil))
	if err != nil {
		return err
	}
	var info wire.DeviceInfo
	if err := info.UnmarshalBinary(body); err != nil {
		return fmt.Errorf("ctp: HELLO response: %w", err)
	}
	if info.HostNonce != nonce {
		return fmt.Errorf("ctp: HELLO nonce mismatch")
	}
	if info.ProtoVer != c.o.HostProtoVer {
		return &ProtocolVersionError{Device: info.ProtoVer, Host: c.o.HostProtoVer}
	}
	c.mu.Lock()
	c.info = info
	c.mu.Unlock()
	if hv := c.o.HostFwVersion; hv != nil {
		dv := fwver.FromWire(info.FwVersion)
		if !fwver.Compatible(*hv, dv) {
			if c.o.VersionPolicy == Strict {
				return &FirmwareMismatchError{Device: dv, Host: *hv, DeviceBuildID: info.FwBuildID, DeviceFlags: info.FwFlags}
			}
			c.mu.Lock()
			c.unsupported = true
			c.mu.Unlock()
			c.emit(EventVersionInfo{Device: dv, Host: *hv, Note: "unsupported: MAJOR.MINOR mismatch"})
		} else if dv != *hv {
			c.emit(EventVersionInfo{Device: dv, Host: *hv, Note: "patch difference"})
		}
	}
	if _, err := c.GetConfig(ctx); err != nil {
		return err
	}
	if c.o.Desired != nil {
		if _, err := c.Push(ctx, *c.o.Desired); err != nil {
			return err
		}
	}
	return nil
}

// Push applies a desired config in doc 03 §7 order and returns the final readback.
func (c *Client) Push(ctx context.Context, d DesiredConfig) (wire.Config, error) {
	cfg, err := c.GetConfig(ctx)
	if err != nil {
		return cfg, err
	}
	if d.Country != nil {
		if cfg, err = c.SetCountry(ctx, *d.Country); err != nil {
			return cfg, err
		}
	}
	if d.Channel != nil {
		bw := wire.BW20
		if d.BW != nil {
			bw = *d.BW
		}
		if cfg, err = c.SetChannel(ctx, *d.Channel, bw); err != nil {
			return cfg, err
		}
	}
	if d.Snaplen != nil {
		if cfg, err = c.SetSnaplen(ctx, *d.Snaplen); err != nil {
			return cfg, err
		}
	}
	if d.Filter != nil {
		if cfg, err = c.SetFilter(ctx, *d.Filter); err != nil {
			return cfg, err
		}
	}
	if d.TypeMask != nil || d.CtrlMask != nil {
		tm, cm := cfg.TypeMask, cfg.CtrlMask
		if d.TypeMask != nil {
			tm = *d.TypeMask
		}
		if d.CtrlMask != nil {
			cm = *d.CtrlMask
		}
		if cfg, err = c.SetDriverMask(ctx, tm, cm); err != nil {
			return cfg, err
		}
	}
	if d.Capture != nil {
		if cfg, err = c.Capture(ctx, *d.Capture); err != nil {
			return cfg, err
		}
	}
	return cfg, nil
}

// Info returns the HELLO device info.
func (c *Client) Info() wire.DeviceInfo {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.info
}

// Unsupported reports a tolerated MAJOR.MINOR mismatch.
func (c *Client) Unsupported() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.unsupported
}

// LastConfig returns the latest CONFIG readback.
func (c *Client) LastConfig() wire.Config {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.cfg
}

// Packets returns the captured packet stream.
func (c *Client) Packets() <-chan *Packet { return c.packets }

// Events returns the event stream (best effort; drops when full).
func (c *Client) Events() <-chan Event { return c.events }

// Counters returns host counters.
func (c *Client) Counters() HostCounters {
	c.mu.Lock()
	defer c.mu.Unlock()
	h := c.counters
	h.Decoder = c.dec.Stats()
	return h
}

// LinkState returns the current link state.
func (c *Client) LinkState() LinkState {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.state
}

func (c *Client) emit(e Event) {
	c.emitMu.RLock()
	defer c.emitMu.RUnlock()
	if c.chDone {
		return
	}
	select {
	case c.events <- e:
	default:
	}
}

func (c *Client) setReadErr(err error) {
	c.mu.Lock()
	if c.readErr == nil {
		c.readErr = err
	}
	c.mu.Unlock()
}

func (c *Client) getReadErr() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.readErr
}

// Done is closed when the client has shut down.
func (c *Client) Done() <-chan struct{} { return c.closed }

// Err returns the error that ended the session, if any.
func (c *Client) Err() error { return c.getReadErr() }

// ---- requests -------------------------------------------------------------

func (c *Client) nextReqID() uint16 {
	for {
		id := uint16(c.reqID.Add(1))
		if id != 0 {
			return id
		}
	}
}

// WriteFrame sends one raw frame (no response handling).
func (c *Client) WriteFrame(h wire.Header, body []byte) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	if h.Ver == 0 {
		h.Ver = wire.ProtoVer
	}
	h.Seq = c.seq
	c.seq++
	if c.o.Trace != nil {
		c.o.Trace("tx", h, body)
	}
	_, err := c.rw.Write(codec.EncodeFrame([]byte{0}, h, body))
	return err
}

// Do sends a raw request and waits for its response, retrying with the same
// req_id. RSP_ERR is returned as *DeviceError together with the header.
func (c *Client) Do(ctx context.Context, typ uint8, body []byte) (wire.Header, []byte, error) {
	return c.DoID(ctx, c.nextReqID(), typ, body)
}

// DoID is Do with an explicit req_id (test bench).
func (c *Client) DoID(ctx context.Context, id uint16, typ uint8, body []byte) (wire.Header, []byte, error) {
	ch := make(chan response, 4)
	c.mu.Lock()
	c.pending[id] = ch
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
	}()
	for attempt := 0; attempt <= c.o.Retries; attempt++ {
		if attempt > 0 {
			c.mu.Lock()
			c.counters.Retries++
			c.mu.Unlock()
		}
		if err := c.WriteFrame(wire.Header{Type: typ, ReqID: id}, body); err != nil {
			return wire.Header{}, nil, err
		}
		t := time.NewTimer(c.o.RequestTimeout)
		select {
		case r := <-ch:
			t.Stop()
			if r.h.Type == wire.TypeRspErr {
				var e wire.RspErr
				_ = e.UnmarshalBinary(r.body)
				return r.h, r.body, &DeviceError{RspErr: e, Request: typ}
			}
			return r.h, r.body, nil
		case <-t.C:
		case <-ctx.Done():
			t.Stop()
			return wire.Header{}, nil, ctx.Err()
		case <-c.closed:
			t.Stop()
			if re := c.getReadErr(); re != nil {
				return wire.Header{}, nil, fmt.Errorf("%w: %v", ErrLinkDead, re)
			}
			return wire.Header{}, nil, ErrClosed
		}
	}
	return wire.Header{}, nil, ErrTimeout
}

func (c *Client) doConfig(ctx context.Context, typ uint8, body []byte) (wire.Config, error) {
	_, b, err := c.Do(ctx, typ, body)
	if err != nil {
		return c.LastConfig(), err
	}
	var cfg wire.Config
	if err := cfg.UnmarshalBinary(b); err != nil {
		return cfg, err
	}
	c.mu.Lock()
	prevOn := c.cfg.CaptureOn && c.haveCfg
	c.cfg, c.haveCfg, c.knownGen = cfg, true, cfg.CfgGen
	if cfg.CaptureOn && !prevOn {
		c.haveRxSeq = false
	}
	c.mu.Unlock()
	return cfg, nil
}

func (c *Client) GetConfig(ctx context.Context) (wire.Config, error) {
	return c.doConfig(ctx, wire.TypeGetConfig, nil)
}
func (c *Client) SetCountry(ctx context.Context, cc wire.Country) (wire.Config, error) {
	return c.doConfig(ctx, wire.TypeSetCountry, cc.AppendBinary(nil))
}
func (c *Client) SetChannel(ctx context.Context, ch uint8, bw wire.Bandwidth) (wire.Config, error) {
	return c.doConfig(ctx, wire.TypeSetChannel, wire.SetChannel{Channel: ch, BW: bw}.AppendBinary(nil))
}
func (c *Client) SetSnaplen(ctx context.Context, n uint16) (wire.Config, error) {
	return c.doConfig(ctx, wire.TypeSetSnaplen, wire.SetSnaplen{Snaplen: n}.AppendBinary(nil))
}
func (c *Client) SetDriverMask(ctx context.Context, typ, ctrl uint32) (wire.Config, error) {
	return c.doConfig(ctx, wire.TypeSetDriverMask, wire.SetDriverMask{TypeMask: typ, CtrlMask: ctrl}.AppendBinary(nil))
}
func (c *Client) Capture(ctx context.Context, on bool) (wire.Config, error) {
	return c.doConfig(ctx, wire.TypeCapture, wire.Capture{On: on}.AppendBinary(nil))
}

// SetFilter applies a filter and verifies rules_crc32.
func (c *Client) SetFilter(ctx context.Context, s filter.Set) (wire.Config, error) {
	body, err := s.Encode()
	if err != nil {
		return c.LastConfig(), err
	}
	cfg, err := c.doConfig(ctx, wire.TypeSetFilter, body)
	if err != nil {
		return cfg, err
	}
	if cfg.RulesCRC32 != codec.CRC32(body) {
		return cfg, ErrFilterMismatch
	}
	return cfg, nil
}

// Stats reads device counters.
func (c *Client) Stats(ctx context.Context, reset bool) (wire.Stats, error) {
	var st wire.Stats
	_, b, err := c.Do(ctx, wire.TypeGetStats, wire.GetStats{ResetAfterRead: reset}.AppendBinary(nil))
	if err != nil {
		return st, err
	}
	err = st.UnmarshalBinary(b)
	return st, err
}

// Ping measures one round trip.
func (c *Client) Ping(ctx context.Context) (time.Duration, error) {
	t0 := time.Now()
	_, _, err := c.Do(ctx, wire.TypePing, wire.Ping{HostTimeMS: uint32(time.Now().UnixMilli())}.AppendBinary(nil))
	return time.Since(t0), err
}

// Reboot asks the device to reboot and returns after RSP_OK.
func (c *Client) Reboot(ctx context.Context, mode wire.RebootMode) error {
	_, _, err := c.Do(ctx, wire.TypeReboot, wire.Reboot{Mode: mode}.AppendBinary(nil))
	return err
}

// Close stops capture (best effort) and closes the transport.
func (c *Client) Close() error {
	select {
	case <-c.closed:
		return nil
	default:
	}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	_, _ = c.Capture(ctx, false)
	cancel()
	return c.shutdown()
}

// Abandon closes the transport without CAPTURE 0 (simulates a host crash).
func (c *Client) Abandon() error { return c.shutdown() }

func (c *Client) shutdown() error {
	var err error
	c.once.Do(func() {
		close(c.closed)
		err = c.rw.Close()
		c.wg.Wait()
		c.emitMu.Lock()
		c.chDone = true
		close(c.events)
		close(c.packets)
		c.emitMu.Unlock()
	})
	return err
}

// ---- reader -----------------------------------------------------------------

func (c *Client) readLoop() {
	defer c.wg.Done()
	buf := make([]byte, 16384)
	for {
		n, err := c.rw.Read(buf)
		if n > 0 {
			c.dec.Feed(buf[:n], c.dispatch)
		}
		if err != nil {
			select {
			case <-c.closed:
			default:
				c.setReadErr(err)
				c.setState(LinkDown)
				go c.shutdown()
			}
			return
		}
	}
}

func (c *Client) setState(s LinkState) {
	c.mu.Lock()
	ch := c.state != s
	c.state = s
	c.mu.Unlock()
	if ch {
		c.emit(EventLink{State: s})
	}
}

func (c *Client) dispatch(h wire.Header, body []byte) {
	if c.o.Trace != nil {
		c.o.Trace("rx", h, body)
	}
	c.mu.Lock()
	c.counters.Frames++
	if h.Type == wire.TypeEvtReset {
		c.haveSeq = false
	}
	if c.haveSeq {
		if gap := h.Seq - (c.lastSeq + 1); gap != 0 {
			c.counters.LinkLost += uint64(gap)
			c.mu.Unlock()
			c.emit(EventGap{Where: GapHost, N: uint32(gap)})
			c.mu.Lock()
		}
	}
	c.lastSeq, c.haveSeq = h.Seq, true
	c.mu.Unlock()

	switch h.Type {
	case wire.TypeRspOK, wire.TypeRspErr:
		c.mu.Lock()
		ch := c.pending[h.ReqID]
		c.mu.Unlock()
		if ch != nil {
			select {
			case ch <- response{h: h, body: append([]byte(nil), body...)}:
			default:
			}
		}
	case wire.TypePkt:
		meta, data, err := wire.DecodePkt(body)
		if err != nil {
			return
		}
		c.mu.Lock()
		if c.haveRxSeq {
			if gap := meta.RxSeq - (c.lastRxSeq + 1); gap != 0 && gap < 0x8000 {
				c.counters.DeviceGaps += uint64(gap)
				c.mu.Unlock()
				c.emit(EventGap{Where: GapDevice, N: uint32(gap)})
				c.mu.Lock()
			}
		}
		c.lastRxSeq, c.haveRxSeq = meta.RxSeq, true
		c.mu.Unlock()
		p := &Packet{PktMeta: meta, Data: append([]byte(nil), data...), CfgGen: h.CfgGen, HostRx: time.Now()}
		if c.o.OnFull == Block {
			select {
			case c.packets <- p:
			case <-c.closed:
			}
			return
		}
		select {
		case c.packets <- p:
		default:
			c.mu.Lock()
			c.counters.QueueDrops++
			c.mu.Unlock()
		}
	case wire.TypeEvtStats:
		var st wire.Stats
		if st.UnmarshalBinary(body) == nil {
			c.emit(EventStats{st})
		}
		c.mu.Lock()
		drift := c.haveCfg && h.CfgGen != c.knownGen
		c.mu.Unlock()
		if drift {
			go c.onDrift()
		}
	case wire.TypeEvtLog:
		var l wire.Log
		if l.UnmarshalBinary(body) == nil {
			c.emit(EventLog{l})
		}
	case wire.TypeEvtReset:
		var r wire.Reset
		if r.UnmarshalBinary(body) == nil {
			c.emit(EventDeviceReset{r})
		}
		c.mu.Lock()
		was := c.haveCfg
		c.haveRxSeq = false
		c.mu.Unlock()
		if was {
			go c.onReset()
		}
	default:
		c.mu.Lock()
		c.counters.UnknownTypes++
		c.mu.Unlock()
	}
}

func (c *Client) onDrift() {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	old := c.LastConfig()
	cfg, err := c.GetConfig(ctx)
	if err != nil {
		return
	}
	c.emit(EventConfigDrift{Old: old, New: cfg})
	if c.o.Desired != nil {
		if cfg, err = c.Push(ctx, *c.o.Desired); err == nil {
			c.emit(EventRepushed{Config: cfg})
		}
	}
}

func (c *Client) onReset() {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	nonce := rand.Uint32()
	_, body, err := c.Do(ctx, wire.TypeHello, wire.Hello{HostNonce: nonce, HostProtoVer: c.o.HostProtoVer}.AppendBinary(nil))
	if err != nil {
		return
	}
	var info wire.DeviceInfo
	if info.UnmarshalBinary(body) == nil {
		c.mu.Lock()
		c.info = info
		c.mu.Unlock()
	}
	if _, err := c.GetConfig(ctx); err != nil {
		return
	}
	if c.o.Desired != nil {
		if cfg, err := c.Push(ctx, *c.o.Desired); err == nil {
			c.emit(EventRepushed{Config: cfg})
		}
	}
}

func (c *Client) keepalive() {
	defer c.wg.Done()
	t := time.NewTicker(c.o.PingInterval)
	defer t.Stop()
	fails := 0
	for {
		select {
		case <-c.closed:
			return
		case <-t.C:
			ctx, cancel := context.WithTimeout(context.Background(), c.o.RequestTimeout*time.Duration(c.o.Retries+1))
			_, err := c.Ping(ctx)
			cancel()
			if err == nil {
				fails = 0
				c.setState(LinkUp)
				continue
			}
			if errors.Is(err, ErrClosed) {
				return
			}
			fails++
			c.setState(LinkDegraded)
			if fails >= 3 {
				c.setState(LinkDown)
				c.setReadErr(ErrLinkDead)
				go c.shutdown()
				return
			}
		}
	}
}
