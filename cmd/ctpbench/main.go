// Command ctpbench is the CTP test bench (docs/06): non-interactive,
// bounded runtime, one JSON document on stdout, stable exit codes.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"runtime"
	"runtime/debug"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/hjkoskel/capturetonkeli/ctp"
	"github.com/hjkoskel/capturetonkeli/ctp/fwver"
	"github.com/hjkoskel/capturetonkeli/ctp/serialport"
	"github.com/hjkoskel/capturetonkeli/ctp/sim"
	"github.com/hjkoskel/capturetonkeli/ctp/wire"
)

// Exit codes (docs/06 §3).
const (
	exitOK         = 0
	exitTestFail   = 1
	exitUsage      = 2
	exitNoDevice   = 3
	exitPortBusy   = 4
	exitProtocol   = 5
	exitDeviceErr  = 6
	exitTimeout    = 7
	exitInternal   = 8
	exitFwMismatch = 9
	exitFlashFail  = 10
)

// benchErr is an error with a stable code, hint and exit code.
type benchErr struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Hint    string `json:"hint,omitempty"`
	exit    int
	extra   map[string]any
}

func (e *benchErr) Error() string { return e.Code + ": " + e.Message }

func errf(exit int, code, hint, format string, a ...any) *benchErr {
	return &benchErr{Code: code, Message: fmt.Sprintf(format, a...), Hint: hint, exit: exit}
}

// testFailure marks a command result as "ran fine, checks failed" (exit 1).
type testFailure struct{ result any }

func (testFailure) Error() string { return "checks failed" }

type globals struct {
	port        string
	json, jsonl bool
	text        bool
	timeout     time.Duration
	outDir      string
	seed        int64
	simFaults   string
	verbose     int
	trace       string
	allowFw     bool
	fwManifest  string
}

type env struct {
	g      *globals
	args   []string
	fs     *flag.FlagSet
	stderr io.Writer
	simDev *sim.Device
}

type command struct {
	name    string
	summary string
	example string
	run     func(ctx context.Context, e *env) (any, error)
	flags   func(fs *flag.FlagSet)
}

var commands []command

func register(c command) { commands = append(commands, c) }

func registerGlobals(fs *flag.FlagSet, g *globals) {
	fs.StringVar(&g.port, "port", "auto", "dongle path | auto | sim")
	fs.BoolVar(&g.json, "json", false, "one JSON document on stdout")
	fs.BoolVar(&g.jsonl, "jsonl", false, "JSON lines (streaming commands)")
	fs.BoolVar(&g.text, "text", false, "human-readable output")
	fs.DurationVar(&g.timeout, "timeout", 30*time.Second, "hard cap for the whole command")
	fs.StringVar(&g.outDir, "out-dir", "", "artifact directory (default ./bench-out/<run-id>)")
	fs.Int64Var(&g.seed, "seed", 0, "simulator/fuzz seed (0 = random, reported)")
	fs.StringVar(&g.simFaults, "sim-faults", "", "with --port sim: drop=0.001,corrupt=0.0005,garbage,reboot@20s")
	fs.Func("v", "verbose (repeat for more)", func(string) error { g.verbose++; return nil })
	fs.StringVar(&g.trace, "trace", "", "write TX/RX frames as JSONL")
	fs.BoolVar(&g.allowFw, "allow-fw-mismatch", false, "connect even if firmware MAJOR.MINOR differs")
	fs.StringVar(&g.fwManifest, "fw-manifest", "", "manifest defining the expected firmware version")
}

// parseInterleaved parses flags that may appear between positional args.
func parseInterleaved(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		args = fs.Args()
		if len(args) == 0 {
			return pos, nil
		}
		if args[0] == "--" {
			return append(pos, args[1:]...), nil
		}
		pos = append(pos, args[0])
		args = args[1:]
	}
}

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }

