// Package romflash talks to the ESP32-C5 mask-ROM serial download protocol
// (SLIP framing; SYNC, READ_REG, FLASH_*, SPI_FLASH_MD5), docs/05 §7b and
// docs/04 §11.1. ROM loader only: no stub binary is uploaded.
package romflash

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"
)

// ROM commands.
const (
	CmdFlashBegin   = 0x02
	CmdFlashData    = 0x03
	CmdFlashEnd     = 0x04
	CmdSync         = 0x08
	CmdWriteReg     = 0x09
	CmdReadReg      = 0x0A
	CmdSPISetParams = 0x0B
	CmdSPIAttach    = 0x0D
	CmdSPIFlashMD5  = 0x13
	CmdSecurityInfo = 0x14
)

// Protocol constants.
const (
	FlashWriteSize = 0x400 // ROM FLASH_DATA block size
	SectorSize     = 0x1000
	checksumSeed   = 0xEF
	ChipIDC5       = 23 // IMAGE_CHIP_ID / GET_SECURITY_INFO chip_id
)

// Errors.
var (
	ErrTimeout = errors.New("romflash: no response from ROM")
	ErrNotC5   = errors.New("romflash: not an ESP32-C5")
)

// StatusError is a non-zero ROM status.
type StatusError struct {
	Op     uint8
	Status uint8
	Reason uint8
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("romflash: command 0x%02x failed: status %d reason 0x%02x", e.Op, e.Status, e.Reason)
}

// ---- SLIP -------------------------------------------------------------------

func slipEncode(p []byte) []byte {
	out := make([]byte, 0, len(p)+8)
	out = append(out, 0xC0)
	for _, b := range p {
		switch b {
		case 0xC0:
			out = append(out, 0xDB, 0xDC)
		case 0xDB:
			out = append(out, 0xDB, 0xDD)
		default:
			out = append(out, b)
		}
	}
	return append(out, 0xC0)
}

// slipReader splits a byte stream into SLIP packets.
type slipReader struct {
	buf    []byte
	in     bool
	esc    bool
	frames chan []byte
}

func (s *slipReader) feed(p []byte) {
	for _, b := range p {
		if !s.in {
			if b == 0xC0 {
				s.in = true
				s.buf = s.buf[:0]
			}
			continue
		}
		switch {
		case s.esc:
			s.esc = false
			switch b {
			case 0xDC:
				s.buf = append(s.buf, 0xC0)
			case 0xDD:
				s.buf = append(s.buf, 0xDB)
			default: // invalid escape: drop the packet
				s.in = false
			}
		case b == 0xDB:
			s.esc = true
		case b == 0xC0:
			if len(s.buf) == 0 {
				continue // back-to-back delimiters
			}
			select {
			case s.frames <- append([]byte(nil), s.buf...):
			default:
			}
			s.buf = s.buf[:0]
		default:
			s.buf = append(s.buf, b)
		}
	}
}

// ---- loader -------------------------------------------------------------------

// Loader is a ROM protocol client on any byte stream.
type Loader struct {
	w      io.Writer
	frames chan []byte
	done   chan struct{}
	once   sync.Once
	// Trace, if set, sees every packet ("tx"/"rx").
	Trace func(dir string, p []byte)
}

// NewLoader starts reading rw. Close stops the reader only when rw is closed.
func NewLoader(rw io.ReadWriter) *Loader {
	l := &Loader{w: rw, frames: make(chan []byte, 64), done: make(chan struct{})}
	sr := &slipReader{frames: l.frames}
	go func() {
		defer close(l.done)
		buf := make([]byte, 4096)
		for {
			n, err := rw.Read(buf)
			if n > 0 {
				sr.feed(buf[:n])
			}
			if err != nil {
				return
			}
		}
	}()
	return l
}

func checksum(data []byte) uint32 {
	c := uint32(checksumSeed)
	for _, b := range data {
		c ^= uint32(b)
	}
	return c
}

func (l *Loader) drain() {
	for {
		select {
		case <-l.frames:
		default:
			return
		}
	}
}

