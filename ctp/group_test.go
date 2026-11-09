package ctp_test

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/hjkoskel/capturetonkeli/ctp"
	"github.com/hjkoskel/capturetonkeli/ctp/sim"
)

// Two simulated dongles with the same seed hear the same air (identical frame sequence);
// the group merges them into one stream.
func TestGroupMergesTwoSims(t *testing.T) {
	traffic := sim.TrafficModel{BSSPerChannel: 3, FramesPerSec: 300, FCSErrorRate: 0}
	var rws []io.ReadWriteCloser
	for i := 0; i < 2; i++ {
		h, _ := sim.Pipe(sim.SimOptions{Seed: 42, Traffic: traffic, MAC: [6]byte{2, 0xc5, 0, 0, 0, byte(2 - i)}})
		rws = append(rws, h)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ch, on := uint8(6), true
	g, err := ctp.NewGroup(ctx, rws, ctp.GroupOptions{Options: ctp.Options{Desired: &ctp.DesiredConfig{Channel: &ch, Capture: &on}}, Archive: true,
		Labels: map[string]string{"02:c5:00:00:00:01": "omni"}})
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]int{}
	live, finals, multi := 0, 0, 0
	deadline := time.After(1500 * time.Millisecond)
loop:
	for {
		select {
		case f := <-g.Frames():
			live++
			if len(f.Data) >= 24 {
				seen[string(f.Data)]++
			}
		case f := <-g.Final():
			finals++
			if len(f.Copies) == 2 {
				multi++
				if a := f.Antennas(); len(a) != 2 || a[0].Index == a[1].Index {
					t.Errorf("antennas %+v", a)
				}
			}
		case <-deadline:
			break loop
		}
	}
	st := g.Stats()
	if !st.Merging || len(st.Members) != 2 {
		t.Fatalf("stats %+v", st)
	}
	if st.Merge.Ref != 1 || st.Members[1].Label != "omni" {
		t.Fatalf("reference must be the lowest MAC (member 1, labelled): %+v", st.Merge)
	}
	dups := 0
	for _, n := range seen {
		dups += n - 1
	}
	if dups > 0 {
		t.Fatalf("%d frames emitted twice", dups)
	}
	if live < 200 || multi < live/2 {
		t.Fatalf("live %d finals %d with both copies %d", live, finals, multi)
	}
	if err := g.Close(); err != nil {
		t.Fatal(err)
	}
	for range g.Frames() {
	}
}

// A lost member leaves a working group of one (pass-through); the primary moves on.
func TestGroupMemberLoss(t *testing.T) {
	var rws []io.ReadWriteCloser
	for i := 0; i < 2; i++ {
		h, _ := sim.Pipe(sim.SimOptions{Seed: 7, MAC: [6]byte{2, 0xc5, 0, 0, 0, byte(1 + i)}})
		rws = append(rws, h)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	g, err := ctp.NewGroup(ctx, rws, ctp.GroupOptions{Options: ctp.Options{PingInterval: 50 * time.Millisecond}})
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	if _, err := g.SetChannel(ctx, 11, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := g.Capture(ctx, true); err != nil {
		t.Fatal(err)
	}
	prim := g.Primary()
	rws[0].Close() // unplug the primary
	var lost, primary bool
	timeout := time.After(3 * time.Second)
	for !(lost && primary) {
		select {
		case e := <-g.Events():
			switch e.Event.(type) {
			case ctp.EventMemberLost:
				lost = e.Member == 0
			case ctp.EventPrimary:
				primary = e.Member == 1
			}
		case <-timeout:
			t.Fatalf("lost %v primary %v: %+v", lost, primary, g.Stats().Members)
		}
	}
	if g.Primary() == prim || g.Stats().Merging {
		t.Fatal("primary not handed over or still merging")
	}
	// frames keep coming from the remaining dongle
	select {
	case f := <-g.Frames():
		if f.Member != 1 || f.Channel != 11 {
			t.Fatalf("%+v", f.PktMeta)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no frames after the loss")
	}
}

func TestGroupRejectsSameDongleTwice(t *testing.T) {
	h1, _ := sim.Pipe(sim.SimOptions{})
	h2, _ := sim.Pipe(sim.SimOptions{}) // same default MAC
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := ctp.NewGroup(ctx, []io.ReadWriteCloser{h1, h2}, ctp.GroupOptions{}); err == nil {
		t.Fatal("two members with the same MAC accepted")
	}
}
