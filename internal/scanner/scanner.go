// Package scanner walks source folders, hashes new/changed files, and
// registers them as pending uploads. Re-scans are fast: files already known
// by (path, mtime, size) skip re-hashing entirely.
package scanner

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/rwcarlsen/goexif/exif"

	"github.com/gmizrahi/gpsync/internal/extensions"
	"github.com/gmizrahi/gpsync/internal/hashing"
	"github.com/gmizrahi/gpsync/internal/pathx"
	"github.com/gmizrahi/gpsync/internal/statedb"
)

// statFn is os.Stat, indirected so a test can substitute a deliberately
// slow stand-in and prove the pre-hash stat pass in ScanFolders actually
// runs concurrently -- same "package var, tests can drive the clock/IO
// instead of really waiting" pattern as internal/uploader's sleepFn/waitFn.
var statFn = os.Stat

// ignoreNames are files, never real photo/video content, skipped wherever found.
var ignoreNames = map[string]bool{
	"thumbs.db": true, "desktop.ini": true, ".ds_store": true,
	"picasa.ini": true, ".picasa.ini": true,
}

// ignoreDirs are whole directory trees pruned from the walk entirely --
// Picasa's ".picasaoriginals" stores a pre-edit backup copy of every photo
// it ever touched in the parent folder, which would otherwise get hashed
// and backed up as pure duplicate noise.
var ignoreDirs = map[string]bool{
	".picasaoriginals": true,
}

// extraIgnoreNames/extraIgnoreDirs are the user's own config.toml
// additions on top of the built-in tables above, installed once at startup
// by ApplyUserIgnoreOverrides.
var (
	extraIgnoreNames = map[string]bool{}
	extraIgnoreDirs  = map[string]bool{}
)

// ApplyUserIgnoreOverrides installs the user's own ignored file/directory
// names from config.toml (ExtraIgnoredFileNames/ExtraIgnoredDirNames),
// replacing whatever was installed before -- meant to be called once per
// process at startup (see cmd/gpsync's applyExtensionOverrides), mirroring
// extensions.ApplyUserOverrides. Matched case-insensitively, same as the
// built-in tables.
func ApplyUserIgnoreOverrides(fileNames, dirNames []string) {
	extraIgnoreNames = normalizeNameSet(fileNames)
	extraIgnoreDirs = normalizeNameSet(dirNames)
}

func normalizeNameSet(names []string) map[string]bool {
	out := make(map[string]bool, len(names))
	for _, n := range names {
		n = strings.ToLower(strings.TrimSpace(n))
		if n != "" {
			out[n] = true
		}
	}
	return out
}

// IsIgnoredFileName reports whether a file name is known OS/app junk (or a
// user-configured extra), never real media -- exported so callers walking
// the tree independently (e.g. `gpsync sync`'s folder discovery) apply the
// exact same exclusions as scan.
func IsIgnoredFileName(name string) bool {
	n := strings.ToLower(name)
	return ignoreNames[n] || extraIgnoreNames[n]
}

// IsIgnoredDirName reports whether a directory name's whole subtree should
// be skipped (e.g. Picasa's originals backup folder, or a user-configured
// extra).
func IsIgnoredDirName(name string) bool {
	n := strings.ToLower(name)
	return ignoreDirs[n] || extraIgnoreDirs[n]
}

// IsInOriginalsFolder reports whether path's IMMEDIATE parent directory is
// literally named "originals" (case-insensitive) -- a plain, visible
// folder some photo editors use for a pre-edit backup copy, distinct from
// the hidden ".picasaoriginals" folder above (which is pruned from the
// walk entirely, never even hashed). Files in here usually share a
// filename with an edited copy already tracked elsewhere
// in the library, but different bytes (a different content hash), so
// auto-uploading them produced pure duplicate noise in Google Photos.
//
// Deliberately checks only the IMMEDIATE parent, not any ancestor -- an
// "originals" folder several levels up (or an unrelated folder that
// happens to be named "Originals Backup" or similar) must not match. This
// is intentionally narrower than a global "does this path contain an
// originals segment anywhere" check for the same reason
// OriginalsFolderRowsPendingOrFailed's own SQL pre-filter doc comment
// explains isn't good enough on its own: false positives here would
// silently route a genuinely new, unrelated photo into the review queue
// instead of the normal upload queue.
func IsInOriginalsFolder(path string) bool {
	return strings.EqualFold(filepath.Base(filepath.Dir(path)), "originals")
}

