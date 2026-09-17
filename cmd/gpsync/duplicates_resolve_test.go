package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fatih/color"

	"github.com/gmizrahi/gpsync/internal/statedb"
)

// dupFixture builds real files on disk and the DuplicateGroups that
// describe them, so the resolver operates on genuine paths it can stat and
// move.
type dupFixture struct {
	root   string
	trash  string
	groups []statedb.DuplicateGroup
}

func newDupFixture(t *testing.T) *dupFixture {
	t.Helper()
	return &dupFixture{root: t.TempDir(), trash: t.TempDir()}
}

// group creates one duplicate group: the same content written to every
// given relative path.
func (f *dupFixture) group(t *testing.T, sha string, size int64, rel ...string) {
	t.Helper()
	var abs []string
	for _, r := range rel {
		p := filepath.Join(f.root, filepath.FromSlash(r))
		writeTestFileContent(t, p, "content-"+sha)
		abs = append(abs, p)
	}
	f.groups = append(f.groups, statedb.DuplicateGroup{SHA256: sha, Size: size, Paths: abs})
}

func (f *dupFixture) path(rel string) string {
	return filepath.Join(f.root, filepath.FromSlash(rel))
}

func exists(t *testing.T, path string) bool {
	t.Helper()
	_, err := os.Stat(path)
	return err == nil
}

// runResolve drives the resolver with scripted answers and returns what it
// printed plus its summary.
func runResolve(t *testing.T, db *statedb.DB, f *dupFixture, dryRun bool, answers ...string) (string, dupSummary) {
	t.Helper()
	orig := color.NoColor
	color.NoColor = true
	defer func() { color.NoColor = orig }()

	in := strings.NewReader(strings.Join(answers, "\n") + "\n")
	var out bytes.Buffer
	sum := resolveDuplicates(db, f.groups, f.trash, dryRun, in, &out)
	return out.String(), sum
}

// trashedPath is where the fixture's trash dir mirrors a given source path.
func (f *dupFixture) trashedPath(rel string) string {
	return trashPath(f.trash, f.path(rel))
}

// TestResolveDuplicates_AutoApplyAcrossMatchingFolderSets covers the [A]
// mechanism end to end, which is the part that turns a wholesale-duplicated
// folder from a file-by-file slog into two answers.
//
// Three groups share the identical pair of folders. The user answers the
// first manually; [A] is then offered on the second (and only then -- there
// is nothing to auto-apply from before the first answer); answering A pins
// the FOLDER, and the third group resolves with no prompt at all.
func TestResolveDuplicates_AutoApplyAcrossMatchingFolderSets(t *testing.T) {
	db := openInfoTestDB(t)
	f := newDupFixture(t)
	f.group(t, "h1", 100, "Library/2024_01/a.mp4", "Copies/2024_01/a.mp4")
	f.group(t, "h2", 200, "Library/2024_01/b.mp4", "Copies/2024_01/b.mp4")
	f.group(t, "h3", 300, "Library/2024_01/c.mp4", "Copies/2024_01/c.mp4")

	// Answer group 1 by keeping the Library copy; then A; group 3 needs
	// no answer at all.
	out, sum := runResolve(t, db, f, false, "1", "A")

	if sum.resolved != 3 {
		t.Errorf("resolved = %d, want 3", sum.resolved)
	}
	if sum.autoApplied != 1 {
		t.Errorf("autoApplied = %d, want 1 (only the third group needed no prompt)", sum.autoApplied)
	}
	if sum.trashed != 3 || sum.bytesFreed != 600 {
		t.Errorf("trashed = %d files / %d bytes, want 3 / 600", sum.trashed, sum.bytesFreed)
	}

	// The [A] offer must have appeared exactly once -- on group 2, not group 1.
	if n := strings.Count(out, "A=keep"); n != 1 {
		t.Errorf("the [A] option was offered %d times, want exactly 1 (only once the previous group established a folder):\n%s", n, out)
	}
	if !strings.Contains(out, "A=keep "+f.path("Library/2024_01")) {
		t.Errorf("the [A] offer names the wrong folder:\n%s", out)
	}
	// Group 3 auto-resolved without a prompt.
	if n := strings.Count(out, "Keep which?"); n != 2 {
		t.Errorf("prompted %d times, want 2 (the third group must be automatic):\n%s", n, out)
	}
	if !strings.Contains(out, "auto:") {
		t.Errorf("the auto-resolved group printed no line -- it must not be silent:\n%s", out)
	}

	// Every kept copy is the pinned folder's; every other copy has moved to
	// the trash, mirrored under its original path.
	for _, rel := range []string{"Library/2024_01/a.mp4", "Library/2024_01/b.mp4", "Library/2024_01/c.mp4"} {
		if !exists(t, f.path(rel)) {
			t.Errorf("%s should have been KEPT", rel)
		}
	}
	for _, rel := range []string{"Copies/2024_01/a.mp4", "Copies/2024_01/b.mp4", "Copies/2024_01/c.mp4"} {
		if exists(t, f.path(rel)) {
			t.Errorf("%s should have left its original location", rel)
		}
		if !exists(t, f.trashedPath(rel)) {
			t.Errorf("%s did not arrive in the trash at %s", rel, f.trashedPath(rel))
		}
	}
}

