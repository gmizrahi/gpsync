// Package pathx compares filesystem paths the way the host filesystem does.
//
// gpsync matches paths it walked against paths recorded in the ledger, and on
// a case-insensitive filesystem those can differ only in case ("C:\Photos"
// vs "c:\photos", "/Users/me/Photos/IMG.JPG" vs ".../img.jpg") while naming
// the same file. Comparing them byte-for-byte there loses files -- the
// missing-file sweep would report a file as gone while it sits on disk.
// Comparing case-insensitively on Linux would do the opposite and merge two
// genuinely different files.
package pathx

import (
	"runtime"
	"strings"
)

// caseInsensitive reports whether this platform's filesystem ignores case by
// default: NTFS on Windows and APFS/HFS+ on macOS both do. Linux does not.
//
// It is a variable rather than a constant so tests can exercise both
// behaviours regardless of the machine they run on.
var caseInsensitive = runtime.GOOS == "windows" || runtime.GOOS == "darwin"

// Key normalises path for use as a map key, so two spellings of one file on
// a case-insensitive filesystem collapse to the same entry.
func Key(path string) string {
	if caseInsensitive {
		return strings.ToLower(path)
	}
	return path
}

// Same reports whether a and b name the same path on this platform.
func Same(a, b string) bool {
	if caseInsensitive {
		return strings.EqualFold(a, b)
	}
	return a == b
}
