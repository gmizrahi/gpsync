package scanner

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gmizrahi/gpsync/internal/extensions"
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

func writeFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestScanFolders_StatPassRunsConcurrently proves the stat pass genuinely
// overlaps its os.Stat calls rather than walking files one at a time: before
// this, every file paid a sequential os.Stat plus a db.GetFileSeen lookup
// before any of them could be classified, which cost real minutes on a large
// library over a slow filesystem.
//
// It asserts on observed concurrency, not on elapsed time. A wall-clock
// budget made this test flaky under parallel load -- it measured how busy the
// machine was as much as whether the code overlapped anything.
func TestScanFolders_StatPassRunsConcurrently(t *testing.T) {
	const files, concurrency = 20, 5

	origStat := statFn
	t.Cleanup(func() { statFn = origStat })

	var inFlight, peak atomic.Int64
	statFn = func(name string) (os.FileInfo, error) {
		now := inFlight.Add(1)
		for {
			was := peak.Load()
			if now <= was || peak.CompareAndSwap(was, now) {
				break
			}
		}
		// Long enough that sequential calls could not overlap by accident,
		// short enough to keep the test quick.
		time.Sleep(20 * time.Millisecond)
		inFlight.Add(-1)
		return origStat(name)
	}

	root := t.TempDir()
	for i := 0; i < files; i++ {
		writeFile(t, filepath.Join(root, fmt.Sprintf("img_%02d.jpg", i)), []byte(fmt.Sprintf("content-%d", i)))
	}

	db := openTestDB(t)
	summary, err := ScanFolders(db, []string{root}, "manual_scan", concurrency, false, true, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if summary.FilesSeen != files {
		t.Fatalf("FilesSeen = %d, want %d", summary.FilesSeen, files)
	}
	if got := peak.Load(); got < 2 {
		t.Errorf("peak concurrent statFn calls = %d, want at least 2 at concurrency=%d -- the stat pass is running sequentially", got, concurrency)
	}
}

func TestScanFolders_DedupsAndCountsCorrectly(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "2024_03", "IMG_0001.jpg"), []byte("fake-jpeg-bytes-AAA"))
	writeFile(t, filepath.Join(root, "2024_03", "IMG_0002.jpg"), []byte("fake-jpeg-bytes-BBB"))
	// duplicate content, different name/folder -- should dedup to the same pending row
	writeFile(t, filepath.Join(root, "2024_07", "IMG_0001_copy.jpg"), []byte("fake-jpeg-bytes-AAA"))
	// known-unsupported extension -- gated out entirely before hashing, see
	// TestScanFolders_SkipsKnownUnsupportedExtensionsBeforeHashing.
	writeFile(t, filepath.Join(root, "2024_03", "IMG_0001.xmp"), []byte("xmp-data"))
	writeFile(t, filepath.Join(root, "2024_07", "mystery.xyz"), []byte("weird"))

	db := openTestDB(t)
	summary, err := ScanFolders(db, []string{root}, "manual_scan", 4, false, true, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}

	if summary.FilesSeen != 4 {
		t.Errorf("FilesSeen = %d, want 4 (the .xmp is gated out, not counted)", summary.FilesSeen)
	}
	if summary.FilesHashed != 4 {
		t.Errorf("FilesHashed = %d, want 4 (all new)", summary.FilesHashed)
	}
	if summary.NewPending != 3 {
		t.Errorf("NewPending = %d, want 3 (duplicate content collapses to one)", summary.NewPending)
	}
	if summary.Skipped != 1 {
		t.Errorf("Skipped = %d, want 1 (the .xmp)", summary.Skipped)
	}
	if summary.Folders != 2 {
		t.Errorf("Folders = %d, want 2", summary.Folders)
	}

	pending, err := db.ListPending()
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 3 {
		t.Fatalf("pending rows = %d, want 3", len(pending))
	}
}

