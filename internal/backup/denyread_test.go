package backup

import (
	"os"
	"testing"
)

// denyRead makes path unopenable and returns a function restoring it.
//
// On POSIX, clearing the mode bits is enough. It is a separate helper (and a
// separate file) because the mechanism is genuinely platform-shaped: running
// as root ignores mode bits entirely, so the caller is skipped rather than
// given a false pass.
func denyRead(t *testing.T, path string) func() {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("running as root: mode bits do not deny reads, so this failure cannot be induced this way")
	}
	if err := os.Chmod(path, 0o000); err != nil {
		t.Fatal(err)
	}
	if f, err := os.Open(path); err == nil {
		f.Close()
		_ = os.Chmod(path, 0o600)
		t.Skip("this filesystem ignores mode bits, so the failure cannot be induced this way")
	}
	return func() { _ = os.Chmod(path, 0o600) }
}
