package statedb

import (
	"database/sql"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// libRoot and photoRoot are host-shaped fixture roots. A LIKE pattern
// built from filepath.Separator can only match rows written with that
// same separator, so POSIX literals here found nothing on Windows.
var (
	libRoot   = filepath.Join(string(filepath.Separator), "lib")
	photoRoot = filepath.Join(string(filepath.Separator), "photos")
)

func openTestDB(t *testing.T) *DB {
	t.Helper()
	t.Setenv("GPSYNC_STATE_DIR", t.TempDir())
	StateDir = getStateDir()
	StateDBPath = filepath.Join(StateDir, "state.sqlite")
	db, err := Open()
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// TestOpen_ConcurrentAccessDoesNotErrorWithDatabaseLocked proves the fix
// for spurious "database is locked" errors under real upload concurrency:
// many goroutines hitting the DB with a mix of reads and writes at once
// used to be able to open multiple physical SQLite connections via
// database/sql's default pool, and a second connection reading/writing
// while another held the file's write lock got an immediate SQLITE_BUSY
// error (no busy_timeout was set). This was observed in practice being
// misread as quota exhaustion by quota.Exhausted() (see its fail-open
// fix), silently aborting sync runs over a DB hiccup unrelated to quota.
// Open() now caps the pool to a single connection (SetMaxOpenConns(1)),
// routing concurrent callers through Go's own connection queue instead --
// no SQLite-level lock contention is possible from within one process.
func TestOpen_ConcurrentAccessDoesNotErrorWithDatabaseLocked(t *testing.T) {
	db := openTestDB(t)

	const goroutines = 30
	var wg sync.WaitGroup
	errs := make(chan error, goroutines*2)
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			sha := fmt.Sprintf("hash-%d", i)
			if err := db.EnsurePending(sha, 100, "image/jpeg", fmt.Sprintf("/p/%d.jpg", i), nil); err != nil {
				errs <- err
			}
			if _, err := db.GetUpload(sha); err != nil {
				errs <- err
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("concurrent DB access error (want none, even under real concurrency): %v", err)
	}
}

func TestEnsurePending_DedupsByHash(t *testing.T) {
	db := openTestDB(t)

	if err := db.EnsurePending("hash1", 100, "image/jpeg", "/a/one.jpg", nil); err != nil {
		t.Fatal(err)
	}
	// Same hash, different path (simulates a renamed/duplicate file) -- must NOT create a second row.
	if err := db.EnsurePending("hash1", 100, "image/jpeg", "/b/copy.jpg", nil); err != nil {
		t.Fatal(err)
	}

	pending, err := db.ListPending()
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 {
		t.Fatalf("expected 1 pending row after dedup, got %d", len(pending))
	}
	if pending[0].FirstSourcePath != "/a/one.jpg" {
		t.Errorf("expected first_source_path to stay as the first-seen path, got %q", pending[0].FirstSourcePath)
	}
}

func TestEnsurePending_DoesNotDowngradeUploaded(t *testing.T) {
	db := openTestDB(t)

	if err := db.EnsurePending("hash1", 100, "image/jpeg", "/a/one.jpg", nil); err != nil {
		t.Fatal(err)
	}
	if err := db.MarkUploaded("hash1", "media-item-123", ""); err != nil {
		t.Fatal(err)
	}
	// Re-scanning the same content later must not reset it back to pending.
	if err := db.EnsurePending("hash1", 100, "image/jpeg", "/a/one.jpg", nil); err != nil {
		t.Fatal(err)
	}

	u, err := db.GetUpload("hash1")
	if err != nil {
		t.Fatal(err)
	}
	if u.Status != "uploaded" {
		t.Errorf("expected status to remain 'uploaded', got %q", u.Status)
	}
}

func TestMarkFailed_PermanentVsRetryable(t *testing.T) {
	db := openTestDB(t)
	must(t, db.EnsurePending("h1", 1, "image/jpeg", "/a.jpg", nil))
	must(t, db.EnsurePending("h2", 1, "image/jpeg", "/b.jpg", nil))

	must(t, db.MarkFailed("h1", true, "INVALID_ARGUMENT", "bad file"))
	must(t, db.MarkFailed("h2", false, "TRANSIENT", "network blip"))

	counts, err := db.CountsByStatus()
	if err != nil {
		t.Fatal(err)
	}
	if counts["failed_permanent"] != 1 || counts["failed_retryable"] != 1 {
		t.Errorf("counts = %+v, want 1 permanent + 1 retryable", counts)
	}
}

func TestRequeueRetryable(t *testing.T) {
	db := openTestDB(t)
	must(t, db.EnsurePending("h1", 1, "image/jpeg", "/a.jpg", nil))
	must(t, db.MarkFailed("h1", false, "TRANSIENT", "network blip"))

	n, err := db.RequeueRetryable()
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("expected 1 row requeued, got %d", n)
	}
	u, err := db.GetUpload("h1")
	if err != nil {
		t.Fatal(err)
	}
	if u.Status != "pending" {
		t.Errorf("expected status pending after requeue, got %q", u.Status)
	}
}

// TestEnsureNeedsReview_FreshHash_InsertsAsNeedsReview covers the data
// behind the originals-folder review workflow: a file found inside an
// "originals" folder is never auto-queued.
func TestEnsureNeedsReview_FreshHash_InsertsAsNeedsReview(t *testing.T) {
	db := openTestDB(t)
	must(t, db.EnsureNeedsReview("h1", 100, "image/jpeg", "/lib/originals/a.jpg", nil))

	u, err := db.GetUpload("h1")
	if err != nil {
		t.Fatal(err)
	}
	if u.Status != "needs_review" {
		t.Errorf("Status = %q, want needs_review", u.Status)
	}
}

// TestEnsureNeedsReview_AlreadyTrackedHash_NeverDowngraded proves the ON
// CONFLICT DO NOTHING semantics: if the exact same bytes were already seen
// at some OTHER, non-originals path first (already pending/uploaded/failed),
// discovering it again inside an originals folder must never downgrade
// that existing status -- mirrors EnsurePending's own precedent.
func TestEnsureNeedsReview_AlreadyTrackedHash_NeverDowngraded(t *testing.T) {
	db := openTestDB(t)
	must(t, db.EnsurePending("h1", 100, "image/jpeg", "/lib/a.jpg", nil))
	must(t, db.MarkUploaded("h1", "media-1", ""))

	must(t, db.EnsureNeedsReview("h1", 100, "image/jpeg", "/lib/originals/a.jpg", nil))

	u, err := db.GetUpload("h1")
	if err != nil {
		t.Fatal(err)
	}
	if u.Status != "uploaded" {
		t.Errorf("Status = %q, want still uploaded -- EnsureNeedsReview must never downgrade an already-tracked hash", u.Status)
	}
}

// TestResolveNeedsReview_Queue_MovesToPending covers the "queue and
// upload" resolution action.
func TestResolveNeedsReview_Queue_MovesToPending(t *testing.T) {
	db := openTestDB(t)
	must(t, db.EnsureNeedsReview("h1", 100, "image/jpeg", "/lib/originals/a.jpg", nil))
	must(t, db.ResolveNeedsReview("h1", true))

	u, err := db.GetUpload("h1")
	if err != nil {
		t.Fatal(err)
	}
	if u.Status != "pending" {
		t.Errorf("Status = %q, want pending", u.Status)
	}
}

// TestResolveNeedsReview_Ignore_MovesToIgnored covers the "add to ignore
// list" resolution action.
func TestResolveNeedsReview_Ignore_MovesToIgnored(t *testing.T) {
	db := openTestDB(t)
	must(t, db.EnsureNeedsReview("h1", 100, "image/jpeg", "/lib/originals/a.jpg", nil))
	must(t, db.ResolveNeedsReview("h1", false))

	u, err := db.GetUpload("h1")
	if err != nil {
		t.Fatal(err)
	}
	if u.Status != "ignored" {
		t.Errorf("Status = %q, want ignored", u.Status)
	}
}

// TestResolveNeedsReview_NotNeedsReview_NoOp proves the WHERE
// status='needs_review' guard: a stale or replayed form submission (e.g.
// double-clicking Resolve, or an already-resolved item's URL bookmarked)
// must not resurrect an already-ignored row or re-touch an unrelated one.
func TestResolveNeedsReview_NotNeedsReview_NoOp(t *testing.T) {
	db := openTestDB(t)
	must(t, db.EnsureNeedsReview("h1", 100, "image/jpeg", "/lib/originals/a.jpg", nil))
	must(t, db.ResolveNeedsReview("h1", false)) // now ignored

	must(t, db.ResolveNeedsReview("h1", true)) // a second, stale "queue" click

	u, err := db.GetUpload("h1")
	if err != nil {
		t.Fatal(err)
	}
	if u.Status != "ignored" {
		t.Errorf("Status = %q, want still ignored -- a second resolve on an already-resolved item must be a no-op", u.Status)
	}
}

// TestMigrateToNeedsReview_FlipsExistingPendingOrFailedRow is the data
// behind `gpsync clean-originals`, which backfills rows a scan queued BEFORE
// the originals-folder review feature existed.
func TestMigrateToNeedsReview_FlipsExistingPendingOrFailedRow(t *testing.T) {
	db := openTestDB(t)
	must(t, db.EnsurePending("h-pending", 100, "image/jpeg", "/lib/originals/a.jpg", nil))
	must(t, db.MigrateToNeedsReview("h-pending"))

	u, err := db.GetUpload("h-pending")
	if err != nil {
		t.Fatal(err)
	}
	if u.Status != "needs_review" {
		t.Errorf("Status = %q, want needs_review", u.Status)
	}
}

// TestMigrateToNeedsReview_UploadedRow_NeverTouched proves the status
// guard: an already-uploaded hash must never be pulled back into review,
// even if MigrateToNeedsReview were ever called on one by mistake.
func TestMigrateToNeedsReview_UploadedRow_NeverTouched(t *testing.T) {
	db := openTestDB(t)
	must(t, db.EnsurePending("h-done", 100, "image/jpeg", "/lib/originals/a.jpg", nil))
	must(t, db.MarkUploaded("h-done", "media-1", ""))
	must(t, db.MigrateToNeedsReview("h-done"))

	u, err := db.GetUpload("h-done")
	if err != nil {
		t.Fatal(err)
	}
	if u.Status != "uploaded" {
		t.Errorf("Status = %q, want still uploaded", u.Status)
	}
}

// TestNeedsReviewItems_ReturnsOnlyNeedsReviewSortedByPath proves the
// listing is scoped to needs_review only and sorted deterministically.
func TestNeedsReviewItems_ReturnsOnlyNeedsReviewSortedByPath(t *testing.T) {
	db := openTestDB(t)
	must(t, db.EnsureNeedsReview("h-b", 100, "image/jpeg", "/lib/originals/b.jpg", nil))
	must(t, db.EnsureNeedsReview("h-a", 200, "image/jpeg", "/lib/originals/a.jpg", nil))
	must(t, db.EnsurePending("h-pending", 50, "image/jpeg", "/lib/c.jpg", nil))

	items, err := db.NeedsReviewItems()
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 {
		t.Fatalf("len(items) = %d, want 2 (only needs_review rows)", len(items))
	}
	if items[0].FirstSourcePath != "/lib/originals/a.jpg" || items[1].FirstSourcePath != "/lib/originals/b.jpg" {
		t.Errorf("items = %+v, want sorted by path (a.jpg then b.jpg)", items)
	}
}

// TestFindByBasename_MatchesExcludesSelfCaseInsensitive proves the
// basename search behind the review UI's candidate list: the same filename
// looked for across the whole library.
func TestFindByBasename_MatchesExcludesSelfCaseInsensitive(t *testing.T) {
	db := openTestDB(t)
	must(t, db.UpsertFileSeen(filepath.Join(libRoot, "2013", "originals", "IMG_1234.jpg"), "h-original", 0, 100))
	must(t, db.UpsertFileSeen(filepath.Join(libRoot, "2013", "IMG_1234.JPG"), "h-edited", 0, 90)) // same name, different case, different hash
	must(t, db.UpsertFileSeen(filepath.Join(libRoot, "2014", "IMG_1234.jpg"), "h-unrelated", 0, 80))
	must(t, db.UpsertFileSeen(filepath.Join(libRoot, "2013", "other.jpg"), "h-other", 0, 70))

	matches, err := db.FindByBasename("IMG_1234.jpg", "h-original")
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 2 {
		t.Fatalf("len(matches) = %d, want 2 (h-edited and h-unrelated, both named IMG_1234.jpg case-insensitively, self excluded)", len(matches))
	}
	for _, m := range matches {
		if m.SHA256 == "h-original" {
			t.Error("FindByBasename must exclude the item's own hash")
		}
	}
}

// TestFindByBasename_MatchesWindowsBackslashPaths is the fix for a second,
// more serious instance of the same escaping bug as
// TestOriginalsLikePattern_MatchesWindowsBackslashPaths: FindByBasename
// built its own pattern by hand (before originalsLikePattern existed) and
// had the identical raw-separator-collides-with-ESCAPE bug independently.
// This one is more serious because FindByBasename backs BOTH the review
// UI's candidate list AND AutoResolveObviousOriginals' own "does a match
// exist elsewhere" check -- on Windows, an always-empty/wrong result here
// meant the auto-resolution sweep could never reach its genuinely-
// ambiguous case at all: every originals-folder file without an
// immediate-parent sibling looked like it had no match ANYWHERE in the
// library, so it was silently auto-queued as a normal upload instead of
// being left for a human to review.
func TestFindByBasename_MatchesWindowsBackslashPaths(t *testing.T) {
	orig := originalsFolderSep
	originalsFolderSep = `\`
	t.Cleanup(func() { originalsFolderSep = orig })

	db := openTestDB(t)
	must(t, db.UpsertFileSeen(`C:\Photos\2013\originals\IMG_1234.jpg`, "h-original", 0, 100))
	must(t, db.UpsertFileSeen(`C:\Photos\2013\IMG_1234.jpg`, "h-edited", 0, 90))

	matches, err := db.FindByBasename("IMG_1234.jpg", "h-original")
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 1 || matches[0].SHA256 != "h-edited" {
		t.Errorf("matches = %+v, want exactly h-edited -- a Windows-style backslash path must still match", matches)
	}
}

// TestFindByBasename_WindowsPath_UnderscoreBasenameNoFalsePositive is the
// test that actually EXPOSES the bug TestFindByBasename_
// MatchesWindowsBackslashPaths (above) doesn't: a basename starting with
// an ordinary letter happens to still work under the buggy pattern-
// building (escaping a raw backslash before a non-special character is a
// harmless no-op in SQLite's LIKE), so it didn't catch the regression on
// its own. A basename starting with "_" does: likeEscaper turns a leading
// "_" into "\_" on its own, and the buggy code then concatenated a SECOND,
// raw, unescaped separator backslash immediately in front of that --
// "\\_" -- which SQLite's ESCAPE '\' parses as one escaped literal
// backslash (consuming BOTH backslashes) followed by an now-UNESCAPED "_",
// turning the intended literal underscore into a stray single-character
// WILDCARD. That silently matched a decoy filename it should never have.
func TestFindByBasename_WindowsPath_UnderscoreBasenameNoFalsePositive(t *testing.T) {
	orig := originalsFolderSep
	originalsFolderSep = `\`
	t.Cleanup(func() { originalsFolderSep = orig })

	db := openTestDB(t)
	// A decoy whose name differs from "_backup.jpg" only in the first
	// character -- must NOT match "_backup.jpg" (a literal underscore is
	// not a single-character wildcard).
	must(t, db.UpsertFileSeen(`C:\Photos\2013\Xbackup.jpg`, "h-decoy", 0, 90))
	// A genuine match, elsewhere in the library, to prove real matches
	// still work correctly under the fix (not just that nothing matches).
	must(t, db.UpsertFileSeen(`C:\Photos\2014\_backup.jpg`, "h-real-match", 0, 80))

	matches, err := db.FindByBasename("_backup.jpg", "h-self")
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 1 || matches[0].SHA256 != "h-real-match" {
		t.Errorf("matches = %+v, want exactly h-real-match -- \"Xbackup.jpg\" must not falsely match \"_backup.jpg\"", matches)
	}
}

// TestOriginalsFolderRowsPendingOrFailed_PreFiltersByOriginalsPathSegment
// proves the SQL-level pre-filter actually narrows to paths that plausibly
// contain an "originals" segment -- cmd/gpsync's own scanner.IsInOriginalsFolder
// re-check (not exercised here, to avoid an import cycle) is what makes
// this exact rather than approximate.
func TestOriginalsFolderRowsPendingOrFailed_PreFiltersByOriginalsPathSegment(t *testing.T) {
	db := openTestDB(t)
	must(t, db.EnsurePending("h-orig", 100, "image/jpeg", filepath.Join(libRoot, "2013", "originals", "a.jpg"), nil))
	must(t, db.UpsertFileSeen(filepath.Join(libRoot, "2013", "originals", "a.jpg"), "h-orig", 0, 100))
	must(t, db.MarkFailed("h-orig", false, "SOME_ERROR", "transient"))

	must(t, db.EnsurePending("h-plain", 100, "image/jpeg", filepath.Join(libRoot, "2013", "b.jpg"), nil))
	must(t, db.UpsertFileSeen(filepath.Join(libRoot, "2013", "b.jpg"), "h-plain", 0, 100))

	must(t, db.EnsurePending("h-done", 100, "image/jpeg", filepath.Join(libRoot, "2013", "originals", "c.jpg"), nil))
	must(t, db.UpsertFileSeen(filepath.Join(libRoot, "2013", "originals", "c.jpg"), "h-done", 0, 100))
	must(t, db.MarkUploaded("h-done", "media-1", "")) // already uploaded -- must not appear

	items, err := db.OriginalsFolderRowsPendingOrFailed()
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].SHA256 != "h-orig" {
		t.Errorf("items = %+v, want exactly h-orig (pending/failed AND inside an originals-like path)", items)
	}
}

// TestOriginalsFolderRowsUploaded_FindsAlreadyUploadedOriginalsFiles
// covers listing originals-folder files already uploaded: files pushed to
// Google Photos before the review
// workflow existed, so there's nothing left for gpsync itself to fix, only
// something for the user to go find and clean up manually.
func TestOriginalsFolderRowsUploaded_FindsAlreadyUploadedOriginalsFiles(t *testing.T) {
	db := openTestDB(t)
	must(t, db.EnsurePending("h-uploaded-orig", 100, "image/jpeg", filepath.Join(libRoot, "2013", "originals", "a.jpg"), nil))
	must(t, db.UpsertFileSeen(filepath.Join(libRoot, "2013", "originals", "a.jpg"), "h-uploaded-orig", 0, 100))
	must(t, db.MarkUploaded("h-uploaded-orig", "media-item-1", ""))

	// A pending (not yet uploaded) originals-folder file -- must not appear.
	must(t, db.EnsurePending("h-pending-orig", 100, "image/jpeg", filepath.Join(libRoot, "2013", "originals", "b.jpg"), nil))
	must(t, db.UpsertFileSeen(filepath.Join(libRoot, "2013", "originals", "b.jpg"), "h-pending-orig", 0, 100))

	// An uploaded file NOT in an originals folder -- must not appear.
	must(t, db.EnsurePending("h-uploaded-plain", 100, "image/jpeg", filepath.Join(libRoot, "2013", "c.jpg"), nil))
	must(t, db.UpsertFileSeen(filepath.Join(libRoot, "2013", "c.jpg"), "h-uploaded-plain", 0, 100))
	must(t, db.MarkUploaded("h-uploaded-plain", "media-item-2", ""))

	rows, err := db.OriginalsFolderRowsUploaded()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("len(rows) = %d, want 1", len(rows))
	}
	if rows[0].SHA256 != "h-uploaded-orig" || rows[0].MediaItemID != "media-item-1" {
		t.Errorf("rows[0] = %+v, want h-uploaded-orig/media-item-1", rows[0])
	}
}

// TestOriginalsLikePattern_MatchesWindowsBackslashPaths is the fix for a
// real, reported bug: `gpsync originals-uploaded` returned nothing on
// Windows despite the user confirming a real match existed. The pattern
// used to concatenate a raw, unescaped separator directly into the LIKE
// pattern -- on Windows ('\'), that collided with the SAME character
// chosen as the LIKE ESCAPE character, silently breaking the match
// entirely (misparsed as escape sequences, not literal backslashes)
// rather than erroring. This project's own test suite runs from
// WSL/Linux, where the separator is '/' and doesn't collide the same
// way -- structurally invisible to Linux-only testing, which is exactly
// why originalsFolderSep is overridable here: this test forces the
// Windows-style separator and proves the query still matches, end to
// end, against a real backslash-separated path.
func TestOriginalsLikePattern_MatchesWindowsBackslashPaths(t *testing.T) {
	orig := originalsFolderSep
	originalsFolderSep = `\`
	t.Cleanup(func() { originalsFolderSep = orig })

	db := openTestDB(t)
	winPath := `C:\Photos\2013\originals\IMG_1234.jpg`
	must(t, db.EnsurePending("h-win", 100, "image/jpeg", winPath, nil))
	must(t, db.UpsertFileSeen(winPath, "h-win", 0, 100))
	must(t, db.MarkFailed("h-win", false, "SOME_ERROR", "transient"))

	items, err := db.OriginalsFolderRowsPendingOrFailed()
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].SHA256 != "h-win" {
		t.Errorf("items = %+v, want exactly h-win -- a Windows-style backslash path must still match", items)
	}
}

// TestRecordThrottleEvent_ThrottleEvents_RoundTripsInOrder covers the
// throttle log itself: the record that makes it possible to say how long
// recovery actually takes.
func TestCreateSession_ValidateSession_RoundTrips(t *testing.T) {
	db := openTestDB(t)
	must(t, db.CreateSession("tok-1", now()+3600))

	valid, err := db.ValidateSession("tok-1")
	if err != nil {
		t.Fatal(err)
	}
	if !valid {
		t.Error("ValidateSession(tok-1) = false, want true for a freshly-created, unexpired session")
	}

	valid, err = db.ValidateSession("does-not-exist")
	if err != nil {
		t.Fatal(err)
	}
	if valid {
		t.Error("ValidateSession(does-not-exist) = true, want false")
	}
}

func TestValidateSession_ExpiredSessionIsRejectedAndPruned(t *testing.T) {
	db := openTestDB(t)
	must(t, db.CreateSession("tok-expired", now()-1)) // already expired

	valid, err := db.ValidateSession("tok-expired")
	if err != nil {
		t.Fatal(err)
	}
	if valid {
		t.Error("ValidateSession(tok-expired) = true, want false for an already-expired session")
	}

	var count int
	if err := db.conn.QueryRow(`SELECT COUNT(*) FROM dashboard_sessions WHERE token = 'tok-expired'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Error("expired session row was not pruned by ValidateSession")
	}
}

