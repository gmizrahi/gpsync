package dashboard

import (
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/gmizrahi/gpsync/internal/config"
	"github.com/gmizrahi/gpsync/internal/engine"
	"github.com/gmizrahi/gpsync/internal/extensions"
	"github.com/gmizrahi/gpsync/internal/pathx"
	"github.com/gmizrahi/gpsync/internal/scanner"
	"github.com/gmizrahi/gpsync/internal/statedb"
)

type originalsPageData struct {
	Done         bool
	ErrorMessage string
	Index        int // 0-based -- the raw item index, for the resolve form's hidden field
	ItemIndex    int // 1-based -- for the "Item X of Y" display only
	ItemTotal    int
	SizeStr      string
	SHA256       string
	Filename     string
	Dir          string
	IsImage      bool
	ThumbSrc     string
	RawSrc       string // the ORIGINAL, unresized file -- what a click on the preview opens
	NextIndex    int    // for "Decide Later" -- move on without resolving anything
	PrevIndex    int    // for "Go Back" -- only rendered when Index > 0
	Candidates   []originalsCandidateItem

	// UploadedFromOriginals/UploadedTotalStr back the "Already uploaded"
	// section -- the dashboard view of `gpsync originals-uploaded`'s own
	// report, shown regardless of whether there's anything left to
	// review (a separate, always-relevant question: what already went
	// out from an originals folder BEFORE this review workflow existed).
	UploadedFromOriginals []originalsUploadedItem
	UploadedTotalStr      string
}

type originalsUploadedItem struct {
	Path        string
	Dir         string
	Filename    string
	SizeStr     string
	MediaItemID string
	LikelyDup   bool // an edited copy is also tracked right next to it
}

type originalsCandidateItem struct {
	Path     string
	Dir      string
	Filename string
	Status   string
	IsImage  bool
	ThumbSrc string
	RawSrc   string
}

var originalsTmpl = template.Must(template.New("originals").Parse(`
{{if .ErrorMessage}}<div class="banner banner-err">{{.ErrorMessage}}</div>{{end}}
{{if .Done}}
<div class="card">
  <h2>Originals</h2>
  <div class="hint">Nothing needs review right now. Every file found inside an "originals" folder has been resolved.</div>
</div>
{{else}}
<div class="card">
  <div class="row"><span class="label">Progress</span><span>Item {{.ItemIndex}} of {{.ItemTotal}} remaining</span></div>
  <div class="row"><span class="label">Size</span><span>{{.SizeStr}}</span></div>
</div>
<div class="card">
  {{if .IsImage}}
  <a class="originals-preview-link" href="{{.RawSrc}}" target="_blank" rel="noopener" title="Click to open the full, unresized original in a new tab">
    <img class="originals-preview" src="{{.ThumbSrc}}" alt="">
  </a>
  {{else}}
  <div class="dup-thumb dup-thumb-video" style="height:300px">Video</div>
  {{end}}
  <div class="dup-name">{{.Filename}}</div>
  <div class="dup-dir">{{.Dir}} (originals folder)</div>
  {{if .IsImage}}<div class="hint">Click the photo to open the full-resolution original in a new tab.</div>{{end}}
</div>
{{if .Candidates}}
<div class="card"><h2>Same name found elsewhere in your library</h2>
<div class="dup-grid">
  {{range .Candidates}}
  <div class="dup-card">
    {{if .IsImage}}<a href="{{.RawSrc}}" target="_blank" rel="noopener" title="Click to open the full, unresized original in a new tab"><img class="dup-thumb" src="{{.ThumbSrc}}" alt=""></a>{{else}}<div class="dup-thumb dup-thumb-video">Video</div>{{end}}
    <div class="dup-name">{{.Filename}}</div>
    <div class="dup-dir">{{.Dir}}{{if .Status}} &mdash; {{.Status}}{{end}}</div>
  </div>
  {{end}}
</div>
</div>
{{else}}
<div class="card"><div class="hint">No file with the same name was found anywhere else in your library.</div></div>
{{end}}
<form method="post" action="/originals/resolve">
  <input type="hidden" name="i" value="{{.Index}}">
  <input type="hidden" name="sha256" value="{{.SHA256}}">
  {{if gt .Index 0}}<a href="/originals?i={{.PrevIndex}}"><button type="button" class="btn-sm">&larr; Go Back</button></a>{{end}}
  <button type="submit" name="action" value="queue">Queue &amp; Upload</button>
  <button type="submit" name="action" value="ignore" class="btn-sm">Add to Ignore List</button>
  <a href="/originals?i={{.NextIndex}}"><button type="button" class="btn-sm">Decide Later</button></a>
</form>
{{end}}
{{if .UploadedFromOriginals}}
<div class="card">
  <h2>Already uploaded from an "originals" folder</h2>
  <div class="hint" style="margin-bottom:0.8rem;">Sent to Google Photos before this review workflow existed -- nothing gpsync can do about these automatically (Google Photos has no API to find/delete a specific upload by path), so this is a lead list to check by hand. {{.UploadedTotalStr}} total.</div>
  <div class="table-wrap"><table class="stats"><thead><tr><th>Folder</th><th>File</th><th class="num">Size</th><th>Confidence</th><th>Media item ID</th></tr></thead><tbody>
    {{range .UploadedFromOriginals}}<tr><td class="name">{{.Dir}}</td><td class="name">{{.Filename}}</td><td class="num">{{.SizeStr}}</td><td>{{if .LikelyDup}}<span class="badge badge-ok">edited copy also tracked</span>{{else}}<span class="badge badge-dim">unconfirmed</span>{{end}}</td><td class="name">{{if .MediaItemID}}{{.MediaItemID}}{{else}}<span class="hint">via mark-synced</span>{{end}}</td></tr>{{end}}
  </tbody></table></div>
</div>
{{end}}
`))

