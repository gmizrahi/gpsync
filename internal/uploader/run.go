package uploader

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/gmizrahi/gpsync/internal/extensions"
	"github.com/gmizrahi/gpsync/internal/retryx"
	"github.com/gmizrahi/gpsync/internal/statedb"
)

type Stats struct {
	Uploaded         int
	FailedPermanent  int
	FailedRetryable  int
	SkippedQuota     int
	SkippedMediaType int // excluded by cfg.MediaTypeFilter -- still pending, just out of scope for THIS run
}

// passResult reports how one dispatch pass ended.
type passResult struct {
	// throttled is true if ANY file in this pass came back
	// throttle-classified -- i.e. the circuit breaker tripped.
	throttled bool
	// throttleMessage is the first throttle's message text, for reporting.
	throttleMessage string
	// remaining is everything from this pass's queue that is still not
	// resolved: throttledRows plus undispatchedRows, concatenated
	// (throttledRows first). Kept for callers that only need a
	// count/list of "everything left" without caring about the
	// distinction (the daily-quota/schedule-exhausted/interrupted abort
	// paths in Run(), which mark or count everything the same way either
	// way).
	remaining []statedb.Upload
	// throttledRows is the subset of remaining that actually received a
	// throttle response THIS pass -- a real signal from Google, as
	// opposed to undispatchedRows below, which never even got a chance to
	// try. Run()'s per-file strike tracking (maxThrottleStrikes) only
	// counts against a row appearing here.
	throttledRows []statedb.Upload
	// undispatchedRows never got dispatched at all this pass (the breaker
	// or the daily-quota check stopped the dispatcher before they got a
	// turn). Always retried first next pass, ahead of throttledRows under
	// the strike cap -- a real answer from Google about one stubborn file
	// must never keep starving files that haven't even been tried yet.
	undispatchedRows []statedb.Upload
}

