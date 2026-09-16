package dashboard

import (
	"fmt"
	"html/template"
	"strings"
	"time"

	"github.com/gmizrahi/gpsync/internal/engine"
	"github.com/gmizrahi/gpsync/internal/statedb"
)

// kindBarColor maps each fixed engine.Statistics.ByKind label to a CSS
// custom property -- same three, in the same order, every time (see
// engine.BuildStatistics's own doc comment on why ByKind is fixed-order,
// not sorted).
var kindBarColor = map[string]string{
	"Photos": "var(--info)",
	"Videos": "var(--warn)",
	"Other":  "var(--text-dim)",
}

// statKindRow renders one media type as ONE segmented bar spanning that
// category's own total: uploaded, then pending. Specified exactly, after
// a two-bars-per-row version was rejected as "afwful":
//
//	|==========XX%===========|==XX%==|
//
// with each segment labelled above it ("2,300 files in 48GB") and the
// category total on the right.
//
// Each bar is 100% of ITS OWN category, not scaled against the largest
// category -- which is what makes "92% of my photos are done, 13% of my
// videos aren't" readable per row. Cross-category comparison lives in the
// totals column instead.
//
// A category whose segments do not sum to 100% is showing something real:
// failed_permanent and needs_review rows are neither done nor outstanding,
// and they surface as unfilled track rather than being silently absorbed.
type statKindRow struct {
	Label string
	// TotalFilesStr sits at the right of the label row, TotalBytesStr at
	// the right of the bar row -- one two-line totals column, per the
	// layout sketch: "the total are well aligned on the right".
	TotalFilesStr string
	TotalBytesStr string

	// HasDone/HasPending gate each segment. A zero-width segment is
	// omitted entirely rather than rendered at 0% -- "either zero (not
	// existence if there is nothing to upload) or a minimum length".
	HasDone    bool
	HasPending bool

	// *Flex is flex sizing only, for the label row; *Bar adds the colour,
	// for the bar itself. They must carry IDENTICAL flex values or the
	// labels stop sitting over their own segments.
	DoneLabel string
	DonePct   string
	DoneFlex  template.CSS
	DoneBar   template.CSS

	PendingLabel string
	PendingPct   string
	PendingFlex  template.CSS
	PendingBar   template.CSS
}

type statTableRow struct {
	Label    string
	ValueStr string
	BarStyle template.CSS
}

type statYearBar struct {
	Label    string
	CountStr string
	// FillStyle sizes the bar itself (height:X%); ValueStyle positions
	// the count label ABOVE it (bottom:calc(X% + gap)) -- computed from
	// the SAME percentage but kept as two separate style strings since
	// they're two different CSS properties on two different elements. In
	// the first version the value and year labels collided on small bars;
	// the fixed gap in ValueStyle guarantees clearance above the bar
	// regardless of how short it is, instead of the label's position
	// being derived from the (possibly near-zero) bar height alone.
	FillStyle  template.CSS
	ValueStyle template.CSS
}

// statActivityBar is one bar in an "uploaded over time" chart. Same
// geometry as statYearBar (it reuses the same .year-chart CSS), but
// carries a Title for the hover tooltip: the daily view packs 30 bars
// into one row, which is too dense for a readable value label on every
// one, so the exact figures live in title="" and the printed labels are
// suppressed for dense charts (see ShowValues).
type statActivityBar struct {
	Label      string
	ValueStr   string
	Title      string
	FillStyle  template.CSS
	ValueStyle template.CSS
}