func run(argv []string, stdout, stderr io.Writer) (code int) {
	g := &globals{}
	// find the command name (first arg that isn't a global flag or its value)
	pre := flag.NewFlagSet("ctpbench", flag.ContinueOnError)
	pre.SetOutput(io.Discard)
	registerGlobals(pre, g)
	pos, err := parseInterleaved(pre, argv)
	if err != nil && !errors.Is(err, flag.ErrHelp) {
		// unknown flags belong to a subcommand; locate the command by name instead
		pos = nil
		for _, a := range argv {
			if !strings.HasPrefix(a, "-") {
				pos = append(pos, a)
			}
		}
	}
	if len(pos) == 0 || pos[0] == "help" && len(pos) == 1 {
		return usage(stdout, g, argv)
	}
	name, cmd := matchCommand(pos)
	for i := 1; cmd == nil && i < len(pos); i++ {
		name, cmd = matchCommand(pos[i:]) // skip values of flags we could not parse
	}
	if cmd == nil {
		fmt.Fprintf(stderr, "ctpbench: unknown command %q (try `ctpbench help`)\n", strings.Join(pos, " "))
		return exitUsage
	}
	// re-parse everything with the command's flag set
	g = &globals{}
	fs := flag.NewFlagSet("ctpbench "+name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	registerGlobals(fs, g)
	if cmd.flags != nil {
		cmd.flags(fs)
	}
	all, err := parseInterleaved(fs, argv)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK
		}
		return exitUsage
	}
	// strip the command words from the positionals
	words := len(strings.Fields(name))
	args := all
	for i := 0; i < words && len(args) > 0; i++ {
		args = args[1:]
	}
	if !g.json && !g.text && !g.jsonl {
		if fi, err := os.Stdout.Stat(); err == nil && fi.Mode()&os.ModeCharDevice == 0 {
			g.json = true
		} else {
			g.text = true
		}
	}
	if g.seed == 0 {
		g.seed = time.Now().UnixNano() & 0x7fffffff
	}
	ctx, cancel := context.WithTimeout(context.Background(), g.timeout)
	defer cancel()
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	e := &env{g: g, args: args, fs: fs, stderr: stderr}
	defer func() {
		if x := recover(); x != nil {
			_ = os.MkdirAll(e.outDir(), 0o755)
			_ = os.WriteFile(e.outDir()+"/crash.txt", []byte(fmt.Sprintf("%v\n%s", x, debug.Stack())), 0o644)
			code = emit(stdout, g, nil, errf(exitInternal, "INTERNAL", "report a bug; stack in "+e.outDir()+"/crash.txt", "%v", x))
		}
	}()
	res, err := cmd.run(ctx, e)
	if err != nil && ctx.Err() != nil && !isBench(err) {
		err = errf(exitTimeout, "TIMEOUT", "raise --timeout or check the device", "%v", err)
	}
	return emit(stdout, g, res, err)
}

func isBench(err error) bool {
	var b *benchErr
	var tf testFailure
	return errors.As(err, &b) || errors.As(err, &tf)
}

func matchCommand(pos []string) (string, *command) {
	// longest match of "word word" names
	var best *command
	bestN := 0
	for i := range commands {
		c := &commands[i]
		w := strings.Fields(c.name)
		if len(w) > len(pos) || len(w) <= bestN {
			continue
		}
		ok := true
		for k := range w {
			if w[k] != pos[k] {
				ok = false
				break
			}
		}
		if ok {
			best, bestN = c, len(w)
		}
	}
	if best == nil {
		return "", nil
	}
	return best.name, best
}

func emit(stdout io.Writer, g *globals, res any, err error) int {
	code := exitOK
	var out any = res
	if err != nil {
		var tf testFailure
		var be *benchErr
		switch {
		case errors.As(err, &tf):
			out, code = tf.result, exitTestFail
		case errors.As(err, &be):
			m := map[string]any{"error": be}
			for k, v := range be.extra {
				m[k] = v
			}
			out, code = m, be.exit
		default:
			out, code = map[string]any{"error": classify(err)}, classify(err).exit
		}
	}
	if out == nil {
		out = map[string]any{"ok": code == 0}
	}
	if g.text {
		printText(stdout, out)
	} else {
		b, _ := json.MarshalIndent(out, "", "  ")
		fmt.Fprintln(stdout, string(b))
	}
	return code
}

