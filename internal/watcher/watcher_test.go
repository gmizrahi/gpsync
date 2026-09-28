package watcher

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

const testDebounce = 100 * time.Millisecond
const testTimeout = 3 * time.Second

func mustWriteFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

// waitReady blocks for one Watcher.Ready() value, failing the test if none
// arrives within testTimeout.
func waitReady(t *testing.T, w *Watcher) string {
	t.Helper()
	select {
	case folder := <-w.Ready():
		return folder
	case err := <-w.Errors():
		t.Fatalf("unexpected watcher error: %v", err)
	case <-time.After(testTimeout):
		t.Fatal("timed out waiting for Ready()")
	}
	return ""
}

// assertNoReady confirms nothing arrives on Ready() within a window
// comfortably longer than the debounce -- the only way to test a negative
// (ignored file/dir never triggers a cycle) against a timing-based
// filesystem watcher.
func assertNoReady(t *testing.T, w *Watcher, within time.Duration) {
	t.Helper()
	select {
	case folder := <-w.Ready():
		t.Fatalf("Ready() fired for %q, want nothing", folder)
	case <-time.After(within):
	}
}

func TestNew_WatchesExistingTreeAndFiresOnChange(t *testing.T) {
	root := t.TempDir()
	sub := filepath.Join(root, "2024", "2024_01")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}

	w, err := New([]string{root}, testDebounce)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	mustWriteFile(t, filepath.Join(sub, "photo.jpg"), []byte("x"))

	got := waitReady(t, w)
	if got != sub {
		t.Errorf("Ready() folder = %q, want %q", got, sub)
	}
}

// TestWatcher_DebouncesBurstOfChanges proves a burst of activity in one
// folder collapses to a single Ready signal once it settles, not one per
// file -- the whole point of debouncing at all.
func TestWatcher_DebouncesBurstOfChanges(t *testing.T) {
	root := t.TempDir()
	// A deliberately generous debounce, and no sleeping between the writes:
	// a burst is meant to be a burst. With a 100ms window and a sleep in the
	// loop, a loaded runner can stretch the burst past the window, fire
	// early, and then fire a second time -- which is how this failed in CI
	// while passing locally. Stays well under testTimeout.
	const burstDebounce = time.Second
	w, err := New([]string{root}, burstDebounce)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	for i := 0; i < 5; i++ {
		mustWriteFile(t, filepath.Join(root, "burst"+string(rune('a'+i))+".jpg"), []byte("x"))
	}

	got := waitReady(t, w)
	if got != root {
		t.Errorf("Ready() folder = %q, want %q", got, root)
	}
	// Nothing further queued -- the burst was really one signal, not five.
	assertNoReady(t, w, burstDebounce)
}

// TestWatcher_NewSubdirectoryGetsWatchedAutomatically proves the core
// reason this package can't just call fsw.Add() once per configured
// folder: a brand-new year/month folder created after gpsync watch is
// already running must start being watched without a restart.
func TestWatcher_NewSubdirectoryGetsWatchedAutomatically(t *testing.T) {
	root := t.TempDir()
	w, err := New([]string{root}, testDebounce)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	newFolder := filepath.Join(root, "2025", "2025_01")
	if err := os.MkdirAll(newFolder, 0o755); err != nil {
		t.Fatal(err)
	}
	// Give the watcher's own event loop time to see "2025" was created and
	// finish walking+adding a watch on "2025/2025_01" before writing into
	// it -- otherwise this write can race the dynamic Add() and land in a
	// window where nothing is watching it yet, which is a real (if very
	// small) inherent race in any "watch new directories as they appear"
	// design, not something this test is trying to prove doesn't exist.
	time.Sleep(testDebounce * 3)
	mustWriteFile(t, filepath.Join(newFolder, "photo.jpg"), []byte("x"))

	got := waitReady(t, w)
	if got != newFolder {
		t.Errorf("Ready() folder = %q, want %q", got, newFolder)
	}
}

// TestWatcher_IgnoresJunkFileNames proves OS/app junk (Thumbs.db etc.)
// never triggers a cycle -- gpsync scan already excludes these, the watcher
// must not manufacture work for files that would just be skipped anyway.
func TestWatcher_IgnoresJunkFileNames(t *testing.T) {
	root := t.TempDir()
	w, err := New([]string{root}, testDebounce)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	mustWriteFile(t, filepath.Join(root, "Thumbs.db"), []byte("junk"))
	assertNoReady(t, w, testDebounce*3)

	// The watcher itself is still healthy: a real file right after fires normally.
	mustWriteFile(t, filepath.Join(root, "photo.jpg"), []byte("x"))
	got := waitReady(t, w)
	if got != root {
		t.Errorf("Ready() folder = %q, want %q", got, root)
	}
}

