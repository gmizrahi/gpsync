package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gmizrahi/gpsync/internal/statedb"
)

// recheckFixture is one source folder holding one of everything recheck
// judges, plus a healthy neighbour that must never be disturbed.
type recheckFixture struct {
	root, missing, unsupported, retryable, failedGone, neighbour string
}

// missingSize is deliberately unlike any file on disk in the fixture, so the
// "an unhashed same-size file could be this one, moved" rule never defers it.
const missingSize = 987654

func newRecheckFixture(t *testing.T, db *statedb.DB) recheckFixture {
	t.Helper()
	root := t.TempDir()
	f := recheckFixture{
		root:        root,
		missing:     filepath.Join(root, "a", "deleted.jpg"),
		unsupported: filepath.Join(root, "a", "slides.ppt"),
		retryable:   filepath.Join(root, "a", "one-off.jpg"),
		failedGone:  filepath.Join(root, "a", "failed-and-gone.jpg"),
		neighbour:   filepath.Join(root, "a", "fine.jpg"),
	}

	mustNoErr(t, db.EnsurePending("h-missing", missingSize, "image/jpeg", f.missing, nil))
	mustNoErr(t, db.MarkUploaded("h-missing", "media-missing", ""))
	mustNoErr(t, db.MarkMissing("h-missing", 1))
	mustNoErr(t, db.ConfirmMissing("h-missing", 2))

	writeTestFile(t, f.unsupported)
	mustNoErr(t, db.EnsurePending("h-unsupported", 1, "application/octet-stream", f.unsupported, nil))
	mustNoErr(t, db.MarkFailed("h-unsupported", true, "UNSUPPORTED_EXTENSION", "'.ppt' is a known-unsupported format"))

	writeTestFile(t, f.retryable)
	mustNoErr(t, db.EnsurePending("h-retryable", 1, "image/jpeg", f.retryable, nil))
	mustNoErr(t, db.MarkFailed("h-retryable", true, "PERMANENT_ERROR", "a one-off 400"))

	mustNoErr(t, db.EnsurePending("h-failed-gone", 1, "image/jpeg", f.failedGone, nil))
	mustNoErr(t, db.MarkFailed("h-failed-gone", true, "PERMANENT_ERROR", "corrupt"))

	writeTestFile(t, f.neighbour)
	mustNoErr(t, db.EnsurePending("h-fine", 1, "image/jpeg", f.neighbour, nil))
	mustNoErr(t, db.MarkUploaded("h-fine", "media-fine", ""))
	return f
}

func runRecheck(t *testing.T, db *statedb.DB, roots []string, opts recheckOptions) (recheckSummary, string, error) {
	t.Helper()
	var sum recheckSummary
	var err error
	out := captureStdout(t, func() { sum, err = recheck(db, roots, opts, time.Now()) })
	return sum, out, err
}

func statusOf(t *testing.T, db *statedb.DB, sha string) string {
	t.Helper()
	u, err := db.GetUpload(sha)
	if err != nil {
		t.Fatal(err)
	}
	if u == nil {
		return "<gone>"
	}
	return u.Status
}

