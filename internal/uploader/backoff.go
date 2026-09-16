package uploader

import (
	"context"
	"sync"
	"time"
)

// throttleCircuitBreakerSchedule is how long the WHOLE pipeline pauses
// after each successive throttle, one rung per consecutive throttling pass.
//
// This is a run-wide circuit breaker, not a per-file backoff, and that
// distinction is the entire point. Per-file retries meant every throttled
// file backed off on its own clock while its siblings carried on at full
// concurrency, so the aggregate request rate against Google barely moved --
// observed in practice as a sustained "concurrent write request" throttle
// that simply never cleared, because gpsync itself was keeping it alive. The
// only thing that actually clears a rate limit is genuinely stopping.
//
// A package var, not a const, so tests can shorten it (same pattern as
// uploadURL/batchCreateURL above).
//
// Extended from a 15m ceiling to 1h (two new rungs, 30m/1h), per a direct
// report of real, sustained throttling that outlasted the old final rung
// again and again: "Google is killing me with the rate limits and 'Quota
// exceeded for quota concurrent write request'... is all over the place,
// even after the 15m rung. i think we need to add two more tiers, 30m and
// 1h." Every downstream consumer (the "gave up after N steps" message,
// TotalRungs shown in the live countdown, the schedule-exhausted "one more
// final rest" branch) derives the step count/duration from this slice's
// own length, so nothing else needed to change to pick this up.
//
// The original first rung (5s) was dropped entirely, not just lengthened,
// per direct user research into Google's own documented guidance for this
// exact error: "On 429, wait at least 30 seconds before retrying, then
// exponential backoff with jitter... Fast retries on a 429 count as more
// concurrent writes and dig the hole deeper." A 5s first retry directly
// violates that documented minimum, and per the same finding, doing so
// isn't just wasted time -- while the account is still in the "hole" a
// burst of requests dug, retrying too fast makes the block WORSE and
// likely prolongs it.
//
// The 30s/1m/3m rungs were then dropped too, on MEASURED evidence rather
// than guidance -- the first tuning change this project has made from its
// own throttle_events data (Phase 5's whole purpose). Over 261 events /
// 71 hours, measuring what each rung's wait actually BUYS (files uploaded
// between the end of that wait and the next throttle):
//
//	rung   wait   events   files after   files/event   bought nothing
//	  0     30s      45            41           0.9        89%
//	  1      1m      40             1           0.0        98%
//	  2      3m      35            62           1.8        83%
//	  3      5m      26           709          27.3        88%
//	  4     10m      25         1,327          53.1        64%
//	  7      1h      51         7,306         143.3        53%
//
// Short waits simply do not work on this account: 40 retries after a
// 1-minute wait landed ONE file. Productivity rises monotonically with
// wait length, and roughly three quarters of all successful uploads in
// that window arrived after a 1-hour wait. The three shortest rungs
// produced 104 files across 120 events combined, while costing ~4.5
// minutes and three extra 429-provoking retries on the way up to the
// rungs that actually work.
//
// Note what this evidence does NOT say: the ceiling is not too long. A
// pre-analysis hypothesis that the 1h rung was over-waiting was exactly
// backwards -- it is where 65% of all waiting time goes AND where most of
// the throughput comes from, so shortening it would remove the only rung
// that reliably clears. The waiting isn't the waste; the futile climb to
// it was.
var throttleCircuitBreakerSchedule = []time.Duration{
	5 * time.Minute,
	10 * time.Minute,
	15 * time.Minute,
	30 * time.Minute,
	1 * time.Hour,
}

// maxThrottleStrikes caps how many times ONE specific file gets thrown at
// Google and throttled before Run() sets it aside (failed_retryable,
// picked up by the next Run()) instead of keeping it at the front of every
// retry pass. This does NOT reintroduce the per-file backoff the comment
// above rejects -- the pipeline-wide pause (throttleCircuitBreakerSchedule)
// is completely unchanged, still paused in full before ANY new dispatch
// resumes. All this changes is which files get tried FIRST once the
// pipeline resumes: undispatched files (never even attempted) always go
// first, then throttled-but-under-the-cap files, and a file that's used up
// its strikes steps out of the way entirely rather than being retried
// again and again while files behind it in the queue never get a turn.
// See Run()'s strike-tracking below. Without the cap, one large video
// could absorb every retry in a pass while untried files waited behind
// it; the set-aside file is picked back up once the queue drains.
const maxThrottleStrikes = 3