// SiblingPath returns the path a file inside an "originals" folder would
// have if the edited copy sits right next to the originals folder itself
// -- the same basename, one directory up from "originals" (i.e. skipping
// past that one path segment). Only meaningful when
// IsInOriginalsFolder(path) is true; the caller is expected to have
// already checked that.
//
// This is the deterministic, unambiguous case: a file inside an originals
// folder whose edited copy sits at the immediate parent level needs no
// judgement -- the same convention Picasa follows with its
// .picasaoriginals subfolder. A file whose sibling at this
// EXACT path is already tracked needs no review at all -- only a file
// with no immediate-parent sibling, but a same-named match SOMEWHERE ELSE
// in the library, is genuinely ambiguous enough to need a human (see
// engine.AutoResolveObviousOriginals).
func SiblingPath(path string) string {
	return filepath.Join(filepath.Dir(filepath.Dir(path)), filepath.Base(path))
}

// mediaKindFilter, set via SetMediaKindFilter, restricts ExpandFolders to
// one media kind -- KindUnknown (the zero value, and the default for every
// command unless --media-type says otherwise) means no filtering at all.
// Package-level rather than threaded through ExpandFolders/ScanFolders'
// already-long parameter lists, mirroring the same pattern
// ApplyUserOverrides/ApplyUserIgnoreOverrides use -- safe here because gpsync
// is a fresh process per invocation, so there's nothing to reset between
// real runs (only tests need to reset it explicitly).
var mediaKindFilter extensions.Kind

// SetMediaKindFilter sets or clears (KindUnknown) the active --media-type
// restriction for subsequent ExpandFolders/ScanFolders calls in this
// process.
func SetMediaKindFilter(k extensions.Kind) { mediaKindFilter = k }

var mimeByExt = map[string]string{
	"jpg": "image/jpeg", "jpeg": "image/jpeg", "png": "image/png",
	"webp": "image/webp", "gif": "image/gif", "heic": "image/heic",
	"heif": "image/heif", "tiff": "image/tiff", "tif": "image/tiff",
	"mp4": "video/mp4", "mov": "video/quicktime", "avi": "video/x-msvideo",
	"wmv": "video/x-ms-wmv", "3gp": "video/3gpp", "mkv": "video/x-matroska",
	"mts": "video/mp2t", "m4v": "video/x-m4v", "mpg": "video/mpeg",
	"mpeg": "video/mpeg",
}

// exifCapableExts: goexif only decodes JPEG/TIFF-container EXIF. Everything
// else (PNG, HEIC, RAW, video) falls back to filesystem mtime for capture date.
var exifCapableExts = map[string]bool{"jpg": true, "jpeg": true, "tif": true, "tiff": true}

func GuessMime(path string) string {
	if m, ok := mimeByExt[extensions.ExtOf(path)]; ok {
		return m
	}
	return "application/octet-stream"
}

