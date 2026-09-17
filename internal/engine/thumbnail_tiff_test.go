package engine

import (
	"testing"

	"github.com/gmizrahi/gpsync/internal/preprocess"
)

// TestGenerateThumbnail_NeverDecodesTIFF guards the second consumer of the
// gate that keeps TIFF away from the image decoder.
//
// preprocess pins the rule in its own package, but GenerateThumbnail is a
// separate caller: re-adding "tiff" to resizableExts would silently give
// BOTH paths the ability to decode one again. The decoder gpsync depends on
// has a known panic on crafted TIFFs with no fixed version, and the reason
// that advisory does not apply is precisely that nothing reaches it.
//
// Asserted through the gate rather than by feeding GenerateThumbnail a file:
// a first version wrote invalid bytes named .tiff, which imaging refuses on
// content alone, so it passed whether the gate held or not. Checking
// IsResizable is what actually fails when the extension comes back.
func TestGenerateThumbnail_NeverDecodesTIFF(t *testing.T) {
	for _, name := range []string{"photo.tif", "photo.tiff", "PHOTO.TIFF"} {
		if preprocess.IsResizable(name) {
			t.Errorf("IsResizable(%q) = true -- GenerateThumbnail would hand this to the decoder", name)
		}
	}
	// The premise: this test is only meaningful while the gate is the thing
	// GenerateThumbnail consults.
	if !preprocess.IsResizable("photo.jpg") {
		t.Fatal("test premise changed: JPEG is no longer resizable, so this proves nothing")
	}
}
