package dashboard

import (
	"html/template"
	"net/http"
	"net/url"
	"strings"

	"github.com/gmizrahi/gpsync/internal/auth"
	"github.com/gmizrahi/gpsync/internal/config"
	"github.com/gmizrahi/gpsync/internal/engine"
	"github.com/gmizrahi/gpsync/internal/extensions"
	"github.com/gmizrahi/gpsync/internal/statedb"
)

// settingsPageData feeds settingsTmpl -- see ApplySettingsForm's doc
// comment in internal/engine/settings.go for which Config fields this
// form does (and deliberately doesn't) expose.
type settingsPageData struct {
	// Account is read-only status -- never submitted by this form, see
	// accountStatus.
	Account accountStatusData

	Theme             string
	SourceFolders     string
	Concurrency       int
	SyncStrategy      string
	SelectedMediaType string
	DebounceSeconds   int
	HeartbeatMinutes  int

	UploadQuality         string
	SpaceSaverMaxDim      int
	SpaceSaverJPEGQuality int

	BackupDir       string
	BackupKeepCount int
	TrashDir        string
	DupesDir        string

	ExtraSupportedPhotoExtensions string
	ExtraSupportedVideoExtensions string
	ExtraUnsupportedExtensions    string
	ExtraIgnoredFileNames         string
	ExtraIgnoredDirNames          string

	// DashboardListenAddr/DashboardPort/DashboardAuth* -- see
	// config.Config's own doc comment on these fields. DashboardAuthSet
	// reports whether a password hash already exists (so the page can
	// say "leave blank to keep the current password" rather than
	// implying none is set); the hash itself never round-trips into the
	// page.
	DashboardListenAddr  string
	DashboardPort        int
	DashboardAuthEnabled bool
	DashboardAuthUser    string
	DashboardAuthSet     bool
	AppName              string

	Saved           bool
	RestartRequired bool
	ErrorMessage    string
}

// accountStatusData is read-only OAuth status for the Account card --
// sourced from internal/auth's own HasCredentials/HasToken/CurrentClientID
// (already designed for exactly this: CurrentClientID's doc comment notes
// a client ID is a public identifier, not a secret, safe to display).
// gpsync-tray doesn't drive the interactive consent flow itself yet -- that
// still needs `gpsync setup`/`gpsync import-rclone` on the CLI; this card is
// what answers "is it actually signed in" without a terminal.
type accountStatusData struct {
	HasCredentials bool
	ClientID       string
	// ClientIDPrefix/ClientIDSuffix split the ID at Google's constant
	// ".apps.googleusercontent.com" tail, so a phone can drop the part
	// that is identical for every desktop OAuth client. A 71-character ID
	// with no
	// break opportunities could not wrap and ran off a phone screen.
	// Desktop renders prefix+suffix as the same continuous text as before.
	ClientIDPrefix string
	ClientIDSuffix string
	HasToken       bool
}

// googleClientIDSuffix is the tail every Google OAuth client ID ends with.
const googleClientIDSuffix = ".apps.googleusercontent.com"

// splitClientID separates the unique part of a client ID from Google's
// constant suffix. An ID without that suffix (a non-Google or malformed
// value) comes back whole as the prefix, so nothing is ever hidden that
// actually distinguishes one client from another.
func splitClientID(id string) (prefix, suffix string) {
	if strings.HasSuffix(id, googleClientIDSuffix) && len(id) > len(googleClientIDSuffix) {
		return strings.TrimSuffix(id, googleClientIDSuffix), googleClientIDSuffix
	}
	return id, ""
}

func accountStatus() accountStatusData {
	id, _ := auth.CurrentClientID()
	prefix, suffix := splitClientID(id)
	return accountStatusData{
		HasCredentials: auth.HasCredentials(),
		ClientID:       id,
		ClientIDPrefix: prefix,
		ClientIDSuffix: suffix,
		HasToken:       auth.HasToken(),
	}
}