func TestDeleteSession_LogsOutAndIsIdempotent(t *testing.T) {
	db := openTestDB(t)
	must(t, db.CreateSession("tok-logout", now()+3600))
	must(t, db.DeleteSession("tok-logout"))

	valid, err := db.ValidateSession("tok-logout")
	if err != nil {
		t.Fatal(err)
	}
	if valid {
		t.Error("session still valid after DeleteSession")
	}

	// Deleting again (double logout) must not error.
	must(t, db.DeleteSession("tok-logout"))
}

func TestPruneExpiredSessions_RemovesOnlyExpiredRows(t *testing.T) {
	db := openTestDB(t)
	must(t, db.CreateSession("tok-live", now()+3600))
	must(t, db.CreateSession("tok-dead", now()-3600))
	must(t, db.PruneExpiredSessions())

	var count int
	if err := db.conn.QueryRow(`SELECT COUNT(*) FROM dashboard_sessions`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("dashboard_sessions has %d row(s) after pruning, want 1 (only the live session)", count)
	}
	valid, err := db.ValidateSession("tok-live")
	if err != nil {
		t.Fatal(err)
	}
	if !valid {
		t.Error("the live session was pruned along with the expired one")
	}
}

func TestLastCrash_NoneRecordedYet(t *testing.T) {
	db := openTestDB(t)
	_, ok, err := db.LastCrash()
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Error("LastCrash ok = true, want false when nothing has ever been recorded")
	}
}

