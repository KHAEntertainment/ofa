//go:build !darwin && !linux

package picker

// PTY fixture stub for platforms without a stdlib/syscall PTY route in
// this suite: the capability tests skip with an exact reason rather
// than hiding behind silence (issue #62 review N1).

import (
	"os"
	"runtime"
	"testing"
)

func openPTY(t *testing.T) (master, slave *os.File) {
	t.Skipf("PTY infrastructure unavailable: no stdlib/syscall PTY route implemented for %s in this suite", runtime.GOOS)
	return nil, nil
}

func setPTYWinsize(t *testing.T, master *os.File, rows, cols uint16) {
	t.Skipf("PTY infrastructure unavailable: no stdlib/syscall winsize route implemented for %s in this suite", runtime.GOOS)
}
