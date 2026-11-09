package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/hjkoskel/capturetonkeli/ctp"
	"github.com/hjkoskel/capturetonkeli/ctp/diversity"
	"github.com/hjkoskel/capturetonkeli/ctp/filter"
	"github.com/hjkoskel/capturetonkeli/ctp/hxfpv"
	"github.com/hjkoskel/capturetonkeli/ctp/openhd"
	"github.com/hjkoskel/capturetonkeli/ctp/pcapng"
	"github.com/hjkoskel/capturetonkeli/ctp/serialport"
	"github.com/hjkoskel/capturetonkeli/ctp/wire"
)

// rxFrame is one captured 802.11 frame (without FCS) and its radio metadata.
type rxFrame struct {
	data    []byte
	rssi    int8
	hasRSSI bool
	channel uint8
	copies  int // dongles that heard it (group); 0 = unknown
}

type source interface {
	frames() <-chan rxFrame
	describe() string
	close()
}

func openSource(ctx context.Context, cfg *config, sh *shared) (source, error) {
	switch {
	case cfg.demo && cfg.system == "openhd":
		return newOpenHDDemo(ctx, cfg)
	case cfg.demo:
		return newHXDemo(ctx, cfg), nil
	case cfg.pcap != "":
		return newPcapSource(ctx, cfg.pcap)
	}
	return newDongles(ctx, cfg, sh)
}

// ---- dongles (one, or several as a receive-diversity group) ----

type dongles struct {
	g   *ctp.Group
	out chan rxFrame
	txt string
}

func newDongles(ctx context.Context, cfg *config, sh *shared) (*dongles, error) {
	ports := strings.Split(cfg.ports, ",")
	if cfg.ports == "" || cfg.ports == "auto" {
		found, err := serialport.FindDongles()
		if err != nil {
			return nil, err
		}
		if len(found) == 0 {
			return nil, fmt.Errorf("no capturetonkeli dongle found (pass -port, or try -demo)")
		}
		ports = nil
		for _, d := range found {
			ports = append(ports, d.Path)
		}
		if len(ports) > ctp.MaxDongles {
			ports = ports[:ctp.MaxDongles]
		}
	}
	ch := uint8(cfg.channel)
	bw := wire.BW20
	if cfg.bw == 40 {
		bw = wire.BW40Minus
		if cfg.system != "openhd" || openhd.HT40Plus(cfg.channel) {
			bw = wire.BW40Plus
		}
	}
	var flt filter.Set
	if cfg.system == "openhd" {
		flt = openhd.CaptureFilter(false)
	} else {
		flt = hxfpv.CaptureFilter(cfg.hxKeep)
	}
	snap, on := uint16(0), true
	d := &ctp.DesiredConfig{Channel: &ch, BW: &bw, Snaplen: &snap, Filter: &flt, Capture: &on}
	if cfg.country != "" {
		c := wire.MakeCountry(strings.ToUpper(cfg.country), wire.CountryValidated, 0, 0)
		d.Country = &c
	}
	octx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	g, err := ctp.OpenGroup(octx, ports, ctp.GroupOptions{Options: ctp.Options{Desired: d}})
	if err != nil {
		return nil, fmt.Errorf("%w (is the channel allowed in the dongle's country? try -country)", err)
	}
	s := &dongles{g: g, out: make(chan rxFrame, 4096)}
	s.txt = fmt.Sprintf("%d dongle(s) · ch %d", len(ports), ch)
	if len(ports) == 1 {
		s.txt = fmt.Sprintf("%s · ch %d", ports[0], ch)
	}
	go func() {
		defer close(s.out)
		for f := range g.Frames() {
			select {
			case s.out <- rxFrame{data: f.Data, rssi: f.RSSI, hasRSSI: true, channel: f.Channel, copies: len(f.Copies)}:
			case <-ctx.Done():
				return
			}
		}
	}()
	go func() {
		for e := range g.Events() {
			switch ev := e.Event.(type) {
			case ctp.EventMemberLost:
				sh.setNote(fmt.Sprintf("dongle %d lost: %v", e.Member+1, ev.Err))
			case ctp.EventMemberJoined:
				sh.setNote(fmt.Sprintf("dongle %d (%s) joined", e.Member+1, ev.MAC))
			case ctp.EventMemberExcluded:
				sh.setNote(fmt.Sprintf("dongle %d not merged: %s", e.Member+1, ev.Reason))
			case ctp.EventLog:
				if cfg.verbose {
					log.Printf("dongle %d: %s", e.Member+1, ev.Text)
				}
			}
		}
	}()
	return s, nil
}

