package engine

import (
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

func touch(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestDiscoverSyncUnits_MultipleSubfolderLevels(t *testing.T) {
	root := t.TempDir()
	// Multiple nesting depths, matching a real "C:\Photos\2024\..." tree.
	touch(t, filepath.Join(root, "2024", "2024_01 Trip", "IMG_0001.jpg"))
	touch(t, filepath.Join(root, "2024", "2024_01 Trip", "raw", "IMG_0001.dng"))
	touch(t, filepath.Join(root, "2024", "2024_02", "IMG_0002.jpg"))
	// A folder with only subfolders, no direct files -- must NOT itself be a unit.
	touch(t, filepath.Join(root, "2024", "empty_parent", "actual_photos", "IMG_0003.jpg"))

	units, err := DiscoverSyncUnits([]string{filepath.Join(root, "2024")})
	if err != nil {
		t.Fatal(err)
	}

	want := []string{
		filepath.Join(root, "2024", "2024_01 Trip"),
		filepath.Join(root, "2024", "2024_01 Trip", "raw"),
		filepath.Join(root, "2024", "2024_02"),
		filepath.Join(root, "2024", "empty_parent", "actual_photos"),
	}
	sort.Strings(want)
	if len(units) != len(want) {
		t.Fatalf("got %d units, want %d: %v", len(units), len(want), units)
	}
	for i := range want {
		if filepath.Clean(units[i]) != filepath.Clean(want[i]) {
			t.Errorf("unit[%d] = %s, want %s", i, units[i], want[i])
		}
	}
}

func TestDiscoverSyncUnits_GlobPatternExpandsToMultipleFolders(t *testing.T) {
	root := t.TempDir()
	touch(t, filepath.Join(root, "2024_01 Trip", "IMG_0001.jpg"))
	touch(t, filepath.Join(root, "2024_01 Hike", "IMG_0002.jpg"))
	touch(t, filepath.Join(root, "2024_02", "IMG_0003.jpg")) // should NOT match the "2024_01*" glob

	units, err := DiscoverSyncUnits([]string{filepath.Join(root, "2024_01*")})
	if err != nil {
		t.Fatal(err)
	}
	if len(units) != 2 {
		t.Fatalf("got %d units, want 2: %v", len(units), units)
	}
	for _, u := range units {
		base := filepath.Base(u)
		if base != "2024_01 Trip" && base != "2024_01 Hike" {
			t.Errorf("unexpected unit matched by glob: %s", u)
		}
	}
}

func TestDiscoverSyncUnits_SingleLeafFolder(t *testing.T) {
	root := t.TempDir()
	touch(t, filepath.Join(root, "photo.jpg"))

	units, err := DiscoverSyncUnits([]string{root})
	if err != nil {
		t.Fatal(err)
	}
	if len(units) != 1 || filepath.Clean(units[0]) != filepath.Clean(root) {
		t.Fatalf("got %v, want [%s]", units, root)
	}
}

func TestDiscoverSyncUnits_TrailingSlashOnPatternIsTolerated(t *testing.T) {
	root := t.TempDir()
	touch(t, filepath.Join(root, "2024_01 Trip", "IMG_0001.jpg"))

	units, err := DiscoverSyncUnits([]string{filepath.Join(root, "2024_01*") + string(filepath.Separator)})
	if err != nil {
		t.Fatal(err)
	}
	if len(units) != 1 {
		t.Fatalf("got %d units, want 1: %v", len(units), units)
	}
}

func TestDiscoverSyncUnits_ExcludesPicasaOriginalsAndJunkOnlyFolders(t *testing.T) {
	root := t.TempDir()
	touch(t, filepath.Join(root, "2024_01", "IMG_0001.jpg"))
	// A folder containing only Picasa's originals-backup and junk files must not become a unit.
	touch(t, filepath.Join(root, "2024_01", ".picasaoriginals", "IMG_0001.jpg"))
	touch(t, filepath.Join(root, "junk_only", "desktop.ini"))

	units, err := DiscoverSyncUnits([]string{root})
	if err != nil {
		t.Fatal(err)
	}
	if len(units) != 1 || filepath.Clean(units[0]) != filepath.Clean(filepath.Join(root, "2024_01")) {
		t.Fatalf("got %v, want only [%s]", units, filepath.Join(root, "2024_01"))
	}
}

func TestDiscoverSyncUnits_DedupsAcrossOverlappingArgs(t *testing.T) {
	root := t.TempDir()
	touch(t, filepath.Join(root, "photo.jpg"))

	// Same folder reachable two ways -- must appear once.
	units, err := DiscoverSyncUnits([]string{root, root})
	if err != nil {
		t.Fatal(err)
	}
	if len(units) != 1 {
		t.Fatalf("got %d units, want 1 (deduped): %v", len(units), units)
	}
}

// TestDiscoverMarkTargets_SplitsFilesFromFolders covers the shape
// `gpsync mark-synced` gained once it accepted files and globs, not just
// folders.
//
// Before this, every argument went through DiscoverSyncUnits, which globs
// and then silently DISCARDS anything that isn't a directory -- so a file
// glob expanded correctly, matched real files, and then vanished, leaving
// only "no folders containing files found".
func TestDiscoverMarkTargets_SplitsFilesFromFolders(t *testing.T) {
	root := t.TempDir()
	trip := filepath.Join(root, "2022", "SummerTrip")
	touch(t, filepath.Join(trip, "a.mp4"))
	touch(t, filepath.Join(trip, "b.mp4"))
	touch(t, filepath.Join(trip, "c.jpg"))
	touch(t, filepath.Join(root, "2019", "old.jpg"))

	// A folder, one named file, and a file glob, all in one invocation.
	folders, files, err := DiscoverMarkTargets([]string{
		filepath.Join(root, "2019"),
		filepath.Join(trip, "c.jpg"),
		filepath.Join(trip, "*.mp4"),
	})
	if err != nil {
		t.Fatal(err)
	}

	wantFolders := []string{filepath.Join(root, "2019")}
	if len(folders) != 1 || folders[0] != wantFolders[0] {
		t.Errorf("folders = %v, want %v", folders, wantFolders)
	}
	wantFiles := []string{
		filepath.Join(trip, "a.mp4"),
		filepath.Join(trip, "b.mp4"),
		filepath.Join(trip, "c.jpg"),
	}
	sort.Strings(wantFiles)
	if !reflect.DeepEqual(files, wantFiles) {
		t.Errorf("files = %v, want %v", files, wantFiles)
	}

	// The folder must NOT also contribute its files to the file list --
	// it is scanned as a unit, and double-counting would scan them twice.
	for _, f := range files {
		if strings.Contains(f, string(filepath.Separator)+"2019"+string(filepath.Separator)) {
			t.Errorf("%s came from a folder argument and should be scanned as part of it, not named individually", f)
		}
	}
}

// TestDiscoverMarkTargets_DedupsAndIgnoresMisses: a pattern matching
// nothing is dropped rather than failing the whole command, matching
// DiscoverSyncUnits' own behaviour -- one typo among several good
// arguments should not throw away the work.
func TestDiscoverMarkTargets_DedupsAndIgnoresMisses(t *testing.T) {
	root := t.TempDir()
	touch(t, filepath.Join(root, "a.mp4"))

	folders, files, err := DiscoverMarkTargets([]string{
		filepath.Join(root, "a.mp4"),
		filepath.Join(root, "a.mp4"), // same file twice
		filepath.Join(root, "*.mp4"), // and again via a glob
		filepath.Join(root, "nope-does-not-exist.mp4"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(folders) != 0 {
		t.Errorf("folders = %v, want none", folders)
	}
	if len(files) != 1 {
		t.Errorf("files = %v, want exactly one (deduped)", files)
	}
}
