//go:build windows

package auth

import (
	"fmt"

	"golang.org/x/sys/windows"
)

// restrictToOwner replaces path's DACL with a single entry granting full
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
func restrictToOwner(path string) error {
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
		return fmt.Errorf("restricting %s to the current user: %w", path, err)
	}
	return nil
}