// TestScanFolders_SkipsKnownUnsupportedExtensionsBeforeHashing proves the
// fix for a real problem: gating unsupported extensions only at upload
// time meant every .ppt/.pdf/.xmp/etc. in a library got hashed and given a
// ledger row on EVERY scan, just to be marked failed_permanent on the next
// upload -- and `gpsync recheck --unsupported` cleaning up that record
// would get silently undone by the very next scan re-registering it. Gating
// at scan time means the file never gets a ledger row (or even a hash) in
// the first place.
func TestScanFolders_SkipsKnownUnsupportedExtensionsBeforeHashing(t *testing.T) {
	root := t.TempDir()
	ppt := filepath.Join(root, "slides.ppt")
	writeFile(t, ppt, []byte("not-a-photo"))
	writeFile(t, filepath.Join(root, "photo.jpg"), []byte("fake-jpeg-bytes"))

	var skips []string
	db := openTestDB(t)
	summary, err := ScanFolders(db, []string{root}, "manual_scan", 4, false, true, func(path, reason string) {
		skips = append(skips, path+": "+reason)
	}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}

	if summary.FilesSeen != 1 {
		t.Errorf("FilesSeen = %d, want 1 (only photo.jpg)", summary.FilesSeen)
	}
	if summary.Skipped != 1 {
		t.Errorf("Skipped = %d, want 1", summary.Skipped)
	}
	if len(skips) != 1 || !strings.Contains(skips[0], "ppt") {
		t.Errorf("onSkip should have fired once for the .ppt naming it as unsupported, got: %v", skips)
	}

	seen, err := db.GetFileSeen(ppt)
	if err != nil {
		t.Fatal(err)
	}
	if seen != nil {
		t.Errorf(".ppt should never have been hashed/cached at all: %+v", seen)
	}

	pending, err := db.ListPending()
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 {
		t.Fatalf("pending rows = %d, want 1 (only photo.jpg)", len(pending))
	}
}

func TestScanFolders_RescanIsNoOpCacheHit(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "a.jpg"), []byte("content-a"))
	writeFile(t, filepath.Join(root, "b.jpg"), []byte("content-b"))

	db := openTestDB(t)
	if _, err := ScanFolders(db, []string{root}, "manual_scan", 4, false, true, nil, nil, nil); err != nil {
		t.Fatal(err)
	}

	summary, err := ScanFolders(db, []string{root}, "manual_scan", 4, false, true, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if summary.FilesHashed != 0 {
		t.Errorf("FilesHashed on rescan = %d, want 0 (cache-hit)", summary.FilesHashed)
	}
	if summary.NewPending != 0 {
		t.Errorf("NewPending on rescan = %d, want 0", summary.NewPending)
	}
}

func TestScanFolders_DetectsChangedFile(t *testing.T) {
	root := t.TempDir()
	p := filepath.Join(root, "a.jpg")
	writeFile(t, p, []byte("version-1"))

	db := openTestDB(t)
	if _, err := ScanFolders(db, []string{root}, "manual_scan", 4, false, true, nil, nil, nil); err != nil {
		t.Fatal(err)
	}

	// Modify content -- must be picked up as a hash change on rescan.
	writeFile(t, p, []byte("version-2-longer-content"))

	summary, err := ScanFolders(db, []string{root}, "manual_scan", 4, false, true, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if summary.FilesHashed != 1 {
		t.Errorf("FilesHashed after content change = %d, want 1", summary.FilesHashed)
	}
}

func TestScanFolders_MarkSyncedRegistersAsUploadedNoNetwork(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "a.jpg"), []byte("content-a"))
	writeFile(t, filepath.Join(root, "b.jpg"), []byte("content-b"))

	db := openTestDB(t)
	summary, err := ScanFolders(db, []string{root}, "mark_synced", 4, true, true, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if summary.NewPending != 2 {
		t.Fatalf("NewPending = %d, want 2 (reused as 'newly marked' count)", summary.NewPending)
	}

	pending, err := db.ListPending()
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Errorf("expected 0 pending rows after mark-synced, got %d", len(pending))
	}

	counts, err := db.CountsByStatus()
	if err != nil {
		t.Fatal(err)
	}
	if counts["uploaded"] != 2 {
		t.Errorf("counts = %+v, want 2 uploaded", counts)
	}
}

