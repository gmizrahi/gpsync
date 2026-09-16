package engine

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gmizrahi/gpsync/internal/statedb"
)

// errCrossDevice stands in for the EXDEV a real cross-volume rename returns.
var errCrossDevice = errors.New("invalid cross-device link")

func writeTestFileContent(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func exists(t *testing.T, path string) bool {
	t.Helper()
	_, err := os.Stat(path)
	return err == nil
}

func TestTrashPath_IsFlatByBasename(t *testing.T) {
	if filepath.Separator == '\\' {
		got := TrashPath(`C:\Photos_Trash`, `C:\Photos\2024\2024_01 Trip\photo.jpg`)
		want := `C:\Photos_Trash\photo.jpg`
		if got != want {
			t.Errorf("TrashPath = %q, want %q", got, want)
		}
		return
	}
	got := TrashPath("/tmp/trash", "/mnt/c/Photos/2024/photo.jpg")
	want := "/tmp/trash/photo.jpg"
	if got != want {
		t.Errorf("TrashPath = %q, want %q", got, want)
	}

	// Two different files sharing a basename land at the SAME initial
	// candidate path now (that's the whole point of "flat") -- it's
	// UniqueTrashPath, called from MoveToTrash, that must then tell them
	// apart before anything is actually written.
	a := TrashPath("/tmp/trash", "/lib/2023/IMG_1.jpg")
	b := TrashPath("/tmp/trash", "/lib/2024/IMG_1.jpg")
	if a != b {
		t.Errorf("TrashPath = %q vs %q, want identical (basename-only) candidates for two same-named files", a, b)
	}
}

// TestMoveToTrash_FallsBackToCopyWhenRenameFails proves the cross-volume
// path. os.Rename cannot move between drives -- the common case here, since
// the trash folder is likely on a different disk from the photos -- so the
// fallback has to actually work, and must not lose the file if it doesn't.
func TestMoveToTrash_FallsBackToCopyWhenRenameFails(t *testing.T) {
	root, trash := t.TempDir(), t.TempDir()
	src := filepath.Join(root, "Photos", "photo.jpg")
	writeTestFileContent(t, src, "the-original-bytes")

	// Force the failure a second volume would produce.
	origRename := RenameFn
	RenameFn = func(string, string) error { return &os.LinkError{Op: "rename", Err: errCrossDevice} }
	defer func() { RenameFn = origRename }()

	dest, err := MoveToTrash(trash, src)
	if err != nil {
		t.Fatalf("cross-volume move failed: %v", err)
	}
	if dest != TrashPath(trash, src) {
		t.Errorf("dest = %q, want the mirrored path %q", dest, TrashPath(trash, src))
	}
	if exists(t, src) {
		t.Error("the original was left behind after a fallback move")
	}
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("trashed file unreadable: %v", err)
	}
	if string(got) != "the-original-bytes" {
		t.Errorf("trashed content = %q, want the original bytes intact", got)
	}
}

// TestMoveToTrash_DisambiguatesInsteadOfOverwriting: re-running resolve
// after an interrupted pass, or two distinct files mirroring to the same
// spot, must never clobber something already in the trash -- that would
// destroy the very copy the trash exists to preserve.
func TestMoveToTrash_DisambiguatesInsteadOfOverwriting(t *testing.T) {
	root, trash := t.TempDir(), t.TempDir()
	src := filepath.Join(root, "Photos", "photo.jpg")

	writeTestFileContent(t, src, "first-version")
	first, err := MoveToTrash(trash, src)
	if err != nil {
		t.Fatal(err)
	}

	// The same path is produced again (e.g. the file was restored and
	// resolve re-run).
	writeTestFileContent(t, src, "second-version")
	second, err := MoveToTrash(trash, src)
	if err != nil {
		t.Fatal(err)
	}

	if second == first {
		t.Fatalf("the second move reused %q and would have overwritten the first", first)
	}
	if !strings.HasSuffix(second, "_1.jpg") {
		t.Errorf("disambiguated name = %q, want a counter suffix keeping the extension", second)
	}
	// Both are intact, and neither was clobbered.
	for path, want := range map[string]string{first: "first-version", second: "second-version"} {
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("%s unreadable: %v", path, err)
		}
		if string(got) != want {
			t.Errorf("%s contains %q, want %q", path, got, want)
		}
	}
}