// TestResolveDuplicates_FolderChangeClearsThePin proves the pin is scoped
// to the folder set it was made on. A group involving different folders
// must not inherit a decision made about entirely different folders --
// that would silently trash files from a folder the user never ruled on.
func TestResolveDuplicates_FolderChangeClearsThePin(t *testing.T) {
	db := openInfoTestDB(t)
	f := newDupFixture(t)
	f.group(t, "h1", 10, "A/x.jpg", "B/x.jpg")
	f.group(t, "h2", 10, "A/y.jpg", "B/y.jpg") // same set -> [A] offered
	f.group(t, "h3", 10, "C/z.jpg", "D/z.jpg") // different set -> pin clears
	f.group(t, "h4", 10, "C/w.jpg", "D/w.jpg") // new set, offered fresh

	// keep A/x, pin A, then the C/D group must prompt again (keep C/z),
	// then C/D repeats so [A] is offered afresh.
	out, sum := runResolve(t, db, f, false, "1", "A", "1", "A")

	if sum.resolved != 4 {
		t.Errorf("resolved = %d, want 4", sum.resolved)
	}
	// Group 3 must have been prompted, not auto-applied under the old pin.
	if n := strings.Count(out, "Keep which?"); n != 4 {
		t.Errorf("prompted %d times, want 4 -- a folder change must force a fresh question:\n%s", n, out)
	}
	// [A] offered twice: once for the A/B pair, once for the fresh C/D pair.
	if n := strings.Count(out, "A=keep"); n != 2 {
		t.Errorf("[A] offered %d times, want 2:\n%s", n, out)
	}
	// Nothing under B or D survives; nothing under A or C was touched.
	for _, rel := range []string{"A/x.jpg", "A/y.jpg", "C/z.jpg", "C/w.jpg"} {
		if !exists(t, f.path(rel)) {
			t.Errorf("%s should have been kept", rel)
		}
	}
	for _, rel := range []string{"B/x.jpg", "B/y.jpg", "D/z.jpg", "D/w.jpg"} {
		if exists(t, f.path(rel)) {
			t.Errorf("%s should have been moved to the trash", rel)
		}
		if !exists(t, f.trashedPath(rel)) {
			t.Errorf("%s is missing from the trash", rel)
		}
	}
}

// TestResolveDuplicates_SkipLeavesEverythingAndKeepsThePinIntact: 0 means
// "leave this one alone" and nothing more. It must not move anything, and
// it must not be read as a statement about which folder the user prefers.
func TestResolveDuplicates_SkipLeavesEverythingAndKeepsThePinIntact(t *testing.T) {
	db := openInfoTestDB(t)
	f := newDupFixture(t)
	f.group(t, "h1", 10, "A/x.jpg", "B/x.jpg")
	f.group(t, "h2", 10, "A/y.jpg", "B/y.jpg")

	out, sum := runResolve(t, db, f, false, "0", "0")

	if sum.skipped != 2 || sum.resolved != 0 || sum.trashed != 0 {
		t.Errorf("summary = %+v, want 2 skipped and nothing moved", sum)
	}
	for _, rel := range []string{"A/x.jpg", "B/x.jpg", "A/y.jpg", "B/y.jpg"} {
		if !exists(t, f.path(rel)) {
			t.Errorf("%s was moved by a skip", rel)
		}
	}
	// A skip establishes no folder preference, so [A] must never be offered
	// off the back of one.
	if strings.Contains(out, "A=keep") {
		t.Errorf("a skip was treated as a folder choice:\n%s", out)
	}
	if entries, _ := os.ReadDir(f.trash); len(entries) != 0 {
		t.Errorf("a skip put something in the trash: %v", entries)
	}
}

