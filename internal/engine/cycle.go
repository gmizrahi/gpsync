package engine

import (
	"context"
	"fmt"
	"time"

	"github.com/gmizrahi/gpsync/internal/config"
	"github.com/gmizrahi/gpsync/internal/extensions"
	"github.com/gmizrahi/gpsync/internal/quota"
	"github.com/gmizrahi/gpsync/internal/scanner"
	"github.com/gmizrahi/gpsync/internal/statedb"
	"github.com/gmizrahi/gpsync/internal/uploader"
)

// cycleWakePollFallback bounds how long the upload loop below can sit idle
// waiting for the scan goroutine to find something (or finish) before
// re-checking on its own -- a safety net in case a wake ping is ever missed
// for some reason, not the primary signal (the wake channel is).
const cycleWakePollFallback = 3 * time.Second

// quietScanConcurrency mirrors cmd/gpsync's own defaultScanConcurrency:
// deliberately conservative, not runtime.NumCPU() -- hashing is disk-bound,
// and concurrent reads thrash on a spinning HDD (common for a bulk photo
// archive). Not exported/configurable here since gpsync-tray has no CLI flags
// to source an override from; it can grow one later if that turns out to
// matter.
const quietScanConcurrency = 2

// FolderCycleResult reports what one quiet folder cycle actually did.
type FolderCycleResult struct {
	Scan   scanner.ScanSummary
	Upload uploader.Stats
}