func TestRecordCrash_LastCrash_ReturnsTheMostRecentOne(t *testing.T) {
	db := openTestDB(t)
	must(t, db.RecordCrash("onReady", "nil pointer dereference"))
	must(t, db.RecordCrash("runTrayIconTicker", "index out of range"))

	rec, ok, err := db.LastCrash()
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("LastCrash ok = false, want true")
	}
	if rec.Context != "runTrayIconTicker" || rec.Message != "index out of range" {
		t.Errorf("LastCrash = %+v, want the SECOND (most recent) recorded crash", rec)
	}
}

func TestRecordThrottleEvent_ThrottleEvents_RoundTripsInOrder(t *testing.T) {
	db := openTestDB(t)
	must(t, db.RecordThrottleEvent(1, 60, "Quota exceeded for quota 'concurrent write request'"))
	must(t, db.RecordThrottleEvent(2, 180, "Quota exceeded for quota 'concurrent write request'"))

	events, err := db.ThrottleEvents()
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 {
		t.Fatalf("len(events) = %d, want 2", len(events))
	}
	if events[0].Rung != 1 || events[0].WaitSeconds != 60 {
		t.Errorf("events[0] = %+v, want rung=1 wait=60", events[0])
	}
	if events[1].Rung != 2 || events[1].WaitSeconds != 180 {
		t.Errorf("events[1] = %+v, want rung=2 wait=180", events[1])
	}
	if events[0].At > events[1].At {
		t.Error("events must be ordered oldest first")
	}
}

// TestSuccessfulUploadTimestamps_AscendingExcludesMarkSynced is the
// primitive BuildThrottleAnalysis now walks in one pass instead of issuing
// a FirstUploadAfter/UploadsBetween query per throttle event (see its own
// doc comment).
func TestSuccessfulUploadTimestamps_AscendingExcludesMarkSynced(t *testing.T) {
	db := openTestDB(t)
	// h2 is marked uploaded (and so gets its uploaded_at, from the real
	// clock -- can't be injected) BEFORE h1, named the OPPOSITE of upload
	// order on purpose: the query's own ORDER BY, not row-id/insertion
	// order, must be what makes the result ascending.
	must(t, db.EnsurePending("h2", 2000, "image/jpeg", "/lib/b.jpg", nil))
	must(t, db.MarkUploaded("h2", "media-2", ""))
	must(t, db.EnsurePending("h1", 1000, "image/jpeg", "/lib/a.jpg", nil))
	must(t, db.MarkUploaded("h1", "media-1", ""))
	// A `gpsync mark-synced` row never actually contended for the write
	// quota, so it must not appear at all.
	must(t, db.EnsureMarkedSynced("h-synced", 5000, "image/jpeg", "/lib/c.jpg", nil))

	u1, err := db.GetUpload("h1")
	if err != nil {
		t.Fatal(err)
	}
	u2, err := db.GetUpload("h2")
	if err != nil {
		t.Fatal(err)
	}

	got, err := db.SuccessfulUploadTimestamps()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d rows, want 2 (mark-synced excluded): %+v", len(got), got)
	}
	// h2 was marked uploaded FIRST, so it must come first here too, even
	// though h1 was named/inserted first.
	if got[0].At != u2.UploadedAt.Float64 || got[0].Size != 2000 {
		t.Errorf("got[0] = %+v, want h2's own At=%v Size=2000", got[0], u2.UploadedAt.Float64)
	}
	if got[1].At != u1.UploadedAt.Float64 || got[1].Size != 1000 {
		t.Errorf("got[1] = %+v, want h1's own At=%v Size=1000", got[1], u1.UploadedAt.Float64)
	}
	if got[0].At > got[1].At {
		t.Error("rows must be ascending by uploaded_at")
	}
}

func TestQuotaIncrement_Accumulates(t *testing.T) {
	db := openTestDB(t)
	must(t, db.QuotaIncrement("2026-08-29", 3))
	must(t, db.QuotaIncrement("2026-08-29", 2))

	used, err := db.QuotaUsed("2026-08-29")
	if err != nil {
		t.Fatal(err)
	}
	if used != 5 {
		t.Errorf("got %d, want 5", used)
	}

	otherDay, err := db.QuotaUsed("2026-08-30")
	if err != nil {
		t.Fatal(err)
	}
	if otherDay != 0 {
		t.Errorf("expected 0 for an untouched date, got %d", otherDay)
	}
}

// TestUploadedSince_CountsRealUploadsExcludesMarkSyncedAndOldOnes proves the
// data behind the "uploaded today" counter: how many files and bytes were
// uploaded since the last (Pacific-midnight) reset. Must
// count only genuine gpsync uploads (a real google_media_item_id), not `gpsync
// mark-synced` rows -- those never moved any bytes, so counting them would
// overstate real upload throughput, same distinction LedgerSummary draws.
func TestUploadedSince_CountsRealUploadsExcludesMarkSyncedAndOldOnes(t *testing.T) {
	db := openTestDB(t)

	must(t, db.EnsurePending("h-real-1", 100, "image/jpeg", "/lib/a.jpg", nil))
	must(t, db.MarkUploaded("h-real-1", "media-1", ""))
	must(t, db.EnsurePending("h-real-2", 250, "image/jpeg", "/lib/b.jpg", nil))
	must(t, db.MarkUploaded("h-real-2", "media-2", ""))
	must(t, db.EnsureMarkedSynced("h-marksynced", 9999, "image/jpeg", "/lib/c.jpg", nil))
	must(t, db.EnsurePending("h-pending", 500, "image/jpeg", "/lib/d.jpg", nil))

	count, bytes, err := db.UploadedSince(0)
	if err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Errorf("count = %d, want 2 (mark-synced and still-pending rows must be excluded)", count)
	}
	if bytes != 350 {
		t.Errorf("bytes = %d, want 350 (100+250)", bytes)
	}

	// A cutoff in the far future must exclude everything -- this is what
	// makes the counter reset in practice at the next Pacific midnight.
	future, _, err := db.UploadedSince(9_999_999_999)
	if err != nil {
		t.Fatal(err)
	}
	if future != 0 {
		t.Errorf("count with a future cutoff = %d, want 0", future)
	}
}

// TestLastUploadAt_IgnoresMarkSyncedAndReportsNeverUploaded covers the data
// behind the dashboard's "Last successful upload" cell. Two things have to
// hold for that cell to be worth reading:
//
// A mark-synced row must not count. It carries an uploaded_at like any
// other 'uploaded' row, but it records a bookkeeping decision rather than a
// transfer -- letting one answer "when did an upload last succeed" would
// report the pipeline as healthy at the exact moment it had been stalled
// for days, which is the one question this cell exists to answer.
//
// And "never" must be distinguishable from "at the epoch": the ok flag is
// what stops an empty ledger from rendering as "January 1, 1970".
func TestLastUploadAt_IgnoresMarkSyncedAndReportsNeverUploaded(t *testing.T) {
	db := openTestDB(t)

	if _, ok, err := db.LastUploadAt(); err != nil {
		t.Fatal(err)
	} else if ok {
		t.Error("ok = true on an empty ledger -- nothing has ever been uploaded")
	}

	// A mark-synced row on its own is still "never uploaded".
	must(t, db.EnsureMarkedSynced("h-marksynced", 10, "image/jpeg", "/lib/c.jpg", nil))
	if _, ok, err := db.LastUploadAt(); err != nil {
		t.Fatal(err)
	} else if ok {
		t.Error("ok = true with only a mark-synced row -- no bytes have ever moved")
	}

	must(t, db.EnsurePending("h-real", 100, "image/jpeg", "/lib/a.jpg", nil))
	must(t, db.MarkUploaded("h-real", "media-1", ""))
	at, ok, err := db.LastUploadAt()
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("ok = false after a real upload")
	}
	if delta := math.Abs(at - float64(time.Now().Unix())); delta > 5 {
		t.Errorf("LastUploadAt = %.0f, want ~now (off by %.0fs)", at, delta)
	}

	// The MAX must track the newest upload, not the first one.
	must(t, db.EnsurePending("h-newer", 100, "image/jpeg", "/lib/b.jpg", nil))
	must(t, db.MarkUploaded("h-newer", "media-2", ""))
	newer, _, err := db.LastUploadAt()
	if err != nil {
		t.Fatal(err)
	}
	if newer < at {
		t.Errorf("LastUploadAt went backwards: %.0f then %.0f", at, newer)
	}
}

func TestResetStatusUnder_ResetsUploadedAndFailedBackToPending(t *testing.T) {
	db := openTestDB(t)
	must(t, db.EnsurePending("h1", 1, "image/jpeg", filepath.Join(photoRoot, "2024", "a.jpg"), nil))
	must(t, db.MarkUploaded("h1", "media-1", ""))
	must(t, db.EnsurePending("h2", 1, "image/jpeg", filepath.Join(photoRoot, "2024", "b.jpg"), nil))
	must(t, db.MarkFailed("h2", true, "INVALID_ARGUMENT", "bad file"))
	must(t, db.UpsertFileSeen(filepath.Join(photoRoot, "2024", "a.jpg"), "h1", 0, 1))
	must(t, db.UpsertFileSeen(filepath.Join(photoRoot, "2024", "b.jpg"), "h2", 0, 1))
	// A file outside the reset scope must be untouched.
	must(t, db.EnsurePending("h3", 1, "image/jpeg", filepath.Join(photoRoot, "2023", "c.jpg"), nil))
	must(t, db.MarkUploaded("h3", "media-3", ""))
	must(t, db.UpsertFileSeen(filepath.Join(photoRoot, "2023", "c.jpg"), "h3", 0, 1))

	n, err := db.ResetStatusUnder([]string{filepath.Join(photoRoot, "2024")})
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("reset count = %d, want 2", n)
	}

	u1, err := db.GetUpload("h1")
	if err != nil {
		t.Fatal(err)
	}
	if u1.Status != "pending" || u1.GoogleMediaItemID.Valid {
		t.Errorf("h1 not properly reset: %+v", u1)
	}
	u2, err := db.GetUpload("h2")
	if err != nil {
		t.Fatal(err)
	}
	if u2.Status != "pending" {
		t.Errorf("h2 not reset: %+v", u2)
	}

	u3, err := db.GetUpload("h3")
	if err != nil {
		t.Fatal(err)
	}
	if u3.Status != "uploaded" {
		t.Errorf("h3 (outside scope) was unexpectedly reset: %+v", u3)
	}
}

func TestResetStatusUnder_RefusesEmptyScope(t *testing.T) {
	db := openTestDB(t)
	if _, err := db.ResetStatusUnder(nil); err == nil {
		t.Error("expected an error when resetting with no scope (would wipe the whole library)")
	}
}