// TestResolveDuplicates_DryRunAsksTheSameQuestionsButDeletesNothing is the
// safety net the user is told to reach for first, so it must genuinely
// exercise the identical flow -- same prompts, same [A] logic -- while
// leaving every byte on disk.
func TestResolveDuplicates_DryRunAsksTheSameQuestionsButDeletesNothing(t *testing.T) {
	db := openInfoTestDB(t)
	f := newDupFixture(t)
	f.group(t, "h1", 100, "Library/a.mp4", "Copies/a.mp4")
	f.group(t, "h2", 100, "Library/b.mp4", "Copies/b.mp4")
	f.group(t, "h3", 100, "Library/c.mp4", "Copies/c.mp4")

	out, sum := runResolve(t, db, f, true, "1", "A")

	// Identical decisions to the real run.
	if sum.resolved != 3 || sum.autoApplied != 1 || sum.trashed != 3 {
		t.Errorf("summary = %+v, want the same decisions a real run would make", sum)
	}
	if !strings.Contains(out, "would trash:") {
		t.Errorf("dry run did not report what it would move:\n%s", out)
	}
	// It must also show WHERE each file would go, which is the thing worth
	// previewing.
	if !strings.Contains(out, f.trashedPath("Copies/a.mp4")) {
		t.Errorf("dry run did not show the trash destination:\n%s", out)
	}
	if strings.Contains(out, "  trashed:") {
		t.Errorf("dry run claimed to actually move something:\n%s", out)
	}
	if entries, _ := os.ReadDir(f.trash); len(entries) != 0 {
		t.Errorf("--dry-run wrote into the trash folder: %v", entries)
	}
	// Every single file must still be there.
	for _, rel := range []string{
		"Library/a.mp4", "Copies/a.mp4",
		"Library/b.mp4", "Copies/b.mp4",
		"Library/c.mp4", "Copies/c.mp4",
	} {
		if !exists(t, f.path(rel)) {
			t.Errorf("--dry-run moved %s", rel)
		}
	}
}

// TestResolveDuplicates_ThreeWayGroupOffersEveryCopy: groups aren't always
// pairs, and every copy has to be individually selectable.
func TestResolveDuplicates_ThreeWayGroupOffersEveryCopy(t *testing.T) {
	db := openInfoTestDB(t)
	f := newDupFixture(t)
	f.group(t, "h1", 50, "A/x.jpg", "B/x.jpg", "C/x.jpg")

	out, sum := runResolve(t, db, f, false, "2")

	if !strings.Contains(out, "3 copies") {
		t.Errorf("group header should say 3 copies:\n%s", out)
	}
	if !strings.Contains(out, "[1/2/3/0=skip]") {
		t.Errorf("prompt must offer all three copies plus skip:\n%s", out)
	}
	if sum.trashed != 2 {
		t.Errorf("trashed = %d, want 2 (the two not kept)", sum.trashed)
	}
	if !exists(t, f.path("B/x.jpg")) {
		t.Error("the chosen copy B/x.jpg was moved")
	}
	for _, rel := range []string{"A/x.jpg", "C/x.jpg"} {
		if exists(t, f.path(rel)) {
			t.Errorf("%s should have been moved to the trash", rel)
		}
	}
}

// TestResolveDuplicates_NeverDeletesWhenTheKeptCopyIsMissing is a guard the
// spec didn't call for but that a delete-files command needs: the listing
// comes from the ledger and can be stale, so if the copy the user chose has
// vanished since, deleting its siblings would destroy the content outright
// rather than deduplicate it.
func TestResolveDuplicates_NeverDeletesWhenTheKeptCopyIsMissing(t *testing.T) {
	db := openInfoTestDB(t)
	f := newDupFixture(t)
	f.group(t, "h1", 10, "A/x.jpg", "B/x.jpg", "C/x.jpg")

	// The user's chosen copy disappears between listing and decision.
	// (Choice [1] is A/x.jpg; remove it after the fixture is built but
	// before resolving -- existingPaths will drop it, so [1] now refers to
	// B/x.jpg. To exercise the guard itself, delete the file the resolver
	// will actually pick, after that filtering, via a group whose paths are
	// all present at listing time but not at deletion time.)
	orig := color.NoColor
	color.NoColor = true
	defer func() { color.NoColor = orig }()

	// Build the group by hand so every path is listed, then remove the one
	// that will be chosen.
	paths := f.groups[0].Paths
	chosen := paths[0]
	if err := os.Remove(chosen); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	moved, freed := applyDuplicateChoice(db, "h1", chosen, paths, 10, f.trash, false, &out, false)

	if moved != 0 || freed != 0 {
		t.Errorf("moved %d files (%d bytes) while the copy to keep was missing -- that relocates the only copies", moved, freed)
	}
	if !strings.Contains(out.String(), "no longer readable") {
		t.Errorf("expected an explicit explanation, got:\n%s", out.String())
	}
	for _, p := range paths[1:] {
		if !exists(t, p) {
			t.Errorf("%s was moved even though the kept copy was gone", p)
		}
	}
}

