package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gmizrahi/gpsync/internal/config"
	"github.com/gmizrahi/gpsync/internal/engine"
	"github.com/gmizrahi/gpsync/internal/extensions"
	"github.com/gmizrahi/gpsync/internal/quota"
	"github.com/gmizrahi/gpsync/internal/scanner"
	"github.com/gmizrahi/gpsync/internal/statedb"
	"github.com/gmizrahi/gpsync/internal/uploader"
	"github.com/spf13/cobra"
)

func syncCmd() *cobra.Command {
	var concurrencyFlag int
	var verboseFlag bool
	var forceFlag bool
	var mediaTypeFlag string
	cmd := &cobra.Command{
		Use:   "sync [folders...]",
		Short: "Scan and upload folders",
		Long: "Scans each folder that contains files and uploads what is pending, one folder at a time. Uses the configured source folders if none are given; folders and globs are accepted.\n" +
			"\n" +
			"A file that was already uploaded is never sent again, whatever its name or location.\n" +
			"\n" +
			"Examples:\n" +
			"  gpsync sync\n" +
			"  gpsync sync \"C:\\Photos\\2026\"\n" +
			"  gpsync sync \"C:\\Photos\\2026\\2026_0*\" --media-type photos",
		Args: cobra.MinimumNArgs(0),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load()
			if err != nil {
				return err
			}
			patterns := args
			if len(patterns) == 0 {
				patterns = cfg.SourceFolders
			}
			if len(patterns) == 0 {
				return fmt.Errorf("no folders given and none configured — run `gpsync setup` or pass a folder explicitly")
			}
			if concurrencyFlag > 0 {
				cfg.Concurrency = concurrencyFlag
			}
			mediaKind, err := extensions.ParseKind(mediaTypeFlag)
			if err != nil {
				return err
			}
			cfg.MediaTypeFilter = mediaKind
			scanner.SetMediaKindFilter(mediaKind)

			units, err := engine.DiscoverSyncUnits(patterns)
			if err != nil {
				return err
			}
			if len(units) == 0 {
				return fmt.Errorf("no folders containing files found under: %s", strings.Join(patterns, ", "))
			}

			db, err := statedb.Open()
			if err != nil {
				return err
			}
			defer db.Close()
			// Ctrl+C during a run must leave a record of WHY it stopped,
			// not a row stuck at status='running' forever.
			defer handleInterrupts(db)()
			reportAutoClean(db)

			fmt.Printf("Found %d folder(s) to sync (recursively discovered under: %s):\n", len(units), strings.Join(patterns, ", "))
			if verboseFlag {
				for _, u := range units {
					fmt.Printf("  %s\n", u)
				}
			}

			// A cheap, no-hashing pass over every discovered folder -- just
			// directory listings and file sizes -- so the live dashboard can
			// show a whole-batch estimate even though folders 2..N haven't
			// actually been scanned yet. With --force, everything in scope
			// gets re-pushed regardless of current ledger status, so the
			// "already synced" estimate would be actively misleading -- skip it.
			batchFiles, batchBytes, batchSynced, batchSyncedBytes, err := estimateBatchTotals(db, units)
			if err != nil {
				return fmt.Errorf("batch estimate failed: %w", err)
			}
			if forceFlag {
				batchSynced, batchSyncedBytes = 0, 0
			}
			fmt.Printf("Batch estimate: %d files, %s total", batchFiles, humanBytes(batchBytes))
			if batchSynced > 0 {
				fmt.Printf(" (%d already synced, %s)", batchSynced, humanBytes(batchSyncedBytes))
			}
			fmt.Println()
			fmt.Println()
			batch := newBatchTracker(batchFiles, batchBytes, batchSynced, batchSyncedBytes)
			// One limiter for the whole invocation, not one per folder --
			// see runUploadWithDashboard. It cold-starts at 1 and ramps up
			// once, across all the folders, rather than restarting that ramp
			// (and forgetting any throttle it already backed off from) every
			// time the loop moves to the next folder. breaker is the same
			// idea for the circuit breaker's rung position.
			conc := quota.NewAdaptiveConcurrency(cfg.Concurrency)
			breaker := uploader.NewCircuitBreakerState()

			grandStart := time.Now()

			totals, err := syncFolders(units, func(folder string) (syncTotals, error) {
				if forceFlag {
					n, rerr := db.ResetStatusUnder([]string{folder})
					if rerr != nil {
						return syncTotals{}, fmt.Errorf("--force reset failed: %w", rerr)
					}
					if n > 0 {
						fmt.Printf("  %s reset %s already-tracked file(s) back to pending\n", colWarn("--force:"), colWarn(fmt.Sprintf("%d", n)))
					}
				}
				synced, total, failedPerm, failedRetry, serr := syncOneFolder(db, cfg, folder, verboseFlag, batch, conc, breaker)
				return syncTotals{synced: synced, total: total, failedPerm: failedPerm, failedRetry: failedRetry}, serr
			})
			if err != nil {
				return err
			}

			fmt.Println(colDim(strings.Repeat("─", separatorWidth)))
			fmt.Println(colHeader(fmt.Sprintf("All done in %s. %s of %d fully synced across %d folder(s).",
				time.Since(grandStart).Round(time.Second), colOK(fmt.Sprintf("%d", totals.synced)), totals.total, len(units))))
			if totals.failedPerm > 0 {
				fmt.Printf("  %s permanently failed (run `gpsync log --permanent` for why).\n", colErr(fmt.Sprintf("%d", totals.failedPerm)))
			}
			if totals.failedRetry > 0 {
				fmt.Printf("  %s will retry on the next `gpsync sync`/`gpsync upload`.\n", colWarn(fmt.Sprintf("%d", totals.failedRetry)))
			}
			resolveMissingFiles(db, cfg, false)
			return nil
		},
	}
	cmd.Flags().IntVar(&concurrencyFlag, "concurrency", 0, "Parallel uploads (overrides config)")
	cmd.Flags().BoolVar(&verboseFlag, "verbose", false, "List every file's status at the end")
	cmd.Flags().BoolVar(&forceFlag, "force", false, "Upload everything in these folders again, including files already uploaded or marked synced")
	cmd.Flags().StringVar(&mediaTypeFlag, "media-type", "", "Only photos or videos (default: all)")
	return cmd
}

