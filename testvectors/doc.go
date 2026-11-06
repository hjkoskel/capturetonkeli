// Package testvectors holds the golden vectors shared by Go, JS and the
// firmware C tests (docs/05 §9). The files are generated from the Go
// reference implementation; never edit them by hand.
package testvectors

//go:generate go run ../internal/genvectors -out .
