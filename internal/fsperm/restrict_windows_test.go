//go:build windows

package fsperm

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

// The classification, not the ACL call: CI has no cloud drive or FAT stick
// mounted, so the codes those return have to be checked directly.
func TestUnsupportedByFilesystem_CoversTheCodesThatMeanNoACLs(t *testing.T) {
	// ERROR_INVALID_PARAMETER is the one that matters: Google Drive's
	// virtual drive answers it for a perfectly valid security descriptor,
	// which is how a backup to G:\My Drive failed outright.
	for _, errno := range []windows.Errno{
		windows.ERROR_INVALID_PARAMETER,
		windows.ERROR_NOT_SUPPORTED,
		windows.ERROR_INVALID_FUNCTION,
	} {
		if !unsupportedByFilesystem[errno] {
			t.Errorf("errno %d is not treated as an ACL-less filesystem", uintptr(errno))
		}
	}
	// A filesystem that HAS permissions and refused is a real failure.
	if unsupportedByFilesystem[windows.ERROR_ACCESS_DENIED] {
		t.Error("ERROR_ACCESS_DENIED must stay a real failure -- it means the ACL could have been set and was not")
	}
}

// The ordinary path still has to work: an NTFS temp dir restricts fine and
// reports no error at all.
func TestRestrictToOwner_OnNTFS_Succeeds(t *testing.T) {
	p := filepath.Join(t.TempDir(), "secret.json")
	if err := os.WriteFile(p, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := RestrictToOwner(p); err != nil {
		t.Fatalf("RestrictToOwner on a normal local path: %v", err)
	}
}

// A missing file is a genuine error, not an ACL-less filesystem -- otherwise
// a caller that tolerates ErrUnsupported would silently accept a path it
// never actually wrote.
func TestRestrictToOwner_MissingFile_IsNotReportedAsUnsupported(t *testing.T) {
	err := RestrictToOwner(filepath.Join(t.TempDir(), "does-not-exist"))
	if err == nil {
		t.Fatal("restricting a file that does not exist reported success")
	}
	if errors.Is(err, ErrUnsupported) {
		t.Errorf("err = %v, wrongly classified as an ACL-less filesystem", err)
	}
}
