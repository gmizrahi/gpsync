package pathx

import "strings"

// DisplayName returns the file name at the end of a path, recognising both
// separators regardless of the host platform.
//
// gpsync is Windows-first, so the ledger normally holds paths like
// `C:\Photos\2026\IMG_0001.jpg`. The dashboard also runs on Linux and macOS
// (`gpsync dashboard`, and a ledger moved between machines by
// `gpsync backup`/`restore`), and there filepath.Base cannot split a Windows
// path at all: `\` is an ordinary character, so the whole path comes back as
// the file name and the folder column collapses to ".". The reverse happens
// for a POSIX path shown on Windows.
//
// These helpers are for rendering only. Anything that touches the filesystem
// must keep using path/filepath, which is correct precisely because it
// follows the host's own rules.
func DisplayName(p string) string {
	if i := strings.LastIndexAny(p, `/\`); i >= 0 {
		return p[i+1:]
	}
	return p
}

// DisplayDir returns everything before the final separator: the folder a file
// is shown as living in. It recognises both separators regardless of host,
// for the reasons given on DisplayName.
//
// A path with no separator has no folder to show, so it reports "." --
// matching filepath.Dir rather than inventing a blank.
func DisplayDir(p string) string {
	i := strings.LastIndexAny(p, `/\`)
	switch {
	case i < 0:
		return "."
	case i == 0:
		// "/photos.jpg" -- the root itself is the folder.
		return p[:1]
	default:
		return p[:i]
	}
}