type statisticsPageData struct {
	TotalFiles     string
	TotalBytes     string
	AvgBytes       string
	ExtensionCount string

	ByKind []statKindRow

	ByExtCount []statTableRow
	ByExtSize  []statTableRow
	// ExtTableNote is "" when every extension fits in the table, or
	// "showing top 10 of 47" when it doesn't: the table rows are capped,
	// while ExtensionCount above always stays the true total.
	ExtTableNote string

	ByYear            []statYearBar
	YearChartHasOlder bool

	// Forecast* back the "Sync progress" card -- completeness plus a
	// finish date projected from MEASURED throughput, which matters here
	// because real throughput is dominated by throttle backoff rather
	// than bandwidth (see throttleCircuitBreakerSchedule), so any
	// theoretical rate would be systematically optimistic.
	ForecastPercentBytes string
	ForecastBarStyle     template.CSS
	ForecastSynced       string
	ForecastRemaining    string
	ForecastRemainingN   string
	ForecastRate         string
	ForecastETA          string
	ForecastDaysLeft     string
	ForecastKnown        bool
	ForecastDone         bool
	ForecastNeedsReview  string

	// ActivityFiles/ActivityBytes back the two "uploaded over time"
	// charts, which can be viewed daily, weekly or monthly. Two separate
	// charts rather than one
	// with two series: files and bytes have wildly different scales (a
	// handful of videos can outweigh thousands of photos), so plotting
	// them on a shared axis would flatten one of them into nothing --
	// the same reason the by-year chart above plots counts, not bytes.
	ActivityFiles     []statActivityBar
	ActivityBytes     []statActivityBar
	ActivityNote      string
	ActivityDense     bool
	ActivityIsDaily   bool
	ActivityIsWeekly  bool
	ActivityIsMonthly bool

	// ThrottleRungs/ThrottleEventCount back the "Throttle recovery" card --
	// the dashboard view of the same data `gpsync throttle-log --analyze`
	// reports, per the retro's own recommendation to surface it somewhere
	// other than a terminal.
	ThrottleRungs      []statThrottleRow
	ThrottleEventCount int
}

// statThrottleRow is one row of the Throttle recovery table -- rung
// numbers are shown 1-based (RungLabel), matching gpsync throttle-log's
// own "backoff rung N" convention, while engine.RungRecoveryStats itself
// stays 0-based (an index into throttleCircuitBreakerSchedule).
type statThrottleRow struct {
	RungLabel string
	Count     int
	MinStr    string
	MaxStr    string
	AvgStr    string
}

// renderStatisticsPage builds the Statistics page from real ledger data,
// via engine.BuildStatistics. Server-rendered
// like Browse/Settings, not JS-polled like Status -- this describes the
// whole library at a point in time, not something that needs a live
// countdown.
//
// Percentages/bar widths are computed here in Go and marked
// template.CSS (bypassing html/template's CSS-value sanitizer, which is
// conservative enough to potentially mangle a bare var(--x) reference or
// reject it outright) -- safe because every value going into these
// strings is a Go-formatted number or a fixed CSS custom property name,
// never anything derived from user input.
// activityDenseBarLimit is the most bars an activity chart can hold at
// the normal label size. Past it the chart gets .year-chart-dense, which
// shrinks the value labels so they still fit rather than dropping them:
// the numbers are printed above every bar regardless of granularity,
// matching the capture-by-year chart. The daily view is the case that needs
// it -- 30 bars across the card leave roughly 47px each, which a size
// label like "101.6GB" would just overflow at the default 0.68rem.
const activityDenseBarLimit = 14

// activityBar scales one bar against the chart's own maximum. A bucket
// with real content never renders as literally nothing (the 2% floor),
// so a quiet-but-not-empty day stays visible next to a busy one; a
// genuinely EMPTY bucket gets no fill at all, since a zero day and a
// nearly-zero day are different facts and the chart shouldn't blur them.
func activityBar(label, value, title string, v, max float64) statActivityBar {
	pct := 0.0
	if max > 0 && v > 0 {
		pct = v / max * 100
		if pct < 2 {
			pct = 2
		}
	}
	return statActivityBar{
		Label:      label,
		ValueStr:   value,
		Title:      title,
		FillStyle:  template.CSS(fmt.Sprintf("height:%.2f%%", pct)),
		ValueStyle: template.CSS(fmt.Sprintf("bottom:calc(%.2f%% + 0.2rem)", pct)),
	}
}

