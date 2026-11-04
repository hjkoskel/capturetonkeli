// Package conformance is the CTP v1 protocol conformance suite (docs/06 §4).
// The same cases run in `go test` against the simulator (proving the suite)
// and in `ctpbench conformance` against firmware (PTY host build or dongle).
package conformance

import (
	"context"
	"fmt"
	"io"
	"regexp"
	"time"

	"github.com/hjkoskel/capturetonkeli/ctp/wire"
)

// Status of a case.
type Status string

const (
	Pass Status = "pass"
	Fail Status = "fail"
	Skip Status = "skip"
)

// Result of one case.
type Result struct {
	ID         string `json:"id"`
	Title      string `json:"title"`
	Spec       string `json:"spec"`
	Status     Status `json:"status"`
	DurationMS int64  `json:"duration_ms"`
	Expected   string `json:"expected,omitempty"`
	Actual     string `json:"actual,omitempty"`
	Hint       string `json:"hint,omitempty"`
	Trace      string `json:"trace,omitempty"`
}

// Env is what a case needs from the bench.
type Env struct {
	// Open returns a fresh transport to the device (re-openable).
	Open func(ctx context.Context) (io.ReadWriteCloser, error)
	// Trace, if set, is called per case to obtain a frame tracer (may return nil).
	Trace func(caseID string) func(dir string, h wire.Header, body []byte)
	// Injector is true when an injector dongle is available (CTP-FLT-RF).
	Injector bool
}

// T is the per-case context.
type T struct {
	ctx   context.Context
	env   Env
	id    string
	links []*Link
	res   *Result
	info  wire.DeviceInfo
}

// failure carries expected/actual/hint out of a case.
type failure struct{ expected, actual, hint string }

type skip struct{ why string }

// Fatalf ends the case as failed.
func (t *T) Fatalf(expected, hint, actualFmt string, a ...any) {
	panic(failure{expected: expected, actual: fmt.Sprintf(actualFmt, a...), hint: hint})
}

// Skip ends the case as skipped.
func (t *T) Skip(why string) { panic(skip{why}) }

// Ctx returns the case context.
func (t *T) Ctx() context.Context { return t.ctx }

// Open opens a link without HELLO.
func (t *T) Open() *Link {
	rw, err := t.env.Open(t.ctx)
	if err != nil {
		t.Fatalf("device can be opened", "check --port / permissions", "%v", err)
	}
	var tr func(string, wire.Header, []byte)
	if t.env.Trace != nil {
		tr = t.env.Trace(t.id)
	}
	l := newLink(rw, tr)
	t.links = append(t.links, l)
	return l
}

// Session opens a link, says HELLO and resets the device to a known config.
func (t *T) Session() *Link {
	l := t.Open()
	info, ok := l.Hello(t.ctx)
	if !ok {
		t.Fatalf("HELLO answered", "link task / HELLO handler (proto.c)", "no valid HELLO response")
	}
	t.info = info
	t.reset(l)
	l.Drain()
	return l
}

// Info returns the HELLO info of the last Session.
func (t *T) Info() wire.DeviceInfo { return t.info }

func (t *T) reset(l *Link) {
	steps := []struct {
		typ  uint8
		body []byte
	}{
		{wire.TypeCapture, []byte{0}},
		{wire.TypeSetCountry, wire.MakeCountry("01", 0, 0, 0).AppendBinary(nil)},
		{wire.TypeSetChannel, []byte{1, 0}},
		{wire.TypeSetSnaplen, []byte{0, 0}},
		{wire.TypeSetFilter, make([]byte, 8)},
		{wire.TypeSetDriverMask, wire.SetDriverMask{TypeMask: wire.DefaultTypeMask, CtrlMask: wire.DefaultCtrlMask}.AppendBinary(nil)},
	}
	for _, s := range steps {
		if _, code, ok := l.Config(t.ctx, s.typ, s.body); !ok || code != 0 {
			t.Fatalf("reset step "+wire.TypeName(s.typ)+" succeeds", "config.c apply path", "ok=%v code=%v", ok, code)
		}
	}
}

// Case is one conformance test.
type Case struct {
	ID    string
	Title string
	Spec  string
	Hint  string // default hint on failure
	Run   func(t *T)
}

// Cases returns the full suite in run order.
func Cases() []Case { return cases }

// Options for Run.
type Options struct {
	Run, Skip   *regexp.Regexp
	CaseTimeout time.Duration
	OnResult    func(Result)
}

// Run executes the selected cases sequentially.
func Run(ctx context.Context, env Env, o Options) []Result {
	if o.CaseTimeout == 0 {
		o.CaseTimeout = 20 * time.Second
	}
	var out []Result
	for _, c := range cases {
		if o.Run != nil && !o.Run.MatchString(c.ID) {
			continue
		}
		r := Result{ID: c.ID, Title: c.Title, Spec: c.Spec}
		if o.Skip != nil && o.Skip.MatchString(c.ID) {
			r.Status, r.Actual = Skip, "skipped by --skip"
		} else {
			r = runOne(ctx, env, c, o.CaseTimeout)
		}
		if o.OnResult != nil {
			o.OnResult(r)
		}
		out = append(out, r)
		if ctx.Err() != nil {
			break
		}
	}
	return out
}

func runOne(parent context.Context, env Env, c Case, timeout time.Duration) (r Result) {
	r = Result{ID: c.ID, Title: c.Title, Spec: c.Spec, Status: Pass}
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	t := &T{ctx: ctx, env: env, id: c.ID, res: &r}
	t0 := time.Now()
	defer func() {
		r.DurationMS = time.Since(t0).Milliseconds()
		if x := recover(); x != nil {
			switch v := x.(type) {
			case failure:
				r.Status, r.Expected, r.Actual, r.Hint = Fail, v.expected, v.actual, v.hint
				if r.Hint == "" {
					r.Hint = c.Hint
				}
			case skip:
				r.Status, r.Actual = Skip, v.why
			default:
				r.Status, r.Actual, r.Hint = Fail, fmt.Sprintf("panic: %v", v), "bug in the conformance case"
			}
		}
		// best-effort cleanup: capture off on the last link, then close all
		for i := len(t.links) - 1; i >= 0; i-- {
			l := t.links[i]
			if i == len(t.links)-1 {
				cctx, cc := context.WithTimeout(context.Background(), 400*time.Millisecond)
				_, _, _ = l.Config(cctx, wire.TypeCapture, []byte{0})
				cc()
			}
			_ = l.Close()
		}
	}()
	c.Run(t)
	return r
}
