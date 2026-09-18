package dashboard

import (
	"errors"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"time"

	"github.com/gmizrahi/gpsync/internal/engine"
	"github.com/gmizrahi/gpsync/internal/statedb"
)

// browseReturn builds the Browse URL to return to from a tab name.
// Server-constructed allowlist -- see browseTabURL.
func browseReturn(tab string) string { return browseTabURL(tab) }

func redirectBrowse(w http.ResponseWriter, r *http.Request, back, msg, errMsg string) {
	v := url.Values{}
	if msg != "" {
		v.Set("synced", msg)
	}
	if errMsg != "" {
		v.Set("error", errMsg)
	}
	sep := "?"
	if strings.Contains(back, "?") {
		sep = "&"
	}
	http.Redirect(w, r, back+sep+v.Encode(), http.StatusSeeOther)
}

// handleBrowseRecheck re-examines one tracked file.
//
// The hash is client-supplied; engine.RecheckOne refuses one that matches
// no ledger row rather than treating it as a no-op.
func handleBrowseRecheck(w http.ResponseWriter, r *http.Request, db *statedb.DB) {
	back := browseReturn(r.FormValue("tab"))
	sha := strings.TrimSpace(r.FormValue("sha256"))
	if sha == "" {
		redirectBrowse(w, r, back, "", "no file was named")
		return
	}

	outcome, err := engine.RecheckOne(db, sha, time.Now())
	switch {
	case errors.Is(err, engine.ErrUnknownHash):
		redirectBrowse(w, r, back, "", "that file is not in the ledger")
		return
	case err != nil:
		redirectBrowse(w, r, back, "", err.Error())
		return
	}

	switch outcome {
	case engine.RecheckBackOnDisk:
		redirectBrowse(w, r, back, "That file is on disk; the missing flag is cleared.", "")
	case engine.RecheckReplaced:
		redirectBrowse(w, r, back, "A file is at that path, but it holds different content now -- this one was edited or overwritten. Recorded as missing, not removed.", "")
	default:
		redirectBrowse(w, r, back, "That file is still gone. It is recorded as missing, not removed.", "")
	}
}

// handleBrowseForget drops one ledger row. The file on disk is untouched.
func handleBrowseForget(w http.ResponseWriter, r *http.Request, db *statedb.DB) {
	back := browseReturn(r.FormValue("tab"))
	sha := strings.TrimSpace(r.FormValue("sha256"))
	if sha == "" {
		redirectBrowse(w, r, back, "", "no file was named")
		return
	}

	path, err := engine.ForgetOne(db, sha)
	switch {
	case errors.Is(err, engine.ErrUnknownHash):
		redirectBrowse(w, r, back, "", "that file is not in the ledger")
		return
	case err != nil:
		redirectBrowse(w, r, back, "", err.Error())
		return
	}
	redirectBrowse(w, r, back, "Forgot "+filepath.Base(path)+". The file on disk was not touched.", "")
}
