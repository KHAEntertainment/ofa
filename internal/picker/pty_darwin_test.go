//go:build darwin

package picker

// PTY fixture for the capability tests, stdlib/syscall only (issue #62
// review N1): /dev/ptmx with the TIOCPTY* ioctls. Infrastructure
// problems skip with the exact reason; capability regressions fail.

import (
	"os"
	"strings"
	"syscall"
	"testing"
	"unsafe"
)

const (
	tiocPTYGrant  = 0x20007454 // _IO('t', 84)
	tiocPTYUnlock = 0x20007452 // _IO('t', 82)
	tiocPTYGname  = 0x40807453 // _IOR('t', 83, char[128])
	tiocCSWinsz   = 0x80087467 // _IOW('t', 103, struct winsize)
)

type winsize struct {
	rows, cols, xpix, ypix uint16
}

// openPTY opens a master/slave PTY pair.
func openPTY(t *testing.T) (master, slave *os.File) {
	t.Helper()
	m, err := os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		t.Skipf("PTY infrastructure unavailable: open /dev/ptmx: %v", err)
	}
	var arg int32
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, m.Fd(), tiocPTYGrant, uintptr(unsafe.Pointer(&arg))); errno != 0 {
		m.Close()
		t.Skipf("PTY infrastructure unavailable: TIOCPTYGRANT: %v", errno)
	}
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, m.Fd(), tiocPTYUnlock, uintptr(unsafe.Pointer(&arg))); errno != 0 {
		m.Close()
		t.Skipf("PTY infrastructure unavailable: TIOCPTYUNLOCK: %v", errno)
	}
	name := make([]byte, 128)
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, m.Fd(), tiocPTYGname, uintptr(unsafe.Pointer(&name[0]))); errno != 0 {
		m.Close()
		t.Skipf("PTY infrastructure unavailable: TIOCPTYGNAME: %v", errno)
	}
	s, err := os.OpenFile(strings.TrimRight(string(name), "\x00"), os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		m.Close()
		t.Skipf("PTY infrastructure unavailable: open slave %q: %v", strings.TrimRight(string(name), "\x00"), err)
	}
	return m, s
}

// setPTYWinsize sets the pty window size — the data source behind a
// real terminal's resize handling.
func setPTYWinsize(t *testing.T, master *os.File, rows, cols uint16) {
	t.Helper()
	ws := winsize{rows: rows, cols: cols}
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, master.Fd(), tiocCSWinsz, uintptr(unsafe.Pointer(&ws))); errno != 0 {
		t.Fatalf("PTY infrastructure unavailable: TIOCSWINSZ: %v", errno)
	}
}
