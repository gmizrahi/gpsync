// Package uploader implements the upload pipeline: concurrency-controlled
// byte uploads, serialized batchCreate, quota + throttle aware retry, and
// uploads.
//
// Dispatch is continuous rather than wave-based: a semaphore bounds how
// many byte uploads run at once and every freed slot is handed the next
// pending row immediately, so one slow multi-GB video never holds up its
// smaller siblings. Successful uploads accumulate into small batchCreate
// calls (well under the 50-item cap) so progress is committed frequently
// and a killed run resumes cleanly without re-uploading anything already
// marked `uploaded`.
//
// The two failure modes are handled at deliberately different scopes:
//
//   - Transient errors (network blips, 5xx) are PER FILE: a small bounded
//     retry with backoff, everything else carrying on around it.
//   - Throttles are RUN-WIDE, via a circuit breaker (see
//     throttleCircuitBreakerSchedule). The first throttle stops the whole
//     pipeline and pauses it on a fixed escalating ladder. Retrying
//     throttled files individually -- the previous design -- left the
//     aggregate request rate essentially unchanged, so the rate limit never
//     cleared; gpsync was keeping its own throttle alive.
package uploader

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/gmizrahi/gpsync/internal/auth"
	"github.com/gmizrahi/gpsync/internal/config"
	"github.com/gmizrahi/gpsync/internal/quota"
	"github.com/gmizrahi/gpsync/internal/retryx"
	"github.com/gmizrahi/gpsync/internal/statedb"
)

// Overridable (not const) so tests can point them at an httptest.Server
// instead of the real Google Photos API.
var (
	uploadURL      = "https://photoslibrary.googleapis.com/v1/uploads"
	batchCreateURL = "https://photoslibrary.googleapis.com/v1/mediaItems:batchCreate"
)

type Uploader struct {
	// ctx bounds this Run() call's lifetime -- checked during the
	// circuit-breaker's backoff wait so a long-running caller (gpsync-tray) can
	// cut a multi-minute pause short on Pause/Quit instead of blocking until
	// the rung naturally elapses (up to the schedule's final 1h rung). Left
	// nil by tests that construct &Uploader{} directly; Run() falls back to
	// context.Background() (never cancels) in that case, same as a nil
	// breaker/concurrency falls back to a fresh one.
	ctx         context.Context
	client      *http.Client
	db          *statedb.DB
	cfg         config.Config
	dailyQuota  *quota.DailyQuota
	concurrency *quota.AdaptiveConcurrency
	breaker     *CircuitBreakerState
	// skip is optional (nil unless the caller passes one via New) -- a
	// manual "bench whatever's held right now" override, see SkipSignal.
	skip *SkipSignal
	// fileCancels is optional (nil unless the caller passes one via New)
	// -- lets a caller interrupt one specific file's ACTIVE transfer, see
	// FileCancelRegistry.
	fileCancels  *FileCancelRegistry
	scopeFolders []string
	onSkip       func(path, reason string)
	onBytes      func(ByteProgressEvent)
	onThrottle   func(message string, backoffSeconds float64, newConcurrency int)
	onBackoff    func(BackoffStatus)
	onProgress   func(ProgressEvent)
	onDBError    func(context string, err error)
	// quotaHitThisRun is read from the dispatcher goroutine and written from
	// both the dispatcher goroutine (proactive Exhausted() check) and the
	// drain-loop goroutine (reactive handleError DailyQuota case) -- atomic
	// so those two goroutines touching it isn't a data race.
	quotaHitThisRun atomic.Bool
}

