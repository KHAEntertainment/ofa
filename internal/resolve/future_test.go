package resolve

import (
	"testing"
	"time"
)

// TestBrewFuture_WaitIsCachedAndJoinIdempotent (issue #62 review B4):
// the outcome is cached on the first wait, so Resolve's consumption and
// the pipeline's Join can both drain the same future — never a second,
// unbounded channel receive. Join waits for the query to finish, and it
// is nil-safe.
func TestBrewFuture_WaitIsCachedAndJoinIdempotent(t *testing.T) {
	release := make(chan struct{})
	ran := make(chan struct{})
	run := func(name string, args ...string) (string, error) {
		close(ran)
		<-release // the query stays in flight until released
		return fixtureBrewJSON(), nil
	}
	f := PrefetchBrew(run)

	done := make(chan struct{})
	var r1, r2 *brewResolver
	go func() {
		defer close(done)
		r1, _ = f.wait()
		r2, _ = f.wait() // cached: must not block on a drained channel
		f.Join()
		f.Join()
	}()

	select {
	case <-done:
		t.Fatal("wait/Join returned while the query was still in flight")
	case <-time.After(50 * time.Millisecond): // the pass path: still waiting
	}
	<-ran // the query really started before the waits above
	close(release)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("wait/Join never returned after the query finished")
	}
	if r1 != r2 {
		t.Errorf("cached waits returned different resolvers: %p vs %p", r1, r2)
	}

	var nilF *BrewFuture
	nilF.Join() // must not panic
}