func TestExistingPaths_DropsPathsNoLongerOnDisk(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a.jpg")
	writeTestFileContent(t, a, "x")
	gone := filepath.Join(dir, "gone.jpg") // never created

	got := ExistingPaths([]string{a, gone})
	if len(got) != 1 || got[0] != a {
		t.Errorf("ExistingPaths = %v, want only %q", got, a)
	}
}

// TestResolveDuplicateGroup_MovesVictimsRepointsLedgerAndClearsScanCache is
// the real gpsync-tray Duplicate Resolver action: keep one path, trash the
// rest, repoint the ledger's first_source_path at the survivor, and clear
// the scan-cache row for each moved file.
func TestResolveDuplicateGroup_MovesVictimsRepointsLedgerAndClearsScanCache(t *testing.T) {
	db := openTestDB(t)
	dir, trash := t.TempDir(), t.TempDir()

	keep := filepath.Join(dir, "keep.jpg")
	victim1 := filepath.Join(dir, "victim1.jpg")
	victim2 := filepath.Join(dir, "victim2.jpg")
	for _, p := range []string{keep, victim1, victim2} {
		writeTestFileContent(t, p, "same-bytes")
	}
	mustNoErr(t, db.EnsurePending("hash-dup", 9, "image/jpeg", victim1, nil))
	mustNoErr(t, db.UpsertFileSeen(victim1, "hash-dup", 1, 9))
	mustNoErr(t, db.UpsertFileSeen(victim2, "hash-dup", 1, 9))

	moved, freed, err := ResolveDuplicateGroup(db, "hash-dup", keep, []string{keep, victim1, victim2}, 9, trash)
	if err != nil {
		t.Fatal(err)
	}
	if moved != 2 {
		t.Errorf("moved = %d, want 2", moved)
	}
	if freed != 18 {
		t.Errorf("freed = %d, want 18 (2 victims x size 9)", freed)
	}
	if exists(t, victim1) || exists(t, victim2) {
		t.Error("a victim was left in place instead of moved to the trash")
	}
	if !exists(t, keep) {
		t.Fatal("the kept file was moved -- it must stay exactly where it is")
	}

	row, err := db.GetUpload("hash-dup")
	if err != nil {
		t.Fatal(err)
	}
	if row.FirstSourcePath != keep {
		t.Errorf("first_source_path = %q, want repointed to the kept copy %q", row.FirstSourcePath, keep)
	}

	if seen, err := db.GetFileSeen(victim1); err != nil {
		t.Fatal(err)
	} else if seen != nil {
		t.Error("victim1's files_seen row should have been cleared")
	}
}

// TestResolveDuplicateGroup_KeptFileMissing_MovesNothing guards the same
// safety check applyDuplicateChoice (cmd/gpsync) has: if the chosen survivor
// has vanished since the group was listed, moving its siblings would leave
// NOTHING of this content in place -- so nothing is moved, and this is
// reported as a normal (err == nil), not exceptional, outcome.
func TestResolveDuplicateGroup_KeptFileMissing_MovesNothing(t *testing.T) {
	db := openTestDB(t)
	dir, trash := t.TempDir(), t.TempDir()
	victim := filepath.Join(dir, "victim.jpg")
	writeTestFileContent(t, victim, "x")
	missingKeep := filepath.Join(dir, "gone.jpg") // never created

	moved, freed, err := ResolveDuplicateGroup(db, "hash-x", missingKeep, []string{missingKeep, victim}, 1, trash)
	if err != nil {
		t.Fatalf("err = %v, want nil (a missing kept file is a normal no-op, not a failure)", err)
	}
	if moved != 0 || freed != 0 {
		t.Errorf("moved=%d freed=%d, want 0/0 -- nothing should move when the kept copy is gone", moved, freed)
	}
	if !exists(t, victim) {
		t.Error("the victim was moved even though the kept copy was missing -- this would leave NO copy of the content")
	}
}

