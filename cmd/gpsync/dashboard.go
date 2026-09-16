package main

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/fatih/color"

	"github.com/gmizrahi/gpsync/internal/scanner"
	"github.com/gmizrahi/gpsync/internal/units"
	"github.com/gmizrahi/gpsync/internal/uploader"
)

// dashboard renders a live upload view in the rclone -P spirit: a permanent
// scrolling log of completed files above a redrawn-in-place block showing
// overall bytes/rate/ETA, file counts, elapsed time, and every file
// currently transferring with its own live percent/size/speed. Falls back
// to just the permanent log (no redraw) when stdout isn't a real terminal.
type dashboard struct {
	mu           sync.Mutex
	folder       string // shown in every repaint, not just the one-time header, so it's visible past a long scrollback
	start        time.Time
	filesDone    int
	filesTotal   int
	bytesDone    int64
	bytesTotal   int64
	inFlight     map[string]*fileTransfer
	linesDrawn   int
	lastRedrawAt time.Time
	batch        *batchTracker // non-nil only for `gpsync sync`'s multi-folder runs -- see batchTracker
	// backoff is the live circuit-breaker pause state, non-nil only while
	// the whole pipeline is paused waiting out a throttle. Rendered as a
	// section of the block rather than a printed line, because it is state
	// that changes every second, not an event.
	backoff *uploader.BackoffStatus
	// pausedTotal/pauseStart exclude circuit-breaker pauses from the
	// transfer rate. During a pause nothing is being sent by design, so
	// counting that wall-clock time against the bytes makes the rate
	// collapse and stay wrong for the rest of the folder -- a 15-minute
	// rung would report a rate near zero even while the pipeline is
	// perfectly healthy on either side of it.
	pausedTotal time.Duration
	pauseStart  time.Time
	// active gates redraw() -- see activate()'s doc comment. Starts false so
	// syncOneFolder can create a dashboard immediately (to accumulate scan
	// progress with nowhere else to put it) without committing to showing
	// the live block for what might turn out to be a genuinely uneventful
	// cycle.
	active bool
	// scanLine is the live "Scanning: ..." status -- see onScanProgress.
	// Empty means nothing to show (scan hasn't started yet, or has already
	// finished).
	scanLine string
}

// activate flips the dashboard from silently accumulating state to actually
// painting the live block -- called the moment syncOneFolder confirms this
// cycle has real work (something already pending, or the concurrent scan
// found something new), never for a cycle that turns out to have nothing to
// do at all (which stays on the single-line "quiet" summary instead, same
// as before scan and upload ran concurrently).
func (d *dashboard) activate() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.active = true
	d.redraw()
}

// refreshTotals recomputes filesTotal/bytesTotal from however much is
// pending RIGHT NOW plus whatever's already done -- called before each
// dispatch loop in syncOneFolder, since the concurrent scan can keep
// discovering more work throughout the cycle (not just once, up front, the
// way a fully-sequential scan-then-upload could get away with).
func (d *dashboard) refreshTotals(pendingCount int, pendingBytes int64) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.filesTotal = d.filesDone + pendingCount
	d.bytesTotal = d.bytesDone + pendingBytes
}

// onScanProgress is scanner.ScanFolders' onProgress callback, run
// concurrently with the upload phase below (see syncOneFolder) -- a real
// report: "why do you wait before your scan gets to the folder; start the
// upload right away, the scan and queueing of new discovered files should
// be done in a parallel thread", with the live display split per the same
// report's own wording: "you can split to scanning: XXXX and uploading
// from: YYYY". Folded into this SAME dashboard/redraw() rather than a
// second, independent live line -- redraw() already owns the one correct
// cursor-math discipline for this terminal (see its own doc comment); a
// competing independent \r-updating line from a second goroutine would
// race it and garble the screen.
func (d *dashboard) onScanProgress(p scanner.ProgressEvent) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if p.Done >= p.Total {
		d.scanLine = ""
	} else {
		d.scanLine = fmt.Sprintf("%d/%d  %s", p.Done, p.Total, formatInFlight(p.InFlight))
	}
	d.redrawThrottled()
}