func (s *dongles) frames() <-chan rxFrame { return s.out }
func (s *dongles) close()                 { _ = s.g.Close() }
func (s *dongles) describe() string       { return s.txt }

// members > 1: OpenHD should wait for late copies (docs/14 §9.3).
func (s *dongles) members() int { return len(s.g.Members()) }

// holdFor returns the FEC hold for a source.
func holdFor(src source) time.Duration {
	if d, ok := src.(*dongles); ok && d.members() > 1 {
		return diversity.HoldTime
	}
	return 0
}

// ---- pcapng replay (real time) ----

type pcapSource struct {
	out  chan rxFrame
	name string
}

func newPcapSource(ctx context.Context, path string) (*pcapSource, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	file, err := pcapng.Read(f)
	if err != nil {
		return nil, err
	}
	if file.LinkType != pcapng.LinkTypeRadiotap {
		return nil, fmt.Errorf("%s: link type %d, want 802.11 + radiotap (127)", path, file.LinkType)
	}
	s := &pcapSource{out: make(chan rxFrame, 256), name: fmt.Sprintf("%s (%d frames)", path, len(file.Packets))}
	go func() {
		defer close(s.out)
		var t0 uint64
		start := time.Now()
		for i, p := range file.Packets {
			if i == 0 {
				t0 = p.TsUS
			}
			if wait := time.Duration(p.TsUS-t0)*time.Microsecond - time.Since(start); wait > 0 {
				select {
				case <-time.After(wait):
				case <-ctx.Done():
					return
				}
			}
			rssi, ok := radiotapSignal(p.Radiotap)
			select {
			case s.out <- rxFrame{data: p.Frame, rssi: rssi, hasRSSI: ok}:
			case <-ctx.Done():
				return
			}
		}
	}()
	return s, nil
}

func (s *pcapSource) frames() <-chan rxFrame { return s.out }
func (s *pcapSource) close()                 {}
func (s *pcapSource) describe() string       { return s.name }

// radiotapSignal finds the dBm antenna signal in a radiotap header (first presence word
// only, the layout written by capturetonkeli).
func radiotapSignal(rt []byte) (int8, bool) {
	if len(rt) < 8 {
		return 0, false
	}
	present := uint32(rt[4]) | uint32(rt[5])<<8 | uint32(rt[6])<<16 | uint32(rt[7])<<24
	off := 8
	for p := present; p&(1<<31) != 0 && off+4 <= len(rt); off += 4 { // extended presence words
		p = uint32(rt[off]) | uint32(rt[off+1])<<8 | uint32(rt[off+2])<<16 | uint32(rt[off+3])<<24
	}
	type field struct{ align, size int }
	fields := []field{{8, 8}, {1, 1}, {1, 1}, {2, 4}, {2, 2}, {1, 1}} // TSFT, flags, rate, channel, FHSS, signal
	for bit, f := range fields {
		if present&(1<<bit) == 0 {
			continue
		}
		off = (off + f.align - 1) / f.align * f.align
		if bit == 5 {
			if off < len(rt) {
				return int8(rt[off]), true
			}
			return 0, false
		}
		off += f.size
	}
	return 0, false
}
