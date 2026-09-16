package preprocess

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/rwcarlsen/goexif/exif"

	"github.com/gmizrahi/gpsync/internal/config"
)

// writeJPEG creates a real w x h JPEG on disk -- PrepareUploadPath decodes
// the file for real (imaging.Open), so a stub byte blob wouldn't exercise
// the resize path at all.
func writeJPEG(t *testing.T, path string, w, h int, shade uint8) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{R: shade, G: uint8(x % 256), B: uint8(y % 256), A: 255})
		}
	}
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := jpeg.Encode(f, img, nil); err != nil {
		t.Fatal(err)
	}
}

// spaceSaverConfig turns on the downscale path with a max dimension small
// enough that any test image above trips it.
func spaceSaverConfig() config.Config {
	cfg := config.Defaults()
	cfg.UploadQuality = "space_saver"
	cfg.SpaceSaverMaxDim = 16
	return cfg
}

// isolateTempDir points os.TempDir() (which PrepareUploadPath builds its
// scratch folder under) at a per-test directory, so tests never collide
// with each other or with a real gpsync run on the same machine.
func isolateTempDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("TMPDIR", dir)
	return dir
}

// TestPrepareUploadPath_SameBasenameInDifferentFolders_GetsDistinctTempFiles
// proves the fix for silent, undetectable upload corruption. The temp file
// used to be named purely from the source basename, so IMG_0001.jpg under
// 2023/ and IMG_0001.jpg under 2024/ mapped to the SAME temp path. With
// concurrent upload workers, one could overwrite the other's temp file
// mid-flight -- meaning one photo's bytes got uploaded and then permanently
// recorded in the ledger under a DIFFERENT photo's SHA-256, with nothing
// anywhere reporting a problem.
func TestPrepareUploadPath_SameBasenameInDifferentFolders_GetsDistinctTempFiles(t *testing.T) {
	isolateTempDir(t)
	src := t.TempDir()

	a := filepath.Join(src, "2023", "IMG_0001.jpg")
	b := filepath.Join(src, "2024", "IMG_0001.jpg")
	writeJPEG(t, a, 64, 64, 10)
	writeJPEG(t, b, 64, 64, 200)

	cfg := spaceSaverConfig()

	pathA, cleanupA := PrepareUploadPath(a, cfg)
	defer cleanupA()
	pathB, cleanupB := PrepareUploadPath(b, cfg)
	defer cleanupB()

	if pathA == a || pathB == b {
		t.Fatalf("expected both oversized images to be downscaled to temp copies, got %q and %q", pathA, pathB)
	}
	if pathA == pathB {
		t.Fatalf("both files got the SAME temp path %q -- concurrent workers would overwrite each other's bytes", pathA)
	}

	// Both must still exist simultaneously with their own content -- the
	// collision bug's real symptom is one file's bytes standing in for the
	// other's.
	bytesA, err := os.ReadFile(pathA)
	if err != nil {
		t.Fatal(err)
	}
	bytesB, err := os.ReadFile(pathB)
	if err != nil {
		t.Fatal(err)
	}
	if len(bytesA) == 0 || len(bytesB) == 0 {
		t.Fatal("expected both temp files to hold real image data")
	}
	if string(bytesA) == string(bytesB) {
		t.Error("the two temp files have identical content -- one overwrote the other")
	}
}

// TestPrepareUploadPath_ConcurrentSameBasename_AllPathsUnique is the same
// bug from the angle the uploader actually hits it: several workers
// preparing same-named files from different folders at once.
func TestPrepareUploadPath_ConcurrentSameBasename_AllPathsUnique(t *testing.T) {
	isolateTempDir(t)
	src := t.TempDir()
	cfg := spaceSaverConfig()

	const n = 8
	sources := make([]string, n)
	for i := 0; i < n; i++ {
		sources[i] = filepath.Join(src, string(rune('a'+i)), "IMG_0001.jpg")
		writeJPEG(t, sources[i], 64, 64, uint8(i*20))
	}

	var mu sync.Mutex
	seen := map[string]bool{}
	var cleanups []func()
	var wg sync.WaitGroup
	for _, s := range sources {
		wg.Add(1)
		go func(s string) {
			defer wg.Done()
			p, cleanup := PrepareUploadPath(s, cfg)
			mu.Lock()
			defer mu.Unlock()
			if seen[p] {
				t.Errorf("duplicate temp path handed to two concurrent workers: %q", p)
			}
			seen[p] = true
			cleanups = append(cleanups, cleanup)
		}(s)
	}
	wg.Wait()
	for _, c := range cleanups {
		c()
	}
	if len(seen) != n {
		t.Errorf("got %d distinct temp paths, want %d", len(seen), n)
	}
}

