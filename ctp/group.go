package ctp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sort"
	"sync"
	"time"

	"github.com/hjkoskel/capturetonkeli/ctp/diversity"
	"github.com/hjkoskel/capturetonkeli/ctp/filter"
	"github.com/hjkoskel/capturetonkeli/ctp/fwver"
	"github.com/hjkoskel/capturetonkeli/ctp/pcapng"
	"github.com/hjkoskel/capturetonkeli/ctp/wire"
)

// MaxDongles is the largest group (same limit as the web UI, docs/14 §9.2).
const MaxDongles = 4

// MemberState is a group member's state.
type MemberState int

const (
	MemberMerged    MemberState = iota // its frames are merged
	MemberNotMerged                    // its config readback differs from the primary's (docs/14 §4.2 mismatch)
	MemberLost                         // link lost; rejoins by itself when opened by path
)

func (s MemberState) String() string {
	return [...]string{"merged", "not merged", "lost"}[s]
}

// GroupOptions for OpenGroup/NewGroup.
type GroupOptions struct {
	// Options are used for every member's Client. Options.Desired is the group's starting
	// config, pushed to every member.
	Options
	// Labels names dongles by MAC ("aa:bb:cc:dd:ee:ff" → "patch N"), docs/14 §4.1.
	Labels map[string]string
	// Archive enables Final(): every frame once its copy window closed, with all copies
	// (for pcapng with per-antenna RSSI, docs/14 §4.6). Final must then be drained.
	Archive bool
	// Rejoin is how often a lost member opened by path is retried (default 2 s; <0 = never).
	Rejoin time.Duration
	// Window is the merge copy window (default 100 ms).
	Window time.Duration
	// FrameQueue is the Frames()/Final() buffer (default Options.PacketQueue).
	FrameQueue int
}

// Frame is one air frame out of a group.
//
// From Frames() (live path, docs/14 §4.6) it is sent as soon as the first copy arrives:
// Copies holds that copy only. From Final() it carries every copy.
type Frame struct {
	wire.PktMeta         // the first copy's metadata (TsUS = that dongle's device time)
	Data         []byte  // the first copy's bytes
	GroupTsUS    float64 // time on the reference dongle's clock (µs)
	Member       int     // the member that delivered the first copy
	Copies       []diversity.Copy
	Bad          bool      // a bad-FCS copy that no good copy replaced
	CfgGen       uint8     // the first copy's config generation
	HostRx       time.Time // host receive time of the first copy
}

// Antennas returns the copies in the form the pcapng writer takes (antenna = member id).
// With one copy it returns nil, so a single dongle writes the plain radiotap header.
func (f *Frame) Antennas() []pcapng.Antenna {
	if len(f.Copies) < 2 {
		return nil
	}
	out := make([]pcapng.Antenna, len(f.Copies))
	for i, c := range f.Copies {
		out[i] = pcapng.Antenna{Index: uint8(c.Member), RSSI: c.RSSI}
	}
	return out
}

// GroupEvent is a member's Event (or a group event) tagged with the member id.
type GroupEvent struct {
	Member int
	Event  Event
}

// Group events.
type (
	EventMemberJoined   struct{ MAC string } // a member joined or rejoined
	EventMemberLost     struct{ Err error }
	EventMemberExcluded struct{ Reason string } // config readback differs from the primary's
	EventPrimary        struct{}                // this member became the primary
)

func (EventMemberJoined) isEvent()   {}
func (EventMemberLost) isEvent()     {}
func (EventMemberExcluded) isEvent() {}
func (EventPrimary) isEvent()        {}

// MemberStatus describes one member (Group.Members, Group.Stats).
type MemberStatus struct {
	ID       int          `json:"id"`
	Path     string       `json:"path,omitempty"`
	MAC      string       `json:"mac"`
	Label    string       `json:"label,omitempty"`
	State    string       `json:"state"`
	Reason   string       `json:"reason,omitempty"`
	Primary  bool         `json:"primary"`
	Counters HostCounters `json:"counters"`
	Device   *wire.Stats  `json:"device,omitempty"` // latest EVT_STATS
}

