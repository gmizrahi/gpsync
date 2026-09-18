package backup

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/gmizrahi/gpsync/internal/fsperm"
)

// stubRestrict replaces the ACL call for one test. The real one is a no-op
// on POSIX, so this is the only way to reach the unsupported-filesystem path
// anywhere but a Windows box with a cloud drive mounted.
func stubRestrict(t *testing.T, err error) {
	t.Helper()
	prev := restrictToOwner
	restrictToOwner = func(string) error { return err }
	t.Cleanup(func() { restrictToOwner = prev })
}

// A backup destination on a filesystem with no per-user permissions -- a
// cloud-sync drive, a FAT stick -- must still produce a backup. Refusing
// would mean no backups at all for the people most likely to want one
// off-machine, and G:\My Drive fails exactly this way on a real install.
func TestCreate_FilesystemWithoutPermissions_StillBacksUp(t *testing.T) {
	db := openTestDB(t)
	dest := t.TempDir()
	stubRestrict(t, fmt.Errorf("restricting it: %w", fsperm.ErrUnsupported))

	result, err := Create(db, dest, 0)
	if err != nil {
		t.Fatalf("Create: %v -- a destination without ACLs must not fail the backup", err)
	}
	if !result.Unrestricted {
		t.Error("Unrestricted = false; the caller has to be able to warn that the archive holds credentials it could not lock down")
	}
	if _, err := os.Stat(result.Path); err != nil {
		t.Fatalf("no archive at %s: %v", result.Path, err)
	}
	if len(result.Files) == 0 {
		t.Error("the archive reported no contents")
	}
	// The whole point of the .partial dance: nothing half-written left over.
	entries, err := os.ReadDir(dest)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".partial") {
			t.Errorf("left a staging file behind: %s", e.Name())
		}
	}
}

// The tolerance above is only for filesystems that HAVE no permissions. A
// filesystem that has them and refused -- access denied on an NTFS path --
// is a real failure, and shipping an unreadable-by-design archive that is
// actually world-readable would be worse than not shipping one.
func TestCreate_RealPermissionFailure_StillFails(t *testing.T) {
	db := openTestDB(t)
	dest := t.TempDir()
	stubRestrict(t, errors.New("Access is denied."))

	if _, err := Create(db, dest, 0); err == nil {
		t.Fatal("Create succeeded; a genuine permission failure must not be swallowed")
	}
	entries, err := os.ReadDir(dest)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		t.Errorf("a failed backup left %s behind", e.Name())
	}
}

// The ordinary case must not start reporting a warning it has no reason to.
func TestCreate_NormalFilesystem_IsNotFlagged(t *testing.T) {
	db := openTestDB(t)
	result, err := Create(db, t.TempDir(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if result.Unrestricted {
		t.Error("Unrestricted = true on a filesystem that restricted the archive fine")
	}
}