// RunFolderCycle scans and uploads one folder CONCURRENTLY -- a real
// report: "why do you wait before your scan gets to the folder; start the
// upload right away, the scan and queueing of new discovered files should
// be done in a parallel thread." Previously scan ran to completion before
// any upload started; now the scan runs in its own goroutine while the
// upload loop below dispatches whatever's already pending (including
// files the scan is still in the middle of discovering) without waiting
// for it to finish.
//
// This is the quiet sibling of cmd/gpsync's syncOneFolder, for callers with
// no terminal to print to (gpsync-tray). Visibility comes from the
// run_progress/ledger tables the underlying scanner/uploader calls already
// write regardless of callbacks, polled by gpsync-tray's dashboard HTTP
// server instead of printed here, plus three optional callbacks -- passed
// straight through to uploader.New -- for the states that write nothing to
// the ledger while they're active: onBackoff (a circuit-breaker pause),
// onBytes (a file's live upload progress), and onProgress (a file just
// finished, so a caller can log it to a recent-activity view). All three
// may be nil.
//
// run_progress is a singleton row (id=1, overwritten in place by every
// writer) -- with scan and upload now running at the same time, only ONE
// of them can safely own it for this cycle, or it would flicker/race
// between the two. The scan below passes trackRun=false to
// scanner.ScanFolders so the UPLOAD phase deterministically owns the row
// (not a race that "usually" resolves in upload's favor -- scan simply
// never touches it). This also matches direct user priority when asked how
// this should be displayed: "i honestly don't care about the scanning...
// i want to see uploads."
//
// The scan goroutine is NOT cancelled if the upload phase aborts early or
// ctx is cancelled -- scanner.ScanFolders has no cancellation hook of its
// own (a possible future enhancement), so it simply runs to natural
// completion in the background. That's bounded and harmless -- it only
// ever writes EnsurePending rows, exactly like any other scan -- just not
// instantly stopped.
//
// ctx bounds the upload phase: cancelling it interrupts a circuit-breaker
// pause immediately instead of blocking until the rung naturally elapses
// (up to an hour on the schedule's final rung) -- see
// uploader.Uploader.ctx's doc comment. This is what makes gpsync-tray's
// Pause/Quit actually responsive instead of hanging for the length of
// whatever backoff happens to be in progress.
//
// The returned error is uploader.ErrRunAborted-wrapping when the circuit
// breaker gave up, the daily quota was hit, or ctx was cancelled --
// expected, not a failure. Callers must not treat that as fatal; it's
// exactly what their own heartbeat mechanism (see RunWatchLoop's
// onHeartbeat) or the next Resume exists to retry later, mirroring
// cmd/gpsync's runWatchCycle.
//
// The upload phase can be more than one uploader.Run() call per lap of the
// loop below -- see uploadPasses -- depending on cfg.SyncStrategy. An
// abort on any pass stops the whole cycle immediately.
//
// skip is optional (nil means no manual override available) -- passed
// straight through to every pass's uploader.New, see uploader.SkipSignal.
//
// fileCancels is also optional (nil means no per-file cancellation
// available) -- passed straight through to every pass's uploader.New, see
// uploader.FileCancelRegistry.
//
// onScanActive and onTotals (both optional) exist purely so gpsync-tray's
// dashboard can show live "Scanning: <folder>" / "Uploading N/M files"
// state without polling run_progress (which this function deliberately
// stops writing to during a concurrent cycle -- see above). onScanActive
// fires true right as the scan goroutine starts and false once it
// finishes. onTotals fires with the CURRENT pending count/bytes every time
// the loop below re-checks (i.e. live, not a one-time snapshot -- the
// concurrent scan can keep discovering more throughout the cycle).
func RunFolderCycle(ctx context.Context, db *statedb.DB, cfg config.Config, folder string, conc *quota.AdaptiveConcurrency, breaker *uploader.CircuitBreakerState, skip *uploader.SkipSignal, fileCancels *uploader.FileCancelRegistry, onBackoff func(uploader.BackoffStatus), onBytes func(uploader.ByteProgressEvent), onProgress func(uploader.ProgressEvent), onScanActive func(active bool), onTotals func(pendingFiles int, pendingBytes int64)) (FolderCycleResult, error) {
	var result FolderCycleResult
	scope := []string{folder}
	uploadScope := UploadScopeFor(cfg, folder)

	// Sweep failed_retryable rows back to pending BEFORE the loop below
	// ever decides whether there's real work. The bug this fixes:
	// RequeueRetryable (unscoped -- every failed_retryable row in
	// the whole DB, not just this scope) used to run only from inside
	// Uploader.Run() itself, but the loop below never CALLS Run() unless
	// db.ListPendingUnder (status='pending' only) already finds
	// something. A file benched by a throttle-schedule abort (or any
	// other failed_retryable outcome) sits in that status, NOT 'pending',
	// until something requeues it -- so once a folder's backlog was
	// ENTIRELY failed_retryable (nothing genuinely 'pending', nothing new
	// left to scan), nothing would ever call Run() again for it, so
	// RequeueRetryable would never run, so it would never become visible
	// as pending either -- permanently stuck. After a mass throttle abort
	// that showed up as `gpsync sync` reporting folders "already synced"
	// that actually had dozens of failed_retryable files sitting
	// untouched, and gpsync-tray's dashboard going completely idle for over
	// an hour with a stale run_progress timestamp. Cheap and safe to call
	// on every cycle regardless of whether anything's actually
	// failed_retryable right now (a single no-op UPDATE).
	if _, err := db.RequeueRetryable(); err != nil {
		return result, err
	}

	// wake is pinged (non-blocking, capacity 1 -- same idiom as
	// uploader.SkipSignal) every time the scan below confirms a file, so
	// the upload loop reacts promptly instead of only on
	// cycleWakePollFallback's timer.
	wake := make(chan struct{}, 1)
	poke := func() {
		select {
		case wake <- struct{}{}:
		default:
		}
	}

	scanDone := make(chan struct{})
	var summary scanner.ScanSummary
	var scanErr error
	if onScanActive != nil {
		onScanActive(true)
	}
	go func() {
		defer close(scanDone)
		defer func() {
			if onScanActive != nil {
				onScanActive(false)
			}
		}()
		// "watch_scan", not cmd/gpsync's "manual_scan" -- RunFolderCycle is
		// only ever called by gpsync-tray's automatic watch loop (baseline/
		// live/backlog/heartbeat), never by a user-typed command, and
		// run_type shows up verbatim as "Active run" on the dashboard, so
		// an automatic scan must not be labelled "manual" there.
		summary, scanErr = scanner.ScanFolders(db, scope, "watch_scan", quietScanConcurrency, false, false,
			nil, nil, func(scanner.ProgressEvent) { poke() })
	}()
	scanIsDone := func() bool {
		select {
		case <-scanDone:
			return true
		default:
			return false
		}
	}

	for {
		pending, perr := db.ListPendingUnder(uploadScope)
		if perr != nil {
			return result, perr
		}
		if len(pending) == 0 {
			if scanIsDone() {
				break
			}
			// Nothing to do yet -- wait for the scan to find something
			// (wake), finish entirely (scanDone), or the fallback timer,
			// rather than spinning. Deliberately not spawning
			// uploader.New until there's real work: this is the
			// overwhelming common case once a library is caught up, and
			// avoids paying for an OAuth client / HTTP setup for nothing,
			// matching cmd/gpsync's syncOneFolder "quiet" path.
			select {
			case <-wake:
			case <-scanDone:
			case <-time.After(cycleWakePollFallback):
			}
			continue
		}

		if onTotals != nil {
			var pendingBytes int64
			for _, row := range pending {
				pendingBytes += row.Size
			}
			onTotals(len(pending), pendingBytes)
		}

		// lapProgress/lapSkippedMediaType detect a lap that changed NOTHING
		// in the pending set despite pending being non-empty (checked
		// above). If every pending row in scope is
		// excluded by every pass's own MediaTypeFilter (a media-type
		// setting that excludes the only kind present, or -- for
		// PhotosFirst's photo/video split -- an extension like `.webm`
		// that extensions.KindOf classifies as neither), Run() returns
		// cleanly (SkippedMediaType>0, no error) having touched nothing,
		// so `pending` above is IDENTICAL on the next lap and this loop
		// spun forever: 100% CPU, and unresponsive to ctx cancellation
		// (this loop's own ctx check only runs in the "wait for the wake
		// channel" branch above, never reached while pending stays
		// non-empty).
		var lapProgress, lapSkippedMediaType int
		for _, passCfg := range uploadPasses(cfg) {
			u, err := uploader.New(ctx, db, passCfg, uploadScope, conc, breaker, skip, fileCancels, nil, onBytes, nil, onBackoff, onProgress, nil)
			if err != nil {
				return result, err
			}
			stats, runErr := u.Run()
			result.Upload.Uploaded += stats.Uploaded
			result.Upload.FailedPermanent += stats.FailedPermanent
			result.Upload.FailedRetryable += stats.FailedRetryable
			result.Upload.SkippedQuota += stats.SkippedQuota
			result.Upload.SkippedMediaType += stats.SkippedMediaType
			lapProgress += stats.Uploaded + stats.FailedPermanent + stats.FailedRetryable
			lapSkippedMediaType += stats.SkippedMediaType
			if runErr != nil {
				// A circuit-breaker abort, daily-quota exhaustion, or ctx
				// cancellation stops the WHOLE cycle here -- never start
				// a second pass into a pipeline that just said "stop."
				// Whatever this pass didn't finish is safely queued for
				// the next cycle either way (see Uploader.Run's own
				// handling of each case). The scan goroutine is left
				// running in the background -- see this function's own
				// doc comment on why that's fine.
				return result, runErr
			}
		}
		if lapProgress == 0 {
			abort := &uploader.AbortError{
				Reason:    uploader.AbortNoEligibleFiles,
				Detail:    fmt.Sprintf("%d file(s) excluded by the media-type filter/sync strategy under current settings", lapSkippedMediaType),
				Remaining: len(pending),
			}
			// Best-effort: RunFolderCycle has no onDBError callback of its
			// own (unlike Uploader, which swallows this identically when
			// its own onDBError is nil) -- a failed write here would only
			// cost the ledger's own record of WHY this cycle stopped, not
			// the abort itself, which is returned regardless.
			_ = db.RunEnd(abort.LedgerReason(), abort.Summary())
			return result, abort
		}
	}

	// scanDone is guaranteed closed by this point (the loop above only
	// exits once scanIsDone() is true), so summary/scanErr -- written by
	// the goroutine strictly before it closes scanDone -- are safe to
	// read here without their own synchronization.
	result.Scan = summary
	return result, scanErr
}