// GroupStats are the group statistics (docs/14 §5).
type GroupStats struct {
	Members    []MemberStatus    `json:"members"`
	Merge      diversity.Summary `json:"merge"`
	Merging    bool              `json:"merging"` // 2+ merged members
	LiveDrops  uint64            `json:"live_drops"`
	FinalDrops uint64            `json:"final_drops"`
}

type gmember struct {
	id     int
	path   string
	c      *Client
	mac    string
	state  MemberState
	reason string
	stats  *wire.Stats
	gen    int // bumps on every (re)join so stale forwarders stop
}

type inPkt struct {
	id  int
	gen int
	p   *Packet
}

// Group is several dongles on one channel acting as one capture device (receive
// diversity, docs/14). Config changes go to every member with readback verification;
// frames are merged (first copy wins). A group of one is a pass-through.
type Group struct {
	o      GroupOptions
	t0     time.Time
	frames chan *Frame
	final  chan *Frame
	events chan GroupEvent
	in     chan inPkt
	done   chan struct{}
	once   sync.Once
	wg     sync.WaitGroup

	mu         sync.Mutex
	members    [MaxDongles]*gmember
	primary    int
	desired    DesiredConfig
	merger     *diversity.Merger
	merging    bool
	liveDrops  uint64
	finalDrops uint64
}

func macString(m [6]byte) string { return net.HardwareAddr(m[:]).String() }

// OpenGroup opens every port (HELLO in parallel), checks that the firmware MAJOR.MINOR
// agree, pushes o.Desired to all and starts merging. Lost members are reopened by path.
func OpenGroup(ctx context.Context, paths []string, o GroupOptions) (*Group, error) {
	return openGroup(ctx, len(paths), o, paths, func(ctx context.Context, i int) (*Client, error) {
		return Dial(ctx, paths[i], o.memberOptions(nil))
	})
}

// NewGroup is OpenGroup over already open transports (pipes, simulators). Members created
// this way do not rejoin after loss.
func NewGroup(ctx context.Context, rws []io.ReadWriteCloser, o GroupOptions) (*Group, error) {
	return openGroup(ctx, len(rws), o, nil, func(ctx context.Context, i int) (*Client, error) {
		return New(ctx, rws[i], o.memberOptions(nil))
	})
}

func (o GroupOptions) memberOptions(d *DesiredConfig) Options {
	m := o.Options
	m.Desired = d
	return m
}

func openGroup(ctx context.Context, n int, o GroupOptions, paths []string, open func(context.Context, int) (*Client, error)) (*Group, error) {
	if n < 1 || n > MaxDongles {
		return nil, fmt.Errorf("ctp: a group has 1–%d dongles, got %d", MaxDongles, n)
	}
	if o.Rejoin == 0 {
		o.Rejoin = 2 * time.Second
	}
	o.Options.defaults()
	if o.FrameQueue == 0 {
		o.FrameQueue = o.PacketQueue
	}
	g := &Group{o: o, t0: time.Now(), frames: make(chan *Frame, o.FrameQueue), events: make(chan GroupEvent, 1024),
		in: make(chan inPkt, 4096), done: make(chan struct{})}
	if o.Archive {
		g.final = make(chan *Frame, o.FrameQueue)
	}
	g.merger = diversity.NewMerger(diversity.Options{Window: o.Window, OnFinal: g.onFinal})
	if o.Desired != nil {
		g.desired = *o.Desired
	}

	clients := make([]*Client, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) { defer wg.Done(); clients[i], errs[i] = open(ctx, i) }(i)
	}
	wg.Wait()
	closeAll := func() {
		for _, c := range clients {
			if c != nil {
				c.Abandon()
			}
		}
	}
	for i, err := range errs {
		if err != nil {
			closeAll()
			if paths != nil {
				return nil, fmt.Errorf("ctp: dongle %d (%s): %w", i+1, paths[i], err)
			}
			return nil, fmt.Errorf("ctp: dongle %d: %w", i+1, err)
		}
	}
	seen := map[string]int{}
	v0 := fwver.FromWire(clients[0].Info().FwVersion)
	for i, c := range clients {
		mac := macString(c.Info().MAC)
		if j, dup := seen[mac]; dup {
			closeAll()
			return nil, fmt.Errorf("ctp: dongles %d and %d are the same device (%s)", j+1, i+1, mac)
		}
		seen[mac] = i
		if v := fwver.FromWire(c.Info().FwVersion); !fwver.Compatible(v0, v) {
			closeAll()
			return nil, fmt.Errorf("ctp: dongle %d runs firmware %s, dongle 1 %s: update them to the same MAJOR.MINOR first", i+1, v, v0)
		}
		m := &gmember{id: i, c: c, mac: mac}
		if paths != nil {
			m.path = paths[i]
		}
		g.members[i] = m
	}

	g.mu.Lock()
	g.syncMergerLocked() // all members merged from the start; Push/verify then excludes mismatches
	g.mu.Unlock()
	g.wg.Add(1)
	go g.mergeLoop()
	for _, m := range g.members[:n] {
		g.startMember(m)
	}
	if o.Desired != nil {
		if _, err := g.Push(ctx, g.desired); err != nil {
			g.Close()
			return nil, err
		}
	} else {
		g.mu.Lock()
		g.verifyLocked()
		g.mu.Unlock()
	}
	return g, nil
}