// TestResolveDuplicates_RepointsTheLedgerWhenTheKeptCopyIsntFirstSourcePath
// is the fix for a real bug: the ledger's first_source_path for a hash can
// be ANY of its duplicate copies (whichever was scanned first), not
// necessarily the one the user chooses to keep. If a still-pending hash's
// first_source_path names a copy that gets trashed, the next `gpsync upload`
// stats that now-missing path and marks the whole hash failed_permanent
// ("Source file no longer exists on disk") -- even though the content is
// sitting right there under the kept copy's path. Resolving a duplicate
// must repoint first_source_path at whichever copy survives.
func TestResolveDuplicates_RepointsTheLedgerWhenTheKeptCopyIsntFirstSourcePath(t *testing.T) {
	db := openInfoTestDB(t)
	f := newDupFixture(t)
	f.group(t, "h1", 10, "A/x.jpg", "B/x.jpg")

	// The ledger recorded A's copy first, and it's still pending upload --
	// exactly the state a duplicate group can be in before `gpsync upload` has
	// ever run against it.
	mustNoErr(t, db.EnsurePending("h1", 10, "image/jpeg", f.path("A/x.jpg"), nil))

	// Keep B (choice 2): A is the one that gets trashed.
	_, sum := runResolve(t, db, f, false, "2")
	if sum.trashed != 1 {
		t.Fatalf("sum.trashed = %d, want 1", sum.trashed)
	}
	if exists(t, f.path("A/x.jpg")) {
		t.Error("A/x.jpg should have been moved to the trash")
	}
	if !exists(t, f.path("B/x.jpg")) {
		t.Error("B/x.jpg (the kept copy) should still be in place")
	}

	row, err := db.GetUpload("h1")
	if err != nil {
		t.Fatal(err)
	}
	if row == nil {
		t.Fatal("ledger row for h1 disappeared")
	}
	if row.FirstSourcePath != f.path("B/x.jpg") {
		t.Errorf("first_source_path = %q, want the kept copy %q -- the next upload run would stat a trashed file",
			row.FirstSourcePath, f.path("B/x.jpg"))
	}
	if row.Status != "pending" {
		t.Errorf("status = %q, want unchanged 'pending' -- repointing the path must not touch upload status", row.Status)
	}
}

// TestResolveDuplicates_AmbiguousPinnedFolderPromptsInsteadOfGuessing is
// the other guard: [A] pins a FOLDER, so if a later group has two copies
// inside that same folder, "keep the pinned folder" doesn't say which file
// to keep. Guessing would delete a real file on a coin flip, so it must
// ask -- while leaving the pin in place, since the folder set still matches.
func TestResolveDuplicates_AmbiguousPinnedFolderPromptsInsteadOfGuessing(t *testing.T) {
	db := openInfoTestDB(t)
	f := newDupFixture(t)
	f.group(t, "h1", 10, "A/x.jpg", "B/x.jpg")
	f.group(t, "h2", 10, "A/y.jpg", "B/y.jpg")
	// Two copies in A, none in B: the folder set is {A} -- different from
	// {A,B} -- so the pin clears and this must be asked.
	f.group(t, "h3", 10, "A/dup1.jpg", "A/dup2.jpg")

	out, sum := runResolve(t, db, f, false, "1", "A", "1")

	if n := strings.Count(out, "Keep which?"); n != 3 {
		t.Errorf("prompted %d times, want 3 -- a group the pin cannot unambiguously answer must be asked:\n%s", n, out)
	}
	if sum.resolved != 3 {
		t.Errorf("resolved = %d, want 3", sum.resolved)
	}
	if !exists(t, f.path("A/dup1.jpg")) {
		t.Error("the chosen copy was moved")
	}
	if exists(t, f.path("A/dup2.jpg")) {
		t.Error("the unchosen copy stayed put")
	}
}

// TestResolveDuplicates_ClearsTheScanCacheForDeletedFiles: the files_seen
// row for a deleted path describes a file that no longer exists, so it goes
// with it.
func TestResolveDuplicates_ClearsTheScanCacheForDeletedFiles(t *testing.T) {
	db := openInfoTestDB(t)
	f := newDupFixture(t)
	f.group(t, "h1", 10, "A/x.jpg", "B/x.jpg")
	kept, gone := f.path("A/x.jpg"), f.path("B/x.jpg")
	mustNoErr(t, db.UpsertFileSeen(kept, "h1", 0, 10))
	mustNoErr(t, db.UpsertFileSeen(gone, "h1", 0, 10))

	runResolve(t, db, f, false, "1")

	if seen, err := db.GetFileSeen(gone); err != nil || seen != nil {
		t.Errorf("scan-cache row for the deleted %s survived: %+v (err=%v)", gone, seen, err)
	}
	if seen, err := db.GetFileSeen(kept); err != nil || seen == nil {
		t.Errorf("scan-cache row for the KEPT %s was removed: %+v (err=%v)", kept, seen, err)
	}
}

