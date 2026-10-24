// Package fspermtest asserts, from a test, that a file is readable by the
// current account only -- by whatever means that is enforced on this OS.
//
// It exists so a test of a component that writes secrets (token.json, the
// dashboard key, a restore) can check the outcome once, portably, instead of
// each package re-deriving what "owner-only" means on POSIX and on Windows.
package fspermtest

import "testing"

// AssertOwnerOnly fails t unless path is readable by the current account
// alone: mode 0600-or-stricter on POSIX, and on Windows a protected DACL
// holding exactly one ACE, for the current user.
func AssertOwnerOnly(t testing.TB, path string) {
	t.Helper()
	if problem := ownerOnlyProblem(path); problem != "" {
		t.Errorf("%s is not owner-only: %s", path, problem)
	}
}
