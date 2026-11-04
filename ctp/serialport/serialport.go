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

// SafeOpenSignals documents the control-line policy on open (docs/01 risk #1).
// Measured in M3 (Linux, XIAO ESP32-C5): the kernel raises DTR and RTS on
// open and that does not reset the running app; actively lowering RTS while
// the ROM loader runs wedged its USB-Serial/JTAG. Policy: never change the
// lines (same as esptool/pyserial defaults).
var SafeOpenSignals = struct{ DTR, RTS bool }{DTR: true, RTS: true}

// ApplySafeOpenSignals is intentionally a no-op: see SafeOpenSignals.
func ApplySafeOpenSignals(p Port) {}

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