// TestPrepareUploadPath_CleanupRemovesTempFile proves the temp copies no
// longer accumulate forever in TempDir: the returned cleanup deletes the
// downscaled file the caller was handed.
func TestPrepareUploadPath_CleanupRemovesTempFile(t *testing.T) {
	isolateTempDir(t)
	src := t.TempDir()
	p := filepath.Join(src, "big.jpg")
	writeJPEG(t, p, 64, 64, 30)

	out, cleanup := PrepareUploadPath(p, spaceSaverConfig())
	if out == p {
		t.Fatal("expected a downscaled temp copy")
	}
	if _, err := os.Stat(out); err != nil {
		t.Fatalf("temp copy should exist before cleanup: %v", err)
	}
	cleanup()
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Errorf("cleanup did not remove the temp copy %q (err=%v)", out, err)
	}
}

// TestPrepareUploadPath_PassthroughCleanupNeverDeletesTheSource is the
// safety property that matters most about the cleanup func: every case that
// returns the caller's ORIGINAL path must return a no-op, because running a
// remove on that path would destroy the user's actual photo.
func TestPrepareUploadPath_PassthroughCleanupNeverDeletesTheSource(t *testing.T) {
	isolateTempDir(t)
	src := t.TempDir()

	small := filepath.Join(src, "small.jpg")
	writeJPEG(t, small, 8, 8, 40) // already under SpaceSaverMaxDim
	video := filepath.Join(src, "clip.mp4")
	if err := os.WriteFile(video, []byte("not an image"), 0o644); err != nil {
		t.Fatal(err)
	}
	corrupt := filepath.Join(src, "corrupt.jpg")
	if err := os.WriteFile(corrupt, []byte("this will not decode"), 0o644); err != nil {
		t.Fatal(err)
	}
	original := filepath.Join(src, "orig.jpg")
	writeJPEG(t, original, 64, 64, 50)

	cases := []struct {
		name string
		path string
		cfg  config.Config
	}{
		{"space saver off", original, config.Defaults()},
		{"non-resizable extension", video, spaceSaverConfig()},
		{"already small enough", small, spaceSaverConfig()},
		{"undecodable image", corrupt, spaceSaverConfig()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, cleanup := PrepareUploadPath(tc.path, tc.cfg)
			if got != tc.path {
				t.Fatalf("expected passthrough of %q, got %q", tc.path, got)
			}
			if cleanup == nil {
				t.Fatal("cleanup must never be nil -- callers defer it unconditionally")
			}
			cleanup()
			if _, err := os.Stat(tc.path); err != nil {
				t.Fatalf("cleanup deleted the user's SOURCE file %q: %v", tc.path, err)
			}
		})
	}
}

// exifAPP1 is a minimal APP1 segment carrying DateTimeOriginal, spliced
// into a JPEG right after SOI. Enough for goexif to decode, which is all
// this needs to prove whether the tag survives a round trip.
func exifAPP1() []byte {
	dt := []byte("2021:07:04 11:22:33\x00") // 20 bytes incl. NUL
	tiff := []byte{'I', 'I', 0x2A, 0x00, 0x08, 0x00, 0x00, 0x00}
	tiff = append(tiff, 0x01, 0x00) // IFD0: one entry
	exifIFDOffset := uint32(26)
	tiff = append(tiff, 0x69, 0x87, 0x04, 0x00, 0x01, 0x00, 0x00, 0x00)
	tiff = append(tiff, byte(exifIFDOffset), byte(exifIFDOffset>>8), byte(exifIFDOffset>>16), byte(exifIFDOffset>>24))
	tiff = append(tiff, 0x00, 0x00, 0x00, 0x00)
	tiff = append(tiff, 0x01, 0x00) // ExifIFD: one entry
	dataOffset := exifIFDOffset + 18
	tiff = append(tiff, 0x03, 0x90, 0x02, 0x00, 0x14, 0x00, 0x00, 0x00)
	tiff = append(tiff, byte(dataOffset), byte(dataOffset>>8), byte(dataOffset>>16), byte(dataOffset>>24))
	tiff = append(tiff, 0x00, 0x00, 0x00, 0x00)
	tiff = append(tiff, dt...)

	payload := append([]byte("Exif\x00\x00"), tiff...)
	segLen := len(payload) + 2
	return append([]byte{0xFF, 0xE1, byte(segLen >> 8), byte(segLen)}, payload...)
}

