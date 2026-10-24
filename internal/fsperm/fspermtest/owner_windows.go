//go:build windows

package fspermtest

import (
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

// ownerOnlyProblem reads the DACL back rather than trusting that the call
// that set it succeeded: what matters is what is on the file now. Mode bits
// mean nothing on Windows -- every file reports 0666 -- so they are not
// consulted.
func ownerOnlyProblem(path string) string {
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return err.Error()
	}
	ctl, _, err := sd.Control()
	if err != nil {
		return err.Error()
	}
	// Without PROTECTED, ACEs inherited from the directory stay in force
	// alongside the explicit one, whatever the explicit one says.
	if ctl&windows.SE_DACL_PROTECTED == 0 {
		return "DACL is not protected, so the directory's permissions are inherited"
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return err.Error()
	}
	if dacl == nil {
		return "no DACL at all, which grants everyone full access"
	}
	if dacl.AceCount != 1 {
		return fmt.Sprintf("DACL holds %d ACEs, want exactly 1", dacl.AceCount)
	}
	var ace *windows.ACCESS_ALLOWED_ACE
	if err := windows.GetAce(dacl, 0, &ace); err != nil {
		return err.Error()
	}
	if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE {
		return fmt.Sprintf("the one ACE is type %d, not access-allowed", ace.Header.AceType)
	}
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return err.Error()
	}
	if sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart)); !sid.Equals(user.User.Sid) {
		return fmt.Sprintf("the one ACE grants %s, not the current user %s", sid, user.User.Sid)
	}
	return ""
}