func renderStatisticsPage(db *statedb.DB, theme string, authEnabled bool, activity engine.ActivityGranularity) (string, error) {
	s, err := engine.BuildStatistics(db)
	if err != nil {
		return "", err
	}

	data := statisticsPageData{
		TotalFiles:     fmt.Sprintf("%d", s.TotalFiles),
		TotalBytes:     humanBytes(s.TotalBytes),
		AvgBytes:       humanBytes(int64(s.AvgBytes)),
		ExtensionCount: fmt.Sprintf("%d", s.ExtensionCount),
	}
	if s.ExtensionCount > len(s.ByExtCount) {
		data.ExtTableNote = fmt.Sprintf("Showing the top %d of %d extensions.", len(s.ByExtCount), s.ExtensionCount)
	}

	for _, k := range s.ByKind {
		if k.Count == 0 {
			continue // nothing of this kind in the library at all
		}
		pct := func(b int64) float64 {
			if k.Bytes == 0 {
				return 0
			}
			return float64(b) / float64(k.Bytes) * 100
		}
		// flex-grow carries the ratio and flex-basis is 0, so the two
		// segments divide the track in proportion. The pending segment's
		// own min-width (see .mt-pending in sharedCSS) then guarantees its
		// label still fits when the share is tiny -- growing the segment
		// past its true percentage is the deliberate trade for keeping
		// "1 file in 2.5MB" legible.
		flex := func(b int64) template.CSS {
			return template.CSS(fmt.Sprintf("flex:%.4f 1 0%%", pct(b)))
		}
		// The pending segment is the SAME hue as its uploaded half, mixed
		// down toward the track, so a row reads as one bar in two states
		// rather than two unrelated colours. color-mix is declared after a
		// plain fallback, so an engine without it still gets the solid
		// category colour instead of no background at all.
		bar := func(b int64, dim bool) template.CSS {
			color := kindBarColor[k.Label]
			if dim {
				return template.CSS(fmt.Sprintf("flex:%.4f 1 0%%;background:%s;background:color-mix(in srgb, %s 42%%, var(--bg-input))",
					pct(b), color, color))
			}
			return template.CSS(fmt.Sprintf("flex:%.4f 1 0%%;background:%s", pct(b), color))
		}
		filesIn := func(n int, b int64) string {
			return fmt.Sprintf("%s files in %s", humanCount(n), humanBytes(b))
		}
		donePct, pendingPct := pairPct(k.UploadedBytes, k.PendingBytes)
		data.ByKind = append(data.ByKind, statKindRow{
			Label:         k.Label,
			TotalFilesStr: fmt.Sprintf("%s files", humanCount(k.Count)),
			TotalBytesStr: humanBytes(k.Bytes),
			HasDone:       k.UploadedCount > 0,
			HasPending:    k.PendingCount > 0,
			DoneLabel:     filesIn(k.UploadedCount, k.UploadedBytes),
			DonePct:       donePct,
			DoneFlex:      flex(k.UploadedBytes),
			DoneBar:       bar(k.UploadedBytes, false),
			PendingLabel:  filesIn(k.PendingCount, k.PendingBytes),
			PendingPct:    pendingPct,
			PendingFlex:   flex(k.PendingBytes),
			PendingBar:    bar(k.PendingBytes, true),
		})
	}

	maxExtCount := 0
	for _, e := range s.ByExtCount {
		if e.Count > maxExtCount {
			maxExtCount = e.Count
		}
	}
	for _, e := range s.ByExtCount {
		pct := 0.0
		if maxExtCount > 0 {
			pct = float64(e.Count) / float64(maxExtCount) * 100
		}
		data.ByExtCount = append(data.ByExtCount, statTableRow{
			Label:    e.Label,
			ValueStr: humanCount(e.Count),
			BarStyle: template.CSS(fmt.Sprintf("width:%.2f%%", pct)),
		})
	}

	maxExtBytes := int64(0)
	for _, e := range s.ByExtSize {
		if e.Bytes > maxExtBytes {
			maxExtBytes = e.Bytes
		}
	}
	for _, e := range s.ByExtSize {
		pct := 0.0
		if maxExtBytes > 0 {
			pct = float64(e.Bytes) / float64(maxExtBytes) * 100
		}
		data.ByExtSize = append(data.ByExtSize, statTableRow{
			Label:    e.Label,
			ValueStr: humanBytes(e.Bytes),
			BarStyle: template.CSS(fmt.Sprintf("width:%.2f%%", pct)),
		})
	}

	// File COUNT, not bytes: plotting bytes made a single video-heavy year
	// dwarf every photo-heavy year
	// next to it (a handful of large videos can outweigh thousands of
	// photos), flattening everything else in the chart by comparison --
	// "how many files" is the more legible story, and matches what
	// CountStr already prints as the visible value label per bar.
	maxYearCount := 0
	for _, y := range s.ByYear {
		if y.Count > maxYearCount {
			maxYearCount = y.Count
		}
	}
	for _, y := range s.ByYear {
		pct := 2.0 // a visible sliver even for a near-empty bucket, rather than nothing at all
		if maxYearCount > 0 {
			pct = float64(y.Count) / float64(maxYearCount) * 100
			if pct < 2 {
				pct = 2
			}
		}
		if y.Label == "Older" {
			data.YearChartHasOlder = true
		}
		data.ByYear = append(data.ByYear, statYearBar{
			Label:      y.Label,
			CountStr:   humanCount(y.Count),
			FillStyle:  template.CSS(fmt.Sprintf("height:%.2f%%", pct)),
			ValueStyle: template.CSS(fmt.Sprintf("bottom:calc(%.2f%% + 0.2rem)", pct)),
		})
	}

	fc, err := engine.BuildForecast(db, time.Now())
	if err != nil {
		return "", err
	}
	data.ForecastDone = fc.Done
	data.ForecastKnown = fc.Known
	data.ForecastPercentBytes = fmt.Sprintf("%.1f%%", fc.PercentBytes)
	data.ForecastBarStyle = template.CSS(fmt.Sprintf("width:%.2f%%", fc.PercentBytes))
	data.ForecastSynced = fmt.Sprintf("%s / %s", humanBytes(fc.SyncedBytes), humanBytes(fc.SyncedBytes+fc.RemainingBytes))
	data.ForecastRemaining = humanBytes(fc.RemainingBytes)
	data.ForecastRemainingN = humanCount(fc.RemainingFiles)
	data.ForecastRate = fmt.Sprintf("%s/day", humanBytes(int64(fc.BytesPerDay)))
	if fc.Known {
		data.ForecastETA = fc.ETA.Format("Mon 2 Jan")
		if fc.DaysRemaining < 1 {
			data.ForecastDaysLeft = "under a day"
		} else {
			data.ForecastDaysLeft = fmt.Sprintf("~%.0f days", fc.DaysRemaining)
		}
	}
	if fc.NeedsReviewFiles > 0 {
		data.ForecastNeedsReview = fmt.Sprintf("%s file(s) (%s) are waiting on a decision in Originals and are not included in the estimate.",
			humanCount(fc.NeedsReviewFiles), humanBytes(fc.NeedsReviewBytes))
	}

	activityBuckets, err := engine.BuildUploadActivity(db, activity, time.Now())
	if err != nil {
		return "", err
	}
	data.ActivityNote = engine.ActivityWindowNote(activity)
	data.ActivityIsDaily = activity == engine.ActivityDaily
	data.ActivityIsWeekly = activity == engine.ActivityWeekly
	data.ActivityIsMonthly = activity == engine.ActivityMonthly
	// Every bar gets its number; a dense chart just gets smaller ones.
	data.ActivityDense = len(activityBuckets) > activityDenseBarLimit
	var maxFiles int
	var maxBytes int64
	for _, b := range activityBuckets {
		if b.Files > maxFiles {
			maxFiles = b.Files
		}
		if b.Bytes > maxBytes {
			maxBytes = b.Bytes
		}
	}
	for _, b := range activityBuckets {
		title := fmt.Sprintf("%s — %s file(s), %s", b.Label, humanCount(b.Files), humanBytes(b.Bytes))
		data.ActivityFiles = append(data.ActivityFiles, activityBar(b.Label, humanCount(b.Files), title, float64(b.Files), float64(maxFiles)))
		data.ActivityBytes = append(data.ActivityBytes, activityBar(b.Label, humanBytes(b.Bytes), title, float64(b.Bytes), float64(maxBytes)))
	}

	if throttleEvents, err := engine.BuildThrottleAnalysis(db); err == nil {
		data.ThrottleEventCount = len(throttleEvents)
		for _, r := range engine.RecoveryStatsByRung(throttleEvents) {
			data.ThrottleRungs = append(data.ThrottleRungs, statThrottleRow{
				RungLabel: fmt.Sprintf("%d", r.Rung+1),
				Count:     r.Count,
				MinStr:    time.Duration(r.MinSeconds * float64(time.Second)).Round(time.Second).String(),
				MaxStr:    time.Duration(r.MaxSeconds * float64(time.Second)).Round(time.Second).String(),
				AvgStr:    time.Duration(r.AvgSeconds * float64(time.Second)).Round(time.Second).String(),
			})
		}
	}

	var buf strings.Builder
	if err := statisticsTmpl.Execute(&buf, data); err != nil {
		return "", err
	}
	return pageShell(db, "Statistics", "statistics", buf.String(), theme, authEnabled), nil
}

