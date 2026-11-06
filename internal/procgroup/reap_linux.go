//go:build linux

package procgroup

import (
	"syscall"
	"unsafe"
)

// pPID is waitid's P_PID id type: wait for the one process named by id.
const pPID = 1

// waitLeaderExit blocks until the child pid has exited, leaving it an
// unreaped zombie (WNOWAIT) so its PID — and therefore its PGID — stays
// allocated until exec.Cmd.Wait reaps it. The syscall package exposes no
// waitid wrapper, so this is the raw call the os package itself makes for
// the same purpose. It blocks in the kernel (no polling); EINTR restarts it.
func waitLeaderExit(pid int) error {
	// siginfo_t is 128 bytes on every Linux ABI. Nothing reads it; waitid
	// just needs somewhere to write.
	var info [16]uint64
	for {
		_, _, errno := syscall.Syscall6(syscall.SYS_WAITID, pPID, uintptr(pid),
			uintptr(unsafe.Pointer(&info)), syscall.WEXITED|syscall.WNOWAIT, 0, 0)
		if errno == syscall.EINTR {
			continue
		}
		if errno != 0 {
			return errno
		}
		return nil
	}
}