// scanFinished clears the live "Scanning:" line for good (called once
// scanner.ScanFolders returns) and prints its own permanent one-line
// summary through the same clearDrawn()/redraw() discipline as
// logCompleted -- an uncoordinated fmt.Printf here would get silently
// scribbled over by the very next repaint, same reasoning as onThrottle's
// own doc comment. A no-op if the dashboard was never activated (a
// genuinely quiet cycle) -- syncOneFolder prints its own single-line
// summary for that case instead.
func (d *dashboard) scanFinished(summary scanner.ScanSummary, elapsed time.Duration) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.scanLine = ""
	if !d.active {
		return
	}
	skippedSuffix := ""
	if summary.Skipped > 0 {
		skippedSuffix = fmt.Sprintf(", %s skipped (junk/unsupported)", colWarn(fmt.Sprintf("%d", summary.Skipped)))
	}
	d.clearDrawn()
	fmt.Printf("  %s scan done in %s — %d files seen, %d hashed, %s newly queued%s\n",
		colHeader("Scanning:"), elapsed.Round(time.Second), summary.FilesSeen, summary.FilesHashed,
		colOK(fmt.Sprintf("%d", summary.NewPending)), skippedSuffix)
	d.redraw()
}

// batchTracker accumulates progress across every folder in a `gpsync sync`
// run, shared by every folder's dashboard in turn (a fresh dashboard is
// created per folder, but the batchTracker persists across the whole loop).
// Only one folder is ever scanned/uploaded at a time, so the per-folder
// dashboard alone can't show "how far through the whole batch am I" --
// this is what fills that gap. filesTotal/bytesTotal/alreadySynced come
// from a one-time, no-hashing pre-scan (see estimateBatchTotals) since the
// folders after the current one haven't been scanned yet.
type batchTracker struct {
	mu            sync.Mutex
	start         time.Time
	filesTotal    int
	bytesTotal    int64
	alreadySynced int // of filesTotal -- informational, and seeded into filesDone/bytesDone below so the count doesn't start misleadingly at 0
	filesDone     int
	bytesDone     int64
}

func newBatchTracker(filesTotal int, bytesTotal int64, alreadySynced int, alreadySyncedBytes int64) *batchTracker {
	return &batchTracker{
		start:         time.Now(),
		filesTotal:    filesTotal,
		bytesTotal:    bytesTotal,
		alreadySynced: alreadySynced,
		filesDone:     alreadySynced,
		bytesDone:     alreadySyncedBytes,
	}
}

func (b *batchTracker) recordCompleted(ok bool, size int64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.filesDone++
	if ok {
		b.bytesDone += size
	}
}

type batchSnapshot struct {
	start                 time.Time
	filesDone, filesTotal int
	bytesDone, bytesTotal int64
	alreadySynced         int
}

func (b *batchTracker) snapshot() batchSnapshot {
	b.mu.Lock()
	defer b.mu.Unlock()
	return batchSnapshot{
		start: b.start, filesDone: b.filesDone, filesTotal: b.filesTotal,
		bytesDone: b.bytesDone, bytesTotal: b.bytesTotal, alreadySynced: b.alreadySynced,
	}
}

// redrawInterval caps how often the live block repaints. onBytes fires on
// every network read -- many times per second per in-flight file -- so
// redrawing on every call is both wasteful and, worse, visibly flickers.
const redrawInterval = 200 * time.Millisecond

type fileTransfer struct {
	sent, total int64
	lastSent    int64
	lastTime    time.Time
	speed       float64 // bytes/sec, exponentially smoothed
}