// UploadScopeFor decides which folders the UPLOAD phase considers: for a
// global sync strategy (Smallest/PhotosFirst), cfg.SourceFolders -- the
// handful of TOP-LEVEL roots actually configured (what `--smallest-first
// -global` scopes itself to on the CLI: `db.ListPendingUnder` builds one
// `first_source_path LIKE 'root%'` clause per entry, and a single
// top-level prefix already matches everything nested arbitrarily deep
// underneath it, via SQL LIKE's own `%` wildcard -- no need to enumerate
// every leaf subfolder separately). Otherwise just folder alone.
//
// This used to take the caller's fully-EXPANDED per-leaf folder list
// instead (every individual sync unit DiscoverSyncUnits finds -- often
// hundreds for a library organized into many subfolders). That was a
// serious bug, not just an inefficiency: ListPendingUnder builds one
// OR'd LIKE clause PER FOLDER PASSED IN, so a query with hundreds of
// clauses against tens of thousands of pending rows could run for
// MINUTES, and since it happens before Run() ever calls RunStart, the
// dashboard showed nothing at all while it churned, for minutes at a
// time, with the queue apparently untouched. cfg.SourceFolders is both
// correct (same matched-file set, since
// a leaf's prefix is redundant with its ancestor root's) and cheap (as
// few as 1-5 clauses instead of hundreds).
func UploadScopeFor(cfg config.Config, folder string) []string {
	if len(cfg.SourceFolders) > 0 && (cfg.SyncStrategy == config.SyncStrategySmallestFirst || cfg.SyncStrategy == config.SyncStrategyPhotosFirst) {
		return cfg.SourceFolders
	}
	return []string{folder}
}