var statisticsTmpl = template.Must(template.New("statistics").Parse(`
<div class="card">
  <h2>Overview</h2>
  <div class="stats-grid">
    <div class="stat"><div class="n">{{.TotalFiles}}</div><div class="l">Total files</div></div>
    <div class="stat"><div class="n">{{.TotalBytes}}</div><div class="l">Total size</div></div>
    <div class="stat"><div class="n">{{.AvgBytes}}</div><div class="l">Average size</div></div>
    <div class="stat"><div class="n">{{.ExtensionCount}}</div><div class="l">Extensions seen</div></div>
  </div>
</div>

<div class="card">
  <h2>By media type</h2>
  <div class="mt-grid">
    {{range .ByKind}}<span></span>
    <div class="mt-labels">{{if .HasDone}}<span class="mt-done" style="{{.DoneFlex}}">{{.DoneLabel}}</span>{{end}}{{if .HasPending}}<span class="mt-pending" style="{{.PendingFlex}}">{{.PendingLabel}}</span>{{end}}</div>
    <span class="mt-tot">{{.TotalFilesStr}}</span>
    <span class="mt-name">{{.Label}}</span>
    <div class="mt-bar">{{if .HasDone}}<span class="mt-seg mt-done" style="{{.DoneBar}}">{{.DonePct}}</span>{{end}}{{if .HasPending}}<span class="mt-seg mt-pending" style="{{.PendingBar}}">{{.PendingPct}}</span>{{end}}</div>
    <span class="mt-tot mt-tot-b">{{.TotalBytesStr}}</span>
    {{end}}
  </div>
  <div class="mt-phone">
    {{range .ByKind}}<div class="mtp-row">
      <div class="mtp-head"><span class="mtp-name">{{.Label}}</span><span class="mtp-tot">{{.TotalFilesStr}} &middot; <b>{{.TotalBytesStr}}</b></span></div>
      <div class="mt-bar">{{if .HasDone}}<span class="mt-seg mt-done" style="{{.DoneBar}}">{{.DonePct}}</span>{{end}}{{if .HasPending}}<span class="mt-seg mt-pending" style="{{.PendingBar}}">{{.PendingPct}}</span>{{end}}</div>
      <div class="mtp-legend">{{if .HasDone}}<span>Uploaded: {{.DoneLabel}}</span>{{end}}{{if .HasPending}}<span>Pending: {{.PendingLabel}}</span>{{end}}</div>
    </div>{{end}}
  </div>
</div>

<div class="card">
  <h2>Sync progress</h2>
  {{if .ForecastDone}}
  <p class="prose"><strong>Everything tracked has been uploaded.</strong> {{.ForecastSynced}} complete.</p>
  {{else}}
  <div class="row"><span class="label">Uploaded</span><span>{{.ForecastSynced}} &mdash; {{.ForecastPercentBytes}}</span></div>
  <div class="bar"><span class="bar-fill" style="{{.ForecastBarStyle}}"></span></div>
  <div class="stats-grid" style="margin-top:1.1rem">
    <div class="stat"><div class="n">{{.ForecastRemaining}}</div><div class="l">Remaining</div></div>
    <div class="stat"><div class="n">{{.ForecastRemainingN}}</div><div class="l">Files left</div></div>
    <div class="stat"><div class="n">{{.ForecastRate}}</div><div class="l">Last 7 days</div></div>
    {{if .ForecastKnown}}<div class="stat"><div class="n">{{.ForecastETA}}</div><div class="l">Projected finish &middot; {{.ForecastDaysLeft}}</div></div>
    {{else}}<div class="stat"><div class="n">&mdash;</div><div class="l">Projected finish</div></div>{{end}}
  </div>
  {{if or (not .ForecastKnown) .ForecastNeedsReview}}<div class="hint">
    {{if not .ForecastKnown}}Nothing has been uploaded in the last 7 days, so there's no measured rate to project from yet.{{end}}
    {{if .ForecastNeedsReview}} {{.ForecastNeedsReview}}{{end}}
  </div>{{end}}
  {{end}}
</div>

<div class="card">
  <h2>Uploaded over time</h2>
  <div class="chart-tabs">
    <a href="/statistics?activity=daily"{{if .ActivityIsDaily}} class="active"{{end}}>Daily</a>
    <a href="/statistics?activity=weekly"{{if .ActivityIsWeekly}} class="active"{{end}}>Weekly</a>
    <a href="/statistics?activity=monthly"{{if .ActivityIsMonthly}} class="active"{{end}}>Monthly</a>
  </div>
  <div class="chart-caption">Files</div>
  <div class="year-chart{{if .ActivityDense}} year-chart-dense{{end}}">
    {{range .ActivityFiles}}<div class="year-bar" title="{{.Title}}"><div class="year-bar-track"><div class="year-bar-value" style="{{.ValueStyle}}">{{.ValueStr}}</div><div class="year-bar-fill" style="{{.FillStyle}}"></div></div><div class="year-bar-label">{{.Label}}</div></div>{{end}}
  </div>
  <div class="chart-tap" aria-live="polite"></div>
  <div class="chart-caption">Size</div>
  <div class="year-chart{{if .ActivityDense}} year-chart-dense{{end}}">
    {{range .ActivityBytes}}<div class="year-bar" title="{{.Title}}"><div class="year-bar-track"><div class="year-bar-value" style="{{.ValueStyle}}">{{.ValueStr}}</div><div class="year-bar-fill year-bar-fill-alt" style="{{.FillStyle}}"></div></div><div class="year-bar-label">{{.Label}}</div></div>{{end}}
  </div>
  <div class="chart-tap" aria-live="polite"></div>
  <div class="hint">{{.ActivityNote}} Counts only files gpsync actually uploaded &mdash; files recorded by <code>mark-synced</code> never moved any bytes, so they're excluded. Days are local time, unlike the quota-linked "Uploaded today" card on Status, which follows Google's midnight-Pacific reset. Hover or tap a bar for exact figures.</div>
</div>

<div class="card">
  <h2>By capture year</h2>
  {{if .ByYear}}
  <div class="year-chart">
    {{range .ByYear}}<div class="year-bar" title="{{.Label}}: {{.CountStr}} file(s)"><div class="year-bar-track"><div class="year-bar-value" style="{{.ValueStyle}}">{{.CountStr}}</div><div class="year-bar-fill" style="{{.FillStyle}}"></div></div><div class="year-bar-label">{{.Label}}</div></div>{{end}}
  </div>
  <div class="chart-tap" aria-live="polite"></div>
  <div class="hint">{{if .YearChartHasOlder}}"Older" folds every year before this window together. {{end}}Years come from a folder/filename date when the path has one (more reliable than file metadata), otherwise the capture date on file; "Unknown" is whatever's left with neither.</div>
  {{else}}<div class="hint">Nothing tracked yet.</div>{{end}}
</div>

<div class="status-columns">
  <div class="card">
    <h2>By extension &mdash; count</h2>
    <div class="table-wrap"><table class="stats"><thead><tr><th>Extension</th><th class="num">Share</th><th class="num">Files</th></tr></thead><tbody>
      {{range .ByExtCount}}<tr><td class="name">{{.Label}}</td><td class="num"><span class="mini-bar-track"><span class="mini-bar-fill" style="{{.BarStyle}}"></span></span></td><td class="num">{{.ValueStr}}</td></tr>{{end}}
    </tbody></table></div>
  </div>
  <div class="card">
    <h2>By extension &mdash; total size</h2>
    <div class="table-wrap"><table class="stats"><thead><tr><th>Extension</th><th class="num">Share</th><th class="num">Size</th></tr></thead><tbody>
      {{range .ByExtSize}}<tr><td class="name">{{.Label}}</td><td class="num"><span class="mini-bar-track"><span class="mini-bar-fill" style="{{.BarStyle}}"></span></span></td><td class="num">{{.ValueStr}}</td></tr>{{end}}
    </tbody></table></div>
  </div>
</div>
{{if .ExtTableNote}}<div class="hint" style="margin:-0.5rem 0 1rem;">{{.ExtTableNote}}</div>{{end}}

<div class="card">
  <h2>Throttle recovery</h2>
  {{if .ThrottleRungs}}
  <div class="hint" style="margin-bottom:0.8rem;">{{.ThrottleEventCount}} throttle event(s) recorded. Recovery time (throttle detected &rarr; next upload actually landed), grouped by backoff rung -- see <code>gpsync throttle-log --analyze</code> for the CLI equivalent.</div>
  <div class="table-wrap"><table class="stats"><thead><tr><th>Rung</th><th class="num">Events</th><th class="num">Min</th><th class="num">Max</th><th class="num">Avg</th></tr></thead><tbody>
    {{range .ThrottleRungs}}<tr><td class="name">{{.RungLabel}}</td><td class="num">{{.Count}}</td><td class="num">{{.MinStr}}</td><td class="num">{{.MaxStr}}</td><td class="num">{{.AvgStr}}</td></tr>{{end}}
  </tbody></table></div>
  {{else}}<div class="hint">No throttle event with a known recovery yet.</div>{{end}}
</div>
<script>
// Touch screens have no hover, so a tap puts the bar's own tooltip text
// into the caption under its chart. Desktop keeps hovering; the caption is
// hidden there by CSS.
document.querySelectorAll('.year-chart').forEach(function (chart) {
  var cap = chart.nextElementSibling;
  if (!cap || !cap.classList.contains('chart-tap')) return;
  chart.addEventListener('click', function (e) {
    var bar = e.target.closest('.year-bar');
    if (!bar || !bar.title) return;
    chart.querySelectorAll('.year-bar.tapped').forEach(function (b) { b.classList.remove('tapped'); });
    bar.classList.add('tapped');
    cap.textContent = bar.title;
  });
});
</script>
`))