// settingsTmpl is html/template, not string concatenation -- everything
// here (folder paths, an error message) can contain characters a browser
// would interpret as markup, and html/template auto-escapes by context, no
// hand-rolled escaping to get wrong.
//
// Section order is deliberate, not alphabetical: the previous flat,
// uncategorised list gave no sense of what mattered. Appearance and
// Account come first (quick, personal,
// answer "is this even signed in" immediately), then what/how to back up
// in the order you'd actually configure a fresh install (folders, then
// sync behavior, then how often to check), then the less-frequently-
// touched safety-net settings (backup/trash/precheck), with the most
// rarely-touched section (extension overrides) collapsed by default.
var settingsTmpl = template.Must(template.New("settings").Parse(`
{{if .Saved}}<div class="banner banner-ok">Settings saved.{{if .RestartRequired}} Restart {{.AppName}} (Quit, then reopen) for the Remote access changes to take effect.{{end}}</div>{{end}}
{{if .ErrorMessage}}<div class="banner banner-err">{{.ErrorMessage}}</div>{{end}}
<div class="card">
  <h2>Account</h2>
  <div class="row"><span class="label">OAuth client</span>{{if .Account.HasCredentials}}<span class="badge badge-ok">configured</span>{{else}}<span class="badge badge-err">not configured</span>{{end}}</div>
  {{if .Account.ClientID}}<div class="row"><span class="label">Client ID</span><span class="cid" title="{{.Account.ClientID}}">{{.Account.ClientIDPrefix}}<span class="cid-suffix">{{.Account.ClientIDSuffix}}</span></span></div>{{end}}
  <div class="row"><span class="label">Signed in</span>{{if .Account.HasToken}}<span class="badge badge-ok">yes</span>{{else}}<span class="badge badge-warn">no</span>{{end}}</div>
  {{if not .Account.HasCredentials}}<div class="hint">Run <code>gpsync setup</code> or <code>gpsync import-rclone</code> once from a terminal to configure this -- gpsync-tray doesn't run the Google consent flow itself yet.</div>{{end}}
</div>
<form method="post" action="/settings" id="settings-form" onsubmit="document.getElementById('save-btn').disabled=true; document.getElementById('save-btn').textContent='Saving…';">
  <div class="card">
    <h2>Appearance</h2>
    <div class="field">
      <label for="theme">Theme</label>
      <select id="theme" name="theme">
        <option value="system" {{if eq .Theme "system"}}selected{{end}}>System default</option>
        <option value="light" {{if eq .Theme "light"}}selected{{end}}>Light</option>
        <option value="dark" {{if eq .Theme "dark"}}selected{{end}}>Dark</option>
      </select>
    </div>
  </div>
  <div class="card">
    <h2>Source folders</h2>
    <div class="field">
      <label for="source_folders">One folder per line</label>
      <textarea id="source_folders" name="source_folders" rows="6">{{.SourceFolders}}</textarea>
      <div class="hint">Everything under each folder is watched and synced. Changes take effect on save.</div>
    </div>
  </div>
  <div class="card">
    <h2>Sync behavior</h2>
    <div class="field">
      <label for="sync_strategy">Order files upload in</label>
      <select id="sync_strategy" name="sync_strategy">
        <option value="folder_by_folder" {{if eq .SyncStrategy "folder_by_folder"}}selected{{end}}>Folder by folder</option>
        <option value="smallest_first" {{if eq .SyncStrategy "smallest_first"}}selected{{end}}>Smallest files first</option>
        <option value="photos_first" {{if eq .SyncStrategy "photos_first"}}selected{{end}}>Photos before videos</option>
      </select>
      <div class="hint">Applies within each folder as it's processed, not globally across the whole library.</div>
    </div>
    <div class="field">
      <label for="media_type_filter">Media type</label>
      <select id="media_type_filter" name="media_type_filter">
        <option value="all" {{if eq "all" .SelectedMediaType}}selected{{end}}>All</option>
        <option value="photos" {{if eq "photos" .SelectedMediaType}}selected{{end}}>Photos only</option>
        <option value="videos" {{if eq "videos" .SelectedMediaType}}selected{{end}}>Videos only</option>
      </select>
    </div>
    <div class="field">
      <label for="concurrency">Concurrency</label>
      <input type="number" id="concurrency" name="concurrency" min="1" value="{{.Concurrency}}">
      <div class="hint">How many files to upload at once. Higher isn't always faster -- Google throttles sustained bursts.</div>
    </div>
  </div>
  <div class="card">
    <h2>Watching</h2>
    <div class="field">
      <label for="watch_debounce_seconds">Debounce (seconds)</label>
      <input type="number" id="watch_debounce_seconds" name="watch_debounce_seconds" min="1" value="{{.DebounceSeconds}}">
      <div class="hint">Quiet period after a folder's last change before it's scanned/uploaded.</div>
    </div>
    <div class="field">
      <label for="watch_heartbeat_minutes">Heartbeat (minutes)</label>
      <input type="number" id="watch_heartbeat_minutes" name="watch_heartbeat_minutes" min="1" value="{{.HeartbeatMinutes}}">
      <div class="hint">How often to re-check the whole pending backlog regardless of filesystem activity.</div>
    </div>
  </div>
  <div class="card">
    <h2>Upload quality</h2>
    <div class="field">
      <label for="upload_quality">Quality</label>
      <select id="upload_quality" name="upload_quality" onchange="document.getElementById('space-saver-fields').style.display = this.value === 'space_saver' ? '' : 'none';">
        <option value="original" {{if eq .UploadQuality "original"}}selected{{end}}>Original (no re-encoding)</option>
        <option value="space_saver" {{if eq .UploadQuality "space_saver"}}selected{{end}}>Space saver (downscale + re-encode)</option>
      </select>
      <div class="hint">Space saver strips EXIF (including capture date) -- see .ai/CLAUDE.md if that matters to you.</div>
    </div>
    <div id="space-saver-fields" {{if ne .UploadQuality "space_saver"}}style="display:none"{{end}}>
      <div class="field">
        <label for="space_saver_max_dimension">Max dimension (pixels)</label>
        <input type="number" id="space_saver_max_dimension" name="space_saver_max_dimension" min="1" value="{{.SpaceSaverMaxDim}}">
      </div>
      <div class="field">
        <label for="space_saver_jpeg_quality">JPEG quality (1-100)</label>
        <input type="number" id="space_saver_jpeg_quality" name="space_saver_jpeg_quality" min="1" max="100" value="{{.SpaceSaverJPEGQuality}}">
      </div>
    </div>
  </div>
  <div class="card">
    <h2>Backup</h2>
    <div class="field">
      <label for="backup_dir">Backup folder</label>
      <input type="text" id="backup_dir" name="backup_dir" value="{{.BackupDir}}" placeholder="e.g. C:\GPSyncBackups">
      <div class="hint">Where "gpsync backup" snapshots ~/.gpsync -- includes your OAuth credentials, so pick somewhere accordingly.</div>
    </div>
    <div class="field">
      <label for="backup_keep_count">Backups to keep</label>
      <input type="number" id="backup_keep_count" name="backup_keep_count" min="1" value="{{.BackupKeepCount}}">
    </div>
  </div>
  <div class="card">
    <h2>Duplicates &amp; precheck</h2>
    <div class="field">
      <label for="trash_dir">Trash folder</label>
      <input type="text" id="trash_dir" name="trash_dir" value="{{.TrashDir}}" placeholder="e.g. C:\GPSyncTrash">
      <div class="hint">Where "gpsync duplicates resolve" moves copies you choose not to keep. Pick somewhere OUTSIDE any source folder.</div>
    </div>
    <div class="field">
      <label for="dupes_dir">Precheck folder</label>
      <input type="text" id="dupes_dir" name="dupes_dir" value="{{.DupesDir}}" placeholder="e.g. C:\GPSyncDupes">
      <div class="hint">Where "gpsync precheck" moves files it finds already synced, before you copy a folder (e.g. a phone dump) into your library.</div>
    </div>
  </div>
  <div class="card">
    <h2>Remote access</h2>
    <div class="hint" style="margin-bottom:0.8rem;">Changes in this card only take effect after restarting {{.AppName}} (Quit, then reopen) -- not on save.</div>
    <div class="field">
      <label for="dashboard_listen_addr">Listen address</label>
      <input type="text" id="dashboard_listen_addr" name="dashboard_listen_addr" value="{{.DashboardListenAddr}}" placeholder="0.0.0.0">
      <div class="hint"><code>127.0.0.1</code> for this device only, <code>0.0.0.0</code> for your local network, or a specific network interface's IP.</div>
    </div>
    <div class="field">
      <label for="dashboard_port">Port</label>
      <input type="number" id="dashboard_port" name="dashboard_port" min="0" max="65535" value="{{.DashboardPort}}">
      <div class="hint">0 picks one automatically the first time and remembers it. A port you set is never overwritten: if it can't be bound, the dashboard runs on a temporary port for that session and tray.log says why. On Windows, pick a port below 49152 &mdash; Hyper-V/WSL reserves blocks above that and moves them on every reboot.</div>
    </div>
    <div class="field">
      <label><input type="checkbox" id="dashboard_auth_enabled" name="dashboard_auth_enabled" {{if .DashboardAuthEnabled}}checked{{end}} style="width:auto;display:inline-block;vertical-align:middle;margin-right:0.4rem;"> Require a username and password</label>
      <div class="hint">Strongly recommended once the listen address is anything other than 127.0.0.1 -- otherwise anyone on your network can open this dashboard, browse your file list, and pause/cancel uploads.</div>
    </div>
    <div class="field">
      <label for="dashboard_auth_user">Username</label>
      <input type="text" id="dashboard_auth_user" name="dashboard_auth_user" value="{{.DashboardAuthUser}}" autocomplete="off">
    </div>
    <div class="field">
      <label for="dashboard_auth_password">Password</label>
      <input type="password" id="dashboard_auth_password" name="dashboard_auth_password" autocomplete="new-password" placeholder="{{if .DashboardAuthSet}}Leave blank to keep the current password{{else}}Required to enable authentication{{end}}">
    </div>
  </div>
  <details class="advanced">
    <summary>Advanced: extension overrides</summary>
    <div class="card">
      <div class="field">
        <label for="extra_supported_photo_extensions">Extra supported photo extensions</label>
        <textarea id="extra_supported_photo_extensions" name="extra_supported_photo_extensions" rows="2">{{.ExtraSupportedPhotoExtensions}}</textarea>
      </div>
      <div class="field">
        <label for="extra_supported_video_extensions">Extra supported video extensions</label>
        <textarea id="extra_supported_video_extensions" name="extra_supported_video_extensions" rows="2">{{.ExtraSupportedVideoExtensions}}</textarea>
      </div>
      <div class="field">
        <label for="extra_unsupported_extensions">Extra unsupported extensions</label>
        <textarea id="extra_unsupported_extensions" name="extra_unsupported_extensions" rows="2">{{.ExtraUnsupportedExtensions}}</textarea>
      </div>
      <div class="field">
        <label for="extra_ignored_file_names">Extra ignored file names</label>
        <textarea id="extra_ignored_file_names" name="extra_ignored_file_names" rows="2">{{.ExtraIgnoredFileNames}}</textarea>
      </div>
      <div class="field">
        <label for="extra_ignored_dir_names">Extra ignored folder names</label>
        <textarea id="extra_ignored_dir_names" name="extra_ignored_dir_names" rows="2">{{.ExtraIgnoredDirNames}}</textarea>
        <div class="hint">One name per list, one per line. Matched case-insensitively, with or without a leading dot for extensions.</div>
      </div>
    </div>
  </details>
  <button type="submit" id="save-btn">Save</button>
</form>
`))

