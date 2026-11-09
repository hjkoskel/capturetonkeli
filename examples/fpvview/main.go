// fpvview shows and records the video of an OpenHD drone (docs/13) or an
// hx-esp32-cam-fpv air unit (docs/12), received passively with one or more
// capturetonkeli dongles. It is an example of the Go library (docs/05 §11):
//
//	dongle(s) ──ctp.Group──▶ 802.11 frames ──▶ openhd.Receiver ─RTP─▶ video.Depacketizer ─▶ ffmpeg ─▶ window
//	                                       └─▶ hxfpv.Receiver ─JPEG─▶ image/jpeg ─────────────────▶ window
//	                                                              access units / JPEGs ─▶ recording
//
// The window uses raylib: Alt+Enter (or F11) toggles full screen, O the info overlay,
// S saves a screenshot, Esc or Q quits.
//
// Examples:
//
//	fpvview -system openhd -country FI -channel 140 -record flight.mp4
//	fpvview -system hx -port /dev/ttyACM0,/dev/ttyACM1 -channel 7 -record cam.mjpeg
//	fpvview -system openhd -demo          (simulated drone, no dongle; needs ffmpeg with libx264)
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"syscall"
	"time"
)

func init() { runtime.LockOSThread() } // raylib must run on the main OS thread

type config struct {
	system     string
	ports      string
	demo       bool
	demoLoss   float64
	demoCodec  string
	pcap       string
	country    string
	channel    int
	freq       int
	bw         int
	phrase     string
	keyFile    string
	unverified bool
	cam        int
	unit       int
	hxKeep     int
	record     string
	flip       string
	width      int
	height     int
	fullscreen bool
	osd        bool
	duration   time.Duration
	screenshot string
	ffmpeg     string
	verbose    bool
}

func parseFlags() (*config, error) {
	c := &config{}
	flag.StringVar(&c.system, "system", "openhd", "video system: openhd or hx (hx-esp32-cam-fpv)")
	flag.StringVar(&c.ports, "port", "auto", "dongle serial port(s), comma separated for receive diversity (\"auto\" = every dongle found)")
	flag.BoolVar(&c.demo, "demo", false, "simulated air unit instead of a dongle (OpenHD demo needs ffmpeg with libx264/libx265)")
	flag.Float64Var(&c.demoLoss, "demo-loss", 0.03, "demo: fraction of frames lost on the air")
	flag.StringVar(&c.demoCodec, "demo-codec", "h264", "demo (OpenHD): h264 or h265")
	flag.StringVar(&c.pcap, "pcap", "", "replay a pcapng capture (web UI or ctpbench) instead of a dongle")
	flag.StringVar(&c.country, "country", "", "regulatory country for the dongle, e.g. FI (empty = keep the dongle's)")
	flag.IntVar(&c.channel, "channel", 0, "Wi-Fi channel (default: OpenHD 149, hx 7)")
	flag.IntVar(&c.freq, "freq", 0, "frequency in MHz instead of -channel, e.g. 5700")
	flag.IntVar(&c.bw, "bw", 20, "channel width: 20 or 40 (OpenHD's HT40 side is chosen from the channel)")
	flag.StringVar(&c.phrase, "bind-phrase", "openhd", "OpenHD bind phrase")
	flag.StringVar(&c.keyFile, "key-file", "", "OpenHD txrx.key (128 bytes) instead of the bind phrase")
	flag.BoolVar(&c.unverified, "unverified", false, "OpenHD: show video even without the right key (not authenticated)")
	flag.IntVar(&c.cam, "cam", 0, "OpenHD camera: 0 primary, 1 secondary")
	flag.IntVar(&c.unit, "unit", 0, "air unit to show when several are in range (OpenHD: unit number in order of discovery; hx: device id); 0 = the first one")
	flag.IntVar(&c.hxKeep, "hx-keep", 8, "hx: dongle keeps packet indexes below 8 or 16 of each FEC block (less USB load); 0 = all")
	flag.StringVar(&c.record, "record", "", "record to a file: OpenHD .mp4 (or .h264/.h265 raw), hx .mjpeg (or a directory for numbered .jpg)")
	flag.StringVar(&c.flip, "flip", "none", "picture orientation: none, rot180, mirror, vflip")
	flag.IntVar(&c.width, "width", 1280, "window width")
	flag.IntVar(&c.height, "height", 720, "window height")
	flag.BoolVar(&c.fullscreen, "fullscreen", false, "start in full screen")
	flag.BoolVar(&c.osd, "osd", true, "show the info overlay (key O toggles)")
	flag.DurationVar(&c.duration, "duration", 0, "quit after this long (0 = run until closed)")
	flag.StringVar(&c.screenshot, "screenshot", "", "save a PNG screenshot of the window when quitting")
	flag.StringVar(&c.ffmpeg, "ffmpeg", "ffmpeg", "ffmpeg binary (OpenHD video decoding and the demo encoder)")
	flag.BoolVar(&c.verbose, "v", false, "log sessions, member changes and decoder messages")
	flag.Usage = func() {
		fmt.Fprintf(flag.CommandLine.Output(), "Usage: fpvview [flags]\n\nShows and records OpenHD or hx-esp32-cam-fpv video received with capturetonkeli dongles.\nKeys: Alt+Enter/F11 full screen, O overlay, S screenshot, Esc/Q quit.\n\nFlags:\n")
		flag.PrintDefaults()
	}
	flag.Parse()
	if flag.NArg() > 0 {
		return nil, fmt.Errorf("unexpected argument %q (flags only)", flag.Arg(0))
	}
	switch c.system {
	case "openhd", "hx":
	default:
		return nil, fmt.Errorf("-system must be openhd or hx, not %q", c.system)
	}
	switch c.flip {
	case "none", "rot180", "mirror", "vflip":
	default:
		return nil, fmt.Errorf("-flip must be none, rot180, mirror or vflip")
	}
	if c.bw != 20 && c.bw != 40 {
		return nil, errors.New("-bw must be 20 or 40")
	}
	if c.cam != 0 && c.cam != 1 {
		return nil, errors.New("-cam must be 0 or 1")
	}
	if c.demo && c.pcap != "" {
		return nil, errors.New("use either -demo or -pcap")
	}
	if c.demoCodec != "h264" && c.demoCodec != "h265" {
		return nil, errors.New("-demo-codec must be h264 or h265")
	}
	if c.freq != 0 {
		ch := freqToChannel(c.freq)
		if ch == 0 {
			return nil, fmt.Errorf("-freq %d MHz is not a Wi-Fi channel centre", c.freq)
		}
		c.channel = ch
	}
	if c.channel == 0 {
		c.channel = map[string]int{"openhd": 149, "hx": 7}[c.system]
	}
	if c.record != "" && c.system == "openhd" {
		ext := strings.ToLower(c.record[strings.LastIndexByte(c.record, '.')+1:])
		if ext != "mp4" && ext != "h264" && ext != "h265" && ext != "hevc" {
			return nil, errors.New("OpenHD recordings are .mp4, .h264 or .h265")
		}
	}
	return c, nil
}