// renderOriginalsPage shows one needs_review item at a time, indexed
// fresh into db.NeedsReviewItems() on every request (never cached) --
// exactly renderDuplicatesPage's own discipline, so a just-resolved item
// is gone from the list on the very next request.
func renderOriginalsPage(db *statedb.DB, cfg config.Config, index int, errMsg string) (string, error) {
	data := originalsPageData{ErrorMessage: errMsg}

	// Resolves the unambiguous cases (an immediate-parent sibling, or no
	// match anywhere at all) BEFORE listing, so the review queue only ever
	// shows genuinely ambiguous items -- see AutoResolveObviousOriginals'
	// own doc comment for why this runs at display time rather than
	// during scanning itself.
	if _, err := engine.AutoResolveObviousOriginals(db); err != nil {
		return "", err
	}

	items, err := db.NeedsReviewItems()
	if err != nil {
		return "", err
	}
	if len(items) == 0 {
		data.Done = true
	} else {
		if index < 0 {
			index = 0
		}
		if index >= len(items) {
			index = len(items) - 1
		}
		it := items[index]
		reviewItem, ok, err := engine.BuildOriginalsReviewItem(db, it.SHA256)
		if err != nil {
			return "", err
		}
		if !ok {
			// Resolved by someone else between the list fetch above and
			// now (a narrow race -- vanishingly unlikely for a single-user
			// local tool, but harmless to handle plainly rather than not
			// at all): ask for a reload instead of risking any recursion.
			data.ErrorMessage = "That item was just resolved -- reload to see what's left."
			var buf strings.Builder
			if err := originalsTmpl.Execute(&buf, data); err != nil {
				return "", err
			}
			return pageShell(db, "Originals", "originals", buf.String(), cfg.Theme, cfg.DashboardAuthEnabled), nil
		}
		data.Index = index
		data.ItemIndex = index + 1
		data.ItemTotal = len(items)
		data.SizeStr = humanBytes(reviewItem.Size)
		data.SHA256 = reviewItem.SHA256
		data.Filename = pathx.DisplayName(reviewItem.FirstSourcePath)
		data.Dir = pathx.DisplayDir(reviewItem.FirstSourcePath)
		data.IsImage = extensions.KindOf(reviewItem.FirstSourcePath) == extensions.KindPhoto
		data.ThumbSrc = "/originals/thumb?size=large&sha256=" + url.QueryEscape(reviewItem.SHA256) + "&path=" + url.QueryEscape(reviewItem.FirstSourcePath)
		data.RawSrc = "/originals/raw?sha256=" + url.QueryEscape(reviewItem.SHA256) + "&path=" + url.QueryEscape(reviewItem.FirstSourcePath)
		// "Decide Later" moves to the next index without resolving
		// anything -- matching the Duplicate
		// Resolver's own "Skip" precedent exactly. Wraps back to 0 rather
		// than past the end, since the list is never longer than
		// ItemTotal and a skip past the last item should just cycle.
		data.NextIndex = index + 1
		if data.NextIndex >= len(items) {
			data.NextIndex = 0
		}
		// "Go Back" is shown only when NOT at the first image: the
		// template guards it with {{if gt .Index
		// 0}}, so PrevIndex being -1 at Index==0 is harmless (never read).
		data.PrevIndex = index - 1
		for _, c := range reviewItem.Candidates {
			data.Candidates = append(data.Candidates, originalsCandidateItem{
				Path:     c.Path,
				Dir:      pathx.DisplayDir(c.Path),
				Filename: pathx.DisplayName(c.Path),
				Status:   c.Status,
				IsImage:  extensions.KindOf(c.Path) == extensions.KindPhoto,
				ThumbSrc: "/originals/thumb?sha256=" + url.QueryEscape(reviewItem.SHA256) + "&path=" + url.QueryEscape(c.Path),
				RawSrc:   "/originals/raw?sha256=" + url.QueryEscape(reviewItem.SHA256) + "&path=" + url.QueryEscape(c.Path),
			})
		}
	}

	// "Already uploaded" -- a separate, always-relevant question from the
	// review queue above: what went out from an originals folder BEFORE
	// this workflow existed to catch it. Same filtering/confidence logic
	// as `gpsync originals-uploaded`.
	if candidates, err := db.OriginalsFolderRowsUploaded(); err == nil {
		var totalBytes int64
		for _, r := range candidates {
			if !scanner.IsInOriginalsFolder(r.FirstSourcePath) {
				continue
			}
			totalBytes += r.Size
			likelyDup := false
			if seen, serr := db.GetFileSeen(scanner.SiblingPath(r.FirstSourcePath)); serr == nil && seen != nil {
				likelyDup = true
			}
			data.UploadedFromOriginals = append(data.UploadedFromOriginals, originalsUploadedItem{
				Path:        r.FirstSourcePath,
				Dir:         pathx.DisplayDir(r.FirstSourcePath),
				Filename:    pathx.DisplayName(r.FirstSourcePath),
				SizeStr:     humanBytes(r.Size),
				MediaItemID: r.MediaItemID,
				LikelyDup:   likelyDup,
			})
		}
		if len(data.UploadedFromOriginals) > 0 {
			data.UploadedTotalStr = fmt.Sprintf("%d file(s), %s", len(data.UploadedFromOriginals), humanBytes(totalBytes))
		}
	}

	var buf strings.Builder
	if err := originalsTmpl.Execute(&buf, data); err != nil {
		return "", err
	}
	return pageShell(db, "Originals", "originals", buf.String(), cfg.Theme, cfg.DashboardAuthEnabled), nil
}