// syncTotals accumulates one folder's -- or a whole batch's -- sync counts.
type syncTotals struct {
	synced, total, failedPerm, failedRetry int
}

func (t *syncTotals) add(o syncTotals) {
	t.synced += o.synced
	t.total += o.total
	t.failedPerm += o.failedPerm
	t.failedRetry += o.failedRetry
}

// syncFolders drives `gpsync sync`'s folder-by-folder loop. syncOne is
// injected so the loop's control flow -- in particular that it STOPS on the
// uploader's deliberate stop-the-run signal -- is testable without a
// network, OAuth credentials, or a real upload.
//
// On uploader.ErrRunAborted (the throttle circuit breaker having exhausted
// its whole backoff schedule, or genuine daily-quota exhaustion) the
// remaining folders are deliberately NOT attempted. Both conditions are
// account/project-wide, so every subsequent folder would hit the identical
// wall -- and would have to sit through the entire backoff ladder again to
// discover it. The counts gathered so far are still returned and reported:
// the work that did happen is real.
func syncFolders(units []string, syncOne func(folder string) (syncTotals, error)) (syncTotals, error) {
	var grand syncTotals
	for idx, folder := range units {
		printFolderHeader(idx+1, len(units), folder)
		got, err := syncOne(folder)
		// These counts are real whether or not the run was stopped early,
		// so fold them in before deciding what to do about err.
		grand.add(got)
		if err != nil {
			if errors.Is(err, uploader.ErrRunAborted) {
				printRunAborted(err, idx+1, len(units))
				return grand, nil
			}
			return grand, fmt.Errorf("%s: %w", folder, err)
		}
		fmt.Println()
	}
	return grand, nil
}

// estimateBatchTotals is a fast, read-only, no-hashing pass over every
// discovered sync unit -- just directory listings and file sizes -- so
// `gpsync sync`'s batch-level dashboard can show a whole-run estimate before
// folders 2..N have actually been scanned. "Already synced" is a
// best-effort look at the files_seen cache (the same cache a real scan
// uses to skip re-hashing unchanged files): a file whose path/size/mtime
// match what was recorded there, and whose resulting content hash is
// already 'uploaded', is counted without touching the network or
// re-reading file bytes. Anything not in that cache yet (new, or changed
// since last scan) is simply not counted as already-synced -- it'll be
// found out for real once its folder is actually scanned.
func estimateBatchTotals(db *statedb.DB, units []string) (filesTotal int, bytesTotal int64, alreadySynced int, alreadySyncedBytes int64, err error) {
	for _, folder := range units {
		entries, rerr := os.ReadDir(folder)
		if rerr != nil {
			continue
		}
		for _, e := range entries {
			if e.IsDir() || scanner.IsIgnoredFileName(e.Name()) {
				continue
			}
			info, ierr := e.Info()
			if ierr != nil {
				continue
			}
			filesTotal++
			bytesTotal += info.Size()

			seen, serr := db.GetFileSeen(filepath.Join(folder, e.Name()))
			if serr != nil || seen == nil {
				continue
			}
			mtime := float64(info.ModTime().UnixNano()) / 1e9
			if seen.Mtime != mtime || seen.Size != info.Size() {
				continue
			}
			up, uerr := db.GetUpload(seen.SHA256)
			if uerr == nil && up != nil && up.Status == "uploaded" {
				alreadySynced++
				alreadySyncedBytes += info.Size()
			}
		}
	}
	return filesTotal, bytesTotal, alreadySynced, alreadySyncedBytes, nil
}

