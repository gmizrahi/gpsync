package dashboard

import (
	"log"
	"sort"
	"time"

	"github.com/gmizrahi/gpsync/internal/config"
	"github.com/gmizrahi/gpsync/internal/engine"
	"github.com/gmizrahi/gpsync/internal/pathx"
	"github.com/gmizrahi/gpsync/internal/statedb"
)

// statusResponse is the dashboard's JSON wire shape -- a thin view over
// engine.Status (the actual data assembly, fully tested independent of
// this Windows-only package: internal/engine/status_test.go), formatting
// the one field (LastUpdatedAt) that benefits from being human-readable
// before it reaches the browser.
type statusResponse struct {
	Watching bool `json:"watching"`

	// RunActive/RunType/FoldersDone/FoldersTotal are no longer read by any
	// page this server renders. RunActive USED to gate the Transfer row's
	// bytes display, but that broke once RunFolderCycle's loop started
	// calling Uploader.Run() repeatedly per cycle (a real follow-up
	// report: "i lack the ETA and speed on the file uploads") --
	// run_active tracks ONE Run() call via run_progress, and genuinely
	// goes false in the real gaps between consecutive calls even though
	// the cycle is still active; see the Transfer row's own comment for
	// what replaced it. Kept here only because engine.Status already
	// carries them for `gpsync info` parity and dropping them here would gain
	// nothing.
	RunActive     bool   `json:"run_active"`
	RunType       string `json:"run_type,omitempty"`
	FoldersDone   int    `json:"folders_done,omitempty"`
	FoldersTotal  int    `json:"folders_total,omitempty"`
	LastEndReason string `json:"last_end_reason,omitempty"`
	LastEndDetail string `json:"last_end_detail,omitempty"`
	LastUpdatedAt string `json:"last_updated_at,omitempty"`

	// FilesUploaded/FilesUploading/FilesTotal/Scanning back the Run line /
	// "Scanning: <folder>" tile -- sourced from watchController's own live
	// tracking (wc.FileProgress/wc.Scanning), NOT engine.Status's
	// run_progress-derived figures, for the same reason FolderIndex/
	// FolderTotal/CurrentFolder below already are: a flat, possibly
	// multi-folder upload queue has no folder identity of its own, and
	// during a concurrent scan+upload cycle run_progress deliberately
	// stops reflecting the scan phase at all (see RunFolderCycle's own
	// doc comment on trackRun).
	// FilesUploaded/FilesUploading are shown separately: their sum went
	// down whenever a throttle failed the files in flight.
	FilesUploaded  int  `json:"files_uploaded,omitempty"`
	FilesUploading int  `json:"files_uploading,omitempty"`
	FilesTotal     int  `json:"files_total,omitempty"`
	Scanning       bool `json:"scanning,omitempty"`

	Synced          int   `json:"synced"`
	Pending         int   `json:"pending"`
	FailedPermanent int   `json:"failed_permanent"`
	FailedRetryable int   `json:"failed_retryable"`
	TotalBytes      int64 `json:"total_bytes"`
	// SyncedBytes/PendingBytes split TotalBytes by status -- a real
	// request: "total size in library (synced, pending, total)".
	SyncedBytes  int64 `json:"synced_bytes"`
	PendingBytes int64 `json:"pending_bytes"`
	// RetryableBytes is failed_retryable work. Total Size counts it as
	// still to upload; leaving it out showed "Pending 0 B" and a short total
	// after a throttle give-up turned every remaining file retryable.
	RetryableBytes int64 `json:"retryable_bytes,omitempty"`
	// MissingConfirmed counts ledger entries whose file is confirmed gone
	// from disk (engine.ResolveMissingFiles). Shown only when non-zero.
	MissingConfirmed int `json:"missing_confirmed,omitempty"`

	// BytesDone/BytesTotal/RunElapsedSecs back a CLI-parity "Transferred:
	// X / Y  Z%  rate/s  ETA T" line. The ledger's run_progress row carries
	// file counts only, and its FoldersDone/FoldersTotal always read "0/0"
	// (RunFolderCycle's
	// upload phase never populates them -- a flat queue that can span
	// many folders under a global sync strategy has no notion of "which
	// folder is done"). Bytes are the one figure the CLI's own dashboard
	// leans on that the ledger doesn't carry, so they're sourced from
	// watchController's own live tracking instead (see its
	// runBytesDone/runBytesTotal doc comment) -- BytesDone already
	// includes in-flight files' partial progress, matching cmd/gpsync
	// dashboard's own `doneOrInFlight`. Percent/rate/ETA are computed
	// client-side from these three raw numbers, the same formula
	// cmd/gpsync/dashboard.go's transferFigures uses.
	//
	// BytesDone deliberately has NO omitempty. A legitimate 0 -- a cycle
	// that just started, nothing sent yet, while BytesTotal is already
	// nonzero -- is a real state, and with omitempty Go's JSON encoder
	// drops a zero int64 field entirely: the browser then reads
	// `bytes_done` as `undefined`, and `100 * undefined / bytes_total` is
	// `NaN`, rendering "NaN% NaN B/s" while nothing has transferred yet.
	BytesDone      int64   `json:"bytes_done"`
	BytesTotal     int64   `json:"bytes_total,omitempty"`
	RunElapsedSecs float64 `json:"run_elapsed_secs,omitempty"`
	// TransferSecs is RunElapsedSecs minus time spent in throttle backoff:
	// what speed and ETA are computed over, matching the CLI.
	TransferSecs float64 `json:"transfer_secs,omitempty"`

	// QuotaUsed/QuotaDailyLimit: the same "quota used today" figure
	// `gpsync info` already prints, surfaced on the dashboard too.
	QuotaUsed       int `json:"quota_used"`
	QuotaDailyLimit int `json:"quota_daily_limit"`

	// UploadedToday/UploadedTodayBytes count the files and bytes that
	// landed today. They reuse the same Pacific-midnight reset the quota
	// counter above uses, so "today" means the same thing in both places.
	UploadedToday      int   `json:"uploaded_today"`
	UploadedTodayBytes int64 `json:"uploaded_today_bytes"`

	// LastUploadAt is the Unix timestamp of the most recent real upload,
	// rendered beside the Last message strip it shares a tile with as
	// "Last successful upload: <Month> <day>, <year> <hh:mm:ss> ;
	// <Hh Mm Ss> ago". It answers the one question the rest of the page
	// can't while throttled: everything else on screen shows what gpsync
	// is TRYING to do, and during a long backoff (rungs go up to an hour)
	// every one of those figures sits frozen without saying whether the
	// last thing that actually worked was ten minutes or three days ago.
	//
	// Sent as a raw epoch, not a preformatted string like LastCrashAt
	// above, because the browser needs it for arithmetic too (the "ago"
	// counter re-renders every poll). Zero/omitted means nothing has ever
	// been uploaded, which the client renders as a dash.
	LastUploadAt float64 `json:"last_upload_at,omitempty"`

	// Backoff carries the live circuit-breaker pause state, if any --
	// nothing in run_progress/the ledger changes while dispatch is paused,
	// so without this the dashboard just freezes with no explanation for up
	// to an hour (the schedule's longest rung). See watchController's
	// backoff field.
	Paused             bool    `json:"paused"`
	PauseReason        string  `json:"pause_reason,omitempty"`
	PauseRung          int     `json:"pause_rung,omitempty"`
	PauseTotalRungs    int     `json:"pause_total_rungs,omitempty"`
	PauseRemainingSecs float64 `json:"pause_remaining_secs,omitempty"`
	// PauseRungWaitSecs/PauseConcurrency/PauseMaxConcurrency fill out the
	// throttle line to CLI parity (cmd/gpsync/dashboard.go's backoffLines:
	// "rung 2/7 (30s) — retrying in 16s — concurrency will resume at
	// 1/6") -- BackoffStatus already carries all three, this just wasn't
	// forwarding them.
	PauseRungWaitSecs   float64 `json:"pause_rung_wait_secs,omitempty"`
	PauseConcurrency    int     `json:"pause_concurrency,omitempty"`
	PauseMaxConcurrency int     `json:"pause_max_concurrency,omitempty"`

	// CurrentFolder/InFlight/Recent carry the "which folder, which file,
	// what %" detail. run_progress holds counts, not identity, so none of
	// this exists in the ledger: it comes from the watch controller's live,
	// process-local state (see its liveMu fields).
	CurrentFolder string             `json:"current_folder,omitempty"`
	InFlight      []inFlightFileJSON `json:"in_flight,omitempty"`
	Recent        []recentEventJSON  `json:"recent,omitempty"`

	// FolderIndex/FolderTotal back a CLI-parity "[N/M] <folder>" counter,
	// matching what the CLI prints. Only meaningful (nonzero) alongside
	// CurrentFolder, so rendered as part of the same row.
	FolderIndex int `json:"folder_index,omitempty"`
	FolderTotal int `json:"folder_total,omitempty"`

	// AutostartInstalled reflects the REAL Windows registry state (see
	// autostartInstalled in autostart.go), read fresh every poll -- never
	// a config.toml preference, since that could drift from what's
	// actually registered (e.g. the user removing it via Windows' own
	// Settings > Startup Apps page).
	AutostartInstalled bool `json:"autostart_installed"`

	// LastCrashAt/LastCrashContext/LastCrashMessage back the "Last crash"
	// row -- silent-death visibility (see crashrecovery.go). Empty when
	// crash_log has never recorded anything, the common case.
	LastCrashAt      string `json:"last_crash_at,omitempty"`
	LastCrashContext string `json:"last_crash_context,omitempty"`
	LastCrashMessage string `json:"last_crash_message,omitempty"`
}