// uploadPasses returns the sequence of upload-phase configs RunFolderCycle
// runs for one cycle, according to cfg.SyncStrategy. Each pass's config is
// applied against uploadScope (UploadScopeFor's widened folder list for
// Smallest/PhotosFirst, see there) via uploader.New, not just the
// triggering folder -- that's what makes these actually global, matching
// `gpsync upload --smallest-first-global`'s ordering rather than
// `--smallest-first`'s per-folder scope.
//
//   - SyncStrategyFolderByFolder (default): one pass, cfg unchanged.
//   - SyncStrategySmallestFirst: one pass, with SortSmallestFirst forced on
//     for it -- same sort mechanism `gpsync upload --smallest-first[-global]`
//     already uses.
//   - SyncStrategyPhotosFirst: two passes, photos then videos, EACH using
//     the existing MediaTypeFilter mechanism (not new sorting logic) --
//     unless cfg.MediaTypeFilter is already narrowed to one kind, in which
//     case there's nothing left to sequence and it's a single pass exactly
//     like the default.
func uploadPasses(cfg config.Config) []config.Config {
	switch cfg.SyncStrategy {
	case config.SyncStrategySmallestFirst:
		smallestFirst := cfg
		smallestFirst.SortSmallestFirst = true
		return []config.Config{smallestFirst}
	case config.SyncStrategyPhotosFirst:
		if cfg.MediaTypeFilter != extensions.KindUnknown {
			return []config.Config{cfg}
		}
		photos := cfg
		photos.MediaTypeFilter = extensions.KindPhoto
		videos := cfg
		videos.MediaTypeFilter = extensions.KindVideo
		return []config.Config{photos, videos}
	default:
		return []config.Config{cfg}
	}
}
