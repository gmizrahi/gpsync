// Package watchctl owns the watch engine's start/stop lifecycle and live,
// process-local progress tracking (current folder, in-flight files, recent
// activity, circuit-breaker backoff) -- the one implementation shared by
// gpsync-tray's systray wrapper and gpsync's own `gpsync dashboard` CLI
// command, instead of two independent copies. It has no Windows
// dependencies of its own (confirmed before this package existed: only its
// callers' surrounding UI -- a systray menu, a terminal prompt -- is
// platform-specific), so it lives here rather than behind a build tag.
package watchctl

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"runtime/debug"
	"sync"
	"time"

	"github.com/gmizrahi/gpsync/internal/config"
	"github.com/gmizrahi/gpsync/internal/engine"
	"github.com/gmizrahi/gpsync/internal/quota"
	"github.com/gmizrahi/gpsync/internal/statedb"
	"github.com/gmizrahi/gpsync/internal/uploader"
	"github.com/gmizrahi/gpsync/internal/watcher"
)

// maxRecentEvents bounds the dashboard's "recent activity" list. Raised
// from 20 -- 20 was FEWER ROWS THAN THE CARD CAN SHOW: the Status page
// deliberately stretches Uploading/Recent activity to fill the viewport's
// remaining height ("UPLOADING AND RECENT ACTIVITY should take entire
// rest of height"), which on a normal window is well over 20 rows -- so
// the old cap left a permanent blank band at the bottom of a card that
// had real history to put there, even on a long-running watch: "still
// lots of rows are kept unused... same for RECENT ACTIVITY!" 60 covers
// any realistic viewport with headroom; the dashboard's own
// measureRecentCapacity (in the dashboard package) is what
// actually decides how many of these render at once -- this is only the
// ceiling on how much history is kept available to slice from.
const maxRecentEvents = 60