// TestWatcher_IgnoresPicasaOriginalsDir proves an ignored directory (and
// everything under it) never gets watched at all -- not just filtered
// after the fact -- mirroring scanner.ExpandFolders' own exclusion.
func TestWatcher_IgnoresPicasaOriginalsDir(t *testing.T) {
	root := t.TempDir()
	w, err := New([]string{root}, testDebounce)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	mustWriteFile(t, filepath.Join(root, ".picasaoriginals", "photo.jpg"), []byte("x"))
	assertNoReady(t, w, testDebounce*3)
}

// TestWatcher_Close_IsSafeAndIdempotent covers basic shutdown hygiene: no
// panic, safe to call twice, and nothing arrives after.
func TestWatcher_Close_IsSafeAndIdempotent(t *testing.T) {
	root := t.TempDir()
	w, err := New([]string{root}, testDebounce)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Errorf("Close() = %v, want nil", err)
	}
	if err := w.Close(); err != nil {
		t.Errorf("second Close() = %v, want nil (must be idempotent)", err)
	}
}

func TestNew_MissingRootReturnsError(t *testing.T) {
	_, err := New([]string{filepath.Join(t.TempDir(), "does-not-exist")}, testDebounce)
	if err == nil {
		t.Fatal("New() with a nonexistent root = nil error, want one")
	}
}

// TestWatcher_PopulatedFolderMovedInIsScanned covers the case a copy does not:
// a folder moved (renamed) into a watched root arrives as ONE atomic event and
// its files produce no events of their own, because they were never created
// inside the watched tree. Adding a watch to the new directory is not enough --
// without a scan signal nothing ever looks inside it, and the files stay
// invisible until an unrelated manual scan happens to run.
func TestWatcher_PopulatedFolderMovedInIsScanned(t *testing.T) {
	root := t.TempDir()
	// Staged OUTSIDE the watched root, then renamed in, so no create event
	// is ever generated for the file itself.
	staging := t.TempDir()
	src := filepath.Join(staging, "Vesuvius")
	mustWriteFile(t, filepath.Join(src, "20260921_162919.mp4"), []byte("video bytes"))

	w, err := New([]string{root}, testDebounce)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	dst := filepath.Join(root, "Vesuvius")
	if err := os.Rename(src, dst); err != nil {
		t.Skipf("cannot rename across these temp dirs: %v", err)
	}

	if got := waitReady(t, w); got != dst {
		t.Errorf("Ready() = %q, want %q (the moved-in folder must be scanned)", got, dst)
	}
}

// TestWatcher_MovedInSubfoldersAreEachScanned is the nested version: a whole
// tree moved in at once must signal every folder that actually holds files,
// not just the top one.
func TestWatcher_MovedInSubfoldersAreEachScanned(t *testing.T) {
	root := t.TempDir()
	staging := t.TempDir()
	src := filepath.Join(staging, "Italy")
	mustWriteFile(t, filepath.Join(src, "top.jpg"), []byte("a"))
	mustWriteFile(t, filepath.Join(src, "Vesuvius", "deep.jpg"), []byte("b"))

	w, err := New([]string{root}, testDebounce)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	dst := filepath.Join(root, "Italy")
	if err := os.Rename(src, dst); err != nil {
		t.Skipf("cannot rename across these temp dirs: %v", err)
	}

	want := map[string]bool{dst: true, filepath.Join(dst, "Vesuvius"): true}
	got := map[string]bool{}
	for range want {
		got[waitReady(t, w)] = true
	}
	for folder := range want {
		if !got[folder] {
			t.Errorf("no Ready() for %q; got %v", folder, got)
		}
	}
}

// TestWatcher_InitialRootsDoNotSignalAScan guards the other side of the fix:
// the scan signal must apply only to trees that appear after the watch is
// running. Signalling the initial roots would make merely starting the tray
// rescan every source folder.
func TestWatcher_InitialRootsDoNotSignalAScan(t *testing.T) {
	root := t.TempDir()
	mustWriteFile(t, filepath.Join(root, "Existing", "already-here.jpg"), []byte("x"))

	w, err := New([]string{root}, testDebounce)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	assertNoReady(t, w, 5*testDebounce)
}
