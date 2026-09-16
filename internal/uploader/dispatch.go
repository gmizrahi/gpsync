package uploader

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gmizrahi/gpsync/internal/extensions"
	"github.com/gmizrahi/gpsync/internal/retryx"
	"github.com/gmizrahi/gpsync/internal/statedb"
)

type dispatchResult struct {
	row   statedb.Upload
	token string
	err   *retryx.Classification
}

// dispatchPass runs one continuous-dispatch pass over queue.
//
// Continuous dispatch, not discrete waves: a fixed-capacity semaphore
// (sized to the *configured* max concurrency) bounds how many byte uploads
// run at once, but as soon as any one finishes -- regardless of its
// siblings -- the freed slot is immediately handed the next pending row. A
// large/slow file (e.g. a multi-GB video) doesn't block smaller sibling
// files from starting, finishing, and being marked uploaded while it's
// still transferring.
//
// The pass ends early, without cancelling anything already in flight, the
// moment a throttle is seen, the daily quota is found exhausted, or ctx is
// cancelled (gpsync-tray's graceful Quit/Pause: "stop dispatching new work,
// but let whatever's already transferring finish"). Whichever row triggers
// the stop, everything from there to the end of queue is reported back as
// undispatched -- Run() decides what to do with it.
func (u *Uploader) dispatchPass(ctx context.Context, queue []statedb.Upload, tally *runTally) passResult {
	var res passResult

	maxConcurrency := u.cfg.Concurrency
	if maxConcurrency < 1 {
		maxConcurrency = 1
	}
	sem := make(chan struct{}, maxConcurrency)
	// Buffered to maxConcurrency, not unbuffered. Each worker goroutine
	// sends its result BEFORE its `defer func() { <-sem }()` runs (defers
	// fire only once the send statement has returned), so on an unbuffered
	// channel a finished worker sat blocked on the send while still holding
	// its semaphore slot. The drain loop is unavailable for the whole of
	// each flush() -- a network round-trip for batchCreate (and, when
	// album support still existed, two more) -- so every worker that finished
	// during a flush froze the pipeline with it, collapsing parallelism to
	// near zero on every batch commit. With room for one result per
	// possible in-flight worker, the hand-off always completes immediately
	// and the slot is freed for the next file right away.
	results := make(chan dispatchResult, maxConcurrency)
	var wg sync.WaitGroup

	// tripped is the circuit breaker itself: set by the FIRST worker to see
	// a throttle-classified response, read by the dispatcher goroutine.
	// Atomic because those are different goroutines, and CompareAndSwap so
	// exactly one throttle per pass shrinks the concurrency ceiling no
	// matter how many land at once.
	var tripped atomic.Bool

	// undispatched is written only by the dispatcher goroutine, and read by
	// the caller only after the drain loop below has observed close(results)
	// -- which the dispatcher does after its final write. That channel close
	// is the happens-before edge, so no mutex is needed here.
	var undispatched []statedb.Upload

	go func() {
		defer close(results)
		for i, row := range queue {
			if tripped.Load() || u.quotaHitThisRun.Load() || ctx.Err() != nil {
				undispatched = append(undispatched, queue[i:]...)
				break
			}
			// Proactive check against our own persistent daily-request
			// counter, ahead of ever sending a request -- distinct from the
			// reactive handleError path below, which only fires after
			// Google's API actually returns a 429. This one has no HTTP
			// response to react to, so without an explicit announcement
			// here it stops the run in complete silence: no error, no
			// throttle line, nothing -- exactly indistinguishable from a
			// hang unless it's called out directly.
			if u.dailyQuota.Exhausted() {
				u.quotaHitThisRun.Store(true)
				if u.onThrottle != nil {
					used, _ := u.dailyQuota.Used()
					u.onThrottle(fmt.Sprintf("daily quota nearly exhausted (%d/%d used today) — stopping further uploads this run to stay under the ceiling", used, u.dailyQuota.DailyLimit()), 0, 0)
				}
				undispatched = append(undispatched, queue[i:]...)
				break
			}
			// Wait for a free slot -- but re-check the breaker each time
			// round, so a trip during a long transfer stops dispatch
			// promptly instead of only after the next slot frees up.
			// Deliberately time.Sleep, not sleepFn: this is a poll interval,
			// not a backoff, and must stay real even when a test stubs out
			// the backoff clock.
			for len(sem) >= u.concurrency.CurrentLimit() && !tripped.Load() && !u.quotaHitThisRun.Load() && ctx.Err() == nil {
				time.Sleep(50 * time.Millisecond)
			}
			if tripped.Load() || u.quotaHitThisRun.Load() || ctx.Err() != nil {
				undispatched = append(undispatched, queue[i:]...)
				break
			}
			sem <- struct{}{}
			wg.Add(1)
			go func(row statedb.Upload) {
				defer wg.Done()
				defer func() { <-sem }()
				token, cls := u.uploadOneBytes(row)
				if cls != nil && cls.Kind == retryx.Throttle {
					// Trip the breaker here, in the worker, rather than in
					// the drain loop: the drain loop can be busy inside
					// flush() for several network round-trips, and every
					// request started in that window is another one piling
					// onto a rate limit that is already complaining.
					if tripped.CompareAndSwap(false, true) {
						u.concurrency.OnThrottled()
					}
				}
				results <- dispatchResult{row: row, token: token, err: cls}
			}(row)
		}
		wg.Wait()
	}()

	var pendingBatch []uploadSuccess
	lastFlush := time.Now()

	flush := func() {
		if len(pendingBatch) == 0 {
			return
		}
		batch := pendingBatch
		pendingBatch = nil
		lastFlush = time.Now()

		bo := u.batchCreateAndLink(batch, &tally.stats)
		// Only files that actually resolved count as processed -- ones held
		// for a retry have not finished, and counting them here would count
		// them twice when the retry lands.
		tally.processed += len(bo.outcomes)
		u.dbErr("updating run progress", u.db.RunUpdate(nil, &tally.processed))
		if u.onProgress != nil {
			for _, o := range bo.outcomes {
				u.onProgress(ProgressEvent{Done: tally.processed, Total: tally.total, LastFile: o.path, LastOK: o.ok, LastSize: o.size, LastErrorMessage: o.errorMessage, LastErrorKind: o.errorKind})
			}
		}

		if bo.throttled == nil {
			return
		}
		// The write quota is the one that actually gets exhausted in
		// practice, so this is the path that matters most: trip the same
		// run-wide breaker the byte-upload path uses.
		if tripped.CompareAndSwap(false, true) {
			u.concurrency.OnThrottled()
		}
		res.throttled = true
		if res.throttleMessage == "" {
			res.throttleMessage = bo.throttled.Message
		}
		res.remaining = append(res.remaining, bo.deferred...)
		res.throttledRows = append(res.throttledRows, bo.deferred...)
		if u.onProgress != nil {
			for _, row := range bo.deferred {
				u.onProgress(ProgressEvent{
					Done: tally.processed, Total: tally.total,
					LastFile: row.FirstSourcePath, LastOK: false, LastSize: row.Size,
					LastDeferred:     true,
					LastErrorMessage: bo.throttled.Message,
				})
			}
		}
	}

	ticker := time.NewTicker(batchFlushInterval)
	defer ticker.Stop()

drainLoop:
	for {
		select {
		case r, ok := <-results:
			if !ok {
				break drainLoop
			}
			ext := extensions.ExtOf(r.row.FirstSourcePath)
			switch {
			case r.token != "":
				pendingBatch = append(pendingBatch, uploadSuccess{row: r.row, token: r.token})
				u.dbErr("recording extension stats for ."+ext, u.db.ExtRecord(ext, 1, 1, 0, ""))
				u.concurrency.OnSuccess()
				if len(pendingBatch) >= batchFlushCount {
					flush()
				}
			case r.err != nil && r.err.Kind == retryx.Throttle:
				// Held, NOT failed: this file gets no per-file retry (that
				// is what kept the aggregate request rate high enough to
				// sustain the throttle), and no ledger row is written yet.
				// It goes back in the queue for after the breaker's pause,
				// and is only ever marked failed_retryable if the whole
				// schedule runs out.
				res.throttled = true
				if res.throttleMessage == "" {
					res.throttleMessage = r.err.Message
				}
				res.remaining = append(res.remaining, r.row)
				res.throttledRows = append(res.throttledRows, r.row)
				if u.onProgress != nil {
					u.onProgress(ProgressEvent{
						Done: tally.processed, Total: tally.total,
						LastFile: r.row.FirstSourcePath, LastOK: false, LastSize: r.row.Size,
						LastDeferred:     true,
						LastErrorMessage: r.err.Message,
					})
				}
			default:
				reason, kind := u.handleError(r.row, r.err, ext, &tally.stats)
				tally.processed++
				u.dbErr("updating run progress", u.db.RunUpdate(nil, &tally.processed))
				if u.onProgress != nil {
					u.onProgress(ProgressEvent{
						Done: tally.processed, Total: tally.total, LastFile: r.row.FirstSourcePath, LastOK: false, LastSize: r.row.Size, LastErrorMessage: reason,
						LastCancelled: r.err != nil && r.err.Kind == retryx.Cancelled,
						LastErrorKind: string(kind),
					})
				}
			}
		case <-ticker.C:
			if time.Since(lastFlush) >= batchFlushInterval {
				flush()
			}
		}
	}
	flush() // whatever's left when the channel closes

	// Deferred files come first and undispatched ones after, which is the
	// original queue order: the dispatcher only ever stops at a suffix.
	res.remaining = append(res.remaining, undispatched...)
	res.undispatchedRows = undispatched
	return res
}

// headerSafeFileName renders a filename for an HTTP header value.
//
// Header values are byte strings, not UTF-8: a photo library full of
// accented or non-Latin names would put raw high bytes on the wire, and a
// name containing CR/LF would be a header-injection vector (Go would reject
// the request outright, failing an upload over a filename). Anything
// outside printable ASCII is percent-encoded, which is what Google's own
// clients do; the exact original name still reaches Google intact via
// batchCreate's JSON fileName field, which overrides this anyway.
func headerSafeFileName(sourcePath string) string {
	name := uploadFileName(sourcePath)
	var b strings.Builder
	for _, c := range []byte(name) {
		if c > 0x20 && c < 0x7f && c != '%' {
			b.WriteByte(c)
			continue
		}
		if c == ' ' {
			b.WriteByte(' ')
			continue
		}
		fmt.Fprintf(&b, "%%%02X", c)
	}
	return b.String()
}
