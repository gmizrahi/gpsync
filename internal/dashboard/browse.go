package dashboard

import (
	"html/template"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/gmizrahi/gpsync/internal/engine"
	"github.com/gmizrahi/gpsync/internal/pathx"
	"github.com/gmizrahi/gpsync/internal/statedb"
)

// browseLimit caps one /browse page. A real library can hold 40k+ pending
// rows, and rendering them in one HTML table would be a multi-megabyte
// page. BrowseFiles (internal/engine) does the actual
// paging; this is just the page size picked for it.
const browseLimit = 200

type browseRowData struct {
	Path     string
	Filename string
	Size     string
	Type     string
	Captured string
	Attempts int
	Error    string
}

// sortColumn is one clickable, sort-aware table header. Href always points
// at the OPPOSITE of the current direction when Active, or ascending on
// this column when not -- see browseSortColumn.
type sortColumn struct {
	Label     string
	Href      string
	Active    bool
	Indicator string
	Center    bool
}

type browsePageData struct {
	Kind        string
	Query       string
	ShowError   bool
	Columns     []sortColumn
	Rows        []browseRowData
	Total       int
	ShowingFrom int
	ShowingTo   int
	PrevLink    string
	NextLink    string
}

// browseTmpl mirrors settingsTmpl's reasoning: html/template, not string
// concatenation -- file paths and error messages routinely contain
// characters a browser would otherwise interpret as markup.
var browseTmpl = template.Must(template.New("browse").Parse(`
<div class="browse-tabs">
  <a href="/browse?type=pending&q={{.Query}}" {{if eq .Kind "pending"}}class="active"{{end}}>Pending</a>
  <a href="/browse?type=failed_retryable&q={{.Query}}" {{if eq .Kind "failed_retryable"}}class="active"{{end}}>Retryable</a>
  <a href="/browse?type=failed_permanent&q={{.Query}}" {{if eq .Kind "failed_permanent"}}class="active"{{end}}>Failed</a>
  <a href="/browse?type=uploaded&q={{.Query}}" {{if eq .Kind "uploaded"}}class="active"{{end}}>Uploaded</a>
  <a href="/browse?type=needs_review&q={{.Query}}" {{if eq .Kind "needs_review"}}class="active"{{end}}>Needs review</a>
  <a href="/browse?type=ignored&q={{.Query}}" {{if eq .Kind "ignored"}}class="active"{{end}}>Ignored</a>
</div>
<form class="search-row" method="get" action="/browse">
  <input type="hidden" name="type" value="{{.Kind}}">
  <input type="text" name="q" value="{{.Query}}" placeholder="Filter by path or hash...">
  <button type="submit">Search</button>
</form>
<div class="card">
  <div class="table-wrap">
  <table class="browse">
    <thead><tr>
      {{range .Columns}}<th{{if .Center}} class="center"{{end}}><a href="{{.Href}}"{{if .Active}} class="active"{{end}}>{{.Label}}{{.Indicator}}</a></th>{{end}}
      {{if .ShowError}}<th>Error</th>{{end}}
    </tr></thead>
    <tbody>
      {{range .Rows}}<tr><td class="b-path">{{.Path}}</td><td class="b-name">{{.Filename}}</td><td class="size" data-label="Size">{{.Size}}</td><td data-label="Type">{{.Type}}</td><td class="center" data-label="Captured">{{.Captured}}</td><td class="center" data-label="Attempts">{{.Attempts}}</td>{{if $.ShowError}}<td class="b-err">{{.Error}}</td>{{end}}</tr>{{end}}
      {{if not .Rows}}<tr><td colspan="7">No matching files.</td></tr>{{end}}
    </tbody>
  </table>
  </div>
  <div class="pager">
    <span>Showing {{.ShowingFrom}}-{{.ShowingTo}} of {{.Total}}</span>
    <span>
      {{if .PrevLink}}<a href="{{.PrevLink}}">&larr; Prev</a>{{else}}<span class="disabled">&larr; Prev</span>{{end}}
      &nbsp;&nbsp;
      {{if .NextLink}}<a href="{{.NextLink}}">Next &rarr;</a>{{else}}<span class="disabled">Next &rarr;</span>{{end}}
    </span>
  </div>
</div>
`))

var browsePageTitles = map[engine.FileListKind]string{
	engine.FileListPending:         "Pending",
	engine.FileListFailedRetryable: "Retryable failures",
	engine.FileListFailedPermanent: "Failed",
	engine.FileListUploaded:        "Uploaded",
	engine.FileListNeedsReview:     "Needs review",
	engine.FileListIgnored:         "Ignored",
}

