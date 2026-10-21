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
	want := filepath.Clean(strings.TrimSpace(folder))
	if want == "" || want == "." {
		return "", false
	}
	for _, root := range cfg.SourceFolders {
		if filepath.Clean(strings.TrimSpace(root)) == want {
			// Returns the CONFIGURED string, never one derived from the
			// request, so nothing user-supplied reaches the scanner or the
			// watch loop -- a traversal has nothing to travel through; the
			// request only selects which stored folder to use.
			//
			// Exact match, not a prefix: the only things that submit a
			// folder are <select>s listing these very entries, so accepting
			// arbitrary subpaths would widen the surface for no feature.
			return root, true
		}
	}
	return "", false
}

// handleSyncFolder queues one folder for a cycle.
func handleSyncFolder(w http.ResponseWriter, r *http.Request, wc Controller) {
	folder := strings.TrimSpace(r.FormValue("folder"))
	// Rebuilt from a validated tab name rather than echoing a URL back
	// from the form: nothing user-supplied reaches the Location header,
	// which is the only way to be sure it cannot point off-site.
	back := browseBackURL(r)

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

// browseTabURL turns a tab name into a Browse URL, server-side.
//
// An allowlist, not a validated echo: only these names produce anything
// and the URL itself is a constant. Earlier versions took the whole
// return URL from the form and tried to prove it was local -- twice with
// a check that missed a leading backslash and control characters. Not
// accepting the input at all removes the question.
// browseBackURL is where a form on Browse returns to: the tab it was
// posted from, with the drill-down filter it was showing, so acting on one
// row of a filtered list does not drop you back into the whole tab.
//
// Still server-constructed: the tab passes browseTabURL's allowlist, and
// the filter is re-parsed through engine.ParseBrowseFilter, which keeps
// only values from closed sets -- nothing from the request reaches the
// Location header unvalidated, and none of it can change the path.
func browseBackURL(r *http.Request) string {
	back := browseTabURL(r.FormValue("tab"))
	if f := engine.ParseBrowseFilter(r.Form); f.Active() {
		back += "&" + f.Values().Encode()
	}
	return back
}

func browseTabURL(tab string) string {
	switch tab {
	case "pending", "failed_retryable", "failed_permanent", "uploaded", "needs_review", "ignored", "all":
		return "/browse?type=" + tab
	default:
		return "/browse?type=pending"
	}
}
