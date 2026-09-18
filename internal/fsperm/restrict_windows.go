//go:build windows

package fsperm

import (
	"errors"
	"fmt"

	"golang.org/x/sys/windows"
)

// Error codes SetNamedSecurityInfo returns when the target's filesystem has
// no ACLs at all, rather than when it has them and refused. Google Drive's
// virtual drive answers ERROR_INVALID_PARAMETER for a perfectly valid
// descriptor (verified against G:\ on a real install); FAT/exFAT volumes and
// some network redirectors answer with the other two.
//
// ERROR_ACCESS_DENIED is deliberately NOT here: that is a filesystem which
// does support permissions and would not let us set them, which is a real
// failure worth reporting.
var unsupportedByFilesystem = map[windows.Errno]bool{
	windows.ERROR_INVALID_FUNCTION:  true,
	windows.ERROR_NOT_SUPPORTED:     true,
	windows.ERROR_INVALID_PARAMETER: true,
}

// RestrictToOwner replaces path's DACL with a single entry granting full
// access to the current user and nobody else, and detaches it from
// inheritance.
//
// Windows ignores the 0600 that os.WriteFile asks for -- the file lands 666
// as far as os.Stat is concerned, and inherits whatever the parent directory
// grants. SECURITY.md promises owner-only credentials, so on the platform
// gpsync primarily targets that promise has to be kept by an ACL rather than
// by a mode bit.
//
// PROTECTED_DACL_SECURITY_INFORMATION is the part that matters: without it a
// permissive inherited ACE stays in force alongside the explicit one.
//
// On a filesystem with no ACLs at all -- Google Drive's virtual drive, a FAT
// stick, some network redirectors -- the error wraps ErrUnsupported, so a
// caller writing to a location the USER picked can carry on and say so
// instead of refusing to work there.
func RestrictToOwner(path string) error {
	token := windows.GetCurrentProcessToken()
	user, err := token.GetTokenUser()
	if err != nil {
		return fmt.Errorf("looking up the current user: %w", err)
	}

	acl, err := windows.ACLFromEntries([]windows.EXPLICIT_ACCESS{{
		AccessPermissions: windows.GENERIC_ALL,
		AccessMode:        windows.GRANT_ACCESS,
		Inheritance:       windows.NO_INHERITANCE,
		Trustee: windows.TRUSTEE{
			TrusteeForm:  windows.TRUSTEE_IS_SID,
			TrusteeType:  windows.TRUSTEE_IS_USER,
			TrusteeValue: windows.TrusteeValueFromSID(user.User.Sid),
		},
	}}, nil)
	if err != nil {
		return fmt.Errorf("building the credential ACL: %w", err)
	}

	if err := windows.SetNamedSecurityInfo(
		path,
		windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil, nil, acl, nil,
	); err != nil {
		// errors.As, not a type assertion: errorlint rejects the latter,
		// and it would miss a wrapped errno if this call ever grows one.
		var errno windows.Errno
		if errors.As(err, &errno) && unsupportedByFilesystem[errno] {
			return fmt.Errorf("restricting %s to the current user: %w (%v)", path, ErrUnsupported, err)
		}
		return fmt.Errorf("restricting %s to the current user: %w", path, err)
	}
	return nil
}
