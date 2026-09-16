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
	"github.com/gmizrahi/gpsync/internal/statedb"
)

type duplicatesPageData struct {
	NotConfigured  bool
	TrashDir       string
	ErrorMessage   string
	Moved          int
	FreedStr       string
	Done           bool
	Index          int // 0-based -- the raw group index, for the Keep & Next form's hidden field
	GroupIndex     int // 1-based -- for the "Group X of Y" display only
	GroupTotal     int
	ReclaimableStr string
	SizeStr        string
	SizeBytes      int64
	SHA256         string
	NextIndex      int
	Items          []duplicateItem
}

type duplicateItem struct {
	Path     string
	Dir      string
	Filename string
	SizeStr  string
	IsImage  bool
	ThumbSrc string
}

var duplicatesTmpl = template.Must(template.New("duplicates").Parse(`
{{if .ErrorMessage}}<div class="banner banner-err">{{.ErrorMessage}}</div>{{end}}
{{if .Moved}}<div class="banner banner-ok">Moved {{.Moved}} file(s) to the trash, freed {{.FreedStr}}.</div>{{end}}
{{if .NotConfigured}}
<div class="card">
  <h2>Duplicates</h2>
  <div class="hint">No trash folder configured yet. Set one on the <a href="/settings">Settings</a> page first -- copies you don't keep are moved there, never deleted.</div>
</div>
{{else if .Done}}
<div class="card">
  <h2>Duplicates</h2>
  <div class="hint">No duplicate content found. Everything in your library is a single copy.</div>
</div>
{{else}}
<div class="card">
  <div class="row"><span class="label">Progress</span><span>Group {{.GroupIndex}} of {{.GroupTotal}} remaining &nbsp; (~{{.ReclaimableStr}} reclaimable)</span></div>
  <div class="row"><span class="label">Size</span><span>{{.SizeStr}} each &nbsp; {{len .Items}} copies</span></div>
</div>
<form method="post" action="/duplicates/resolve">
  <input type="hidden" name="i" value="{{.Index}}">
  <input type="hidden" name="sha256" value="{{.SHA256}}">
  <input type="hidden" name="size_bytes" value="{{.SizeBytes}}">
  {{range .Items}}<input type="hidden" name="paths" value="{{.Path}}">{{end}}
  <div class="dup-grid">
    {{range $i, $it := .Items}}
    <label class="dup-card">
      <input type="radio" name="keep" value="{{$it.Path}}" {{if eq $i 0}}checked{{end}}>
      {{if $it.IsImage}}<img class="dup-thumb" src="{{$it.ThumbSrc}}" alt="">{{else}}<div class="dup-thumb dup-thumb-video">Video</div>{{end}}
      <div class="dup-name">{{$it.Filename}}</div>
      <div class="dup-dir">{{$it.Dir}}</div>
    </label>
    {{end}}
  </div>
  <button type="submit">Keep &amp; Next</button>
  <a href="/duplicates?i={{.NextIndex}}"><button type="button" class="btn-sm">Skip</button></a>
</form>
{{end}}
`))

// dupGroupsFiltered returns db.DuplicateGroups() with each group narrowed
// to copies still actually on disk (engine.ExistingPaths), dropping any
// group left with fewer than 2 survivors -- exactly
// cmd/gpsync/duplicates_resolve.go's own skip rule. Recomputed fresh on every
// call rather than cached: a group just resolved must not still appear
// on the very next request.
func dupGroupsFiltered(db *statedb.DB) ([]statedb.DuplicateGroup, error) {
	groups, err := db.DuplicateGroups()
	if err != nil {
		return nil, err
	}
	var out []statedb.DuplicateGroup
	for _, g := range groups {
		g.Paths = engine.ExistingPaths(g.Paths)
		if len(g.Paths) >= 2 {
			out = append(out, g)
		}
	}
	return out, nil
}

