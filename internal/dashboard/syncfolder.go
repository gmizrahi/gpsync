package dashboard

import (
	"net/http"
	"net/url"
	"path/filepath"
	"strings"

	"github.com/gmizrahi/gpsync/internal/config"
	"github.com/gmizrahi/gpsync/internal/engine"
)

// folderIsConfigured reports whether folder sits inside one of the
// configured source folders.
//
// The folder arrives from the client, so it is checked rather than trusted.
// Without this the endpoint is "scan any directory on this machine", which
// on a dashboard that can be bound beyond loopback would populate the
// ledger with paths from outside the library -- an information-disclosure
// primitive, not just sloppiness. Same discipline as originalsPathIsKnown
// and backupFileFromRequest.
//
// Compares cleaned absolute paths and requires a separator boundary, so
// "C:\PhotosOther" is not accepted as living inside "C:\Photos".
func folderIsConfigured(cfg config.Config, folder string) (string, bool) {
	folder = strings.TrimSpace(folder)
	if folder == "" {
		return "", false
	}
	want := filepath.Clean(folder)
	for _, root := range cfg.SourceFolders {
		root = filepath.Clean(strings.TrimSpace(root))
		if root == "" {
			continue
		}
		if want == root || strings.HasPrefix(want, root+string(filepath.Separator)) {
			// The CLEANED path is returned so callers use the value that
			// was actually checked. Validating one string and passing a
			// different one is how a traversal slips past a guard that
			// looked correct.
			return want, true
		}
	}
	return "", false
}

// handleSyncFolder queues one folder for a cycle.
func handleSyncFolder(w http.ResponseWriter, r *http.Request, wc Controller) {
	folder := strings.TrimSpace(r.FormValue("folder"))
	// engine.LoginRedirectTarget, not a hand-rolled check: it also rejects
	// a leading "/\" (browsers normalise that to "//" -- protocol-relative)
	// and any ASCII control character (browsers strip tab/newline before
	// parsing). A local copy of this check missed both.
	back := safeReturnTo(r.FormValue("return"), "/browse?type=pending")

	redirect := func(msg, errMsg string) {
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

	clean, ok := folderIsConfigured(wc.Config(), folder)
	if !ok {
		redirect("", "that folder is not inside any configured source folder")
		return
	}
	if err := wc.SyncFolderNow(clean); err != nil {
		redirect("", err.Error())
		return
	}
	redirect("Queued "+filepath.Base(clean)+" for a sync. It runs after whatever is already in progress.", "")
}

// safeReturnTo confines a form-supplied redirect target to this site.
//
// Delegates to engine.LoginRedirectTarget rather than re-implementing the
// rules: it rejects a leading "//" AND "/\\" (browsers normalise the
// latter to protocol-relative) and any ASCII control character (browsers
// strip tab/newline/CR before parsing a URL, so "/\tevil.example" would
// otherwise sail past a naive second-character check). Both were missed by
// a local copy of this logic, twice.
func safeReturnTo(raw, fallback string) string {
	if raw == "" {
		return fallback
	}
	if got := engine.LoginRedirectTarget(raw); got == raw {
		return raw
	}
	return fallback
}