// TestResolveDuplicates_StopsCleanlyWhenInputRunsOut: a piped or truncated
// stdin must end the loop rather than spin on EOF, leaving whatever was
// already decided applied and the rest untouched.
func TestResolveDuplicates_StopsCleanlyWhenInputRunsOut(t *testing.T) {
	db := openInfoTestDB(t)
	f := newDupFixture(t)
	f.group(t, "h1", 10, "A/x.jpg", "B/x.jpg")
	f.group(t, "h2", 10, "A/y.jpg", "B/y.jpg")

	// Only one answer for two groups.
	_, sum := runResolve(t, db, f, false, "1")

	if sum.resolved != 1 {
		t.Errorf("resolved = %d, want 1", sum.resolved)
	}
	// The unanswered group is untouched.
	if !exists(t, f.path("A/y.jpg")) || !exists(t, f.path("B/y.jpg")) {
		t.Error("the group that was never answered had files moved")
	}
}

// TestResolveDuplicates_SkipsGroupsWithNothingLeftToResolve: a group whose
// redundant copies are already gone is not a duplicate any more.
func TestResolveDuplicates_SkipsGroupsWithNothingLeftToResolve(t *testing.T) {
	db := openInfoTestDB(t)
	f := newDupFixture(t)
	f.group(t, "h1", 10, "A/x.jpg", "B/x.jpg")
	if err := os.Remove(f.path("B/x.jpg")); err != nil {
		t.Fatal(err)
	}

	out, sum := runResolve(t, db, f, false)

	if sum.passedOver != 1 || sum.resolved != 0 {
		t.Errorf("summary = %+v, want the group passed over", sum)
	}
	if strings.Contains(out, "Keep which?") {
		t.Errorf("asked about a group with only one copy left:\n%s", out)
	}
	if !exists(t, f.path("A/x.jpg")) {
		t.Error("the sole remaining copy was moved")
	}
}

// TestTrashPath_MirrorsTheOriginalLocation covers the destination layout,
// including the Windows shape the user will actually see. Mirroring the
// original path is what makes the trash browsable ("where did this come
// from?") and what guarantees two groups sharing a basename can't collide.
// TestTrashPath_IsFlatByBasename proves the trash lands files directly in
// the configured folder (no recreated source tree) -- an earlier version
// mirrored the original path underneath the trash dir; the user found that
// tree annoying and asked for everything in one directory instead.
// Collision-proofing for same-named files from different folders is
// uniqueTrashPath's job, not trashPath's -- see
// TestMoveToTrash_DisambiguatesInsteadOfOverwriting for that.
// TestSortGroupsByFolder_ClustersMatchingFoldersAndKeepsSizeOrderWithinThem
// proves the fix for [A] resetting almost immediately in practice:
// DuplicateGroups' size-descending order scatters groups sharing a folder
// throughout the whole list, so consecutive groups rarely shared a folder
// set even when many matching ones existed elsewhere. After sorting, every
// group for a given folder set must be contiguous, and within one folder
// set the original largest-first relative order must survive (a stable
// sort, not an arbitrary reordering).
func TestSortGroupsByFolder_ClustersMatchingFoldersAndKeepsSizeOrderWithinThem(t *testing.T) {
	// Interleaved on input, exactly as DuplicateGroups' size ordering would
	// produce: A(big), B(big), A(small), B(small), A(medium).
	groups := []statedb.DuplicateGroup{
		{SHA256: "a-big", Size: 900, Paths: []string{"/x/a.jpg", "/y/a.jpg"}},
		{SHA256: "b-big", Size: 800, Paths: []string{"/m/b.jpg", "/n/b.jpg"}},
		{SHA256: "a-small", Size: 100, Paths: []string{"/x/c.jpg", "/y/c.jpg"}},
		{SHA256: "b-small", Size: 50, Paths: []string{"/m/d.jpg", "/n/d.jpg"}},
		{SHA256: "a-medium", Size: 500, Paths: []string{"/x/e.jpg", "/y/e.jpg"}},
	}
	sortGroupsByFolder(groups)

	var order []string
	for _, g := range groups {
		order = append(order, g.SHA256)
	}

	// Every "a-*" must be contiguous, every "b-*" must be contiguous.
	folderOf := func(sha string) string { return strings.SplitN(sha, "-", 2)[0] }
	seenFolders := map[string]bool{}
	for i, sha := range order {
		f := folderOf(sha)
		if i > 0 && folderOf(order[i-1]) != f && seenFolders[f] {
			t.Fatalf("folder %q appears again after another folder interrupted it -- not clustered: %v", f, order)
		}
		seenFolders[f] = true
	}

	// Within "a", the original big/small/medium relative order must survive.
	var aOrder []string
	for _, sha := range order {
		if folderOf(sha) == "a" {
			aOrder = append(aOrder, sha)
		}
	}
	wantAOrder := []string{"a-big", "a-small", "a-medium"}
	if strings.Join(aOrder, ",") != strings.Join(wantAOrder, ",") {
		t.Errorf("within-folder order = %v, want %v (stable sort must preserve it)", aOrder, wantAOrder)
	}
}