// newDashboard creates an ACTIVE dashboard (paints immediately, same as
// every caller before dashboard.active existed) -- runUploadWithDashboard
// and the other pure-upload commands always have real work by construction
// time, so there's nothing to lazily decide. syncOneFolder is the one
// caller that doesn't know that yet at construction time (a concurrent
// scan might still turn up nothing) -- see newLazyDashboard.
func newDashboard(folder string, filesTotal int, bytesTotal int64, batch *batchTracker) *dashboard {
	return &dashboard{
		folder:     folder,
		start:      time.Now(),
		filesTotal: filesTotal,
		bytesTotal: bytesTotal,
		inFlight:   map[string]*fileTransfer{},
		batch:      batch,
		active:     true,
	}
}

// newLazyDashboard is newDashboard's inactive twin -- see dashboard.active
// and activate()'s own doc comments for why syncOneFolder needs this
// instead.
func newLazyDashboard(folder string, batch *batchTracker) *dashboard {
	return &dashboard{
		folder:   folder,
		start:    time.Now(),
		inFlight: map[string]*fileTransfer{},
		batch:    batch,
	}
}

// onThrottle is the uploader's onThrottle callback. It must go through the
// same clearDrawn()/redraw() bookkeeping as logCompleted below, not a bare
// fmt.Printf -- the live block's redraw() moves the cursor up by exactly
// d.linesDrawn lines to overwrite it in place, and has no idea a throttle
// line got printed independently in between. An uncoordinated Printf here
// gets silently scribbled over by the very next repaint (which can be a
// fraction of a second later, since onBytes fires many times/sec), making
// backoff/quota events invisible even though they did fire -- exactly the
// "it just stops with no error" symptom.
func (d *dashboard) onThrottle(message string, backoffSeconds float64, newConcurrency int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.clearDrawn()
	if backoffSeconds > 0 {
		fmt.Printf("  %s %s — backing off %.1fs, concurrency now %s\n",
			colWarn("throttled:"), message, backoffSeconds, colWarn(fmt.Sprintf("%d", newConcurrency)))
	} else {
		fmt.Printf("  %s %s\n", colErr("quota:"), message)
	}
	d.redraw()
}

// onDBError is the uploader's onDBError callback: a ledger write on the
// upload hot path failed. Goes through the same clearDrawn()/redraw()
// bookkeeping as onThrottle above, for exactly the same reason -- a bare
// Printf here would be scribbled over by the next repaint a fraction of a
// second later, which for this particular class of problem (the ledger not
// recording an upload that actually happened) would be the worst possible
// outcome: silent, and the file gets re-uploaded next run.
func (d *dashboard) onDBError(context string, err error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.clearDrawn()
	printDBError(context, err)
	d.redraw()
}

// onBackoff is the uploader's onBackoff callback: the live state of a
// circuit-breaker pause, arriving about once a second for the whole pause.
//
// Unlike onThrottle/onDBError this does NOT print a permanent line -- it
// updates a section of the redrawn block and repaints in place, so a
// 15-minute pause shows a ticking countdown rather than a frozen screen
// with no explanation. onThrottle still writes the one-shot scrollback
// record when the pause begins; the two are complementary (history vs.
// current state) and only this one repeats.
//
// The repaint is deliberately unthrottled: during a pause dispatch is
// stopped, so no onBytes/onProgress events are firing and redrawThrottled's
// rate limiter would have nothing else to coalesce with -- these ticks are
// the only thing driving the display, at a sedate 1/sec.
func (d *dashboard) onBackoff(s uploader.BackoffStatus) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if s.Active {
		copied := s
		d.backoff = &copied
		if d.pauseStart.IsZero() {
			d.pauseStart = time.Now()
		}
	} else {
		d.backoff = nil
		if !d.pauseStart.IsZero() {
			d.pausedTotal += time.Since(d.pauseStart)
			d.pauseStart = time.Time{}
		}
	}
	d.redraw()
}