// TestScanFolders_FileInOriginalsFolder_GoesToNeedsReviewNotPending covers
// the scan-time rule end-to-end: a file inside an "originals" folder must
// never be auto-queued, since it is usually a
// pre-edit backup of a file with the SAME NAME (but different bytes,
// hence a different hash) already tracked elsewhere -- auto-uploading it
// produced pure duplicate noise in Google Photos.
func TestScanFolders_FileInOriginalsFolder_GoesToNeedsReviewNotPending(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "IMG_1234.jpg"), []byte("edited version"))
	writeFile(t, filepath.Join(root, "originals", "IMG_1234.jpg"), []byte("pre-edit original, different bytes"))

	db := openTestDB(t)
	summary, err := ScanFolders(db, []string{root}, "scan", 4, false, true, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if summary.FilesSeen != 2 {
		t.Fatalf("FilesSeen = %d, want 2 -- the originals-folder file is still discovered and hashed, just not queued", summary.FilesSeen)
	}

	counts, err := db.CountsByStatus()
	if err != nil {
		t.Fatal(err)
	}
	if counts["pending"] != 1 {
		t.Errorf("counts[pending] = %d, want 1 (the edited version only)", counts["pending"])
	}
	if counts["needs_review"] != 1 {
		t.Errorf("counts[needs_review] = %d, want 1 (the originals-folder copy)", counts["needs_review"])
	}
}

func TestScanFolders_MarkSyncedDoesNotAffectFutureNewFiles(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "a.jpg"), []byte("content-a"))

	db := openTestDB(t)
	if _, err := ScanFolders(db, []string{root}, "mark_synced", 4, true, true, nil, nil, nil); err != nil {
		t.Fatal(err)
	}

	// A file added later must scan as pending normally, not get swept into "already synced".
	writeFile(t, filepath.Join(root, "b.jpg"), []byte("content-b"))
	if _, err := ScanFolders(db, []string{root}, "manual_scan", 4, false, true, nil, nil, nil); err != nil {
		t.Fatal(err)
	}

	pending, err := db.ListPending()
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || filepath.Base(pending[0].FirstSourcePath) != "b.jpg" {
		t.Errorf("expected exactly b.jpg pending, got %+v", pending)
	}
}

func TestExpandFolders_SkipsPicasaJunkAndReportsIt(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "IMG_0001.jpg"), []byte("real photo"))
	writeFile(t, filepath.Join(root, "picasa.ini"), []byte("junk"))
	writeFile(t, filepath.Join(root, "Thumbs.db"), []byte("junk"))
	writeFile(t, filepath.Join(root, ".picasaoriginals", "IMG_0001.jpg"), []byte("pre-edit backup, should be pruned"))

	var skipped []string
	paths, err := ExpandFolders([]string{root}, func(path, reason string) {
		skipped = append(skipped, path)
	})
	if err != nil {
		t.Fatal(err)
	}

	if len(paths) != 1 || filepath.Base(paths[0]) != "IMG_0001.jpg" {
		t.Fatalf("expected only the real photo, got %v", paths)
	}
	if len(skipped) != 3 {
		t.Fatalf("expected 3 skip callbacks (picasa.ini, Thumbs.db, .picasaoriginals dir), got %d: %v", len(skipped), skipped)
	}
}

