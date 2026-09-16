package dashboard

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gmizrahi/gpsync/internal/uploader"
)

func TestHandler_StatusPage_Renders200(t *testing.T) {
	db := openTestDB(t)
	ctrl := newFakeController()
	srv := newTestServer(t, db, ctrl, Options{AppName: "gpsync"})
	client := newTestClient(t)

	resp := mustGet(t, client, srv.URL+"/")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
}

// TestHandler_StatusPage_SkeletonSectionsInOrder locks in the page's
// section order -- Activity+Status, then Uploading/Recent activity, then
// the Last message strip, then API usage/Library at the very end. The
// actual card CONTENT within each
// container is built client-side (this page polls /api/status and
// renders in JS, not server-side), so this only locks in the STATIC
// skeleton's own div order -- the one part actually expressed in this
// Go file that a copy-paste reorder could silently break.
func TestHandler_StatusPage_SkeletonSectionsInOrder(t *testing.T) {
	db := openTestDB(t)
	ctrl := newFakeController()
	srv := newTestServer(t, db, ctrl, Options{})
	client := newTestClient(t)

	resp := mustGet(t, client, srv.URL+"/")
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	page := string(body)

	top := strings.Index(page, `id="status-top"`)
	live := strings.Index(page, `class="status-live"`)
	footer := strings.Index(page, `id="status-footer"`)
	bottom := strings.Index(page, `id="status-bottom"`)
	if top < 0 || live < 0 || footer < 0 || bottom < 0 {
		t.Fatalf("one or more status page sections missing entirely; top=%d live=%d footer=%d bottom=%d", top, live, footer, bottom)
	}
	if !(top < live && live < footer && footer < bottom) {
		t.Errorf("sections out of order: top=%d live=%d footer=%d bottom=%d, want strictly increasing", top, live, footer, bottom)
	}
}

func TestHandler_StatisticsAndBrowseAndBackupPages_Render200(t *testing.T) {
	db := openTestDB(t)
	ctrl := newFakeController()
	srv := newTestServer(t, db, ctrl, Options{})
	client := newTestClient(t)

	for _, path := range []string{"/statistics", "/browse", "/backup", "/settings", "/duplicates", "/originals"} {
		resp := mustGet(t, client, srv.URL+path)
		if resp.StatusCode != http.StatusOK {
			t.Errorf("GET %s: status = %d, want 200", path, resp.StatusCode)
		}
		resp.Body.Close()
	}
}

func TestHandler_APIStatus_ReflectsController(t *testing.T) {
	db := openTestDB(t)
	ctrl := newFakeController()
	ctrl.running = true
	ctrl.currentFolder = `C:\Photos\2026`
	srv := newTestServer(t, db, ctrl, Options{})
	client := newTestClient(t)

	resp := mustGet(t, client, srv.URL+"/api/status")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}

	var got struct {
		Watching      bool   `json:"watching"`
		CurrentFolder string `json:"current_folder"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if !got.Watching {
		t.Error(`"watching" = false, want true -- the response never reflected ctrl.running`)
	}
	if got.CurrentFolder != `C:\Photos\2026` {
		t.Errorf(`"current_folder" = %q, want %q -- the response never reflected ctrl.currentFolder`, got.CurrentFolder, `C:\Photos\2026`)
	}
}

// TestHandler_APIStatus_RecentEventsCarryErrorKind proves buildStatus
// forwards uploader.ProgressEvent.LastErrorKind into the JSON response's
// error_kind field -- the badge logic in statusPageBody's own JS reads
// this to show THROTTLED/QUOTA/RETRYING instead of one flat FAIL for every
// kind of failure.
func TestHandler_APIStatus_RecentEventsCarryErrorKind(t *testing.T) {
	db := openTestDB(t)
	ctrl := newFakeController()
	ctrl.recent = []uploader.ProgressEvent{
		{LastFile: "/lib/throttled.jpg", LastOK: false, LastErrorKind: "throttle"},
	}
	srv := newTestServer(t, db, ctrl, Options{})
	client := newTestClient(t)

	resp := mustGet(t, client, srv.URL+"/api/status")
	defer resp.Body.Close()

	var got struct {
		Recent []struct {
			Name      string `json:"name"`
			ErrorKind string `json:"error_kind"`
		} `json:"recent"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if len(got.Recent) != 1 || got.Recent[0].ErrorKind != "throttle" {
		t.Errorf("recent = %+v, want exactly one event with error_kind=throttle", got.Recent)
	}
}

func TestHandler_APIRetryNow_CallsController(t *testing.T) {
	db := openTestDB(t)
	ctrl := newFakeController()
	srv := newTestServer(t, db, ctrl, Options{})
	client := newTestClient(t)

	resp := mustPostForm(t, client, srv.URL+"/api/retry-now", srv.URL, "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("status = %d, want 204", resp.StatusCode)
	}
	if !ctrl.retryRequested {
		t.Error("RequestRetryNow was not called on the controller")
	}
}