// TestResolveDuplicates_ReportsTheTrashDestination: the user needs to know
// where a file went, not just that it moved.
func TestResolveDuplicates_ReportsTheTrashDestination(t *testing.T) {
	db := openInfoTestDB(t)
	f := newDupFixture(t)
	f.group(t, "h1", 10, "A/x.jpg", "B/x.jpg")

	out, _ := runResolve(t, db, f, false, "1")
	if !strings.Contains(out, f.trashedPath("B/x.jpg")) {
		t.Errorf("output does not say where the trashed file went:\n%s", out)
	}
	if strings.Contains(strings.ToLower(out), "permanent") {
		t.Errorf("output still claims deletion is permanent:\n%s", out)
	}
}

// TestResolveDuplicates_AutoAppliesUnsuffixedCopyWithinOneFolder covers the
// shape the by-folder pin structurally cannot: every copy in ONE folder,
// distinguished only by Windows' copy/paste "_1"/"_2" markers.
//
// Real transcript that exposed the gap -- DSC05754.JPG/_1/_2/_3, then
// DSC05753.JPG/_1/_2/_3, both in the same folder. The user's answer was the
// same well-defined rule both times (keep the copy with no marker), but [A]
// was never offered: the folder set has one member, so the by-folder pin's
// candidate list was all four copies, which the ambiguity guard correctly
// refused to auto-resolve -- and therefore never offered either.
func TestResolveDuplicates_AutoAppliesUnsuffixedCopyWithinOneFolder(t *testing.T) {
	db := openInfoTestDB(t)
	f := newDupFixture(t)
	const dir = "Photos/2023_06 Lakeside"
	f.group(t, "h1", 100, dir+"/DSC05754.JPG", dir+"/DSC05754_1.JPG", dir+"/DSC05754_2.JPG", dir+"/DSC05754_3.JPG")
	f.group(t, "h2", 100, dir+"/DSC05753.JPG", dir+"/DSC05753_1.JPG", dir+"/DSC05753_2.JPG", dir+"/DSC05753_3.JPG")
	f.group(t, "h3", 100, dir+"/DSC05752.JPG", dir+"/DSC05752_1.JPG", dir+"/DSC05752_2.JPG")

	// Keep the unsuffixed copy manually, then A; the third group needs no answer.
	out, sum := runResolve(t, db, f, false, "1", "A")

	if sum.resolved != 3 {
		t.Errorf("resolved = %d, want 3", sum.resolved)
	}
	if sum.autoApplied != 1 {
		t.Errorf("autoApplied = %d, want 1 (the third group must need no prompt)", sum.autoApplied)
	}
	if n := strings.Count(out, "Keep which?"); n != 2 {
		t.Errorf("prompted %d times, want 2:\n%s", n, out)
	}
	// The offer must use the wording that fits this strategy -- naming a
	// folder would be meaningless when there is only one.
	if !strings.Contains(out, "A=keep the un-suffixed copy for all remaining files in this folder") {
		t.Errorf("the [A] offer does not describe the un-suffixed strategy:\n%s", out)
	}
	if strings.Contains(out, "A=keep "+f.path(dir)+" for") {
		t.Errorf("the [A] offer used the by-folder wording for a single-folder group:\n%s", out)
	}

	// Every unsuffixed original survives; every _N copy is gone.
	for _, rel := range []string{dir + "/DSC05754.JPG", dir + "/DSC05753.JPG", dir + "/DSC05752.JPG"} {
		if !exists(t, f.path(rel)) {
			t.Errorf("%s (the un-suffixed original) should have been kept", rel)
		}
	}
	for _, rel := range []string{
		dir + "/DSC05754_1.JPG", dir + "/DSC05754_2.JPG", dir + "/DSC05754_3.JPG",
		dir + "/DSC05753_1.JPG", dir + "/DSC05753_2.JPG", dir + "/DSC05753_3.JPG",
		dir + "/DSC05752_1.JPG", dir + "/DSC05752_2.JPG",
	} {
		if exists(t, f.path(rel)) {
			t.Errorf("%s (a _N copy) should have been trashed", rel)
		}
	}
	if sum.trashed != 8 {
		t.Errorf("trashed = %d, want 8", sum.trashed)
	}
}