// New builds an Uploader for a single run, obtaining an authenticated
// HTTP client and wiring up the shared limiter, circuit breaker and
// progress callbacks described below.
//
// scopeFolders, if non-empty, limits the run to pending files under those
// folder path prefixes (e.g. just what a `gpsync scan <folder>` just found)
// instead of the entire library's pending backlog. onSkip, if non-nil, is
// called synchronously and immediately for every file gated out by a
// known-unsupported extension, before any upload attempt -- so the caller
// can show it live during the run, not just via `gpsync extensions` afterward.
// onBytes, if non-nil, is called repeatedly during each file's byte
// transfer with incremental progress (see ByteProgressEvent) -- callers not
// wanting per-chunk granularity should throttle on their own side. onThrottle,
// if non-nil, is called every time Google's API returns a 429/throttle
// response, with the backoff delay and the concurrency limit it was just
// reduced to -- so retry/backoff isn't invisible to the caller. onBackoff,
// if non-nil, is called about once a second for the whole of every
// circuit-breaker pause with the live countdown (see BackoffStatus), then
// once with Active:false when the pause ends -- onThrottle records that a
// pause started, onBackoff is what makes the pause itself visible while it
// is happening instead of looking like a hang. onDBError,
// if non-nil, is called whenever a ledger write on the upload hot path
// fails (see dbErr) -- those errors used to be discarded outright, which
// meant a file whose bytes Google already had could silently stay `pending`
// and be re-uploaded on the next run, burning quota and bandwidth while
// hiding the real problem (a full disk, a corrupt DB) from the user
// entirely.
//
// concurrency is the adaptive concurrency limiter. Callers processing
// several folders in ONE invocation must build it once and pass the same
// one to every folder's Uploader: it carries what the run has learned about
// the safe request rate, in both directions (how far it had to back off
// after a throttle, and how far it has climbed since). Constructing a fresh
// one per folder throws that away at every folder boundary and restarts the
// cold-start ramp from 1 -- which, for the many-small-folders shape this
// tool exists for (year/month folders of ~20-50 files), means most of every
// folder runs at low concurrency and configuredMax is rarely reached.
// Passing nil builds a fresh limiter, which is right for a single-folder
// invocation and keeps direct construction simple in tests.
//
// breaker is the circuit breaker's rung state, shared across folders for
// the identical reason concurrency is: a fresh breaker per folder throws
// away how far it had to back off, so a throttle that's still ongoing when
// the next folder starts gets treated as brand new (rung 0, 5s) instead of
// continuing anywhere near where the last folder left off. Passing nil
// builds a fresh, unshared state -- see CircuitBreakerState's own doc
// comment.
func New(ctx context.Context, db *statedb.DB, cfg config.Config, scopeFolders []string, concurrency *quota.AdaptiveConcurrency, breaker *CircuitBreakerState, skip *SkipSignal, fileCancels *FileCancelRegistry, onSkip func(path, reason string), onBytes func(ByteProgressEvent), onThrottle func(message string, backoffSeconds float64, newConcurrency int), onBackoff func(BackoffStatus), onProgress func(ProgressEvent), onDBError func(context string, err error)) (*Uploader, error) {
	client, err := auth.GetHTTPClient(ctx)
	if err != nil {
		return nil, err
	}
	if concurrency == nil {
		concurrency = quota.NewAdaptiveConcurrency(cfg.Concurrency)
	}
	if breaker == nil {
		breaker = NewCircuitBreakerState()
	}
	return &Uploader{
		ctx:          ctx,
		client:       client,
		db:           db,
		cfg:          cfg,
		dailyQuota:   quota.NewDailyQuota(db),
		breaker:      breaker,
		skip:         skip,
		fileCancels:  fileCancels,
		concurrency:  concurrency,
		scopeFolders: scopeFolders,
		onSkip:       onSkip,
		onBytes:      onBytes,
		onThrottle:   onThrottle,
		onBackoff:    onBackoff,
		onProgress:   onProgress,
		onDBError:    onDBError,
	}, nil
}

// dbErr reports a failed ledger write on the upload hot path. These were
// all `_ = u.db.X(...)` before: a MarkUploaded that failed AFTER Google
// already had the file's bytes left the row `pending`, so the next run
// silently re-uploaded it -- wasted quota and bandwidth, and the actual
// cause (disk full, DB corruption) never reached the user at all. Never
// panics and never blocks: a broken callback must not take down a run
// that's otherwise still making progress.
func (u *Uploader) dbErr(context string, err error) {
	if err == nil || u.onDBError == nil {
		return
	}
	u.onDBError(context, err)
}

// Request marks a skip pending and queues a wakeup for C(). Idempotent --
// calling it again before the previous one is taken/drained has no
// additional effect (the non-blocking send below is a no-op if the
// buffer already holds one).
func (s *SkipSignal) Request() {
	s.requested.Store(true)
	select {
	case s.ch <- struct{}{}:
	default:
	}
}

// Take reports whether a skip was requested, and clears it -- one-shot,
// so a single button click only benches whatever's held once, not every
// pass from then on. Also drains C()'s buffer if Request()'s send is
// still sitting there unclaimed (e.g. the wait it was meant to interrupt
// had already finished naturally moments before the click arrived) --
// without this, that leftover buffered value would immediately fire the
// NEXT wait's watcher too, applying one click's effect twice.
func (s *SkipSignal) Take() bool {
	if !s.requested.CompareAndSwap(true, false) {
		return false
	}
	select {
	case <-s.ch:
	default:
	}
	return true
}