// ExpandFolders expands glob patterns / plain directory paths into a
// sorted, deduped list of absolute file paths, recursively, skipping known
// OS/Picasa junk. onSkip, if non-nil, is called synchronously and
// immediately (not batched) for every file or directory tree skipped, so
// the caller can show it live during the walk rather than only in a
// summary afterward.
func ExpandFolders(patterns []string, onSkip func(path, reason string)) ([]string, error) {
	seen := map[string]bool{}
	var out []string

	add := func(p string) {
		abs, err := filepath.Abs(p)
		if err != nil {
			abs = p
		}
		if !seen[abs] {
			seen[abs] = true
			out = append(out, abs)
		}
	}

	for _, pattern := range patterns {
		matches, err := filepath.Glob(pattern)
		if err != nil {
			return nil, err
		}
		if len(matches) == 0 {
			matches = []string{pattern}
		}
		for _, m := range matches {
			info, err := os.Stat(m)
			if err != nil {
				continue
			}
			if info.IsDir() {
				_ = filepath.WalkDir(m, func(path string, d fs.DirEntry, err error) error {
					if err != nil {
						return nil
					}
					if d.IsDir() {
						if IsIgnoredDirName(d.Name()) {
							if onSkip != nil {
								onSkip(path, "ignored directory")
							}
							return filepath.SkipDir
						}
						return nil
					}
					if !fileIsScannable(path, onSkip) {
						return nil
					}
					add(path)
					return nil
				})
			} else {
				// A directly-named file (or a file glob like "*.mp4")
				// goes through the IDENTICAL gates as one found by the
				// walk above. It used to be added unconditionally, which
				// only stayed harmless while every caller passed folders:
				// the moment `gpsync mark-synced` learned to take files
				// and globs, an unconditional add would let a
				// named .pdf be recorded as synced, something scanning
				// its containing folder would never do.
				if fileIsScannable(m, onSkip) {
					add(m)
				}
			}
		}
	}
	sort.Strings(out)
	return out, nil
}

// WouldHash reports whether a scan would hash this file and give it a ledger
// entry, ignoring any --media-type filter: it is not an OS/app metadata file
// and not a known-unsupported format.
func WouldHash(path string) bool {
	return !IsIgnoredFileName(filepath.Base(path)) && extensions.Classify(path) != extensions.Unsupported
}

// fileIsScannable applies the three content gates every file must pass to
// earn a ledger row: not an OS/app metadata file, not a known-unsupported
// extension, and matching the active --media-type filter. Shared by
// ExpandFolders' walk and its directly-named-file branch so the two can
// never disagree about what counts as media.
func fileIsScannable(path string, onSkip func(path, reason string)) bool {
	if IsIgnoredFileName(filepath.Base(path)) {
		if onSkip != nil {
			onSkip(path, "OS/app metadata file, not media")
		}
		return false
	}
	// Known-unsupported extensions (ppt, pdf, bmp, ...) are
	// gated out HERE, before the file is ever hashed or
	// given a ledger row -- not just at upload time (see
	// uploader.Run's identical Classify check on rows it
	// pulls from the ledger). Filtering only at upload time
	// meant every known-unsupported file cycled through a
	// full scan/hash pass and a real 'pending' row on every
	// single scan, existing purely to be marked
	// failed_permanent on the next upload -- wasted I/O on
	// files that were never going anywhere, and it meant a
	// `gpsync recheck --unsupported` cleanup got silently
	// undone by the very next `gpsync scan` of the same
	// folder, since scanning would just re-register the
	// same hash as pending again.
	if extensions.Classify(path) == extensions.Unsupported {
		if onSkip != nil {
			onSkip(path, fmt.Sprintf("known-unsupported extension '.%s'", extensions.ExtOf(path)))
		}
		return false
	}
	// --media-type: exclude anything that isn't a recognized
	// match for the requested kind, INCLUDING extensions
	// that are supported but the other kind, or Unknown --
	// "photos only" means only confirmed photos pass, not
	// "not confirmed video".
	if mediaKindFilter != extensions.KindUnknown && extensions.KindOf(path) != mediaKindFilter {
		if onSkip != nil {
			onSkip(path, fmt.Sprintf("media-type filter: %s only", mediaKindFilter))
		}
		return false
	}
	return true
}