func renderDuplicatesPage(db *statedb.DB, cfg config.Config, index int, moved int, freed int64, errMsg string) (string, error) {
	data := duplicatesPageData{ErrorMessage: errMsg, Moved: moved, FreedStr: humanBytes(freed)}
	if cfg.TrashDir == "" {
		data.NotConfigured = true
		var buf strings.Builder
		if err := duplicatesTmpl.Execute(&buf, data); err != nil {
			return "", err
		}
		return pageShell(db, "Duplicates", "duplicates", buf.String(), cfg.Theme, cfg.DashboardAuthEnabled), nil
	}
	data.TrashDir = cfg.TrashDir

	groups, err := dupGroupsFiltered(db)
	if err != nil {
		return "", err
	}
	if len(groups) == 0 {
		data.Done = true
	} else {
		if index < 0 {
			index = 0
		}
		if index >= len(groups) {
			index = len(groups) - 1
		}
		g := groups[index]
		var reclaimable int64
		for _, gr := range groups {
			reclaimable += gr.Size * int64(len(gr.Paths)-1)
		}
		data.Index = index
		data.GroupIndex = index + 1
		data.GroupTotal = len(groups)
		data.ReclaimableStr = humanBytes(reclaimable)
		data.SizeStr = humanBytes(g.Size)
		data.SizeBytes = g.Size
		data.SHA256 = g.SHA256
		data.NextIndex = index + 1
		for _, p := range g.Paths {
			data.Items = append(data.Items, duplicateItem{
				Path:     p,
				Dir:      pathx.DisplayDir(p),
				Filename: pathx.DisplayName(p),
				SizeStr:  humanBytes(g.Size),
				IsImage:  extensions.KindOf(p) == extensions.KindPhoto,
				ThumbSrc: "/duplicates/thumb?path=" + url.QueryEscape(p),
			})
		}
	}

	var buf strings.Builder
	if err := duplicatesTmpl.Execute(&buf, data); err != nil {
		return "", err
	}
	return pageShell(db, "Duplicates", "duplicates", buf.String(), cfg.Theme, cfg.DashboardAuthEnabled), nil
}

// ── Originals-folder review ────────────────────────────────────────────

func handleDuplicatesResolve(w http.ResponseWriter, r *http.Request, db *statedb.DB, wc Controller) {
	if err := r.ParseForm(); err != nil {
		http.Redirect(w, r, "/duplicates?error="+url.QueryEscape(err.Error()), http.StatusSeeOther)
		return
	}
	cfg := wc.Config()
	index, _ := strconv.Atoi(r.FormValue("i"))
	sha256 := r.FormValue("sha256")
	keep := r.FormValue("keep")

	// The client's own "paths"/"size_bytes" fields are NEVER trusted for
	// what actually gets moved -- see engine.FindDuplicateGroupAndValidateKeep's
	// doc comment for why: without this check, a plain POST to a
	// LAN-reachable dashboard became an arbitrary file-move primitive. Only
	// sha256/keep come from the request; group.Paths/group.Size below are
	// read straight from the CURRENT, real group.
	groups, err := dupGroupsFiltered(db)
	if err != nil {
		http.Redirect(w, r, fmt.Sprintf("/duplicates?i=%d&error=%s", index, url.QueryEscape(err.Error())), http.StatusSeeOther)
		return
	}
	group, ok := engine.FindDuplicateGroupAndValidateKeep(groups, sha256, keep)
	if !ok {
		http.Redirect(w, r, fmt.Sprintf("/duplicates?i=%d&error=%s", index, url.QueryEscape("invalid submission")), http.StatusSeeOther)
		return
	}

	moved, freed, err := engine.ResolveDuplicateGroup(db, sha256, keep, group.Paths, group.Size, cfg.TrashDir)
	if err != nil {
		http.Redirect(w, r, fmt.Sprintf("/duplicates?i=%d&error=%s", index, url.QueryEscape(err.Error())), http.StatusSeeOther)
		return
	}
	v := url.Values{}
	v.Set("i", strconv.Itoa(index))
	v.Set("moved", strconv.Itoa(moved))
	v.Set("freed", strconv.FormatInt(freed, 10))
	http.Redirect(w, r, "/duplicates?"+v.Encode(), http.StatusSeeOther)
}

// handleDuplicatesThumb serves a resized preview -- but ONLY for a path
// that is currently a real member of a real duplicate group, re-derived
// server-side on every request via dupGroupsFiltered, never a raw
// client-supplied path read off disk unchecked. This dashboard can be
// LAN-exposed (0.0.0.0 binding), so an arbitrary-file-read endpoint here
// would be a real vulnerability, not just sloppy -- see
// sessionAuthMiddleware's own doc comment on the same exposure.
func handleDuplicatesThumb(w http.ResponseWriter, r *http.Request, db *statedb.DB) {
	path := r.URL.Query().Get("path")
	groups, err := dupGroupsFiltered(db)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	known := false
	for _, g := range groups {
		for _, p := range g.Paths {
			if p == path {
				known = true
				break
			}
		}
	}
	if !known {
		http.NotFound(w, r)
		return
	}
	data, contentType, ok := engine.GenerateThumbnail(path, 300)
	if !ok {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Cache-Control", "private, max-age=3600")
	w.Write(data)
}