// startMember starts the packet and event forwarders of a (re)joined member.
func (g *Group) startMember(m *gmember) {
	g.mu.Lock()
	m.gen++
	gen, c, id := m.gen, m.c, m.id
	g.mu.Unlock()
	g.event(id, EventMemberJoined{MAC: m.mac})
	g.wg.Add(2)
	go func() {
		defer g.wg.Done()
		for {
			select {
			case p, ok := <-c.Packets():
				if !ok {
					return
				}
				select {
				case g.in <- inPkt{id, gen, p}:
				case <-g.done:
					return
				}
			case <-c.Done():
				g.memberLost(id, gen, c.Err())
				return
			case <-g.done:
				return
			}
		}
	}()
	go func() {
		defer g.wg.Done()
		for {
			select {
			case e := <-c.Events():
				if st, ok := e.(EventStats); ok {
					s := st.Stats
					g.mu.Lock()
					m.stats = &s
					g.mu.Unlock()
				}
				g.event(id, e)
			case <-c.Done():
				return
			case <-g.done:
				return
			}
		}
	}()
}

func (g *Group) event(id int, e Event) {
	select {
	case g.events <- GroupEvent{Member: id, Event: e}:
	default:
	}
}

// verifyLocked marks members whose readback differs from the primary's as not merged and
// updates the merger's member set.
func (g *Group) verifyLocked() {
	p := g.members[g.primary]
	if p == nil || p.state == MemberLost {
		g.pickPrimaryLocked()
		p = g.members[g.primary]
	}
	var ref wire.Config
	if p != nil {
		ref = p.c.LastConfig()
	}
	for _, m := range g.members {
		if m == nil || m.state == MemberLost {
			continue
		}
		cfg := m.c.LastConfig()
		if m == p || cfg.SameSettings(ref) {
			if m.state == MemberNotMerged {
				m.reason = ""
			}
			m.state = MemberMerged
			continue
		}
		reason := fmt.Sprintf("config differs from the primary's: ch %d bw %d snaplen %d capture %v vs ch %d bw %d snaplen %d capture %v",
			cfg.Channel, cfg.BW, cfg.Snaplen, cfg.CaptureOn, ref.Channel, ref.BW, ref.Snaplen, ref.CaptureOn)
		if m.state != MemberNotMerged || m.reason != reason {
			m.state, m.reason = MemberNotMerged, reason
			g.event(m.id, EventMemberExcluded{Reason: reason})
		}
	}
	g.syncMergerLocked()
}

func (g *Group) pickPrimaryLocked() {
	for _, m := range g.members {
		if m != nil && m.state != MemberLost {
			if m.id != g.primary {
				g.primary = m.id
				g.event(m.id, EventPrimary{})
			}
			return
		}
	}
}

