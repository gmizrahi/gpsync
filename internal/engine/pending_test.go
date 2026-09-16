package engine

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/gmizrahi/gpsync/internal/statedb"
)

func openTestDB(t *testing.T) *statedb.DB {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("GPSYNC_STATE_DIR", dir)
	statedb.StateDir = dir
	statedb.StateDBPath = filepath.Join(dir, "state.sqlite")
	db, err := statedb.Open()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func mustNoErr(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// TestPendingFolders_GroupsAndOrdersTheBacklog proves the data behind `gpsync
// pending`: only pending rows, grouped by containing folder, folders in
// path order and files within a folder in the order `gpsync upload` will
// actually work through them.
func TestPendingFolders_GroupsAndOrdersTheBacklog(t *testing.T) {
	db := openTestDB(t)
	root := filepath.Join(string(filepath.Separator), "lib")

	// Deliberately inserted out of order, to prove the ordering is real.
	type seed struct {
		hash, path string
		size       int64
		uploaded   bool
	}
	for _, s := range []seed{
		{"h5", filepath.Join(root, "2024", "b.jpg"), 200, false},
		{"h1", filepath.Join(root, "2023", "z.jpg"), 50, false},
		{"h4", filepath.Join(root, "2024", "a.jpg"), 100, false},
		{"h2", filepath.Join(root, "2023", "a.jpg"), 70, false},
		{"h9", filepath.Join(root, "2024", "already.jpg"), 999, true},
	} {
		mustNoErr(t, db.EnsurePending(s.hash, s.size, "image/jpeg", s.path, nil))
		if s.uploaded {
			mustNoErr(t, db.MarkUploaded(s.hash, "media-"+s.hash, ""))
		}
	}

	folders, err := PendingFolders(db, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(folders) != 2 {
		t.Fatalf("got %d folders, want 2: %+v", len(folders), folders)
	}

	if folders[0].Path != filepath.Join(root, "2023") {
		t.Errorf("first folder = %q, want the 2023 one (folders must be in path order)", folders[0].Path)
	}
	if folders[0].Files != 2 || folders[0].Bytes != 120 {
		t.Errorf("2023 = %d files / %d bytes, want 2 / 120", folders[0].Files, folders[0].Bytes)
	}
	// The uploaded file must not be counted anywhere.
	if folders[1].Files != 2 || folders[1].Bytes != 300 {
		t.Errorf("2024 = %d files / %d bytes, want 2 / 300 (already-uploaded content must be excluded)", folders[1].Files, folders[1].Bytes)
	}

	// Within a folder, files come out in the order they'll be uploaded.
	var names []string
	for _, r := range folders[1].Rows {
		names = append(names, filepath.Base(r.FirstSourcePath))
	}
	if strings.Join(names, ",") != "a.jpg,b.jpg" {
		t.Errorf("2024 rows = %v, want [a.jpg b.jpg] in upload order", names)
	}
}

// TestPendingFolders_ScopesToTheGivenFolders: the scope argument uses the
// same folder-prefix rules as `gpsync upload`, so a sibling folder that merely
// shares a name prefix must not be swept in.
func TestPendingFolders_ScopesToTheGivenFolders(t *testing.T) {
	db := openTestDB(t)
	root := filepath.Join(string(filepath.Separator), "lib")
	inScope := filepath.Join(root, "2024", "a.jpg")
	sibling := filepath.Join(root, "2024 Backup", "b.jpg")
	mustNoErr(t, db.EnsurePending("h1", 10, "image/jpeg", inScope, nil))
	mustNoErr(t, db.EnsurePending("h2", 10, "image/jpeg", sibling, nil))

	folders, err := PendingFolders(db, []string{filepath.Join(root, "2024")})
	if err != nil {
		t.Fatal(err)
	}
	if len(folders) != 1 || folders[0].Path != filepath.Join(root, "2024") {
		t.Fatalf("scoped result = %+v, want only the 2024 folder", folders)
	}
}

// After a throttle give-up every remaining file is failed_retryable and
// nothing is 'pending'. The heartbeat must still find that work.
func TestHeartbeatFolders_RequeuesRetryableFiles(t *testing.T) {
	db := openTestDB(t)
	path := filepath.Join(t.TempDir(), "2022_07 Summer Trip", "clip.mp4")
	if err := db.EnsurePending("h1", 100, "video/mp4", path, nil); err != nil {
		t.Fatal(err)
	}
	if err := db.MarkFailed("h1", false, "THROTTLE_BACKOFF_EXHAUSTED", "gave up after 1h0m0s of throttle backoff"); err != nil {
		t.Fatal(err)
	}

	folders, err := HeartbeatFolders(db, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(folders) != 1 || folders[0].Path != filepath.Dir(path) {
		t.Fatalf("HeartbeatFolders() = %+v, want the folder holding the retryable file", folders)
	}
	if u, err := db.GetUpload("h1"); err != nil || u == nil || u.Status != "pending" {
		t.Errorf("file status after the heartbeat = %+v (err %v), want pending", u, err)
	}
}
