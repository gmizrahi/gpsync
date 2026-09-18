// Package fsperm restricts a file to the account that owns it.
//
// gpsync writes several files that must not be readable by other users on
// the machine: the OAuth credentials, and the backup archive that contains
// copies of them. On Linux and macOS the 0600 passed to os.WriteFile is the
// whole answer. Windows does not implement those mode bits at all -- the
// file reports 666 and inherits the parent directory's ACL -- so it needs an
// explicit ACL instead, which is what this package provides.
package fsperm

import "errors"

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