// TestIsInOriginalsFolder_MatchesImmediateParentCaseInsensitive covers the
// detection itself: files inside a plain, visible "originals" folder
// (distinct from the already-pruned hidden ".picasaoriginals") must never
// be auto-queued.
func TestIsInOriginalsFolder_MatchesImmediateParentCaseInsensitive(t *testing.T) {
	cases := []struct {
		path string
		want bool
	}{
		{filepath.Join("D:", "Photos", "2013", "originals", "a.jpg"), true},
		{filepath.Join("D:", "Photos", "2013", "Originals", "a.jpg"), true},
		{filepath.Join("D:", "Photos", "2013", "ORIGINALS", "a.jpg"), true},
		{filepath.Join("D:", "Photos", "2013", "a.jpg"), false},
		// Only the IMMEDIATE parent counts -- an ancestor named "originals"
		// several levels up must not match.
		{filepath.Join("D:", "Photos", "originals", "2013", "a.jpg"), false},
		// A folder that merely CONTAINS "originals" as a substring, not an
		// exact segment match, must not match either.
		{filepath.Join("D:", "Photos", "2013", "Originals Backup", "a.jpg"), false},
	}
	for _, c := range cases {
		if got := IsInOriginalsFolder(c.path); got != c.want {
			t.Errorf("IsInOriginalsFolder(%q) = %v, want %v", c.path, got, c.want)
		}
	}
}

// TestSiblingPath_SkipsPastTheOriginalsSegment is the fix for a real,
// direct correction to the review workflow: an immediate-parent sibling
// match is unambiguous ("resolution is clear - ignore it"), so this must
// compute the EXACT expected location, not just a basename to search for.
func TestSiblingPath_SkipsPastTheOriginalsSegment(t *testing.T) {
	got := SiblingPath(filepath.Join("C:", "Photos", "2013_08 Lakeside", "originals", "IMG_1234.jpg"))
	want := filepath.Join("C:", "Photos", "2013_08 Lakeside", "IMG_1234.jpg")
	if got != want {
		t.Errorf("SiblingPath = %q, want %q", got, want)
	}
}

// TestExpandFolders_AppliesUserConfiguredIgnoreOverrides proves the
// config.toml integration: a file/directory name the user adds via
// ApplyUserIgnoreOverrides is skipped exactly like a built-in one, and a
// name NOT in either list still passes through.
func TestExpandFolders_AppliesUserConfiguredIgnoreOverrides(t *testing.T) {
	t.Cleanup(func() { ApplyUserIgnoreOverrides(nil, nil) })

	root := t.TempDir()
	writeFile(t, filepath.Join(root, "IMG_0001.jpg"), []byte("real photo"))
	writeFile(t, filepath.Join(root, "CustomJunk.log"), []byte("junk"))
	writeFile(t, filepath.Join(root, "CustomIgnoreDir", "hidden.jpg"), []byte("should be pruned"))

	ApplyUserIgnoreOverrides([]string{"customjunk.log"}, []string{"CUSTOMIGNOREDIR"})

	paths, err := ExpandFolders([]string{root}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 1 || filepath.Base(paths[0]) != "IMG_0001.jpg" {
		t.Fatalf("expected only the real photo, got %v", paths)
	}
}

// TestExpandFolders_MediaKindFilter_RestrictsToOneKind proves --media-type
// gating happens at the SAME scan-time gate as unsupported extensions:
// files of the other kind, and files of an unrecognized kind, are both
// excluded when a filter is active -- "photos only" means only confirmed
// photos, not merely "not confirmed video".
func TestExpandFolders_MediaKindFilter_RestrictsToOneKind(t *testing.T) {
	t.Cleanup(func() { SetMediaKindFilter(extensions.KindUnknown) })

	root := t.TempDir()
	writeFile(t, filepath.Join(root, "photo.jpg"), []byte("photo"))
	writeFile(t, filepath.Join(root, "clip.mp4"), []byte("video"))
	writeFile(t, filepath.Join(root, "mystery.xyz"), []byte("unknown kind"))

	SetMediaKindFilter(extensions.KindPhoto)

	paths, err := ExpandFolders([]string{root}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 1 || filepath.Base(paths[0]) != "photo.jpg" {
		t.Fatalf("--media-type photos: expected only photo.jpg, got %v", paths)
	}
}

// TestExpandFolders_MediaKindFilter_UnsetMeansNoFiltering guards the
// default: KindUnknown (never called SetMediaKindFilter, or explicitly
// reset) must behave exactly like today -- every recognized file passes.
func TestExpandFolders_MediaKindFilter_UnsetMeansNoFiltering(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "photo.jpg"), []byte("photo"))
	writeFile(t, filepath.Join(root, "clip.mp4"), []byte("video"))

	paths, err := ExpandFolders([]string{root}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 2 {
		t.Fatalf("no filter set: expected both files, got %v", paths)
	}
}

func TestScanFolders_SkippedCountReflectsJunk(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "a.jpg"), []byte("content-a"))
	writeFile(t, filepath.Join(root, "desktop.ini"), []byte("junk"))

	db := openTestDB(t)
	summary, err := ScanFolders(db, []string{root}, "manual_scan", 4, false, true, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if summary.Skipped != 1 {
		t.Errorf("Skipped = %d, want 1", summary.Skipped)
	}
	if summary.FilesSeen != 1 {
		t.Errorf("FilesSeen = %d, want 1 (junk not counted as seen)", summary.FilesSeen)
	}
}

