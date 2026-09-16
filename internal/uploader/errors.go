package uploader

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/gmizrahi/gpsync/internal/retryx"
	"github.com/gmizrahi/gpsync/internal/statedb"
)

// dailyQuotaTrustThreshold: a DailyQuota classification is only downgraded
// to a plain throttle when our OWN request counter is still below this many
// requests for the day -- i.e. nowhere near the real 10k/day ceiling, so
// genuine exhaustion is implausible and the response is far more likely the
// known-ambiguous throttle wording (see retryx.ClassifyResponse's "per day"
// note). Above it, Google's own claim is trusted.
//
// Half the ceiling, deliberately, and NOT a "prove we're nearly exhausted"
// bar. Two things make a stricter bar wrong:
//
//   - The local counter only records structured Library API calls, and one
//     batchCreate covers ~20 files, so a full day of heavy uploading barely
//     moves it. An "8800+ used" style bar (what this check used to require,
//     back when every file cost ~2 counted units) became unreachable after
//     that counting fix, so EVERY genuine per-day 429 got silently
//     downgraded to a retry -- the reactive daily-quota stop never fired at
//     all, and files just piled up as failed_retryable with no explanation.
//   - The counter can never PROVE exhaustion is impossible anyway: the same
//     OAuth client can be metered by other processes (a second gpsync run,
//     rclone using the same imported client -- the documented reason `gpsync
//     quota` exists), so local usage is a floor, not the total.
//
// The job here is filtering one specific known false positive, not
// demanding near-total proof from a counter that can't supply it.
const dailyQuotaTrustThreshold = 5000

// reclassifyDailyQuota downgrades a DailyQuota classification to Throttle
// (retry with backoff) when our local counter says exhaustion is
// implausible. On a read error it leaves the classification alone -- a
// local DB hiccup is not grounds for overriding Google.
func (u *Uploader) reclassifyDailyQuota(cls retryx.Classification) retryx.Classification {
	if cls.Kind != retryx.DailyQuota {
		return cls
	}
	if used, err := u.dailyQuota.Used(); err == nil && used < dailyQuotaTrustThreshold {
		cls.Kind = retryx.Throttle
	}
	return cls
}

// throttleReasonDisplayLen caps how much of Google's raw error text appears
// in the circuit breaker's scrollback line. These run long ("Quota exceeded
// for quota 'concurrent write request' of service 'photoslibrary.googleapis.com'
// for consumer ..."), and the useful part of that line -- which rung, how
// long the pause is, how many files are queued behind it -- belongs at a
// glance, not buried after a wall of text. Matches cmd/gpsync's own
// maxReasonLen for the same reasoning, kept as a separate local constant
// rather than importing cmd/gpsync (which would be a layering inversion --
// display-length choices belong with whoever prints them, this package just
// keeps its own copy of the number).
const throttleReasonDisplayLen = 54

// ErrRunAborted marks a run that stopped early on purpose. It is expected,
// handled behavior -- not a crash -- and callers processing several folders
// in one invocation MUST check for it (errors.Is) and stop the whole loop
// rather than moving on to the next folder, which by construction would run
// straight into the same wall. Everything unfinished is left pending or
// failed_retryable in the ledger, so the next invocation resumes naturally.
var ErrRunAborted = errors.New("upload run stopped early; remaining folders should be skipped")

// AbortError carries why a run stopped, for a caller that wants to explain
// it rather than just print an error. It unwraps to ErrRunAborted.
type AbortError struct {
	Reason    AbortReason
	Detail    string        // the underlying API message, or the quota figures
	Waited    time.Duration // total circuit-breaker backoff spent (throttle case)
	Steps     int           // rungs of the schedule exhausted (throttle case)
	Remaining int           // files left unfinished when the run stopped
}