// mediaTypeFormValue maps a config.Config's Kind back to this form's
// dropdown values -- the inverse of extensions.ParseKind.
func mediaTypeFormValue(k extensions.Kind) string {
	switch k {
	case extensions.KindPhoto:
		return "photos"
	case extensions.KindVideo:
		return "videos"
	default:
		return "all"
	}
}

func renderSettingsPage(db *statedb.DB, cfg config.Config, saved, restartRequired bool, errMsg string) (string, error) {
	data := settingsPageData{
		Account: accountStatus(),

		Theme:             cfg.Theme,
		SourceFolders:     strings.Join(cfg.SourceFolders, "\n"),
		Concurrency:       cfg.Concurrency,
		SyncStrategy:      cfg.SyncStrategy,
		SelectedMediaType: mediaTypeFormValue(cfg.MediaTypeFilter),
		DebounceSeconds:   cfg.WatchDebounceSeconds,
		HeartbeatMinutes:  cfg.WatchHeartbeatMinutes,

		UploadQuality:         cfg.UploadQuality,
		SpaceSaverMaxDim:      cfg.SpaceSaverMaxDim,
		SpaceSaverJPEGQuality: cfg.SpaceSaverJPEGQuality,

		BackupDir:       cfg.BackupDir,
		BackupKeepCount: cfg.BackupKeepCount,
		TrashDir:        cfg.TrashDir,
		DupesDir:        cfg.DupesDir,

		ExtraSupportedPhotoExtensions: strings.Join(cfg.ExtraSupportedPhotoExtensions, "\n"),
		ExtraSupportedVideoExtensions: strings.Join(cfg.ExtraSupportedVideoExtensions, "\n"),
		ExtraUnsupportedExtensions:    strings.Join(cfg.ExtraUnsupportedExtensions, "\n"),
		ExtraIgnoredFileNames:         strings.Join(cfg.ExtraIgnoredFileNames, "\n"),
		ExtraIgnoredDirNames:          strings.Join(cfg.ExtraIgnoredDirNames, "\n"),

		DashboardListenAddr:  cfg.DashboardListenAddr,
		DashboardPort:        cfg.DashboardPort,
		DashboardAuthEnabled: cfg.DashboardAuthEnabled,
		DashboardAuthUser:    cfg.DashboardAuthUser,
		DashboardAuthSet:     cfg.DashboardAuthPassHash != "",
		AppName:              appName,

		Saved:           saved,
		RestartRequired: restartRequired,
		ErrorMessage:    errMsg,
	}
	var buf strings.Builder
	if err := settingsTmpl.Execute(&buf, data); err != nil {
		return "", err
	}
	return pageShell(db, "Settings", "settings", buf.String(), cfg.Theme, cfg.DashboardAuthEnabled), nil
}