func TestScanFolders_OnPreScanFiresBeforeHashingWithCacheCounts(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "a.jpg"), []byte("content-a"))
	writeFile(t, filepath.Join(root, "b.jpg"), []byte("content-b"))

	db := openTestDB(t)
	// First scan: everything is new, so alreadyKnown should be 0.
	var total1, known1, toHash1 int
	preScanCalled := false
	_, err := ScanFolders(db, []string{root}, "manual_scan", 4, false, true, nil,
		func(total, alreadyKnown, toHashNow int) {
			preScanCalled = true
			total1, known1, toHash1 = total, alreadyKnown, toHashNow
		}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !preScanCalled {
		t.Fatal("onPreScan was never called")
	}
	if total1 != 2 || known1 != 0 || toHash1 != 2 {
		t.Errorf("first scan onPreScan = (total=%d, known=%d, toHash=%d), want (2,0,2)", total1, known1, toHash1)
	}

	// Add a third file; the first two should now be reported as "already known".
	writeFile(t, filepath.Join(root, "c.jpg"), []byte("content-c"))
	var total2, known2, toHash2 int
	_, err = ScanFolders(db, []string{root}, "manual_scan", 4, false, true, nil,
		func(total, alreadyKnown, toHashNow int) {
			total2, known2, toHash2 = total, alreadyKnown, toHashNow
		}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if total2 != 3 || known2 != 2 || toHash2 != 1 {
		t.Errorf("second scan onPreScan = (total=%d, known=%d, toHash=%d), want (3,2,1)", total2, known2, toHash2)
	}
}

func TestScanFolders_MarkSyncedPromotesAlreadyCachedPendingFiles(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "a.jpg"), []byte("content-a"))
	writeFile(t, filepath.Join(root, "b.jpg"), []byte("content-b"))

	db := openTestDB(t)
	// Plain scan first -- both files become 'pending' AND get cached into files_seen.
	if _, err := ScanFolders(db, []string{root}, "manual_scan", 4, false, true, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	pendingBefore, err := db.ListPending()
	if err != nil {
		t.Fatal(err)
	}
	if len(pendingBefore) != 2 {
		t.Fatalf("precondition failed: expected 2 pending after plain scan, got %d", len(pendingBefore))
	}

	// Now mark-synced the SAME folder. Both files hit the files_seen cache
	// (mtime/size unchanged) -- this must still promote them to synced,
	// not silently no-op because "nothing changed" from a hashing standpoint.
	summary, err := ScanFolders(db, []string{root}, "mark_synced", 4, true, true, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if summary.FilesHashed != 0 {
		t.Fatalf("expected 0 re-hashed (cache-hit), got %d", summary.FilesHashed)
	}
	if summary.NewPending != 2 {
		t.Errorf("expected 2 files promoted to synced, got %d", summary.NewPending)
	}

	pendingAfter, err := db.ListPending()
	if err != nil {
		t.Fatal(err)
	}
	if len(pendingAfter) != 0 {
		t.Errorf("expected 0 pending after mark-synced, got %d", len(pendingAfter))
	}
	counts, err := db.CountsByStatus()
	if err != nil {
		t.Fatal(err)
	}
	if counts["uploaded"] != 2 {
		t.Errorf("counts = %+v, want 2 uploaded", counts)
	}
}

// TestScanFolders_TrackRunFalseLeavesRunProgressUntouched is the fix
// enabling scan and upload to run concurrently (internal/engine.
// RunFolderCycle): run_progress is a singleton row (id=1, overwritten in
// place by every writer), so a scan running at the same time as an upload
// that owns the row for that cycle must never touch it. Simulates an
// upload phase already owning the row (db.RunStart("upload", ...)), then
// proves a trackRun=false scan leaves every field exactly as the upload
// left it -- run_type, status, and progress counts alike.
func TestScanFolders_TrackRunFalseLeavesRunProgressUntouched(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "a.jpg"), []byte("fake-jpeg-bytes"))

	db := openTestDB(t)
	if err := db.RunStart("upload", 0, 5); err != nil {
		t.Fatal(err)
	}

	if _, err := ScanFolders(db, []string{root}, "watch_scan", 4, false, false, nil, nil, nil); err != nil {
		t.Fatal(err)
	}

	run, err := db.RunGet()
	if err != nil {
		t.Fatal(err)
	}
	if run == nil {
		t.Fatal("run_progress row is gone after a trackRun=false scan")
	}
	if run.RunType != "upload" {
		t.Errorf("run_type = %q, want %q -- a trackRun=false scan must never touch run_progress", run.RunType, "upload")
	}
	if run.Status != "running" {
		t.Errorf("status = %q, want %q -- a trackRun=false scan must never touch run_progress", run.Status, "running")
	}
	if run.FilesTotal != 5 {
		t.Errorf("files_total = %d, want 5 (the upload's own count, untouched by the scan finding a file to hash)", run.FilesTotal)
	}
}

