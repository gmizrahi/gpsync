package dashboard

import (
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"sync"

	"github.com/gmizrahi/gpsync/internal/config"
	"github.com/gmizrahi/gpsync/internal/scanner"
	"github.com/gmizrahi/gpsync/internal/statedb"
)

// markSyncedPhrase must be typed exactly. mark-synced tells gpsync these
// files are already in Google Photos and must never be uploaded -- getting
// it wrong means photos silently never get backed up, which is worse than
// most mistakes this dashboard can make. Same protection the backup
// restore already uses, for the same reason.
const markSyncedPhrase = "MARK SYNCED"

// markSyncedScanConcurrency matches the CLI's own default.
const markSyncedScanConcurrency = 8

// markSyncedJob tracks the one in-progress pass. Created per Handler call,
// never a package var -- same reason loginAttemptTracker and
// consentTracker are.
//
// One at a time: the pass hashes every file in a folder, and two running
// at once would compete for the single SQLite connection while telling the
// user nothing useful.
type markSyncedJob struct {
	mu     sync.Mutex
	active bool
	folder string
	marked int
	seen   int
	err    error
	done   bool
}

func newMarkSyncedJob() *markSyncedJob { return &markSyncedJob{} }

func (j *markSyncedJob) start(db *statedb.DB, folder string) error {
	j.mu.Lock()
	if j.active {
		busy := j.folder
		j.mu.Unlock()
		return fmt.Errorf("already marking %s -- wait for it to finish", filepath.Base(busy))
	}
	j.active, j.done, j.err, j.folder, j.marked, j.seen = true, false, nil, folder, 0, 0
	j.mu.Unlock()

	// Hashing a folder takes as long as it takes -- far longer than the
	// dashboard's own ReadTimeout -- so the request returns immediately and
	// the page polls for the outcome.
	go func() {
		sum, err := scanner.ScanFolders(db, []string{folder}, "mark_synced",
			markSyncedScanConcurrency, true, true, nil, nil, nil)
		j.mu.Lock()
		j.active, j.done, j.err = false, true, err
		j.marked, j.seen = sum.NewPending, sum.FilesSeen
		j.mu.Unlock()
	}()
	return nil
}

func (j *markSyncedJob) snapshot() (active, done bool, folder string, marked, seen int, err error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.active, j.done, j.folder, j.marked, j.seen, j.err
}

type markSyncedConfirmData struct {
	Folder  string
	Phrase  string
	Quality string
	AppName string
}

var markSyncedConfirmTmpl = template.Must(template.New("marksynced").Parse(`
<div class="card">
  <h2>Mark a folder as already synced</h2>
  <div class="hint">
    This records every file in <code>{{.Folder}}</code> as already in Google Photos, <strong>without uploading anything</strong>.
    gpsync will then never upload them. Use it only for files you know are already there, put up by the Photos app,
    rclone, or another copy of gpsync.
  </div>
  <div class="hint">
    Recorded at quality <strong>{{.Quality}}</strong>, from your configured upload quality. If that is wrong, fix it in
    Settings first &mdash; gpsync cannot ask Google what quality your files are stored at.
  </div>
  <div class="hint">The folder is marked as it is now. Files added later are scanned as new, so nothing is lost by running this again.</div>
  <form method="post" action="/mark-synced">
    <input type="hidden" name="folder" value="{{.Folder}}">
    <div class="field">
      <label for="confirm">Type <code>{{.Phrase}}</code> to continue</label>
      <input type="text" id="confirm" name="confirm" autocomplete="off" placeholder="{{.Phrase}}">
    </div>
    <button type="submit">Mark as synced</button>
    <a href="/browse?type=pending">Cancel</a>
  </form>
</div>
`))

func renderMarkSyncedConfirm(db *statedb.DB, cfg config.Config, folder string) (string, error) {
	var buf strings.Builder
	err := markSyncedConfirmTmpl.Execute(&buf, markSyncedConfirmData{
		Folder:  folder,
		Phrase:  markSyncedPhrase,
		Quality: cfg.UploadQuality,
		AppName: appName,
	})
	if err != nil {
		return "", err
	}
	return pageShell(db, "Mark as synced", "browse", buf.String(), cfg.Theme, cfg.DashboardAuthEnabled), nil
}

// handleMarkSynced validates the folder and the typed phrase, then starts
// the pass in the background.
func handleMarkSynced(w http.ResponseWriter, r *http.Request, db *statedb.DB, wc Controller, job *markSyncedJob) {
	cfg := wc.Config()
	folder := strings.TrimSpace(r.FormValue("folder"))

	back := func(msg, errMsg string) {
		v := url.Values{}
		if msg != "" {
			v.Set("synced", msg)
		}
		if errMsg != "" {
			v.Set("error", errMsg)
		}
		http.Redirect(w, r, "/browse?type=pending&"+v.Encode(), http.StatusSeeOther)
	}

	// Same guard as /sync-folder: unchecked, this would record files from
	// anywhere on the machine as backed up.
	clean, ok := folderIsConfigured(cfg, folder)
	if !ok {
		back("", "that folder is not inside any configured source folder")
		return
	}

	if r.Method == http.MethodGet {
		page, err := renderMarkSyncedConfirm(db, cfg, clean)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte(page))
		return
	}

	// Checked server-side, byte-exact. Any client-side hint is a
	// convenience, never the guard.
	if r.FormValue("confirm") != markSyncedPhrase {
		back("", "type "+markSyncedPhrase+" exactly to confirm")
		return
	}
	if err := job.start(db, clean); err != nil {
		back("", err.Error())
		return
	}
	back("Marking "+filepath.Base(clean)+" as already synced. This hashes every file, so it takes a while.", "")
}

func handleMarkSyncedStatus(w http.ResponseWriter, _ *http.Request, job *markSyncedJob) {
	active, done, folder, marked, seen, err := job.snapshot()
	resp := struct {
		Active bool   `json:"active"`
		Done   bool   `json:"done"`
		Folder string `json:"folder,omitempty"`
		Marked int    `json:"marked"`
		Seen   int    `json:"seen"`
		Error  string `json:"error,omitempty"`
	}{Active: active, Done: done, Folder: folder, Marked: marked, Seen: seen}
	if err != nil {
		resp.Error = err.Error()
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}
