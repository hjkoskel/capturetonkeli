//go:build !linux

package serialport

import "os"

// Open is not implemented yet on this OS (docs/05 §5: darwin/windows TODO).
func Open(path string) (Port, error) { return nil, ErrUnsupported }

// List is not implemented yet on this OS.
func List() ([]Info, error) { return nil, ErrUnsupported }

// OpenPTY is not implemented on this OS.
func OpenPTY() (*os.File, string, *os.File, error) { return nil, "", nil, ErrUnsupported }
