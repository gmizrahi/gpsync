package engine

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gmizrahi/gpsync/internal/pathx"
	"github.com/gmizrahi/gpsync/internal/scanner"
	"github.com/gmizrahi/gpsync/internal/statedb"
)

// MissingResolveSummary reports what one ResolveMissingFiles pass did.
type MissingResolveSummary struct {
	// NewlyFlagged entries were found gone during this pass. They are
	// decided in the same pass, so they are also counted below.
	NewlyFlagged int
	// Reappeared entries' files are back at their recorded path.
	Reappeared int
	// Relocated entries were found, same content, at another path under
	// the source folders and now point there.
	Relocated int
	// Confirmed entries are newly confirmed missing: gone from every
	// source folder. gpsync recheck --missing forgets them.
	Confirmed int
	// Deferred entries could not be decided yet: a file of the same size
	// under the source folders has not been hashed, and it may be this one,
	// moved. The next scan hashes it and the next pass decides.
	Deferred int
	// Outside entries are not under any configured source folder, so no
	// pass over those folders can say where they went. Left alone.
	Outside int
	// StaleCache counts scan-cache entries dropped for paths that no longer
	// exist.
	StaleCache int
	// Skipped is set when nothing could be decided at all -- a source folder
	// is unavailable, or so much of the library looks missing that an
	// unavailable drive is likelier than real deletions. SkipReason says why.
	Skipped    bool
	SkipReason string
}

// Changed reports whether the pass changed anything worth telling the user.
func (s MissingResolveSummary) Changed() bool {
	return s.Reappeared > 0 || s.Relocated > 0 || s.Confirmed > 0
}

// Too many confirmations at once usually means a drive or folder went
// missing, not that the user deleted files. Both thresholds must be crossed:
// a small library can legitimately lose most of its files, and a large one
// can legitimately lose a few hundred.
const (
	missingConfirmFloor              = 200
	missingConfirmSuspiciousFraction = 0.2
)

// ResolveMissingIfNeeded runs ResolveMissingFiles unless onlyIfUndecided is
// set and nothing is currently flagged, in which case it reports ran=false
// without touching the disk.
//
// The guard exists because resolving walks every source folder: worth it
// after a scan or a sync, wasteful on a watch heartbeat that fires every few
// minutes with nothing new to decide. Callers differ only in how they present
// the result -- the CLI prints it, the tray logs it -- so the decision lives
// here rather than being copied into both.
func ResolveMissingIfNeeded(db *statedb.DB, sourceFolders []string, onlyIfUndecided bool, now time.Time) (sum MissingResolveSummary, ran bool, err error) {
	if onlyIfUndecided {
		c, cerr := db.MissingCounts()
		if cerr != nil {
			return sum, false, cerr
		}
		if c.Flagged == 0 {
			return sum, false, nil
		}
	}
	sum, err = ResolveMissingFiles(db, sourceFolders, now)
	return sum, true, err
}