// C returns the channel a blocked circuit-breaker wait selects on to
// react to a skip immediately instead of only at the next natural pass
// boundary -- safe to call fresh every time (it's always the same
// channel; see the type's own doc comment for why that matters).
func (s *SkipSignal) C() <-chan struct{} {
	return s.ch
}

func (r *FileCancelRegistry) register(path string, cancel context.CancelFunc) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cancels[path] = cancel
}

func (r *FileCancelRegistry) unregister(path string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.cancels, path)
}

// Cancel interrupts path's active upload, if one is in flight right now.
// Reports whether a matching in-flight upload was actually found -- a
// miss (the file already finished, or was never in flight) is not an
// error, just a no-op.
func (r *FileCancelRegistry) Cancel(path string) bool {
	r.mu.Lock()
	cancel, ok := r.cancels[path]
	r.mu.Unlock()
	if ok {
		cancel()
	}
	return ok
}

// escalate advances the rung by one (a throttle just happened and its wait
// just completed) and resets the banked-successes counter -- a fresh climb
// starts owing its own decay from zero, not coasting on credit earned
// before the rung it's now escalating past. Returns the new rung.
func (s *CircuitBreakerState) escalate() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cleanUploads = 0
	s.rung++
	return s.rung
}

// truncateThrottleReason flattens a message to one line and caps its
// length, rune-safe (Google's error text is not guaranteed ASCII).
func truncateThrottleReason(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if len(r) <= throttleReasonDisplayLen {
		return s
	}
	if throttleReasonDisplayLen <= 3 {
		return string(r[:throttleReasonDisplayLen])
	}
	return string(r[:throttleReasonDisplayLen-3]) + "..."
}

// realBackoffWait sits out `total`, ticking onTick roughly every
// backoffTickInterval. Returns true if it ran to completion, false if ctx
// was cancelled first (gpsync-tray's Pause/Quit) -- the caller treats that as
// "stop now," not as a completed rung.
func realBackoffWait(ctx context.Context, total time.Duration, onTick func(remaining time.Duration)) bool {
	remaining := total
	for remaining > 0 {
		onTick(remaining)
		step := backoffTickInterval
		if step > remaining {
			step = remaining
		}
		select {
		case <-time.After(step):
			remaining -= step
		case <-ctx.Done():
			return false
		}
	}
	return true
}

// AbortReason says which of the two "stop everything" conditions ended a run.
type AbortReason string

const (
	// AbortDailyQuota: the project's 10k/day Library API quota is used up.
	// It is a per-project ceiling, so every remaining folder in this
	// invocation would hit the identical wall.
	AbortDailyQuota AbortReason = "daily_quota"
	// AbortThrottleBackoffExhausted: every rung of
	// throttleCircuitBreakerSchedule was waited out and Google is still
	// throttling.
	AbortThrottleBackoffExhausted AbortReason = "throttle_backoff_exhausted"
	// AbortInterrupted: the caller's context was cancelled (gpsync-tray's
	// Pause/Quit), either while sitting out a circuit-breaker pause (Steps
	// is set to the rung reached) or mid-dispatch, before the current
	// pass's queue was fully sent (Steps left at 0 -- "stop dispatch"
	// requests, gracefully, always land between whole files, never
	// mid-transfer, since nothing threads ctx into an individual HTTP
	// request). Not a real throttle verdict either way, so the breaker's
	// rung is left exactly where it was.
	AbortInterrupted AbortReason = "interrupted"
	// AbortNoEligibleFiles: a full lap over every configured upload pass
	// left the pending set completely unchanged -- every remaining file
	// was excluded by the media-type filter (or, for gpsync-tray's
	// PhotosFirst/SmallestFirst passes, by every pass's own filter at
	// once, e.g. a `.webm`/`.mpo`-class extension that extensions.KindOf
	// classifies as neither photo nor video). Constructed by the caller
	// (internal/engine's RunFolderCycle, cmd/gpsync's syncOneFolder), not by
	// Run() itself -- only the caller knows whether ANOTHER pass or a
	// later invocation with different settings could ever pick these rows
	// up, since Run() only ever sees one pass's own filter. Without this,
	// a caller's own "loop until nothing pending" driver spun forever
	// (100% CPU, unresponsive to ctx cancellation) on exactly this
	// condition -- a real, verified bug: Run() returns cleanly with
	// SkippedMediaType>0 and no error, so nothing ever told the loop to
	// stop trying.
	AbortNoEligibleFiles AbortReason = "no_eligible_files"
)

