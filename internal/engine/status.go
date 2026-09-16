package engine

import (
	"github.com/gmizrahi/gpsync/internal/quota"
	"github.com/gmizrahi/gpsync/internal/statedb"
)

// Status is the read-only "what's happening right now" view shared by
// `gpsync info` and gpsync-tray's dashboard -- built from the exact same two
// statedb queries `gpsync info` itself already uses (RunGet, LedgerSummary),
// so a polled dashboard reports exactly what the CLI would tell you.
type Status struct {
	Watching bool

	RunActive     bool
	RunType       string
	FilesDone     int
	FilesTotal    int
	FoldersDone   int
	FoldersTotal  int
	LastEndReason string
	LastEndDetail string
	LastUpdatedAt float64 // unix seconds, 0 if there's no run on record yet

	Synced          int
	Pending         int
	FailedPermanent int
	FailedRetryable int
	TotalBytes      int64
	// SyncedBytes/PendingBytes split TotalBytes by status -- a real
	// request: "total size in library (synced, pending, total)".
	SyncedBytes  int64
	PendingBytes int64
	// RetryableBytes is failed_retryable work: still to upload, requeued
	// by the next cycle.
	RetryableBytes int64

	// QuotaUsed/QuotaDailyLimit are Google's documented 10k/project/day
	// Library API ceiling -- the same figures `gpsync info`'s own "Quota used
	// today" line already shows (cmd/gpsync/main.go), added here so the
	// dashboard can show it too without duplicating the read logic.
	QuotaUsed       int
	QuotaDailyLimit int

	// UploadedToday/UploadedTodayBytes: how many files (and how many bytes)
	// have actually been pushed to Google since the same Pacific-midnight
	// reset the quota counter above uses.
	UploadedToday      int
	UploadedTodayBytes int64
}

// BuildStatus assembles a Status snapshot. watching reports whatever the
// caller's own engine lifecycle currently is (gpsync-tray's Pause/Resume
// state) -- BuildStatus itself has no opinion on that, it only reads the
// ledger.
func BuildStatus(db *statedb.DB, watching bool) (Status, error) {
	s := Status{Watching: watching}

	run, err := db.RunGet()
	if err != nil {
		return s, err
	}
	if run != nil {
		s.RunActive = run.Status == "running"
		s.RunType = run.RunType
		s.FilesDone, s.FilesTotal = run.FilesDone, run.FilesTotal
		s.FoldersDone, s.FoldersTotal = run.FoldersDone, run.FoldersTotal
		s.LastEndReason = run.EndReason
		s.LastEndDetail = run.EndDetail
		s.LastUpdatedAt = run.UpdatedAt
	}

	summary, err := db.LedgerSummary()
	if err != nil {
		return s, err
	}
	s.Synced = summary.UploadedByGPB + summary.MarkedSynced
	s.Pending = summary.Pending
	s.FailedPermanent = summary.FailedPermanent
	s.FailedRetryable = summary.FailedRetryable
	s.TotalBytes = summary.TotalBytesTracked
	s.SyncedBytes = summary.SyncedBytes
	s.PendingBytes = summary.PendingBytes
	s.RetryableBytes = summary.FailedRetryableBytes

	dq := quota.NewDailyQuota(db)
	s.QuotaDailyLimit = dq.DailyLimit()
	if used, err := dq.Used(); err == nil {
		s.QuotaUsed = used
	}

	if n, bytes, err := db.UploadedSince(quota.PacificMidnightEpoch()); err == nil {
		s.UploadedToday = n
		s.UploadedTodayBytes = bytes
	}

	return s, nil
}