func (d *dashboard) onBytes(e uploader.ByteProgressEvent) {
	d.mu.Lock()
	defer d.mu.Unlock()

	ft, ok := d.inFlight[e.Path]
	if !ok {
		ft = &fileTransfer{total: e.Total, lastTime: time.Now()}
		d.inFlight[e.Path] = ft
	}
	// A retry of the same file restarts its progressReader's counter at 0
	// while lastSent still holds the previous attempt's high-water mark --
	// so the next instantaneous-rate calculation below would divide a large
	// NEGATIVE delta by dt and print nonsense like "-52428800B/s". Treat a
	// counter that went backwards as a fresh transfer and rebuild the rate
	// from scratch.
	if e.Sent < ft.sent || e.Sent < ft.lastSent {
		ft.lastSent = 0
		ft.speed = 0
		ft.lastTime = time.Now()
	}
	now := time.Now()
	if dt := now.Sub(ft.lastTime).Seconds(); dt > 0.05 { // avoid noisy divide-by-tiny-dt
		inst := float64(e.Sent-ft.lastSent) / dt
		if ft.speed == 0 {
			ft.speed = inst
		} else {
			ft.speed = 0.7*ft.speed + 0.3*inst
		}
		ft.lastSent = e.Sent
		ft.lastTime = now
	}
	ft.sent = e.Sent

	// The chunk that actually reaches 100% must always paint -- it's the
	// last onBytes call for this file before onProgress removes it from
	// the list entirely, so throttling it away means the file visibly
	// jumps from some partial percent straight to gone, never showing 100%.
	if e.Total > 0 && e.Sent >= e.Total {
		d.redraw()
	} else {
		d.redrawThrottled()
	}
}

func (d *dashboard) onProgress(e uploader.ProgressEvent) {
	d.mu.Lock()
	defer d.mu.Unlock()

	if e.Total > 0 {
		d.filesTotal = e.Total
	}
	if e.LastFile != "" {
		// Always drop it from the in-flight list -- it is no longer
		// transferring either way -- but a DEFERRED file is not finished:
		// it was set aside for the circuit breaker to retry after its
		// pause. Counting it here would count it a second time when the
		// retry lands, and would make the batch totals overshoot.
		delete(d.inFlight, e.LastFile)
		if e.LastDeferred {
			d.logDeferred(e.LastFile, e.LastErrorMessage)
		} else {
			d.filesDone = e.Done
			if e.LastOK {
				d.bytesDone += e.LastSize
			}
			if d.batch != nil {
				d.batch.recordCompleted(e.LastOK, e.LastSize)
			}
			d.logCompleted(e.LastFile, e.LastOK, e.LastSize, e.LastErrorMessage)
		}
	}
	d.redraw()
}

// maxReasonLen caps how much of a failure reason one log line carries.
// Google's error messages run long ("Quota exceeded for quota 'concurrent
// write request' of service 'photoslibrary.googleapis.com' for consumer
// ..."), and a folder where dozens of files fail in a row must stay
// readable -- one line per file, never wrapped into several. 54 keeps the
// informative head of that message ("Quota exceeded for quota 'concurrent
// write request'") and drops the boilerplate service/consumer tail, without
// any per-message special-casing.
const maxReasonLen = 54

// logNameWidth is the fixed-width filename column in the permanent log. It
// is padded so the trailing detail lines up across files, but at the old 45
// it left a chasm of whitespace after every normal-length photo name.
const logNameWidth = 32

// logCompleted prints one permanent line per finished file -- never
// overwritten, so the full transfer history stays visible in the scrollback
// the way rclone's own log does. A failed file carries its reason inline:
// without it the user watching a run sees a column of bare "failed" marks
// with no way to tell a quota throttle from a rejected file format, and has
// to go run `gpsync log` separately to find out what happened.
func (d *dashboard) logCompleted(path string, ok bool, size int64, errorMessage string) {
	d.clearDrawn()
	fmt.Println(formatCompletedLine(time.Now(), path, ok, size, errorMessage))
}

