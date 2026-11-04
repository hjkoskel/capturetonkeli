//go:build linux

package serialport

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unsafe"
)

type linuxPort struct {
	f  *os.File
	fd int
}

func ioctl(fd int, req uint, arg uintptr) error {
	_, _, e := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), uintptr(req), arg)
	if e != 0 {
		return e
	}
	return nil
}

// MakeRaw puts a tty fd into raw 8N1 mode (exported for PTY helpers).
func MakeRaw(fd int) error {
	var t syscall.Termios
	if err := ioctl(fd, syscall.TCGETS, uintptr(unsafe.Pointer(&t))); err != nil {
		return err
	}
	t.Iflag &^= syscall.IGNBRK | syscall.BRKINT | syscall.PARMRK | syscall.ISTRIP | syscall.INLCR | syscall.IGNCR | syscall.ICRNL | syscall.IXON | syscall.IXOFF
	t.Oflag &^= syscall.OPOST
	t.Lflag &^= syscall.ECHO | syscall.ECHONL | syscall.ICANON | syscall.ISIG | syscall.IEXTEN
	t.Cflag &^= syscall.CSIZE | syscall.PARENB | 0x80000000 /* CRTSCTS */
	t.Cflag |= syscall.CS8 | syscall.CLOCAL | syscall.CREAD
	t.Cc[syscall.VMIN] = 1
	t.Cc[syscall.VTIME] = 0
	return ioctl(fd, syscall.TCSETS, uintptr(unsafe.Pointer(&t)))
}

// Open opens path in raw mode with an exclusive lock.
func Open(path string) (Port, error) {
	fd, err := syscall.Open(path, syscall.O_RDWR|syscall.O_NOCTTY|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
	if err != nil {
		if errors.Is(err, syscall.EBUSY) {
			return nil, ErrPortBusy
		}
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	if err := ioctl(fd, syscall.TIOCEXCL, 0); err != nil {
		syscall.Close(fd)
		return nil, &os.PathError{Op: "TIOCEXCL", Path: path, Err: err}
	}
	if err := MakeRaw(fd); err != nil {
		syscall.Close(fd)
		return nil, &os.PathError{Op: "termios", Path: path, Err: err}
	}
	// os.NewFile on a non-blocking fd registers it with the runtime poller,
	// so Read blocks without spinning and Close unblocks it.
	return &linuxPort{f: os.NewFile(uintptr(fd), path), fd: fd}, nil
}

func (p *linuxPort) Read(b []byte) (int, error)  { return p.f.Read(b) }
func (p *linuxPort) Write(b []byte) (int, error) { return p.f.Write(b) }

// Close releases the exclusive lock first: a PTY whose other side keeps
// the slave open would otherwise stay locked for the next opener.
func (p *linuxPort) Close() error {
	_ = ioctl(p.fd, syscall.TIOCNXCL, 0)
	return p.f.Close()
}

func (p *linuxPort) modem(bit int, on bool) error {
	req := uint(syscall.TIOCMBIC)
	if on {
		req = syscall.TIOCMBIS
	}
	v := bit
	return ioctl(p.fd, req, uintptr(unsafe.Pointer(&v)))
}

func (p *linuxPort) SetDTR(on bool) error { return p.modem(syscall.TIOCM_DTR, on) }
func (p *linuxPort) SetRTS(on bool) error { return p.modem(syscall.TIOCM_RTS, on) }

func (p *linuxPort) SetReadTimeout(d time.Duration) error {
	if d <= 0 {
		return p.f.SetReadDeadline(time.Time{})
	}
	return p.f.SetReadDeadline(time.Now().Add(d))
}

// List walks /sys/class/tty for USB serial devices.
func List() ([]Info, error) {
	ents, err := os.ReadDir("/sys/class/tty")
	if err != nil {
		return nil, err
	}
	var out []Info
	for _, e := range ents {
		name := e.Name()
		if !strings.HasPrefix(name, "ttyACM") && !strings.HasPrefix(name, "ttyUSB") {
			continue
		}
		dev, err := filepath.EvalSymlinks(filepath.Join("/sys/class/tty", name, "device"))
		if err != nil {
			continue
		}
		info := Info{Path: "/dev/" + name}
		// walk up to the USB device directory (has idVendor)
		for dir := dev; dir != "/" && dir != "."; dir = filepath.Dir(dir) {
			if v, err := os.ReadFile(filepath.Join(dir, "idVendor")); err == nil {
				info.VID = parseHex16(v)
				pid, _ := os.ReadFile(filepath.Join(dir, "idProduct"))
				info.PID = parseHex16(pid)
				s, _ := os.ReadFile(filepath.Join(dir, "serial"))
				info.Serial = strings.TrimSpace(string(s))
				pr, _ := os.ReadFile(filepath.Join(dir, "product"))
				info.Product = strings.TrimSpace(string(pr))
				break
			}
		}
		out = append(out, info)
	}
	return out, nil
}

func parseHex16(b []byte) uint16 {
	v, _ := strconv.ParseUint(strings.TrimSpace(string(b)), 16, 16)
	return uint16(v)
}

// OpenPTY creates a pseudo-terminal pair in raw mode. It returns the master
// and the slave path. The slave is kept open internally (returned as hold)
// so the master never sees EIO while no client is attached.
func OpenPTY() (master *os.File, slavePath string, hold *os.File, err error) {
	m, err := os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		return nil, "", nil, err
	}
	fd := int(m.Fd())
	var unlock int32
	if err := ioctl(fd, syscall.TIOCSPTLCK, uintptr(unsafe.Pointer(&unlock))); err != nil {
		m.Close()
		return nil, "", nil, err
	}
	var n uint32
	if err := ioctl(fd, syscall.TIOCGPTN, uintptr(unsafe.Pointer(&n))); err != nil {
		m.Close()
		return nil, "", nil, err
	}
	sp := "/dev/pts/" + strconv.Itoa(int(n))
	s, err := os.OpenFile(sp, os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		m.Close()
		return nil, "", nil, err
	}
	if err := MakeRaw(int(s.Fd())); err != nil {
		m.Close()
		s.Close()
		return nil, "", nil, err
	}
	return m, sp, s, nil
}
