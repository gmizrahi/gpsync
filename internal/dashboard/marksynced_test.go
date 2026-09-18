package dashboard

import (
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
)

// The phrase is checked server-side, byte-exact. Anything less and a
// stray click records a folder as backed up when it is not -- after which
// gpsync never uploads it.
func TestMarkSynced_WrongPhraseDoesNothing(t *testing.T) {
	db := openTestDB(t)
	ctrl := newFakeController()
	root := t.TempDir()
	ctrl.cfg.SourceFolders = []string{root}

	srv := newTestServer(t, db, ctrl, Options{AppName: "GPhotos Sync"})
	for _, phrase := range []string{"", "mark synced", "MARK  SYNCED", "yes"} {
		resp, err := newTestClient(t).PostForm(srv.URL+"/mark-synced", url.Values{
			"folder":  {root},
			"confirm": {phrase},
		})
		mustNoErr(t, err)
		loc := resp.Header.Get("Location")
		resp.Body.Close()

		decoded, derr := url.QueryUnescape(loc)
		mustNoErr(t, derr)
		if !strings.Contains(decoded, "exactly to confirm") {
			t.Errorf("phrase %q: message = %q, want the confirmation refusal", phrase, decoded)
		}
	}
}

// Same guard as /sync-folder: unchecked, this records files from anywhere
// on the machine as already backed up.
func TestMarkSynced_RefusesAnUnconfiguredFolder(t *testing.T) {
	db := openTestDB(t)
	ctrl := newFakeController()
	ctrl.cfg.SourceFolders = []string{filepath.Join("C:", "Photos")}

	srv := newTestServer(t, db, ctrl, Options{AppName: "GPhotos Sync"})
	resp, err := newTestClient(t).PostForm(srv.URL+"/mark-synced", url.Values{
		"folder":  {filepath.Join("C:", "Windows")},
		"confirm": {markSyncedPhrase},
	})
	mustNoErr(t, err)
	loc := resp.Header.Get("Location")
	resp.Body.Close()

	decoded, derr := url.QueryUnescape(loc)
	mustNoErr(t, derr)
	if !strings.Contains(decoded, "not inside any configured source folder") {
		t.Errorf("message = %q, want the folder guard's refusal", decoded)
	}
}

// The confirm page must state the irreversible part plainly before anyone
// can act on it.
func TestMarkSynced_ConfirmPageExplainsWhatItDoes(t *testing.T) {
	db := openTestDB(t)
	ctrl := newFakeController()
	root := t.TempDir()
	ctrl.cfg.SourceFolders = []string{root}

	srv := newTestServer(t, db, ctrl, Options{AppName: "GPhotos Sync"})
	resp, err := newTestClient(t).Get(srv.URL + "/mark-synced?folder=" + url.QueryEscape(root))
	mustNoErr(t, err)
	body := readBody(t, resp)
	resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	for _, want := range []string{"without uploading", markSyncedPhrase, "never upload them"} {
		if !strings.Contains(body, want) {
			t.Errorf("confirm page does not mention %q", want)
		}
	}
}

func TestMarkSyncedStatus_ReportsIdle(t *testing.T) {
	db := openTestDB(t)
	srv := newTestServer(t, db, newFakeController(), Options{AppName: "GPhotos Sync"})
	resp, err := newTestClient(t).Get(srv.URL + "/mark-synced/status")
	mustNoErr(t, err)
	body := readBody(t, resp)
	resp.Body.Close()

	if !strings.Contains(body, `"active":false`) {
		t.Errorf("idle status = %s, want active false", body)
	}
}
