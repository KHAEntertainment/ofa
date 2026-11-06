//go:build !linux && !darwin

package doctor

import "errors"

// waitLeaderExit cannot wait for an exit without reaping on this platform,
// so it reports failure at once. As a result no group kill is ever sent
// here: killGroupBeforeReap skips it, and the guard is released before Wait
// while the leader may still be running, which also makes Cancel send
// nothing. The only safe group kill is one sent while the leader is
// unreaped, and that cannot be arranged without a no-reap wait.
// Consequences: same-group stragglers cost at most WaitDelay and may outlive
// the runner, and a timeout is enforced only by os/exec killing the leader
// WaitDelay after the deadline. ofa ships only for Linux and macOS.
func waitLeaderExit(int) error {
	return errors.New("doctor runner: no exit wait without reaping on this platform")
}