// WatchController owns the engine's start/stop lifecycle so Pause/Resume
// (or, from the CLI, Ctrl+C) has something concrete to act on. Deliberately
// simple: Stop cancels everything (the loop AND the filesystem watcher);
// Start begins completely fresh, baseline pass included. No attempt to
// preserve in-flight watch state across a stop -- re-running the baseline
// on Start is correct anyway, the same "catch up on anything missed"
// behavior `gpsync watch` itself already relies on.
type WatchController struct {
	db  *statedb.DB
	cfg config.Config

	mu      sync.Mutex
	running bool
	// sourceFoldersOverride, when non-nil, pins cfg.SourceFolders against
	// every future SetConfig call -- see SetSourceFoldersOverride's own
	// doc comment for the real bug this exists to close.
	sourceFoldersOverride []string
	stopCh                chan struct{}
	// done is closed by THIS invocation's run() goroutine when it returns.
	// Deliberately a fresh channel per Start(), not a single shared
	// sync.WaitGroup -- see Stop()'s own doc comment for the race that
	// created: a shared WaitGroup let a concurrent Start() (e.g. a
	// Settings save racing a dashboard Pause click) Add(1) for a NEW
	// goroutine while an in-progress Stop() was still blocked in Wait(),
	// so that Wait() ended up waiting on the WRONG (newly-started)
	// goroutine instead of the one it was actually told to stop -- it
	// would then hang until that unrelated goroutine happened to exit on
	// its own (in practice, never), or trip Go's own "WaitGroup misuse:
	// Add called concurrently with Wait" panic, silently killing the
	// -H=windowsgui tray binary with no visible error. Capturing a
	// per-Start() channel reference locally, before releasing the lock,
	// makes each Stop() call wait on exactly the goroutine IT is
	// stopping, regardless of what any later, concurrent Start() does.
	done         chan struct{}
	manualRescan chan struct{}
	// adhoc carries a single folder to sync on demand. Buffered and
	// per-Start() like manualRescan; see SyncFolderNow.
	adhoc       chan string
	skip        *uploader.SkipSignal
	fileCancels *uploader.FileCancelRegistry

	// liveMu guards every ephemeral, process-local "what's happening right
	// now" field below -- none of it is in the ledger (run_progress only
	// has folder/file COUNTS, not which folder/file), so without this the
	// dashboard has nothing to show beyond a frozen counter: which folder,
	// which file, and its upload percentage all live here.
	liveMu        sync.Mutex
	backoff       *uploader.BackoffStatus
	currentFolder string
	// folderIndex/folderTotal back a CLI-parity "[N/M] <folder>" counter,
	// matching what the CLI prints. This is deliberately NOT the same
	// thing as run_progress's FoldersDone/FoldersTotal (dropped earlier
	// for always reading "0/0" -- meaningless for one Uploader.Run()
	// call's flat file queue). It tracks the OUTER loop instead: which
	// folder, out of how many, this trigger (baseline sweep / backlog
	// drain / heartbeat sweep) is currently walking through -- exactly
	// what the CLI's own `[3/25] <folder>` header shows. Both zero
	// between cycles and for a one-off live-file trigger, which isn't
	// part of any "N of M" sweep.
	folderIndex, folderTotal int
	inFlight                 map[string]uploader.InFlightFile
	recent                   []uploader.ProgressEvent

	// runStart/runBytesDone/runBytesTotal back a CLI-parity "Transferred:
	// X / Y  Z%  rate/s  ETA T" line. Bytes are the one figure the CLI's
	// own dashboard leans on that the ledger's run_progress table doesn't
	// carry at all, so this tracks them the same way cmd/gpsync's own
	// dashboard struct does: runBytesDone only counts REAL completions
	// (accumulated in onProgress below), and runBytesTotal is a one-time
	// SumPendingSizeUnder query taken when a cycle begins (see beginRun,
	// called from run()'s cycle closure) -- the same scope
	// RunFolderCycle's own upload phase uses (engine.UploadScopeFor), so
	// the two numbers describe the same work. Reset once per triggering
	// cycle (baseline folder, live event, heartbeat backlog folder)
	// rather than once per internal uploader.Run() pass, so a two-pass
	// photos-then-videos strategy shows one continuous progress line
	// across both passes instead of snapping back to 0% when the second
	// pass starts.
	runStart      time.Time
	runBytesDone  int64
	runBytesTotal int64
	// runPausedTotal/runPauseStart exclude throttle backoff from the
	// cycle's transfer time, as cmd/gpsync's dashboard does with
	// pausedTotal/pauseStart. Without it the rate averaged over hour-long
	// pauses and the ETA ran to hundreds of hours.
	runPausedTotal time.Duration
	runPauseStart  time.Time
	// filesDone/filesTotal back the merged "Scanning: <folder>" /
	// "Uploading N/M files" tile. Once scan and upload run concurrently
	// (engine.RunFolderCycle), run_progress can no longer be trusted for
	// live display (see RunFolderCycle's own doc comment on why it stops
	// writing to that table during a concurrent cycle) -- these are this
	// struct's own live equivalent, same pattern as runBytesDone/Total
	// above. filesDone accumulates real completions (onProgress below,
	// success or failure both); filesTotal is refreshed LIVE via
	// onTotals (setPendingTotals below), not just once at cycle start,
	// since the concurrent scan can keep discovering more throughout the
	// cycle -- unlike runBytesTotal's older one-time
	// SumPendingSizeUnder snapshot above, which beginRun still seeds for
	// an immediate non-zero value before the first onTotals callback
	// arrives.
	//
	// filesDone deliberately counts FAILURES too, even though the
	// displayed "N of M" number doesn't (that's filesSucceeded below):
	// it's what keeps filesTotal STABLE. setPendingTotals computes
	// filesTotal as filesDone + whatever is still pending, and a
	// permanently-failed file is in neither the pending queue nor the
	// success count -- so counting only successes here would silently
	// shrink the total (70 -> 69 -> 68) every time a file failed,
	// instead of holding at "49 of 70".
	filesDone, filesTotal int
	// filesSucceeded counts ONLY real successes, and is what FileProgress
	// reports alongside whatever is in flight right now: a file being sent
	// counts toward the visible number, a failed one does not, and a
	// throttle pause leaves the figure where it stands.
	filesSucceeded int
	// scanning is true for exactly the span of engine.RunFolderCycle's
	// background scan goroutine (see its onScanActive callback) -- lets
	// a live view show "Scanning: <folder>" specifically when nothing is
	// uploading yet, distinct from genuine idle.
	scanning bool
}

