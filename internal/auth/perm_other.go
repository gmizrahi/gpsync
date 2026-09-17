//go:build !windows

package auth

// restrictToOwner is a no-op everywhere except Windows.
//
// On POSIX the 0600 that os.WriteFile already asks for IS the restriction,
// and it is honoured. Windows ignores that mode bit, so it needs an explicit
// ACL instead -- see perm_windows.go.
func restrictToOwner(string) error { return nil }