// CircuitBreakerState carries the breaker's rung position ACROSS multiple
// Run() calls in one multi-folder invocation (gpsync sync/upload processing
// several folders, gpsync watch draining a backlog) -- exactly the same
// reason quota.AdaptiveConcurrency is shared the same way: a fresh state
// per folder throws away what the run just learned.
//
// Without this, a sustained throttle spanning many folders showed as
// "retrying every 5s forever": each new folder's Run() call started a
// brand-new breaker at rung 0, got throttled again almost immediately
// since Google was still limiting, and never got the chance to escalate
// before the NEXT folder boundary reset it right back to 0, seen while
// draining a large backlog under a sustained throttle.
//
// Passing nil to New() builds a fresh, unshared state (Run() also
// lazy-inits one internally if u.breaker is nil, for callers -- mostly
// tests -- that construct an Uploader directly without going through
// New()), which is correct for a single-call invocation and keeps direct
// construction simple in tests that don't care about cross-call
// persistence.
type CircuitBreakerState struct {
	mu   sync.Mutex
	rung int
	// cleanUploads counts real, successful uploads banked toward the next
	// rung decay -- see decayedRung's own doc comment for why this
	// replaced an earlier elapsed-clean-TIME measurement.
	cleanUploads int
}

func NewCircuitBreakerState() *CircuitBreakerState {
	return &CircuitBreakerState{}
}

// decayUploadsPerRung is how many real, successful uploads bank toward
// easing the breaker back down one rung -- a direct, concrete request:
// "after x successful uploads, i would expect to ease on the ramp-up..
// (basically start the ramp down) - so 10 successful uploads - one rung
// down." Cascades the same way the elapsed-time version it replaced did:
// a big enough batch of successes in one pass can drop several rungs at
// once, not just one.
const decayUploadsPerRung = 10

// decayedRung applies the ramp-down (see Run()'s own doc comment on the
// algorithm) and returns the resulting rung: every decayUploadsPerRung
// real successful uploads banked since the last rung change drops one
// rung. Called once per throttle detected, before that throttle's own
// escalation is applied.
//
// progressCount is how many files THIS pass actually got through before
// throttling again -- 0 is common and correct (the pipeline resumed and
// was throttled again before accomplishing anything), and contributes
// nothing toward decay, which is exactly the point: only real, confirmed
// uploads count as evidence the pipeline can work again, never elapsed
// time on its own. An earlier version measured elapsed wall-clock time
// against the rung's own duration instead -- technically it also gated on
// "at least one file got through this pass" (hadProgress), which caught
// the worst case (a sustained throttle that never once succeeds "earning"
// a decay purely from burning a few real seconds per failed attempt) --
// but it couldn't distinguish "one file limped through, then a long
// unproductive stretch" from genuine sustained recovery, and had no
// live/visible way to show progress between throttles. Counting real
// uploads directly measures the thing that actually matters.
func (s *CircuitBreakerState) decayedRung(progressCount int) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cleanUploads += progressCount
	for s.rung > 0 && s.cleanUploads >= decayUploadsPerRung {
		s.cleanUploads -= decayUploadsPerRung
		s.rung--
	}
	return s.rung
}

// sleepFn is time.Sleep for the PER-FILE transient backoff, indirected so
// tests can drive the clock instead of really waiting. Only real backoffs go
// through it -- never the dispatcher's slot-polling loop, which must keep
// ticking for real. The circuit breaker's pauses use waitFn instead.
var sleepFn = time.Sleep

// backoffTickInterval is how often a circuit-breaker pause reports its
// remaining time. A var so tests can shorten it.
var backoffTickInterval = time.Second

// waitFn performs one circuit-breaker pause of the given total duration,
// calling onTick with the time still remaining -- immediately, and then
// roughly once per backoffTickInterval until the pause is over.
//
// It sleeps in short steps rather than one long call specifically so the
// pause is not a black hole: dispatch is stopped, so no upload progress
// events fire, and without these ticks a 15-minute rung looks exactly like
// a hang. Indirected for tests, same as sleepFn.
var waitFn = realBackoffWait

// BackoffStatus is the live state of a circuit-breaker pause, reported
// about once a second so a caller can render a countdown rather than
// freezing with no explanation. See Uploader.onBackoff.
type BackoffStatus struct {
	// Active is false exactly once at the end of each pause, to clear the
	// display. Every other field is meaningless when Active is false.
	Active bool
	// Reason is the throttle message that tripped the breaker.
	Reason string
	// Rung is 1-based: which step of throttleCircuitBreakerSchedule this
	// pause is, out of TotalRungs.
	Rung       int
	TotalRungs int
	// RungWait is this rung's full duration; RemainingWait counts down from
	// it toward zero across the ticks of one pause.
	RungWait      time.Duration
	RemainingWait time.Duration
	// Concurrency is the level dispatch will resume at (already reduced by
	// the throttle); MaxConcurrency is the configured ceiling.
	Concurrency    int
	MaxConcurrency int
}