func TestGuessMime(t *testing.T) {
	if GuessMime("photo.jpg") != "image/jpeg" {
		t.Error("expected image/jpeg for .jpg")
	}
	if GuessMime("clip.mp4") != "video/mp4" {
		t.Error("expected video/mp4 for .mp4")
	}
	if GuessMime("mystery.xyz") != "application/octet-stream" {
		t.Error("expected application/octet-stream fallback for unknown ext")
	}
}

// TestExpandFolders_NamedFileGoesThroughTheSameGates guards an asymmetry
// that was harmless only while every caller passed folders: a file found
// by walking a directory was filtered (OS metadata, known-unsupported
// extension, media-type), while a file named DIRECTLY was added
// unconditionally.
//
// `gpsync mark-synced` now accepts files and globs, so that difference
// became reachable and wrong: naming a .pdf would record it as backed up,
// something scanning its containing folder would never do.
func TestExpandFolders_NamedFileGoesThroughTheSameGates(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "photo.jpg")
	pdf := filepath.Join(dir, "notes.pdf")
	junk := filepath.Join(dir, "Thumbs.db")
	for _, p := range []string{good, pdf, junk} {
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// Naming each file directly must produce the same verdicts as
	// scanning the folder that contains them.
	direct, err := ExpandFolders([]string{good, pdf, junk}, nil)
	if err != nil {
		t.Fatal(err)
	}
	walked, err := ExpandFolders([]string{dir}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(direct, walked) {
		t.Errorf("naming files directly gave %v but scanning their folder gave %v -- the gates must be identical", direct, walked)
	}
	if len(direct) != 1 || filepath.Base(direct[0]) != "photo.jpg" {
		t.Errorf("expanded to %v, want only photo.jpg (a .pdf is a known-unsupported extension, Thumbs.db is OS metadata)", direct)
	}

	// And a file glob behaves the same as naming them.
	globbed, err := ExpandFolders([]string{filepath.Join(dir, "*")}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(globbed, walked) {
		t.Errorf("glob gave %v, want %v", globbed, walked)
	}
}

// TestScanFolders_FlagsMissingFilesAndClearsThemWhenBack covers the scan
// half of missing-file handling: a deleted file is flagged (never deleted),
// its dead scan-cache entry is dropped, and the flag clears if it returns.
func TestScanFolders_FlagsMissingFilesAndClearsThemWhenBack(t *testing.T) {
	db := openTestDB(t)
	dir := t.TempDir()
	keep := filepath.Join(dir, "keep.jpg")
	gone := filepath.Join(dir, "gone.jpg")
	writeFile(t, keep, []byte("keep"))
	writeFile(t, gone, []byte("gone"))
	if _, err := ScanFolders(db, []string{dir}, "manual_scan", 2, false, false, nil, nil, nil); err != nil {
		t.Fatal(err)
	}

	if err := os.Remove(gone); err != nil {
		t.Fatal(err)
	}
	sum, err := ScanFolders(db, []string{dir}, "manual_scan", 2, false, false, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Missing != 1 || sum.StaleCache != 1 {
		t.Errorf("summary = %+v, want Missing=1 and StaleCache=1", sum)
	}
	counts, err := db.MissingCounts()
	if err != nil {
		t.Fatal(err)
	}
	if counts.Flagged != 1 {
		t.Errorf("flagged = %d, want 1", counts.Flagged)
	}
	rows, _ := db.UploadsUnder([]string{dir})
	if len(rows) != 2 {
		t.Errorf("ledger holds %d entries under the folder, want 2 -- a scan must flag, never delete", len(rows))
	}

	writeFile(t, gone, []byte("gone"))
	sum, err = ScanFolders(db, []string{dir}, "manual_scan", 2, false, false, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Reappeared != 1 {
		t.Errorf("Reappeared = %d, want 1", sum.Reappeared)
	}
	if counts, _ := db.MissingCounts(); counts.Flagged != 0 {
		t.Errorf("flagged = %d after the file came back, want 0", counts.Flagged)
	}
}

// TestScanFolders_UnavailableFolderFlagsNothing: a source folder that is not
// there right now (unplugged drive, disconnected share) must flag nothing.
func TestScanFolders_UnavailableFolderFlagsNothing(t *testing.T) {
	db := openTestDB(t)
	parent := t.TempDir()
	dir := filepath.Join(parent, "drive")
	writeFile(t, filepath.Join(dir, "a.jpg"), []byte("a"))
	writeFile(t, filepath.Join(dir, "b.jpg"), []byte("b"))
	if _, err := ScanFolders(db, []string{dir}, "manual_scan", 2, false, false, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(dir, filepath.Join(parent, "unplugged")); err != nil {
		t.Fatal(err)
	}
	sum, err := ScanFolders(db, []string{dir}, "manual_scan", 2, false, false, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Missing != 0 {
		t.Errorf("Missing = %d for a folder that is unavailable, want 0", sum.Missing)
	}
	if counts, _ := db.MissingCounts(); counts.Flagged != 0 {
		t.Errorf("flagged = %d, want 0", counts.Flagged)
	}
}

// TestScanFolders_FilteredOutFileIsNotMissing: a file the scan leaves out on
// purpose (here the media-type filter) still exists and must not be flagged.
func TestScanFolders_FilteredOutFileIsNotMissing(t *testing.T) {
	db := openTestDB(t)
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "photo.jpg"), []byte("p"))
	writeFile(t, filepath.Join(dir, "clip.mp4"), []byte("v"))
	if _, err := ScanFolders(db, []string{dir}, "manual_scan", 2, false, false, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	SetMediaKindFilter(extensions.KindPhoto)
	defer SetMediaKindFilter(extensions.KindUnknown)
	sum, err := ScanFolders(db, []string{dir}, "manual_scan", 2, false, false, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Missing != 0 {
		t.Errorf("Missing = %d, want 0 -- the video still exists, it was only filtered out of this scan", sum.Missing)
	}
}
