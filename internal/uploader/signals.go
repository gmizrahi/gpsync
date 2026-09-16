package uploader

import (
	"context"
	"sync"
	"sync/atomic"
)

// SkipSignal is a one-shot, cross-goroutine "expire the current
// countdown right now" request. It began as the manual override for
// maxThrottleStrikes -- skip this file now rather than waiting out its
// remaining attempts -- and was extended to interrupt an in-progress
// circuit-breaker wait too, rather than only applying at the next natural
// pass boundary. Whatever the wait was going to do when it expired
// happens immediately instead: retry the same file if its strikes are not
// yet spent, or move on to the next one.
//
// That distinction is doing real work: this is NOT a "always force
// everything to bench" switch (an earlier version of this was, and was
// corrected). A skip request only ever makes an in-progress wait
// complete SOONER -- the very next opportunity, either immediately (a
// wait is currently running -- see the C() channel below and Run()'s
// watcher goroutine around each waitWithBackoffStatus call) or at the
// already-imminent next pass boundary otherwise (see Run()'s existing
// Take() check). Every consequence of that -- the breaker escalating,
// and each throttled row individually either retrying (strikes still
// under maxThrottleStrikes) or getting benched (strikes exhausted) --
// runs through the exact same code a NORMALLY completed wait already
// used, unmodified. Nothing about SkipSignal itself decides retry vs.
// bench; it only decides WHEN "whatever was going to happen anyway"
// happens.
//
// Interrupting an in-progress wait early is still a real, accepted
// tradeoff: that pause exists because Google's aggregate rate limit
// hasn't cleared yet, which has nothing to do with any ONE file, so
// ending it early risks the very next dispatch attempt re-tripping the
// same throttle immediately (which just escalates the breaker again, as
// normal). The alternative -- acknowledge the click but let the countdown
// run -- was rejected: a Retry Now that visibly does nothing is worse
// than one that risks re-tripping the throttle.
// ch is buffered (capacity 1), and deliberately NEVER replaced. Retry Now
// used to work exactly once and then do nothing.
// An earlier version used a close-and-
// replace broadcast channel (a fresh Request() closed the old one and
// swapped in a new one), which has a genuine race: a watcher goroutine
// only captures "whichever channel is current" at the moment it calls
// C(), so a click landing in the window between one wait's watcher
// stopping and the next wait's watcher starting (however brief --
// dispatchPass making real HTTP requests in between, say) closed a
// channel nobody was listening to. The click wasn't lost outright --
// Take()'s `requested` bool still carried it forward -- but it silently
// downgraded from "cut the current wait short" to "apply whenever some
// LATER wait happens to complete naturally," which is exactly what a
// second click doing nothing visible looks like. A buffered channel
// can't have this problem: Request()'s send either reaches an already-
// listening watcher immediately, or sits in the 1-slot buffer until the
// NEXT watcher's very first receive, whichever comes first -- there is
// no gap where a signal can go unheard.
type SkipSignal struct {
	requested atomic.Bool
	ch        chan struct{}
}

func NewSkipSignal() *SkipSignal { return &SkipSignal{ch: make(chan struct{}, 1)} }

// FileCancelRegistry lets a caller interrupt ONE SPECIFIC file's ACTIVE
// byte-upload transfer, immediately, by path -- independent of any
// throttle/pause state. This is a genuinely different feature from
// SkipSignal above, per direct clarification: "i didn't want to have the
// skip for when i'm inside the throttle window, i wanted to have a skip
// button to cancel a sync of a file and put it in a later-retry bucket."
// SkipSignal answers "give up on whatever's currently held for a
// throttle retry"; this answers "stop THIS file's upload right now,
// whether or not anything is throttled" -- e.g. a large, slow video the
// user doesn't want to wait on right now.
//
// A worker registers its per-file cancel func for the duration of ONE
// uploadBytesAttempt call (see there); Cancel looks it up by path and
// calls it, which aborts that file's in-flight HTTP request via context
// cancellation (uploadBytesAttempt classifies the resulting error as
// retryx.Cancelled, which handleError's default case sends straight to
// failed_retryable -- the same "picked up by the next run" bucket
// maxThrottleStrikes already uses, just reached a different way).
type FileCancelRegistry struct {
	mu      sync.Mutex
	cancels map[string]context.CancelFunc
}

func NewFileCancelRegistry() *FileCancelRegistry {
	return &FileCancelRegistry{cancels: make(map[string]context.CancelFunc)}
}
