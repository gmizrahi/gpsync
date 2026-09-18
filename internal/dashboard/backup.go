package dashboard

import (
	"fmt"
	"html/template"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gmizrahi/gpsync/internal/backup"
	"github.com/gmizrahi/gpsync/internal/config"
	"github.com/gmizrahi/gpsync/internal/statedb"
)

type backupPageData struct {
	NotConfigured bool
	BackupDir     string
	Created       bool
	CreatedFiles  string
	Unrestricted  bool
	ErrorMessage  string
	Backups       []backupRow
}

type backupRow struct {
	Name    string
	SizeStr string
	When    string
}

var backupTmpl = template.Must(template.New("backup").Parse(`
{{if .Created}}<div class="banner banner-ok">Backup created ({{.CreatedFiles}}).</div>{{end}}
{{if .Unrestricted}}<div class="banner banner-warn">{{.BackupDir}} has no per-user file permissions, so this archive could not be locked to your account. It contains your OAuth credentials.</div>{{end}}
{{if .ErrorMessage}}<div class="banner banner-err">{{.ErrorMessage}}</div>{{end}}
{{if .NotConfigured}}
<div class="card">
  <h2>Backup</h2>
  <div class="hint">Snapshots gpsync's own data &mdash; the ledger, config and credentials in <code>~/.gpsync</code> &mdash; not your photos. No backup folder configured yet; set one on the <a href="/settings">Settings</a> page first.</div>
</div>
{{else}}
<div class="card">
  <h2>Backup</h2>
  <div class="hint" style="margin-bottom:0.9rem;">Snapshots gpsync's own data &mdash; the ledger, config and credentials in <code>~/.gpsync</code> &mdash; not your photos. Losing the ledger means re-hashing the whole library to work out what is already in Google Photos.</div>
  <div class="row"><span class="label">Folder</span><span>{{.BackupDir}}</span></div>
  <form method="post" action="/backup/create">
    <button type="submit">Backup Now</button>
  </form>
</div>
<div class="card">
  <h2>Existing backups</h2>
  {{if not .Backups}}<div class="hint">No backups yet.</div>{{end}}
  {{range .Backups}}
  <div class="row"><span class="label">{{.When}}</span><span>{{.SizeStr}} &nbsp; <a href="/backup/restore?file={{.Name}}"><button type="button" class="btn-sm">Restore</button></a></span></div>
  {{end}}
</div>
{{end}}
`))

// renderBackupPage lists existing backups (newest first) and, unless
// cfg.BackupDir is unset, the Backup Now action -- backup.Create already
// refuses to run without a destination configured, but surfacing that
// up front (matching the Duplicate Resolver's own TrashDir check below)
// is a friendlier first thing to see than an error after clicking.
func renderBackupPage(db *statedb.DB, cfg config.Config, created, unrestricted bool, createdFiles, errMsg string) (string, error) {
	data := backupPageData{
		BackupDir:    cfg.BackupDir,
		Created:      created,
		Unrestricted: unrestricted,
		CreatedFiles: createdFiles,
		ErrorMessage: errMsg,
	}
	if cfg.BackupDir == "" {
		data.NotConfigured = true
	} else if paths, err := backup.List(cfg.BackupDir); err != nil {
		data.ErrorMessage = "listing backups: " + err.Error()
	} else {
		// backup.List returns oldest-first (its own filenames sort
		// chronologically as plain strings) -- reversed here so the
		// newest, most likely to matter, backup is what you see first.
		for i := len(paths) - 1; i >= 0; i-- {
			p := paths[i]
			row := backupRow{Name: filepath.Base(p)}
			if info, statErr := os.Stat(p); statErr == nil {
				row.SizeStr = humanBytes(info.Size())
				row.When = info.ModTime().Format("2006-01-02 15:04:05")
			}
			data.Backups = append(data.Backups, row)
		}
	}
	var buf strings.Builder
	if err := backupTmpl.Execute(&buf, data); err != nil {
		return "", err
	}
	return pageShell(db, "Backup", "backup", buf.String(), cfg.Theme, cfg.DashboardAuthEnabled), nil
}

// backupRestoreConfirmPhrase is what the user must type, byte-exact, to
// actually restore, so a destructive action cannot be a single click.
// Enforced
// SERVER-side in handleBackupRestore; the confirm page's own JS disabling
// the submit button until it matches is a convenience, never the real
// guard -- a client can always bypass client-side JS.
const backupRestoreConfirmPhrase = "RESTORE NOW"

var backupRestoreConfirmTmpl = template.Must(template.New("backup-restore-confirm").Parse(`
{{if .ErrorMessage}}<div class="banner banner-err">{{.ErrorMessage}}</div>{{end}}
<div class="card">
  <h2>Restore backup</h2>
  <div class="row"><span class="label">File</span><span>{{.File}}</span></div>
  <div class="hint">This OVERWRITES the ledger, settings, and saved sign-in with the contents of this backup. A safety copy of what's currently there is made automatically first, but this is still a big, deliberate action -- {{$.AppName}} will close immediately afterward; reopen it to continue.</div>
  <form method="post" action="/backup/restore" onsubmit="return document.getElementById('confirm-text').value === '` + backupRestoreConfirmPhrase + `';">
    <input type="hidden" name="file" value="{{.File}}">
    <div class="field">
      <label for="confirm-text">Type ` + backupRestoreConfirmPhrase + ` to confirm</label>
      <input type="text" id="confirm-text" name="confirm" autocomplete="off" oninput="document.getElementById('restore-btn').disabled = (this.value !== '` + backupRestoreConfirmPhrase + `');">
    </div>
    <button type="submit" id="restore-btn" disabled>Restore Now</button>
  </form>
</div>
`))