// runUploadWithDashboard is shared by `gpsync sync` and `gpsync upload`: runs the
// uploader against scopeFolders with a live dashboard (permanent per-file
// log + redrawn bytes/rate/ETA/in-flight block, rclone -P style).
//
// conc is the batch-wide adaptive concurrency limiter, threaded through
// with the same lifetime as batch: one per invocation, shared by every
// folder, so the safe request rate the run has discovered isn't relearned
// from scratch at each folder boundary. nil is valid and means "fresh
// limiter" (see uploader.New). breaker is the same idea for the throttle
// circuit breaker's rung position -- without sharing it, a throttle still
// in progress when the next folder starts gets treated as brand new
// (rung 0, 5s) instead of continuing anywhere near where the last folder
// left off; nil is valid and means "fresh, unshared state".
// labelOverride, if non-empty, replaces the default "(entire library)" /
// joined-scopeFolders header -- used by --smallest-first-global, whose
// scopeFolders can span several (or zero) folders at once and whose header
// should say so rather than imply a single-folder run.
func runUploadWithDashboard(db *statedb.DB, cfg config.Config, scopeFolders []string, batch *batchTracker, conc *quota.AdaptiveConcurrency, breaker *uploader.CircuitBreakerState, labelOverride string) (uploader.Stats, error) {
	pending, err := db.ListPendingUnder(scopeFolders)
	if err != nil {
		return uploader.Stats{}, err
	}
	var bytesTotal int64
	for _, row := range pending {
		bytesTotal += row.Size
	}

	folderLabel := "(entire library)"
	if len(scopeFolders) > 0 {
		folderLabel = strings.Join(scopeFolders, ", ")
	}
	if labelOverride != "" {
		folderLabel = labelOverride
	}
	if cfg.UploadQuality == "space_saver" {
		// Not cosmetic: re-encoding drops EXIF, and since batchCreate has no
		// date field, Google falls back to the upload time and every
		// downscaled photo gets the wrong "date taken". Worth saying out
		// loud rather than letting it be discovered months later.
		fmt.Printf("  %s space-saver re-encodes images, which %s — Google will show the upload date for those files.\n",
			colWarn("note:"), colWarn("strips their EXIF capture date"))
	}
	dash := newDashboard(folderLabel, len(pending), bytesTotal, batch)
	u, err := uploader.New(context.Background(), db, cfg, scopeFolders, conc, breaker, nil, nil, printSkip, dash.onBytes, dash.onThrottle, dash.onBackoff, dash.onProgress, dash.onDBError)
	if err != nil {
		return uploader.Stats{}, err
	}
	stats, runErr := u.Run()
	dash.finish()
	return stats, runErr
}

// syncOneFolder runs scan -> upload scoped to a single folder and returns
// (synced, total, failedPermanent, failedRetryable). Dedup is by content
// hash (see EnsurePending/EnsureMarkedSynced in statedb): once a hash has
// been uploaded or marked synced, re-scanning it anywhere never re-uploads
// it, so no separate reconcile-against-the-API step is needed.
// cliWakePollFallback mirrors internal/engine.cycleWakePollFallback -- see
// its own doc comment.
const cliWakePollFallback = 3 * time.Second

