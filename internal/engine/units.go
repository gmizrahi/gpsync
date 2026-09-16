package engine

import (
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/gmizrahi/gpsync/internal/scanner"
)

// DiscoverSyncUnits expands glob patterns and walks every subfolder level
// under each match, returning the sorted, deduped set of directories that
// directly contain at least one file -- the actual units `gpsync sync`/`gpsync
// scan` process one at a time. A parent folder with many levels of
// subfolders (or a glob matching several of them) expands to all of their
// file-containing leaves.
func DiscoverSyncUnits(patterns []string) ([]string, error) {
	var roots []string
	for _, pattern := range patterns {
		// A trailing slash/backslash (e.g. "...\2024_01*\", common from copy-pasting
		// a path in Explorer/PowerShell) is meaningless for matching purposes and
		// can confuse Glob -- strip it before matching.
		pattern = strings.TrimRight(pattern, `/\`)
		matches, err := filepath.Glob(pattern)
		if err != nil {
			return nil, err
		}
		if len(matches) == 0 {
			matches = []string{pattern}
		}
		roots = append(roots, matches...)
	}

	seen := map[string]bool{}
	var units []string
	for _, root := range roots {
		info, err := os.Stat(root)
		if err != nil || !info.IsDir() {
			continue
		}
		err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil || !d.IsDir() {
				return nil
			}
			if scanner.IsIgnoredDirName(d.Name()) {
				return filepath.SkipDir
			}
			entries, err := os.ReadDir(path)
			if err != nil {
				return nil
			}
			for _, e := range entries {
				if !e.IsDir() && !scanner.IsIgnoredFileName(e.Name()) {
					abs, absErr := filepath.Abs(path)
					if absErr != nil {
						abs = path
					}
					if !seen[abs] {
						seen[abs] = true
						units = append(units, abs)
					}
					break
				}
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	sort.Strings(units)
	return units, nil
}

// DiscoverMarkTargets splits patterns into the folders to scan whole and
// the individual files named directly -- what `gpsync mark-synced` needs
// once it accepts folders, files and globs alike.
//
// The split matters because the two are handled differently downstream.
// A folder is scanned recursively and its leaves reported one at a time;
// a named file is taken exactly as given. Crucially, --quality then has to
// match the files by EXACT path rather than by folder prefix: the prefix
// form turns "...\*.mp4" into "...\*.mp4\%", which matches nothing, so
// recording quality would silently do nothing for precisely the argument
// shape this function was added to support.
//
// A pattern matching neither (a typo, or a folder that does not exist) is
// dropped here and surfaces as "nothing found" by the caller, matching
// DiscoverSyncUnits' own behaviour rather than failing the whole command
// over one bad argument among several good ones.
func DiscoverMarkTargets(patterns []string) (folders, files []string, err error) {
	seenFile := map[string]bool{}
	var folderPatterns []string
	for _, pattern := range patterns {
		trimmed := strings.TrimRight(pattern, `/\`)
		matches, gerr := filepath.Glob(trimmed)
		if gerr != nil {
			return nil, nil, gerr
		}
		if len(matches) == 0 {
			matches = []string{trimmed}
		}
		for _, m := range matches {
			info, serr := os.Stat(m)
			if serr != nil {
				continue
			}
			if info.IsDir() {
				// Hand the ORIGINAL pattern to DiscoverSyncUnits rather
				// than this one match, so its own glob handling stays the
				// single source of truth for folder expansion.
				folderPatterns = append(folderPatterns, m)
				continue
			}
			abs, aerr := filepath.Abs(m)
			if aerr != nil {
				abs = m
			}
			if !seenFile[abs] {
				seenFile[abs] = true
				files = append(files, abs)
			}
		}
	}
	if len(folderPatterns) > 0 {
		folders, err = DiscoverSyncUnits(folderPatterns)
		if err != nil {
			return nil, nil, err
		}
	}
	sort.Strings(files)
	return folders, files, nil
}

// ResolveWatchRoots expands patterns (globs, or plain folder paths) into
// the existing directories they match -- the SAME glob-resolution
// DiscoverSyncUnits does internally, before IT narrows further down to
// leaf (file-containing) folders. gpsync watch/gpsync-tray need the untrimmed
// directories themselves, not just their leaves: they have to watch every
// intermediate folder too, so a brand-new leaf created later (e.g. a whole
// new year) gets picked up without a restart.
func ResolveWatchRoots(patterns []string) ([]string, error) {
	seen := map[string]bool{}
	var roots []string
	for _, pattern := range patterns {
		pattern = strings.TrimRight(pattern, `/\`)
		matches, err := filepath.Glob(pattern)
		if err != nil {
			return nil, err
		}
		if len(matches) == 0 {
			matches = []string{pattern}
		}
		for _, m := range matches {
			info, err := os.Stat(m)
			if err != nil || !info.IsDir() {
				continue
			}
			abs, absErr := filepath.Abs(m)
			if absErr != nil {
				abs = m
			}
			if !seen[abs] {
				seen[abs] = true
				roots = append(roots, abs)
			}
		}
	}
	sort.Strings(roots)
	return roots, nil
}