// waitInterruptible wraps waitWithBackoffStatus with the ability to be cut
// short early by a manual Retry Now click (u.skip), returning true if the
// rung should be treated as having completed -- either because it ran its
// full course, or because a skip request expired it early (see
// SkipSignal's doc comment for why an early skip is never handled any
// differently from a normal completion beyond timing).
//
// This used to be inlined only at the ONE call site below for the normal
// per-rung wait, which left a second wait site without the same
// treatment. Once breaker.decayedRung() reports the
// schedule already exhausted, Run() pays for one final rest at the last
// rung using what used to be a bare waitWithBackoffStatus(ctx, ...) call --
// no watcher on u.skip.C() at all, so Retry Now was a silent no-op the
// entire time the breaker sat at the final rung and kept re-throttling.
// Not a race like the earlier SkipSignal bugs -- a plain coverage gap,
// one of two call sites never wired up. Both now share this
// one helper so a future third wait site can't reintroduce the same gap.
func (u *Uploader) waitInterruptible(ctx context.Context, wait time.Duration, reason string, rung int) bool {
	waitCtx := ctx
	cancelWait := func() {}
	var stopWatch, watcherDone chan struct{}
	// skipped is written ONLY by the watcher goroutine below, and read
	// ONLY after <-watcherDone joins it (see the close(stopWatch) block)
	// -- that join is what makes it safe to read here without its own
	// synchronization, not a defer/atomic. See this function's own doc
	// comment and internal/uploader's git history for the two distinct
	// races this mechanism has already had ("works once, then does
	// nothing" -- a close-and-replace channel; "stopped working again" --
	// this same read-before-join gap, originally only fixed at the other
	// call site).
	skipped := false
	if u.skip != nil {
		waitCtx, cancelWait = context.WithCancel(ctx)
		stopWatch = make(chan struct{})
		watcherDone = make(chan struct{})
		go func() {
			defer close(watcherDone)
			select {
			case <-u.skip.C():
				skipped = true
				cancelWait()
			case <-stopWatch:
			}
		}()
	}
	completed := u.waitWithBackoffStatus(waitCtx, wait, reason, rung)
	cancelWait()
	// Explicit close, NOT defer -- every call site sits inside Run()'s own
	// loop, and defer only runs when the ENCLOSING FUNCTION returns, not
	// at the end of a loop iteration. A deferred close here would leak one
	// goroutine+channel per throttled pass for the rest of Run()'s call.
	if stopWatch != nil {
		close(stopWatch)
		<-watcherDone // join before reading `skipped` -- see the doc comment above
	}
	if !completed && skipped {
		completed = true
	}
	return completed
}

// waitWithBackoffStatus sits out one circuit-breaker rung, reporting the
// countdown through onBackoff about once a second and clearing that status
// once the pause is over.
//
// The trailing Active:false always fires, whatever happens next -- whether
// the retry pass that follows succeeds, throttles again (which starts a
// fresh pause and sets it active once more), or the schedule runs out and
// the run aborts. That makes it impossible to leave a stale "paused"
// display on screen while the pipeline is actually running again.
// waitWithBackoffStatus sits out one circuit-breaker rung, returning true if
// it ran to completion or false if ctx was cancelled partway through.
func (u *Uploader) waitWithBackoffStatus(ctx context.Context, wait time.Duration, reason string, rung int) bool {
	if u.onBackoff == nil {
		// Still go through waitFn rather than sleeping directly, so tests
		// that stub the clock work identically with and without a callback.
		return waitFn(ctx, wait, func(time.Duration) {})
	}
	maxConcurrency := u.cfg.Concurrency
	if maxConcurrency < 1 {
		maxConcurrency = 1
	}
	completed := waitFn(ctx, wait, func(remaining time.Duration) {
		u.onBackoff(BackoffStatus{
			Active:         true,
			Reason:         reason,
			Rung:           rung + 1,
			TotalRungs:     len(throttleCircuitBreakerSchedule),
			RungWait:       wait,
			RemainingWait:  remaining,
			Concurrency:    u.concurrency.CurrentLimit(),
			MaxConcurrency: maxConcurrency,
		})
	})
	u.onBackoff(BackoffStatus{Active: false})
	return completed
}
