package conformance

import (
	"context"
	"encoding/binary"
	"io"
	"sync"
	"time"

	"github.com/hjkoskel/capturetonkeli/ctp/codec"
	"github.com/hjkoskel/capturetonkeli/ctp/wire"
)

// Frame is one decoded device→host frame with its arrival time.
type Frame struct {
	H    wire.Header
	Body []byte
	At   time.Time
}

// Link is a raw CTP endpoint used by test cases: it sends arbitrary frames
// and collects every decoded frame.
type Link struct {
	rw     io.ReadWriteCloser
	frames chan Frame
	dec    *codec.Decoder
	seq    uint16
	nextID uint16
	wmu    sync.Mutex
	done   chan struct{}
	trace  func(dir string, h wire.Header, body []byte)
}

func newLink(rw io.ReadWriteCloser, trace func(string, wire.Header, []byte)) *Link {
	l := &Link{rw: rw, frames: make(chan Frame, 65536), dec: codec.NewDecoder(codec.MaxFrame), nextID: 0x4000, done: make(chan struct{}), trace: trace}
	go l.read()
	return l
}

func (l *Link) read() {
	defer close(l.done)
	buf := make([]byte, 16384)
	for {
		n, err := l.rw.Read(buf)
		if n > 0 {
			now := time.Now()
			l.dec.Feed(buf[:n], func(h wire.Header, body []byte) {
				if l.trace != nil {
					l.trace("rx", h, body)
				}
				select {
				case l.frames <- Frame{H: h, Body: append([]byte(nil), body...), At: now}:
				default:
				}
			})
		}
		if err != nil {
			return
		}
	}
}

// Close closes the transport.
func (l *Link) Close() error {
	err := l.rw.Close()
	select {
	case <-l.done:
	case <-time.After(500 * time.Millisecond):
	}
	return err
}

// ID returns a fresh req_id.
func (l *Link) ID() uint16 {
	l.nextID++
	if l.nextID == 0 {
		l.nextID = 1
	}
	return l.nextID
}

// Encode builds the wire bytes of one frame (with leading delimiter).
func (l *Link) Encode(h wire.Header, body []byte) []byte {
	if h.Ver == 0 {
		h.Ver = wire.ProtoVer
	}
	l.wmu.Lock()
	h.Seq = l.seq
	l.seq++
	l.wmu.Unlock()
	if l.trace != nil {
		l.trace("tx", h, body)
	}
	return codec.EncodeFrame([]byte{0}, h, body)
}

// WriteRaw writes raw bytes.
func (l *Link) WriteRaw(b []byte) error {
	l.wmu.Lock()
	defer l.wmu.Unlock()
	_, err := l.rw.Write(b)
	return err
}

// Send sends a request frame.
func (l *Link) Send(typ uint8, id uint16, body []byte) error {
	return l.WriteRaw(l.Encode(wire.Header{Type: typ, ReqID: id}, body))
}

// Await waits for the response with req_id id; other frames go to keep (if non-nil).
func (l *Link) Await(ctx context.Context, id uint16, timeout time.Duration, keep func(Frame)) (Frame, bool) {
	t := time.NewTimer(timeout)
	defer t.Stop()
	for {
		select {
		case f := <-l.frames:
			if (f.H.Type == wire.TypeRspOK || f.H.Type == wire.TypeRspErr) && f.H.ReqID == id {
				return f, true
			}
			if keep != nil {
				keep(f)
			}
		case <-t.C:
			return Frame{}, false
		case <-ctx.Done():
			return Frame{}, false
		}
	}
}

// Req sends a request and waits for its response (with retries using the same req_id).
func (l *Link) Req(ctx context.Context, typ uint8, body []byte) (Frame, bool) {
	id := l.ID()
	for i := 0; i < 4; i++ {
		if l.Send(typ, id, body) != nil {
			return Frame{}, false
		}
		if f, ok := l.Await(ctx, id, 250*time.Millisecond, nil); ok {
			return f, true
		}
	}
	return Frame{}, false
}

// Collect gathers all frames for d.
func (l *Link) Collect(ctx context.Context, d time.Duration) []Frame {
	var out []Frame
	t := time.NewTimer(d)
	defer t.Stop()
	for {
		select {
		case f := <-l.frames:
			out = append(out, f)
		case <-t.C:
			return out
		case <-ctx.Done():
			return out
		}
	}
}

// Drain discards queued frames.
func (l *Link) Drain() {
	for {
		select {
		case <-l.frames:
		default:
			return
		}
	}
}

// Hello performs HELLO and returns the device info.
func (l *Link) Hello(ctx context.Context) (wire.DeviceInfo, bool) {
	var info wire.DeviceInfo
	f, ok := l.Req(ctx, wire.TypeHello, wire.Hello{HostNonce: 0xC0FFEE, HostProtoVer: wire.ProtoVer}.AppendBinary(nil))
	if !ok || f.H.Type != wire.TypeRspOK || info.UnmarshalBinary(f.Body) != nil {
		return info, false
	}
	return info, true
}

// Config sends a config-returning request; ok=false on timeout, err code on RSP_ERR.
func (l *Link) Config(ctx context.Context, typ uint8, body []byte) (wire.Config, wire.ErrCode, bool) {
	var c wire.Config
	f, ok := l.Req(ctx, typ, body)
	if !ok {
		return c, 0, false
	}
	if f.H.Type == wire.TypeRspErr {
		var e wire.RspErr
		_ = e.UnmarshalBinary(f.Body)
		return c, e.Code, true
	}
	if c.UnmarshalBinary(f.Body) != nil {
		return c, 0, false
	}
	return c, 0, true
}

// Stats sends GET_STATS.
func (l *Link) Stats(ctx context.Context, reset bool) (wire.Stats, bool) {
	var st wire.Stats
	f, ok := l.Req(ctx, wire.TypeGetStats, wire.GetStats{ResetAfterRead: reset}.AppendBinary(nil))
	if !ok || f.H.Type != wire.TypeRspOK || st.UnmarshalBinary(f.Body) != nil {
		return st, false
	}
	return st, true
}

// Ping sends one PING and returns the RTT.
func (l *Link) Ping(ctx context.Context, timeout time.Duration) (time.Duration, bool) {
	id := l.ID()
	t0 := time.Now()
	if l.Send(wire.TypePing, id, binary.LittleEndian.AppendUint32(nil, uint32(t0.UnixMilli()))) != nil {
		return 0, false
	}
	_, ok := l.Await(ctx, id, timeout, nil)
	return time.Since(t0), ok
}

// KeepAlive pings every 400 ms until stop is closed (keeps the device watchdog quiet).
// Responses are consumed by whoever reads frames; this only writes.
func (l *Link) KeepAlive(stop <-chan struct{}) {
	t := time.NewTicker(400 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			_ = l.Send(wire.TypePing, 0x7FFF, binary.LittleEndian.AppendUint32(nil, 0))
		}
	}
}
