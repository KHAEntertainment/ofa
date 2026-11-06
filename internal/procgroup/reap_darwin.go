//go:build darwin

package procgroup

import "syscall"

// waitLeaderExit blocks until the child pid has exited, without reaping it,
// so its PID — and therefore its PGID — stays allocated until exec.Cmd.Wait
// reaps it. macOS has no waitid in the syscall package, but a kqueue
// EVFILT_PROC/NOTE_EXIT event reports the exit and never reaps. It blocks
// in the kernel (no polling); EINTR restarts the wait.
func waitLeaderExit(pid int) error {
	kq, err := syscall.Kqueue()
	if err != nil {
		return err
	}
	defer syscall.Close(kq)

	var change syscall.Kevent_t
	syscall.SetKevent(&change, pid, syscall.EVFILT_PROC, syscall.EV_ADD|syscall.EV_ONESHOT)
	change.Fflags = syscall.NOTE_EXIT
	// Register with no event buffer, so a registration error comes back as
	// the call's error rather than as an EV_ERROR event.
	if _, err := syscall.Kevent(kq, []syscall.Kevent_t{change}, nil, nil); err != nil {
		if err == syscall.ESRCH {
			// pid is our own unreaped child, so it cannot be gone: ESRCH
			// means it already exited and is a zombie.
			return nil
		}
		return err
	}
	events := make([]syscall.Kevent_t, 1)
	for {
		n, err := syscall.Kevent(kq, nil, events, nil)
		if err == syscall.EINTR {
			continue
		}
		if err != nil {
			return err
		}
		if n == 1 && events[0].Fflags&syscall.NOTE_EXIT != 0 {
			return nil
		}
	}
}
