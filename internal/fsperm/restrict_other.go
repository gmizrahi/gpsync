//go:build !windows

// Package fsperm restricts a file to the account that owns it.
//
// gpsync writes several files that must not be readable by other users on
// the machine: the OAuth credentials, and the backup archive that contains
// copies of them. On Linux and macOS the 0600 passed to os.WriteFile is the
// whole answer. Windows does not implement those mode bits at all -- the
// file reports 666 and inherits the parent directory's ACL -- so it needs an
// explicit ACL instead, which is what this package provides.
package fsperm

// RestrictToOwner is a no-op everywhere except Windows.
//
// On POSIX the 0600 that os.WriteFile already asks for IS the restriction,
// and it is honoured. Windows ignores that mode bit, so it needs an explicit
// ACL instead -- see perm_windows.go.
func RestrictToOwner(string) error { return nil }
