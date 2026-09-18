// Package backup snapshots and restores every file gpsync keeps in its state
// directory (~/.gpsync) as a single timestamped zip archive: the SQLite ledger
// (state.sqlite), settings (config.toml), and OAuth credentials
// (client_secret.json, token.json) alike.
//
// That last pair are live bearer credentials for the user's Google Photos
// library, and a backup destination is often a synced cloud folder -- gpsync
// reports exactly which files a backup/restore included so this is never a
// silent surprise, but the choice of what to include is the user's: this
// package doesn't second-guess it. Treat the destination folder with the
// same care as the credentials themselves.
package backup

import (
	"archive/zip"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/gmizrahi/gpsync/internal/fsperm"
	"github.com/gmizrahi/gpsync/internal/statedb"
	"github.com/gmizrahi/gpsync/internal/units"
)

const filePrefix = "gpsync-backup-"

// stateFileName is the name state.sqlite is always stored under, both in
// the archive and in StateDir -- backed up via VACUUM INTO (a consistent
// snapshot of the live, possibly-in-use database) rather than a raw copy.
const stateFileName = "state.sqlite"

// Create snapshots statedb.StateDir's entire contents -- the ledger (taken
// consistently via DB.BackupTo, not a raw file copy) plus every other file
// directly inside it, whatever that turns out to be, not a hardcoded list
// -- into a single timestamped zip in destDir, then prunes older backups
// beyond keepCount (oldest first; keepCount <= 0 means keep everything).
func Create(db *statedb.DB, destDir string, keepCount int) (Result, error) {
	if destDir == "" {
		return Result{}, fmt.Errorf("no backup destination configured -- set one with `gpsync backup --dest <folder>` (saved for next time)")
	}
	if err := os.MkdirAll(destDir, 0o700); err != nil {
		return Result{}, fmt.Errorf("creating backup destination: %w", err)
	}

	tmpDBCopy := filepath.Join(os.TempDir(), fmt.Sprintf("gpsync-backup-tmp-%d.sqlite", time.Now().UnixNano()))
	defer os.Remove(tmpDBCopy)
	if err := db.BackupTo(tmpDBCopy); err != nil {
		return Result{}, fmt.Errorf("snapshotting database: %w", err)
	}

	// Write to a ".partial" name and only rename it into place once the
	// archive is complete. A writeZip that failed partway (disk full while
	// snapshotting a big library) used to leave a truncated file sitting at
	// the final destPath -- which still matched List/prune's
	// "gpsync-backup-*.zip" glob, and being the NEWEST, made prune delete the
	// OLDEST genuine backups to make room for it. Repeat that against a
	// flaky or full drive and every real backup gets pruned away in favour
	// of corrupt ones. A ".partial" leftover matches neither the glob nor
	// prune, and is removed here anyway.
	destPath := filepath.Join(destDir, backupFileName(time.Now()))
	partialPath := destPath + ".partial"
	included, unrestricted, err := writeZip(partialPath, tmpDBCopy)
	if err != nil {
		os.Remove(partialPath)
		return Result{}, err
	}
	if err := os.Rename(partialPath, destPath); err != nil {
		os.Remove(partialPath)
		return Result{}, fmt.Errorf("finalizing backup archive: %w", err)
	}
	result := Result{Path: destPath, Files: included, Unrestricted: unrestricted}

	if keepCount > 0 {
		if err := prune(destDir, keepCount); err != nil {
			// The new backup itself succeeded -- a pruning failure shouldn't
			// look like the backup failed, just surface it to the caller.
			return result, fmt.Errorf("backup succeeded, but pruning old backups failed: %w", err)
		}
	}
	return result, nil
}

// Result reports exactly what a backup contains, so callers can print it
// rather than leaving the user to guess (or find out the hard way that
// something was missing).
type Result struct {
	Path  string
	Files []string // names actually included, e.g. "state.sqlite", "config.toml", "token.json"
	// Unrestricted reports that the destination's filesystem has no
	// per-user permissions, so the archive could not be locked down to
	// this account. It is not an error -- see writeZip -- but the archive
	// holds OAuth credentials, so every caller says so out loud.
	Unrestricted bool
}