// ResolveMissingFiles finds ledger entries whose file is gone and decides
// them.
//
// It walks the source folders once -- directory listings and file sizes, no
// hashing -- and flags every entry under them whose file no longer exists.
// Scans flag too, but only under the folders they scan; a deleted or renamed
// folder is never scanned again, so only this walk catches it.
//
// Each flagged entry then ends up in one of four places:
//   - its file is back at the recorded path: the flag is cleared;
//   - its content is indexed at another current path under the source
//     folders (a move or rename): the entry is repointed and the flag cleared;
//   - a same-size file under the source folders that a scan would hash has
//     not been hashed yet, so a move cannot be ruled out: left for a later
//     pass;
//   - otherwise: confirmed missing.
//
// Already-confirmed entries go through the same checks, so a reattached
// drive or a late-discovered move undoes a confirmation, and one that can no
// longer be decided goes back to flagged.
//
// Nothing is ever deleted from the ledger. It does nothing at all if any
// source folder is unavailable, since a file could have moved there.
func ResolveMissingFiles(db *statedb.DB, sourceFolders []string, now time.Time) (MissingResolveSummary, error) {
	var sum MissingResolveSummary

	roots, unavailable := resolveSourceRoots(sourceFolders)
	if len(unavailable) > 0 {
		sum.Skipped = true
		sum.SkipReason = fmt.Sprintf("source folder not available: %s", strings.Join(unavailable, ", "))
		return sum, nil
	}
	if len(roots) == 0 {
		sum.Skipped = true
		sum.SkipReason = "no source folders configured"
		return sum, nil
	}

	idx, err := indexSourceFolders(db, roots)
	if err != nil {
		return sum, err
	}
	nowF := float64(now.UnixNano()) / 1e9
	if err := flagGoneUnderRoots(db, roots, idx, nowF, &sum); err != nil {
		return sum, err
	}

	flagged, err := db.MissingFlagged()
	if err != nil || len(flagged) == 0 {
		return sum, err
	}

	var gone []statedb.MissingCandidate
	for _, c := range flagged {
		if !underAnyRoot(c.Path, roots) {
			sum.Outside++
			continue
		}
		if exists, known := pathExists(c.Path); !known {
			sum.Deferred++
			continue
		} else if exists {
			if err := db.ClearMissing(c.SHA256); err != nil {
				return sum, err
			}
			sum.Reappeared++
			continue
		}
		if newPath := idx.relocation(c); newPath != "" {
			if err := db.UpdateFirstSourcePath(c.SHA256, newPath); err != nil {
				return sum, err
			}
			if err := db.ClearMissing(c.SHA256); err != nil {
				return sum, err
			}
			sum.Relocated++
			continue
		}
		if idx.mayBeUnhashedCopy(c.Size) {
			if c.Confirmed {
				if err := db.UnconfirmMissing(c.SHA256); err != nil {
					return sum, err
				}
			}
			sum.Deferred++
			continue
		}
		if !c.Confirmed {
			gone = append(gone, c)
		}
	}

	total, err := db.CountUploads()
	if err != nil {
		return sum, err
	}
	if len(gone) >= missingConfirmFloor && float64(len(gone)) >= missingConfirmSuspiciousFraction*float64(total) {
		sum.Skipped = true
		sum.SkipReason = fmt.Sprintf("%d of %d files look missing (over %.0f%%), which usually means an unavailable drive "+
			"rather than deleted files; none were confirmed", len(gone), total, missingConfirmSuspiciousFraction*100)
		return sum, nil
	}

	for _, c := range gone {
		if err := db.ConfirmMissing(c.SHA256, nowF); err != nil {
			return sum, err
		}
		sum.Confirmed++
	}
	return sum, nil
}

// flagGoneUnderRoots flags entries under the source folders whose file is
// gone, and drops scan-cache entries for vanished paths. A path the walk did
// not list is stat'ed first (the walk skips ignored folders), and only a
// definite "does not exist" counts.
func flagGoneUnderRoots(db *statedb.DB, roots []string, idx *sourceIndex, now float64, sum *MissingResolveSummary) error {
	rows, err := db.UploadsUnder(roots)
	if err != nil {
		return err
	}
	for _, r := range rows {
		if r.Flagged || idx.onDisk[pathx.Key(r.Path)] {
			continue
		}
		if exists, known := pathExists(r.Path); exists || !known {
			continue
		}
		if err := db.MarkMissing(r.SHA256, now); err != nil {
			return err
		}
		sum.NewlyFlagged++
	}
	for _, p := range idx.cachedPaths {
		if idx.onDisk[pathx.Key(p)] {
			continue
		}
		if exists, known := pathExists(p); exists || !known {
			continue
		}
		if err := db.DeleteFileSeen(p); err != nil {
			return err
		}
		sum.StaleCache++
	}
	return nil
}

