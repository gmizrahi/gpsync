package dashboard

import (
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"strings"

	"github.com/gmizrahi/gpsync/internal/auth"
	"github.com/gmizrahi/gpsync/internal/config"
	"github.com/gmizrahi/gpsync/internal/statedb"
)

// The sign-in page exists because gpsync-tray could not authenticate at all:
// the Account card reported whether credentials existed, but getting them
// there needed `gpsync setup` or `gpsync import-rclone` from a terminal. For
// someone who installs the tray and never opens a shell, that is a hard
// block -- the app runs and can do nothing.
//
// This is the rclone half. It reads an existing rclone config and imports a
// Google Photos remote's credentials, which for anyone arriving from rclone
// skips Google's console entirely. Every piece of it already existed in
// internal/auth and was reachable only from the CLI.
//
// Deliberately NOT a GCP wizard: creating an OAuth consent screen and a
// Desktop client has no API, so a human must click through the console
// regardless, and wrapping manual steps in more UI removes none of them.
// docs/setup.md is linked instead.
//
// Note there is no consent flow here, which matters for where this can be
// used from: importing is a local file read, so unlike the browser-based
// flow it works just as well when the dashboard is open on another device.

type signInPageData struct {
	AppName string
	Account accountStatusData

	RcloneConfPath string
	RcloneFound    bool
	Remotes        []string
	RemotesError   string

	Message      string
	ErrorMessage string
}

var signInTmpl = template.Must(template.New("signin").Parse(`
<div class="card">
  <h2>Account</h2>
  <div class="row"><span class="label">OAuth client</span>{{if .Account.HasCredentials}}<span class="badge badge-ok">configured</span>{{else}}<span class="badge badge-err">not configured</span>{{end}}</div>
  {{if .Account.ClientID}}<div class="row"><span class="label">Client ID</span><span class="cid" title="{{.Account.ClientID}}">{{.Account.ClientIDPrefix}}<span class="cid-suffix">{{.Account.ClientIDSuffix}}</span></span></div>{{end}}
  <div class="row"><span class="label">Signed in</span>{{if .Account.HasToken}}<span class="badge badge-ok">yes</span>{{else}}<span class="badge badge-warn">no</span>{{end}}</div>
</div>

{{if .Message}}<div class="banner banner-ok">{{.Message}}</div>{{end}}
{{if .ErrorMessage}}<div class="banner banner-err">{{.ErrorMessage}}</div>{{end}}

<div class="card">
  <h2>Import from rclone</h2>
  {{if not .RcloneFound}}
    <div class="hint">No rclone configuration found. gpsync looks in the usual places and at <code>RCLONE_CONFIG</code> if it is set.</div>
  {{else}}
    <div class="row"><span class="label">Config</span><span class="cid" title="{{.RcloneConfPath}}">{{.RcloneConfPath}}</span></div>
    {{if .RemotesError}}
      <div class="hint">That file could not be read: {{.RemotesError}}</div>
    {{else if not .Remotes}}
      <div class="hint">No Google Photos remotes in that configuration.</div>
    {{else}}
      <div class="hint">Copies the remote's own OAuth client and token into gpsync. Nothing in rclone is changed.</div>
      <form method="post" action="/signin/import-rclone">
        <div class="field">
          <label for="remote">Remote</label>
          <select id="remote" name="remote">
            {{range .Remotes}}<option value="{{.}}">{{.}}</option>{{end}}
          </select>
        </div>
        <button type="submit">Import</button>
      </form>
    {{end}}
  {{end}}
</div>

<div class="card">
  <h2>Set up from scratch</h2>
  <div class="hint">
    Google has no API for creating an OAuth client, so this part is done once in their console by hand.
    Follow <a href="https://github.com/gmizrahi/gpsync/blob/main/docs/setup.md">the setup guide</a>, then run
    <code>gpsync setup</code> once from a terminal on this computer.
  </div>
</div>
`))

func renderSignInPage(db *statedb.DB, cfg config.Config, appName, msg, errMsg string) (string, error) {
	data := signInPageData{
		AppName:      appName,
		Account:      accountStatus(),
		Message:      msg,
		ErrorMessage: errMsg,
	}

	if path, ok := auth.FindRcloneConf(); ok {
		data.RcloneConfPath = path
		data.RcloneFound = true
		remotes, err := auth.ListGooglePhotosRemotes(path)
		if err != nil {
			data.RemotesError = err.Error()
		} else {
			data.Remotes = remotes
		}
	}

	var buf strings.Builder
	if err := signInTmpl.Execute(&buf, data); err != nil {
		return "", err
	}
	return pageShell(db, "Sign in", "signin", buf.String(), cfg.Theme, cfg.DashboardAuthEnabled), nil
}

// handleSignInImportRclone imports the chosen remote's credentials.
//
// The remote name comes from the client, so it is re-derived against the
// list the server itself found rather than trusted -- the same discipline
// dupGroupsFiltered and backupFileFromRequest already apply. The config
// PATH is never client-supplied, so this cannot be pointed at an arbitrary
// file, but validating membership keeps a crafted name from reaching
// ImportRemote's parser at all.
func handleSignInImportRclone(w http.ResponseWriter, r *http.Request, _ *statedb.DB, _ Controller) {
	remote := strings.TrimSpace(r.FormValue("remote"))
	if remote == "" {
		redirectSignIn(w, r, "", "choose a remote to import")
		return
	}

	path, ok := auth.FindRcloneConf()
	if !ok {
		redirectSignIn(w, r, "", "no rclone configuration found")
		return
	}
	remotes, err := auth.ListGooglePhotosRemotes(path)
	if err != nil {
		redirectSignIn(w, r, "", fmt.Sprintf("reading the rclone configuration: %v", err))
		return
	}
	known := false
	for _, x := range remotes {
		if x == remote {
			known = true
			break
		}
	}
	if !known {
		redirectSignIn(w, r, "", "that remote is not a Google Photos remote in this rclone configuration")
		return
	}

	// ImportRemote's own errors are the useful ones -- in particular it
	// refuses a remote using rclone's shared built-in OAuth app and explains
	// that it pools quota with every other rclone user. Surfaced verbatim
	// rather than replaced with "import failed", which would lose the
	// reason and the remedy.
	if _, err := auth.ImportRemote(path, remote); err != nil {
		redirectSignIn(w, r, "", err.Error())
		return
	}
	redirectSignIn(w, r, fmt.Sprintf("Imported %q. gpsync is signed in.", remote), "")
}

func redirectSignIn(w http.ResponseWriter, r *http.Request, msg, errMsg string) {
	v := url.Values{}
	if msg != "" {
		v.Set("imported", msg)
	}
	if errMsg != "" {
		v.Set("error", errMsg)
	}
	target := "/signin"
	if len(v) > 0 {
		target += "?" + v.Encode()
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}