func captureDate(path string) *float64 {
	if exifCapableExts[extensions.ExtOf(path)] {
		if f, err := os.Open(path); err == nil {
			defer f.Close()
			if x, err := exif.Decode(f); err == nil {
				for _, field := range []exif.FieldName{exif.DateTimeOriginal, exif.DateTime} {
					if tag, err := x.Get(field); err == nil {
						if s, err := tag.StringVal(); err == nil {
							if t, err := time.ParseInLocation("2006:01:02 15:04:05", s, time.Local); err == nil {
								ts := float64(t.Unix())
								return &ts
							}
						}
					}
				}
			}
		}
	}
	// A date embedded in the file's own path (folder names, filename) --
	// tried BEFORE the filesystem-mtime fallback below, since mtime is
	// demonstrably unreliable: files whose own filenames stated their
	// capture date ("2002-07-10-01.jpg") had an mtime drifted decades
	// forward, almost certainly from a
	// later backup/migration touching the file long after it was taken.
	// See DateFromPath's own doc comment for the full story.
	if t, ok := DateFromPath(path); ok {
		ts := float64(t.Unix())
		return &ts
	}
	// Last resort: filesystem mtime.
	if info, err := os.Stat(path); err == nil {
		ts := float64(info.ModTime().Unix())
		return &ts
	}
	return nil
}

type ScanSummary struct {
	FilesSeen   int
	FilesHashed int
	NewPending  int
	Folders     int
	Skipped     int // OS/Picasa junk AND known-unsupported extensions, gated out before hashing (see onSkip)
	// Missing counts ledger entries under this scan whose file is no
	// longer at its recorded path. They are flagged, never deleted -- see
	// sweepMissing.
	Missing int
	// Reappeared counts previously flagged entries whose file is back.
	Reappeared int
	// StaleCache counts scan-cache entries dropped for paths that no
	// longer exist.
	StaleCache int
}

// wasNewlyRegistered reports whether this hash's ledger entry meaningfully
// changed as a result of this scan: either it didn't exist before (plain
// scan or mark-synced), or -- mark-synced only -- it existed as 'pending'
// and just got promoted to synced. A hash that was already uploaded, already
// mark-synced, or in a failed state is left untouched either way, so it
// doesn't count.
func wasNewlyRegistered(existing *statedb.Upload, markSynced bool) bool {
	if existing == nil {
		return true
	}
	return markSynced && existing.Status == "pending"
}

// ProgressEvent is reported as each file finishes hashing, so the caller
// can show which file(s) are currently being processed, not just a bare
// counter. InFlight lists every file actively being hashed right now
// (there can be more than one with concurrency > 1) sorted for stable
// display, not just the one that most recently finished.
type ProgressEvent struct {
	Done, Total int
	CurrentFile string
	InFlight    []string
}

// inFlightTracker records which files are actively being hashed right now,
// for progress display -- concurrency > 1 means more than one at a time.
type inFlightTracker struct {
	mu    sync.Mutex
	files map[string]bool
}

func newInFlightTracker() *inFlightTracker {
	return &inFlightTracker{files: map[string]bool{}}
}

func (t *inFlightTracker) start(path string) {
	t.mu.Lock()
	t.files[path] = true
	t.mu.Unlock()
}

func (t *inFlightTracker) finish(path string) {
	t.mu.Lock()
	delete(t.files, path)
	t.mu.Unlock()
}

func (t *inFlightTracker) snapshot() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]string, 0, len(t.files))
	for f := range t.files {
		out = append(out, f)
	}
	sort.Strings(out)
	return out
}

// hashJob is the result of hashing+reading capture-date for one file,
// computed in a worker goroutine; all statedb writes happen back on the
// single calling goroutine (SQLite only wants one writer at a time).
type hashJob struct {
	path       string
	digest     string
	capturedAt *float64
	size       int64
	mtime      float64
	err        error
}