// Summary is the explanation stored in the ledger (run_progress.end_detail)
// and shown by `gpsync info` for a run that stopped early -- phrased for
// someone reading it later, who needs to know both what stopped the run and
// what it means for the files that didn't make it.
func (e *AbortError) Summary() string {
	left := fmt.Sprintf("%d file(s) left pending (will retry next run)", e.Remaining)
	switch e.Reason {
	case AbortDailyQuota:
		return fmt.Sprintf("%s — %s", e.Detail, left)
	case AbortInterrupted:
		if e.Steps > 0 {
			// Interrupted while sitting out a circuit-breaker pause -- Steps
			// is only ever set (to rung+1) on that path, never on the
			// mid-dispatch one below, since dispatch has no "rung."
			return fmt.Sprintf("stopped mid-backoff (rung %d of %d) — %s", e.Steps, len(throttleCircuitBreakerSchedule), left)
		}
		return fmt.Sprintf("stopped before finishing this folder's queue — %s", left)
	case AbortNoEligibleFiles:
		return fmt.Sprintf("%s — %s", e.Detail, left)
	default:
		return fmt.Sprintf("gave up after %s of escalating backoff across %d steps — %s",
			e.Waited.Round(time.Second), e.Steps, left)
	}
}

// LedgerReason maps an abort to the ledger's end_reason vocabulary.
func (e *AbortError) LedgerReason() string {
	switch e.Reason {
	case AbortDailyQuota:
		return statedb.EndReasonAbortedDailyQuota
	case AbortInterrupted:
		return statedb.EndReasonInterrupted
	case AbortNoEligibleFiles:
		return statedb.EndReasonAbortedNoEligibleFiles
	default:
		return statedb.EndReasonAbortedThrottle
	}
}

// handleError records one file's failure in the ledger and returns the
// human-readable reason, so the caller can pass it straight to onProgress
// (see ProgressEvent.LastErrorMessage) instead of leaving a live view
// showing a bare "failed" with the explanation only in the DB.
//
// Throttle-classified failures never reach here -- dispatchPass sets those
// aside for the circuit breaker instead of recording them as failures.
func (u *Uploader) handleError(row statedb.Upload, cls *retryx.Classification, ext string, stats *Stats) (string, retryx.Kind) {
	if cls == nil {
		cls = &retryx.Classification{Kind: retryx.Transient, Message: "unknown error"}
	}
	markCtx := "recording failure for " + row.FirstSourcePath
	switch cls.Kind {
	case retryx.Permanent:
		u.dbErr(markCtx, u.db.MarkFailed(row.SHA256, true, "PERMANENT_ERROR", cls.Message))
		u.dbErr("recording extension stats for ."+ext, u.db.ExtRecord(ext, 1, 0, 1, cls.Message))
		stats.FailedPermanent++
	case retryx.DailyQuota:
		u.dbErr(markCtx, u.db.MarkFailed(row.SHA256, false, "DAILY_QUOTA_EXCEEDED", cls.Message))
		u.quotaHitThisRun.Store(true)
		stats.FailedRetryable++
		if u.onThrottle != nil {
			used, _ := u.dailyQuota.Used()
			u.onThrottle(fmt.Sprintf("daily quota exceeded (confirmed by local count: %d used today) — %s", used, cls.Message), 0, 0)
		}
	default:
		u.dbErr(markCtx, u.db.MarkFailed(row.SHA256, false, strings.ToUpper(string(cls.Kind)), cls.Message))
		stats.FailedRetryable++
	}
	return cls.Message, cls.Kind
}

// ── phase 1: parallel raw-byte upload ──────────────────────────────────

// maxUploadAttempts bounds in-run retries for one file's byte upload. It
// applies to transient errors only -- a network blip or a 5xx, where an
// immediate second try genuinely often works. A file still failing after
// this lands as failed_retryable and is picked up by the next run's
// RequeueRetryable.
//
// Throttles are deliberately excluded and get ZERO per-file retries (see
// uploadBytesAttempt): they are owned by the run-wide circuit breaker in
// Run() instead. Retrying a throttled file in place is worse than useless
// -- the rate limit is about the whole pipeline, so the only thing that
// clears it is stopping the whole pipeline.
const maxUploadAttempts = 2
