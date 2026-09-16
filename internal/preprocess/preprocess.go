// Package preprocess implements optional local "space saver" pre-processing.
// Google's API has no server-side quality option (Google retired free "High
// quality" compression in 2021) — this is the only real way to trade
// fidelity for storage. Images only; the ledger's identity hash is always
// computed from the *original* file, never the downscaled copy, so dedup
// correctness is unaffected by this setting.
package preprocess

import (
	"os"
	"path/filepath"

	"github.com/disintegration/imaging"

	"github.com/gmizrahi/gpsync/internal/config"
	"github.com/gmizrahi/gpsync/internal/extensions"
)

var resizableExts = map[string]bool{
	"jpg": true, "jpeg": true, "png": true, "webp": true, "tiff": true, "tif": true,
}

// IsResizable reports whether imaging.Open can actually decode this file --
// deliberately narrower than extensions.KindOf's "photo" classification,
// which also includes HEIC and RAW formats (CR2, NEF, ARW, ...) that this
// pure-Go, no-cgo image library cannot open at all. Exported so
// internal/engine's thumbnail generator (gpsync-tray's Duplicate Resolver tab)
// can reuse the exact same "can we actually decode this" answer instead of
// maintaining a second, driftable list.
func IsResizable(path string) bool {
	return resizableExts[extensions.ExtOf(path)]
}

// noCleanup is the cleanup returned whenever PrepareUploadPath hands back
// the caller's ORIGINAL path -- deleting that would destroy the user's
// actual source file, so the passthrough case must never return anything
// that removes it.
func noCleanup() {}

// PrepareUploadPath returns the path whose bytes should actually be
// uploaded -- the original, unless space-saver mode is on and this is a
// resizable image over the configured max dimension -- plus a cleanup func
// the caller MUST call once the upload attempt is over (success or
// failure). cleanup is a no-op when the original path was returned, and
// removes the temporary downscaled copy otherwise; it's always non-nil and
// always safe to call exactly once. Any pre-processing failure falls back
// to the original — never block a backup over a cosmetic optimization.
//
// The temp file's name comes from os.CreateTemp, not from the source
// basename alone. Keying it on the basename collided across folders (an
// IMG_0001.jpg in both 2023/ and 2024/ mapped to the same temp path), and
// with concurrent upload workers that meant one worker could overwrite
// another's temp file mid-flight -- uploading one photo's bytes and then
// permanently recording them in the ledger under a DIFFERENT photo's
// SHA-256. Silent, undetectable corruption; a unique path per call is the
// only thing that rules it out.
func PrepareUploadPath(path string, cfg config.Config) (string, func()) {
	if cfg.UploadQuality != "space_saver" {
		return path, noCleanup
	}
	if !IsResizable(path) {
		return path, noCleanup
	}

	img, err := imaging.Open(path, imaging.AutoOrientation(true))
	if err != nil {
		return path, noCleanup
	}

	bounds := img.Bounds()
	w, h := bounds.Dx(), bounds.Dy()
	maxDim := cfg.SpaceSaverMaxDim
	if maxDim <= 0 {
		maxDim = 2048
	}
	if w <= maxDim && h <= maxDim {
		return path, noCleanup
	}

	out := imaging.Fit(img, maxDim, maxDim, imaging.Lanczos)

	tmpDir := filepath.Join(os.TempDir(), "gpsync-space-saver")
	if err := os.MkdirAll(tmpDir, 0o700); err != nil {
		return path, noCleanup
	}
	// imaging.Save only takes a path, so claim a unique one via CreateTemp
	// and close the handle immediately -- Save reopens it by name.
	tmpFile, err := os.CreateTemp(tmpDir, trimExt(filepath.Base(path))+"-*.jpg")
	if err != nil {
		return path, noCleanup
	}
	outPath := tmpFile.Name()
	tmpFile.Close()
	remove := func() { os.Remove(outPath) }

	quality := cfg.SpaceSaverJPEGQuality
	if quality <= 0 {
		quality = 85
	}
	if err := imaging.Save(out, outPath, imaging.JPEGQuality(quality)); err != nil {
		remove()
		return path, noCleanup
	}
	return outPath, remove
}

func trimExt(name string) string {
	ext := filepath.Ext(name)
	return name[:len(name)-len(ext)]
}
