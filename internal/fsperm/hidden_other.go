//go:build !windows

package fsperm

// MarkHidden is a no-op except on Windows: elsewhere a leading dot in the
// name is what hides a file.
func MarkHidden(string) error { return nil }
