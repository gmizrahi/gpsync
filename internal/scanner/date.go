package scanner

import (
	"regexp"
	"strconv"
	"time"
)

// datePathMinYear/datePathMaxYear bound which years DateFromPath (and the
// ReadMetadata fallback that uses it) will accept as a real capture year --
// wide enough to cover any genuinely old scanned/digitized photo while
// still rejecting nonsense (a stray 4-digit number that isn't a year at
// all, or a corrupted value) that would otherwise produce a wildly wrong
// date.
const datePathMinYear = 1900

func datePathMaxYear() int { return time.Now().Year() + 1 }

var (
	// pathDateSepRe matches a YYYY-MM-DD-shaped date (separated by -, _,
	// or .) anywhere in a path -- the most precise match, since it's
	// unlikely to appear by coincidence. Matches common export/organizing
	// conventions directly, e.g. "2002-07-10-01.jpg" (this project's own
	// real-world source of the bug this file fixes -- see DateFromPath's
	// doc comment).
	pathDateSepRe = regexp.MustCompile(`((?:19|20)\d{2})[-_.](\d{2})[-_.](\d{2})`)
	// pathDateCompactRe matches a bare YYYYMMDD run -- common in
	// phone/app export filenames, e.g. WhatsApp's
	// "IMG-20220701-WA0060.jpg" or "VID-20210603-WA0007.mp4".
	pathDateCompactRe = regexp.MustCompile(`((?:19|20)\d{2})(\d{2})(\d{2})`)
	// pathYearMonthRe matches a year and month with no day: a
	// "2024_05 Trip" or "2024-05" folder, or a bare "202405" (#73). Unlike
	// the two above it must not be part of a longer number, or a counter
	// such as "IMG_200512345" would read as a month.
	pathYearMonthRe = regexp.MustCompile(`(?:^|\D)((?:19|20)\d{2})[-_.]?(0[1-9]|1[0-2])(?:\D|$)`)
	// pathYearOnlyRe is the last resort: just a plausible 4-digit year
	// anywhere in the path, e.g. a library organized as "Photos/2002/...".
	pathYearOnlyRe = regexp.MustCompile(`(?:19|20)\d{2}`)
)

// DateFromPath tries to recover a capture date from a file's own path --
// its folder names and/or filename -- for a photo/video library organized
// by date. It exists because per-year counts came out wrong: investigation
// against a real library found that
// scanner.ReadMetadata's filesystem-mtime fallback (used whenever EXIF is
// missing/unreadable) can drift arbitrarily far from the true capture
// date -- a file named "2002-07-10-01.jpg" had an mtime of March 2026,
// almost certainly from a later backup/migration touching the file long
// after the photo was actually taken. A date embedded in the path itself
// is a far more reliable signal than a demonstrably-unreliable mtime.
//
// Tries, in order: a separated YYYY-MM-DD date anywhere in the path (most
// precise), then a compact YYYYMMDD run (common in phone/app exports),
// then a year and month (the 1st of that month), then a bare plausible
// year with no day/month information (January 1st). Every one is at noon
// UTC -- noon avoids a date-only match shifting to the adjacent calendar
// day when later rendered in a different timezone. Returns false if
// nothing plausible is found anywhere in the path.
func DateFromPath(path string) (time.Time, bool) {
	if m := pathDateSepRe.FindStringSubmatch(path); m != nil {
		if t, ok := buildDateFromParts(m[1], m[2], m[3]); ok {
			return t, true
		}
	}
	if m := pathDateCompactRe.FindStringSubmatch(path); m != nil {
		if t, ok := buildDateFromParts(m[1], m[2], m[3]); ok {
			return t, true
		}
	}
	if m := pathYearMonthRe.FindStringSubmatch(path); m != nil {
		if t, ok := buildDateFromParts(m[1], m[2], "01"); ok {
			return t, true
		}
	}
	if m := pathYearOnlyRe.FindString(path); m != "" {
		if y, ok := plausibleYear(m); ok {
			return YearOnlyDate(y), true
		}
	}
	return time.Time{}, false
}

// YearOnlyDate is the date DateFromPath gives a path that names only a
// year: January 1st, noon UTC. fix-dates recognizes it to refine rows
// dated by it before a path rule could read more.
func YearOnlyDate(y int) time.Time {
	return time.Date(y, time.January, 1, 12, 0, 0, 0, time.UTC)
}

func buildDateFromParts(yearStr, monthStr, dayStr string) (time.Time, bool) {
	y, ok := plausibleYear(yearStr)
	if !ok {
		return time.Time{}, false
	}
	mo, err := strconv.Atoi(monthStr)
	if err != nil || mo < 1 || mo > 12 {
		return time.Time{}, false
	}
	d, err := strconv.Atoi(dayStr)
	if err != nil || d < 1 || d > 31 {
		return time.Time{}, false
	}
	return time.Date(y, time.Month(mo), d, 12, 0, 0, 0, time.UTC), true
}

func plausibleYear(s string) (int, bool) {
	y, err := strconv.Atoi(s)
	if err != nil || y < datePathMinYear || y > datePathMaxYear() {
		return 0, false
	}
	return y, true
}