// formatCompletedLine is split out of logCompleted so the line's content
// (in particular that a failure's reason actually reaches it) is testable
// without capturing stdout.
func formatCompletedLine(at time.Time, path string, ok bool, size int64, errorMessage string) string {
	mark, verb := checkMark, "uploaded"
	if !ok {
		mark, verb = crossMark, "failed"
	}
	detail := fmt.Sprintf("%s (%s)", verb, humanBytes(size))
	if !ok && errorMessage != "" {
		detail = fmt.Sprintf("%s (%s) — %s", verb, humanBytes(size), singleLine(errorMessage, maxReasonLen))
	}
	return fmt.Sprintf("%s %s %s %s", colDim(at.Format("15:04:05")), mark, logName(path), colDim(detail))
}

// logName renders the filename column: truncated to logNameWidth (by rune,
// so non-ASCII names don't get cut mid-character) and padded to it, so the
// detail column stays aligned whatever the name's length.
func logName(path string) string {
	return fmt.Sprintf("%-*s", logNameWidth, truncateName(filepath.Base(path), logNameWidth))
}

// logDeferred prints the permanent line for a file the throttle circuit
// breaker set aside. Deliberately distinct from a failure: nothing is wrong
// with this file, it just hasn't been retried yet, and printing it as
// "failed" would be a lie the user would act on.
func (d *dashboard) logDeferred(path string, reason string) {
	d.clearDrawn()
	fmt.Println(formatDeferredLine(time.Now(), path, reason))
}

func formatDeferredLine(at time.Time, path string, reason string) string {
	detail := "held for retry after backoff"
	if reason != "" {
		detail = fmt.Sprintf("held for retry — %s", singleLine(reason, maxReasonLen))
	}
	return fmt.Sprintf("%s %s %s %s", colDim(at.Format("15:04:05")), colWarn("⏸"), logName(path), colDim(detail))
}

// singleLine flattens any embedded newlines/tabs (some API error messages
// carry them) and truncates by RUNE count, so a non-ASCII message can't be
// cut mid-character into mojibake.
func singleLine(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	return truncateName(s, max)
}

func (d *dashboard) clearDrawn() {
	if d.linesDrawn > 0 {
		fmt.Printf("\x1b[%dA\x1b[J", d.linesDrawn)
		d.linesDrawn = 0
	}
}

// redrawThrottled is the high-frequency entry point (called from onBytes,
// which fires on every network read -- easily dozens of times per second
// across several in-flight files). Actual terminal repaints are capped to
// redrawInterval; state is still updated on every call, just not painted.
func (d *dashboard) redrawThrottled() {
	if time.Since(d.lastRedrawAt) < redrawInterval {
		return
	}
	d.redraw()
}

// redraw repaints the live block in place: move the cursor back to the top
// of the block and overwrite each line (with a trailing clear-to-end-of-line
// for anything shorter than what was there), instead of blanking the whole
// block first and reprinting -- clearing first is what caused the visible
// flicker even before throttling made it worse.
func (d *dashboard) redraw() {
	if color.NoColor {
		return // not a real terminal (piped/redirected) -- permanent log lines only, no live block
	}
	if !d.active {
		// Not yet confirmed this cycle has real work -- see activate()'s
		// doc comment. Scan progress can arrive before that's decided;
		// silently accumulate it (onScanProgress above already updated
		// scanLine) without painting anything yet.
		return
	}
	d.lastRedrawAt = time.Now()
	lines := d.blockLines()

	// Move to the top of the previous block (if any) without clearing it
	// first, then overwrite each line -- \x1b[K wipes only whatever
	// leftover characters extend past the new content on that one line.
	if d.linesDrawn > 0 {
		fmt.Printf("\x1b[%dA", d.linesDrawn)
	}
	for _, l := range lines {
		fmt.Printf("%s\x1b[K\n", l)
	}
	// The block shrank (e.g. fewer files in flight than last paint) --
	// only now do we need to clear the leftover trailing lines.
	if extra := d.linesDrawn - len(lines); extra > 0 {
		fmt.Print("\x1b[J")
	}
	d.linesDrawn = len(lines)
}