// New constructs a WatchController for db/cfg, not yet started -- call
// Start to actually begin the baseline+watch loop.
func New(db *statedb.DB, cfg config.Config) *WatchController {
	return &WatchController{db: db, cfg: cfg}
}

func (wc *WatchController) IsRunning() bool {
	wc.mu.Lock()
	defer wc.mu.Unlock()
	return wc.running
}

// Config returns the config currently in effect. run() snapshots this once
// at the top of every Start(), so a SetConfig call while running only takes
// effect on the next Start (a settings-save handler calls Stop() first if
// it wants the change applied immediately, same "full restart is fine"
// precedent as Pause/Resume).
func (wc *WatchController) Config() config.Config {
	wc.mu.Lock()
	defer wc.mu.Unlock()
	return wc.cfg
}

// SetConfig updates the config used by future Start() calls. If
// SetSourceFoldersOverride has pinned this controller to a specific
// folder set, cfg.SourceFolders is silently forced back to that override
// regardless of what cfg itself carries -- see that method's own doc
// comment for why.
func (wc *WatchController) SetConfig(cfg config.Config) {
	wc.mu.Lock()
	defer wc.mu.Unlock()
	if wc.sourceFoldersOverride != nil {
		cfg.SourceFolders = wc.sourceFoldersOverride
	}
	wc.cfg = cfg
}

// SetSourceFoldersOverride pins this controller's watched folders to
// folders for the rest of its lifetime, immune to any later SetConfig
// call. The bug this closes: `gpsync dashboard
// <folders>` resolves its folder argument once at startup and hands it
// to New via cfg.SourceFolders, but the dashboard's own Settings page
// (shared code, also used by gpsync-tray) always calls SetConfig with a
// FRESH config.Config loaded straight from config.toml on disk -- which,
// for a dashboard invocation given folders only on the command line,
// has an EMPTY SourceFolders. Saving Settings for any unrelated reason
// (changing the theme, say) used to silently overwrite the in-memory
// override, restart the engine, and have it immediately give up with
// "no source folders configured" -- no error surfaced anywhere in the
// UI, just watching quietly stopping. A nil override (gpsync-tray's own
// case, and the zero value) leaves SetConfig's behavior completely
// unchanged.
func (wc *WatchController) SetSourceFoldersOverride(folders []string) {
	wc.mu.Lock()
	defer wc.mu.Unlock()
	wc.sourceFoldersOverride = folders
	wc.cfg.SourceFolders = folders
}

// RescanNow injects one extra backlog re-check into the running watch
// loop, on demand, without waiting for the periodic heartbeat -- a no-op
// if the engine isn't currently running. Non-blocking: a rescan already
// pending (the buffered channel is full) just gets this click folded into
// it rather than queuing a second one.
func (wc *WatchController) RescanNow() {
	wc.mu.Lock()
	ch, running := wc.manualRescan, wc.running
	wc.mu.Unlock()
	if !running || ch == nil {
		return
	}
	select {
	case ch <- struct{}{}:
	default:
	}
}

// RequestRetryNow asks the running upload pipeline to expire whatever
// circuit-breaker countdown is currently in progress, right now, instead
// of waiting out the rest of it -- a no-op if the engine isn't running.

func (wc *WatchController) RequestRetryNow() {
	wc.mu.Lock()
	s, running := wc.skip, wc.running
	wc.mu.Unlock()
	if !running || s == nil {
		return
	}
	s.Request()
}

