// Package extensions provides a best-effort table of which file extensions
// Google Photos accepts. It's deliberately not exhaustive: Google documents
// supported formats loosely and changes them over time. Extensions not in
// either list are attempted and the real outcome is recorded back into the
// extension_stats table (see internal/statedb) — the picture of "what's
// actually unsupported" gets more accurate with real usage instead of
// relying on a hardcoded guess.
//
// The built-in tables can be extended (in either direction) via
// config.toml, without a gpsync code change -- see ApplyUserOverrides, called
// once at startup from cmd/gpsync.
package extensions

import (
	"fmt"
	"path/filepath"
	"strings"
)

type Classification string

const (
	Supported   Classification = "supported"
	Unsupported Classification = "unsupported"
	Unknown     Classification = "unknown"
)

// Kind is the media type Google Photos would treat a supported extension
// as -- used for --media-type filtering (gpsync scan/upload/sync), which
// needs to know not just "is this uploadable" but "is this a photo or a
// video". KindUnknown (the zero value) means the extension isn't a
// recognized supported type at all.
type Kind string

const (
	KindPhoto   Kind = "photo"
	KindVideo   Kind = "video"
	KindUnknown Kind = ""
)

// Per Google's published Photos file-type support (images, RAW formats, video).
var supportedExts = map[string]Kind{
	// Common images
	"jpg": KindPhoto, "jpeg": KindPhoto, "png": KindPhoto, "webp": KindPhoto,
	"gif": KindPhoto, "tiff": KindPhoto, "tif": KindPhoto, "heic": KindPhoto,
	"heif": KindPhoto,
	// RAW image formats Google Photos accepts
	"3fr": KindPhoto, "arw": KindPhoto, "cr2": KindPhoto, "cr3": KindPhoto,
	"crw": KindPhoto, "dng": KindPhoto, "erf": KindPhoto, "kdc": KindPhoto,
	"mrw": KindPhoto, "nef": KindPhoto, "nrw": KindPhoto, "orf": KindPhoto,
	"pef": KindPhoto, "raf": KindPhoto, "raw": KindPhoto, "rw2": KindPhoto,
	"sr2": KindPhoto, "srw": KindPhoto,
	// Video
	"mp4": KindVideo, "mov": KindVideo, "wmv": KindVideo, "avi": KindVideo,
	"3gp": KindVideo, "3g2": KindVideo, "asf": KindVideo, "divx": KindVideo,
	"m2t": KindVideo, "m2ts": KindVideo, "m4v": KindVideo, "mkv": KindVideo,
	"mmv": KindVideo, "mod": KindVideo, "mpg": KindVideo, "mpeg": KindVideo,
	"mts": KindVideo, "tod": KindVideo, "mxf": KindVideo,
}

// Formats commonly found in a personal photo library that Google Photos is
// known NOT to accept — rejected before ever spending a network request.
var unsupportedExts = map[string]bool{
	"bmp": true, "svg": true, "psd": true, "ai": true, "eps": true,
	"ico": true, "avif": true, "xmp": true, "thm": true, "aae": true,
	"pdf": true, "ppt": true, "pptx": true, "xml": true, "arg": true,
}

// extraSupported/extraUnsupported hold the user's own config.toml
// additions on top of the built-in tables above, installed once at startup
// by ApplyUserOverrides. A user entry always wins over a conflicting
// built-in classification for the same extension -- see Classify/KindOf.
var (
	extraSupported   = map[string]Kind{}
	extraUnsupported = map[string]bool{}
)

// ApplyUserOverrides installs the user's own extension overrides from
// config.toml (ExtraSupportedPhotoExtensions/ExtraSupportedVideoExtensions/
// ExtraUnsupportedExtensions), replacing whatever was installed before --
// meant to be called once per process at startup (see cmd/gpsync's
// applyExtensionOverrides), not accumulated across calls. nil/empty slices
// are a valid, harmless "no overrides configured" and clear any previous
// call's overrides (needed for tests that call this more than once).
func ApplyUserOverrides(extraSupportedPhoto, extraSupportedVideo, extraUnsupportedList []string) {
	supported := make(map[string]Kind, len(extraSupportedPhoto)+len(extraSupportedVideo))
	for _, e := range extraSupportedPhoto {
		if n := normalizeExt(e); n != "" {
			supported[n] = KindPhoto
		}
	}
	for _, e := range extraSupportedVideo {
		if n := normalizeExt(e); n != "" {
			supported[n] = KindVideo
		}
	}
	unsupported := make(map[string]bool, len(extraUnsupportedList))
	for _, e := range extraUnsupportedList {
		if n := normalizeExt(e); n != "" {
			unsupported[n] = true
		}
	}
	extraSupported = supported
	extraUnsupported = unsupported
}

// normalizeExt matches ExtOf's own convention (lowercase, no leading dot),
// so a config.toml entry of "PPT", "ppt", or ".ppt" all resolve to the same
// key a real scanned path would produce.
func normalizeExt(e string) string {
	return strings.ToLower(strings.TrimPrefix(strings.TrimSpace(e), "."))
}

// ExtOf returns the lowercased extension without its leading dot, or
// "(none)" if the path has no extension.
func ExtOf(path string) string {
	ext := strings.ToLower(strings.TrimPrefix(filepath.Ext(path), "."))
	if ext == "" {
		return "(none)"
	}
	return ext
}

// Classify returns whether path's extension is known-supported,
// known-unsupported, or unknown to Google Photos. User config
// (ApplyUserOverrides) is checked first, so it always wins over a
// conflicting built-in entry.
func Classify(path string) Classification {
	ext := ExtOf(path)
	if _, ok := extraSupported[ext]; ok {
		return Supported
	}
	if extraUnsupported[ext] {
		return Unsupported
	}
	if _, ok := supportedExts[ext]; ok {
		return Supported
	}
	if unsupportedExts[ext] {
		return Unsupported
	}
	return Unknown
}

// ParseKind maps a --media-type flag value to a Kind, for filtering. ""
// and "all" both mean KindUnknown, i.e. no filtering -- the default. Accepts
// singular or plural ("photo"/"photos", "video"/"videos") since either
// reads naturally on a command line.
func ParseKind(s string) (Kind, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "all":
		return KindUnknown, nil
	case "photo", "photos":
		return KindPhoto, nil
	case "video", "videos":
		return KindVideo, nil
	default:
		return KindUnknown, fmt.Errorf("invalid --media-type %q: want \"photos\", \"videos\", or \"all\"", s)
	}
}

// KindOf returns the media kind path's extension is a recognized supported
// type for (built-in or user-configured), or KindUnknown if it isn't --
// used by --media-type filtering. An extension classified Unsupported or
// Unknown always reports KindUnknown here, never a guess.
func KindOf(path string) Kind {
	ext := ExtOf(path)
	if k, ok := extraSupported[ext]; ok {
		return k
	}
	if k, ok := supportedExts[ext]; ok {
		return k
	}
	return KindUnknown
}