// backupFileName always includes a fixed-width nanosecond field, not just
// the human-readable seconds-resolution timestamp -- otherwise two `gpsync
// backup` runs within the same second (a quick scripted retry, back-to-back
// calls) would collide on the same filename and silently overwrite each
// other's archive. The fixed-width numeric suffix also keeps List's plain
// lexicographic sort exactly equal to chronological order.
func backupFileName(t time.Time) string {
	return fmt.Sprintf("%s%s-%09d.zip", filePrefix, t.Format("20060102-150405"), t.Nanosecond())
}

// writeZip archives the consistent state.sqlite snapshot plus every OTHER
// file directly inside statedb.StateDir -- config.toml, client_secret.json,
// token.json, and anything else gpsync keeps there now or in the future.
// restrictToOwner is a seam: the real thing is a no-op on POSIX, so without
// it the unsupported-filesystem path below could only ever be exercised on a
// Windows machine with a FAT stick or a cloud drive mounted -- which is to
// say, never in CI, on the one behaviour a user has already been bitten by.
var restrictToOwner = fsperm.RestrictToOwner

// The bool result reports that the destination filesystem has no per-user
// permissions to set. That is NOT treated as a failure: the destination is a
// folder the user chose, and it is very often a cloud-sync folder (Google
// Drive's virtual drive answers ERROR_INVALID_PARAMETER for any ACL) or a
// removable stick, where refusing to write would mean no backups at all for
// the people most likely to want one off-machine. The caller reports it
// instead, because the archive contains OAuth credentials.
func writeZip(destPath, dbSnapshotPath string) (included []string, unrestricted bool, err error) {
	// filePerm, not os.Create's 0666&umask: this archive contains
	// client_secret.json and token.json (see this function's doc comment
	// above), and it is usually written somewhere synced.
	zf, err := os.OpenFile(destPath, os.O_RDWR|os.O_CREATE|os.O_TRUNC, filePerm)
	if err != nil {
		return nil, false, err
	}
	defer zf.Close()
	// Windows ignores the mode above; keep the same guarantee there with an
	// ACL. No-op on POSIX. Done here, on the empty staging file, so there is
	// never a moment where a COMPLETE archive sits readable -- and the DACL
	// survives the rename that follows (verified on NTFS: restricting the
	// .partial and renaming leaves the final archive owner-only).
	if err := restrictToOwner(destPath); err != nil {
		if !errors.Is(err, fsperm.ErrUnsupported) {
			return nil, false, err
		}
		unrestricted = true
	}
	zw := zip.NewWriter(zf)

	if err := addFileToZip(zw, dbSnapshotPath, stateFileName); err != nil {
		zw.Close()
		return nil, unrestricted, err
	}
	included = []string{stateFileName}

	entries, err := os.ReadDir(statedb.StateDir)
	if err != nil {
		zw.Close()
		return nil, unrestricted, err
	}
	for _, e := range entries {
		if e.IsDir() || e.Name() == stateFileName {
			continue
		}
		if err := addFileToZip(zw, filepath.Join(statedb.StateDir, e.Name()), e.Name()); err != nil {
			zw.Close()
			return nil, unrestricted, err
		}
		included = append(included, e.Name())
	}

	if err := zw.Close(); err != nil {
		return nil, unrestricted, err
	}
	return included, unrestricted, nil
}

func addFileToZip(zw *zip.Writer, srcPath, nameInZip string) error {
	src, err := os.Open(srcPath)
	if err != nil {
		return err
	}
	defer src.Close()
	w, err := zw.Create(nameInZip)
	if err != nil {
		return err
	}
	_, err = io.Copy(w, src)
	return err
}

// List returns backup archive paths in destDir, oldest first (the
// fixed-width timestamp in the filename sorts chronologically as a plain
// string, so no parsing is needed).
func List(destDir string) ([]string, error) {
	entries, err := os.ReadDir(destDir)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasPrefix(e.Name(), filePrefix) && strings.HasSuffix(e.Name(), ".zip") {
			out = append(out, filepath.Join(destDir, e.Name()))
		}
	}
	sort.Strings(out)
	return out, nil
}