// SyncFolderNow queues one folder for a scan-and-upload cycle.
//
// Mirrors RescanNow, with two deliberate differences. It carries a folder,
// fanning into the same channel the filesystem watcher feeds, so the
// request is handled exactly like a live change and stays serialised on
// the watch goroutine -- two concurrent RunFolderCycle calls would each
// get their own circuit-breaker state and neither would know the other had
// just been throttled.
//
// And it reports rather than silently doing nothing when the engine is
// stopped. RescanNow can no-op there because it is a nudge to work already
// scheduled; this is a request a person just made, and "paused watching,
// want this one folder synced" is the common case for asking.
//
// Queued, not immediate: if a baseline sweep or another folder's cycle is
// already running, this waits its turn.
func (wc *WatchController) SyncFolderNow(folder string) error {
	wc.mu.Lock()
	ch, running := wc.adhoc, wc.running
	wc.mu.Unlock()
	if !running || ch == nil {
		return errors.New("watching is paused -- resume it first, then sync a folder")
	}
	select {
	case ch <- folder:
		return nil
	default:
		return errors.New("a sync is already queued; it will run shortly")
	}
}

// CancelFile interrupts ONE specific file's active byte-upload transfer,
// right now, regardless of throttle state -- distinct from
// RequestRetryNow, which only affects a circuit-breaker pause already in
// progress. Returns false if the engine isn't running or the path isn't
// currently uploading (already finished, or never started).
func (wc *WatchController) CancelFile(path string) bool {
	wc.mu.Lock()
	fc, running := wc.fileCancels, wc.running
	wc.mu.Unlock()
	if !running || fc == nil {
		return false
	}
	return fc.Cancel(path)
}

// CurrentBackoff reports the circuit breaker's live pause state, or nil
// when dispatch isn't currently paused.
func (wc *WatchController) CurrentBackoff() *uploader.BackoffStatus {
	wc.liveMu.Lock()
	defer wc.liveMu.Unlock()
	return wc.backoff
}

func (wc *WatchController) setBackoff(s uploader.BackoffStatus) {
	wc.liveMu.Lock()
	defer wc.liveMu.Unlock()
	if s.Active {
		cp := s
		wc.backoff = &cp
		if wc.runPauseStart.IsZero() {
			wc.runPauseStart = time.Now()
		}
	} else {
		wc.backoff = nil
		if !wc.runPauseStart.IsZero() {
			wc.runPausedTotal += time.Since(wc.runPauseStart)
			wc.runPauseStart = time.Time{}
		}
	}
}

// CurrentFolder reports the folder the active cycle is scanning/uploading,
// or "" between cycles.
func (wc *WatchController) CurrentFolder() string {
	wc.liveMu.Lock()
	defer wc.liveMu.Unlock()
	return wc.currentFolder
}

func (wc *WatchController) setCurrentFolder(folder string) {
	wc.liveMu.Lock()
	defer wc.liveMu.Unlock()
	wc.currentFolder = folder
}

// FolderProgress reports "this is folder N of M" for whatever sweep is
// currently walking multiple folders (baseline, backlog drain, heartbeat),
// or (0, 0) between cycles / for a one-off live-file trigger.
func (wc *WatchController) FolderProgress() (index, total int) {
	wc.liveMu.Lock()
	defer wc.liveMu.Unlock()
	return wc.folderIndex, wc.folderTotal
}

func (wc *WatchController) setFolderProgress(index, total int) {
	wc.liveMu.Lock()
	defer wc.liveMu.Unlock()
	wc.folderIndex, wc.folderTotal = index, total
}

// InFlightFiles reports every file currently mid-upload, keyed by path --
// there can be more than one when cfg.Concurrency > 1. The caller sorts for
// a stable render order.
func (wc *WatchController) InFlightFiles() map[string]uploader.InFlightFile {
	wc.liveMu.Lock()
	defer wc.liveMu.Unlock()
	out := make(map[string]uploader.InFlightFile, len(wc.inFlight))
	for k, v := range wc.inFlight {
		out[k] = v
	}
	return out
}

// onBytes is the uploader's per-chunk progress callback: it only ever
// records the latest snapshot for a path, never accumulates -- the
// uploader itself fires this many times per second per file.
func (wc *WatchController) onBytes(e uploader.ByteProgressEvent) {
	wc.liveMu.Lock()
	defer wc.liveMu.Unlock()
	if wc.inFlight == nil {
		wc.inFlight = make(map[string]uploader.InFlightFile)
	}
	wc.inFlight[e.Path] = uploader.InFlightFile{Sent: e.Sent, Total: e.Total}
}

