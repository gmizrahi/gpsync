package engine

import (
	"math"
	"testing"
	"time"

	"github.com/gmizrahi/gpsync/internal/statedb"
)

// upAt builds an upload record N hours before `now`.
func upAt(now time.Time, hoursAgo float64, size int64) statedb.UploadedAtSize {
	return statedb.UploadedAtSize{
		At:   float64(now.Add(-time.Duration(hoursAgo * float64(time.Hour))).Unix()),
		Size: size,
	}
}

func TestBuildForecast_ProjectsFromMeasuredThroughput(t *testing.T) {
	now := time.Date(2026, time.September, 9, 12, 0, 0, 0, time.UTC)
	const gib = int64(1) << 30

	sum := statedb.LedgerSummary{
		UploadedByGPB: 700, SyncedBytes: 700 * gib,
		Pending: 300, PendingBytes: 300 * gib,
	}
	// 70 GiB inside the 7-day window => 10 GiB/day => 30 days for 300 GiB.
	var uploads []statedb.UploadedAtSize
	for i := 0; i < 70; i++ {
		uploads = append(uploads, upAt(now, float64(i)+1, gib))
	}

	f := buildForecast(sum, uploads, now)

	if f.RemainingBytes != 300*gib || f.RemainingFiles != 300 {
		t.Errorf("remaining = %d files/%d bytes, want 300/%d", f.RemainingFiles, f.RemainingBytes, 300*gib)
	}
	if math.Abs(f.PercentBytes-70) > 0.01 {
		t.Errorf("PercentBytes = %.2f, want 70", f.PercentBytes)
	}
	if f.WindowFiles != 70 {
		t.Errorf("WindowFiles = %d, want 70 (all inside the 7-day window)", f.WindowFiles)
	}
	if got := f.BytesPerDay / float64(gib); math.Abs(got-10) > 0.01 {
		t.Errorf("BytesPerDay = %.2f GiB, want 10", got)
	}
	if !f.Known {
		t.Fatal("Known = false, want a projection from real throughput")
	}
	if math.Abs(f.DaysRemaining-30) > 0.05 {
		t.Errorf("DaysRemaining = %.2f, want 30", f.DaysRemaining)
	}
	if want := now.AddDate(0, 0, 30); math.Abs(f.ETA.Sub(want).Hours()) > 1 {
		t.Errorf("ETA = %v, want ~%v", f.ETA, want)
	}
}

func TestBuildForecast_IgnoresUploadsOutsideTheWindow(t *testing.T) {
	now := time.Date(2026, time.September, 9, 12, 0, 0, 0, time.UTC)
	const gib = int64(1) << 30

	sum := statedb.LedgerSummary{Pending: 1, PendingBytes: 70 * gib}
	uploads := []statedb.UploadedAtSize{
		upAt(now, 1, gib),    // inside
		upAt(now, 167, gib),  // inside (just under 7 days)
		upAt(now, 169, gib),  // outside -- older than the window
		upAt(now, -5, gib),   // outside -- in the future (clock skew)
		upAt(now, 5000, gib), // outside -- ancient history
	}

	f := buildForecast(sum, uploads, now)

	if f.WindowFiles != 2 || f.WindowBytes != 2*gib {
		t.Errorf("window = %d files/%d bytes, want 2/%d -- only uploads inside the trailing 7 days count", f.WindowFiles, f.WindowBytes, 2*gib)
	}
	// 2 GiB / 7 days => 35 days for 70 GiB remaining.
	if math.Abs(f.DaysRemaining-245) > 1 {
		t.Errorf("DaysRemaining = %.1f, want ~245 (70 GiB at 2/7 GiB per day)", f.DaysRemaining)
	}
}