// TestResolveDuplicates_UnsuffixedPinAsksWhenTheWinnerIsNotUnique: the pin
// says "keep the one without a copy marker", so a group where two copies
// carry no marker has no winner under that rule. Same fallback philosophy
// as the by-folder ambiguity guard -- ask rather than pick one at random.
func TestResolveDuplicates_UnsuffixedPinAsksWhenTheWinnerIsNotUnique(t *testing.T) {
	db := openInfoTestDB(t)
	f := newDupFixture(t)
	const dir = "Photos/2023_06 Lakeside"
	f.group(t, "h1", 10, dir+"/A.JPG", dir+"/A_1.JPG")
	f.group(t, "h2", 10, dir+"/B.JPG", dir+"/B_1.JPG")
	// A genuine tie: two copies, neither carrying a _N marker.
	f.group(t, "h3", 10, dir+"/C.JPG", dir+"/C-copy.JPG")
	// ...and then a resolvable one again, proving the pin survived the tie.
	f.group(t, "h4", 10, dir+"/D.JPG", dir+"/D_1.JPG")

	out, sum := runResolve(t, db, f, false, "1", "A", "1")

	// Groups 1 and 2 prompted, 3 prompted (tie), 4 auto-applied.
	if n := strings.Count(out, "Keep which?"); n != 3 {
		t.Errorf("prompted %d times, want 3 -- the tie must be asked, the last group must not be:\n%s", n, out)
	}
	if sum.autoApplied != 1 {
		t.Errorf("autoApplied = %d, want 1 (only the final group)", sum.autoApplied)
	}
	if sum.resolved != 4 {
		t.Errorf("resolved = %d, want 4", sum.resolved)
	}
	// The tie was decided by the user's answer, not by a guess.
	if !exists(t, f.path(dir+"/C.JPG")) {
		t.Error("the copy chosen for the tied group was trashed")
	}
	if exists(t, f.path(dir+"/C-copy.JPG")) {
		t.Error("the unchosen copy of the tied group survived")
	}
	// And the pin still worked afterwards.
	if !exists(t, f.path(dir+"/D.JPG")) || exists(t, f.path(dir+"/D_1.JPG")) {
		t.Error("the un-suffixed pin did not survive an intervening ambiguous group")
	}
}

// TestResolveDuplicates_UnsuffixedPinClearsOnFolderChange: the reset rule
// applies to this strategy exactly as it does to the by-folder one. A
// different folder is a different question.
func TestResolveDuplicates_UnsuffixedPinClearsOnFolderChange(t *testing.T) {
	db := openInfoTestDB(t)
	f := newDupFixture(t)
	f.group(t, "h1", 10, "Trips/A.JPG", "Trips/A_1.JPG")
	f.group(t, "h2", 10, "Trips/B.JPG", "Trips/B_1.JPG")
	f.group(t, "h3", 10, "Events/C.JPG", "Events/C_1.JPG") // different folder
	f.group(t, "h4", 10, "Events/D.JPG", "Events/D_1.JPG")

	out, sum := runResolve(t, db, f, false, "1", "A", "1", "A")

	// 1 manual, 2 A, 3 must prompt again (folder changed), 4 A.
	if n := strings.Count(out, "Keep which?"); n != 4 {
		t.Errorf("prompted %d times, want 4 -- a folder change must re-ask:\n%s", n, out)
	}
	if n := strings.Count(out, "A=keep the un-suffixed copy"); n != 2 {
		t.Errorf("[A] offered %d times, want 2 (once per folder):\n%s", n, out)
	}
	if sum.autoApplied != 0 {
		t.Errorf("autoApplied = %d, want 0 -- every group here was answered directly", sum.autoApplied)
	}
	for _, rel := range []string{"Trips/A.JPG", "Trips/B.JPG", "Events/C.JPG", "Events/D.JPG"} {
		if !exists(t, f.path(rel)) {
			t.Errorf("%s should have been kept", rel)
		}
	}
	for _, rel := range []string{"Trips/A_1.JPG", "Trips/B_1.JPG", "Events/C_1.JPG", "Events/D_1.JPG"} {
		if exists(t, f.path(rel)) {
			t.Errorf("%s should have been trashed", rel)
		}
	}
}