func TestEnsureMarkedSynced_DedupsByHashAndNeverDowngrades(t *testing.T) {
	db := openTestDB(t)
	must(t, db.EnsureMarkedSynced("h1", 100, "image/jpeg", "/a/one.jpg", nil))

	u, err := db.GetUpload("h1")
	if err != nil {
		t.Fatal(err)
	}
	if u.Status != "uploaded" || u.GoogleMediaItemID.Valid {
		t.Errorf("expected status=uploaded with no media item id, got %+v", u)
	}

	// Marking the same hash synced again (e.g. re-running mark-synced) must not error or change anything.
	must(t, db.EnsureMarkedSynced("h1", 100, "image/jpeg", "/b/copy.jpg", nil))
	pending, err := db.ListPending()
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Errorf("expected no pending rows, got %d", len(pending))
	}
}

func TestEnsureMarkedSynced_PromotesExistingPendingRow(t *testing.T) {
	db := openTestDB(t)
	// Simulates the real workflow: scan first (-> pending), then mark-synced on the same folder.
	must(t, db.EnsurePending("h1", 100, "image/jpeg", "/a/one.jpg", nil))

	u, err := db.GetUpload("h1")
	if err != nil {
		t.Fatal(err)
	}
	if u.Status != "pending" {
		t.Fatalf("precondition failed: expected pending, got %q", u.Status)
	}

	must(t, db.EnsureMarkedSynced("h1", 100, "image/jpeg", "/a/one.jpg", nil))

	u, err = db.GetUpload("h1")
	if err != nil {
		t.Fatal(err)
	}
	if u.Status != "uploaded" {
		t.Errorf("expected mark-synced to promote a pending row to uploaded, got %q", u.Status)
	}
	if u.GoogleMediaItemID.Valid {
		t.Errorf("expected no media item id on a mark-synced row, got %+v", u.GoogleMediaItemID)
	}
}

func TestEnsureMarkedSynced_DoesNotOverrideARealUploadOrFailure(t *testing.T) {
	db := openTestDB(t)
	must(t, db.EnsurePending("h-uploaded", 100, "image/jpeg", "/a.jpg", nil))
	must(t, db.MarkUploaded("h-uploaded", "media-real-123", ""))
	must(t, db.EnsurePending("h-failed", 100, "image/jpeg", "/b.jpg", nil))
	must(t, db.MarkFailed("h-failed", true, "UNSUPPORTED_EXTENSION", "bad format"))

	must(t, db.EnsureMarkedSynced("h-uploaded", 100, "image/jpeg", "/a.jpg", nil))
	must(t, db.EnsureMarkedSynced("h-failed", 100, "image/jpeg", "/b.jpg", nil))

	uploaded, err := db.GetUpload("h-uploaded")
	if err != nil {
		t.Fatal(err)
	}
	if uploaded.GoogleMediaItemID.String != "media-real-123" {
		t.Errorf("mark-synced must not clear a real media item id, got %+v", uploaded.GoogleMediaItemID)
	}

	failed, err := db.GetUpload("h-failed")
	if err != nil {
		t.Fatal(err)
	}
	if failed.Status != "failed_permanent" {
		t.Errorf("mark-synced must not silently clear a real failure, got status %q", failed.Status)
	}
}

func TestDuplicateGroups_FindsAndSortsBySpaceReclaimable(t *testing.T) {
	db := openTestDB(t)

	// h-big: 3 copies of a 1000-byte file -> 2000 bytes reclaimable.
	must(t, db.EnsurePending("h-big", 1000, "image/jpeg", "/a/one.jpg", nil))
	must(t, db.UpsertFileSeen("/a/one.jpg", "h-big", 0, 1000))
	must(t, db.UpsertFileSeen("/b/two.jpg", "h-big", 0, 1000))
	must(t, db.UpsertFileSeen("/c/three.jpg", "h-big", 0, 1000))

	// h-small: 2 copies of a 100-byte file -> 100 bytes reclaimable (should rank after h-big).
	must(t, db.EnsurePending("h-small", 100, "image/jpeg", "/x/one.jpg", nil))
	must(t, db.UpsertFileSeen("/x/one.jpg", "h-small", 0, 100))
	must(t, db.UpsertFileSeen("/y/copy.jpg", "h-small", 0, 100))

	// h-unique: only one path -- must not appear as a duplicate group.
	must(t, db.EnsurePending("h-unique", 500, "image/jpeg", "/z/solo.jpg", nil))
	must(t, db.UpsertFileSeen("/z/solo.jpg", "h-unique", 0, 500))

	groups, err := db.DuplicateGroups()
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != 2 {
		t.Fatalf("got %d groups, want 2", len(groups))
	}
	if groups[0].SHA256 != "h-big" {
		t.Errorf("groups[0] = %q, want h-big (largest reclaimable first)", groups[0].SHA256)
	}
	if len(groups[0].Paths) != 3 {
		t.Errorf("h-big paths = %d, want 3", len(groups[0].Paths))
	}
	if groups[1].SHA256 != "h-small" {
		t.Errorf("groups[1] = %q, want h-small", groups[1].SHA256)
	}
	if len(groups[1].Paths) != 2 {
		t.Errorf("h-small paths = %d, want 2", len(groups[1].Paths))
	}
}

