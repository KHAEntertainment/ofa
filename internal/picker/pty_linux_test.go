//go:build linux

package picker

// PTY fixture for the capability tests, stdlib/syscall only (issue #62
// review N1): /dev/ptmx with the TIOCSPTLCK/TIOCGPTN ioctls.
// Infrastructure problems skip with the exact reason; capability
// regressions fail.

import (
	"fmt"
	"os"
	"syscall"
	"testing"
	"unsafe"
)

const (
	tiocSPtLck  = 0x40045431 // TIOCSPTLCK
	tiocGPtn    = 0x80045430 // TIOCGPTN
	tiocCSWinsz = 0x5414     // TIOCSWINSZ
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
	var unlock int32
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, m.Fd(), tiocSPtLck, uintptr(unsafe.Pointer(&unlock))); errno != 0 {
		m.Close()
		t.Skipf("PTY infrastructure unavailable: TIOCSPTLCK: %v", errno)
	}
	var n uint32
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, m.Fd(), tiocGPtn, uintptr(unsafe.Pointer(&n))); errno != 0 {
		m.Close()
		t.Skipf("PTY infrastructure unavailable: TIOCGPTN: %v", errno)
	}
	s, err := os.OpenFile(fmt.Sprintf("/dev/pts/%d", n), os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		m.Close()
		t.Skipf("PTY infrastructure unavailable: open /dev/pts/%d: %v", n, err)
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