// maxInFlightRows bounds how many per-file rows the live block will ever
// render, and it is a CORRECTNESS constraint, not a matter of taste.
//
// redraw() repaints by moving the cursor up linesDrawn lines and
// overwriting. That only works while the whole block fits on screen: once
// it is taller than the terminal, the terminal scrolls, the top of the
// block is gone, and "up N lines" lands somewhere arbitrary -- every
// repaint then leaves a smeared copy behind instead of overwriting the
// previous one. Reported exactly like that, with pages of duplicated and
// half-overwritten blocks: "something is completely off with the output
// now".
//
// It took an unrelated change to expose it. A file leaves inFlight only
// when onProgress fires, which happens after mediaItems.batchCreate
// commits -- so up to batchFlushCount (50) files can sit at 100% waiting
// for one flush. With album linking gone and `--smallest-first-global`
// feeding it 300KB files, all 50 finish sending within seconds of each
// other, and the block hit ~57 lines against a ~30-line window. Bigger
// files and a busier pipeline had always kept it well under that before.
//
// 12 plus the six or so fixed lines above stays inside any terminal worth
// supporting, without needing to discover the real height (which would
// mean a new dependency for one number).
const maxInFlightRows = 12

// blockLines renders the live block's content. Split out from redraw() so
// the height bound above is actually testable -- redraw() itself writes
// ANSI escapes straight to stdout and can't be asserted on.
func (d *dashboard) blockLines() []string {
	var inFlightSent int64
	for _, ft := range d.inFlight {
		inFlightSent += ft.sent
	}
	doneOrInFlight := d.bytesDone + inFlightSent
	elapsed := time.Since(d.start)
	pct, rate, eta := d.transferFigures(doneOrInFlight)

	var lines []string
	if d.batch != nil {
		lines = append(lines, colDim(strings.Repeat("·", batchSeparatorWidth)))
		lines = append(lines, d.batchLines(inFlightSent)...)
		lines = append(lines, colDim(strings.Repeat("·", batchSeparatorWidth)))
	} else {
		// No batch block to host it (single-folder run): show the pause on
		// its own rather than not at all.
		lines = append(lines, d.backoffLines()...)
	}
	if d.scanLine != "" {
		// Scanning and uploading are reported as separate lines, shown
		// above the transfer block whenever the concurrent scan (see
		// syncOneFolder) still has work left, and removed the moment it
		// finishes (onScanProgress/scanFinished).
		lines = append(lines, fmt.Sprintf("%s %s", colHeader("Scanning:"), d.scanLine))
	}
	if d.folder != "" {
		lines = append(lines, fmt.Sprintf("%s %s", colHeader("Uploading from:"), d.folder))
	}
	lines = append(lines, fmt.Sprintf("  %s %s / %s  %s  %s  ETA %s",
		colHeader("Transferred:"), humanBytes(doneOrInFlight), humanBytes(d.bytesTotal),
		colOK(fmt.Sprintf("%.0f%%", pct)), colDim(humanBytes(int64(rate))+"/s"), colDim(eta)))
	lines = append(lines, fmt.Sprintf("  %s %d/%d files  •  %s elapsed",
		colHeader("Files:"), d.filesDone, d.filesTotal, elapsed.Round(time.Second)))

	if len(d.inFlight) > 0 {
		lines = append(lines, colHeader("  Uploading:"))
		paths := make([]string, 0, len(d.inFlight))
		for p := range d.inFlight {
			paths = append(paths, p)
		}
		sort.Strings(paths)
		// Files still moving bytes come first, so the ones worth watching
		// are never the ones the cap hides. Everything else has finished
		// sending and is only waiting on the next batchCreate -- which is
		// exactly the pile that can reach 50 at once.
		sort.SliceStable(paths, func(i, j int) bool {
			return d.stillSending(paths[i]) && !d.stillSending(paths[j])
		})
		hidden := 0
		if len(paths) > maxInFlightRows {
			hidden = len(paths) - maxInFlightRows
			paths = paths[:maxInFlightRows]
		}
		for _, p := range paths {
			ft := d.inFlight[p]
			filePct := 0.0
			if ft.total > 0 {
				filePct = float64(ft.sent) / float64(ft.total) * 100
			}
			lines = append(lines, fmt.Sprintf("    %-40s %s  %s/%s  %s",
				colFile(truncateName(filepath.Base(p), 40)), colOK(fmt.Sprintf("%3.0f%%", filePct)),
				humanBytes(ft.sent), humanBytes(ft.total), colDim(humanBytes(int64(ft.speed))+"/s")))
		}
		if hidden > 0 {
			lines = append(lines, colDim(fmt.Sprintf("    … and %d more, sent and waiting for the batch commit", hidden)))
		}
	}
	return lines
}