// TestMigrateFilesSeenCaseInsensitive_MergesCaseVariantPaths_KeepsRealDuplicates
// simulates a state.sqlite created by an older gpsync version, before
// files_seen.path had a case-insensitive collation: two rows for the exact
// same physical file recorded under different-case paths (e.g. from
// running `gpsync sync` against "C:\Photos\..." once and "C:\photos\..."
// another time -- Windows filesystems don't distinguish the two, but the
// old schema's case-sensitive TEXT primary key did). Proves Open()'s
// migration merges those into one row going forward -- so `gpsync duplicates`
// stops reporting the same file as "2 copies" of itself -- while a genuine
// duplicate (two different real paths, same content) is left untouched.
func TestMigrateFilesSeenCaseInsensitive_MergesCaseVariantPaths_KeepsRealDuplicates(t *testing.T) {
	t.Setenv("GPSYNC_STATE_DIR", t.TempDir())
	StateDir = getStateDir()
	StateDBPath = filepath.Join(StateDir, "state.sqlite")
	if err := os.MkdirAll(StateDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Hand-build the OLD (pre-migration) schema directly, bypassing Open()
	// entirely, so this test actually exercises the migration path rather
	// than starting from the already-fixed schema.
	raw, err := sql.Open("sqlite", StateDBPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`
		CREATE TABLE uploads (
		    sha256 TEXT PRIMARY KEY, size INTEGER, mime_type TEXT, status TEXT NOT NULL,
		    google_media_item_id TEXT, first_source_path TEXT, captured_at REAL,
		    uploaded_at REAL, last_error_code TEXT, last_error_message TEXT,
		    attempt_count INTEGER NOT NULL DEFAULT 0
		);
		CREATE TABLE files_seen (path TEXT PRIMARY KEY, sha256 TEXT, mtime REAL, size INTEGER, last_scanned REAL);
	`); err != nil {
		t.Fatal(err)
	}
	// h-samefile: one physical file, scanned twice under different casing.
	if _, err := raw.Exec(`INSERT INTO uploads (sha256, size, mime_type, status, first_source_path) VALUES ('h-samefile', 1000, 'video/mp4', 'uploaded', 'C:\Photos\2024\clip.mp4')`); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`INSERT INTO files_seen (path, sha256, mtime, size, last_scanned) VALUES (?, 'h-samefile', 0, 1000, 100)`, `C:\Photos\2024\clip.mp4`); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`INSERT INTO files_seen (path, sha256, mtime, size, last_scanned) VALUES (?, 'h-samefile', 0, 1000, 200)`, `c:\photos\2024\clip.mp4`); err != nil {
		t.Fatal(err)
	}
	// h-realdupe: two genuinely different paths -- must survive migration untouched.
	if _, err := raw.Exec(`INSERT INTO uploads (sha256, size, mime_type, status, first_source_path) VALUES ('h-realdupe', 500, 'image/jpeg', 'uploaded', '/a/one.jpg')`); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`INSERT INTO files_seen (path, sha256, mtime, size, last_scanned) VALUES ('/a/one.jpg', 'h-realdupe', 0, 500, 100)`); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`INSERT INTO files_seen (path, sha256, mtime, size, last_scanned) VALUES ('/b/two.jpg', 'h-realdupe', 0, 500, 200)`); err != nil {
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	// Now open for real -- this is what must trigger the migration.
	db, err := Open()
	if err != nil {
		t.Fatalf("Open (expected to migrate cleanly): %v", err)
	}
	t.Cleanup(func() { db.Close() })

	var count int
	if err := db.conn.QueryRow(`SELECT COUNT(*) FROM files_seen WHERE sha256='h-samefile'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Errorf("files_seen rows for h-samefile after migration = %d, want 1 (the two case-variant paths must merge)", count)
	}

	// The kept row should be the most-recently-scanned one (last_scanned=200 -> lowercase path).
	seen, err := db.GetFileSeen(`c:\photos\2024\clip.mp4`)
	if err != nil {
		t.Fatal(err)
	}
	if seen == nil {
		t.Fatal("GetFileSeen: expected a cache hit regardless of path casing")
	}
	if seen.LastScanned != 200 {
		t.Errorf("kept row's last_scanned = %v, want 200 (the more recently scanned of the two case variants)", seen.LastScanned)
	}
	// A lookup under the OTHER casing must also hit the same row -- proves
	// the collation is actually case-insensitive now, not just that one
	// row happened to survive.
	seenOtherCase, err := db.GetFileSeen(`C:\PHOTOS\2024\CLIP.MP4`)
	if err != nil {
		t.Fatal(err)
	}
	if seenOtherCase == nil || seenOtherCase.SHA256 != "h-samefile" {
		t.Errorf("GetFileSeen with different casing = %+v, want a hit on h-samefile", seenOtherCase)
	}

	groups, err := db.DuplicateGroups()
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != 1 {
		t.Fatalf("got %d duplicate group(s), want exactly 1 -- h-samefile must no longer appear (it was never really 2 files), h-realdupe must still appear: %+v", len(groups), groups)
	}
	if groups[0].SHA256 != "h-realdupe" {
		t.Errorf("the surviving duplicate group = %q, want h-realdupe", groups[0].SHA256)
	}
	if len(groups[0].Paths) != 2 {
		t.Errorf("h-realdupe paths = %d, want 2 (untouched by the migration)", len(groups[0].Paths))
	}
}

func TestLedgerSummary_DistinguishesUploadedFromMarkedSynced(t *testing.T) {
	db := openTestDB(t)

	// A real upload: EnsurePending then MarkUploaded with a real media item id.
	must(t, db.EnsurePending("h-real", 1000, "image/jpeg", filepath.Join(photoRoot, "2024", "a.jpg"), nil))
	must(t, db.MarkUploaded("h-real", "media-item-123", ""))
	must(t, db.UpsertFileSeen(filepath.Join(photoRoot, "2024", "a.jpg"), "h-real", 0, 1000))

	// A mark-synced snapshot: no network call, no media item id.
	must(t, db.EnsureMarkedSynced("h-marked", 2000, "image/jpeg", filepath.Join(photoRoot, "2023", "b.jpg"), nil))
	must(t, db.UpsertFileSeen(filepath.Join(photoRoot, "2023", "b.jpg"), "h-marked", 0, 2000))

	// A still-pending file, and one of each failure kind.
	must(t, db.EnsurePending("h-pending", 500, "image/jpeg", filepath.Join(photoRoot, "2024", "c.jpg"), nil))
	must(t, db.UpsertFileSeen(filepath.Join(photoRoot, "2024", "c.jpg"), "h-pending", 0, 500))

	must(t, db.EnsurePending("h-failperm", 100, "application/octet-stream", filepath.Join(photoRoot, "2024", "d.bmp"), nil))
	must(t, db.MarkFailed("h-failperm", true, "UNSUPPORTED_EXTENSION", "bad format"))
	must(t, db.UpsertFileSeen(filepath.Join(photoRoot, "2024", "d.bmp"), "h-failperm", 0, 100))

	must(t, db.EnsurePending("h-failretry", 300, "image/jpeg", filepath.Join(photoRoot, "2024", "e.jpg"), nil))
	must(t, db.MarkFailed("h-failretry", false, "TRANSIENT", "network blip"))
	must(t, db.UpsertFileSeen(filepath.Join(photoRoot, "2024", "e.jpg"), "h-failretry", 0, 300))

	// Duplicate content under a second path -- files_seen grows, uploads (distinct hashes) does not.
	must(t, db.UpsertFileSeen(filepath.Join(photoRoot, "2024_copy", "a.jpg"), "h-real", 0, 1000))

	s, err := db.LedgerSummary()
	if err != nil {
		t.Fatal(err)
	}

	if s.UploadedByGPB != 1 {
		t.Errorf("UploadedByGPB = %d, want 1", s.UploadedByGPB)
	}
	if s.MarkedSynced != 1 {
		t.Errorf("MarkedSynced = %d, want 1", s.MarkedSynced)
	}
	if s.Pending != 1 {
		t.Errorf("Pending = %d, want 1", s.Pending)
	}
	if s.FailedPermanent != 1 {
		t.Errorf("FailedPermanent = %d, want 1", s.FailedPermanent)
	}
	if s.FailedRetryable != 1 {
		t.Errorf("FailedRetryable = %d, want 1", s.FailedRetryable)
	}
	if s.TotalHashes != 5 {
		t.Errorf("TotalHashes = %d, want 5", s.TotalHashes)
	}
	if want := int64(1000 + 2000 + 500 + 100 + 300); s.TotalBytesTracked != want {
		t.Errorf("TotalBytesTracked = %d, want %d", s.TotalBytesTracked, want)
	}
	if want := int64(1000 + 2000); s.SyncedBytes != want {
		t.Errorf("SyncedBytes = %d, want %d (real upload + marked-synced)", s.SyncedBytes, want)
	}
	if s.PendingBytes != 500 {
		t.Errorf("PendingBytes = %d, want 500", s.PendingBytes)
	}
	if s.TotalFilesSeen != 6 {
		t.Errorf("TotalFilesSeen = %d, want 6 (5 unique + 1 duplicate path)", s.TotalFilesSeen)
	}
	if s.TotalFolders != 3 {
		t.Errorf("TotalFolders = %d, want 3 (2024, 2023, 2024_copy)", s.TotalFolders)
	}
}

// TestLedgerSummary_CountsNeedsReviewAndIgnored is the fix for a real gap:
// a hash routed to needs_review/ignored (see scanner.IsInOriginalsFolder)
// was already counted in TotalHashes/TotalBytesTracked, but invisible in
// every OTHER LedgerSummary field -- `gpsync info`'s printed sections
// wouldn't account for it anywhere at all.
func TestLedgerSummary_CountsNeedsReviewAndIgnored(t *testing.T) {
	db := openTestDB(t)
	must(t, db.EnsureNeedsReview("h-review", 100, "image/jpeg", "/lib/originals/a.jpg", nil))
	must(t, db.EnsureNeedsReview("h-ignored", 200, "image/jpeg", "/lib/originals/b.jpg", nil))
	must(t, db.ResolveNeedsReview("h-ignored", false))

	s, err := db.LedgerSummary()
	if err != nil {
		t.Fatal(err)
	}
	if s.NeedsReview != 1 {
		t.Errorf("NeedsReview = %d, want 1", s.NeedsReview)
	}
	if s.NeedsReviewBytes != 100 {
		t.Errorf("NeedsReviewBytes = %d, want 100", s.NeedsReviewBytes)
	}
	if s.Ignored != 1 {
		t.Errorf("Ignored = %d, want 1", s.Ignored)
	}
	if s.TotalHashes != 2 {
		t.Errorf("TotalHashes = %d, want 2 (both still tracked, regardless of status)", s.TotalHashes)
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// TestFolderLikePattern_EscapesWildcardsAndAnchorsToTheFolder is the unit
// half of the sibling-folder fix: the pattern must end at a path separator
// and must neutralize LIKE's own wildcards (`%`, `_`) plus the escape
// character itself.
func TestFolderLikePattern_EscapesWildcardsAndAnchorsToTheFolder(t *testing.T) {
	sep := string(filepath.Separator)
	cases := []struct {
		name   string
		folder string
		want   string
	}{
		{"plain folder gets a trailing separator", "/lib/2024", `/lib/2024` + escapeSep(sep) + "%"},
		{"an existing trailing separator is not doubled", "/lib/2024" + sep, `/lib/2024` + escapeSep(sep) + "%"},
		{"underscore is escaped, not left as a wildcard", "/lib/2024_01", `/lib/2024\_01` + escapeSep(sep) + "%"},
		{"percent is escaped", "/lib/100%", `/lib/100\%` + escapeSep(sep) + "%"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := folderLikePattern(tc.folder); got != tc.want {
				t.Errorf("folderLikePattern(%q) = %q, want %q", tc.folder, got, tc.want)
			}
		})
	}
}

// escapeSep mirrors what folderLikePattern does to the path separator: on
// Windows it is a backslash and therefore has to be escaped for LIKE too.
func escapeSep(sep string) string {
	if sep == `\` {
		return `\\`
	}
	return sep
}

// seedFolderScopeFixture lays down one in-scope folder, a true subfolder,
// and the two kinds of sibling that the old `prefix + "%"` matching
// wrongly swept in: one differing only by trailing characters
// ("2024 Backup"), and one that the `_` LIKE wildcard matched by accident.
func seedFolderScopeFixture(t *testing.T) (db *DB, base string) {
	t.Helper()
	db = openTestDB(t)
	base = filepath.Join("/lib")

	files := map[string]string{
		"in-scope":        filepath.Join(base, "2024", "a.jpg"),
		"true-subfolder":  filepath.Join(base, "2024", "2024_01", "b.jpg"),
		"sibling-space":   filepath.Join(base, "2024 Backup", "c.jpg"),
		"sibling-suffix":  filepath.Join(base, "2024Archive", "d.jpg"),
		"sibling-usc":     filepath.Join(base, "2024_01", "e.jpg"),
		"sibling-usc-alt": filepath.Join(base, "2024X01", "f.jpg"),
	}
	for hash, path := range files {
		must(t, db.EnsurePending(hash, 1, "image/jpeg", path, nil))
		must(t, db.UpsertFileSeen(path, hash, 0, 1))
	}
	return db, base
}

// TestResetStatusUnder_DoesNotTouchSiblingFolders proves the fix for
// `gpsync sync --force "C:\Photos\2024"` silently resetting -- and therefore
// re-uploading, burning quota -- entirely unrelated sibling folders.
// Without a trailing path separator, `LIKE '<prefix>%'` matched
// "2024 Backup" and "2024Archive" as if they were the same folder; and since
// `_` is LIKE's single-character wildcard, a scope of ".../2024_01"
// additionally matched ".../2024X01". A folder that genuinely IS a
// subdirectory must still be reset.
func TestResetStatusUnder_DoesNotTouchSiblingFolders(t *testing.T) {
	db, base := seedFolderScopeFixture(t)
	for _, h := range []string{"in-scope", "true-subfolder", "sibling-space", "sibling-suffix", "sibling-usc", "sibling-usc-alt"} {
		must(t, db.MarkUploaded(h, "media-"+h, ""))
	}

	n, err := db.ResetStatusUnder([]string{filepath.Join(base, "2024")})
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("reset %d rows, want 2 (the in-scope file and its true subfolder only)", n)
	}

	wantPending := map[string]bool{"in-scope": true, "true-subfolder": true}
	for _, h := range []string{"in-scope", "true-subfolder", "sibling-space", "sibling-suffix", "sibling-usc", "sibling-usc-alt"} {
		u, err := db.GetUpload(h)
		if err != nil {
			t.Fatal(err)
		}
		if wantPending[h] && u.Status != "pending" {
			t.Errorf("%s: status = %q, want pending (it is inside the scoped folder)", h, u.Status)
		}
		if !wantPending[h] && u.Status != "uploaded" {
			t.Errorf("%s: status = %q, want uploaded -- a SIBLING folder was reset, which would re-upload files the user never scoped", h, u.Status)
		}
	}
}

// TestResetStatusUnder_UnderscoreIsNotAWildcard scopes directly to an
// underscore-containing folder (this project's own `2024_01` naming) and
// proves it no longer matches `2024X01`.
func TestResetStatusUnder_UnderscoreIsNotAWildcard(t *testing.T) {
	db, base := seedFolderScopeFixture(t)
	must(t, db.MarkUploaded("sibling-usc", "media-usc", ""))
	must(t, db.MarkUploaded("sibling-usc-alt", "media-alt", ""))

	n, err := db.ResetStatusUnder([]string{filepath.Join(base, "2024_01")})
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("reset %d rows, want 1 -- `_` must be a literal, not LIKE's single-character wildcard", n)
	}
	alt, err := db.GetUpload("sibling-usc-alt")
	if err != nil {
		t.Fatal(err)
	}
	if alt.Status != "uploaded" {
		t.Errorf("2024X01 was reset by a scope of 2024_01: status = %q", alt.Status)
	}
}

// TestListPendingUnder_ScopesToTheFolderNotItsSiblings proves the same
// prefix fix on the read path `gpsync upload`/`gpsync sync` uses to pick work:
// an over-matching scope quietly pulls a sibling folder's whole backlog
// into a run the user scoped to one folder.
// TestSumPendingSizeUnder_ScopesLikeListPendingUnder proves the aggregate
// query (gpsync-tray's dashboard, wanting a CLI-parity "Transferred: X / Y
// bytes" line without fetching every row) matches ListPendingUnder's own
// scoping exactly -- same prefixes in, same rows counted -- and that
// non-pending rows and out-of-scope siblings are excluded from the sum.

// TestAllCaptureDatesAndUpdateCapturedAt proves `gpsync fix-dates`'s two raw
// building blocks: AllCaptureDates covers the whole ledger (regardless of
// status, matching AllSizes' own reasoning) with the sha256 needed to
// write a fix, and UpdateCapturedAt actually applies one without
// disturbing anything else about that row.
func TestAllCaptureDatesAndUpdateCapturedAt(t *testing.T) {
	db := openTestDB(t)
	wrong := 1700000000.0
	must(t, db.EnsurePending("h-pending", 100, "image/jpeg", "/lib/2002/a.jpg", &wrong))
	must(t, db.EnsurePending("h-uploaded", 200, "image/jpeg", "/lib/b.jpg", nil))
	must(t, db.MarkUploaded("h-uploaded", "media-1", ""))

	rows, err := db.AllCaptureDates()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("AllCaptureDates returned %d rows, want 2 (every status)", len(rows))
	}
	byHash := map[string]CaptureDateRow{}
	for _, r := range rows {
		byHash[r.SHA256] = r
	}
	if got := byHash["h-pending"]; got.Path != "/lib/2002/a.jpg" || !got.CapturedAt.Valid || got.CapturedAt.Float64 != wrong {
		t.Errorf("h-pending row = %+v, want path=/lib/2002/a.jpg captured_at=%v", got, wrong)
	}
	if got := byHash["h-uploaded"]; got.CapturedAt.Valid {
		t.Errorf("h-uploaded row = %+v, want no capture date", got)
	}

	fixed := 1015887545.0 // a real March-2002 timestamp
	if err := db.UpdateCapturedAt("h-pending", fixed); err != nil {
		t.Fatal(err)
	}
	row, err := db.GetUpload("h-pending")
	if err != nil {
		t.Fatal(err)
	}
	if !row.CapturedAt.Valid || row.CapturedAt.Float64 != fixed {
		t.Errorf("captured_at after UpdateCapturedAt = %+v, want %v", row.CapturedAt, fixed)
	}
	if row.Status != "pending" || row.Size != 100 {
		t.Errorf("UpdateCapturedAt disturbed other fields: status=%q size=%d, want pending/100 unchanged", row.Status, row.Size)
	}
}

// TestAllSizes_IncludesEveryStatusWithCaptureDate proves the Statistics
// page's raw data source covers the WHOLE ledger regardless of status
// (pending, uploaded, failed) -- unlike SumPendingSizeUnder, which is
// deliberately pending-only.
func TestAllSizes_IncludesEveryStatusWithCaptureDate(t *testing.T) {
	db := openTestDB(t)
	captured := 1700000000.0
	must(t, db.EnsurePending("h-pending", 100, "image/jpeg", "/lib/a.jpg", &captured))
	must(t, db.EnsurePending("h-uploaded", 200, "image/jpeg", "/lib/b.jpg", nil))
	must(t, db.MarkUploaded("h-uploaded", "media-1", ""))
	must(t, db.EnsurePending("h-failed", 300, "video/mp4", "/lib/c.mp4", nil))
	must(t, db.MarkFailed("h-failed", true, "UNSUPPORTED_EXTENSION", "bad format"))

	rows, err := db.AllSizes()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 {
		t.Fatalf("AllSizes returned %d rows, want 3 (every status, not just pending)", len(rows))
	}

	byPath := map[string]StatRow{}
	for _, r := range rows {
		byPath[r.Path] = r
	}
	if r := byPath["/lib/a.jpg"]; r.Size != 100 || !r.CapturedAt.Valid || r.CapturedAt.Float64 != captured {
		t.Errorf("/lib/a.jpg row = %+v, want size 100 and a valid captured_at of %v", r, captured)
	}
	if r := byPath["/lib/b.jpg"]; r.Size != 200 || r.CapturedAt.Valid {
		t.Errorf("/lib/b.jpg row = %+v, want size 200 and NO capture date", r)
	}
	if r := byPath["/lib/c.mp4"]; r.Size != 300 {
		t.Errorf("/lib/c.mp4 row = %+v, want size 300", r)
	}
}

func TestSumPendingSizeUnder_ScopesLikeListPendingUnder(t *testing.T) {
	db := openTestDB(t)
	base := filepath.Join("/lib", "2024")
	must(t, db.EnsurePending("in-scope-a", 100, "image/jpeg", filepath.Join(base, "a.jpg"), nil))
	must(t, db.EnsurePending("in-scope-b", 250, "image/jpeg", filepath.Join(base, "sub", "b.jpg"), nil))
	must(t, db.EnsurePending("sibling", 999, "image/jpeg", filepath.Join("/lib", "2024 Backup", "c.jpg"), nil))
	must(t, db.EnsurePending("already-uploaded", 500, "image/jpeg", filepath.Join(base, "d.jpg"), nil))
	must(t, db.MarkUploaded("already-uploaded", "media-1", ""))

	count, totalBytes, err := db.SumPendingSizeUnder([]string{base})
	if err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Errorf("count = %d, want 2 (sibling and already-uploaded rows excluded)", count)
	}
	if totalBytes != 350 {
		t.Errorf("totalBytes = %d, want 350 (100+250)", totalBytes)
	}

	// Empty prefixes: same "no scope" fallback as ListPendingUnder/ListPending.
	countAll, totalAll, err := db.SumPendingSizeUnder(nil)
	if err != nil {
		t.Fatal(err)
	}
	if countAll != 3 || totalAll != 1349 {
		t.Errorf("unscoped = (%d, %d), want (3, 1349) -- every pending row library-wide", countAll, totalAll)
	}
}

func TestListPendingUnder_ScopesToTheFolderNotItsSiblings(t *testing.T) {
	db, base := seedFolderScopeFixture(t)

	rows, err := db.ListPendingUnder([]string{filepath.Join(base, "2024")})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, r := range rows {
		got[r.SHA256] = true
	}
	if len(got) != 2 || !got["in-scope"] || !got["true-subfolder"] {
		t.Errorf("pending under .../2024 = %v, want exactly {in-scope, true-subfolder}", got)
	}
}

// TestSyncStatusAndDetailUnder_ScopeToTheFolderNotItsSiblings covers the
// two reporting queries behind `gpsync sync`'s "X of Y fully synced" line and
// its --verbose per-file listing -- both used the same over-matching
// prefix, so both inflated their numbers with sibling folders.
func TestSyncStatusAndDetailUnder_ScopeToTheFolderNotItsSiblings(t *testing.T) {
	db, base := seedFolderScopeFixture(t)
	scope := []string{filepath.Join(base, "2024")}

	total, byStatus, err := db.SyncStatusUnder(scope)
	if err != nil {
		t.Fatal(err)
	}
	if total != 2 {
		t.Errorf("SyncStatusUnder total = %d, want 2 (sibling folders must not be counted)", total)
	}
	if byStatus["pending"] != 2 {
		t.Errorf("SyncStatusUnder byStatus = %v, want 2 pending", byStatus)
	}

	details, err := db.SyncDetailUnder(scope)
	if err != nil {
		t.Fatal(err)
	}
	if len(details) != 2 {
		t.Fatalf("SyncDetailUnder returned %d rows, want 2: %+v", len(details), details)
	}
	for _, d := range details {
		if !strings.HasPrefix(d.Path, filepath.Join(base, "2024")+string(filepath.Separator)) {
			t.Errorf("SyncDetailUnder returned an out-of-scope path %q", d.Path)
		}
	}
}

// TestOpen_SetsBusyTimeout proves the DSN pragma actually took effect.
// SetMaxOpenConns(1) only serializes writers WITHIN one process; two gpsync
// processes (a scheduled `gpsync backup` alongside an interactive `gpsync sync`)
// hold separate SQLite handles, and with no busy_timeout the second one to
// want the lock fails instantly with "database is locked" instead of
// waiting the moment out.
func TestOpen_SetsBusyTimeout(t *testing.T) {
	db := openTestDB(t)
	var ms int
	if err := db.conn.QueryRow(`PRAGMA busy_timeout`).Scan(&ms); err != nil {
		t.Fatal(err)
	}
	if ms != 10000 {
		t.Errorf("PRAGMA busy_timeout = %d, want 10000 -- the DSN pragma did not apply", ms)
	}
}

// TestRunEnd_RecordsHowTheRunEnded covers the outcome vocabulary: a clean
// finish reads back as completed, and each early-stop reason is preserved
// along with its human-readable detail. Before this, every ending looked
// identical in the ledger -- `gpsync info` could not tell a run that finished
// from one that gave up on a throttle.
func TestRunEnd_RecordsHowTheRunEnded(t *testing.T) {
	cases := []struct {
		name       string
		end        func(db *DB) error
		wantReason string
		wantStatus string
		wantDetail string
	}{
		{
			name:       "clean finish",
			end:        func(db *DB) error { return db.RunFinish() },
			wantReason: EndReasonCompleted,
			wantStatus: "done",
		},
		{
			name: "gave up on a throttle",
			end: func(db *DB) error {
				return db.RunEnd(EndReasonAbortedThrottle, "gave up after 34m35s of escalating backoff across 7 steps — 44 file(s) left pending")
			},
			wantReason: EndReasonAbortedThrottle,
			wantStatus: "stopped",
			wantDetail: "gave up after 34m35s of escalating backoff across 7 steps — 44 file(s) left pending",
		},
		{
			name: "daily quota exhausted",
			end: func(db *DB) error {
				return db.RunEnd(EndReasonAbortedDailyQuota, "daily API quota exhausted (9850/10000 requests used today)")
			},
			wantReason: EndReasonAbortedDailyQuota,
			wantStatus: "stopped",
			wantDetail: "daily API quota exhausted (9850/10000 requests used today)",
		},
		{
			name: "interrupted",
			end: func(db *DB) error {
				return db.RunEnd(EndReasonInterrupted, "stopped by Ctrl+C after 12 of 400 file(s)")
			},
			wantReason: EndReasonInterrupted,
			wantStatus: "stopped",
			wantDetail: "stopped by Ctrl+C after 12 of 400 file(s)",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := openTestDB(t)
			must(t, db.RunStart("upload", 0, 400))

			// A run in progress has no outcome yet.
			run, err := db.RunGet()
			if err != nil {
				t.Fatal(err)
			}
			if run.Status != "running" || run.EndReason != "" {
				t.Fatalf("fresh run = status %q reason %q, want running with no reason yet", run.Status, run.EndReason)
			}

			must(t, tc.end(db))

			run, err = db.RunGet()
			if err != nil {
				t.Fatal(err)
			}
			if run.EndReason != tc.wantReason {
				t.Errorf("EndReason = %q, want %q", run.EndReason, tc.wantReason)
			}
			if run.Status != tc.wantStatus {
				t.Errorf("Status = %q, want %q", run.Status, tc.wantStatus)
			}
			if run.EndDetail != tc.wantDetail {
				t.Errorf("EndDetail = %q, want %q", run.EndDetail, tc.wantDetail)
			}
		})
	}
}

// TestRunStart_ClearsAPreviousOutcome: starting a new run must not leave
// the previous run's ending attached to it, or `gpsync info` would report a
// stale abort against a run that is currently going fine.
func TestRunStart_ClearsAPreviousOutcome(t *testing.T) {
	db := openTestDB(t)
	must(t, db.RunStart("upload", 0, 10))
	must(t, db.RunEnd(EndReasonAbortedThrottle, "gave up"))

	must(t, db.RunStart("upload", 0, 20))
	run, err := db.RunGet()
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != "running" {
		t.Errorf("Status = %q, want running", run.Status)
	}
	if run.EndReason != "" || run.EndDetail != "" {
		t.Errorf("new run inherited the previous outcome: reason=%q detail=%q", run.EndReason, run.EndDetail)
	}
}

// TestMigrateRunProgressEndReason_OldDatabaseStillReads proves an existing
// state.sqlite written by a gpsync version predating outcome tracking opens
// and reads cleanly: the columns are added in place, and the old row -- for
// which no outcome was ever recorded -- comes back with an EMPTY EndReason
// (NULL means "unset"), not an error and not a bogus value.
//
// This is the case the migration exists for, so it's built the way it
// actually occurs: a run_progress table created WITHOUT the new columns,
// populated, then opened by current gpsync.
func TestMigrateRunProgressEndReason_OldDatabaseStillReads(t *testing.T) {
	t.Setenv("GPSYNC_STATE_DIR", t.TempDir())
	StateDir = getStateDir()
	StateDBPath = filepath.Join(StateDir, "state.sqlite")
	if err := os.MkdirAll(StateDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Build the pre-migration table shape by hand.
	raw, err := sql.Open("sqlite", StateDBPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`
		CREATE TABLE run_progress (
		    id INTEGER PRIMARY KEY CHECK (id = 1),
		    run_type TEXT,
		    status TEXT,
		    folders_total INTEGER,
		    folders_done INTEGER,
		    files_total INTEGER,
		    files_done INTEGER,
		    started_at REAL,
		    updated_at REAL
		);
		INSERT INTO run_progress (id, run_type, status, folders_total, folders_done, files_total, files_done, started_at, updated_at)
		VALUES (1, 'upload', 'done', 3, 3, 120, 120, 1000, 2000);`); err != nil {
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	db, err := Open()
	if err != nil {
		t.Fatalf("opening a pre-migration database failed: %v", err)
	}
	defer db.Close()

	run, err := db.RunGet()
	if err != nil {
		t.Fatalf("reading a migrated row failed: %v", err)
	}
	if run == nil {
		t.Fatal("expected the pre-existing run row to survive the migration")
	}
	// Everything that was there before must be intact...
	if run.RunType != "upload" || run.Status != "done" || run.FilesDone != 120 || run.FilesTotal != 120 {
		t.Errorf("pre-existing row was corrupted by the migration: %+v", run)
	}
	// ...and the new columns read as unset, not as an error.
	if run.EndReason != "" || run.EndDetail != "" {
		t.Errorf("old row reports reason=%q detail=%q, want both empty (no outcome was ever recorded for it)", run.EndReason, run.EndDetail)
	}

	// And the migrated table is fully usable going forward.
	must(t, db.RunEnd(EndReasonInterrupted, "stopped by Ctrl+C after 5 of 10 file(s)"))
	run, err = db.RunGet()
	if err != nil {
		t.Fatal(err)
	}
	if run.EndReason != EndReasonInterrupted {
		t.Errorf("EndReason = %q, want %q after writing to the migrated table", run.EndReason, EndReasonInterrupted)
	}
}

// TestMigrateRunProgressEndReason_IsIdempotent: Open() runs the migration
// every time, so a second open of an already-migrated database must be a
// no-op rather than failing on a duplicate column.
func TestMigrateRunProgressEndReason_IsIdempotent(t *testing.T) {
	t.Setenv("GPSYNC_STATE_DIR", t.TempDir())
	StateDir = getStateDir()
	StateDBPath = filepath.Join(StateDir, "state.sqlite")

	for i := 0; i < 3; i++ {
		db, err := Open()
		if err != nil {
			t.Fatalf("open #%d failed: %v", i+1, err)
		}
		must(t, db.RunStart("upload", 0, 1))
		must(t, db.RunEnd(EndReasonCompleted, ""))
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

// TestDeleteUpload_RemovesOnlyTheTargetHash proves the surgical delete
// behind `gpsync recheck`: it clears exactly one stale record and leaves every
// sibling -- including ones in the same folder -- untouched. That scoping
// is the whole point, since the pre-existing alternative (ResetStatusUnder,
// via `gpsync sync --force`) can only work at folder-prefix scope and would
// disturb every already-synced file alongside it.
func TestDeleteUpload_RemovesOnlyTheTargetHash(t *testing.T) {
	db := openTestDB(t)
	must(t, db.EnsurePending("h-gone", 1, "image/jpeg", "/lib/2024/gone.jpg", nil))
	must(t, db.MarkFailed("h-gone", true, "UNSUPPORTED_EXTENSION", "nope"))
	must(t, db.EnsurePending("h-neighbour", 1, "image/jpeg", "/lib/2024/neighbour.jpg", nil))
	must(t, db.MarkUploaded("h-neighbour", "media-1", ""))
	must(t, db.EnsurePending("h-elsewhere", 1, "image/jpeg", "/lib/2023/other.jpg", nil))

	must(t, db.DeleteUpload("h-gone"))

	if u, err := db.GetUpload("h-gone"); err != nil || u != nil {
		t.Errorf("expected h-gone to be deleted: u=%+v err=%v", u, err)
	}
	// A file sitting in the very same folder must be completely unaffected.
	n, err := db.GetUpload("h-neighbour")
	if err != nil {
		t.Fatal(err)
	}
	if n == nil || n.Status != "uploaded" {
		t.Errorf("neighbour in the same folder was disturbed: %+v", n)
	}
	if e, err := db.GetUpload("h-elsewhere"); err != nil || e == nil {
		t.Errorf("unrelated row was removed: e=%+v err=%v", e, err)
	}

	// Deleting something already gone is a harmless no-op.
	must(t, db.DeleteUpload("h-gone"))
}

// TestUpdateFirstSourcePath_RepointsWithoutTouchingStatus proves the fix
// `gpsync duplicates resolve` relies on: repointing first_source_path at a
// surviving copy must be a pure path update, leaving status/attempt
// count/everything else untouched -- otherwise fixing "the old path is
// gone" would risk silently reviving a genuinely failed row or re-queuing
// something already uploaded.
func TestUpdateFirstSourcePath_RepointsWithoutTouchingStatus(t *testing.T) {
	db := openTestDB(t)
	must(t, db.EnsurePending("h1", 10, "image/jpeg", "/lib/2024/A/x.jpg", nil))

	must(t, db.UpdateFirstSourcePath("h1", "/lib/2024/B/x.jpg"))

	row, err := db.GetUpload("h1")
	if err != nil {
		t.Fatal(err)
	}
	if row == nil {
		t.Fatal("row disappeared")
	}
	if row.FirstSourcePath != "/lib/2024/B/x.jpg" {
		t.Errorf("first_source_path = %q, want /lib/2024/B/x.jpg", row.FirstSourcePath)
	}
	if row.Status != "pending" {
		t.Errorf("status = %q, want unchanged 'pending'", row.Status)
	}

	// Nonexistent hash: a harmless no-op, not an error.
	if err := db.UpdateFirstSourcePath("no-such-hash", "/anywhere.jpg"); err != nil {
		t.Errorf("UpdateFirstSourcePath on an unknown hash should be a no-op, got: %v", err)
	}
}

// TestResetFailedToPending_ScopedToOneHashAndOnlyFailures proves the other
// half: a single failed hash goes back in the queue with its recorded error
// cleared, while an already-uploaded row is never downgraded even if asked
// -- the status guard is what makes this safe to run over a whole failure
// list without risking re-uploading good work.
func TestResetFailedToPending_ScopedToOneHashAndOnlyFailures(t *testing.T) {
	db := openTestDB(t)
	must(t, db.EnsurePending("h-perm", 1, "image/jpeg", "/lib/2024/a.heic", nil))
	must(t, db.MarkFailed("h-perm", true, "UNSUPPORTED_EXTENSION", "'.heic' is a known-unsupported format"))
	must(t, db.EnsurePending("h-retry", 1, "image/jpeg", "/lib/2024/b.jpg", nil))
	must(t, db.MarkFailed("h-retry", false, "THROTTLE", "slow down"))
	must(t, db.EnsurePending("h-done", 1, "image/jpeg", "/lib/2024/c.jpg", nil))
	must(t, db.MarkUploaded("h-done", "media-1", ""))

	n, err := db.ResetFailedToPending("h-perm")
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("rows affected = %d, want 1", n)
	}

	got, err := db.GetUpload("h-perm")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "pending" {
		t.Errorf("status = %q, want pending", got.Status)
	}
	if got.LastErrorCode.Valid || got.LastErrorMessage.Valid {
		t.Errorf("the recorded error should have been cleared, got code=%v message=%v", got.LastErrorCode, got.LastErrorMessage)
	}

	// Siblings untouched.
	if r, _ := db.GetUpload("h-retry"); r == nil || r.Status != "failed_retryable" {
		t.Errorf("h-retry was disturbed: %+v", r)
	}
	if d, _ := db.GetUpload("h-done"); d == nil || d.Status != "uploaded" {
		t.Errorf("h-done was disturbed: %+v", d)
	}

	// An uploaded row must never be downgraded, even when targeted directly.
	n, err = db.ResetFailedToPending("h-done")
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("rows affected = %d, want 0 -- an uploaded row must never be reset back to pending", n)
	}
	if d, _ := db.GetUpload("h-done"); d == nil || d.Status != "uploaded" {
		t.Errorf("h-done status = %+v, want still uploaded", d)
	}
}

// TestListByStatus_ReturnsExactStatusOnly proves the general-purpose query
// added for BrowseFiles' uploaded/needs_review/ignored kinds returns
// exactly one status, unlike ListFailures (hard-scoped to the two failure
// statuses) which can't be reused for these.
func TestListByStatus_ReturnsExactStatusOnly(t *testing.T) {
	db := openTestDB(t)
	must(t, db.EnsurePending("h-pending", 10, "image/jpeg", "/lib/pending.jpg", nil))
	must(t, db.EnsurePending("h-uploaded", 10, "image/jpeg", "/lib/uploaded.jpg", nil))
	must(t, db.MarkUploaded("h-uploaded", "media-1", ""))
	must(t, db.EnsureNeedsReview("h-review", 10, "image/jpeg", "/originals/review.jpg", nil))

	got, err := db.ListByStatus("uploaded")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].SHA256 != "h-uploaded" {
		t.Errorf("ListByStatus(uploaded) = %+v, want exactly h-uploaded", got)
	}

	got, err = db.ListByStatus("needs_review")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].SHA256 != "h-review" {
		t.Errorf("ListByStatus(needs_review) = %+v, want exactly h-review", got)
	}
}

// TestMarkedSyncedAndRequeueForReupload covers the re-upload path: which
// rows count as "unknown remote quality", that scoping works, and that
// requeueing only touches those rows.
func TestMarkedSyncedAndRequeueForReupload(t *testing.T) {
	db := openTestDB(t)

	// OS-native paths: folderLikePattern builds its LIKE pattern with
	// filepath.Separator, so a hardcoded Windows path would never match
	// when this test runs on Linux (the same platform-shaped trap
	// originalsLikePattern was fixed for).
	dir2024 := filepath.Join("D:", "Photos", "2024")
	dir2025 := filepath.Join("D:", "Photos", "2025")

	// Uploaded BY gpsync -- known original quality, must never be touched.
	must(t, db.EnsurePending("real", 10, "image/jpeg", filepath.Join(dir2024, "a.jpg"), nil))
	must(t, db.MarkUploaded("real", "media-real", ""))
	// mark-synced: recorded as uploaded with no media item id. The
	// rclone-era rows this command exists for.
	must(t, db.EnsureMarkedSynced("ms1", 20, "image/jpeg", filepath.Join(dir2024, "b.jpg"), nil))
	must(t, db.EnsureMarkedSynced("ms2", 30, "image/jpeg", filepath.Join(dir2025, "c.jpg"), nil))
	// Still pending -- not a re-upload candidate either.
	must(t, db.EnsurePending("pend", 40, "image/jpeg", filepath.Join(dir2024, "d.jpg"), nil))

	// Nothing is a candidate until quality is RECORDED -- "gpsync did not
	// upload this" is not evidence of low quality, which was the original
	// bug: it would have re-sent a library of perfectly good originals.
	none, err := db.MarkedSyncedUnder(nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(none) != 0 {
		t.Fatalf("got %d candidates before any quality was recorded, want 0: %+v", len(none), none)
	}
	if _, err := db.SetRemoteQuality(QualityStorageSaver, []string{dir2024, dir2025}); err != nil {
		t.Fatal(err)
	}
	// The genuinely-uploaded row is in dir2024 too, so this also proves a
	// recorded quality is what matters, not who uploaded it.
	if _, err := db.SetRemoteQuality(QualityOriginal, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := db.SetRemoteQuality(QualityStorageSaver, []string{dir2024, dir2025}); err != nil {
		t.Fatal(err)
	}

	all, err := db.MarkedSyncedUnder(nil)
	if err != nil {
		t.Fatal(err)
	}
	// real + ms1 (dir2024) and ms2 (dir2025) are all now recorded
	// storage-saver; "pend" is not uploaded so it never qualifies.
	if len(all) != 3 {
		t.Fatalf("got %d candidates, want the 3 uploaded rows recorded storage-saver: %+v", len(all), all)
	}

	// Scoping by folder narrows it.
	scoped, err := db.MarkedSyncedUnder([]string{dir2025})
	if err != nil {
		t.Fatal(err)
	}
	if len(scoped) != 1 || scoped[0].SHA256 != "ms2" {
		t.Fatalf("scoped lookup = %+v, want just ms2", scoped)
	}

	n, err := db.RequeueForReupload([]string{"ms1", "ms2"})
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("requeued %d, want 2", n)
	}
	for _, sha := range []string{"ms1", "ms2"} {
		row, err := db.GetUpload(sha)
		if err != nil || row == nil {
			t.Fatal(err)
		}
		if row.Status != "pending" {
			t.Errorf("%s status = %q, want pending", sha, row.Status)
		}
	}
	// A file recorded as ORIGINAL must be untouched even if named
	// explicitly -- the WHERE clause, not the caller, is the guarantee.
	if _, err := db.SetRemoteQuality(QualityOriginal, nil); err != nil {
		t.Fatal(err)
	}
	must(t, db.EnsureMarkedSynced("ms3", 50, "image/jpeg", filepath.Join(dir2024, "e.jpg"), nil))
	if _, err := db.SetRemoteQuality(QualityOriginal, nil); err != nil {
		t.Fatal(err)
	}
	if n, err := db.RequeueForReupload([]string{"ms3"}); err != nil || n != 0 {
		t.Errorf("requeued a row recorded as ORIGINAL quality (n=%d, err=%v)", n, err)
	}
	row, err := db.GetUpload("ms3")
	if err != nil || row == nil {
		t.Fatal(err)
	}
	if row.Status != "uploaded" {
		t.Errorf("ms3 status = %q, want it left uploaded", row.Status)
	}
}

// TestSetRemoteQualityForPaths_ExactMatchNotPrefix covers why the exact
// form had to exist at all: `gpsync mark-synced --quality` used to run its
// raw arguments through folderLikePattern, which appends a separator and
// a wildcard. Give it a file glob and it builds "...\*.mp4\%", which
// matches nothing -- so quality was silently recorded for zero files in
// exactly the case file support was added to serve.
func TestSetRemoteQualityForPaths_ExactMatchNotPrefix(t *testing.T) {
	db := openTestDB(t)
	trip := filepath.Join("C:", "Photos", "2022", "SummerTrip")
	a := filepath.Join(trip, "a.mp4")
	b := filepath.Join(trip, "b.mp4")
	sibling := filepath.Join(trip, "untouched.jpg")
	for i, p := range []string{a, b, sibling} {
		h := fmt.Sprintf("h%d", i)
		must(t, db.EnsurePending(h, 10, "video/mp4", p, nil))
		must(t, db.MarkUploaded(h, fmt.Sprintf("media-%d", i), ""))
	}

	n, err := db.SetRemoteQualityForPaths("original", []string{a, b})
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("updated %d rows, want 2", n)
	}
	counts, err := db.RemoteQualityCounts()
	if err != nil {
		t.Fatal(err)
	}
	if counts["original"] != 2 {
		t.Errorf("original count = %d, want 2", counts["original"])
	}
	// The un-named sibling in the same folder must be untouched -- that is
	// the whole difference from the prefix form.
	if counts[""] != 1 {
		t.Errorf("unrecorded count = %d, want 1 (the sibling must not be swept in)", counts[""])
	}

	// A path nobody has uploaded updates nothing rather than erroring.
	n, err = db.SetRemoteQualityForPaths("original", []string{filepath.Join(trip, "ghost.mp4")})
	if err != nil || n != 0 {
		t.Errorf("unknown path: n=%d err=%v, want 0/nil", n, err)
	}
}

// TestSetRemoteQualityForPaths_ChunksPastTheParameterLimit: SQLite caps
// host parameters per statement (999 by default), and a glob can easily
// name more files than that, so the update is batched.
func TestSetRemoteQualityForPaths_ChunksPastTheParameterLimit(t *testing.T) {
	db := openTestDB(t)
	var paths []string
	for i := 0; i < 1500; i++ {
		p := filepath.Join("D:", "Photos", "many", fmt.Sprintf("f%04d.mp4", i))
		h := fmt.Sprintf("hash-%04d", i)
		must(t, db.EnsurePending(h, 1, "video/mp4", p, nil))
		must(t, db.MarkUploaded(h, fmt.Sprintf("m-%04d", i), ""))
		paths = append(paths, p)
	}
	n, err := db.SetRemoteQualityForPaths("storage-saver", paths)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1500 {
		t.Errorf("updated %d rows, want all 1500 -- the chunking must cover every batch", n)
	}
}

// qualityOf reads remote_quality directly -- it is not on the Upload
// struct, and adding it there would mean touching every SELECT that
// builds one for the sake of a test assertion.
func qualityOf(t *testing.T, db *DB, sha string) string {
	t.Helper()
	var q sql.NullString
	if err := db.conn.QueryRow(`SELECT remote_quality FROM uploads WHERE sha256=?`, sha).Scan(&q); err != nil {
		t.Fatal(err)
	}
	return q.String
}

// TestUnmarkSynced_RevertsBothKindsAndSaysWhich covers `gpsync
// mark-synced --unmark`, the escape hatch for "if i performed a mistake,
// allow to unmark".
//
// It reverts REAL gpsync uploads as well as mark-synced assertions. An
// earlier version refused the former, reasoning that a real upload is a
// receipt and re-sending is waste. That was wrong: Google merges a
// re-uploaded original over a storage-saver copy in place, so requeuing a
// file is the supported way to upgrade its quality -- "if i wish to
// revalidate and reupload a file, that's a way to do it". What the caller
// gets instead of a refusal is the BREAKDOWN, since re-sending is cheap
// for one file and ruinous for a library.
func TestUnmarkSynced_RevertsBothKindsAndSaysWhich(t *testing.T) {
	db := openTestDB(t)
	dir := filepath.Join("D:", "Photos", "2023")
	asserted := filepath.Join(dir, "marked.jpg")
	uploaded := filepath.Join(dir, "really-uploaded.jpg")

	must(t, db.EnsureMarkedSynced("h-marked", 10, "image/jpeg", asserted, nil))
	must(t, db.EnsurePending("h-real", 4096, "image/jpeg", uploaded, nil))
	must(t, db.MarkUploaded("h-real", "media-1", QualityOriginal))
	if _, err := db.SetRemoteQualityForPaths(QualityOriginal, []string{asserted}); err != nil {
		t.Fatal(err)
	}

	res, err := db.UnmarkSyncedUnder([]string{dir})
	if err != nil {
		t.Fatal(err)
	}
	if res.Asserted != 1 || res.Uploaded != 1 {
		t.Errorf("breakdown = %d asserted / %d uploaded, want 1/1", res.Asserted, res.Uploaded)
	}
	// The byte figure is what tells someone whether they just asked for a
	// one-file upgrade or a full re-send, so it has to count only the
	// rows that will actually go back over the wire.
	if res.UploadedBytes != 4096 {
		t.Errorf("UploadedBytes = %d, want 4096 (only the real upload's bytes)", res.UploadedBytes)
	}

	for _, sha := range []string{"h-marked", "h-real"} {
		got, err := db.GetUpload(sha)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status != "pending" {
			t.Errorf("%s status = %q, want pending", sha, got.Status)
		}
		if got.GoogleMediaItemID.Valid {
			t.Errorf("%s kept media item id %q -- it describes a copy this row no longer claims", sha, got.GoogleMediaItemID.String)
		}
		if q := qualityOf(t, db, sha); q != "" {
			t.Errorf("%s kept remote_quality %q after being unmarked", sha, q)
		}
	}

	// Nothing left to revert: a second run is a no-op, not a double count.
	again, err := db.UnmarkSyncedUnder([]string{dir})
	if err != nil {
		t.Fatal(err)
	}
	if again.Asserted != 0 || again.Uploaded != 0 {
		t.Errorf("second unmark reverted %+v, want nothing -- the rows are already pending", again)
	}
}

// TestMarkUploaded_RecordsTheQualityItSent closes a gap that kept
// reopening: remote_quality was only ever set by mark-synced/`gpsync
// quality`, so every file gpsync uploaded itself landed with the field
// empty and needed a manual backfill. A real library had drifted to 6,524
// unrecorded rows that way -- every one of them original, every one known
// to be original at the moment it was written.
func TestMarkUploaded_RecordsTheQualityItSent(t *testing.T) {
	db := openTestDB(t)
	must(t, db.EnsurePending("h1", 10, "image/jpeg", "/lib/a.jpg", nil))
	must(t, db.MarkUploaded("h1", "media-1", QualityFor("original")))
	if q := qualityOf(t, db, "h1"); q != QualityOriginal {
		t.Errorf("remote_quality = %q, want %q recorded at upload time", q, QualityOriginal)
	}

	// An empty quality means "genuinely unknown" and must leave the column
	// alone rather than writing a wrong value -- nothing can detect a wrong
	// verdict later, since the API exposes no storage tier.
	must(t, db.EnsurePending("h2", 10, "image/jpeg", "/lib/b.jpg", nil))
	must(t, db.MarkUploaded("h2", "media-2", ""))
	if q := qualityOf(t, db, "h2"); q != "" {
		t.Errorf("remote_quality = %q, want empty when the caller does not know", q)
	}
}

// TestQualityFor_MapsConfigVocabularyToLedgerVocabulary: config says
// "space_saver" (a local downscale setting), the ledger says
// "storage_saver" (what Google stores). Same idea, two spellings, and
// mark-synced now bridges them when --quality is omitted.
func TestQualityFor_MapsConfigVocabularyToLedgerVocabulary(t *testing.T) {
	for in, want := range map[string]string{
		"original":      QualityOriginal,
		"Original":      QualityOriginal,
		"space_saver":   QualityStorageSaver,
		"storage_saver": QualityStorageSaver,
		"storage-saver": QualityStorageSaver,
		"":              "",
		"nonsense":      "",
	} {
		if got := QualityFor(in); got != want {
			t.Errorf("QualityFor(%q) = %q, want %q", in, got, want)
		}
	}
}

// Folder-prefix lookups use idx_uploads_path instead of reading every row
// (review finding: ~8ms per folder, ~18s over a full 2,410-folder pass), and
// still match exactly the files under that folder.
func TestUploadsUnder_UsesPathIndexAndMatchesOnlyThatFolder(t *testing.T) {
	db := openTestDB(t)
	sep := string(filepath.Separator)
	folder := sep + filepath.Join("lib", "2022", "2022_07 Summer Trip")
	inside := filepath.Join(folder, "a.jpg")
	nested := filepath.Join(folder, "sub", "b.jpg")
	sibling := sep + filepath.Join("lib", "2022", "2022_07 Summer Trip 2", "c.jpg")
	underscore := sep + filepath.Join("lib", "2022", "2022X07 Summer Trip", "d.jpg") // "_" must not act as a wildcard
	for i, p := range []string{inside, nested, sibling, underscore} {
		if err := db.EnsurePending(fmt.Sprintf("h%d", i), 1, "image/jpeg", p, nil); err != nil {
			t.Fatal(err)
		}
	}

	rows, err := db.UploadsUnder([]string{folder})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, r := range rows {
		got[r.Path] = true
	}
	if len(got) != 2 || !got[inside] || !got[nested] {
		t.Errorf("UploadsUnder(%q) = %v, want exactly %s and %s", folder, got, inside, nested)
	}

	where, args := likeAnyPrefix("first_source_path", []string{folder})
	plan, err := db.conn.Query(`EXPLAIN QUERY PLAN SELECT sha256 FROM uploads WHERE `+where, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer plan.Close()
	var detail []string
	for plan.Next() {
		var id, parent, notused int
		var d string
		if err := plan.Scan(&id, &parent, &notused, &d); err != nil {
			t.Fatal(err)
		}
		detail = append(detail, d)
	}
	if !strings.Contains(strings.Join(detail, " | "), "idx_uploads_path") {
		t.Errorf("query plan %q does not use idx_uploads_path", detail)
	}
}