// ScanFolders walks patterns, hashes every file no previous scan has
// already recorded, and registers what it finds in the ledger. It returns
// a summary of what was scanned, skipped and newly registered.
//
// markSynced, when true, registers newly-hashed files directly as
// 'uploaded' (no network call, no media item ID) instead of 'pending' --
// for content the user confirms is already synced outside of gpsync. It's
// a one-time snapshot of what's in the folder(s) right now; files added
// later are unaffected and scan as pending normally.
// onPreScan, if non-nil, is called once per ScanFolders call right after the
// folder listing is known -- before any hashing starts -- with
// (totalFiles, alreadyKnownFromLastScan, toHashNow), so the caller can show
// "already known" info up front instead of only in a final summary.
//
// trackRun controls whether this call writes to the singleton run_progress
// row (db.RunStart/RunUpdate/RunFinish) -- false for a caller running this
// scan CONCURRENTLY with an upload phase that owns that row itself for the
// same cycle (internal/engine.RunFolderCycle's concurrent path). That row
// has exactly one slot (id=1, overwritten in place by every writer), so two
// concurrent writers would flicker/race on it; the fix isn't a lock, it's
// scanning simply never touching the row during a concurrent cycle, so
// upload deterministically owns it -- see RunFolderCycle's own doc comment.
// Every other caller (CLI commands, non-concurrent legacy paths, tests)
// passes true, preserving today's behavior exactly.
func ScanFolders(db *statedb.DB, patterns []string, runType string, concurrency int, markSynced bool, trackRun bool, onSkip func(path, reason string), onPreScan func(total, alreadyKnown, toHashNow int), onProgress func(ProgressEvent)) (ScanSummary, error) {
	var summary ScanSummary
	if concurrency < 1 {
		concurrency = 1
	}

	allPaths, err := ExpandFolders(patterns, func(path, reason string) {
		summary.Skipped++
		if onSkip != nil {
			onSkip(path, reason)
		}
	})
	if err != nil {
		return summary, err
	}
	summary.FilesSeen = len(allPaths)

	folderSet := map[string]bool{}
	for _, p := range allPaths {
		folderSet[filepath.Dir(p)] = true
	}
	summary.Folders = len(folderSet)

	if trackRun {
		if err := db.RunStart(runType, summary.Folders, len(allPaths)); err != nil {
			return summary, err
		}
	}

	// os.Stat runs in the SAME worker pool shape as the hashing pass below
	// (statJobs -> statResults, one consuming goroutine). Running scan and
	// upload concurrently was not enough on its own: uploads still waited
	// on a long discovery pass even when the pending files were already in
	// the ledger. That shortcut holds only for files a PREVIOUS scan
	// discovered -- a file
	// that's never been seen before (the common case for "a folder's
	// mostly synced, a few new photos just got dropped in") has no ledger
	// row at all until THIS scan finds and hashes it, so some discovery
	// pass is unavoidable. What was avoidable: this pass used to touch
	// EVERY file in the folder with two fully sequential round-trips each
	// (os.Stat, then a db.GetFileSeen lookup) before a single one could be
	// classified -- on a real multi-thousand-file library over a slower
	// filesystem, that's real, measured minutes, not the low-hundreds-of-
	// ms this benchmarked as on a fast local disk. db.GetFileSeen itself
	// stays serialized on ONE consuming goroutine regardless (statedb's
	// single-connection cap means concurrent DB calls would just queue
	// behind each other anyway, see statedb.Open's own doc comment) -- the
	// os.Stat calls are what actually parallelize, same reasoning as the
	// hashing pass's own "IO/CPU-bound work in a pool, DB writes
	// serialized on one goroutine" comment below. toHash/cacheHits'
	// ORDER is no longer allPaths' own order (results arrive as each
	// worker finishes, not in submission order) -- already true of the
	// hashing pass's own output for the exact same reason, and nothing
	// downstream depends on either being stable.
	type statResult struct {
		path string
		info os.FileInfo
	}
	statJobs := make(chan string, len(allPaths))
	statResults := make(chan statResult, len(allPaths))
	var statWG sync.WaitGroup
	for w := 0; w < concurrency; w++ {
		statWG.Add(1)
		go func() {
			defer statWG.Done()
			for p := range statJobs {
				if info, err := statFn(p); err == nil {
					statResults <- statResult{path: p, info: info}
				}
			}
		}()
	}
	for _, p := range allPaths {
		statJobs <- p
	}
	close(statJobs)
	go func() { statWG.Wait(); close(statResults) }()

	var toHash []string
	var cacheHits []*statedb.FileSeen
	statCache := map[string]os.FileInfo{}
	for res := range statResults {
		statCache[res.path] = res.info
		seen, err := db.GetFileSeen(res.path)
		if err != nil {
			return summary, err
		}
		mtime := float64(res.info.ModTime().UnixNano()) / 1e9
		if seen != nil && seen.Mtime == mtime && seen.Size == res.info.Size() {
			cacheHits = append(cacheHits, seen)
			continue
		}
		toHash = append(toHash, res.path)
	}
	summary.FilesHashed = len(toHash)

	if onPreScan != nil {
		onPreScan(len(allPaths), len(allPaths)-len(toHash), len(toHash))
	}

	// markSynced must still register cache-hit files (already hashed by a
	// prior plain scan, so no re-hash needed here) -- the "skip if unchanged"
	// fast path above is only a hashing optimization, it must not also skip
	// promoting an already-known hash to synced.
	if markSynced {
		for _, seen := range cacheHits {
			existing, err := db.GetUpload(seen.SHA256)
			if err != nil {
				return summary, err
			}
			if err := db.EnsureMarkedSynced(seen.SHA256, seen.Size, GuessMime(seen.Path), seen.Path, nil); err != nil {
				return summary, err
			}
			if wasNewlyRegistered(existing, markSynced) {
				summary.NewPending++
			}
		}
	}

	// Hashing + EXIF reads are CPU/IO-bound and independent per file, so
	// they run in a worker pool; only the resulting statedb writes are
	// serialized back on this goroutine.
	jobs := make(chan string, len(toHash))
	results := make(chan hashJob, len(toHash))
	inFlight := newInFlightTracker()
	var wg sync.WaitGroup
	for w := 0; w < concurrency; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for p := range jobs {
				inFlight.start(p)
				info := statCache[p]
				digest, err := hashing.SHA256File(p)
				if err != nil {
					inFlight.finish(p)
					results <- hashJob{path: p, err: err}
					continue
				}
				capturedAt := captureDate(p)
				inFlight.finish(p)
				results <- hashJob{
					path:       p,
					digest:     digest,
					capturedAt: capturedAt,
					size:       info.Size(),
					mtime:      float64(info.ModTime().UnixNano()) / 1e9,
				}
			}
		}()
	}
	for _, p := range toHash {
		jobs <- p
	}
	close(jobs)
	go func() { wg.Wait(); close(results) }()

	seenFolders := map[string]bool{}
	filesDone := 0
	for job := range results {
		filesDone++
		if job.err != nil {
			continue
		}
		if err := db.UpsertFileSeen(job.path, job.digest, job.mtime, job.size); err != nil {
			return summary, err
		}

		existing, err := db.GetUpload(job.digest)
		if err != nil {
			return summary, err
		}
		switch {
		case markSynced:
			err = db.EnsureMarkedSynced(job.digest, job.size, GuessMime(job.path), job.path, job.capturedAt)
		case IsInOriginalsFolder(job.path):
			// Never auto-queued -- see IsInOriginalsFolder's own doc
			// comment. Still hashed and tracked (UpsertFileSeen above
			// already ran) so the review UI has something to show and
			// FindByBasename can find it as a candidate for OTHER
			// originals-folder items, just routed to needs_review instead
			// of pending.
			err = db.EnsureNeedsReview(job.digest, job.size, GuessMime(job.path), job.path, job.capturedAt)
		default:
			err = db.EnsurePending(job.digest, job.size, GuessMime(job.path), job.path, job.capturedAt)
		}
		if err != nil {
			return summary, err
		}
		if wasNewlyRegistered(existing, markSynced) {
			summary.NewPending++
		}

		seenFolders[filepath.Dir(job.path)] = true
		if trackRun {
			foldersDone := len(seenFolders)
			_ = db.RunUpdate(&foldersDone, &filesDone)
		}
		if onProgress != nil {
			onProgress(ProgressEvent{Done: filesDone, Total: len(toHash), CurrentFile: job.path, InFlight: inFlight.snapshot()})
		}
	}

	if err := sweepMissing(db, patterns, allPaths, &summary); err != nil {
		return summary, err
	}

	if !trackRun {
		return summary, nil
	}
	return summary, db.RunFinish()
}