// browseShowErrorKinds is the subset of kinds that actually carry a
// meaningful last_error_message -- showing the column on Uploaded/Needs
// review/Ignored would just render an empty column every time.
var browseShowErrorKinds = map[engine.FileListKind]bool{
	engine.FileListFailedRetryable: true,
	engine.FileListFailedPermanent: true,
}

// browseColumns is the fixed set of sortable columns, in display order --
// Error isn't here since it's never sortable and only shown on the two
// failure tabs (see browsePageData.ShowError).
var browseColumns = []struct {
	Key    engine.SortKey
	Label  string
	Center bool
}{
	{engine.SortKeyPath, "Path", false},
	{engine.SortKeyName, "Filename", false},
	{engine.SortKeySize, "Size", false},
	{engine.SortKeyType, "Type", false},
	{engine.SortKeyCaptured, "Captured", true},
	{engine.SortKeyAttempts, "Attempts", true},
}

func browseLink(kind engine.FileListKind, query string, sortKey engine.SortKey, sortDesc bool, offset int) string {
	v := url.Values{}
	v.Set("type", string(kind))
	if query != "" {
		v.Set("q", query)
	}
	if sortKey != "" {
		v.Set("sort", string(sortKey))
	}
	if sortDesc {
		v.Set("dir", "desc")
	}
	if offset > 0 {
		v.Set("offset", strconv.Itoa(offset))
	}
	return "/browse?" + v.Encode()
}

// buildSortColumns renders the header row: each column links to itself,
// ascending, unless it's already the active sort -- then it links to the
// opposite direction (a second click flips it) and shows an arrow for the
// CURRENT direction. Changing sort/column always resets back to page 1
// (offset omitted from the link), since a new order makes any prior
// offset meaningless.
func buildSortColumns(kind engine.FileListKind, query string, sortKey engine.SortKey, sortDesc bool) []sortColumn {
	cols := make([]sortColumn, len(browseColumns))
	for i, c := range browseColumns {
		active := sortKey == c.Key || (sortKey == "" && c.Key == engine.SortKeyPath)
		nextDesc := false
		indicator := ""
		if active {
			indicator = " ↑"
			if !sortDesc {
				nextDesc = true
				indicator = " ↓"
			}
		}
		cols[i] = sortColumn{
			Label:     c.Label,
			Href:      browseLink(kind, query, c.Key, nextDesc, 0),
			Active:    active,
			Indicator: indicator,
			Center:    c.Center,
		}
	}
	return cols
}

func renderBrowsePage(db *statedb.DB, kind engine.FileListKind, query string, sortKey engine.SortKey, sortDesc bool, offset int, theme string, authEnabled bool) (string, error) {
	res, err := engine.BrowseFiles(db, engine.BrowseQuery{
		Kind: kind, Search: query, Sort: sortKey, SortDesc: sortDesc, Offset: offset, Limit: browseLimit,
	})
	if err != nil {
		return "", err
	}

	rows := make([]browseRowData, len(res.Rows))
	for i, u := range res.Rows {
		errMsg := ""
		if u.LastErrorMessage.Valid {
			errMsg = u.LastErrorMessage.String
		}
		captured := "—"
		if u.CapturedAt.Valid {
			captured = time.Unix(int64(u.CapturedAt.Float64), 0).Format("2006-01-02")
		}
		rows[i] = browseRowData{
			Path:     pathx.DisplayDir(u.FirstSourcePath),
			Filename: pathx.DisplayName(u.FirstSourcePath),
			Size:     humanBytes(u.Size),
			Type:     mediaKindLabel(u.MimeType),
			Captured: captured,
			Attempts: u.AttemptCount,
			Error:    errMsg,
		}
	}

	data := browsePageData{
		Kind:      string(kind),
		Query:     query,
		ShowError: browseShowErrorKinds[kind],
		Columns:   buildSortColumns(kind, query, sortKey, sortDesc),
		Rows:      rows,
		Total:     res.Total,
	}
	if res.Total > 0 {
		data.ShowingFrom = res.Offset + 1
		data.ShowingTo = res.Offset + len(res.Rows)
	}
	if res.Offset > 0 {
		prevOffset := res.Offset - browseLimit
		if prevOffset < 0 {
			prevOffset = 0
		}
		data.PrevLink = browseLink(kind, query, sortKey, sortDesc, prevOffset)
	}
	if res.Offset+len(res.Rows) < res.Total {
		data.NextLink = browseLink(kind, query, sortKey, sortDesc, res.Offset+browseLimit)
	}

	var buf strings.Builder
	if err := browseTmpl.Execute(&buf, data); err != nil {
		return "", err
	}
	title := browsePageTitles[kind]
	if title == "" {
		title = "Browse"
	}
	return pageShell(db, title, "browse", buf.String(), theme, authEnabled), nil
}