func (g *Group) syncMergerLocked() {
	var list []diversity.Member
	for _, m := range g.members {
		if m != nil && m.state == MemberMerged {
			list = append(list, diversity.Member{ID: m.id, MAC: m.mac})
		}
	}
	merging := len(list) > 1
	if g.merging && !merging {
		g.merger.Flush()
		g.flushDueLocked(int64(^uint64(0) >> 1))
	}
	g.merging = merging
	_ = g.merger.SetMembers(list) // ids are < MaxDongles
}

func (g *Group) memberLost(id, gen int, err error) {
	select {
	case <-g.done:
		return // closing
	default:
	}
	g.mu.Lock()
	m := g.members[id]
	if m == nil || m.gen != gen || m.state == MemberLost {
		g.mu.Unlock()
		return
	}
	m.state, m.reason = MemberLost, fmt.Sprint(err)
	wasPrimary := id == g.primary
	if wasPrimary {
		g.pickPrimaryLocked()
	}
	g.syncMergerLocked()
	path := m.path
	g.mu.Unlock()
	g.event(id, EventMemberLost{Err: err})
	if path != "" && g.o.Rejoin > 0 {
		g.wg.Add(1)
		go g.rejoin(m)
	}
}

func (g *Group) rejoin(m *gmember) {
	defer g.wg.Done()
	t := time.NewTicker(g.o.Rejoin)
	defer t.Stop()
	for {
		select {
		case <-g.done:
			return
		case <-t.C:
		}
		g.mu.Lock()
		d := g.desired
		g.mu.Unlock()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		c, err := Dial(ctx, m.path, g.o.memberOptions(&d))
		cancel()
		if err != nil {
			continue
		}
		select {
		case <-g.done:
			c.Close()
			return
		default:
		}
		g.mu.Lock()
		m.c, m.mac, m.state, m.reason, m.stats = c, macString(c.Info().MAC), MemberMerged, "", nil
		if g.members[g.primary] == nil || g.members[g.primary].state == MemberLost {
			g.primary = m.id
		}
		g.verifyLocked()
		g.mu.Unlock()
		g.startMember(m)
		return
	}
}

func (g *Group) mergeLoop() {
	defer g.wg.Done()
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-g.done:
			return
		case x := <-g.in:
			g.mu.Lock()
			g.offerLocked(x)
			g.mu.Unlock()
		case <-tick.C:
			g.mu.Lock()
			if g.merging {
				now := time.Since(g.t0).Microseconds()
				g.merger.Expire(now)
				g.flushDueLocked(now)
			}
			g.mu.Unlock()
		}
	}
}

func (g *Group) offerLocked(x inPkt) {
	m := g.members[x.id]
	if m == nil || m.gen != x.gen || m.state != MemberMerged {
		return // not merged (config mismatch) or a stale forwarder
	}
	p := x.p
	if !g.merging { // a group of one: pass-through
		f := &Frame{PktMeta: p.PktMeta, Data: p.Data, GroupTsUS: float64(p.TsUS), Member: x.id, CfgGen: p.CfgGen, HostRx: p.HostRx,
			Copies: []diversity.Copy{{Member: x.id, RSSI: p.RSSI, TsUS: p.TsUS, Bad: false}}}
		g.sendLive(f)
		if g.final != nil {
			g.sendFinal(f)
		}
		return
	}
	host := p.HostRx.Sub(g.t0).Microseconds()
	res, err := g.merger.Offer(x.id, p.PktMeta, p.Data, host)
	if err != nil || res.Dup || res.Held {
		g.flushDueLocked(host)
		return
	}
	e := res.Entry
	g.merger.Bind(e, p)
	g.sendLive(&Frame{PktMeta: e.Meta, Data: e.Data, GroupTsUS: e.GroupTs, Member: x.id, CfgGen: p.CfgGen, HostRx: p.HostRx,
		Copies: []diversity.Copy{e.Copies[0]}})
	g.flushDueLocked(host)
}