// Without flags recheck is a report: every category is named with the flag
// that acts on it, and nothing in the ledger changes.
func TestRecheck_NoFlagsOnlyReports(t *testing.T) {
	db := openInfoTestDB(t)
	f := newRecheckFixture(t, db)

	_, out, err := runRecheck(t, db, []string{f.root}, recheckOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for sha, want := range map[string]string{
		"h-missing": "uploaded", "h-unsupported": "failed_permanent", "h-retryable": "failed_permanent",
		"h-failed-gone": "failed_permanent", "h-fine": "uploaded",
	} {
		if got := statusOf(t, db, sha); got != want {
			t.Errorf("%s status = %s, want %s untouched", sha, got, want)
		}
	}
	for _, want := range []string{"--missing", "--unsupported", "--retry", "deleted.jpg", "slides.ppt", "one-off.jpg"} {
		if !strings.Contains(out, want) {
			t.Errorf("report is missing %q:\n%s", want, out)
		}
	}
}

// --missing forgets confirmed-missing entries -- uploaded ones included, by
// the user's choice -- and nothing else.
func TestRecheck_MissingForgetsConfirmedEntriesOnly(t *testing.T) {
	db := openInfoTestDB(t)
	f := newRecheckFixture(t, db)

	sum, out, err := runRecheck(t, db, []string{f.root}, recheckOptions{missing: true})
	if err != nil {
		t.Fatal(err)
	}
	if got := statusOf(t, db, "h-missing"); got != "<gone>" {
		t.Errorf("confirmed-missing entry status = %s, want forgotten", got)
	}
	if sum.forgotten != 1 || sum.forgottenUploaded != 1 {
		t.Errorf("summary = %+v, want 1 forgotten, 1 of them uploaded", sum)
	}
	if !strings.Contains(out, "remain in Google Photos") {
		t.Errorf("output must say the uploaded copies stay in Google Photos:\n%s", out)
	}
	for sha, want := range map[string]string{"h-unsupported": "failed_permanent", "h-retryable": "failed_permanent", "h-failed-gone": "failed_permanent", "h-fine": "uploaded"} {
		if got := statusOf(t, db, sha); got != want {
			t.Errorf("--missing disturbed %s: status %s, want %s", sha, got, want)
		}
	}
}

// --missing stats every file again right before forgetting it. The resolve
// pass it runs first only covers entries under the source folders, so this is
// the only thing protecting an entry confirmed while its folder was still
// configured, whose file is present now.
func TestRecheck_MissingReChecksEachFileBeforeForgetting(t *testing.T) {
	db := openInfoTestDB(t)
	f := newRecheckFixture(t, db)
	elsewhere := filepath.Join(t.TempDir(), "no-longer-a-source-folder", "restored.jpg")
	mustNoErr(t, db.EnsurePending("h-restored", missingSize+1, "image/jpeg", elsewhere, nil))
	mustNoErr(t, db.MarkUploaded("h-restored", "media-restored", ""))
	mustNoErr(t, db.MarkMissing("h-restored", 1))
	mustNoErr(t, db.ConfirmMissing("h-restored", 2))
	writeTestFile(t, elsewhere) // back on disk since it was confirmed

	if _, _, err := runRecheck(t, db, []string{f.root}, recheckOptions{missing: true}); err != nil {
		t.Fatal(err)
	}
	if got := statusOf(t, db, "h-restored"); got != "uploaded" {
		t.Errorf("entry whose file is back = %s, want kept", got)
	}
	if got := statusOf(t, db, "h-missing"); got != "<gone>" {
		t.Errorf("genuinely missing entry = %s, want forgotten", got)
	}
}

// A file on an unplugged drive looks exactly like a deleted one, so --missing
// must refuse to forget anything while a source folder is unavailable.
func TestRecheck_MissingRefusesWhileASourceFolderIsUnavailable(t *testing.T) {
	db := openInfoTestDB(t)
	f := newRecheckFixture(t, db)

	_, _, err := runRecheck(t, db, []string{f.root, filepath.Join(t.TempDir(), "unplugged")}, recheckOptions{missing: true})
	if err == nil || !strings.Contains(err.Error(), "not available") {
		t.Fatalf("err = %v, want a refusal naming the unavailable folder", err)
	}
	if got := statusOf(t, db, "h-missing"); got != "uploaded" {
		t.Errorf("entry status = %s, want kept", got)
	}
}

// --revalidate undoes a confirmation when the file is back, e.g. a
// reattached drive.
func TestRecheck_RevalidateClearsFilesThatAreBack(t *testing.T) {
	db := openInfoTestDB(t)
	f := newRecheckFixture(t, db)
	writeTestFile(t, f.missing)

	sum, _, err := runRecheck(t, db, []string{f.root}, recheckOptions{revalidate: true})
	if err != nil {
		t.Fatal(err)
	}
	if sum.reappeared != 1 {
		t.Errorf("summary = %+v, want 1 back in place", sum)
	}
	flagged, err := db.MissingFlagged()
	if err != nil {
		t.Fatal(err)
	}
	state := map[string]bool{}
	for _, c := range flagged {
		state[c.SHA256] = true
	}
	if state["h-missing"] {
		t.Errorf("h-missing is still flagged after its file came back")
	}
	// The walk also flags entries it has not seen flagged before: the
	// failed entry whose file was never on disk.
	if !state["h-failed-gone"] {
		t.Errorf("h-failed-gone was not flagged; --revalidate must find files gone from the source folders")
	}
	if got := statusOf(t, db, "h-missing"); got != "uploaded" {
		t.Errorf("entry status = %s, want uploaded", got)
	}
}

// --unsupported is not --missing: it forgets only still-unsupported
// failures, and never the file itself.
func TestRecheck_UnsupportedForgetsOnlyUnsupportedFailures(t *testing.T) {
	db := openInfoTestDB(t)
	f := newRecheckFixture(t, db)

	if _, _, err := runRecheck(t, db, []string{f.root}, recheckOptions{unsupported: true}); err != nil {
		t.Fatal(err)
	}
	if got := statusOf(t, db, "h-unsupported"); got != "<gone>" {
		t.Errorf("unsupported failure status = %s, want forgotten", got)
	}
	if _, err := os.Stat(f.unsupported); err != nil {
		t.Errorf("the file itself must stay on disk: %v", err)
	}
	for sha, want := range map[string]string{"h-missing": "uploaded", "h-retryable": "failed_permanent", "h-failed-gone": "failed_permanent"} {
		if got := statusOf(t, db, sha); got != want {
			t.Errorf("--unsupported touched %s: status %s, want %s", sha, got, want)
		}
	}
}

// --retry queues supported-format failures whose file exists, and nothing else.
func TestRecheck_RetryQueuesOnlyRetryableFailures(t *testing.T) {
	db := openInfoTestDB(t)
	f := newRecheckFixture(t, db)

	if _, _, err := runRecheck(t, db, []string{f.root}, recheckOptions{retry: true}); err != nil {
		t.Fatal(err)
	}
	if got := statusOf(t, db, "h-retryable"); got != "pending" {
		t.Errorf("retryable failure status = %s, want pending", got)
	}
	for sha, want := range map[string]string{"h-unsupported": "failed_permanent", "h-failed-gone": "failed_permanent", "h-missing": "uploaded", "h-fine": "uploaded"} {
		if got := statusOf(t, db, sha); got != want {
			t.Errorf("--retry touched %s: status %s, want %s", sha, got, want)
		}
	}
}

// failed_retryable rows are requeued by every run already; recheck leaves them.
func TestRecheck_IgnoresRetryableStatus(t *testing.T) {
	db := openInfoTestDB(t)
	dir := t.TempDir()
	p := filepath.Join(dir, "a.jpg")
	writeTestFile(t, p)
	mustNoErr(t, db.EnsurePending("h-retry", 1, "image/jpeg", p, nil))
	mustNoErr(t, db.MarkFailed("h-retry", false, "THROTTLE", "slow down"))

	if _, _, err := runRecheck(t, db, []string{dir}, recheckOptions{missing: true, unsupported: true, retry: true}); err != nil {
		t.Fatal(err)
	}
	if got := statusOf(t, db, "h-retry"); got != "failed_retryable" {
		t.Errorf("status = %s, want failed_retryable untouched", got)
	}
}

func TestRecheck_NothingToDo(t *testing.T) {
	db := openInfoTestDB(t)
	dir := t.TempDir()
	_, out, err := runRecheck(t, db, []string{dir}, recheckOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Nothing to recheck") {
		t.Errorf("expected an explicit 'nothing to recheck', got:\n%s", out)
	}
}

func writeTestFile(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func mustNoErr(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