// Command sends one request and waits for the response with the same op.
// It returns the value word and the response data (status bytes included).
func (l *Loader) Command(ctx context.Context, op uint8, data []byte, chk uint32, timeout time.Duration) (uint32, []byte, error) {
	pkt := make([]byte, 8, 8+len(data))
	pkt[0], pkt[1] = 0x00, op
	binary.LittleEndian.PutUint16(pkt[2:], uint16(len(data)))
	binary.LittleEndian.PutUint32(pkt[4:], chk)
	pkt = append(pkt, data...)
	if l.Trace != nil {
		l.Trace("tx", pkt)
	}
	if _, err := l.w.Write(slipEncode(pkt)); err != nil {
		return 0, nil, err
	}
	t := time.NewTimer(timeout)
	defer t.Stop()
	for {
		select {
		case p := <-l.frames:
			if l.Trace != nil {
				l.Trace("rx", p)
			}
			if len(p) < 8 || p[0] != 0x01 || p[1] != op {
				continue
			}
			n := int(binary.LittleEndian.Uint16(p[2:]))
			body := p[8:]
			if n < len(body) {
				body = body[:n]
			}
			return binary.LittleEndian.Uint32(p[4:]), body, nil
		case <-t.C:
			return 0, nil, fmt.Errorf("%w (command 0x%02x)", ErrTimeout, op)
		case <-ctx.Done():
			return 0, nil, ctx.Err()
		case <-l.done:
			return 0, nil, io.ErrUnexpectedEOF
		}
	}
}

// Check runs Command and validates the 2 status bytes that follow respLen bytes
// of response data (the ROM appends 2 more reserved bytes, ignored).
func (l *Loader) Check(ctx context.Context, op uint8, data []byte, chk uint32, respLen int, timeout time.Duration) (uint32, []byte, error) {
	val, body, err := l.Command(ctx, op, data, chk, timeout)
	if err != nil {
		return 0, nil, err
	}
	if len(body) < respLen+2 {
		if len(body) >= 2 && body[0] != 0 {
			return 0, nil, &StatusError{op, body[0], body[1]}
		}
		return 0, nil, fmt.Errorf("romflash: command 0x%02x: short response (%d bytes)", op, len(body))
	}
	if st := body[respLen:]; st[0] != 0 {
		return 0, nil, &StatusError{op, st[0], st[1]}
	}
	return val, body[:respLen], nil
}