// RecentEvents reports the last few completed files (success or failure),
// most recent first.
func (wc *WatchController) RecentEvents() []uploader.ProgressEvent {
	wc.liveMu.Lock()
	defer wc.liveMu.Unlock()
	out := make([]uploader.ProgressEvent, len(wc.recent))
	for i, e := range wc.recent {
		out[len(wc.recent)-1-i] = e
	}
	return out
}

// onProgress is the uploader's per-file completion callback: a file just
// finished (success or failure) or was deferred back into the queue by a
// throttle. Deferred files aren't a real completion -- they'll show up
// again as a normal completion once the circuit breaker lets them through
// -- so they're dropped from inFlight without being logged to recent.
func (wc *WatchController) onProgress(e uploader.ProgressEvent) {
	wc.liveMu.Lock()
	defer wc.liveMu.Unlock()
	if e.LastFile == "" {
		return
	}
	delete(wc.inFlight, e.LastFile)
	if e.LastDeferred {
		return
	}
	wc.filesDone++
	if e.LastOK {
		wc.filesSucceeded++
		wc.runBytesDone += e.LastSize
	}
	wc.recent = append(wc.recent, e)
	if len(wc.recent) > maxRecentEvents {
		wc.recent = wc.recent[len(wc.recent)-maxRecentEvents:]
	}
}

// beginRun resets the CLI-parity byte counters for a freshly-starting
// cycle. Called once per triggering cycle, before engine.RunFolderCycle,
// with bytesTotal from a SumPendingSizeUnder query over the same scope
// the upload phase is about to use.
func (wc *WatchController) beginRun(bytesTotal int64) {
	wc.liveMu.Lock()
	defer wc.liveMu.Unlock()
	wc.runStart = time.Now()
	wc.runPausedTotal = 0
	wc.runPauseStart = time.Time{}
	if wc.backoff != nil {
		// The circuit breaker is shared across cycles, so a cycle can
		// begin in the middle of a pause.
		wc.runPauseStart = wc.runStart
	}
	wc.runBytesDone = 0
	wc.runBytesTotal = bytesTotal
	wc.filesDone = 0
	wc.filesSucceeded = 0
	wc.filesTotal = 0
}

// endRun clears the cycle's counters once engine.RunFolderCycle returns, so
// the Status page stops showing a finished (or aborted) cycle as if it were
// still running -- "103 total, 1h 27m elapsed" with the clock still ticking
// after a throttle give-up -- and shows the last run's outcome instead.
func (wc *WatchController) endRun() {
	wc.liveMu.Lock()
	defer wc.liveMu.Unlock()
	wc.runStart = time.Time{}
	wc.runPausedTotal = 0
	wc.runPauseStart = time.Time{}
	wc.runBytesDone = 0
	wc.runBytesTotal = 0
	wc.filesDone = 0
	wc.filesSucceeded = 0
	wc.filesTotal = 0
}

// RunProgress reports the current cycle's byte totals and start time, for
// a dashboard to compute a live percent/rate/ETA from -- zero value
// (including a zero start time) before the first cycle has begun.
func (wc *WatchController) RunProgress() (bytesDone, bytesTotal int64, start time.Time) {
	wc.liveMu.Lock()
	defer wc.liveMu.Unlock()
	return wc.runBytesDone, wc.runBytesTotal, wc.runStart
}

// RunPausedFor reports how long the current cycle has spent in throttle
// backoff, including a pause still in progress.
func (wc *WatchController) RunPausedFor() time.Duration {
	wc.liveMu.Lock()
	defer wc.liveMu.Unlock()
	paused := wc.runPausedTotal
	if !wc.runPauseStart.IsZero() {
		paused += time.Since(wc.runPauseStart)
	}
	return paused
}

