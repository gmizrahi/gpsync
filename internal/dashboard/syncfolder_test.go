package dashboard

import (
	"errors"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gmizrahi/gpsync/internal/config"
)

// The folder is client-supplied. Without this check the endpoint is "scan
// any directory on this machine", which on a dashboard bound beyond
// loopback would pull paths from outside the library into the ledger.
func TestFolderIsConfigured(t *testing.T) {
	cfg := config.Defaults()
	cfg.SourceFolders = []string{
		filepath.Join("C:", "Photos"),
		filepath.Join("D:", "Media", "Video"),
	}

	for _, tc := range []struct {
		folder string
		want   bool
	}{
		{filepath.Join("C:", "Photos"), true},
		{filepath.Join("C:", "Photos", "2026"), true},
		{filepath.Join("D:", "Media", "Video", "clips"), true},
		// A sibling whose name merely starts with a configured root.
		{filepath.Join("C:", "PhotosOther"), false},
		{filepath.Join("D:", "Media"), false},
		{filepath.Join("C:", "Windows", "System32"), false},
		{"", false},
		{"   ", false},
		{filepath.Join("C:", "Photos", "..", "Windows"), false},
	} {
		if got := folderIsConfigured(cfg, tc.folder); got != tc.want {
			t.Errorf("folderIsConfigured(%q) = %v, want %v", tc.folder, got, tc.want)
		}
	}
}

func TestSyncFolder_QueuesAConfiguredFolder(t *testing.T) {
	db := openTestDB(t)
	ctrl := newFakeController()
	ctrl.cfg.SourceFolders = []string{filepath.Join("C:", "Photos")}

	srv := newTestServer(t, db, ctrl, Options{AppName: "GPhotos Sync"})
	want := filepath.Join("C:", "Photos", "2026")
	resp, err := newTestClient(t).PostForm(srv.URL+"/sync-folder", url.Values{"folder": {want}})
	mustNoErr(t, err)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", resp.StatusCode)
	}
	if ctrl.syncedFolder != want {
		t.Errorf("controller got %q, want %q", ctrl.syncedFolder, want)
	}
	if loc := resp.Header.Get("Location"); !strings.Contains(loc, "synced=") {
		t.Errorf("Location = %q, want a confirmation", loc)
	}
}

// The guard's own message, not merely "an error": a folder outside the
// configured roots must never reach the engine at all.
func TestSyncFolder_RefusesAnUnconfiguredFolder(t *testing.T) {
	db := openTestDB(t)
	ctrl := newFakeController()
	ctrl.cfg.SourceFolders = []string{filepath.Join("C:", "Photos")}

	srv := newTestServer(t, db, ctrl, Options{AppName: "GPhotos Sync"})
	for _, folder := range []string{
		filepath.Join("C:", "Windows", "System32"),
		filepath.Join("C:", "PhotosOther"),
		"",
	} {
		resp, err := newTestClient(t).PostForm(srv.URL+"/sync-folder", url.Values{"folder": {folder}})
		mustNoErr(t, err)
		loc := resp.Header.Get("Location")
		resp.Body.Close()

		decoded, derr := url.QueryUnescape(loc)
		mustNoErr(t, derr)
		if !strings.Contains(decoded, "not inside any configured source folder") {
			t.Errorf("folder %q: message = %q, want the guard's refusal", folder, decoded)
		}
		if ctrl.syncedFolder != "" {
			t.Fatalf("folder %q reached the controller", folder)
		}
	}
}

// A stopped engine reports rather than silently doing nothing -- someone
// just asked for this, and "paused, want one folder synced" is the common
// reason to ask.
func TestSyncFolder_ReportsWhenTheEngineIsStopped(t *testing.T) {
	db := openTestDB(t)
	ctrl := newFakeController()
	ctrl.cfg.SourceFolders = []string{filepath.Join("C:", "Photos")}
	ctrl.syncErr = errors.New("watching is paused -- resume it first, then sync a folder")

	srv := newTestServer(t, db, ctrl, Options{AppName: "GPhotos Sync"})
	resp, err := newTestClient(t).PostForm(srv.URL+"/sync-folder",
		url.Values{"folder": {filepath.Join("C:", "Photos")}})
	mustNoErr(t, err)
	loc := resp.Header.Get("Location")
	resp.Body.Close()

	decoded, derr := url.QueryUnescape(loc)
	mustNoErr(t, derr)
	if !strings.Contains(decoded, "paused") {
		t.Errorf("message = %q, want the controller's reason surfaced", decoded)
	}
}

// The return target comes from the form, so it must never become an open
// redirect.
func TestSyncFolder_ReturnTargetCannotLeaveTheSite(t *testing.T) {
	db := openTestDB(t)
	ctrl := newFakeController()
	ctrl.cfg.SourceFolders = []string{filepath.Join("C:", "Photos")}

	srv := newTestServer(t, db, ctrl, Options{AppName: "GPhotos Sync"})
	for _, back := range []string{"https://evil.example/x", "//evil.example/x", "evil.example"} {
		resp, err := newTestClient(t).PostForm(srv.URL+"/sync-folder", url.Values{
			"folder": {filepath.Join("C:", "Photos")},
			"return": {back},
		})
		mustNoErr(t, err)
		loc := resp.Header.Get("Location")
		resp.Body.Close()
		if !strings.HasPrefix(loc, "/browse") {
			t.Errorf("return %q produced Location %q, want it forced back to /browse", back, loc)
		}
	}
}
