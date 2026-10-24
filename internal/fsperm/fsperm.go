// Package fsperm restricts a file to the account that owns it.
//
// gpsync writes several files that must not be readable by other users on
// the machine: the OAuth credentials, and the backup archive that contains
// copies of them. On Linux and macOS the 0600 passed to os.WriteFile is the
// whole answer. Windows does not implement those mode bits at all -- the
// file reports 666 and inherits the parent directory's ACL -- so it needs an
// explicit ACL instead, which is what this package provides.
package fsperm

import (
	"errors"
	"fmt"
	"os"
)

// ErrUnsupported reports that the file's filesystem has no ACLs to set, so
// there is no per-user restriction to make there at all.
//
// This is not a failure to secure a file that could have been secured: it
// means the concept does not exist on that volume. Google Drive's virtual
// drive, a FAT/exFAT stick and some network redirectors all behave this way.
// A caller writing somewhere the USER chose -- a backup destination, say --
// should carry on and say so, rather than refusing to work there at all. A
// caller writing to gpsync's own state directory should still treat it as
// fatal, because that directory is expected to be a normal local one.
var ErrUnsupported = errors.New("this filesystem does not support per-user file permissions")

// Writing a secret: one way, used everywhere (issue #60).
//
// Five places used to write owner-only files, each its own way, and three of
// the ways leaked. os.WriteFile applies its mode only when it CREATES the
// file, so a staging file left behind with a looser mode -- by an older
// version, a crash, another tool -- handed that mode to token.json and the
// dashboard key through the rename, and an existing client_secret.json kept
// its own; on POSIX nothing corrected either, since RestrictToOwner is a
// no-op there. A restore never restricted on Windows at all: every restored
// file, credentials included, inherited its directory's ACL (measured on
// Windows 11 / NTFS).
//
// The order below is what makes it hold on every OS:
//
//  1. Remove any stale staging file and create it fresh, O_EXCL, mode 0600
//     -- so the mode is applied, never inherited from a leftover.
//  2. Restrict it to the owner while it is still EMPTY -- so the secret never
//     exists in a readable file, not even between the write and the rename.
//  3. Rename it into place, and restrict the target again. A rename carries
//     the staging file's protected DACL to the target on NTFS, including when
//     it REPLACES an existing file (measured, #60); restricting again costs
//     one call and does not depend on that measurement holding everywhere.
//
// A filesystem with no ACLs (ErrUnsupported) is an error here: every caller
// writes into gpsync's own state directory, which is expected to be an
// ordinary local one. A destination the USER chose -- a backup folder on a
// cloud drive -- is the backup package's business, which tolerates it.

// StagingPath is where CreateStaging stages path.
func StagingPath(path string) string { return path + ".tmp" }

// CreateStaging creates path's staging file, empty and already restricted to
// the current account, and returns it open for writing. The caller writes,
// closes, and then calls CommitStaging -- separately, so a caller that must
// stage several files before committing any (a restore) can.
func CreateStaging(path string) (*os.File, error) {
	tmp := StagingPath(path)
	if err := os.Remove(tmp); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("removing a stale %s: %w", tmp, err)
	}
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, err
	}
	if err := RestrictToOwner(tmp); err != nil {
		f.Close()
		os.Remove(tmp)
		return nil, err
	}
	return f, nil
}

// CommitStaging moves path's staging file into place and restricts the
// result. On failure the staging file is removed, so nothing half-done is
// left to be mistaken for a real one.
func CommitStaging(path string) error {
	tmp := StagingPath(path)
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return RestrictToOwner(path)
}

// WriteFileOwnerOnly atomically replaces path with data, readable only by
// the current account on every OS.
func WriteFileOwnerOnly(path string, data []byte) error {
	f, err := CreateStaging(path)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(StagingPath(path))
		return err
	}
	// Sync before the rename: a crash after it must not leave a renamed
	// file whose contents never reached the disk.
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(StagingPath(path))
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(StagingPath(path))
		return err
	}
	return CommitStaging(path)
}
