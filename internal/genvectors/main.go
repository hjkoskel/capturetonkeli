// Command genvectors regenerates testvectors/ (see internal/vectorgen).
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/hjkoskel/capturetonkeli/internal/vectorgen"
)

func main() {
	out := flag.String("out", ".", "testvectors directory")
	flag.Parse()
	if err := vectorgen.Generate(*out); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