func (e *AbortError) Error() string {
	switch e.Reason {
	case AbortDailyQuota:
		return e.Detail
	case AbortInterrupted:
		return "stopped: " + e.Detail
	case AbortNoEligibleFiles:
		return e.Detail
	default:
		return fmt.Sprintf("still throttled after %s of backoff across %d steps: %s",
			e.Waited.Round(time.Second), e.Steps, e.Detail)
	}
}

func (e *AbortError) Unwrap() error { return ErrRunAborted }

// runTally is the state that accumulates across every dispatch pass of one
// Run: a run can take several passes when the throttle circuit breaker
// trips, and the counters and the "N of M files" the caller sees must span
// all of them, not restart per pass.
type runTally struct {
	stats     Stats
	processed int
	total     int
}

// progressReader wraps a file body reader, reporting each chunk actually
// handed to the network layer -- so ByteProgressEvent reflects real
// transfer progress, not just "read from disk" (which for a small local
// file would race far ahead of the actual upload).
type progressReader struct {
	r      io.Reader
	onRead func(n int64)
}

func (p *progressReader) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	if n > 0 && p.onRead != nil {
		p.onRead(int64(n))
	}
	return n, err
}

// sleepFor is the PER-FILE backoff, for transient errors only (network
// blips, 5xx). Throttles never reach it: those are handled run-wide by the
// circuit breaker in Run(), because backing off one file at a time while
// its siblings keep going never actually reduces the request rate.
func (u *Uploader) sleepFor(cls *retryx.Classification, attempt int) {
	var delay float64
	if cls.HasRetryAfter {
		delay = cls.RetryAfter
	} else {
		delay = retryx.BackoffDelay(attempt, 1.0, 60.0)
	}
	sleepFn(time.Duration(delay * float64(time.Second)))
}

func (u *Uploader) uploadOneBytes(row statedb.Upload) (string, *retryx.Classification) {
	if _, err := os.Stat(row.FirstSourcePath); err != nil {
		return "", &retryx.Classification{Kind: retryx.Permanent, Message: "Source file no longer exists on disk"}
	}

	var lastErr *retryx.Classification
	for attempt := 0; attempt < maxUploadAttempts; attempt++ {
		token, cls, retry := u.uploadBytesAttempt(row)
		if !retry {
			return token, cls
		}
		lastErr = cls
		// Sleep only when there's actually going to BE another attempt.
		// Sleeping on the final, doomed attempt just delayed a failure
		// that was already decided -- up to ~60s of backoff per file, for
		// nothing.
		if attempt+1 < maxUploadAttempts {
			u.sleepFor(lastErr, attempt)
		}
	}
	return "", lastErr
}

// uploadFileName is the name Google Photos should display for a file: its
// original basename. Capped at the 255 characters the batchCreate reference
// documents, counted in RUNES so a non-ASCII name is never cut mid-character
// into invalid UTF-8 (the field is JSON, so it carries any valid UTF-8
// exactly).
func uploadFileName(sourcePath string) string {
	name := filepath.Base(sourcePath)
	if name == "." || name == string(filepath.Separator) {
		return ""
	}
	const maxFileNameRunes = 255
	if r := []rune(name); len(r) > maxFileNameRunes {
		// Keep the extension, which is the part Google actually needs.
		ext := filepath.Ext(name)
		keep := maxFileNameRunes - len([]rune(ext))
		if keep < 1 {
			return string(r[:maxFileNameRunes])
		}
		return string(r[:keep]) + ext
	}
	return name
}

// finalForThisFile reports classifications that must never be retried by
// this file on its own: permanent failures (retrying can't help),
// daily-quota exhaustion (the run is over), throttles (the circuit
// breaker owns the retry, after pausing everything), and a manual
// per-file cancellation (the user asked for exactly this file to stop).
func finalForThisFile(kind retryx.Kind) bool {
	return kind == retryx.Permanent || kind == retryx.DailyQuota || kind == retryx.Throttle || kind == retryx.Cancelled
}

type uploadSuccess struct {
	row   statedb.Upload
	token string
}

type errorEnvelope struct {
	Error retryx.APIError `json:"error"`
}

func classifyHTTPError(statusCode int, body []byte, retryAfter string) retryx.Classification {
	var env errorEnvelope
	var apiErr *retryx.APIError
	if json.Unmarshal(body, &env) == nil && (env.Error.Status != "" || env.Error.Message != "") {
		apiErr = &env.Error
	}
	return retryx.ClassifyResponse(statusCode, apiErr, retryAfter)
}