type backupRestoreConfirmData struct {
	File         string
	ErrorMessage string
	AppName      string
}

func renderBackupRestoreConfirmPage(db *statedb.DB, file, theme, errMsg string, authEnabled bool) (string, error) {
	var buf strings.Builder
	err := backupRestoreConfirmTmpl.Execute(&buf, backupRestoreConfirmData{File: file, ErrorMessage: errMsg, AppName: appName})
	if err != nil {
		return "", err
	}
	return pageShell(db, "Restore backup", "backup", buf.String(), theme, authEnabled), nil
}

// backupFileFromRequest validates a file query/form value names an actual
// backup CURRENTLY in cfg.BackupDir -- never trusts the raw value as a
// path. filepath.Base strips any directory component a crafted request
// might include (this dashboard can be LAN-exposed, see sessionAuthMiddleware's
// own doc comment), and re-listing the real directory confirms it's an
// actual backup archive, not an arbitrary filename that happens to exist.
func backupFileFromRequest(cfg config.Config, raw string) (fullPath string, ok bool) {
	name := filepath.Base(raw)
	if name == "" || name == "." || name == string(filepath.Separator) {
		return "", false
	}
	paths, err := backup.List(cfg.BackupDir)
	if err != nil {
		return "", false
	}
	for _, p := range paths {
		if filepath.Base(p) == name {
			return p, true
		}
	}
	return "", false
}

func handleBackupCreate(w http.ResponseWriter, r *http.Request, db *statedb.DB, wc Controller) {
	cfg := wc.Config()
	result, err := backup.Create(db, cfg.BackupDir, cfg.BackupKeepCount)
	if err != nil {
		http.Redirect(w, r, "/backup?error="+url.QueryEscape(err.Error()), http.StatusSeeOther)
		return
	}
	v := url.Values{}
	v.Set("created", "1")
	v.Set("files", strings.Join(result.Files, ", "))
	if result.Unrestricted {
		// The backup worked; this is the caveat that comes with the
		// destination the user picked. See backup.Result.Unrestricted.
		v.Set("unrestricted", "1")
	}
	http.Redirect(w, r, "/backup?"+v.Encode(), http.StatusSeeOther)
}

// handleBackupRestore is the one truly destructive action this dashboard
// exposes. On success it deliberately does not try to hot-swap the live DB
// handle or restart the watch engine in-process -- it closes the DB and
// exits the WHOLE app (same mechanism "Quit Now" uses), the same "a big
// structural change needs a real restart" precedent already established
// for the five remote-access settings. This avoids any risk of a stale
// handle or a half-restarted watcher after overwriting the ledger out from
// under a running process.
func handleBackupRestore(w http.ResponseWriter, r *http.Request, db *statedb.DB, wc Controller) {
	if err := r.ParseForm(); err != nil {
		http.Redirect(w, r, "/backup?error="+url.QueryEscape(err.Error()), http.StatusSeeOther)
		return
	}
	cfg := wc.Config()
	path, ok := backupFileFromRequest(cfg, r.FormValue("file"))
	if !ok {
		http.Redirect(w, r, "/backup?error="+url.QueryEscape("that backup no longer exists"), http.StatusSeeOther)
		return
	}
	// Server-side re-check -- the confirm page's JS disabling the submit
	// button is a convenience only, never trusted alone.
	if r.FormValue("confirm") != backupRestoreConfirmPhrase {
		page, err := renderBackupRestoreConfirmPage(db, filepath.Base(path), cfg.Theme, `Type "`+backupRestoreConfirmPhrase+`" exactly to confirm.`, cfg.DashboardAuthEnabled)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte(page))
		return
	}

	if wc.IsRunning() {
		wc.Stop()
	}
	if err := db.Close(); err != nil {
		log.Printf("closing ledger before restore: %v", err)
	}
	result, restoreErr := backup.Restore(path)

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if restoreErr != nil {
		// The DB handle is already closed and the watch engine already
		// stopped -- a failed restore here leaves gpsync-tray unable to do
		// anything useful (no ledger to read/write), so this also closes
		// the app rather than limping on in a half-broken state.
		w.Write([]byte(pageShell(db, "Restore failed", "backup", fmt.Sprintf(
			`<div class="banner banner-err">Restore failed: %s. %s is closing -- reopen it and try again.</div>`,
			template.HTMLEscapeString(restoreErr.Error()), appName), cfg.Theme, cfg.DashboardAuthEnabled)))
	} else {
		w.Write([]byte(pageShell(db, "Restored", "backup", fmt.Sprintf(
			`<div class="banner banner-ok">Restored %d file(s) (a safety copy of what was there before is at %s). %s is closing -- reopen it to continue.</div>`,
			len(result.Files), template.HTMLEscapeString(result.PreRestoreDir), appName), cfg.Theme, cfg.DashboardAuthEnabled)))
	}
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	go func() {
		time.Sleep(500 * time.Millisecond)
		if shutdown != nil {
			shutdown()
		} else {
			os.Exit(0)
		}
	}()
}

// ── Duplicate Resolver ─────────────────────────────────────────────────
