package conformance

import (
	"context"
	"io"
	"testing"

	"github.com/hjkoskel/capturetonkeli/ctp/sim"
)

// TestSuiteAgainstSim proves the suite: every case must pass on the reference simulator.
func TestSuiteAgainstSim(t *testing.T) {
	if testing.Short() {
		t.Skip("takes ~25 s")
	}
	dev := sim.NewDevice(sim.SimOptions{Seed: 1})
	env := Env{Open: func(ctx context.Context) (io.ReadWriteCloser, error) { return dev.Attach(), nil }}
	res := Run(context.Background(), env, Options{})
	n := map[Status]int{}
	for _, r := range res {
		n[r.Status]++
	}
	t.Logf("%d cases: %v", len(res), n)
	for _, r := range res {
		switch r.Status {
		case Fail:
			t.Errorf("%s %s: expected %s, actual %s (hint %s)", r.ID, r.Title, r.Expected, r.Actual, r.Hint)
		case Skip:
			if r.ID != "CTP-FLT-RF-001" {
				t.Errorf("%s skipped: %s", r.ID, r.Actual)
			}
		}
	}
}