// handleSettingsSave applies and persists a settings-page submission, then
// redirects back to /settings with a saved/error banner -- POST-redirect-
// GET, so refreshing the result page never resubmits the form.
//
// Restarting the engine on a successful save (when it was already running)
// is the same "full restart is fine, the baseline pass catches anything
// up" precedent Pause/Resume already established -- simpler and more
// robust than trying to apply a folder-list or concurrency change to an
// already-running watch loop in place.
func handleSettingsSave(w http.ResponseWriter, r *http.Request, wc Controller) {
	if err := r.ParseForm(); err != nil {
		http.Redirect(w, r, "/settings?error="+url.QueryEscape(err.Error()), http.StatusSeeOther)
		return
	}
	current, err := config.Load()
	if err != nil {
		http.Redirect(w, r, "/settings?error="+url.QueryEscape("loading current settings: "+err.Error()), http.StatusSeeOther)
		return
	}
	updated, err := engine.ApplySettingsForm(current, r.PostForm)
	if err != nil {
		http.Redirect(w, r, "/settings?error="+url.QueryEscape(err.Error()), http.StatusSeeOther)
		return
	}
	if err := config.Save(updated); err != nil {
		http.Redirect(w, r, "/settings?error="+url.QueryEscape("saving settings: "+err.Error()), http.StatusSeeOther)
		return
	}

	// Applies immediately -- both target tables are plain package vars,
	// not tied to the watch engine's own start/stop lifecycle. See
	// engine.ApplyExtensionOverrides's doc comment: without this, the
	// Settings page's five extension/ignore-list fields saved to
	// config.toml but never actually changed what a scan/upload did.
	engine.ApplyExtensionOverrides(updated)

	wasRunning := wc.IsRunning()
	wc.SetConfig(updated)
	if wasRunning {
		wc.Stop()
		wc.Start()
	}

	// The dashboard server itself is started once, in onReady(), and
	// deliberately not hot-restarted from inside its own request handler
	// (see config.Config's own doc comment on these fields for why) --
	// so a change to any of the five fields that control it needs an
	// actual app restart, which this banner makes explicit rather than
	// silently doing nothing until the user notices on their own.
	restartRequired := current.DashboardListenAddr != updated.DashboardListenAddr ||
		current.DashboardPort != updated.DashboardPort ||
		current.DashboardAuthEnabled != updated.DashboardAuthEnabled ||
		current.DashboardAuthUser != updated.DashboardAuthUser ||
		current.DashboardAuthPassHash != updated.DashboardAuthPassHash

	redirectTo := "/settings?saved=1"
	if restartRequired {
		redirectTo += "&restart=1"
	}
	http.Redirect(w, r, redirectTo, http.StatusSeeOther)
}
