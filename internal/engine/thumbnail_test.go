package engine

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"os"
	"path/filepath"
	"testing"

	"github.com/disintegration/imaging"
)

func writeTestJPEG(t *testing.T, path string, w, h int) {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{R: uint8(x % 256), G: uint8(y % 256), B: 100, A: 255})
		}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
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

func TestGenerateThumbnail_RealImage_ReturnsResizedJPEG(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "photo.jpg")
	writeTestJPEG(t, p, 800, 600)

	data, contentType, ok := GenerateThumbnail(p, 200)
	if !ok {
		t.Fatal("ok = false, want true for a real, decodable JPEG")
	}
	if contentType != "image/jpeg" {
		t.Errorf("contentType = %q, want image/jpeg", contentType)
	}
	decoded, err := imaging.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("thumbnail bytes did not decode as an image: %v", err)
	}
	b := decoded.Bounds()
	if b.Dx() > 200 || b.Dy() > 200 {
		t.Errorf("thumbnail size = %dx%d, want both dimensions <= 200 (maxDim)", b.Dx(), b.Dy())
	}
	if b.Dx() != 200 && b.Dy() != 200 {
		t.Errorf("thumbnail size = %dx%d, want at least one dimension to hit maxDim (200) exactly -- Fit should scale up to fill it", b.Dx(), b.Dy())
	}
}

func TestGenerateThumbnail_VideoPath_ReturnsOkFalseNotAnError(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "clip.mp4")
	if err := os.WriteFile(p, []byte("not a real video, doesn't matter"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, _, ok := GenerateThumbnail(p, 200)
	if ok {
		t.Error("ok = true for a video file, want false -- the caller should fall back to a badge, not try to render this as an image")
	}
}

func TestGenerateThumbnail_UnreadableFile_ReturnsOkFalseNotAPanic(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "corrupt.jpg")
	if err := os.WriteFile(p, []byte("this is not actually a jpeg"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, _, ok := GenerateThumbnail(p, 200)
	if ok {
		t.Error("ok = true for a corrupt/undecodable file, want false")
	}
}

func TestGenerateThumbnail_MissingFile_ReturnsOkFalseNotAPanic(t *testing.T) {
	_, _, ok := GenerateThumbnail(filepath.Join(t.TempDir(), "does-not-exist.jpg"), 200)
	if ok {
		t.Error("ok = true for a missing file, want false")
	}
}
