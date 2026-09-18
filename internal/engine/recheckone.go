package engine

import (
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/gmizrahi/gpsync/internal/statedb"
)

// ErrUnknownHash is returned when no ledger row has the given hash. The
// hash arrives from a client, so an unknown one is reported rather than
// silently treated as a no-op.
var ErrUnknownHash = errors.New("no tracked file has that hash")

// RecheckOutcome is what RecheckOne decided for one row.
type RecheckOutcome string

const (
	// RecheckBackOnDisk: the file is where the ledger says. Any missing
	// flag is cleared.
	RecheckBackOnDisk RecheckOutcome = "back"
	// RecheckStillGone: the file is not there. Recorded as confirmed
	// missing, which is what `gpsync recheck --missing` acts on.
	RecheckStillGone RecheckOutcome = "gone"
)

// RecheckOne re-examines a single tracked file, the per-row equivalent of
// `gpsync recheck --revalidate`.
//
// Deliberately narrower than ResolveMissingFiles: it stats the one path
// rather than walking the source folders, so it cannot relocate a file
// that moved. Someone acting on one row wants a quick verdict on that row;
// finding moves is what the full pass is for, and the CLI still does it.
//
// Never deletes. A confirmed-missing row stays in the ledger until someone
// explicitly forgets it, so a drive that was merely unplugged loses
// nothing.
func RecheckOne(db *statedb.DB, sha256 string, now time.Time) (RecheckOutcome, error) {
	u, err := db.GetUpload(sha256)
	if err != nil {
		return "", err
	}
	if u == nil {
		return "", ErrUnknownHash
	}

	if _, statErr := os.Stat(u.FirstSourcePath); statErr == nil {
		if err := db.ClearMissing(sha256); err != nil {
			return "", fmt.Errorf("clearing the missing flag: %w", err)
		}
		return RecheckBackOnDisk, nil
	} else if !os.IsNotExist(statErr) {
		// A permission error or an unreachable share says nothing about
		// whether the file exists. Reporting "gone" on one would be a
		// false verdict about whether a backup still has a source.
		return "", fmt.Errorf("could not check %s: %w", u.FirstSourcePath, statErr)
	}

	nowF := float64(now.Unix())
	if err := db.MarkMissing(sha256, nowF); err != nil {
		return "", fmt.Errorf("flagging as missing: %w", err)
	}
	if err := db.ConfirmMissing(sha256, nowF); err != nil {
		return "", fmt.Errorf("confirming missing: %w", err)
	}
	return RecheckStillGone, nil
}

// ForgetOne removes a single file's ledger row, the per-row equivalent of
// `gpsync recheck --missing` for one entry.
//
// The file on disk is never touched -- this only drops what gpsync
// remembers. A later scan that finds the file again will treat it as new.
func ForgetOne(db *statedb.DB, sha256 string) (path string, err error) {
	u, err := db.GetUpload(sha256)
	if err != nil {
		return "", err
	}
	if u == nil {
		return "", ErrUnknownHash
	}
	if err := db.DeleteUpload(sha256); err != nil {
		return "", err
	}
	// Drop the scan-cache entry too, or the next scan sees a cache hit for
	// a hash with no ledger row and never re-registers the file.
	if err := db.DeleteFileSeen(u.FirstSourcePath); err != nil {
		return "", err
	}
	return u.FirstSourcePath, nil
}
