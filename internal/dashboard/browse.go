package dashboard

import (
	"fmt"
	"html/template"
	"sort"
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
	SHA256   string
	Path     string
	Filename string
	Size     string
	Type     string
	Captured string
	// When is RowTime, formatted; "—" when the ledger does not know it.
	When string
	// MarkedSynced flags an Uploaded row recorded by Mark as already synced,
	// whose When is the time of marking, not of an upload.
	MarkedSynced bool
	Attempts     int
	Error        string
	// Status is shown only on the All tab, where rows are of every status.
	Status string
}

// browseTab is one tab link. Built in Go, not in the template: a tab keeps
// the current search and drill-down filter, and html/template would
// percent-encode the "&" and "=" of a pre-built query fragment.
type browseTab struct {
	Label  string
	Href   string
	Active bool
}

// hiddenField carries the active drill-down filter through a form, so a
// search or a row action keeps the list it came from.
type hiddenField struct {
	Name, Value string
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
	Kind       string
	Query      string
	Tabs       []browseTab
	ShowError  bool
	ShowStatus bool
	WhenLabel  string
	// FilterChips describe an active drill-down filter in words, and
	// FilterClear is the same list without it. FilterFields carries the
	// filter through the page's forms.
	FilterChips  []string
	FilterClear  string
	FilterFields []hiddenField
	Columns      []sortColumn
	Rows         []browseRowData
	Total        int
	ShowingFrom  int
	ShowingTo    int
	PrevLink     string
	NextLink     string

	// SourceFolders backs the "sync one folder now" control. Only
	// configured folders are offered, and the handler re-checks the
	// submitted value -- a select is a convenience, not a guarantee.
	SourceFolders []string
	Message       string
	ErrorMessage  string
}

