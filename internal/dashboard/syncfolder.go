package dashboard

import (
	"net/http"
	"net/url"
	"path/filepath"
	"strings"

	"github.com/gmizrahi/gpsync/internal/config"
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
func folderIsConfigured(cfg config.Config, folder string) bool {
	folder = strings.TrimSpace(folder)
	if folder == "" {
		return false
	}
	want := filepath.Clean(folder)
	for _, root := range cfg.SourceFolders {
		root = filepath.Clean(strings.TrimSpace(root))
		if root == "" {
			continue
		}
		if want == root {
			return true
		}
		if strings.HasPrefix(want, root+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

// handleSyncFolder queues one folder for a cycle.
func handleSyncFolder(w http.ResponseWriter, r *http.Request, wc Controller) {
	folder := strings.TrimSpace(r.FormValue("folder"))
	back := r.FormValue("return")
	if back == "" || !strings.HasPrefix(back, "/") || strings.HasPrefix(back, "//") {
		// Never an absolute or protocol-relative URL: this value comes
		// from the form and is used as a redirect target.
		back = "/browse?type=pending"
	}

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

	if !folderIsConfigured(wc.Config(), folder) {
		redirect("", "that folder is not inside any configured source folder")
		return
	}
	if err := wc.SyncFolderNow(folder); err != nil {
		redirect("", err.Error())
		return
	}
	redirect("Queued "+filepath.Base(folder)+" for a sync. It runs after whatever is already in progress.", "")
}
