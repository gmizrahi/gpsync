//go:build windows

package fsperm

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

func TestMarkHidden_SetsTheHiddenAttribute(t *testing.T) {
	dir := filepath.Join(t.TempDir(), ".thumbnails")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := MarkHidden(dir); err != nil {
		t.Fatal(err)
	}
	p, _ := windows.UTF16PtrFromString(dir)
	attrs, err := windows.GetFileAttributes(p)
	if err != nil || attrs&windows.FILE_ATTRIBUTE_HIDDEN == 0 || attrs&windows.FILE_ATTRIBUTE_DIRECTORY == 0 {
		t.Errorf("attributes %#x (%v); want a hidden directory", attrs, err)
	}
}
