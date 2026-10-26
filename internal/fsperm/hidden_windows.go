//go:build windows

package fsperm

import "golang.org/x/sys/windows"

// MarkHidden sets path's Hidden attribute, which is what a leading dot means
// everywhere but Windows.
func MarkHidden(path string) error {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	attrs, err := windows.GetFileAttributes(p)
	if err != nil {
		return err
	}
	return windows.SetFileAttributes(p, attrs|windows.FILE_ATTRIBUTE_HIDDEN)
}