func (g *Group) flushDueLocked(host int64) {
	for _, h := range g.merger.Due(host) {
		f := &Frame{PktMeta: h.Meta, Data: h.Data, GroupTsUS: h.GroupTs, Member: h.Member, Bad: true,
			Copies: []diversity.Copy{{Member: h.Member, RSSI: h.Meta.RSSI, TsUS: h.Meta.TsUS, Bad: true}}, HostRx: g.t0.Add(time.Duration(host) * time.Microsecond)}
		g.sendLive(f)
		if g.final != nil {
			g.sendFinal(f)
		}
	}
}

// onFinal runs under g.mu (called from Merger.Expire/Flush).
func (g *Group) onFinal(e *diversity.Entry) {
	if g.final == nil {
		return
	}
	f := &Frame{PktMeta: e.Meta, Data: e.Data, GroupTsUS: e.GroupTs, Member: e.First, Copies: append([]diversity.Copy(nil), e.Copies...)}
	if p, ok := e.Tag.(*Packet); ok {
		f.CfgGen, f.HostRx = p.CfgGen, p.HostRx
	}
	g.sendFinal(f)
}

func (g *Group) sendLive(f *Frame) {
	select {
	case g.frames <- f:
	default:
		g.liveDrops++
	}
}

func (g *Group) sendFinal(f *Frame) {
	select {
	case g.final <- f:
	default:
		g.finalDrops++
	}
}

// Frames is the live path: each air frame once, as soon as its first copy arrives.
func (g *Group) Frames() <-chan *Frame { return g.frames }

// Final is the archive path (GroupOptions.Archive): each frame with all its copies once
// its copy window closed. nil without Archive.
func (g *Group) Final() <-chan *Frame { return g.final }

// Events are every member's events plus group events, tagged with the member id.
func (g *Group) Events() <-chan GroupEvent { return g.events }

// Primary is the member whose config is the reference (the first one; the next one if it is lost).
func (g *Group) Primary() *Client {
	g.mu.Lock()
	defer g.mu.Unlock()
	if m := g.members[g.primary]; m != nil {
		return m.c
	}
	return nil
}

// Clients returns the members' clients that are not lost, by member id.
func (g *Group) Clients() map[int]*Client {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := map[int]*Client{}
	for _, m := range g.members {
		if m != nil && m.state != MemberLost {
			out[m.id] = m.c
		}
	}
	return out
}

// Members returns every member's status.
func (g *Group) Members() []MemberStatus { return g.Stats().Members }

// Stats returns per-member and group statistics.
func (g *Group) Stats() GroupStats {
	g.mu.Lock()
	defer g.mu.Unlock()
	s := GroupStats{Merge: g.merger.Summary(), Merging: g.merging, LiveDrops: g.liveDrops, FinalDrops: g.finalDrops}
	for _, m := range g.members {
		if m == nil {
			continue
		}
		ms := MemberStatus{ID: m.id, Path: m.path, MAC: m.mac, Label: g.o.Labels[m.mac], State: m.state.String(), Reason: m.reason,
			Primary: m.id == g.primary, Counters: m.c.Counters(), Device: m.stats}
		s.Members = append(s.Members, ms)
	}
	sort.Slice(s.Members, func(i, j int) bool { return s.Members[i].ID < s.Members[j].ID })
	return s
}

// Push applies a desired config to every member (doc 03 §7 order each) and verifies the
// readbacks: a member that differs from the primary is excluded (EventMemberExcluded).
// The fields set in d are remembered and re-pushed after a reset or rejoin. Returns the
// primary's readback; fails when the primary fails.
func (g *Group) Push(ctx context.Context, d DesiredConfig) (wire.Config, error) {
	g.mu.Lock()
	mergeDesired(&g.desired, d)
	full := g.desired
	type target struct {
		id int
		c  *Client
	}
	var ts []target
	for _, m := range g.members {
		if m != nil && m.state != MemberLost {
			ts = append(ts, target{m.id, m.c})
			m.c.SetDesired(&full)
		}
	}
	prim := g.primary
	g.mu.Unlock()

	cfgs := make([]wire.Config, len(ts))
	errs := make([]error, len(ts))
	var wg sync.WaitGroup
	for i, t := range ts {
		wg.Add(1)
		go func(i int, c *Client) { defer wg.Done(); cfgs[i], errs[i] = c.Push(ctx, d) }(i, t.c)
	}
	wg.Wait()
	var primCfg wire.Config
	var primErr error
	g.mu.Lock()
	for i, t := range ts {
		if t.id == prim {
			primCfg, primErr = cfgs[i], errs[i]
		} else if errs[i] != nil {
			if m := g.members[t.id]; m != nil && m.state != MemberLost {
				m.state, m.reason = MemberNotMerged, errs[i].Error()
				g.event(t.id, EventMemberExcluded{Reason: m.reason})
			}
		}
	}
	if primErr == nil {
		g.verifyLocked()
	}
	g.mu.Unlock()
	return primCfg, primErr
}