func TestBuildForecast_NoRecentThroughput_ReportsUnknownNotZero(t *testing.T) {
	now := time.Date(2026, time.September, 9, 12, 0, 0, 0, time.UTC)
	sum := statedb.LedgerSummary{Pending: 500, PendingBytes: 500 << 20}
	// Everything is older than the window -- a long pause, or a fresh
	// install that hasn't uploaded yet.
	uploads := []statedb.UploadedAtSize{upAt(now, 400, 1<<20)}

	f := buildForecast(sum, uploads, now)

	if f.Done {
		t.Error("Done = true, but there is still work pending")
	}
	if f.Known {
		t.Error("Known = true with no throughput in the window -- an ETA here would be a fabrication")
	}
	if !f.ETA.IsZero() || f.DaysRemaining != 0 {
		t.Errorf("ETA/DaysRemaining = %v/%.1f, want zero values when unknown -- a zero ETA must never render as 'finishes now'", f.ETA, f.DaysRemaining)
	}
}

func TestBuildForecast_NothingRemaining_IsDone(t *testing.T) {
	now := time.Date(2026, time.September, 9, 12, 0, 0, 0, time.UTC)
	sum := statedb.LedgerSummary{UploadedByGPB: 100, SyncedBytes: 100 << 20}

	f := buildForecast(sum, []statedb.UploadedAtSize{upAt(now, 1, 1<<20)}, now)

	if !f.Done {
		t.Error("Done = false with an empty queue")
	}
	if f.Known {
		t.Error("Known = true when finished -- there is nothing left to project")
	}
	if math.Abs(f.PercentBytes-100) > 0.01 {
		t.Errorf("PercentBytes = %.2f, want 100", f.PercentBytes)
	}
}

func TestBuildForecast_RetryableCountsAsRemainingButNeedsReviewDoesNot(t *testing.T) {
	now := time.Date(2026, time.September, 9, 12, 0, 0, 0, time.UTC)
	const mib = int64(1) << 20

	sum := statedb.LedgerSummary{
		UploadedByGPB: 10, SyncedBytes: 10 * mib,
		Pending: 5, PendingBytes: 5 * mib,
		// Swept back to pending by every Uploader.Run -- real remaining work.
		FailedRetryable: 3, FailedRetryableBytes: 3 * mib,
		// Blocked on a human decision, not on upload time.
		NeedsReview: 7, NeedsReviewBytes: 7 * mib,
		// A permanent failure is not remaining work at all.
		FailedPermanent: 2,
	}

	f := buildForecast(sum, []statedb.UploadedAtSize{upAt(now, 1, mib)}, now)

	if f.RemainingFiles != 8 || f.RemainingBytes != 8*mib {
		t.Errorf("remaining = %d files/%d bytes, want 8/%d (pending + failed_retryable)", f.RemainingFiles, f.RemainingBytes, 8*mib)
	}
	if f.NeedsReviewFiles != 7 || f.NeedsReviewBytes != 7*mib {
		t.Errorf("needs-review = %d files/%d bytes, want 7/%d reported separately", f.NeedsReviewFiles, f.NeedsReviewBytes, 7*mib)
	}
	// Folding needs_review into the projection would make the finish date
	// recede while the upload queue was actually draining.
	wantDays := float64(8*mib) / (float64(mib) / 7)
	if math.Abs(f.DaysRemaining-wantDays) > 0.01 {
		t.Errorf("DaysRemaining = %.2f, want %.2f -- needs_review must not extend the ETA", f.DaysRemaining, wantDays)
	}
}

func TestBuildForecast_ReadsTheLedger(t *testing.T) {
	db := openTestDB(t)
	mustNoErr(t, db.EnsurePending("h-done", 1024, "image/jpeg", "/lib/a.jpg", nil))
	mustNoErr(t, db.MarkUploaded("h-done", "media-a", ""))
	mustNoErr(t, db.EnsurePending("h-todo", 2048, "image/jpeg", "/lib/b.jpg", nil))

	f, err := BuildForecast(db, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if f.SyncedFiles != 1 || f.RemainingFiles != 1 {
		t.Errorf("synced/remaining = %d/%d, want 1/1", f.SyncedFiles, f.RemainingFiles)
	}
	if f.RemainingBytes != 2048 {
		t.Errorf("RemainingBytes = %d, want 2048", f.RemainingBytes)
	}
	if !f.Known {
		t.Error("Known = false -- the just-uploaded file is inside the window and should give a rate")
	}
}
