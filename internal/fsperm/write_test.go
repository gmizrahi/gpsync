package fsperm

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/gmizrahi/gpsync/internal/fsperm/fspermtest"
)

func TestWriteFileOwnerOnly_ReplacesAndLeavesNoStaging(t *testing.T) {
	p := filepath.Join(t.TempDir(), "key.pem")
	for _, body := range []string{"first", "second"} {
		if err := WriteFileOwnerOnly(p, []byte(body)); err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile(p)
		if err != nil || string(got) != body {
			t.Fatalf("read %q, %v; want %q", got, err, body)
		}
		fspermtest.AssertOwnerOnly(t, p)
	}
	if _, err := os.Stat(StagingPath(p)); !os.IsNotExist(err) {
		t.Fatalf("staging file left behind: %v", err)
	}
}

// The issue #60 case: a stale staging file with a loose mode must not lend
// that mode to the secret -- and its stale CONTENT must not survive either.
func TestWriteFileOwnerOnly_StaleLooseStagingFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "key.pem")
	loose(t, StagingPath(p), "stale")
	if err := WriteFileOwnerOnly(p, []byte("secret")); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(p); string(got) != "secret" {
		t.Fatalf("content = %q, want the new secret", got)
	}
	fspermtest.AssertOwnerOnly(t, p)
}

// A target that already exists, loose, ends up owner-only too -- on Windows
// that is the replace-an-existing-file rename #60 measured.
func TestWriteFileOwnerOnly_LooseExistingTarget(t *testing.T) {
	p := filepath.Join(t.TempDir(), "key.pem")
	loose(t, p, "old")
	if err := WriteFileOwnerOnly(p, []byte("new")); err != nil {
		t.Fatal(err)
	}
	fspermtest.AssertOwnerOnly(t, p)
}

// The staging file is owner-only BEFORE anything is written to it, which is
// what keeps the secret from ever sitting in a readable file.
func TestCreateStaging_IsOwnerOnlyWhileStillEmpty(t *testing.T) {
	p := filepath.Join(t.TempDir(), "token.json")
	loose(t, StagingPath(p), "stale")
	f, err := CreateStaging(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if st, err := f.Stat(); err != nil || st.Size() != 0 {
		t.Fatalf("staging file: size %v, %v -- want a fresh, empty file, not the stale one", st.Size(), err)
	}
	fspermtest.AssertOwnerOnly(t, StagingPath(p))
}

// A commit that cannot rename removes its staging file, so nothing
// half-done is left to be mistaken for a real one.
func TestCommitStaging_FailedRenameLeavesNoStaging(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "token.json")
	f, err := CreateStaging(p)
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	// A directory where the target should be makes the rename fail.
	if err := os.Mkdir(p, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(p, "occupied"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := CommitStaging(p); err == nil {
		t.Fatal("CommitStaging over a non-empty directory reported success")
	}
	if _, err := os.Stat(StagingPath(p)); !os.IsNotExist(err) {
		t.Errorf("staging file left behind after a failed commit: %v", err)
	}
}

// loose writes path readable by everyone -- 0644 past any umask on POSIX,
// and on Windows a plain file, which only inherits its directory's ACL.
func loose(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
}
