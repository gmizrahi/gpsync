package engine

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/gmizrahi/gpsync/internal/statedb"
)

// RenameFn is os.Rename, indirected so a test can force the cross-volume
// failure that only happens on a real multi-drive machine.
var RenameFn = os.Rename

// TrashPath drops a file directly into the trash dir by its basename --
// deliberately flat, not mirroring the original folder tree (a real
// report: an earlier version did, and the recreated tree was found
// annoying -- everything in one directory was asked for instead).
// Basename collisions are handled by UniqueTrashPath, not by this
// function -- two different files sharing a name still land safely side
// by side, just as "photo.jpg"/"photo_1.jpg" rather than in separate
// mirrored folders.
func TrashPath(trashDir, originalPath string) string {
	return filepath.Join(trashDir, filepath.Base(originalPath))
}

// UniqueTrashPath avoids ever overwriting something already in the trash --
// a re-run after an interrupted pass, or two genuinely different files
// that happen to share a basename. Returns the path to use.
func UniqueTrashPath(dest string) string {
	if _, err := os.Lstat(dest); err != nil {
		return dest
	}
	ext := filepath.Ext(dest)
	base := strings.TrimSuffix(dest, ext)
	for i := 1; ; i++ {
		candidate := fmt.Sprintf("%s_%d%s", base, i, ext)
		if _, err := os.Lstat(candidate); err != nil {
			return candidate
		}
	}
}

// MoveToTrash relocates one file directly into the trash dir (flat, by
// basename -- see TrashPath). Returns the destination actually used. This
// is the ONE shared implementation of "move a duplicate/already-backed-up
// copy out of the way" -- both `gpsync duplicates resolve`/`gpsync precheck`
// (cmd/gpsync) and gpsync-tray's Duplicate Resolver tab call this, never a
// private copy each, since it must never fall back to deleting.
//
// RenameFn (os.Rename) is tried first, since it is atomic and instant; it
// cannot cross volumes, which is the common case here (trashing from D:
// into a trash folder on another drive), so a copy-then-remove fallback
// covers that. The original is only removed once the copy is safely
// closed, so an interrupted fallback leaves the source intact rather than
// losing the file.
func MoveToTrash(trashDir, src string) (string, error) {
	dest := UniqueTrashPath(TrashPath(trashDir, src))
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return "", fmt.Errorf("creating trash folder: %w", err)
	}
	if err := RenameFn(src, dest); err == nil {
		return dest, nil
	}
	// Cross-volume (or any other rename refusal): copy, then drop the original.
	if err := copyFileContents(src, dest); err != nil {
		return "", err
	}
	if err := os.Remove(src); err != nil {
		return "", fmt.Errorf("copied to trash but could not remove the original: %w", err)
	}
	return dest, nil
}

func copyFileContents(src, dest string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dest, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(dest)
		return err
	}
	if err := out.Close(); err != nil {
		os.Remove(dest)
		return err
	}
	return nil
}

// ExistingPaths filters a duplicate group's path list down to the copies
// still actually on disk.
//
// The ledger's file list can be stale -- a copy may have been deleted by
// hand, or by an earlier resolve pass -- and acting on a path that no
// longer exists would at best miscount and at worst make the "how many
// copies are left" reasoning wrong.
func ExistingPaths(paths []string) []string {
	var out []string
	for _, p := range paths {
		if info, err := os.Stat(p); err == nil && !info.IsDir() {
			out = append(out, p)
		}
	}
	return out
}

// FindDuplicateGroupAndValidateKeep looks up the duplicate group matching
// sha256 among groups (a fresh, current list -- e.g. from
// dupGroupsFiltered) and confirms keep genuinely names one of its members.
//
// A caller (gpsync-tray's dashboard, which can be LAN-exposed) MUST use this
// -- not a client-submitted paths list -- to decide what ResolveDuplicateGroup
// is actually allowed to move. Trusting the client's own paths/size fields
// directly, as a code review found, turned a plain POST into an
// arbitrary file-move primitive: any sha256/keep/paths
// combination was accepted verbatim, and every path other than keep got
// moved to the trash dir, whether or not it was a real duplicate at all.
// Only sha256 (which group) and keep (which member to keep) should ever
// come from a request; group.Paths/group.Size here are the only values
// that should ever reach ResolveDuplicateGroup.
func FindDuplicateGroupAndValidateKeep(groups []statedb.DuplicateGroup, sha256, keep string) (group statedb.DuplicateGroup, ok bool) {
	if sha256 == "" || keep == "" {
		return statedb.DuplicateGroup{}, false
	}
	for _, g := range groups {
		if g.SHA256 != sha256 {
			continue
		}
		for _, p := range g.Paths {
			if p == keep {
				return g, true
			}
		}
		return statedb.DuplicateGroup{}, false
	}
	return statedb.DuplicateGroup{}, false
}

// ResolveDuplicateGroup moves every copy in the group except keep into the
// trash dir -- the one-shot (no dry-run, no interactive prompt) version of
// `cmd/gpsync`'s own `applyDuplicateChoice`, for gpsync-tray's Duplicate
// Resolver tab, which always acts immediately rather than offering a
// preview pass. Shares the exact same destructive primitive (MoveToTrash)
// and the same ledger-repoint-before-move ordering, just without the
// CLI's io.Writer/dry-run/auto-apply concerns.
//
// keep is stat'd again immediately before anything is moved. That guard
// is the difference between "trash the redundant copies" and "move away
// the only copies": if the chosen file has gone missing since the group
// was listed, relocating its siblings would leave nothing in place --
// nothing is moved in that case, and err is nil (this is a normal,
// expected outcome the caller should show as "nothing to do", not a
// failure).
func ResolveDuplicateGroup(db *statedb.DB, sha256, keep string, paths []string, size int64, trashDir string) (moved int, freed int64, err error) {
	if info, statErr := os.Stat(keep); statErr != nil || info.IsDir() {
		return 0, 0, nil
	}

	// The ledger's first_source_path may currently name one of the
	// victims about to move -- repoint it at the survivor BEFORE moving
	// anything, so a still-pending/failed_retryable row is never left
	// describing a path this call is about to move out from under it.
	// keep was just confirmed to exist above; this is a no-op write for a
	// hash that's already 'uploaded' (nothing re-reads the path then).
	if err := db.UpdateFirstSourcePath(sha256, keep); err != nil {
		return 0, 0, fmt.Errorf("repointing the ledger to the kept copy of %s: %w", keep, err)
	}

	for _, p := range paths {
		if p == keep {
			continue
		}
		if _, moveErr := MoveToTrash(trashDir, p); moveErr != nil {
			return moved, freed, fmt.Errorf("trashing %s: %w", p, moveErr)
		}
		moved++
		freed += size
		// The scan-cache row now describes a path with no file at it.
		if err := db.DeleteFileSeen(p); err != nil {
			return moved, freed, fmt.Errorf("clearing the scan-cache entry for %s: %w", p, err)
		}
	}
	return moved, freed, nil
}