// FileProgress reports the current cycle's files for the Run line:
// uploaded (successes only), uploading (in flight right now), and total.
//
// They are separate because a combined "uploaded + in flight" number went
// DOWN whenever a throttle failed the files being sent. Read under one lock:
// a file that completes between two separate reads moves from inFlight to
// filesSucceeded, so composing the counts across two locks could miss it or
// count it twice.
func (wc *WatchController) FileProgress() (uploaded, uploading, total int) {
	wc.liveMu.Lock()
	defer wc.liveMu.Unlock()
	return wc.filesSucceeded, len(wc.inFlight), wc.filesTotal
}

// setPendingTotals is engine.RunFolderCycle's onTotals callback.
// Overwrites filesTotal/runBytesTotal with the CURRENT pending
// count/bytes (filesDone/runBytesDone already accumulated separately via
// onProgress), so the displayed total keeps growing as the concurrent
// scan discovers more instead of staying frozen at whatever was pending
// the moment the cycle began.
func (wc *WatchController) setPendingTotals(pendingFiles int, pendingBytes int64) {
	wc.liveMu.Lock()
	defer wc.liveMu.Unlock()
	wc.filesTotal = wc.filesDone + pendingFiles
	wc.runBytesTotal = wc.runBytesDone + pendingBytes
}

// Scanning reports whether engine.RunFolderCycle's background scan
// goroutine is currently active for the running cycle.
func (wc *WatchController) Scanning() bool {
	wc.liveMu.Lock()
	defer wc.liveMu.Unlock()
	return wc.scanning
}

func (wc *WatchController) setScanning(active bool) {
	wc.liveMu.Lock()
	defer wc.liveMu.Unlock()
	wc.scanning = active
}

// logAndRecordCrash logs a recovered panic (with stack trace) and
// persists it to crash_log -- split out from Start's own deferred
// recover so the logging/recording half is testable without also
// triggering the os.Exit(1) that has to follow a real panic (an
// unavoidably untestable side effect in-process; matching
// cmd/gpsync-tray/crashrecovery.go's own recoverAndLog, which has the
// identical constraint).
func (wc *WatchController) logAndRecordCrash(context string, r any) {
	log.Printf("panic in %s: %v\n%s", context, r, debug.Stack())
	if err := wc.db.RecordCrash(context, fmt.Sprintf("%v", r)); err != nil {
		log.Printf("recording crash: %v", err)
	}
}

func (wc *WatchController) Start() {
	wc.mu.Lock()
	defer wc.mu.Unlock()
	if wc.running {
		return
	}
	wc.running = true
	stopCh := make(chan struct{})
	done := make(chan struct{})
	manualRescan := make(chan struct{}, 1)
	adhoc := make(chan string, 4)
	skip := uploader.NewSkipSignal()
	fileCancels := uploader.NewFileCancelRegistry()
	wc.stopCh = stopCh
	wc.done = done
	wc.manualRescan = manualRescan
	wc.adhoc = adhoc
	wc.skip = skip
	wc.fileCancels = fileCancels
	go func() {
		defer close(done)
		// Silent-death visibility: both of gpsync-tray's own real,
		// motivating crashes (a NewTicker(0) panic, a WaitGroup-misuse
		// panic) happened INSIDE this goroutine, not in the tray's own
		// menu/ticker goroutines that already got recover-wrapping --
		// wrapping only those left the actual watch loop (and everything
		// it calls: RunFolderCycle, the scanner, the uploader, the
		// filesystem watcher) able to kill the whole process with
		// nothing in tray.log and nothing in crash_log, the exact
		// symptom that feature exists to close. Recovering here does
		// NOT risk masking a real bug -- os.Exit(1) still ends the
		// process either way, this only adds a stack trace and a
		// permanent record before it does. wc.db is always non-nil by
		// the time Start() can be called (New requires it), so
		// RecordCrash is safe to call unconditionally.
		defer func() {
			if r := recover(); r != nil {
				wc.logAndRecordCrash("watch loop", r)
				os.Exit(1)
			}
		}()
		wc.run(stopCh, manualRescan, adhoc, skip, fileCancels)
	}()
}

