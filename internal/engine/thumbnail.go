package engine

import (
	"bytes"

	"github.com/disintegration/imaging"

	"github.com/gmizrahi/gpsync/internal/preprocess"
)

// GenerateThumbnail decodes path and returns a resized JPEG preview (maxDim
// on the long edge) for gpsync-tray's Duplicate Resolver tab -- the dashboard
// has no other way to show "which of these copies is which" beyond a bare
// filename.
//
// ok is false (data/contentType unset, no error) whenever no real preview
// can be produced -- a video file, or a photo format this pure-Go image
// library can't actually decode (HEIC, RAW: preprocess.IsResizable is
// deliberately narrower than "is this a photo", see its own doc comment).
// The caller falls back to a generic badge + filename either way; nothing
// here distinguishes "wrong kind of file" from "corrupt/unreadable file"
// because the caller doesn't need to -- both get the same fallback.
func GenerateThumbnail(path string, maxDim int) (data []byte, contentType string, ok bool) {
	if !preprocess.IsResizable(path) {
		return nil, "", false
	}
	img, err := imaging.Open(path, imaging.AutoOrientation(true))
	if err != nil {
		return nil, "", false
	}
	thumb := imaging.Fit(img, maxDim, maxDim, imaging.Lanczos)
	var buf bytes.Buffer
	if err := imaging.Encode(&buf, thumb, imaging.JPEG, imaging.JPEGQuality(80)); err != nil {
		return nil, "", false
	}
	return buf.Bytes(), "image/jpeg", true
}