// stillSending reports whether a file is actually transferring, as opposed
// to having finished its bytes and being parked until batchCreate commits.
func (d *dashboard) stillSending(path string) bool {
	ft := d.inFlight[path]
	return ft != nil && (ft.total <= 0 || ft.sent < ft.total)
}

// transferringFor is wall-clock time minus every circuit-breaker pause --
// the time actually available for moving bytes.
func (d *dashboard) transferringFor() time.Duration {
	elapsed := time.Since(d.start) - d.pausedTotal
	if !d.pauseStart.IsZero() {
		elapsed -= time.Since(d.pauseStart) // currently paused
	}
	if elapsed < time.Millisecond {
		return time.Millisecond // avoid a divide-by-~zero spike on the first paint
	}
	return elapsed
}

// transferFigures computes the percentage, rate and ETA shown on one line.
//
// All three derive from the SAME byte figure. They used to disagree: the
// percentage and the "X / Y" counted bytes actually sent (including files
// whose transfer had finished but whose batchCreate had not yet landed),
// while the rate counted only ledger-CONFIRMED bytes over the same elapsed
// time. At the tail of a folder that gap is at its widest, so the line
// could read "48.1MiB / 48.1MiB 100%" next to a rate computed from
// noticeably fewer bytes -- two numbers on one line that could not both be
// true.
func (d *dashboard) transferFigures(doneOrInFlight int64) (pct, rate float64, eta string) {
	if d.bytesTotal > 0 {
		pct = float64(doneOrInFlight) / float64(d.bytesTotal) * 100
		if pct > 100 {
			pct = 100
		}
	}
	rate = float64(doneOrInFlight) / d.transferringFor().Seconds()
	eta = "-"
	if rate > 0 && d.bytesTotal > doneOrInFlight {
		secs := float64(d.bytesTotal-doneOrInFlight) / rate
		eta = time.Duration(secs * float64(time.Second)).Round(time.Second).String()
	}
	return pct, rate, eta
}