func (u *Uploader) Run() (Stats, error) {
	tally := &runTally{}

	if _, err := u.db.RequeueRetryable(); err != nil {
		return tally.stats, err
	}
	pending, err := u.db.ListPendingUnder(u.scopeFolders)
	if err != nil {
		return tally.stats, err
	}

	var rows []statedb.Upload
	for _, row := range pending {
		ext := extensions.ExtOf(row.FirstSourcePath)
		if extensions.Classify(row.FirstSourcePath) == extensions.Unsupported {
			reason := fmt.Sprintf("unsupported extension '.%s'", ext)
			u.dbErr("recording unsupported-extension failure for "+row.FirstSourcePath,
				u.db.MarkFailed(row.SHA256, true, "UNSUPPORTED_EXTENSION",
					fmt.Sprintf("'.%s' is a known-unsupported format", ext)))
			u.dbErr("recording extension stats for ."+ext, u.db.ExtRecord(ext, 1, 0, 1, "known-unsupported extension"))
			tally.stats.FailedPermanent++
			if u.onSkip != nil {
				u.onSkip(row.FirstSourcePath, reason)
			}
			continue
		}
		// --media-type: excluded rows stay 'pending' untouched -- unlike
		// the unsupported-extension case above, this isn't a rejection,
		// just out of scope for THIS run. A later run with a different (or
		// no) --media-type picks them right back up.
		if u.cfg.MediaTypeFilter != extensions.KindUnknown && extensions.KindOf(row.FirstSourcePath) != u.cfg.MediaTypeFilter {
			tally.stats.SkippedMediaType++
			if u.onSkip != nil {
				u.onSkip(row.FirstSourcePath, fmt.Sprintf("media-type filter: %s only", u.cfg.MediaTypeFilter))
			}
			continue
		}
		rows = append(rows, row)
	}
	// --smallest-first: get quick, cheap files done (and counted) early
	// rather than having a handful of huge videos hold up everything behind
	// them. Stable so files of equal size keep ListPendingUnder's own
	// first_source_path order rather than shuffling arbitrarily.
	if u.cfg.SortSmallestFirst {
		sort.SliceStable(rows, func(i, j int) bool { return rows[i].Size < rows[j].Size })
	}
	tally.total = len(rows)

	if err := u.db.RunStart("upload", 0, len(rows)); err != nil {
		return tally.stats, err
	}

	// One pass per circuit-breaker rung. A pass dispatches until it either
	// drains its queue or is stopped (by a throttle or by daily-quota
	// exhaustion); anything left over comes back as pass.remaining and is
	// re-queued for the next pass after the breaker's sleep.
	queue := rows
	var waited time.Duration
	// breaker's rung position persists across folders (see New()'s doc
	// comment) when u.breaker was explicitly shared by the caller; a
	// direct &Uploader{} construction (mostly tests) that never set it
	// gets a fresh, unshared one here instead, matching Run()'s old
	// always-local behavior.
	breaker := u.breaker
	if breaker == nil {
		breaker = NewCircuitBreakerState()
	}
	ctx := u.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	// strikes counts how many times EACH file has actually been thrown at
	// Google and throttled, across every pass of this Run() call -- see
	// maxThrottleStrikes' doc comment for why a file that hits the cap
	// gets set aside instead of kept at the front of every retry pass.
	// Scoped to this one Run() call, not persisted like the breaker's rung:
	// a fresh Run() (the next invocation, or gpsync-tray's next heartbeat)
	// gives every file a full fresh set of strikes again.
	strikes := make(map[string]int)
	for len(queue) > 0 {
		uploadedBefore := tally.stats.Uploaded
		pass := u.dispatchPass(ctx, queue, tally)
		progressCount := tally.stats.Uploaded - uploadedBefore

		if ctx.Err() != nil {
			// Interrupted before this pass finished dispatching its queue
			// -- gpsync-tray's graceful Quit/Pause ("don't push more uploads,
			// just finish what's already transferring"). Checked before
			// quotaHitThisRun/pass.throttled below: a user-requested stop
			// takes priority over how the pass would otherwise have been
			// classified. Nothing in pass.remaining here was ever attempted
			// (dispatchPass only ever adds to it via `undispatched`, never
			// a real throttle response, once ctx is cancelled), so it needs
			// no ledger write at all -- it's already sitting there
			// `pending`, exactly like an undispatched daily-quota tail.
			abort := &AbortError{
				Reason:    AbortInterrupted,
				Detail:    "stopped before finishing this folder's queue",
				Remaining: len(pass.remaining),
			}
			u.dbErr("recording run outcome", u.db.RunEnd(abort.LedgerReason(), abort.Summary()))
			return tally.stats, abort
		}

		if u.quotaHitThisRun.Load() {
			// Genuine daily-quota exhaustion: nothing this run can do about
			// it, and it applies to the whole GCP project -- so subsequent
			// folders in this invocation would hit exactly the same wall.
			// Whatever is left stays `pending` in the ledger and is picked
			// up by the next invocation after the Pacific-midnight reset.
			tally.stats.SkippedQuota += len(pass.remaining)
			used, _ := u.dailyQuota.Used()
			abort := &AbortError{
				Reason:    AbortDailyQuota,
				Detail:    fmt.Sprintf("daily API quota exhausted (%d/%d requests used today)", used, u.dailyQuota.DailyLimit()),
				Remaining: len(pass.remaining),
			}
			// Record HOW this ended, not just that it stopped -- otherwise
			// `gpsync info` can't tell an abandoned run from a finished one.
			u.dbErr("recording run outcome", u.db.RunEnd(abort.LedgerReason(), abort.Summary()))
			return tally.stats, abort
		}

		if !pass.throttled {
			// Clean pass: nothing throttled, so the dispatcher ran the queue
			// to the end and every file is accounted for -- which is also
			// how the breaker fully resets (rung would decay to 0 anyway
			// given enough clean time -- this is just the immediate case).
			break
		}

		// Smart ramp-down: real successful uploads banked since the last
		// escalation ease the rung back down before this new throttle is
		// applied, symmetric with how it climbed -- a direct, concrete
		// request: "after x successful uploads, i would expect to ease on
		// the ramp-up.. (basically start the ramp down) - so 10
		// successful uploads - one rung down." A big enough batch of
		// successes can cascade down multiple rungs at once.
		//
		// Without this, a throttle right after a long recovery cost
		// strictly more than the last one no matter how well things had
		// been going in between -- purely because SOME throttle occurred
		// again eventually, which on a large enough folder it always will.
		// That's the exact bug report this fixed: "we're at the 3m mark,
		// back up fine for a while, then it gets stuck again and jumps
		// straight to 5m -- it wasn't a full failure in between, why did
		// the wait get longer?"
		//
		// breaker.decayedRung() banks progressCount (this pass's own real
		// upload successes -- 0 is common and correct) against whatever's
		// already accumulated since the LAST rung change, whether that
		// change happened earlier in this very call or in a PREVIOUS
		// folder's Run() call against the same shared breaker -- see
		// CircuitBreakerState's doc comment for the "retrying every 5s
		// forever" bug this half fixes, and decayedRung's own doc comment
		// for why counting real uploads (not elapsed time) is what
		// actually measures recovery.
		rung := breaker.decayedRung(progressCount)

		if rung >= len(throttleCircuitBreakerSchedule) {
			// The schedule is exhausted: we have already waited every rung
			// including the final one, and Google is STILL throttling.
			//
			// Giving up here is deliberately script-appropriate: `gpsync` is
			// invoked, does what it can, and exits so the user (or the
			// scheduler that started it) gets control back, with everything
			// unfinished safely queued for the next invocation. gpsync watch
			// (Phase 2) is the one long-running caller, and it deliberately
			// does NOT teach this loop to keep cycling forever either --
			// its periodic heartbeat just calls Run() again later instead,
			// which this same shared breaker still gives a real chance to
			// recover from once enough clean time has passed (see
			// CircuitBreakerState.decayedRung).
			//
			// waited==0 here means THIS Run() call never actually climbed
			// any rung itself -- it inherited an already-exhausted breaker
			// from an EARLIER Run() call against the same shared
			// *CircuitBreakerState (gpsync-tray only: each folder cycle gets
			// its own Run() call, but they all share one breaker). Without
			// the wait below, that meant every single subsequent Run()
			// call -- one per folder in a baseline sweep, or one per live
			// filesystem event, however many arrived before the breaker
			// ever got a chance to decay -- aborted INSTANTLY with zero
			// backoff, forever, as soon as its first real dispatch attempt
			// got throttled again -- the backoff appeared to stop
			// working entirely: no wait ever visibly happened again,
			// and worse, it meant gpsync kept
			// hammering an endpoint that was still actively throttling it
			// with a real HTTP request every single folder cycle, exactly
			// what the circuit breaker exists to prevent. Paying for one
			// real rest at the final (longest) rung before giving up --
			// exactly once, since there's nothing past the ceiling to
			// escalate to -- restores a genuine pause before control
			// returns to the caller's own natural retry cadence. A call
			// that DID climb at least one rung itself this time
			// (waited>0, the ordinary schedule-exhaustion case a fresh
			// breaker or one resuming mid-ladder hits) already paid for
			// real backoff and gives up immediately, unchanged.
			if waited == 0 {
				finalRung := len(throttleCircuitBreakerSchedule) - 1
				finalWait := throttleCircuitBreakerSchedule[finalRung]
				if u.onThrottle != nil {
					u.onThrottle(fmt.Sprintf(
						"%s — pausing ALL uploads for %s (already at the final backoff step), then retrying %d file(s)",
						truncateThrottleReason(pass.throttleMessage), finalWait, len(pass.remaining)),
						finalWait.Seconds(), u.concurrency.CurrentLimit())
				}
				// Permanent record for the "what's the real recovery
				// window" investigation this backs -- see
				// statedb.RecordThrottleEvent's own doc comment.
				u.dbErr("recording throttle event", u.db.RecordThrottleEvent(finalRung, finalWait.Seconds(), pass.throttleMessage))
				if !u.waitInterruptible(ctx, finalWait, pass.throttleMessage, finalRung) {
					abort := &AbortError{
						Reason:    AbortInterrupted,
						Detail:    pass.throttleMessage,
						Waited:    waited,
						Steps:     len(throttleCircuitBreakerSchedule),
						Remaining: len(pass.remaining),
					}
					u.dbErr("recording run outcome", u.db.RunEnd(abort.LedgerReason(), abort.Summary()))
					return tally.stats, abort
				}
				u.client.CloseIdleConnections()
				waited += finalWait
			}
			message := fmt.Sprintf("gave up after %s of throttle backoff — %s", waited.Round(time.Second), pass.throttleMessage)
			abort := &AbortError{
				Reason:    AbortThrottleBackoffExhausted,
				Detail:    pass.throttleMessage,
				Waited:    waited,
				Steps:     len(throttleCircuitBreakerSchedule),
				Remaining: len(pass.remaining),
			}
			u.markRemainingRetryable(pass.remaining, tally, message, false)
			u.dbErr("recording run outcome", u.db.RunEnd(abort.LedgerReason(), abort.Summary()))
			return tally.stats, abort
		}

		wait := throttleCircuitBreakerSchedule[rung]
		// onThrottle writes the permanent scrollback record of this pause;
		// onBackoff below drives the live countdown. Complementary, not
		// duplicated: one is history, the other is current state.
		if u.onThrottle != nil {
			u.onThrottle(fmt.Sprintf(
				"%s — pausing ALL uploads for %s (backoff step %d of %d), then retrying %d file(s)",
				truncateThrottleReason(pass.throttleMessage), wait, rung+1, len(throttleCircuitBreakerSchedule), len(pass.remaining)),
				wait.Seconds(), u.concurrency.CurrentLimit())
		}
		// Permanent record for working out the real recovery window: how
		// long a throttle actually lasts before uploads resume.
		// Deliberately NOT logged for the separate daily-quota onThrottle
		// calls elsewhere in this file -- that's a different, documented,
		// once-a-day phenomenon that would contaminate this analysis. See
		// statedb.RecordThrottleEvent's own doc comment.
		u.dbErr("recording throttle event", u.db.RecordThrottleEvent(rung, wait.Seconds(), pass.throttleMessage))
		// A skip request during this wait "expires" the countdown early --
		// per direct clarification of what this button should actually
		// do: "we will zero the counter so the retry happens immediately,
		// basically it expires the current countdown. so whatever it
		// planned to do i[n] X amount of seconds - will happen now,
		// whether... to retry the same file (because we didn't count to
		// 3 yet) or to move on to the next file". That's exactly what
		// already happens once a wait completes NORMALLY (escalate the
		// breaker, then the skipNow-or-strikes bench/retry decision per
		// row below) -- so an early skip doesn't need any bespoke
		// handling of its own; it just needs `completed` to come back
		// true sooner than the full rung's duration, and every line
		// after this block runs completely unchanged either way.
		//
		// waitCtx is a child of ctx specifically for this one wait:
		// cancelled either by a genuine ctx cancel (Pause/Quit, still
		// handled below exactly as before -- skippedEarly distinguishes
		// the two) or by the skip watcher goroutine below. Deliberately
		// does NOT call u.skip.Take() itself -- leaving the request
		// queued for the EXISTING `skipNow := u.skip.Take()` check
		// further down is what makes this a normal completion in every
		// other respect, not a special case.
		completed := u.waitInterruptible(ctx, wait, pass.throttleMessage, rung)
		if !completed {
			// ctx cancelled mid-wait (gpsync-tray Pause/Quit): stop now instead
			// of finishing out the rung. pass.remaining is still sitting
			// there `pending` -- nothing here was ever actually confirmed
			// as failed, it was just paused -- exactly like the
			// mid-dispatch interrupt case above, and this needs to match
			// it: an earlier version called markRemainingRetryable here,
			// which was a real bug ("the throttle mechanism is ruined"):
			// under a global sync strategy pass.remaining can be the
			// ENTIRE backlog (tens of thousands of files), and a Pause
			// landing mid-throttle-wait dumped ALL of them into
			// failed_retryable at once, each firing its own onProgress
			// "FAIL" event -- flooding Recent Activity and the Retryable
			// stat with a wall of identical-looking failures for files
			// that were never actually tried. Doubly pointless, since
			// RequeueRetryable (unscoped, run at the top of every Run())
			// would have swept them straight back to pending on the very
			// next cycle anyway -- the only real effect was the scary
			// display. Leaving them untouched here also matches what
			// Summary() already claims ("N file(s) left pending"), which
			// the old code was silently contradicting.
			abort := &AbortError{
				Reason:    AbortInterrupted,
				Detail:    pass.throttleMessage,
				Waited:    waited,
				Steps:     rung + 1,
				Remaining: len(pass.remaining),
			}
			u.dbErr("recording run outcome", u.db.RunEnd(abort.LedgerReason(), abort.Summary()))
			return tally.stats, abort
		}
		// u.client's transport keeps idle connections pooled indefinitely
		// (no IdleConnTimeout) -- fine normally, but every rung here is a
		// deliberate multi-minute idle gap, and Google's own load balancer
		// almost certainly closes idle connections well before the longer
		// rungs (5m/10m/15m). A silently-dropped pooled connection reused
		// after that looks identical to a hung server: the client writes
		// into it successfully and then just never gets a response, timing
		// out on ResponseHeaderTimeout instead of failing fast. Dropping
		// the pool here costs one fresh TLS handshake on the next request
		// -- negligible next to the minutes just spent paused -- and
		// guarantees dispatch resumes on a connection that's actually alive.
		u.client.CloseIdleConnections()
		waited += wait
		breaker.escalate()

		// Strike-count only the rows Google actually throttled this pass
		// (never undispatchedRows -- those haven't earned a strike, they
		// just haven't had a turn yet). A row that's used up its strikes
		// is set aside now (failed_retryable) rather than kept in the
		// retry set -- the next Run() (next invocation, or gpsync-tray's next
		// heartbeat) gives it a fresh set of strikes. Everyone else goes
		// back into queue for the next pass, undispatchedRows FIRST so a
		// file that's never even been tried always gets first crack ahead
		// of one that's already known to be stubborn but still has
		// strikes left.
		// A pending skip request is Take()n here (not read for its value --
		// see below) purely for the defensive channel-drain Take()'s own
		// doc comment describes: without it, a click landing just as THIS
		// wait finished naturally (before any watcher was there to
		// consume it) would sit buffered and falsely fire the NEXT wait's
		// watcher too.
		//
		// A real regression, caught by direct correction: "no, this is a
		// mistake, the retry now on the rung just zeroes the countdown,
		// that's it." This code had drifted from SkipSignal's own doc
		// comment just above it in this file -- which already explicitly
		// says a skip request is "NOT an always force everything to
		// bench switch" -- back into being exactly that: the boolean
		// used to be OR'd into the bench condition below, benching every
		// currently-throttled row the instant Retry Now was clicked,
		// regardless of its own strike count. Retry vs. bench is decided
		// by strikes alone now, exactly as if this rung had run its full
		// natural duration -- a skip request only ever changes WHEN that
		// decision happens, never WHAT it decides.
		if u.skip != nil {
			u.skip.Take()
		}
		var benched, retryNow []statedb.Upload
		for _, row := range pass.throttledRows {
			strikes[row.SHA256]++
			if strikes[row.SHA256] >= maxThrottleStrikes {
				benched = append(benched, row)
			} else {
				retryNow = append(retryNow, row)
			}
		}
		if len(benched) > 0 {
			benchedMsg := fmt.Sprintf("throttled %d times in a row — set aside so other files can proceed; will retry on the next run", maxThrottleStrikes)
			u.markRemainingRetryable(benched, tally, benchedMsg, false)
		}
		queue = append(pass.undispatchedRows, retryNow...)
	}

	return tally.stats, u.db.RunFinish()
}