func TestHandler_APISkipFile_CallsControllerWithPath(t *testing.T) {
	db := openTestDB(t)
	ctrl := newFakeController()
	srv := newTestServer(t, db, ctrl, Options{})
	client := newTestClient(t)

	resp := mustPostForm(t, client, srv.URL+"/api/skip-file", srv.URL, "path=C%3A%5CPhotos%5Cfoo.jpg")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("status = %d, want 204", resp.StatusCode)
	}
	if ctrl.cancelledPath != `C:\Photos\foo.jpg` {
		t.Errorf("cancelledPath = %q, want the submitted path", ctrl.cancelledPath)
	}
}

func TestHandler_APISkipFile_MissingPath_400(t *testing.T) {
	db := openTestDB(t)
	ctrl := newFakeController()
	srv := newTestServer(t, db, ctrl, Options{})
	client := newTestClient(t)

	resp := mustPostForm(t, client, srv.URL+"/api/skip-file", srv.URL, "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for a missing path", resp.StatusCode)
	}
}

func TestHandler_APIAutostartToggle_NilAutostart_501(t *testing.T) {
	db := openTestDB(t)
	ctrl := newFakeController()
	srv := newTestServer(t, db, ctrl, Options{}) // Autostart left nil
	client := newTestClient(t)

	resp := mustPostForm(t, client, srv.URL+"/api/autostart-toggle", srv.URL, "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotImplemented {
		t.Errorf("status = %d, want 501 when no AutostartController is configured", resp.StatusCode)
	}
}

// fakeAutostart is a trivial in-memory AutostartController double.
type fakeAutostart struct{ installed bool }

func (f *fakeAutostart) Installed() (bool, error) { return f.installed, nil }
func (f *fakeAutostart) Install(string) error     { f.installed = true; return nil }
func (f *fakeAutostart) Uninstall() error         { f.installed = false; return nil }

func TestHandler_APIAutostartToggle_TogglesRealController(t *testing.T) {
	db := openTestDB(t)
	ctrl := newFakeController()
	auto := &fakeAutostart{installed: false}
	srv := newTestServer(t, db, ctrl, Options{Autostart: auto})
	client := newTestClient(t)

	resp := mustPostForm(t, client, srv.URL+"/api/autostart-toggle", srv.URL, "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", resp.StatusCode)
	}
	if !auto.installed {
		t.Error("autostart should now be installed")
	}
}

func TestHandler_SettingsSave_PersistsAndAppliesToController(t *testing.T) {
	db := openTestDB(t)
	ctrl := newFakeController()
	ctrl.running = true
	srv := newTestServer(t, db, ctrl, Options{})
	client := newTestClient(t)

	form := "source_folders=&concurrency=9&media_type_filter=" +
		"&watch_debounce_seconds=8&watch_heartbeat_minutes=15" +
		"&upload_quality=original&space_saver_max_dimension=2048&space_saver_jpeg_quality=85" +
		"&backup_dir=&backup_keep_count=10&trash_dir=&dupes_dir=" +
		"&theme=system&sync_strategy=folder_by_folder" +
		"&dashboard_listen_addr=127.0.0.1&dashboard_port=0"

	resp := mustPostForm(t, client, srv.URL+"/settings", srv.URL, form)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); !strings.HasPrefix(loc, "/settings?saved=1") {
		t.Errorf("Location = %q, want a saved=1 redirect", loc)
	}
	if len(ctrl.setConfigCalls) == 0 || ctrl.setConfigCalls[len(ctrl.setConfigCalls)-1].Concurrency != 9 {
		t.Errorf("SetConfig was not called with the new concurrency=9; calls: %+v", ctrl.setConfigCalls)
	}
	// The engine was running -- a save must restart it (Stop then Start),
	// same "full restart is fine" precedent Pause/Resume already uses.
	if !ctrl.stopped || !ctrl.started {
		t.Error("a settings save while running must Stop() then Start() the engine")
	}
}

func TestHandler_OriginalsResolve_QueuesOrIgnoresAndClearsFromReview(t *testing.T) {
	db := openTestDB(t)
	mustNoErr(t, db.EnsureNeedsReview("hash-orig", 50, "image/jpeg", filepath.Join(t.TempDir(), "originals", "x.jpg"), nil))
	ctrl := newFakeController()
	srv := newTestServer(t, db, ctrl, Options{})
	client := newTestClient(t)

	resp := mustPostForm(t, client, srv.URL+"/originals/resolve", srv.URL, "i=0&sha256=hash-orig&action=ignore")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", resp.StatusCode)
	}

	items, err := db.NeedsReviewItems()
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 0 {
		t.Errorf("needs_review still has %d item(s), want 0 -- resolve should have cleared it", len(items))
	}
}

