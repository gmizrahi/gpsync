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

// browseReturn confines the form's return target to this site.
//
// Shares safeReturnTo rather than checking here: a local version of this
// missed a leading "/\\" (which browsers normalise to protocol-relative)
// and embedded control characters (which browsers strip before parsing).
func browseReturn(raw string) string {
	return safeReturnTo(raw, "/browse?type=pending")
}

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
	back := browseReturn(r.FormValue("return"))
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
	default:
		redirectBrowse(w, r, back, "That file is still gone. It is recorded as missing, not removed.", "")
	}
}

// handleBrowseForget drops one ledger row. The file on disk is untouched.
func handleBrowseForget(w http.ResponseWriter, r *http.Request, db *statedb.DB) {
	back := browseReturn(r.FormValue("return"))
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
