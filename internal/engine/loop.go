// Package engine holds the folder-discovery and watch-loop orchestration
// shared between cmd/gpsync (the CLI, with its own colored-terminal output)
// and cmd/gpsync-tray (the systray wrapper, which has no terminal at all) --
// each wires the same logic to its own callbacks/output.
package engine

import "time"

// RunWatchLoop drives gpsync watch's (and gpsync-tray's) live loop: a live change
// (ready) always takes priority over backlog catch-up, but the two never
// run concurrently -- this whole loop is single-threaded, so backlog and
// live work take turns rather than overlapping. That matters because the
// circuit breaker's "a throttle pauses the WHOLE pipeline" guarantee is
// per Uploader.Run() call: two concurrent calls (one for backlog, one for
// a live change) would each have their own breaker state, and neither
// would know the other had just been throttled.
//
// Driven by plain channels rather than concrete *watcher.Watcher/
// *time.Ticker types, and with the actual work injected as callbacks
// (onLiveChange/onBacklogItem/onHeartbeat/onWatchErr) rather than calling
// any particular sync function directly, so the prioritization logic
// itself is unit-testable without a real filesystem watcher, a real
// ticker, or a real ledger -- and so it's reusable as-is by both callers
// above, each supplying its own idea of "do one folder cycle". stop is nil
// in production (a nil channel never becomes ready in a select, so that
// case is simply never chosen -- the loop runs until the caller's own
// shutdown path, e.g. cmd/gpsync's handleInterrupts, ends the process); tests
// close it to end the loop deterministically once they've driven the
// scenario they want.
func RunWatchLoop(
	onLiveChange func(folder string),
	onBacklogItem func(folder string, remaining int),
	onHeartbeat func(),
	onWatchErr func(err error),
	backlog []string,
	ready <-chan string,
	heartbeat <-chan time.Time,
	watchErrs <-chan error,
	stop <-chan struct{},
) {
	for {
		if len(backlog) == 0 {
			// Nothing left to catch up on: block normally.
			select {
			case folder := <-ready:
				onLiveChange(folder)
			case <-heartbeat:
				onHeartbeat()
			case werr := <-watchErrs:
				onWatchErr(werr)
			case <-stop:
				return
			}
			continue
		}
		// Backlog remains: a live signal already waiting takes priority;
		// otherwise spend this iteration on the next backlog folder
		// instead of blocking.
		select {
		case folder := <-ready:
			onLiveChange(folder)
		case <-heartbeat:
			onHeartbeat()
		case werr := <-watchErrs:
			onWatchErr(werr)
		case <-stop:
			return
		default:
			folder := backlog[0]
			backlog = backlog[1:]
			onBacklogItem(folder, len(backlog))
		}
	}
}