// sweepMissing flags ledger entries under the scanned folders whose file is
// no longer on disk, clears the flag on entries whose file is back, and drops
// scan-cache entries for vanished paths.
//
// Before this, scanning only ever looked at files that exist, so a deleted
// or moved file left its ledger entry pointing at a dead path indefinitely.
// It FLAGS rather than deletes because a single-folder scan cannot tell a
// deletion from a move into a folder it did not cover (see
// engine.ResolveMissingFiles).
//
// Only folders that exist right now are swept. A source folder that is
// temporarily unavailable -- an unplugged drive, a disconnected share --
// flags nothing, instead of flagging everything under it.
func sweepMissing(db *statedb.DB, patterns, allPaths []string, summary *ScanSummary) error {
	roots := existingDirRoots(patterns)
	if len(roots) == 0 {
		return nil
	}
	present := make(map[string]bool, len(allPaths))
	for _, p := range allPaths {
		present[pathx.Key(p)] = true
	}
	// A path the walk did not list may still exist: the extension and
	// media-type gates leave real files out of allPaths. Only a definite
	// "does not exist" counts; any other stat error (permissions, a flaky
	// disk) is treated as present rather than as a deletion.
	exists := func(p string) bool {
		if present[pathx.Key(p)] {
			return true
		}
		_, err := statFn(p)
		return err == nil || !errors.Is(err, fs.ErrNotExist)
	}
	now := float64(time.Now().UnixNano()) / 1e9

	rows, err := db.UploadsUnder(roots)
	if err != nil {
		return err
	}
	for _, r := range rows {
		if exists(r.Path) {
			if r.Flagged {
				if err := db.ClearMissing(r.SHA256); err != nil {
					return err
				}
				summary.Reappeared++
			}
			continue
		}
		if err := db.MarkMissing(r.SHA256, now); err != nil {
			return err
		}
		summary.Missing++
	}

	cached, err := db.FileSeenPathsUnder(roots)
	if err != nil {
		return err
	}
	for _, p := range cached {
		if exists(p) {
			continue
		}
		if err := db.DeleteFileSeen(p); err != nil {
			return err
		}
		summary.StaleCache++
	}
	return nil
}

// existingDirRoots resolves scan patterns (folders or globs) to the absolute
// directories that exist right now. Files named directly are skipped: only a
// walked folder gives a complete picture of what should be under it.
func existingDirRoots(patterns []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, pattern := range patterns {
		pattern = strings.TrimRight(pattern, `/\`)
		matches, err := filepath.Glob(pattern)
		if err != nil || len(matches) == 0 {
			matches = []string{pattern}
		}
		for _, m := range matches {
			info, err := statFn(m)
			if err != nil || !info.IsDir() {
				continue
			}
			abs, err := filepath.Abs(m)
			if err != nil {
				abs = m
			}
			if k := pathx.Key(abs); !seen[k] {
				seen[k] = true
				out = append(out, abs)
			}
		}
	}
	return out
}
