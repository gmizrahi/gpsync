package engine

import (
	"testing"

	"github.com/gmizrahi/gpsync/internal/quota"
)

// TestBuildStatus_ReflectsLedgerAndWatchingFlag proves the dashboard's
// entire data contract without needing systray, HTTP, or Windows at all --
// this is what cmd/gpsync-tray's dashboard.go serves as JSON, but the actual
// data assembly is fully testable here.
func TestBuildStatus_ReflectsLedgerAndWatchingFlag(t *testing.T) {
	db := openTestDB(t)

	mustNoErr(t, db.EnsurePending("h-synced", 100, "image/jpeg", "/lib/a.jpg", nil))
	mustNoErr(t, db.MarkUploaded("h-synced", "media-1", ""))
	mustNoErr(t, db.EnsurePending("h-pending", 200, "image/jpeg", "/lib/b.jpg", nil))
	mustNoErr(t, db.EnsurePending("h-failed-perm", 50, "image/jpeg", "/lib/c.jpg", nil))
	mustNoErr(t, db.MarkFailed("h-failed-perm", true, "UNSUPPORTED_EXTENSION", "nope"))

	status, err := BuildStatus(db, true)
	if err != nil {
		t.Fatal(err)
	}
	if !status.Watching {
		t.Error("Watching = false, want true (passed in as true)")
	}
	if status.Synced != 1 {
		t.Errorf("Synced = %d, want 1", status.Synced)
	}
	if status.Pending != 1 {
		t.Errorf("Pending = %d, want 1", status.Pending)
	}
	if status.FailedPermanent != 1 {
		t.Errorf("FailedPermanent = %d, want 1", status.FailedPermanent)
	}

	// watching=false must be reflected verbatim, independent of ledger state.
	paused, err := BuildStatus(db, false)
	if err != nil {
		t.Fatal(err)
	}
	if paused.Watching {
		t.Error("Watching = true, want false (passed in as false)")
	}
}

// TestBuildStatus_NoRunYet_LeavesRunFieldsZero proves a freshly-opened
// ledger (no gpsync sync/upload/watch ever run) doesn't error out just
// because there's no run_progress row to read yet.
func TestBuildStatus_NoRunYet_LeavesRunFieldsZero(t *testing.T) {
	db := openTestDB(t)

	status, err := BuildStatus(db, false)
	if err != nil {
		t.Fatal(err)
	}
	if status.RunActive {
		t.Error("RunActive = true, want false -- no run has ever started")
	}
	if status.LastEndReason != "" {
		t.Errorf("LastEndReason = %q, want empty", status.LastEndReason)
	}
}

// TestBuildStatus_ReportsQuotaUsage proves the dashboard can show the same
// "quota used today" figure `gpsync info` already prints.
func TestBuildStatus_ReportsQuotaUsage(t *testing.T) {
	db := openTestDB(t)
	dq := quota.NewDailyQuota(db)
	mustNoErr(t, dq.Record(42))

	status, err := BuildStatus(db, true)
	if err != nil {
		t.Fatal(err)
	}
	if status.QuotaUsed != 42 {
		t.Errorf("QuotaUsed = %d, want 42", status.QuotaUsed)
	}
	if status.QuotaDailyLimit != quota.DailyLimit {
		t.Errorf("QuotaDailyLimit = %d, want %d", status.QuotaDailyLimit, quota.DailyLimit)
	}
}

// TestBuildStatus_ReportsUploadedToday proves the data behind the
// dashboard's "Uploaded today" card and `gpsync info`'s matching line.
func TestBuildStatus_ReportsUploadedToday(t *testing.T) {
	db := openTestDB(t)
	mustNoErr(t, db.EnsurePending("h1", 1000, "image/jpeg", "/lib/a.jpg", nil))
	mustNoErr(t, db.MarkUploaded("h1", "media-1", ""))
	mustNoErr(t, db.EnsurePending("h2", 2000, "image/jpeg", "/lib/b.jpg", nil))
	mustNoErr(t, db.MarkUploaded("h2", "media-2", ""))
	// A mark-synced row never moved any bytes -- must not inflate the count.
	mustNoErr(t, db.EnsureMarkedSynced("h3", 5000, "image/jpeg", "/lib/c.jpg", nil))

	status, err := BuildStatus(db, true)
	if err != nil {
		t.Fatal(err)
	}
	if status.UploadedToday != 2 {
		t.Errorf("UploadedToday = %d, want 2", status.UploadedToday)
	}
	if status.UploadedTodayBytes != 3000 {
		t.Errorf("UploadedTodayBytes = %d, want 3000", status.UploadedTodayBytes)
	}
}