// classify maps library errors to bench errors.
func classify(err error) *benchErr {
	var de *ctp.DeviceError
	var pe *ctp.ProtocolVersionError
	var fe *ctp.FirmwareMismatchError
	switch {
	case errors.As(err, &fe):
		b := errf(exitFwMismatch, "FW_MISMATCH", "run `ctpbench fw update` or pass --allow-fw-mismatch", "%v", err)
		b.extra = map[string]any{"device": fe.Device.String(), "expected": fe.Host.String(), "device_build_id": fmt.Sprintf("%x", fe.DeviceBuildID)}
		return b
	case errors.As(err, &pe):
		return errf(exitProtocol, "PROTO_MISMATCH", "ctpbench fw update", "%v", err)
	case errors.As(err, &de):
		return errf(exitDeviceErr, "DEVICE_ERROR", "see err_code / esp_err", "%v", err)
	case errors.Is(err, serialport.ErrPortBusy):
		return errf(exitPortBusy, "PORT_BUSY", "stop idf.py monitor or other tools holding the port (ctpbench doctor)", "%v", err)
	case errors.Is(err, os.ErrPermission):
		return errf(exitPortBusy, "PERMISSION", "add your user to the dialout/uucp group or install a udev rule", "%v", err)
	case errors.Is(err, os.ErrNotExist):
		return errf(exitNoDevice, "NO_DEVICE", "check --port; `ctpbench list` shows ports", "%v", err)
	case errors.Is(err, ctp.ErrTimeout), errors.Is(err, ctp.ErrLinkDead), errors.Is(err, context.DeadlineExceeded):
		return errf(exitTimeout, "TIMEOUT", "device not answering: check firmware is running (`ctpbench doctor`)", "%v", err)
	}
	return errf(exitInternal, "INTERNAL", "", "%v", err)
}

func printText(w io.Writer, v any) {
	b, _ := json.Marshal(v)
	var m any
	_ = json.Unmarshal(b, &m)
	printTree(w, "", m)
}

func printTree(w io.Writer, indent string, v any) {
	switch t := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			switch t[k].(type) {
			case map[string]any, []any:
				fmt.Fprintf(w, "%s%s:\n", indent, k)
				printTree(w, indent+"  ", t[k])
			default:
				fmt.Fprintf(w, "%s%s: %v\n", indent, k, t[k])
			}
		}
	case []any:
		for _, x := range t {
			switch x.(type) {
			case map[string]any:
				fmt.Fprintf(w, "%s-\n", indent)
				printTree(w, indent+"  ", x)
			default:
				fmt.Fprintf(w, "%s- %v\n", indent, x)
			}
		}
	default:
		fmt.Fprintf(w, "%s%v\n", indent, t)
	}
}

func usage(stdout io.Writer, g *globals, argv []string) int {
	type cmdInfo struct {
		Name    string `json:"name"`
		Summary string `json:"summary"`
		Example string `json:"example,omitempty"`
	}
	var list []cmdInfo
	for _, c := range commands {
		list = append(list, cmdInfo{c.name, c.summary, c.example})
	}
	asJSON := g.json
	for _, a := range argv {
		if a == "--json" || a == "-json" {
			asJSON = true
		}
	}
	if asJSON {
		b, _ := json.MarshalIndent(map[string]any{"commands": list, "exit_codes": exitCodeDoc}, "", "  ")
		fmt.Fprintln(stdout, string(b))
		return exitOK
	}
	fmt.Fprintln(stdout, "ctpbench – CTP test bench (docs/06). Global flags: --port PATH|auto|sim --json --text --timeout --out-dir --seed -v")
	for _, c := range list {
		fmt.Fprintf(stdout, "  %-22s %s\n", c.Name, c.Summary)
		if c.Example != "" {
			fmt.Fprintf(stdout, "  %-22s   e.g. %s\n", "", c.Example)
		}
	}
	return exitOK
}

var exitCodeDoc = map[int]string{0: "success", 1: "test failures", 2: "usage", 3: "no device", 4: "port busy/permission",
	5: "protocol error", 6: "device RSP_ERR", 7: "timeout/link dead", 8: "internal error", 9: "firmware mismatch", 10: "flashing failed"}