// browseTmpl mirrors settingsTmpl's reasoning: html/template, not string
// concatenation -- file paths and error messages routinely contain
// characters a browser would otherwise interpret as markup.
var browseTmpl = template.Must(template.New("browse").Parse(`
<div class="browse-tabs">
  {{range .Tabs}}<a href="{{.Href}}"{{if .Active}} class="active"{{end}}>{{.Label}}</a>{{end}}
</div>
{{if .FilterChips}}<div class="banner banner-info">Showing only {{range .FilterChips}}<span class="badge badge-dim">{{.}}</span> {{end}}&middot; {{.Total}} file(s) &middot; <a href="{{.FilterClear}}">Clear filter</a></div>{{end}}
{{if .Message}}<div class="banner banner-ok">{{.Message}}</div>{{end}}
{{if .ErrorMessage}}<div class="banner banner-err">{{.ErrorMessage}}</div>{{end}}
<div class="browse-toolbar">
{{if .SourceFolders}}
{{/* One folder picker for both folder actions. The second button
     overrides the form's action and method, so it still lands on the
     /mark-synced confirmation page by GET exactly as before -- it only
     reads "folder", so the extra "tab" it now carries is ignored. */}}
<form method="post" action="/sync-folder">
  <input type="hidden" name="tab" value="{{.Kind}}">
  {{range .FilterFields}}<input type="hidden" name="{{.Name}}" value="{{.Value}}">{{end}}
  <select name="folder" aria-label="Source folder">
    {{range .SourceFolders}}<option value="{{.}}">{{.}}</option>{{end}}
  </select>
  <button type="submit">Sync this folder now</button>
  <button type="submit" class="btn-secondary" formaction="/mark-synced" formmethod="get" title="Record these files as already in Google Photos, without uploading">Mark as already synced…</button>
</form>
{{end}}
<form method="get" action="/browse">
  <input type="hidden" name="type" value="{{.Kind}}">
  {{range .FilterFields}}<input type="hidden" name="{{.Name}}" value="{{.Value}}">{{end}}
  <input type="text" class="tb-field-wide" name="q" value="{{.Query}}" placeholder="Filter by path or hash..." aria-label="Filter by path or hash">
  <button type="submit" class="btn-secondary tb-search">Search</button>
</form>
</div>
<div class="card">
  <div class="table-wrap">
  <table class="browse">
    <thead><tr>
      {{range .Columns}}<th{{if .Center}} class="center"{{end}}><a href="{{.Href}}"{{if .Active}} class="active"{{end}}>{{.Label}}{{.Indicator}}</a></th>{{end}}
      {{if .ShowStatus}}<th>Status</th>{{end}}
      {{if .ShowError}}<th>Error</th>{{end}}
      <th class="center">Actions</th>
    </tr></thead>
    <tbody>
      {{range .Rows}}<tr><td class="b-path">{{.Path}}</td><td class="b-name">{{.Filename}}</td><td class="size" data-label="Size">{{.Size}}</td><td data-label="Type">{{.Type}}</td><td class="center" data-label="Captured">{{.Captured}}</td><td class="b-when" data-label="{{$.WhenLabel}}">{{.When}}{{if .MarkedSynced}} <span class="badge badge-dim" title="Recorded with Mark as already synced: gpsync never uploaded this file, and the time is when it was marked">marked synced</span>{{end}}</td><td class="center" data-label="Attempts">{{.Attempts}}</td>{{if $.ShowStatus}}<td data-label="Status">{{.Status}}</td>{{end}}{{if $.ShowError}}<td class="b-err">{{.Error}}</td>{{end}}<td class="center b-actions"><div class="row-actions"><form method="post" action="/browse/recheck"><input type="hidden" name="sha256" value="{{.SHA256}}"><input type="hidden" name="tab" value="{{$.Kind}}">{{range $.FilterFields}}<input type="hidden" name="{{.Name}}" value="{{.Value}}">{{end}}<button type="submit" class="btn-sm" title="Check whether this file is still on disk">Re-check</button></form><form method="post" action="/browse/forget" onsubmit="return confirm('Forget this file? The file on disk is not touched.');"><input type="hidden" name="sha256" value="{{.SHA256}}"><input type="hidden" name="tab" value="{{$.Kind}}">{{range $.FilterFields}}<input type="hidden" name="{{.Name}}" value="{{.Value}}">{{end}}<button type="submit" class="btn-sm" title="Remove this entry from the ledger; the file is not deleted">Forget</button></form></div></td></tr>{{end}}
      {{if not .Rows}}<tr><td colspan="10">No matching files.</td></tr>{{end}}
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

// browseTabOrder is the tab bar, left to right. A status's display name is
// its tab's label (see browseStatusLabel), so the two cannot drift apart.
var browseTabOrder = []struct {
	Kind  engine.FileListKind
	Label string
}{
	{engine.FileListPending, "Pending"},
	{engine.FileListFailedRetryable, "Retryable"},
	{engine.FileListFailedPermanent, "Failed"},
	{engine.FileListUploaded, "Uploaded"},
	{engine.FileListNeedsReview, "Needs review"},
	{engine.FileListIgnored, "Ignored"},
	{engine.FileListAll, "All"},
}

// browseStatusLabel names a ledger status the way its tab does.
func browseStatusLabel(status string) string {
	for _, tab := range browseTabOrder {
		if string(tab.Kind) == status {
			return tab.Label
		}
	}
	return status
}

var browsePageTitles = map[engine.FileListKind]string{
	engine.FileListPending:         "Pending",
	engine.FileListFailedRetryable: "Retryable failures",
	engine.FileListFailedPermanent: "Failed",
	engine.FileListUploaded:        "Uploaded",
	engine.FileListNeedsReview:     "Needs review",
	engine.FileListIgnored:         "Ignored",
	engine.FileListAll:             "All files",
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
	// Left-aligned, unlike Captured: a "marked synced" tag trails some of
	// these dates, and centred they would no longer line up.
	{engine.SortKeyWhen, "", false}, // labelled per tab -- see browseWhenLabels
	{engine.SortKeyAttempts, "Attempts", true},
}

// browseWhenLabels heads the timestamp column, which means something
// different on each tab: RowTime is uploaded_at on Uploaded and the time
// the row entered its status everywhere else. Failed rather than "Last
// failed" on the permanent tab, because a permanent failure is not
// retried, so there is no later failure for it to be the last of.
var browseWhenLabels = map[engine.FileListKind]string{
	engine.FileListPending:         "Queued",
	engine.FileListFailedRetryable: "Last failed",
	engine.FileListFailedPermanent: "Failed",
	engine.FileListUploaded:        "Uploaded",
	engine.FileListNeedsReview:     "Flagged",
	engine.FileListIgnored:         "Ignored",
	// Rows of every status: RowTime is each one's own -- an upload for an
	// uploaded row, entering its status for the rest.
	engine.FileListAll: "Last change",
}

func browseLink(kind engine.FileListKind, query string, sortKey engine.SortKey, sortDesc bool, offset int, filter engine.BrowseFilter) string {
	v := filter.Values()
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
func buildSortColumns(kind engine.FileListKind, query string, sortKey engine.SortKey, sortDesc bool, filter engine.BrowseFilter) []sortColumn {
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
		label := c.Label
		if c.Key == engine.SortKeyWhen {
			label = browseWhenLabels[kind]
		}
		cols[i] = sortColumn{
			Label:     label,
			Href:      browseLink(kind, query, c.Key, nextDesc, 0, filter),
			Active:    active,
			Indicator: indicator,
			Center:    c.Center,
		}
	}
	return cols
}

func renderBrowsePage(db *statedb.DB, kind engine.FileListKind, query string, sortKey engine.SortKey, sortDesc bool, offset int, filter engine.BrowseFilter, theme string, authEnabled bool, sourceFolders []string, msg, errMsg string) (string, error) {
	res, err := engine.BrowseFiles(db, engine.BrowseQuery{
		Kind: kind, Search: query, Sort: sortKey, SortDesc: sortDesc, Offset: offset, Limit: browseLimit, Filter: filter,
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
		// Local time, minutes: the same local-day reading Statistics uses,
		// precise enough to order a day's work without a column of seconds.
		when := "—"
		if at, ok := engine.RowTime(u); ok {
			when = time.Unix(int64(at), 0).Format("2006-01-02 15:04")
		}
		rows[i] = browseRowData{
			SHA256:       u.SHA256,
			Path:         pathx.DisplayDir(u.FirstSourcePath),
			Filename:     pathx.DisplayName(u.FirstSourcePath),
			Size:         humanBytes(u.Size),
			Type:         mediaKindLabel(u.MimeType),
			Captured:     captured,
			When:         when,
			MarkedSynced: u.MarkedSynced(),
			Attempts:     u.AttemptCount,
			Error:        errMsg,
			Status:       browseStatusLabel(u.Status),
		}
	}

	data := browsePageData{
		Kind:       string(kind),
		Query:      query,
		ShowError:  browseShowErrorKinds[kind],
		ShowStatus: kind == engine.FileListAll,
		WhenLabel:  browseWhenLabels[kind],
		Columns:    buildSortColumns(kind, query, sortKey, sortDesc, filter),
		Rows:       rows,
		Total:      res.Total,

		SourceFolders: sourceFolders,
		Message:       msg,
		ErrorMessage:  errMsg,
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
		data.PrevLink = browseLink(kind, query, sortKey, sortDesc, prevOffset, filter)
	}
	if res.Offset+len(res.Rows) < res.Total {
		data.NextLink = browseLink(kind, query, sortKey, sortDesc, res.Offset+browseLimit, filter)
	}

	// A tab keeps the search and the filter, and starts from page one in
	// its own default order.
	for _, tab := range browseTabOrder {
		data.Tabs = append(data.Tabs, browseTab{Label: tab.Label, Href: browseLink(tab.Kind, query, "", false, 0, filter), Active: tab.Kind == kind})
	}
	if filter.Active() {
		chips, err := browseFilterChips(db, filter)
		if err != nil {
			return "", err
		}
		data.FilterChips = chips
		data.FilterClear = browseLink(kind, query, sortKey, sortDesc, 0, engine.BrowseFilter{})
		for name, vals := range filter.Values() {
			data.FilterFields = append(data.FilterFields, hiddenField{Name: name, Value: vals[0]})
		}
		sort.Slice(data.FilterFields, func(i, j int) bool { return data.FilterFields[i].Name < data.FilterFields[j].Name })
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

// browseFilterChips says in words what a drill-down filter keeps, one
// phrase per condition -- what the bar that opened this list was counting.
func browseFilterChips(db *statedb.DB, f engine.BrowseFilter) ([]string, error) {
	var chips []string
	switch f.Kind {
	case "photo":
		chips = append(chips, "Photos")
	case "video":
		chips = append(chips, "Videos")
	case "other":
		chips = append(chips, "Other files")
	}
	switch f.Ext {
	case "":
	case "(none)":
		chips = append(chips, "No extension")
	default:
		chips = append(chips, "."+f.Ext+" files")
	}
	switch f.Year {
	case "":
	case "unknown":
		chips = append(chips, "No known capture year")
	case "older":
		before, ok, err := engine.OlderThan(db)
		if err != nil {
			return nil, err
		}
		if ok {
			chips = append(chips, fmt.Sprintf("Captured before %d", before))
		} else {
			chips = append(chips, "Captured in an older year")
		}
	default:
		chips = append(chips, "Captured in "+f.Year)
	}
	if f.Outstanding {
		chips = append(chips, "Still to upload")
	}
	if !f.UploadedOn.IsZero() {
		from, _ := engine.ActivityRange(f.UploadedOn, f.Span)
		switch f.Span {
		case engine.ActivityWeekly:
			chips = append(chips, "Uploaded in the week of "+from.Format("Mon 2 Jan 2006"))
		case engine.ActivityMonthly:
			chips = append(chips, "Uploaded in "+from.Format("January 2006"))
		default:
			chips = append(chips, "Uploaded on "+from.Format("Mon 2 Jan 2006"))
		}
	}
	return chips, nil
}
