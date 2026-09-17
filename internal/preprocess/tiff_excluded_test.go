package preprocess

import "testing"

// TestTIFFIsNeverResized pins the decision that TIFF is never downscaled:
// decoding it is what exposes the known crash in the pure-Go TIFF decoder,
// and TIFF is lossless so the original is the better upload anyway.
func TestTIFFIsNeverResized(t *testing.T) {
	for _, p := range []string{"a.tif", "a.tiff", "A.TIFF"} {
		if IsResizable(p) {
			t.Errorf("IsResizable(%q) = true -- gpsync must never hand a TIFF to the decoder", p)
		}
	}
	if !IsResizable("a.jpg") {
		t.Error("JPEG must still be resizable")
	}
}
