package testvectors

import (
	"testing"

	"github.com/hjkoskel/capturetonkeli/internal/vectorgen"
)

func TestVectorsUpToDate(t *testing.T) {
	diff, err := vectorgen.Check(".")
	if err != nil {
		t.Fatal(err)
	}
	if len(diff) > 0 {
		t.Fatalf("testvectors out of date (run go generate ./...): %v", diff)
	}
}
