package main

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/fatih/color"

	"github.com/gmizrahi/gpsync/internal/engine"
)

// TestPrintPendingFolders_FullAndSummary covers both output shapes: the
// full listing names every file, and --summary collapses to per-folder
// lines only, which is what makes a multi-thousand-file backlog readable.
func TestPrintPendingFolders_FullAndSummary(t *testing.T) {
	orig := color.NoColor
	color.NoColor = true
	defer func() { color.NoColor = orig }()

	db := openInfoTestDB(t)
	root := filepath.Join(string(filepath.Separator), "lib")
	mustNoErr(t, db.EnsurePending("h1", 1024, "image/jpeg", filepath.Join(root, "2024", "one.jpg"), nil))
	mustNoErr(t, db.EnsurePending("h2", 2048, "image/jpeg", filepath.Join(root, "2024", "two.jpg"), nil))

	folders, err := engine.PendingFolders(db, nil)
	if err != nil {
		t.Fatal(err)
	}

	var files int
	var bytes int64
	full := captureStdout(t, func() { files, bytes = printPendingFolders(folders, false) })
	if files != 2 || bytes != 3072 {
		t.Errorf("totals = %d files / %d bytes, want 2 / 3072", files, bytes)
	}
	for _, want := range []string{"one.jpg", "two.jpg", "2 files", "3.0KB"} {
		if !strings.Contains(full, want) {
			t.Errorf("full listing is missing %q:\n%s", want, full)
		}
	}

	summary := captureStdout(t, func() { printPendingFolders(folders, true) })
	if !strings.Contains(summary, "2 files") {
		t.Errorf("summary lost the per-folder count:\n%s", summary)
	}
	if strings.Contains(summary, "one.jpg") || strings.Contains(summary, "two.jpg") {
		t.Errorf("--summary must not list individual files:\n%s", summary)
	}
}