// batchLines renders the whole-batch totals block shown above the current
// folder's own block in a `gpsync sync` run. currentFolderInFlightSent is
// folded into the batch's "in progress" figure since only one folder is
// ever active at a time -- the batchTracker itself only tracks completed
// files (see recordCompleted), it has no concept of the active folder's
// partial progress on its own.
func (d *dashboard) batchLines(currentFolderInFlightSent int64) []string {
	bs := d.batch.snapshot()
	doneOrInFlight := bs.bytesDone + currentFolderInFlightSent
	elapsed := time.Since(bs.start)

	pct := 0.0
	if bs.bytesTotal > 0 {
		pct = float64(doneOrInFlight) / float64(bs.bytesTotal) * 100
		if pct > 100 {
			pct = 100
		}
	}
	rate := float64(bs.bytesDone) / elapsed.Seconds()
	eta := "-"
	if rate > 0 && bs.bytesTotal > doneOrInFlight {
		secs := float64(bs.bytesTotal-doneOrInFlight) / rate
		eta = time.Duration(secs * float64(time.Second)).Round(time.Second).String()
	}

	syncedSuffix := ""
	if bs.alreadySynced > 0 {
		syncedSuffix = fmt.Sprintf(" (%s already synced)", colDim(fmt.Sprintf("%d", bs.alreadySynced)))
	}

	lines := []string{
		colBatch("Totals for batch:"),
		fmt.Sprintf("  %s %s / %s  %s  %s  ETA %s",
			colBatch("Transferred:"), humanBytes(doneOrInFlight), humanBytes(bs.bytesTotal),
			colOK(fmt.Sprintf("%.0f%%", pct)), colDim(humanBytes(int64(rate))+"/s"), colDim(eta)),
		fmt.Sprintf("  %s %d/%d files%s  •  %s elapsed",
			colBatch("Files:"), bs.filesDone, bs.filesTotal, syncedSuffix, elapsed.Round(time.Second)),
	}
	// The pause belongs with the batch totals, not the per-folder block: a
	// circuit-breaker pause stops the entire invocation, not just the folder
	// that happened to trip it.
	return append(lines, d.backoffLines()...)
}

// backoffLines renders the live circuit-breaker pause, or nothing at all
// when the pipeline is running normally. Two lines: what stopped us, and
// where we are in the backoff ladder -- including the concurrency dispatch
// will resume at, since a pause that resumes at 1/6 means something quite
// different from one that resumes at 6/6.
func (d *dashboard) backoffLines() []string {
	if d.backoff == nil {
		return nil
	}
	b := d.backoff
	return []string{
		fmt.Sprintf("  %s %s", colErr("⏸ THROTTLED —"), singleLine(b.Reason, maxReasonLen)),
		fmt.Sprintf("    %s rung %s (%s) — retrying in %s — concurrency will resume at %s",
			colWarn("Backoff:"),
			colWarn(fmt.Sprintf("%d/%d", b.Rung, b.TotalRungs)),
			colDim(shortDuration(b.RungWait)),
			colWarn(shortDuration(b.RemainingWait)),
			colDim(fmt.Sprintf("%d/%d", b.Concurrency, b.MaxConcurrency))),
	}
}

// shortDuration renders a countdown the way a person reads one: whole
// seconds, and no trailing zero component above them ("3m", "1m30s",
// "47s"). time.Duration's own String gives "3m0s" and, mid-tick,
// "47.000000001s".
func shortDuration(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	d = d.Round(time.Second)
	if d == 0 {
		return "0s"
	}
	s := d.String()
	if strings.HasSuffix(s, "m0s") {
		s = strings.TrimSuffix(s, "0s")
	}
	if strings.HasSuffix(s, "h0m") {
		s = strings.TrimSuffix(s, "0m")
	}
	return s
}

// batchSeparatorWidth matches the folder-header rule's width (separatorWidth)
// so the batch block reads as clearly bounded above and below, not just a
// thin divider easy to miss between it and the current folder's own block.
const batchSeparatorWidth = separatorWidth

// finish paints one last, unthrottled repaint reflecting the true final
// state (100% / all files accounted for) and leaves it on screen as a
// permanent record -- like rclone's own closing summary -- rather than
// wiping the block away right as it would show completion. The caller
// prints its own additional summary line below this.
func (d *dashboard) finish() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.redraw()
}

// humanBytes is units.Bytes: KB/MB/GB/TB labels, shared with the tray.
func humanBytes(n int64) string {
	return units.Bytes(n)
}

// truncateName shortens s to at most max characters, by RUNE not by byte.
// Byte-slicing cut multi-byte characters in half on any non-ASCII filename
// -- common in a real photo library (accented folder/file names, non-Latin
// scripts) -- emitting invalid UTF-8 that renders as mojibake in the
// terminal.
func truncateName(s string, max int) string {
	if max <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	if max <= 3 {
		return string(r[:max])
	}
	return string(r[:max-3]) + "..."
}