func TestHandler_OriginalsResolve_InvalidAction_Redirects(t *testing.T) {
	db := openTestDB(t)
	mustNoErr(t, db.EnsureNeedsReview("hash-orig", 50, "image/jpeg", filepath.Join(t.TempDir(), "originals", "x.jpg"), nil))
	ctrl := newFakeController()
	srv := newTestServer(t, db, ctrl, Options{})
	client := newTestClient(t)

	resp := mustPostForm(t, client, srv.URL+"/originals/resolve", srv.URL, "i=0&sha256=hash-orig&action=bogus")
	defer resp.Body.Close()
	loc := resp.Header.Get("Location")
	if !strings.Contains(loc, "error=") {
		t.Errorf("Location = %q, want an error= redirect for an invalid action", loc)
	}

	items, err := db.NeedsReviewItems()
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Error("an invalid action must not resolve the item")
	}
}

func TestHandler_DuplicatesResolve_MovesLosersToTrashAndKeepsChosenFile(t *testing.T) {
	db := openTestDB(t)
	libDir := t.TempDir()
	trashDir := t.TempDir()
	seedDupGroup(t, db, libDir, "hash-dup", 10, 2)

	groups, err := dupGroupsFiltered(db)
	if err != nil || len(groups) != 1 {
		t.Fatalf("setup: dupGroupsFiltered = %+v, %v", groups, err)
	}
	keep := groups[0].Paths[0]

	ctrl := newFakeController()
	ctrl.cfg.TrashDir = trashDir
	srv := newTestServer(t, db, ctrl, Options{})
	client := newTestClient(t)

	form := "i=0&sha256=hash-dup&keep=" + url.QueryEscape(keep)
	resp := mustPostForm(t, client, srv.URL+"/duplicates/resolve", srv.URL, form)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", resp.StatusCode)
	}
	loc := resp.Header.Get("Location")
	if !strings.Contains(loc, "moved=1") {
		t.Errorf("Location = %q, want moved=1 (one loser file moved to trash)", loc)
	}
	if _, err := os.Stat(keep); err != nil {
		t.Errorf("the kept file %s should still exist: %v", keep, err)
	}
}

func TestHandler_SettingsSave_InvalidConcurrency_RedirectsWithError(t *testing.T) {
	db := openTestDB(t)
	ctrl := newFakeController()
	srv := newTestServer(t, db, ctrl, Options{})
	client := newTestClient(t)

	resp := mustPostForm(t, client, srv.URL+"/settings", srv.URL, "concurrency=not-a-number")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", resp.StatusCode)
	}
	loc := resp.Header.Get("Location")
	if !strings.HasPrefix(loc, "/settings?error=") {
		t.Errorf("Location = %q, want an error= redirect for invalid input", loc)
	}
	if ctrl.started || ctrl.stopped {
		t.Error("an invalid submission must never touch the running engine")
	}
}

// TestHandler_APIQuit_RunsTheGracefulHookAfterResponding covers the
// endpoint added so a build can replace gpsync-tray.exe without cutting an
// upload off -- Windows locks a running binary, and killing it outright
// aborts whatever is mid-transfer.
//
// The ordering assertion is the substantive one: the hook shuts down the
// very server handling this request, so it MUST run after the response,
// on its own goroutine. Called inline, it would wait on the request that
// called it.
func TestHandler_APIQuit_RunsTheGracefulHookAfterResponding(t *testing.T) {
	db := openTestDB(t)
	ctrl := newFakeController()
	quit := make(chan struct{})
	srv := newTestServer(t, db, ctrl, Options{QuitGracefully: func() { close(quit) }})
	client := newTestClient(t)

	resp := mustPostForm(t, client, srv.URL+"/api/quit", srv.URL, "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 -- the caller needs an answer before the server goes away", resp.StatusCode)
	}
	select {
	case <-quit:
	case <-time.After(2 * time.Second):
		t.Fatal("the graceful-quit hook was never called")
	}
}

// TestHandler_APIQuit_NoHook_SaysSo: an embedding with no such concept
// must answer honestly rather than silently doing nothing, which would
// leave a caller waiting for a process that is never going to exit.
func TestHandler_APIQuit_NoHook_SaysSo(t *testing.T) {
	db := openTestDB(t)
	srv := newTestServer(t, db, newFakeController(), Options{})
	client := newTestClient(t)

	resp := mustPostForm(t, client, srv.URL+"/api/quit", srv.URL, "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotImplemented {
		t.Errorf("status = %d, want 501 with no QuitGracefully configured", resp.StatusCode)
	}
}

// TestHandler_APIQuit_GETIsRejected: a bare GET must never be able to
// shut the app down -- that is one browser prefetch, or one link, away.
func TestHandler_APIQuit_GETIsRejected(t *testing.T) {
	db := openTestDB(t)
	called := false
	srv := newTestServer(t, db, newFakeController(), Options{QuitGracefully: func() { called = true }})
	client := newTestClient(t)

	resp, err := client.Get(srv.URL + "/api/quit")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405 for GET /api/quit", resp.StatusCode)
	}
	time.Sleep(50 * time.Millisecond)
	if called {
		t.Error("a GET shut the app down")
	}
}