// ---- helpers shared by commands ----------------------------------------------

func (e *env) outDir() string {
	if e.g.outDir == "" {
		e.g.outDir = "bench-out/" + time.Now().Format("2006-01-02T15-04-05")
	}
	return e.g.outDir
}

func (e *env) logf(level int, format string, a ...any) {
	if e.g.verbose >= level {
		fmt.Fprintf(e.stderr, format+"\n", a...)
	}
}

func (e *env) sim() *sim.Device {
	if e.simDev == nil {
		o := sim.SimOptions{Seed: e.g.seed}
		applyFaults(&o, e.g.simFaults)
		e.simDev = sim.NewDevice(o)
	}
	return e.simDev
}

func applyFaults(o *sim.SimOptions, spec string) {
	for _, f := range strings.Split(spec, ",") {
		k, v, _ := strings.Cut(strings.TrimSpace(f), "=")
		switch {
		case k == "drop":
			fmt.Sscan(v, &o.Faults.DropRate)
		case k == "corrupt":
			fmt.Sscan(v, &o.Faults.CorruptRate)
		case k == "garbage":
			o.Faults.BootGarbage = true
		case strings.HasPrefix(k, "reboot@"):
			o.Faults.RebootAt, _ = time.ParseDuration(strings.TrimPrefix(k, "reboot@"))
		case k == "safe":
			o.SafeMode = true
		}
	}
}

// openRaw opens the transport selected by --port.
func (e *env) openRaw(ctx context.Context) (io.ReadWriteCloser, error) {
	switch p := e.g.port; p {
	case "sim":
		return e.sim().Attach(), nil
	case "", "auto":
		ds, err := serialport.FindDongles()
		if err != nil {
			return nil, errf(exitNoDevice, "NO_DEVICE", "pass --port PATH (or --port sim)", "%v", err)
		}
		if len(ds) != 1 {
			return nil, errf(exitNoDevice, "NO_DEVICE", "pass --port PATH (or --port sim)", "%d dongles found, need exactly 1", len(ds))
		}
		e.g.port = ds[0].Path
		return e.openRaw(ctx)
	default:
		sp, err := serialport.Open(p)
		if err != nil {
			return nil, err
		}
		serialport.ApplySafeOpenSignals(sp)
		return sp, nil
	}
}

func (e *env) expectedFw() (*fwver.Version, error) {
	if e.g.fwManifest == "" {
		return nil, nil // no bundled firmware yet (fw/ is produced by `make release`)
	}
	dir, file := splitPath(e.g.fwManifest)
	m, err := fwver.LoadManifest(os.DirFS(dir), file)
	if err != nil {
		return nil, errf(exitUsage, "BAD_MANIFEST", "check --fw-manifest", "%v", err)
	}
	v, _ := fwver.Parse(m.Version)
	return &v, nil
}

func splitPath(p string) (string, string) {
	i := strings.LastIndex(p, "/")
	if i < 0 {
		return ".", p
	}
	return p[:i], p[i+1:]
}

// client opens a session with the doc 03 §11 version check.
func (e *env) client(ctx context.Context, o ctp.Options) (*ctp.Client, error) {
	rw, err := e.openRaw(ctx)
	if err != nil {
		return nil, err
	}
	hv, err := e.expectedFw()
	if err != nil {
		rw.Close()
		return nil, err
	}
	o.HostFwVersion = hv
	if e.g.allowFw {
		o.VersionPolicy = ctp.AllowMismatch
	}
	if e.g.verbose >= 2 {
		o.Trace = func(dir string, h wire.Header, body []byte) {
			fmt.Fprintf(e.stderr, "%s %-10s seq=%d req=%d gen=%d % x\n", dir, wire.TypeName(h.Type), h.Seq, h.ReqID, h.CfgGen, body)
		}
	}
	c, err := ctp.New(ctx, rw, o)
	if err != nil {
		return nil, err
	}
	return c, nil
}

func hostInfo() map[string]string {
	v := "dev"
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" {
		v = bi.Main.Version
	}
	return map[string]string{"os": runtime.GOOS, "arch": runtime.GOARCH, "ctpbench": v, "go": runtime.Version()}
}