// sourceIndex is one listing of the source folders, checked against the
// scan cache.
type sourceIndex struct {
	// onDisk holds every regular file the walk found, by pathKey.
	onDisk map[string]bool
	// cachedPaths are the scan-cache entries under the source folders.
	cachedPaths []string
	// hashed maps content hash to the current paths holding it, from
	// scan-cache entries whose size and mtime still match the file on disk.
	hashed map[string][]string
	// unhashedSizes counts, by size, files a scan would hash that have no
	// up-to-date scan-cache entry. Files a scan never hashes (OS metadata,
	// unsupported formats) are left out: they can never be a moved ledger
	// entry, and counting them deferred deletions forever.
	unhashedSizes map[int64]int
	unhashed      int
}

func indexSourceFolders(db *statedb.DB, roots []string) (*sourceIndex, error) {
	cached, err := db.FilesSeenUnder(roots)
	if err != nil {
		return nil, err
	}
	idx := &sourceIndex{
		onDisk:        map[string]bool{},
		cachedPaths:   make([]string, 0, len(cached)),
		hashed:        map[string][]string{},
		unhashedSizes: map[int64]int{},
	}
	byPath := make(map[string]statedb.FileSeenRow, len(cached))
	for _, r := range cached {
		byPath[pathx.Key(r.Path)] = r
		idx.cachedPaths = append(idx.cachedPaths, r.Path)
	}

	for _, root := range roots {
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if d.IsDir() {
				if path != root && scanner.IsIgnoredDirName(d.Name()) {
					return filepath.SkipDir
				}
				return nil
			}
			info, err := d.Info()
			if err != nil || !info.Mode().IsRegular() {
				return nil
			}
			key := pathx.Key(path)
			idx.onDisk[key] = true
			mtime := float64(info.ModTime().UnixNano()) / 1e9
			if r, ok := byPath[key]; ok && r.SHA256 != "" && r.Size == info.Size() && r.Mtime == mtime {
				idx.hashed[r.SHA256] = append(idx.hashed[r.SHA256], path)
				return nil
			}
			if scanner.WouldHash(path) {
				idx.unhashedSizes[info.Size()]++
				idx.unhashed++
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	return idx, nil
}

func (idx *sourceIndex) relocation(c statedb.MissingCandidate) string {
	for _, p := range idx.hashed[c.SHA256] {
		if !pathx.Same(p, c.Path) {
			return p
		}
	}
	return ""
}

func (idx *sourceIndex) mayBeUnhashedCopy(size int64) bool {
	if size <= 0 {
		return idx.unhashed > 0
	}
	return idx.unhashedSizes[size] > 0
}

// resolveSourceRoots expands configured source folders (plain folders or
// globs) to existing directories, and lists the ones that are not there.
func resolveSourceRoots(patterns []string) (roots, unavailable []string) {
	for _, pattern := range patterns {
		pattern = strings.TrimRight(pattern, `/\`)
		matches, err := filepath.Glob(pattern)
		if err != nil || len(matches) == 0 {
			matches = []string{pattern}
		}
		found := false
		for _, m := range matches {
			if info, err := os.Stat(m); err == nil && info.IsDir() {
				if abs, err := filepath.Abs(m); err == nil {
					m = abs
				}
				roots = append(roots, m)
				found = true
			}
		}
		if !found {
			unavailable = append(unavailable, pattern)
		}
	}
	return roots, unavailable
}

// pathExists reports whether path exists. known is false when the answer is
// unclear (permissions, a flaky disk), which must never count as deleted.
func pathExists(path string) (exists, known bool) {
	_, err := os.Stat(path)
	if err == nil {
		return true, true
	}
	if errors.Is(err, fs.ErrNotExist) {
		return false, true
	}
	return false, false
}

func underAnyRoot(path string, roots []string) bool {
	for _, r := range roots {
		prefix := strings.TrimRight(r, `/\`) + string(filepath.Separator)
		if len(path) > len(prefix) && pathx.Same(path[:len(prefix)], prefix) {
			return true
		}
	}
	return false
}