// Sync performs the SYNC handshake, retrying for up to attempts.
func (l *Loader) Sync(ctx context.Context, attempts int) error {
	data := append([]byte{0x07, 0x07, 0x12, 0x20}, bytes.Repeat([]byte{0x55}, 32)...)
	var last error
	for i := 0; i < attempts; i++ {
		l.drain()
		_, _, err := l.Command(ctx, CmdSync, data, 0, 200*time.Millisecond)
		if err == nil {
			// the ROM answers SYNC several times; swallow the extras
			time.Sleep(50 * time.Millisecond)
			l.drain()
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		last = err
	}
	return fmt.Errorf("romflash: SYNC failed after %d attempts: %w", attempts, last)
}

// ReadReg reads a 32-bit register.
func (l *Loader) ReadReg(ctx context.Context, addr uint32) (uint32, error) {
	v, _, err := l.Check(ctx, CmdReadReg, binary.LittleEndian.AppendUint32(nil, addr), 0, 0, time.Second)
	return v, err
}

// WriteReg writes a 32-bit register (mask all, no delay).
func (l *Loader) WriteReg(ctx context.Context, addr, value uint32) error {
	d := binary.LittleEndian.AppendUint32(nil, addr)
	d = binary.LittleEndian.AppendUint32(d, value)
	d = binary.LittleEndian.AppendUint32(d, 0xFFFFFFFF)
	d = binary.LittleEndian.AppendUint32(d, 0)
	_, _, err := l.Check(ctx, CmdWriteReg, d, 0, 0, time.Second)
	return err
}

// SecurityInfo is the GET_SECURITY_INFO response.
type SecurityInfo struct {
	Flags       uint32
	FlashCrypt  uint8
	KeyPurposes [7]byte
	ChipID      uint32
	EcoVersion  uint32
}

// SecurityInfo queries chip id and security flags.
func (l *Loader) SecurityInfo(ctx context.Context) (SecurityInfo, error) {
	var si SecurityInfo
	_, b, err := l.Check(ctx, CmdSecurityInfo, nil, 0, 20, time.Second)
	if err != nil {
		return si, err
	}
	si.Flags = binary.LittleEndian.Uint32(b)
	si.FlashCrypt = b[4]
	copy(si.KeyPurposes[:], b[5:12])
	si.ChipID = binary.LittleEndian.Uint32(b[12:])
	si.EcoVersion = binary.LittleEndian.Uint32(b[16:])
	return si, nil
}

// SPIAttach attaches the default SPI flash (ROM: 8-byte argument).
func (l *Loader) SPIAttach(ctx context.Context) error {
	_, _, err := l.Check(ctx, CmdSPIAttach, make([]byte, 8), 0, 0, 3*time.Second)
	return err
}

// SPISetParams tells the ROM the flash geometry.
func (l *Loader) SPISetParams(ctx context.Context, totalSize uint32) error {
	var d []byte
	for _, v := range []uint32{0, totalSize, 64 * 1024, SectorSize, 256, 0xFFFF} {
		d = binary.LittleEndian.AppendUint32(d, v)
	}
	_, _, err := l.Check(ctx, CmdSPISetParams, d, 0, 0, time.Second)
	return err
}

// MD5 returns the MD5 (hex) of a flash region (ROM: 32 ASCII hex chars).
func (l *Loader) MD5(ctx context.Context, addr, size uint32) (string, error) {
	var d []byte
	for _, v := range []uint32{addr, size, 0, 0} {
		d = binary.LittleEndian.AppendUint32(d, v)
	}
	timeout := 3*time.Second + time.Duration(size/(1<<20))*8*time.Second
	_, b, err := l.Check(ctx, CmdSPIFlashMD5, d, 0, 32, timeout)
	if err != nil {
		return "", err
	}
	return string(bytes.ToLower(b)), nil
}

// FlashBegin erases [offset, offset+size) and prepares the write.
func (l *Loader) FlashBegin(ctx context.Context, offset, size uint32) (blocks uint32, err error) {
	blocks = (size + FlashWriteSize - 1) / FlashWriteSize
	var d []byte
	for _, v := range []uint32{size, blocks, FlashWriteSize, offset, 0 /* not encrypted */} {
		d = binary.LittleEndian.AppendUint32(d, v)
	}
	timeout := 3*time.Second + time.Duration(size)*30*time.Second/(1<<20) // ROM erases up front
	_, _, err = l.Check(ctx, CmdFlashBegin, d, 0, 0, timeout)
	return blocks, err
}

// FlashData writes one block (padded with 0xFF to FlashWriteSize).
func (l *Loader) FlashData(ctx context.Context, seq uint32, block []byte) error {
	if len(block) < FlashWriteSize {
		block = append(append([]byte(nil), block...), bytes.Repeat([]byte{0xFF}, FlashWriteSize-len(block))...)
	}
	var d []byte
	for _, v := range []uint32{uint32(len(block)), seq, 0, 0} {
		d = binary.LittleEndian.AppendUint32(d, v)
	}
	d = append(d, block...)
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		if _, _, err = l.Check(ctx, CmdFlashData, d, checksum(block), 0, 5*time.Second); err == nil {
			return nil
		}
	}
	return err
}

// FlashEnd leaves flash mode; stay=true keeps the ROM loader running.
func (l *Loader) FlashEnd(ctx context.Context, stay bool) error {
	v := uint32(0)
	if stay {
		v = 1
	}
	_, _, err := l.Check(ctx, CmdFlashEnd, binary.LittleEndian.AppendUint32(nil, v), 0, 0, time.Second)
	return err
}

// LP watchdog registers (ESP32-C5 inherits the C6 layout, esptool targets/esp32c6.py).
const (
	lpWDTBase     = 0x600B1C00
	lpWDTConfig0  = lpWDTBase + 0x00
	lpWDTConfig1  = lpWDTBase + 0x04
	lpWDTWProtect = lpWDTBase + 0x18
	lpWDTWKey     = 0x50D83AA1
)

// WatchdogReset resets the chip with the LP watchdog. Unlike the RTS "hard
// reset" this reliably leaves ROM on the C5 (docs/10 §3, M3). The USB device
// re-enumerates, so the port must be re-found afterwards.
func (l *Loader) WatchdogReset(ctx context.Context) error {
	if err := l.WriteReg(ctx, lpWDTWProtect, lpWDTWKey); err != nil {
		return err
	}
	if err := l.WriteReg(ctx, lpWDTConfig1, 2000); err != nil {
		return err
	}
	// enable, stage0 action = system reset; the chip resets ~immediately, so
	// the response to this write may never arrive
	_ = l.WriteReg(ctx, lpWDTConfig0, (1<<31)|(5<<28)|(1<<8)|2)
	_ = l.WriteReg(ctx, lpWDTWProtect, 0)
	return nil
}