// TestFindDuplicateGroupAndValidateKeep_ValidRequest_ReturnsTheRealGroup is
// the ordinary case: sha256 names a real group and keep names one of its
// real members.
func TestFindDuplicateGroupAndValidateKeep_ValidRequest_ReturnsTheRealGroup(t *testing.T) {
	groups := []statedb.DuplicateGroup{
		{SHA256: "hash-a", Size: 100, Paths: []string{"/lib/a1.jpg", "/lib/a2.jpg"}},
		{SHA256: "hash-b", Size: 200, Paths: []string{"/lib/b1.jpg", "/lib/b2.jpg"}},
	}
	group, ok := FindDuplicateGroupAndValidateKeep(groups, "hash-b", "/lib/b2.jpg")
	if !ok {
		t.Fatal("ok = false, want true for a real group/member combination")
	}
	if group.SHA256 != "hash-b" || group.Size != 200 {
		t.Errorf("group = %+v, want the real hash-b group", group)
	}
}

// TestFindDuplicateGroupAndValidateKeep_RejectsUnrelatedPath is the actual
// security fix under test: this is what used to be exploitable via
// handleDuplicatesResolve trusting a client-submitted paths list directly.
// keep names a real file, but one that belongs to a DIFFERENT group (or no
// group at all) than sha256 -- must be rejected, not silently accepted.
func TestFindDuplicateGroupAndValidateKeep_RejectsUnrelatedPath(t *testing.T) {
	groups := []statedb.DuplicateGroup{
		{SHA256: "hash-a", Size: 100, Paths: []string{"/lib/a1.jpg", "/lib/a2.jpg"}},
		{SHA256: "hash-b", Size: 200, Paths: []string{"/lib/b1.jpg", "/lib/b2.jpg"}},
	}
	// keep is a real path, but a member of hash-a's group, not hash-b's.
	if _, ok := FindDuplicateGroupAndValidateKeep(groups, "hash-b", "/lib/a1.jpg"); ok {
		t.Error("ok = true, want false -- keep belongs to a different group than sha256")
	}
	// keep isn't a member of ANY group -- e.g. an attacker naming an
	// arbitrary file on disk that happens to exist.
	if _, ok := FindDuplicateGroupAndValidateKeep(groups, "hash-b", "/etc/passwd"); ok {
		t.Error("ok = true, want false -- keep isn't a member of any real duplicate group")
	}
}

// TestFindDuplicateGroupAndValidateKeep_UnknownSHA256_Rejected covers a
// sha256 that doesn't match any CURRENT group at all -- e.g. one already
// resolved (and so no longer listed) by a stale/replayed form submission.
func TestFindDuplicateGroupAndValidateKeep_UnknownSHA256_Rejected(t *testing.T) {
	groups := []statedb.DuplicateGroup{
		{SHA256: "hash-a", Size: 100, Paths: []string{"/lib/a1.jpg", "/lib/a2.jpg"}},
	}
	if _, ok := FindDuplicateGroupAndValidateKeep(groups, "hash-does-not-exist", "/lib/a1.jpg"); ok {
		t.Error("ok = true, want false -- sha256 doesn't match any current group")
	}
}

// TestFindDuplicateGroupAndValidateKeep_EmptyInputs_Rejected guards the
// same "invalid submission" case handleDuplicatesResolve's original
// sha256==""/keep=="" check covered, now folded into this function.
func TestFindDuplicateGroupAndValidateKeep_EmptyInputs_Rejected(t *testing.T) {
	groups := []statedb.DuplicateGroup{
		{SHA256: "hash-a", Size: 100, Paths: []string{"/lib/a1.jpg", "/lib/a2.jpg"}},
	}
	if _, ok := FindDuplicateGroupAndValidateKeep(groups, "", "/lib/a1.jpg"); ok {
		t.Error("ok = true, want false for an empty sha256")
	}
	if _, ok := FindDuplicateGroupAndValidateKeep(groups, "hash-a", ""); ok {
		t.Error("ok = true, want false for an empty keep")
	}
}