func prune(destDir string, keepCount int) error {
	all, err := List(destDir)
	if err != nil {
		return err
	}
	if len(all) <= keepCount {
		return nil
	}
	for _, old := range all[:len(all)-keepCount] {
		if err := os.Remove(old); err != nil {
			return err
		}
	}
	return nil
}

// RestoreResult reports what a restore actually wrote, and where the
// pre-restore safety copy landed.
type RestoreResult struct {
	PreRestoreDir string
	Files         []string
}

// Restore overwrites every file directly inside statedb.StateDir with
// whatever the given backup archive contains -- the ledger, settings, and
// (if the backup has them) OAuth credentials alike. Before doing so, it
// makes a quick, best-effort raw copy of everything currently in StateDir
// into a "pre-restore-<timestamp>" folder next to the archive, purely as
// an undo safety net -- NOT a substitute for a real `gpsync backup`, just
// insurance against a mistaken restore.
//
// The caller must not hold an open *statedb.DB at the time of this call --
// on Windows especially, an open handle to state.sqlite would block
// overwriting it -- and is responsible for confirming this destructive
// action with the user first.
//
// Restore is all-or-nothing with respect to the live StateDir: the archive
// is validated first, then every entry is extracted to a ".tmp" sibling,
// and only once ALL of them have been written are they renamed into place.
// The old order (extract straight into StateDir with O_TRUNC, check for
// state.sqlite afterwards) could overwrite config.toml and the OAuth
// credentials and THEN report "nothing was restored" -- both a half-restore
// and a false error message. Any failure now leaves the live state exactly
// as it was; the leftover ".tmp" files are inert.
func Restore(archivePath string) (RestoreResult, error) {
	r, zerr := zip.OpenReader(archivePath)
	if zerr != nil {
		return RestoreResult{}, fmt.Errorf("opening backup archive: %w", zerr)
	}
	defer r.Close()

	// Validate up front, before writing a single byte anywhere.
	var entries []*zip.File
	sawDB := false
	for _, f := range r.File {
		// StateDir is flat -- ignore anything unexpectedly nested rather
		// than following it outside StateDir.
		if strings.ContainsAny(f.Name, `/\`) {
			continue
		}
		entries = append(entries, f)
		if f.Name == stateFileName {
			sawDB = true
		}
	}
	if !sawDB {
		return RestoreResult{}, fmt.Errorf("backup archive does not contain state.sqlite -- nothing was restored")
	}

	preRestoreDir := filepath.Join(filepath.Dir(archivePath), "pre-restore-"+time.Now().Format("20060102-150405"))
	if err := snapshotRaw(preRestoreDir); err != nil {
		return RestoreResult{}, fmt.Errorf("saving a safety copy of the current state before restoring (nothing was changed): %w", err)
	}

	// Stage every entry beside its final location, then commit.
	staged := make(map[string]string, len(entries)) // tmp path -> final path
	cleanupStaged := func() {
		for tmp := range staged {
			os.Remove(tmp)
		}
	}
	var restored []string
	for _, f := range entries {
		destPath, err := safeStatePath(f.Name)
		if err != nil {
			cleanupStaged()
			return RestoreResult{PreRestoreDir: preRestoreDir}, fmt.Errorf("refusing to restore %q -- nothing was changed: %w", f.Name, err)
		}
		tmpPath := destPath + ".tmp"
		if err := extractFile(f, tmpPath); err != nil {
			cleanupStaged()
			os.Remove(tmpPath)
			return RestoreResult{PreRestoreDir: preRestoreDir}, fmt.Errorf("restoring %s -- nothing was changed, and a pre-restore safety copy is at %s: %w", f.Name, preRestoreDir, err)
		}
		staged[tmpPath] = destPath
		restored = append(restored, f.Name)
	}

	for tmpPath, destPath := range staged {
		if err := os.Rename(tmpPath, destPath); err != nil {
			return RestoreResult{PreRestoreDir: preRestoreDir}, fmt.Errorf("putting restored %s into place (the restore is now PARTIAL -- the pre-restore safety copy at %s has the previous state): %w", filepath.Base(destPath), preRestoreDir, err)
		}
	}
	return RestoreResult{PreRestoreDir: preRestoreDir, Files: restored}, nil
}

func snapshotRaw(destDir string) error {
	entries, err := os.ReadDir(statedb.StateDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // nothing there yet -- nothing to snapshot
		}
		return err
	}
	if len(entries) == 0 {
		return nil
	}
	// 0700, not 0755: this directory holds a raw copy of client_secret.json
	// and token.json, and it's created next to the backup archive -- which
	// is typically inside a synced cloud folder. Group/other have no
	// business reading it.
	if err := os.MkdirAll(destDir, 0o700); err != nil {
		return err
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if err := copyFile(filepath.Join(statedb.StateDir, e.Name()), filepath.Join(destDir, e.Name())); err != nil {
			return err
		}
	}
	return nil
}

// filePerm is what both copyFile and extractFile write with. `gpsync setup`
// and `gpsync import-rclone` deliberately create client_secret.json and
// token.json at 0600 (a documented invariant); os.Create's 0666&umask and
// extractFile's old 0o644 silently downgraded that on every restore and
// every pre-restore safety copy. Applied to every file rather than
// special-cased by name -- state.sqlite and config.toml at 0600 costs
// nothing and leaves no gap to get the name list wrong.
const filePerm = 0o600

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, filePerm)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}

// safeStatePath resolves a zip entry name to a path inside the state
// directory, rejecting anything that would escape it.
//
// Archive entry names are attacker-controlled: a zip crafted elsewhere can
// carry names like "../../.ssh/authorized_keys" or an absolute path, and a
// naive filepath.Join would happily write outside ~/.gpsync (the "zip slip"
// class of bug). gpsync only ever archives flat files from the state
// directory, so anything with a path separator, a parent reference or a
// volume name is refused outright rather than sanitised.
func safeStatePath(name string) (string, error) {
	if name == "" || name == "." {
		return "", errors.New("empty entry name")
	}
	if filepath.IsAbs(name) || filepath.VolumeName(name) != "" {
		return "", errors.New("absolute paths are not allowed in a backup archive")
	}
	if strings.ContainsAny(name, `/\`) || name != filepath.Clean(name) || strings.HasPrefix(name, "..") {
		return "", errors.New("entry names must be plain file names")
	}
	dest := filepath.Join(statedb.StateDir, name)
	root := filepath.Clean(statedb.StateDir) + string(filepath.Separator)
	if !strings.HasPrefix(filepath.Clean(dest)+string(filepath.Separator), root) {
		return "", errors.New("entry would be written outside the state directory")
	}
	return dest, nil
}

// maxRestoredFileSize bounds how much a single archive entry may expand to
// during a restore. The ledger of a very large library is the biggest thing
// gpsync ever archives, and that is measured in hundreds of megabytes.
const maxRestoredFileSize = 8 << 30 // 8 GB

func extractFile(f *zip.File, destPath string) error {
	rc, err := f.Open()
	if err != nil {
		return err
	}
	defer rc.Close()
	if err := os.MkdirAll(filepath.Dir(destPath), 0o700); err != nil {
		return err
	}
	out, err := os.OpenFile(destPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, filePerm)
	if err != nil {
		return err
	}
	defer out.Close()
	// Cap the copy: a zip header's declared size is attacker-controlled, so
	// an archive can claim to hold a few kilobytes and decompress into
	// gigabytes ("zip bomb"), filling the disk during what looks like a
	// routine restore. gpsync only ever archives its own state directory,
	// where the ledger is the one large file, so maxRestoredFileSize is far
	// above any legitimate entry and still bounds the damage.
	limited := io.LimitReader(rc, maxRestoredFileSize+1)
	written, err := io.Copy(out, limited)
	if err != nil {
		return err
	}
	if written > maxRestoredFileSize {
		return fmt.Errorf("entry %q expands past the %s restore limit; refusing to continue",
			f.Name, units.Bytes(maxRestoredFileSize))
	}
	// An existing file keeps its old mode through O_CREATE, so set it
	// explicitly -- a restore must not leave a credential file at whatever
	// looser mode happened to be there before.
	return os.Chmod(destPath, filePerm)
}