func writeJPEGWithEXIF(t *testing.T, path string, w, h int) {
	t.Helper()
	writeJPEG(t, path, w, h, 9)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	spliced := append(append(append([]byte{}, raw[:2]...), exifAPP1()...), raw[2:]...)
	if err := os.WriteFile(path, spliced, 0o644); err != nil {
		t.Fatal(err)
	}
	// Confirm the fixture itself is sound before any test relies on it.
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	x, err := exif.Decode(f)
	if err != nil {
		t.Fatalf("fixture has no readable EXIF: %v", err)
	}
	if _, err := x.Get(exif.DateTimeOriginal); err != nil {
		t.Fatalf("fixture has no DateTimeOriginal: %v", err)
	}
}

func dateTimeOriginal(t *testing.T, path string) (string, bool) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	x, err := exif.Decode(f)
	if err != nil {
		return "", false
	}
	tag, err := x.Get(exif.DateTimeOriginal)
	if err != nil {
		return "", false
	}
	return tag.String(), true
}

// TestPrepareUploadPath_OriginalQualityPreservesBytesAndEXIF pins the
// property the default configuration depends on for capture dates to be
// right in Google Photos.
//
// The batchCreate API accepts no date field -- Google derives "date taken"
// from EXIF embedded in the uploaded bytes -- so in upload_quality
// "original" the bytes must reach Google completely untouched. This asserts
// byte-for-byte identity, not merely that EXIF survives, because anything
// that rewrites the file at all is a regression here.
func TestPrepareUploadPath_OriginalQualityPreservesBytesAndEXIF(t *testing.T) {
	isolateTempDir(t)
	src := t.TempDir()
	p := filepath.Join(src, "photo.jpg")
	writeJPEGWithEXIF(t, p, 64, 64)

	before, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}

	cfg := config.Defaults() // upload_quality defaults to "original"
	if cfg.UploadQuality != "original" {
		t.Fatalf("test premise changed: default upload_quality is now %q", cfg.UploadQuality)
	}
	out, cleanup := PrepareUploadPath(p, cfg)
	defer cleanup()

	if out != p {
		t.Fatalf("original quality returned %q, want the source path unchanged", out)
	}
	after, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("the uploaded bytes are not byte-identical to the source -- anything that re-encodes here silently destroys the EXIF Google reads the capture date from")
	}
	got, ok := dateTimeOriginal(t, out)
	if !ok || got != "\"2021:07:04 11:22:33\"" {
		t.Errorf("DateTimeOriginal = %q (present=%v), want it preserved exactly", got, ok)
	}
}

// TestPrepareUploadPath_SpaceSaverStripsEXIF documents a currently
// unfixed limitation, verified rather than assumed.
//
// disintegration/imaging decodes to a raw image and re-encodes, carrying no
// metadata across, so every EXIF tag -- DateTimeOriginal included -- is gone
// from the bytes that reach Google. Since batchCreate has no date field,
// Google then falls back to the upload time and the photo shows the wrong
// "date taken". Only files that actually get downscaled are affected;
// anything returned unchanged (see the passthrough test) keeps its EXIF.
//
// This test asserts the CURRENT behavior deliberately. If someone makes
// space-saver preserve EXIF, this test should fail and be replaced by one
// asserting preservation -- note that re-attaching the original APP1
// segment naively would double-apply rotation, since imaging.Open already
// auto-orients the pixels, so the Orientation tag must be normalized to 1.
func TestPrepareUploadPath_SpaceSaverStripsEXIF(t *testing.T) {
	isolateTempDir(t)
	src := t.TempDir()
	p := filepath.Join(src, "photo.jpg")
	writeJPEGWithEXIF(t, p, 64, 64)

	if _, ok := dateTimeOriginal(t, p); !ok {
		t.Fatal("fixture lost its EXIF before the test started")
	}

	out, cleanup := PrepareUploadPath(p, spaceSaverConfig())
	defer cleanup()
	if out == p {
		t.Fatal("expected the image to be downscaled")
	}

	if got, ok := dateTimeOriginal(t, out); ok {
		t.Errorf("DateTimeOriginal = %q survived the resize -- if this now works, replace this test with one asserting preservation (and check Orientation isn't double-applied)", got)
	}

	// The source itself must be untouched either way.
	if _, ok := dateTimeOriginal(t, p); !ok {
		t.Error("the ORIGINAL file lost its EXIF -- space saver must never modify the source")
	}
}
