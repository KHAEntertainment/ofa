package update

import (
	"errors"
	"sync/atomic"
	"testing"
)

// Tests for the observed apply path (issue #62): lifecycle events with
// stable request indexes, and race-safe between-request cancellation
// that never kills an in-flight installer.

// TestApplyObserved_EventsUseStableIndexesWithDuplicateLabels: events
// carry the request index — the stable row identity — so two installs
// sharing a label never merge.
func TestApplyObserved_EventsUseStableIndexesWithDuplicateLabels(t *testing.T) {
	withLookPathAllowed(t)
	exec := stubExecutor(map[string]Result{
		"claude (native)": {ExitCode: 0},
	})
	var events []Event
	s := ApplyObserved([]Request{
		{Label: "claude (native)", Cmd: CmdSpec{Cmd: []string{"x", "1"}}},
		{Label: "claude (native)", Cmd: CmdSpec{Cmd: []string{"x", "2"}}},
	}, exec, func(ev Event) { events = append(events, ev) }, nil)

	if len(s.Succeeded) != 2 || s.ExitCode() != 0 {
		t.Fatalf("summary = %+v, want 2 succeeded, exit 0", s)
	}
	if len(events) != 4 {
		t.Fatalf("events = %+v, want 2 started + 2 finished", events)
	}
	want := []struct {
		typ   EventType
		index int
	}{
		{EventStarted, 0}, {EventFinished, 0},
		{EventStarted, 1}, {EventFinished, 1},
	}
	for i, w := range want {
		if events[i].Type != w.typ || events[i].Index != w.index {
			t.Errorf("event[%d] = %+v, want type %v index %d", i, events[i], w.typ, w.index)
		}
		if events[i].Label != "claude (native)" {
			t.Errorf("event[%d] label = %q", i, events[i].Label)
		}
	}
	if events[1].Result == nil || events[3].Result == nil {
		t.Fatal("finished events must carry their Result")
	}
}

// TestApplyObserved_SkipPathsEmitFinished: requests skipped without
// executing (empty command, missing binary) still report exactly one
// Finished event each — a row never stays stuck at queued.
func TestApplyObserved_SkipPathsEmitFinished(t *testing.T) {
	withLookPathDenied(t)
	var events []Event
	s := ApplyObserved([]Request{
		{Label: "empty", Cmd: CmdSpec{}},
		{Label: "gone", Cmd: CmdSpec{Cmd: []string{"ghost", "update"}}},
	}, stubExecutor(nil), func(ev Event) { events = append(events, ev) }, nil)

	if len(s.Skipped) != 2 {
		t.Fatalf("summary = %+v, want 2 skipped", s)
	}
	if len(events) != 2 {
		t.Fatalf("events = %+v, want one Finished per request", events)
	}
	for i, ev := range events {
		if ev.Type != EventFinished || ev.Result == nil || !ev.Result.Skipped {
			t.Errorf("event[%d] = %+v, want Finished with a skipped result", i, ev)
		}
	}
}

// TestApplyObserved_StopSkipsRemainingAndCompletesInFlight: closing stop
// while a request runs lets that request finish (no forced kill) and
// marks every not-yet-started request skipped "canceled". Skipped rows
// don't fail the exit code.
func TestApplyObserved_StopSkipsRemainingAndCompletesInFlight(t *testing.T) {
	withLookPathAllowed(t)
	stop := make(chan struct{})
	started := make(chan struct{})
	release := make(chan struct{})
	var ranLate atomic.Bool
	exec := func(req Request) Result {
		if req.Label != "first" {
			ranLate.Store(true)
			return Result{Label: req.Label, Cmd: req.Cmd}
		}
		close(started)
		<-release
		return Result{Label: req.Label, Cmd: req.Cmd}
	}
	var events []Event
	done := make(chan Summary, 1)
	go func() {
		done <- ApplyObserved([]Request{
			{Label: "first", Cmd: CmdSpec{Cmd: []string{"x", "1"}}},
			{Label: "second", Cmd: CmdSpec{Cmd: []string{"x", "2"}}},
			{Label: "third", Cmd: CmdSpec{Cmd: []string{"x", "3"}}},
		}, exec, func(ev Event) { events = append(events, ev) }, stop)
	}()

	<-started
	close(stop)    // while "first" is in flight
	close(release) // "first" finishes normally
	s := <-done

	if len(s.Succeeded) != 1 || s.Succeeded[0].Label != "first" {
		t.Errorf("succeeded = %+v, want only the in-flight request", s.Succeeded)
	}
	if len(s.Skipped) != 2 {
		t.Fatalf("skipped = %+v, want the two not-yet-started requests", s.Skipped)
	}
	for _, sk := range s.Skipped {
		if sk.SkippedReason != "canceled" {
			t.Errorf("skip reason = %q, want \"canceled\"", sk.SkippedReason)
		}
	}
	if ranLate.Load() {
		t.Error("a request ran after the stop was observed — cancellation is between requests only")
	}
	if s.ExitCode() != 0 {
		t.Errorf("exit = %d, want 0 (skipped/canceled never fail)", s.ExitCode())
	}
}

// TestApplyObserved_StopAlreadyClosedCancelsEverything: a stop observed
// before the first request means nothing runs at all.
func TestApplyObserved_StopAlreadyClosedCancelsEverything(t *testing.T) {
	withLookPathAllowed(t)
	stop := make(chan struct{})
	close(stop)
	var ran atomic.Bool
	s := ApplyObserved([]Request{
		{Label: "a", Cmd: CmdSpec{Cmd: []string{"x"}}},
	}, func(req Request) Result {
		ran.Store(true)
		return Result{Label: req.Label}
	}, nil, stop)
	if ran.Load() {
		t.Error("no request may start after the stop is observed")
	}
	if len(s.Skipped) != 1 || s.Skipped[0].SkippedReason != "canceled" {
		t.Errorf("skipped = %+v, want one canceled", s.Skipped)
	}
}

// TestApply_NilObserverParity: Apply is exactly ApplyObserved with no
// observer and no stop — same buckets, same exit code.
func TestApply_NilObserverParity(t *testing.T) {
	withLookPathAllowed(t)
	reqs := []Request{
		{Label: "ok", Cmd: CmdSpec{Cmd: []string{"x", "1"}}},
		{Label: "bad", Cmd: CmdSpec{Cmd: []string{"x", "2"}}},
		{Label: "none", Cmd: CmdSpec{}},
	}
	exec := stubExecutor(map[string]Result{
		"ok":  {ExitCode: 0},
		"bad": {Failed: true, Err: errors.New("boom"), ExitCode: 1},
	})
	a := Apply(reqs, exec)
	b := ApplyObserved(reqs, exec, nil, nil)
	if len(a.Succeeded) != len(b.Succeeded) || len(a.Failed) != len(b.Failed) || len(a.Skipped) != len(b.Skipped) {
		t.Errorf("Apply buckets %+v != ApplyObserved buckets %+v", a, b)
	}
	if a.ExitCode() != b.ExitCode() {
		t.Errorf("exit codes differ: %d vs %d", a.ExitCode(), b.ExitCode())
	}
}