// TestUnsuffixedPaths_MarkerIsRelativeToTheGroup pins the rule that makes
// the un-suffixed strategy safe on real libraries: a "_N" tail only means
// "copy" when stripping it names a file actually present in the same group.
//
// Without that, "IMG_0042.jpg" -- an ordinary camera filename -- reads as a
// copy of a non-existent "IMG.jpg", and a folder of such names would have
// no un-suffixed candidate at all.
func TestUnsuffixedPaths_MarkerIsRelativeToTheGroup(t *testing.T) {
	cases := []struct {
		name  string
		paths []string
		want  []string
	}{
		{
			name:  "the reported case: one original, three copies",
			paths: []string{"/d/DSC05754.JPG", "/d/DSC05754_1.JPG", "/d/DSC05754_2.JPG", "/d/DSC05754_3.JPG"},
			want:  []string{"/d/DSC05754.JPG"},
		},
		{
			name:  "camera-style names are not copies of anything",
			paths: []string{"/d/IMG_0042.jpg", "/d/IMG_0042_1.jpg"},
			want:  []string{"/d/IMG_0042.jpg"},
		},
		{
			name:  "a whole group of camera-style names has no copies at all",
			paths: []string{"/d/IMG_0042.jpg", "/d/IMG_0043.jpg"},
			want:  []string{"/d/IMG_0042.jpg", "/d/IMG_0043.jpg"},
		},
		{
			name:  "hyphen is not the convention",
			paths: []string{"/d/photo.jpg", "/d/photo-1.jpg"},
			want:  []string{"/d/photo.jpg", "/d/photo-1.jpg"},
		},
		{
			name:  "trailing underscore with no digits is not a marker",
			paths: []string{"/d/photo.jpg", "/d/photo_.jpg"},
			want:  []string{"/d/photo.jpg", "/d/photo_.jpg"},
		},
		{
			name:  "works without extensions",
			paths: []string{"/d/noext", "/d/noext_2"},
			want:  []string{"/d/noext"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := unsuffixedPaths(tc.paths)
			if strings.Join(got, "|") != strings.Join(tc.want, "|") {
				t.Errorf("unsuffixedPaths(%v) = %v, want %v", tc.paths, got, tc.want)
			}
		})
	}
}

// TestPinFromChoice_PicksTheRightStrategy pins the mutual exclusivity the
// two strategies rely on: a single-folder group can only ever produce the
// un-suffixed pin, a multi-folder group only the by-folder pin, so no
// answer can ever qualify for both.
func TestPinFromChoice_PicksTheRightStrategy(t *testing.T) {
	d := filepath.Join(string(filepath.Separator), "d")
	d1 := filepath.Join(string(filepath.Separator), "d1")
	d2 := filepath.Join(string(filepath.Separator), "d2")
	oneFolder := []string{filepath.Join(d, "A.jpg"), filepath.Join(d, "A_1.jpg")}
	twoFolders := []string{filepath.Join(d1, "A.jpg"), filepath.Join(d2, "A.jpg")}

	if p := pinFromChoice(oneFolder, filepath.Join(d, "A.jpg"), folderSetKey(oneFolder)); p.kind != pinUnsuffixed {
		t.Errorf("single-folder group produced kind %v, want pinUnsuffixed", p.kind)
	}
	// Choosing a SUFFIXED copy states no reusable rule.
	if p := pinFromChoice(oneFolder, filepath.Join(d, "A_1.jpg"), folderSetKey(oneFolder)); p.kind != pinNone {
		t.Errorf("keeping a _N copy produced kind %v, want pinNone", p.kind)
	}
	if p := pinFromChoice(twoFolders, filepath.Join(d1, "A.jpg"), folderSetKey(twoFolders)); p.kind != pinFolder || p.folder != d1 {
		t.Errorf("multi-folder group produced %+v, want pinFolder on /d1", p)
	}
	// Multi-folder where the kept folder holds two copies: ambiguous, no pin.
	amb := []string{filepath.Join(d1, "A.jpg"), filepath.Join(d1, "A_1.jpg"), filepath.Join(d2, "A.jpg")}
	if p := pinFromChoice(amb, filepath.Join(d1, "A.jpg"), folderSetKey(amb)); p.kind != pinNone {
		t.Errorf("ambiguous multi-folder group produced kind %v, want pinNone", p.kind)
	}
}