func mergeDesired(dst *DesiredConfig, d DesiredConfig) {
	if d.Country != nil {
		dst.Country = d.Country
	}
	if d.Channel != nil {
		dst.Channel = d.Channel
	}
	if d.BW != nil {
		dst.BW = d.BW
	}
	if d.Snaplen != nil {
		dst.Snaplen = d.Snaplen
	}
	if d.TypeMask != nil {
		dst.TypeMask = d.TypeMask
	}
	if d.CtrlMask != nil {
		dst.CtrlMask = d.CtrlMask
	}
	if d.Filter != nil {
		dst.Filter = d.Filter
	}
	if d.Capture != nil {
		dst.Capture = d.Capture
	}
}

// SetChannel sets the channel on every member.
func (g *Group) SetChannel(ctx context.Context, ch uint8, bw wire.Bandwidth) (wire.Config, error) {
	return g.Push(ctx, DesiredConfig{Channel: &ch, BW: &bw})
}

// SetCountry sets the country on every member.
func (g *Group) SetCountry(ctx context.Context, cc wire.Country) (wire.Config, error) {
	return g.Push(ctx, DesiredConfig{Country: &cc})
}

// SetSnaplen sets the snaplen on every member (dedup needs the same snaplen everywhere).
func (g *Group) SetSnaplen(ctx context.Context, n uint16) (wire.Config, error) {
	return g.Push(ctx, DesiredConfig{Snaplen: &n})
}

// SetFilter sets the filter on every member.
func (g *Group) SetFilter(ctx context.Context, s filter.Set) (wire.Config, error) {
	return g.Push(ctx, DesiredConfig{Filter: &s})
}

// SetDriverMask sets the driver masks on every member.
func (g *Group) SetDriverMask(ctx context.Context, typ, ctrl uint32) (wire.Config, error) {
	return g.Push(ctx, DesiredConfig{TypeMask: &typ, CtrlMask: &ctrl})
}

// Capture starts or stops capture on every member.
func (g *Group) Capture(ctx context.Context, on bool) (wire.Config, error) {
	return g.Push(ctx, DesiredConfig{Capture: &on})
}

// Close stops capture on every member (best effort), closes them and flushes the merge
// (remaining frames go to Final). Frames, Final and Events are closed afterwards.
func (g *Group) Close() error {
	var errs []error
	g.once.Do(func() {
		close(g.done)
		g.mu.Lock()
		var cs []*Client
		for _, m := range g.members {
			if m != nil && m.state != MemberLost {
				cs = append(cs, m.c)
			}
		}
		g.mu.Unlock()
		var wg sync.WaitGroup
		var emu sync.Mutex
		for _, c := range cs {
			wg.Add(1)
			go func(c *Client) {
				defer wg.Done()
				if err := c.Close(); err != nil && !errors.Is(err, ErrClosed) {
					emu.Lock()
					errs = append(errs, err)
					emu.Unlock()
				}
			}(c)
		}
		wg.Wait()
		g.wg.Wait()
		g.mu.Lock()
		if g.merging {
			g.merger.Flush()
			g.flushDueLocked(int64(^uint64(0) >> 1))
		}
		g.mu.Unlock()
		close(g.frames)
		if g.final != nil {
			close(g.final)
		}
		close(g.events)
	})
	return errors.Join(errs...)
}
