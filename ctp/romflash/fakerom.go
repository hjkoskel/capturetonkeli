package romflash

import (
	"bytes"
	"crypto/md5"
	"encoding/binary"
	"encoding/hex"
	"io"
	"sync"
)

// FakeROM emulates the ESP32-C5 ROM download protocol over an in-memory
// flash (docs/05 §8), for tests and the simulator. Only the commands used by
// Flasher are implemented; others answer status 1/0x05 like the ROM.
type FakeROM struct {
	mu       sync.Mutex
	Flash    []byte
	ChipID   uint32
	Writes   int // FLASH_DATA blocks written
	Resets   int // watchdog resets triggered
	Regs     map[uint32]uint32
	begin    struct{ off, size, blocks, next uint32 }
	CorruptN int // corrupt the next N written blocks (tests retry)
}

// NewFakeROM returns a C5 with 8 MB of erased flash.
func NewFakeROM() *FakeROM {
	return &FakeROM{Flash: bytes.Repeat([]byte{0xFF}, 8<<20), ChipID: ChipIDC5, Regs: map[uint32]uint32{}}
}

// Serve answers requests on rw until it fails.
func (r *FakeROM) Serve(rw io.ReadWriter) error {
	frames := make(chan []byte, 64)
	sr := &slipReader{frames: frames}
	errc := make(chan error, 1)
	go func() {
		buf := make([]byte, 8192)
		for {
			n, err := rw.Read(buf)
			if n > 0 {
				sr.feed(buf[:n])
			}
			if err != nil {
				errc <- err
				close(frames)
				return
			}
		}
	}()
	for p := range frames {
		if len(p) < 8 || p[0] != 0 {
			continue
		}
		op := p[1]
		data := p[8:]
		val, body, st := r.exec(op, data, binary.LittleEndian.Uint32(p[4:]))
		resp := make([]byte, 8)
		resp[0], resp[1] = 1, op
		body = append(body, st[0], st[1], 0, 0)
		binary.LittleEndian.PutUint16(resp[2:], uint16(len(body)))
		binary.LittleEndian.PutUint32(resp[4:], val)
		resp = append(resp, body...)
		if _, err := rw.Write(slipEncode(resp)); err != nil {
			return err
		}
	}
	return <-errc
}

func (r *FakeROM) exec(op uint8, d []byte, chk uint32) (uint32, []byte, [2]byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	le := binary.LittleEndian
	ok := [2]byte{0, 0}
	bad := [2]byte{1, 0x05}
	switch op {
	case CmdSync:
		return 0, nil, ok
	case CmdSecurityInfo:
		b := make([]byte, 20)
		le.PutUint32(b[12:], r.ChipID)
		return 0, b, ok
	case CmdSPIAttach, CmdSPISetParams:
		return 0, nil, ok
	case CmdReadReg:
		if len(d) < 4 {
			return 0, nil, bad
		}
		return r.Regs[le.Uint32(d)], nil, ok
	case CmdWriteReg:
		if len(d) < 8 {
			return 0, nil, bad
		}
		a, v := le.Uint32(d), le.Uint32(d[4:])
		r.Regs[a] = v
		if a == lpWDTConfig0 && v&(1<<31) != 0 {
			r.Resets++
		}
		return 0, nil, ok
	case CmdFlashBegin:
		if len(d) < 16 {
			return 0, nil, bad
		}
		size, blocks, bs, off := le.Uint32(d), le.Uint32(d[4:]), le.Uint32(d[8:]), le.Uint32(d[12:])
		if bs != FlashWriteSize || uint64(off)+uint64(size) > uint64(len(r.Flash)) {
			return 0, nil, bad
		}
		end := (off + size + SectorSize - 1) / SectorSize * SectorSize
		for i := off; i < end && int(i) < len(r.Flash); i++ {
			r.Flash[i] = 0xFF
		}
		r.begin.off, r.begin.size, r.begin.blocks, r.begin.next = off, size, blocks, 0
		return 0, nil, ok
	case CmdFlashData:
		if len(d) < 16 {
			return 0, nil, bad
		}
		n, seq := le.Uint32(d), le.Uint32(d[4:])
		blk := d[16:]
		if uint32(len(blk)) != n || seq != r.begin.next || seq >= r.begin.blocks || checksum(blk) != chk {
			return 0, nil, bad
		}
		off := r.begin.off + seq*FlashWriteSize
		w := append([]byte(nil), blk...)
		if r.CorruptN > 0 {
			r.CorruptN--
			w[0] ^= 0xFF
		}
		copy(r.Flash[off:], w)
		r.begin.next++
		r.Writes++
		return 0, nil, ok
	case CmdFlashEnd:
		if r.begin.blocks == 0 {
			return 0, nil, [2]byte{1, 0x06} // like the real ROM: no FLASH_BEGIN
		}
		r.begin.blocks = 0
		return 0, nil, ok
	case CmdSPIFlashMD5:
		if len(d) < 8 {
			return 0, nil, bad
		}
		a, n := le.Uint32(d), le.Uint32(d[4:])
		if uint64(a)+uint64(n) > uint64(len(r.Flash)) {
			return 0, nil, bad
		}
		s := md5.Sum(r.Flash[a : a+n])
		return 0, []byte(hex.EncodeToString(s[:])), ok
	}
	return 0, nil, bad
}