// syncOneFolder scans and uploads one folder CONCURRENTLY. This used to
// run the scan to completion and only THEN upload, which left the
// uploader idle while a long scan finished; now the scan runs in its own
// goroutine while the upload
// loop below dispatches whatever's already pending (including files the
// scan is still in the middle of discovering) without waiting for it to
// finish. Same design as internal/engine.RunFolderCycle (gpsync-tray's
// equivalent) -- see its own, more detailed doc comment for the full
// rationale; this is the CLI's version, with a live console dashboard
// instead of a polled HTTP status endpoint.
//
// The live display reports the two phases separately -- see dashboard's
// onScanProgress/scanFinished and its "Scanning:"/"Uploading from:" lines.
//
// "quiet" (collapsing an entirely uneventful cycle to one line instead of
// the full dashboard -- the overwhelming common case for `gpsync watch`'s
// backlog drain, which revisits every folder regardless of whether it has
// anything outstanding) is now decided lazily instead of up front: the
// dashboard starts inactive (accumulating scan progress but painting
// nothing -- see dashboard.active's doc comment) and is only activated the
// moment real work is confirmed, either immediately (alreadyPending, or
// --verbose) or the first time the loop below finds something pending.
func syncOneFolder(db *statedb.DB, cfg config.Config, folder string, verbose bool, batch *batchTracker, conc *quota.AdaptiveConcurrency, breaker *uploader.CircuitBreakerState) (int, int, int, int, error) {
	scope := []string{folder}

	// Sweep failed_retryable rows back to pending BEFORE the loop below
	// decides whether there's real work -- see RunFolderCycle's own doc
	// comment (internal/engine/cycle.go) for the full real-report
	// explanation of the stuck-forever bug this fixes. RequeueRetryable
	// is unscoped (every failed_retryable row in the whole DB), matching
	// Uploader.Run()'s own existing unconditional call to it.
	if _, err := db.RequeueRetryable(); err != nil {
		return 0, 0, 0, 0, fmt.Errorf("requeuing retryable files: %w", err)
	}

	pendingBefore, perr := db.ListPendingUnder(scope)
	alreadyPending := perr == nil && len(pendingBefore) > 0

	dash := newLazyDashboard(folder, batch)

	wake := make(chan struct{}, 1)
	poke := func() {
		select {
		case wake <- struct{}{}:
		default:
		}
	}

	scanStart := time.Now()
	scanDone := make(chan struct{})
	var scanErr error
	go func() {
		defer close(scanDone)
		var summary scanner.ScanSummary
		summary, scanErr = scanner.ScanFolders(db, scope, "manual_scan", defaultScanConcurrency, false, false,
			nil, nil, func(p scanner.ProgressEvent) {
				dash.onScanProgress(p)
				poke()
			})
		dash.scanFinished(summary, time.Since(scanStart))
		poke()
	}()
	// Unlike RunFolderCycle (gpsync-tray), this CLI path prints to a real,
	// SHARED terminal -- syncFolders calls this function once per folder
	// in a loop, each with its own *dashboard writing to the same stdout.
	// Returning early (an aborted upload, say) while this folder's scan
	// goroutine is still mid-flight risks its trailing scanFinished()
	// print landing AFTER the next folder's dashboard has already started
	// painting its own live block -- genuinely garbled, interleaved
	// terminal output, not just a cosmetic ordering nit. RunFolderCycle
	// can safely leave its scan goroutine running in the background
	// because its only "output" is mutex-guarded in-memory state read by
	// an HTTP handler; this one can't take that shortcut. Every return
	// below waits for it first.
	defer func() { <-scanDone }()
	scanIsDone := func() bool {
		select {
		case <-scanDone:
			return true
		default:
			return false
		}
	}

	// --verbose asks for the full live picture even when there's nothing
	// new -- don't collapse it out from under someone who explicitly asked
	// for more detail, not less.
	activated := alreadyPending || verbose
	if activated {
		dash.activate()
	}

	uploadStart := time.Now()
	var lastStats uploader.Stats
	var uploadErr error
	for {
		pending, lerr := db.ListPendingUnder(scope)
		if lerr != nil {
			return 0, 0, 0, 0, fmt.Errorf("checking pending: %w", lerr)
		}
		if len(pending) == 0 {
			if scanIsDone() {
				break
			}
			select {
			case <-wake:
			case <-scanDone:
			case <-time.After(cliWakePollFallback):
			}
			continue
		}

		if !activated {
			activated = true
			dash.activate()
		}
		var bytesTotal int64
		for _, row := range pending {
			bytesTotal += row.Size
		}
		dash.refreshTotals(len(pending), bytesTotal)

		u, err := uploader.New(context.Background(), db, cfg, scope, conc, breaker, nil, nil, printSkip, dash.onBytes, dash.onThrottle, dash.onBackoff, dash.onProgress, dash.onDBError)
		if err != nil {
			return 0, 0, 0, 0, fmt.Errorf("upload failed: %w", err)
		}
		stats, runErr := u.Run()
		lastStats.Uploaded += stats.Uploaded
		lastStats.FailedPermanent += stats.FailedPermanent
		lastStats.FailedRetryable += stats.FailedRetryable
		lastStats.SkippedQuota += stats.SkippedQuota
		lastStats.SkippedMediaType += stats.SkippedMediaType
		if runErr != nil {
			// An aborted run (throttle backoff exhausted / daily quota) is
			// expected, handled behavior, and its Stats are real -- so
			// fall through to print this folder's summary as usual and
			// hand the signal up, rather than bailing out here as if the
			// folder had crashed. A genuine failure returns immediately.
			if !errors.Is(runErr, uploader.ErrRunAborted) {
				return 0, 0, 0, 0, fmt.Errorf("upload failed: %w", runErr)
			}
			uploadErr = runErr
			break
		}
		// Otherwise this spun forever: `gpsync sync --media-type photos
		// <folder>` (or --media-type videos) against a folder that already
		// has pending rows of the OTHER kind (from an earlier unfiltered
		// scan) spun this loop forever -- Run() returns cleanly with
		// SkippedMediaType>0 and no error, leaving `pending` above
		// unchanged on the next lap, with no ctx check reached to make it
		// stoppable. If this lap resolved nothing at all despite pending
		// being non-empty, every remaining row is excluded from every pass
		// this cfg will ever run -- stop here instead of spinning; the
		// files stay `pending` and pick right back up on the next
		// `gpsync sync`/`gpsync upload` (e.g. after --media-type is dropped).
		if stats.Uploaded+stats.FailedPermanent+stats.FailedRetryable == 0 {
			abort := &uploader.AbortError{
				Reason:    uploader.AbortNoEligibleFiles,
				Detail:    fmt.Sprintf("%d file(s) excluded by the media-type filter under current settings", stats.SkippedMediaType),
				Remaining: len(pending),
			}
			if werr := db.RunEnd(abort.LedgerReason(), abort.Summary()); werr != nil {
				dash.onDBError("recording run outcome", werr)
			}
			uploadErr = abort
			break
		}
	}

	// Wait for the scan goroutine HERE, before dash.finish() and any of
	// the plain fmt.Printf summary lines below -- not just at the
	// deferred function return -- so scanFinished()'s own permanent-log
	// print (which goes through dash's clearDrawn()/redraw() discipline)
	// can never land in between and get scribbled over, or interleave
	// with the un-coordinated Printfs that follow. See the defer above
	// this loop for why this CLI path can't take RunFolderCycle's
	// "leave it running in the background" shortcut.
	<-scanDone
	dash.finish()

	if !activated {
		// Nothing was ever pending, and the scan found nothing new either.
		total, byStatus, serr := db.SyncStatusUnder(scope)
		if serr != nil {
			return 0, 0, 0, 0, serr
		}
		fmt.Printf("  %s already synced (%d file(s))\n", colOK("✓"), total)
		return byStatus["uploaded"], total, byStatus["failed_permanent"], byStatus["failed_retryable"], nil
	}

	if scanErr != nil {
		return 0, 0, 0, 0, fmt.Errorf("scan failed: %w", scanErr)
	}

	skippedMediaSuffix := ""
	if lastStats.SkippedMediaType > 0 {
		skippedMediaSuffix = fmt.Sprintf(", %d skipped (media-type)", lastStats.SkippedMediaType)
	}
	fmt.Printf("  %s upload done in %s — %s uploaded, %s failed permanent / %s retryable, %d skipped (quota)%s\n",
		colHeader("Uploading from:"), time.Since(uploadStart).Round(time.Second), colOK(fmt.Sprintf("%d", lastStats.Uploaded)),
		colErr(fmt.Sprintf("%d", lastStats.FailedPermanent)), colWarn(fmt.Sprintf("%d", lastStats.FailedRetryable)), lastStats.SkippedQuota, skippedMediaSuffix)

	// ── summary ──
	total, byStatus, err := db.SyncStatusUnder(scope)
	if err != nil {
		return 0, 0, 0, 0, err
	}
	synced := byStatus["uploaded"]
	failedPerm := byStatus["failed_permanent"]
	failedRetry := byStatus["failed_retryable"]
	fmt.Printf("  %s of %d fully synced.\n", colOK(fmt.Sprintf("%d", synced)), total)

	if verbose {
		fmt.Println("  Per-file status:")
		details, err := db.SyncDetailUnder(scope)
		if err != nil {
			return synced, total, failedPerm, failedRetry, err
		}
		for _, d := range details {
			line := fmt.Sprintf("    %-70s %s", d.Path, d.Status)
			if d.Message != "" {
				line += " — " + d.Message
			}
			fmt.Println(line)
		}
	}

	// uploadErr is nil, or the "stop the whole run" signal the caller must
	// act on (see printRunAborted).
	return synced, total, failedPerm, failedRetry, uploadErr
}
