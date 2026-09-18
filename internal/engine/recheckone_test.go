package engine

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gmizrahi/gpsync/internal/statedb"
)

func seedUpload(t *testing.T, db *statedb.DB, sha, path string) {
	t.Helper()
	if err := db.EnsurePending(sha, 123, "image/jpeg", path, nil); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertFileSeen(path, sha, 0, 123); err != nil {
		t.Fatal(err)
	}
}

func TestRecheckOne_FileOnDisk_ClearsMissing(t *testing.T) {
	db := openTestDB(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "photo.jpg")
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	seedUpload(t, db, "sha-present", path)
	if err := db.MarkMissing("sha-present", float64(time.Now().Unix())); err != nil {
		t.Fatal(err)
	}

	got, err := RecheckOne(db, "sha-present", time.Now())
	if err != nil {
		t.Fatalf("RecheckOne: %v", err)
	}
	if got != RecheckBackOnDisk {
		t.Errorf("outcome = %q, want %q", got, RecheckBackOnDisk)
	}
}

// Confirmed missing, never deleted: a drive that was merely unplugged must
// not lose its ledger rows.
func TestRecheckOne_FileGone_ConfirmsButKeepsTheRow(t *testing.T) {
	db := openTestDB(t)
	path := filepath.Join(t.TempDir(), "vanished.jpg")
	seedUpload(t, db, "sha-gone", path)

	got, err := RecheckOne(db, "sha-gone", time.Now())
	if err != nil {
		t.Fatalf("RecheckOne: %v", err)
	}
	if got != RecheckStillGone {
		t.Errorf("outcome = %q, want %q", got, RecheckStillGone)
	}
	u, err := db.GetUpload("sha-gone")
	if err != nil {
		t.Fatal(err)
	}
	if u == nil {
		t.Fatal("the row was deleted; recheck must only flag, never remove")
	}
}

// The hash comes from a client. An unknown one is reported, not silently
// treated as a no-op.
func TestRecheckOne_UnknownHash_IsReported(t *testing.T) {
	db := openTestDB(t)
	if _, err := RecheckOne(db, "no-such-hash", time.Now()); !errors.Is(err, ErrUnknownHash) {
		t.Fatalf("err = %v, want ErrUnknownHash", err)
	}
}

func TestForgetOne_DropsTheRowAndScanCacheButNotTheFile(t *testing.T) {
	db := openTestDB(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "keep-me.jpg")
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	seedUpload(t, db, "sha-forget", path)

	got, err := ForgetOne(db, "sha-forget")
	if err != nil {
		t.Fatalf("ForgetOne: %v", err)
	}
	if got != path {
		t.Errorf("path = %q, want %q", got, path)
	}
	u, err := db.GetUpload("sha-forget")
	if err != nil {
		t.Fatal(err)
	}
	if u != nil {
		t.Error("the ledger row survived")
	}
	// The scan cache must go too, or the next scan sees a cache hit for a
	// hash with no ledger row and never re-registers the file.
	if seen, err := db.GetFileSeen(path); err == nil && seen != nil {
		t.Error("the files_seen row survived; the file would never be re-registered")
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("the file on disk was touched: %v", err)
	}
}

func TestForgetOne_UnknownHash_IsReported(t *testing.T) {
	db := openTestDB(t)
	if _, err := ForgetOne(db, "no-such-hash"); !errors.Is(err, ErrUnknownHash) {
		t.Fatalf("err = %v, want ErrUnknownHash", err)
	}
}