// handleOriginalsResolve applies the user's choice (queue and upload, or
// add to the ignore list) for one review item. Only sha256 and the chosen
// action are ever read from the request -- statedb.ResolveNeedsReview's
// own WHERE status='needs_review' guard means a stale or replayed
// submission can't resurrect or re-touch an already-resolved row, so there
// is nothing here equivalent to FindDuplicateGroupAndValidateKeep's
// separate re-derivation step (that one existed because duplicates-resolve
// used to trust a client-submitted LIST of paths; this endpoint only ever
// acts on the single hash the guard itself protects).
func handleOriginalsResolve(w http.ResponseWriter, r *http.Request, db *statedb.DB) {
	if err := r.ParseForm(); err != nil {
		http.Redirect(w, r, "/originals?error="+url.QueryEscape(err.Error()), http.StatusSeeOther)
		return
	}
	index, _ := strconv.Atoi(r.FormValue("i"))
	sha256 := r.FormValue("sha256")
	action := r.FormValue("action")
	if sha256 == "" || (action != "queue" && action != "ignore") {
		http.Redirect(w, r, fmt.Sprintf("/originals?i=%d&error=%s", index, url.QueryEscape("invalid submission")), http.StatusSeeOther)
		return
	}
	if err := db.ResolveNeedsReview(sha256, action == "queue"); err != nil {
		http.Redirect(w, r, fmt.Sprintf("/originals?i=%d&error=%s", index, url.QueryEscape(err.Error())), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/originals?i=%d", index), http.StatusSeeOther)
}

// originalsReviewMaxDim is the review item's own preview size -- much
// larger than the 300px thumbnails used elsewhere (duplicates, the
// review item's OWN small filmstrip-style card). 300px is fine for
// "which file is this"
// but useless for judging sharpness/quality, which is the whole point of
// looking at a photo before choosing Queue-and-Upload vs
// Add-to-Ignore-List for something that might be a deliberately-discarded
// bad shot. Candidate thumbnails stay at the smaller size (they're
// side-by-side "is this the same photo" confirmation, not a quality
// judgment call).
const originalsReviewMaxDim = 1600

// originalsPathIsKnown reports whether path is currently either the named
// review item's own file or one of its candidates -- the one check
// shared by handleOriginalsThumb and handleOriginalsRaw, both of which
// must never serve a raw client-supplied path unchecked (this dashboard
// can be LAN-exposed). Re-derived server-side via
// engine.BuildOriginalsReviewItem on every call, never cached.
func originalsPathIsKnown(db *statedb.DB, sha256, path string) (bool, error) {
	item, ok, err := engine.BuildOriginalsReviewItem(db, sha256)
	if err != nil {
		return false, err
	}
	if ok && path == item.FirstSourcePath {
		return true, nil
	}
	for _, c := range item.Candidates {
		if c.Path == path {
			return true, nil
		}
	}
	return false, nil
}

// handleOriginalsThumb serves a resized preview -- but ONLY for a path
// that is currently the named review item's own file or one of its
// candidates (see originalsPathIsKnown) -- the same discipline as
// handleDuplicatesThumb, for the same reason.
func handleOriginalsThumb(w http.ResponseWriter, r *http.Request, db *statedb.DB) {
	sha256 := r.URL.Query().Get("sha256")
	path := r.URL.Query().Get("path")
	known, err := originalsPathIsKnown(db, sha256, path)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if !known {
		http.NotFound(w, r)
		return
	}
	// ?size=large is set only on the review item's OWN thumbnail URL (see
	// renderOriginalsPage) -- candidate thumbnails stay at the smaller
	// default, since they're side-by-side "is this the same photo"
	// confirmation, not a quality judgment call.
	maxDim := 300
	if r.URL.Query().Get("size") == "large" {
		maxDim = originalsReviewMaxDim
	}
	data, contentType, ok := engine.GenerateThumbnail(path, maxDim)
	if !ok {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Cache-Control", "private, max-age=3600")
	w.Write(data)
}

// handleOriginalsRaw serves the ORIGINAL, unmodified file -- no resizing
// at all -- so the browser's own native image viewer/zoom can be used to
// actually judge quality at full resolution, not from a thumbnail.
// Same path re-validation discipline as handleOriginalsThumb: only ever
// serves the named review item's own file or one of its candidates, never
// a raw client-supplied path unchecked. http.ServeFile sets Content-Type
// from the file's own extension, so a browser renders it inline (a new
// tab, via the template's target="_blank") rather than downloading it.
func handleOriginalsRaw(w http.ResponseWriter, r *http.Request, db *statedb.DB) {
	sha256 := r.URL.Query().Get("sha256")
	path := r.URL.Query().Get("path")
	known, err := originalsPathIsKnown(db, sha256, path)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if !known {
		http.NotFound(w, r)
		return
	}
	// #nosec G703 -- path is not raw user input: originalsPathIsKnown has
	// already matched it against the review candidates for this hash.
	http.ServeFile(w, r, path)
}