// Stop cancels the running engine and waits for it to actually exit --
// cancelling ctx makes Uploader.Run() stop dispatching NEW files
// immediately (both between circuit-breaker rungs and, via dispatchPass's
// per-item ctx check, between individual files mid-pass) while letting
// whatever's already transferring finish normally, so this returns once
// that tail is done -- no new uploads start, nothing in flight is cut off.
func (wc *WatchController) Stop() {
	wc.mu.Lock()
	if !wc.running {
		wc.mu.Unlock()
		return
	}
	wc.running = false
	close(wc.stopCh)
	// Captured before unlocking, so this waits on exactly the goroutine
	// THIS call is stopping -- see the done field's own doc comment for
	// why a shared sync.WaitGroup here was a race.
	done := wc.done
	wc.mu.Unlock()
	<-done
}

// run is the headless equivalent of cmd/gpsync's watchCmd: baseline pass,
// then the same engine.RunWatchLoop prioritization every `gpsync watch`
// invocation uses, wired to engine.RunFolderCycle (no terminal output)
// instead of the CLI's colored dashboard.
func (wc *WatchController) run(stopCh chan struct{}, manualRescan <-chan struct{}, adhoc <-chan string, skip *uploader.SkipSignal, fileCancels *uploader.FileCancelRegistry) {
	// Whatever exit path this goroutine takes -- Stop() was called, or
	// there was nothing to watch at all -- wc.running must end up false
	// so IsRunning()/a live status view stay accurate. Stop() also sets
	// this itself, immediately, so IsRunning() doesn't have to wait for
	// the goroutine to actually unwind on that path; this defer covers
	// the "exited on its own" paths below (no source folders configured,
	// no valid watch roots, the filesystem watcher failed to start).
	defer func() {
		wc.mu.Lock()
		wc.running = false
		wc.mu.Unlock()
	}()

	// ctx is what actually makes Stop() responsive: cancelling it
	// interrupts a circuit-breaker pause mid-wait instead of letting
	// RunFolderCycle block for the rest of whatever rung is active.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		select {
		case <-stopCh:
			cancel()
		case <-ctx.Done():
		}
	}()

	// Snapshotted once: a settings save mid-run (SetConfig) only takes
	// effect on the next Start, not retroactively into this goroutine.
	cfg := wc.Config()

	patterns := cfg.SourceFolders
	if len(patterns) == 0 {
		log.Println("no source folders configured")
		return
	}

	conc := quota.NewAdaptiveConcurrency(cfg.Concurrency)
	breaker := uploader.NewCircuitBreakerState()

	// cycleAt runs one folder's cycle. index/total are 1-based "this is
	// folder N of M" for whatever sweep is calling it (baseline, backlog
	// drain, heartbeat) -- 0,0 for a one-off live-file trigger, which
	// isn't part of any such sweep.
	cycleAt := func(folder string, index, total int) {
		wc.setCurrentFolder(folder)
		defer wc.setCurrentFolder("")
		wc.setFolderProgress(index, total)
		defer wc.setFolderProgress(0, 0)
		// Best-effort: a query failure here just means the byte totals
		// stay at whatever beginRun last set (or zero) -- never blocks
		// the actual upload phase below over a stat only a live view
		// needs.
		if _, bytesTotal, err := wc.db.SumPendingSizeUnder(engine.UploadScopeFor(cfg, folder)); err == nil {
			wc.beginRun(bytesTotal)
		}
		if _, err := engine.RunFolderCycle(ctx, wc.db, cfg, folder, conc, breaker, skip, fileCancels, wc.setBackoff, wc.onBytes, wc.onProgress, wc.setScanning, wc.setPendingTotals); err != nil && !errors.Is(err, uploader.ErrRunAborted) {
			log.Printf("cycle for %s: %v", folder, err)
		}
		wc.endRun()
	}
	// cycle is the plain, not-part-of-a-sweep form -- a single live-file
	// trigger.
	cycle := func(folder string) { cycleAt(folder, 0, 0) }

	// ── baseline ──
	units, err := engine.DiscoverSyncUnits(patterns)
	if err != nil {
		log.Printf("baseline: discovering folders: %v", err)
	}
	for i, folder := range units {
		select {
		case <-stopCh:
			return
		default:
		}
		cycleAt(folder, i+1, len(units))
	}
	resolveMissingFiles(wc.db, cfg, false)

	// ── watch ──
	roots, err := engine.ResolveWatchRoots(patterns)
	if err != nil {
		log.Printf("resolving watch roots: %v", err)
		return
	}
	if len(roots) == 0 {
		log.Println("none of the configured source folders exist")
		return
	}
	debounce := time.Duration(cfg.WatchDebounceSeconds) * time.Second
	w, err := watcher.New(roots, debounce)
	if err != nil {
		log.Printf("starting the filesystem watcher: %v", err)
		return
	}
	defer w.Close()

	backlog, err := engine.DiscoverSyncUnits(patterns)
	if err != nil {
		log.Printf("backlog: discovering folders: %v", err)
	}
	// Captured before RunWatchLoop consumes backlog -- onBacklogItem
	// below only ever gets told how many are left AFTER this one
	// (remaining), so the original count is what turns that into
	// "folder N of M".
	backlogTotal := len(backlog)

	hb := time.NewTicker(time.Duration(cfg.WatchHeartbeatMinutes) * time.Minute)
	defer hb.Stop()

	// Rescan Now needs no change to RunWatchLoop itself: it already
	// fires onHeartbeat (a full backlog re-check) on every tick of
	// whatever channel it's handed as `heartbeat` -- so a manual trigger
	// just fans into the SAME channel alongside the real ticker, one
	// extra tick on demand instead of a second code path.
	heartbeatCh := make(chan time.Time)
	go func() {
		for {
			select {
			case t := <-hb.C:
				select {
				case heartbeatCh <- t:
				case <-ctx.Done():
					return
				}
			case <-manualRescan:
				select {
				case heartbeatCh <- time.Now():
				case <-ctx.Done():
					return
				}
			case <-ctx.Done():
				return
			}
		}
	}()

	// An ad-hoc request is a live-change event as far as the loop is
	// concerned, so it fans into the SAME `ready` channel the filesystem
	// watcher feeds -- exactly how Rescan Now fans into heartbeatCh above.
	// Work therefore stays serialised on this one goroutine: no second
	// RunFolderCycle can race the first, which is the guarantee the
	// circuit breaker depends on.
	readyCh := make(chan string)
	go func() {
		for {
			select {
			case f := <-w.Ready():
				select {
				case readyCh <- f:
				case <-ctx.Done():
					return
				}
			case f := <-adhoc:
				select {
				case readyCh <- f:
				case <-ctx.Done():
					return
				}
			case <-ctx.Done():
				return
			}
		}
	}()

	engine.RunWatchLoop(
		func(folder string) { cycle(folder) },
		func(folder string, remaining int) { cycleAt(folder, backlogTotal-remaining, backlogTotal) },
		func() {
			pending, err := engine.HeartbeatFolders(wc.db, patterns)
			if err != nil {
				log.Printf("heartbeat: listing pending folders: %v", err)
				return
			}
			for i, f := range pending {
				cycleAt(f.Path, i+1, len(pending))
			}
			resolveMissingFiles(wc.db, cfg, true)
		},
		func(werr error) { log.Printf("watch error: %v", werr) },
		backlog, readyCh, heartbeatCh, w.Errors(), stopCh,
	)
}

// resolveMissingFiles decides files scans have flagged as missing (see
// engine.ResolveMissingFiles). onlyIfUndecided skips the source-folder walk
// when nothing new was flagged -- the heartbeat must not walk the whole
// library every few minutes just because confirmed entries exist.
func resolveMissingFiles(db *statedb.DB, cfg config.Config, onlyIfUndecided bool) {
	res, ran, err := engine.ResolveMissingIfNeeded(db, cfg.SourceFolders, onlyIfUndecided, time.Now())
	switch {
	case err != nil:
		log.Printf("missing files: %v", err)
	case !ran:
	case res.Skipped:
		log.Printf("missing files not checked: %s", res.SkipReason)
	case res.Changed():
		log.Printf("missing files: %d confirmed missing, %d moved, %d back in place", res.Confirmed, res.Relocated, res.Reappeared)
	}
}
