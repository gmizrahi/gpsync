package engine

import (
	"time"

	"github.com/gmizrahi/gpsync/internal/statedb"
)

// forecastWindow is how far back BuildForecast measures throughput. A
// rolling 7x24h window ending at now, deliberately not "the last 7
// calendar days": today is always partial, so including it as a whole day
// would drag the average down every morning and drift it back up by
// evening, making the projected date wander for no real reason.
const forecastWindow = 7 * 24 * time.Hour

// Forecast answers "how far along is this, and when does it finish" from
// MEASURED throughput rather than a theoretical rate -- which matters
// here because real throughput is dominated by throttle backoff (see
// throttleCircuitBreakerSchedule in internal/uploader), not by bandwidth.
// A projection derived from anything other than observed history would be
// systematically optimistic.
type Forecast struct {
	SyncedFiles    int
	SyncedBytes    int64
	RemainingFiles int
	RemainingBytes int64
	PercentFiles   float64
	PercentBytes   float64

	// NeedsReview is remaining work that no amount of waiting will
	// finish -- it's blocked on a human decision on the Originals tab.
	// Reported separately, and deliberately EXCLUDED from RemainingBytes
	// and the projection: folding it in would make the finish date recede
	// forever while the queue itself was actually empty.
	NeedsReviewFiles int
	NeedsReviewBytes int64

	// Measured over the trailing forecastWindow.
	WindowFiles int
	WindowBytes int64
	FilesPerDay float64
	BytesPerDay float64

	// Done is true when nothing is left to upload. Known is true when
	// there's enough recent throughput to project from -- both false
	// means "still work to do, but no idea how long", which is a real
	// state (a fresh install, or a long pause) and must not be rendered
	// as an ETA of zero.
	Done          bool
	Known         bool
	DaysRemaining float64
	ETA           time.Time
}

// BuildForecast reads the ledger and the recent upload history. `now` is a
// parameter rather than time.Now() so the projection is testable.
func BuildForecast(db *statedb.DB, now time.Time) (Forecast, error) {
	sum, err := db.LedgerSummary()
	if err != nil {
		return Forecast{}, err
	}
	uploads, err := db.SuccessfulUploadTimestamps()
	if err != nil {
		return Forecast{}, err
	}
	return buildForecast(sum, uploads, now), nil
}

// buildForecast is the pure half, split out for the same reason
// bucketUploads is: the ledger can't backdate an uploaded_at, so a
// DB-driven test could only ever exercise "everything happened just now".
func buildForecast(sum statedb.LedgerSummary, uploads []statedb.UploadedAtSize, now time.Time) Forecast {
	f := Forecast{
		SyncedFiles: sum.UploadedByGPB + sum.MarkedSynced,
		SyncedBytes: sum.SyncedBytes,
		// failed_retryable counts as remaining, not as failed: every
		// Uploader.Run starts by sweeping those rows back to pending.
		RemainingFiles:   sum.Pending + sum.FailedRetryable,
		RemainingBytes:   sum.PendingBytes + sum.FailedRetryableBytes,
		NeedsReviewFiles: sum.NeedsReview,
		NeedsReviewBytes: sum.NeedsReviewBytes,
	}

	if total := f.SyncedFiles + f.RemainingFiles; total > 0 {
		f.PercentFiles = float64(f.SyncedFiles) / float64(total) * 100
	}
	if total := f.SyncedBytes + f.RemainingBytes; total > 0 {
		f.PercentBytes = float64(f.SyncedBytes) / float64(total) * 100
	}

	since := now.Add(-forecastWindow)
	for _, u := range uploads {
		t := time.Unix(int64(u.At), 0)
		if t.Before(since) || t.After(now) {
			continue
		}
		f.WindowFiles++
		f.WindowBytes += u.Size
	}
	days := forecastWindow.Hours() / 24
	f.FilesPerDay = float64(f.WindowFiles) / days
	f.BytesPerDay = float64(f.WindowBytes) / days

	if f.RemainingFiles == 0 && f.RemainingBytes == 0 {
		f.Done = true
		return f
	}
	// Project on BYTES, not file count: a queue's remaining files can be
	// mostly small images while the measured rate came from large videos,
	// or the reverse. Bytes is the quantity the throttle actually meters.
	if f.BytesPerDay > 0 && f.RemainingBytes > 0 {
		f.Known = true
		f.DaysRemaining = float64(f.RemainingBytes) / f.BytesPerDay
		f.ETA = now.Add(time.Duration(f.DaysRemaining * float64(24*time.Hour)))
	}
	return f
}
