package engine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gmizrahi/gpsync/internal/config"
	"github.com/gmizrahi/gpsync/internal/scanner"
	"github.com/gmizrahi/gpsync/internal/statedb"
)

// The thumbnail folder may sit inside a source folder, but may not be one or
// contain one: scans skip it, and either would hide photos from them.
func TestThumbnailDir(t *testing.T) {
	lib := filepath.Join(t.TempDir(), "Pictures")
	cfg := config.Defaults()
	cfg.SourceFolders = []string{lib}

	if dir, err := ThumbnailDir(cfg); err != nil || dir != filepath.Join(statedb.StateDir, "thumbs") {
		t.Errorf("unset: %q, %v; want thumbs in the state folder", dir, err)
	}
	cfg.ThumbnailDir = "  " + filepath.Join(lib, ".thumbnails") + "  "
	if dir, err := ThumbnailDir(cfg); err != nil || dir != filepath.Join(lib, ".thumbnails") {
		t.Errorf("inside a source folder: %q, %v; want it accepted", dir, err)
	}
	for why, dir := range map[string]string{
		"the source folder itself": lib,
		"a parent of it":           filepath.Dir(lib),
		"a relative path":          "thumbs",
	} {
		cfg.ThumbnailDir = dir
		if _, err := ThumbnailDir(cfg); err == nil {
			t.Errorf("%s (%q) was accepted", why, dir)
		}
	}
}

// Scans skip the configured thumbnail folder -- by its path, so a folder of
// the same name elsewhere is still scanned -- and an invalid setting is not
// installed, or a thumbnail folder equal to the library would hide it all.
func TestApplyExtensionOverrides_ScansSkipTheThumbnailFolder(t *testing.T) {
	lib := t.TempDir()
	thumbs := filepath.Join(lib, ".thumbnails")
	other := filepath.Join(lib, "Trip", ".thumbnails")
	for _, d := range []string{filepath.Join(thumbs, "256"), other} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range []string{filepath.Join(thumbs, "256", strings.Repeat("a", 64)+".jpg"), filepath.Join(other, "kept.jpg"), filepath.Join(lib, "Trip", "a.jpg")} {
		if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cfg := config.Defaults()
	cfg.SourceFolders = []string{lib}
	cfg.ThumbnailDir = thumbs
	ApplyExtensionOverrides(cfg)
	t.Cleanup(func() { ApplyExtensionOverrides(config.Defaults()) })

	files, err := scanner.ExpandFolders([]string{lib}, nil)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(files, "\n")
	if strings.Contains(got, thumbs+string(filepath.Separator)) || len(files) != 2 {
		t.Errorf("scan found:\n%s\nwant a.jpg and kept.jpg only -- nothing under %s", got, thumbs)
	}

	cfg.ThumbnailDir = lib // invalid: it is the source folder
	ApplyExtensionOverrides(cfg)
	if scanner.IsIgnoredDir(lib) {
		t.Error("an invalid thumbnail folder (the library itself) was installed as ignored")
	}
}

// The folder may be inside the user's library, so the cache must only ever
// evict its own files: other JPEGs there -- even in its size folders --
// survive any amount of eviction.
func TestThumbCache_EvictsOnlyItsOwnFiles(t *testing.T) {
	dir := filepath.Join(t.TempDir(), ".thumbnails")
	foreign := []string{filepath.Join(dir, "IMG_0001.jpg"), filepath.Join(dir, "256", "IMG_0002.jpg"), filepath.Join(dir, "Trip", strings.Repeat("b", 64)+".jpg")}
	for _, f := range foreign {
		if err := os.MkdirAll(filepath.Dir(f), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(f, make([]byte, 50_000), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	src := t.TempDir()
	c := NewThumbCache(dir, 1) // evicts everything it can, every time
	for i := 1; i <= 3; i++ {
		p := filepath.Join(src, "p.jpg")
		writeTestJPEG(t, p, 300, 200)
		if _, err := c.Thumbnail(testSHA(i), p, 256); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range foreign {
		if _, err := os.Stat(f); err != nil {
			t.Errorf("eviction removed a file it didn't make: %s", f)
		}
	}
	if left := c.entries(); len(left) != 0 {
		t.Errorf("%d of its own thumbnails survived a 1-byte bound", len(left))
	}
}

// UseDir moves the cache: the next thumbnail goes to the new folder.
func TestThumbCache_UseDirMovesIt(t *testing.T) {
	a, b := filepath.Join(t.TempDir(), "a"), filepath.Join(t.TempDir(), "b")
	p := filepath.Join(t.TempDir(), "p.jpg")
	writeTestJPEG(t, p, 300, 200)
	c := NewThumbCache(a, 1<<30)
	if _, err := c.Thumbnail(testSHA(1), p, 256); err != nil {
		t.Fatal(err)
	}
	c.UseDir(b)
	if _, err := c.Thumbnail(testSHA(2), p, 256); err != nil {
		t.Fatal(err)
	}
	for f, want := range map[string]bool{
		filepath.Join(a, "256", testSHA(1)+".jpg"): true,
		filepath.Join(b, "256", testSHA(2)+".jpg"): true,
		filepath.Join(a, "256", testSHA(2)+".jpg"): false,
	} {
		if _, err := os.Stat(f); (err == nil) != want {
			t.Errorf("%s exists = %v, want %v", f, err == nil, want)
		}
	}
}