type inFlightFileJSON struct {
	Path    string `json:"path"`
	Folder  string `json:"folder"`
	Name    string `json:"name"`
	Sent    int64  `json:"sent"`
	Total   int64  `json:"total"`
	Percent int    `json:"percent"`
}

type recentEventJSON struct {
	Name string `json:"name"`
	OK   bool   `json:"ok"`
	// Cancelled marks a manual per-file skip (uploader.ProgressEvent's
	// LastCancelled) -- OK is still false (it did land failed_retryable),
	// but a deliberate skip is not a failure, and Recent activity must not
	// label it FAIL.
	Cancelled bool   `json:"cancelled,omitempty"`
	Error     string `json:"error,omitempty"`
	Size      int64  `json:"size"`
	// ErrorKind is uploader.ProgressEvent.LastErrorKind (retryx.Kind, as a
	// plain string) -- the JS badge in Recent Activity uses it to show
	// THROTTLED/QUOTA/FAILED/RETRY instead of a single generic FAIL. Empty
	// when OK is true.
	ErrorKind string `json:"error_kind,omitempty"`
}

func buildStatus(db *statedb.DB, wc Controller) (statusResponse, error) {
	s, err := engine.BuildStatus(db, wc.IsRunning())
	if err != nil {
		return statusResponse{}, err
	}
	filesUploaded, filesUploading, filesTotal := wc.FileProgress()
	resp := statusResponse{
		Watching:           s.Watching,
		RunActive:          s.RunActive,
		RunType:            s.RunType,
		FilesUploaded:      filesUploaded,
		FilesUploading:     filesUploading,
		FilesTotal:         filesTotal,
		Scanning:           wc.Scanning(),
		FoldersDone:        s.FoldersDone,
		FoldersTotal:       s.FoldersTotal,
		LastEndReason:      s.LastEndReason,
		LastEndDetail:      s.LastEndDetail,
		Synced:             s.Synced,
		Pending:            s.Pending,
		FailedPermanent:    s.FailedPermanent,
		FailedRetryable:    s.FailedRetryable,
		TotalBytes:         s.TotalBytes,
		SyncedBytes:        s.SyncedBytes,
		PendingBytes:       s.PendingBytes,
		RetryableBytes:     s.RetryableBytes,
		QuotaUsed:          s.QuotaUsed,
		QuotaDailyLimit:    s.QuotaDailyLimit,
		UploadedToday:      s.UploadedToday,
		UploadedTodayBytes: s.UploadedTodayBytes,
	}
	if autostartCtrl != nil {
		if installed, err := autostartCtrl.Installed(); err == nil {
			resp.AutostartInstalled = installed
		} else {
			log.Printf("checking autostart registry state: %v", err)
		}
	}
	if mc, err := db.MissingCounts(); err == nil {
		resp.MissingConfirmed = mc.Confirmed
	} else {
		log.Printf("counting missing files: %v", err)
	}
	if at, ok, err := db.LastUploadAt(); err == nil && ok {
		resp.LastUploadAt = at
	} else if err != nil {
		log.Printf("reading last upload time: %v", err)
	}
	if crash, ok, err := db.LastCrash(); err == nil && ok {
		resp.LastCrashAt = time.Unix(int64(crash.At), 0).Format("2006-01-02 15:04:05")
		resp.LastCrashContext = crash.Context
		resp.LastCrashMessage = crash.Message
	} else if err != nil {
		log.Printf("checking last crash: %v", err)
	}
	// CurrentFolder reflects whichever folder's watch-triggered cycle is
	// currently running -- accurate for the default folder-by-folder
	// strategy (the upload phase never leaves that one folder), but
	// actively misleading for a global strategy (Smallest/PhotosFirst):
	// the cycle that scans folder A can spend a long time uploading files
	// from many OTHER folders too, while this stayed frozen on "A" the
	// whole time, leaving the row frozen on the first folder in the list.
	// Each in-flight file
	// already carries its own real folder (see below), which is never
	// ambiguous regardless of strategy, so this summary row is simply
	// omitted for the two global strategies rather than shown wrong.
	strategy := wc.Config().SyncStrategy
	if strategy != config.SyncStrategySmallestFirst && strategy != config.SyncStrategyPhotosFirst {
		resp.CurrentFolder = wc.CurrentFolder()
		resp.FolderIndex, resp.FolderTotal = wc.FolderProgress()
	}
	if s.LastUpdatedAt > 0 {
		resp.LastUpdatedAt = time.Unix(int64(s.LastUpdatedAt), 0).Format("2006-01-02 15:04:05")
	}
	if backoff := wc.CurrentBackoff(); backoff != nil {
		resp.Paused = true
		resp.PauseReason = backoff.Reason
		resp.PauseRung = backoff.Rung
		resp.PauseTotalRungs = backoff.TotalRungs
		resp.PauseRemainingSecs = backoff.RemainingWait.Seconds()
		resp.PauseRungWaitSecs = backoff.RungWait.Seconds()
		resp.PauseConcurrency = backoff.Concurrency
		resp.PauseMaxConcurrency = backoff.MaxConcurrency
	}

	inFlight := wc.InFlightFiles()
	paths := make([]string, 0, len(inFlight))
	var inFlightSent int64
	for p, f := range inFlight {
		paths = append(paths, p)
		inFlightSent += f.Sent
	}
	sort.Strings(paths)
	for _, p := range paths {
		f := inFlight[p]
		pct := 0
		if f.Total > 0 {
			pct = int(f.Sent * 100 / f.Total)
		}
		resp.InFlight = append(resp.InFlight, inFlightFileJSON{
			Path: p, Folder: pathx.DisplayDir(p), Name: pathx.DisplayName(p), Sent: f.Sent, Total: f.Total, Percent: pct,
		})
	}

	bytesDone, bytesTotal, runStart := wc.RunProgress()
	resp.BytesDone = bytesDone + inFlightSent
	resp.BytesTotal = bytesTotal
	if !runStart.IsZero() {
		elapsed := time.Since(runStart)
		resp.RunElapsedSecs = elapsed.Seconds()
		transfer := elapsed - wc.RunPausedFor()
		if transfer < time.Millisecond {
			transfer = time.Millisecond
		}
		resp.TransferSecs = transfer.Seconds()
	}

	for _, e := range wc.RecentEvents() {
		resp.Recent = append(resp.Recent, recentEventJSON{
			Name: pathx.DisplayName(e.LastFile), OK: e.LastOK, Cancelled: e.LastCancelled, Error: e.LastErrorMessage, Size: e.LastSize,
			ErrorKind: e.LastErrorKind,
		})
	}

	return resp, nil
}
