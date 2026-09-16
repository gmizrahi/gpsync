package engine

import (
	"fmt"
	"reflect"
	"testing"
	"time"
)

// TestRunWatchLoop_LiveEventInjectedMidBacklog_JumpsAheadOfRemainingBacklog
// is the core behavior this whole redesign was for: a live change arriving
// WHILE the backlog is being worked through must be processed before the
// NEXT backlog item, not after the entire remaining backlog finishes --
// otherwise a large pending backlog (the reported real case: 40k queued
// files) means a freshly-dropped file isn't noticed for a very long time.
func TestRunWatchLoop_LiveEventInjectedMidBacklog_JumpsAheadOfRemainingBacklog(t *testing.T) {
	var processed []string
	ready := make(chan string, 1)
	stop := make(chan struct{})

	onLiveChange := func(folder string) {
		processed = append(processed, "live:"+folder)
	}
	onBacklogItem := func(folder string, remaining int) {
		processed = append(processed, fmt.Sprintf("backlog:%s(%d)", folder, remaining))
		if folder == "a" {
			// A real change lands WHILE "a" is being processed -- must be
			// seen before "b" gets its turn.
			ready <- "urgent"
		}
		if remaining == 0 {
			close(stop)
		}
	}

	RunWatchLoop(onLiveChange, onBacklogItem, func() {}, func(error) {},
		[]string{"a", "b", "c"}, ready, nil, nil, stop)

	want := []string{"backlog:a(2)", "live:urgent", "backlog:b(1)", "backlog:c(0)"}
	if !reflect.DeepEqual(processed, want) {
		t.Errorf("processed = %v, want %v", processed, want)
	}
}

// TestRunWatchLoop_LiveEventAlreadyWaitingGoesFirst covers the simpler
// case: a live change queued up before the loop even starts must still be
// handled before any backlog work begins.
func TestRunWatchLoop_LiveEventAlreadyWaitingGoesFirst(t *testing.T) {
	var processed []string
	ready := make(chan string, 1)
	ready <- "live1"
	stop := make(chan struct{})

	onLiveChange := func(folder string) {
		processed = append(processed, "live:"+folder)
	}
	onBacklogItem := func(folder string, remaining int) {
		processed = append(processed, fmt.Sprintf("backlog:%s(%d)", folder, remaining))
		if remaining == 0 {
			close(stop)
		}
	}

	RunWatchLoop(onLiveChange, onBacklogItem, func() {}, func(error) {},
		[]string{"a", "b"}, ready, nil, nil, stop)

	want := []string{"live:live1", "backlog:a(1)", "backlog:b(0)"}
	if !reflect.DeepEqual(processed, want) {
		t.Errorf("processed = %v, want %v", processed, want)
	}
}

// TestRunWatchLoop_BacklogProcessedInOrderWhenIdle proves the plain case
// (no live activity at all) still works: strict FIFO order, remaining
// count decrementing to 0.
func TestRunWatchLoop_BacklogProcessedInOrderWhenIdle(t *testing.T) {
	var processed []string
	stop := make(chan struct{})

	onBacklogItem := func(folder string, remaining int) {
		processed = append(processed, fmt.Sprintf("%s(%d)", folder, remaining))
		if remaining == 0 {
			close(stop)
		}
	}

	RunWatchLoop(func(string) {}, onBacklogItem, func() {}, func(error) {},
		[]string{"a", "b", "c"}, nil, nil, nil, stop)

	want := []string{"a(2)", "b(1)", "c(0)"}
	if !reflect.DeepEqual(processed, want) {
		t.Errorf("processed = %v, want %v", processed, want)
	}
}

// TestRunWatchLoop_HeartbeatFiresWhenBacklogEmpty proves the heartbeat
// path is reachable once there's no backlog left to occupy the loop.
func TestRunWatchLoop_HeartbeatFiresWhenBacklogEmpty(t *testing.T) {
	heartbeatCalls := 0
	heartbeat := make(chan time.Time, 1)
	heartbeat <- time.Now()
	stop := make(chan struct{})

	RunWatchLoop(func(string) {}, func(string, int) {}, func() {
		heartbeatCalls++
		close(stop)
	}, func(error) {}, nil, nil, heartbeat, nil, stop)

	if heartbeatCalls != 1 {
		t.Errorf("heartbeatCalls = %d, want 1", heartbeatCalls)
	}
}

// TestRunWatchLoop_WatchErrorsReported proves a watcher-reported error
// (e.g. a directory that couldn't be added) reaches the caller instead of
// being silently dropped.
func TestRunWatchLoop_WatchErrorsReported(t *testing.T) {
	var gotErr error
	watchErrs := make(chan error, 1)
	watchErrs <- fmt.Errorf("permission denied")
	stop := make(chan struct{})

	RunWatchLoop(func(string) {}, func(string, int) {}, func() {}, func(err error) {
		gotErr = err
		close(stop)
	}, nil, nil, nil, watchErrs, stop)

	if gotErr == nil || gotErr.Error() != "permission denied" {
		t.Errorf("onWatchErr got %v, want %q", gotErr, "permission denied")
	}
}
