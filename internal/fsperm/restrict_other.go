//go:build !windows

package fsperm

// RestrictToOwner is a no-op everywhere except Windows.
//
// On POSIX the 0600 that os.WriteFile already asks for IS the restriction,
// and it is honoured. Windows ignores that mode bit, so it needs an explicit
// ACL instead -- see restrict_windows.go.
func RestrictToOwner(string) error { return nil }
