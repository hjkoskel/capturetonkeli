// Package serialport is a stdlib-only serial port layer (docs/05 §5).
// Linux is implemented with raw syscalls; macOS and Windows are not yet
// implemented and return ErrUnsupported.
package serialport

import (
	"errors"
	"io"
	"time"
)

// Errors.
var (
	ErrPortBusy    = errors.New("serialport: port busy (opened by another process)")
	ErrUnsupported = errors.New("serialport: not implemented on this OS yet")
)

// Port is an open serial port.
type Port interface {
	io.ReadWriteCloser
	SetDTR(bool) error
	SetRTS(bool) error
	SetReadTimeout(time.Duration) error
}

// Info describes a serial port.
type Info struct {
	Path    string `json:"path"`
	VID     uint16 `json:"vid"`
	PID     uint16 `json:"pid"`
	Serial  string `json:"serial,omitempty"`
	Product string `json:"product,omitempty"`
}

// IsDongle reports whether the port is an Espressif USB-Serial/JTAG device.
func (i Info) IsDongle() bool { return i.VID == DongleVID && i.PID == DonglePID }

// USB IDs of the ESP32-C5 USB-Serial/JTAG controller.
const (
	DongleVID = 0x303A
	DonglePID = 0x1001
)

// SafeOpenSignals is the DTR/RTS state applied right after open so the
// USB-JTAG auto-reset logic is not triggered (docs/01 risk #1).
// PROVISIONAL until measured on hardware in M3: DTR=1, RTS=0 is the
// "run" state of esptool's USB-JTAG reset sequence.
var SafeOpenSignals = struct{ DTR, RTS bool }{DTR: true, RTS: false}

// ApplySafeOpenSignals sets SafeOpenSignals; errors (e.g. on a PTY) are ignored.
func ApplySafeOpenSignals(p Port) {
	_ = p.SetRTS(SafeOpenSignals.RTS)
	_ = p.SetDTR(SafeOpenSignals.DTR)
}

// FindDongles lists ports with the dongle VID/PID.
func FindDongles() ([]Info, error) {
	all, err := List()
	if err != nil {
		return nil, err
	}
	var out []Info
	for _, p := range all {
		if p.IsDongle() {
			out = append(out, p)
		}
	}
	return out, nil
}