// markRemainingRetryable writes off everything still unfinished when the
// run gives up, so the ledger explains why rather than leaving rows in a
// state that looks like they were never reached. failed_retryable (not
// permanent) is the point: the next `gpsync sync`/`gpsync upload` requeues them
// automatically via RequeueRetryable.
// cancelled marks a row as the user's own deliberate action rather than a
// real failure -- the SAME neutral "MANUAL-SKIP" dashboard/CLI treatment
// FileCancelRegistry's per-file skip already gets (see
// ProgressEvent.LastCancelled's own doc comment), instead of a red FAIL.
// Every current call site always passes false: a Retry Now click is NOT
// this kind of deliberate per-row action (see SkipSignal's own doc
// comment) -- it only ever ends the current wait sooner, and every row's
// retry-vs-bench outcome is still decided by strikes alone. This
// parameter stays a general primitive (and is tested directly, both
// ways) for whatever future path DOES need to mark a row cancelled
// rather than genuinely failed -- it just isn't reachable through the
// throttle-retry path itself right now.
func (u *Uploader) markRemainingRetryable(rows []statedb.Upload, tally *runTally, message string, cancelled bool) {
	kind := retryx.Throttle
	if cancelled {
		kind = retryx.Cancelled
	}
	for _, row := range rows {
		u.dbErr("recording throttle-abandoned file "+row.FirstSourcePath,
			u.db.MarkFailed(row.SHA256, false, "THROTTLE_BACKOFF_EXHAUSTED", message))
		tally.stats.FailedRetryable++
		tally.processed++
		if u.onProgress != nil {
			u.onProgress(ProgressEvent{
				Done: tally.processed, Total: tally.total,
				LastFile: row.FirstSourcePath, LastOK: false, LastSize: row.Size,
				LastErrorMessage: message, LastCancelled: cancelled,
				LastErrorKind: string(kind),
			})
		}
	}
	if len(rows) > 0 {
		u.dbErr("updating run progress", u.db.RunUpdate(nil, &tally.processed))
	}
}

// fileOutcome records the final result for one file, for progress
// reporting. errorMessage carries the same text written to the ledger when
// ok is false, so a live view can show WHY a file failed rather than just
// that it did.
type fileOutcome struct {
	path         string
	ok           bool
	size         int64
	errorMessage string
	// errorKind mirrors ProgressEvent.LastErrorKind -- see its doc comment.
	// Empty for ok==true and for the two per-item batchCreate failures
	// below, which have no retryx.Classification of their own to draw from
	// (no HTTP response to classify -- a missing/failed result INSIDE an
	// otherwise-200 batchCreate response); both land failed_retryable, so
	// "transient" describes them correctly.
	errorKind string
}
