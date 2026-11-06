//go:build !linux && !darwin

package doctor

import "errors"

// waitLeaderExit has no no-reap exit wait here, so it reports failure and
// killGroupBeforeReap sends nothing after the leader exits. The alternative,
// killing the group after Wait has reaped the leader, could signal a group
// that reused the PID. Same-group stragglers then cost at most WaitDelay
// before the runner returns, and may outlive it. Cancellation still kills
// the whole group, because Cancel runs while the leader is alive.
func waitLeaderExit(int) error {
	return errors.New("doctor runner: no exit wait without reaping on this platform")
}