func freqToChannel(mhz int) int {
	switch {
	case mhz == 2484:
		return 14
	case mhz >= 2412 && mhz <= 2472 && (mhz-2407)%5 == 0:
		return (mhz - 2407) / 5
	case mhz >= 5160 && mhz <= 5885 && (mhz-5000)%5 == 0:
		return (mhz - 5000) / 5
	}
	return 0
}

func main() {
	log.SetFlags(log.Ltime)
	cfg, err := parseFlags()
	if err != nil {
		fmt.Fprintln(os.Stderr, "fpvview:", err)
		os.Exit(2)
	}
	if err := run(cfg); err != nil {
		fmt.Fprintln(os.Stderr, "fpvview:", err)
		os.Exit(1)
	}
}

func run(cfg *config) error {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if cfg.duration > 0 {
		var c2 context.CancelFunc
		ctx, c2 = context.WithTimeout(ctx, cfg.duration)
		defer c2()
	}
	sh := newShared()

	src, err := openSource(ctx, cfg, sh)
	if err != nil {
		return err
	}
	var p pipeline
	if cfg.system == "openhd" {
		p, err = newOpenHD(cfg, sh, holdFor(src)) // several dongles: wait for late copies
	} else {
		p, err = newHX(cfg, sh)
	}
	if err != nil {
		src.close()
		return err
	}
	sh.setSource(src.describe())

	done := make(chan struct{})
	go func() {
		defer close(done)
		runPipeline(ctx, src.frames(), p, sh)
	}()

	winErr := window(ctx, cfg, sh) // returns when the window is closed or ctx ends
	cancel()
	src.close()
	<-done
	for _, m := range p.close() {
		log.Print(m)
	}
	return winErr
}

// runPipeline feeds the receiver from the source and refreshes the overlay twice a second.
func runPipeline(ctx context.Context, frames <-chan rxFrame, p pipeline, sh *shared) {
	tick := time.NewTicker(500 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case f, ok := <-frames:
			if !ok {
				sh.setNote("source ended")
				<-ctx.Done()
				return
			}
			p.push(f)
		case <-tick.C:
			sh.setStatus(p.status())
		}
	}
}
