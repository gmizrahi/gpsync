package dashboard

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gmizrahi/gpsync/internal/auth"
	"github.com/gmizrahi/gpsync/internal/config"
	"github.com/gmizrahi/gpsync/internal/engine"
	"github.com/gmizrahi/gpsync/internal/statedb"
)

// These lock in bug classes already found once by manual review in this
// exact code (the duplicates-group off-by-one among them, caught before
// this package was portable enough to test directly). Now that it is,
// the next one should fail a test rather than need a human to spot it.

func TestRenderBrowsePage_OffsetZero_PrevLinkAbsent(t *testing.T) {
	db := openTestDB(t)
	mustNoErr(t, db.EnsurePending("h1", 10, "image/jpeg", "/lib/a.jpg", nil))

	page, err := renderBrowsePage(db, "pending", "", "", false, 0, config.ThemeDark, false)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(page, `<a href="/browse?type=pending&offset=`) {
		t.Error("a Prev link was rendered at offset 0 -- there is no prior page to go back to")
	}
	if !strings.Contains(page, `<span class="disabled">&larr; Prev</span>`) {
		t.Error("Prev should render as disabled at offset 0")
	}
}

func TestRenderDuplicatesPage_IndexClampingAndReclaimableBytes(t *testing.T) {
	db := openTestDB(t)
	dir := t.TempDir()
	seedDupGroup(t, db, dir, "hash-a", 100, 2) // reclaimable: 100*(2-1) = 100
	seedDupGroup(t, db, dir, "hash-b", 200, 3) // reclaimable: 200*(3-1) = 400
	cfg := config.Defaults()
	cfg.TrashDir = dir

	// Negative index clamps to 0 (the first group), not a panic or an
	// out-of-range slice access.
	page, err := renderDuplicatesPage(db, cfg, -1, 0, 0, "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(page, "Group 1 of 2") {
		t.Errorf("negative index did not clamp to the first group; page:\n%s", page)
	}
	// Total reclaimable across BOTH groups (100 + 400 = 500 bytes) must
	// show regardless of which single group is currently displayed.
	if !strings.Contains(page, "~500B reclaimable") {
		t.Errorf("reclaimable total wrong -- want 500B summed across both groups; page:\n%s", page)
	}

	// An index past the end clamps to the LAST group, not a panic.
	page, err = renderDuplicatesPage(db, cfg, 999, 0, 0, "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(page, "Group 2 of 2") {
		t.Errorf("out-of-range index did not clamp to the last group; page:\n%s", page)
	}
}

func TestRenderOriginalsPage_NextIndexWrapsToZero(t *testing.T) {
	db := openTestDB(t)
	// A source with no sibling anywhere in the ledger doesn't match
	// AutoResolveObviousOriginals' one auto-resolvable case (an immediate
	// parent sibling), so it stays in needs_review for this test to see.
	mustNoErr(t, db.EnsureNeedsReview("hash-only", 50, "image/jpeg", filepath.Join(t.TempDir(), "originals", "solo.jpg"), nil))

	page, err := renderOriginalsPage(db, config.Defaults(), 0, "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(page, "Item 1 of 1") {
		t.Fatalf("expected exactly one review item; page:\n%s", page)
	}
	// NextIndex (the "Decide Later" target) must wrap to 0, not run past
	// the end of a 1-item list -- ItemTotal=1 so index+1=1 must become 0.
	if !strings.Contains(page, `href="/originals?i=0"`) {
		t.Errorf("Decide Later should wrap NextIndex back to 0 for a single-item list; page:\n%s", page)
	}
}

// TestSharedCSS_StatusWideRow_KeepsWideNarrowSplit proves .status-wide-row
// and .status-live stay a WIDE/narrow split (Activity+Uploading wide,
// Status+Recent activity narrow), per the original, explicit spec: "i
// think ACTIVITY should be on the left and STATUS -> much smaller - on
// the right... i would align the sizes with UPLOADING==ACTIVITY and
// RECENT ACTIVITY==STATUS." A later pass flattened this to an even
// 1fr/1fr split, mistaking a complaint about dead space WITHIN the wide
// box for a complaint about the ratio itself -- it wasn't. The exact
// ratio was later nudged narrower for Status/Recent by a small, precise
// amount ("4 chars... not much, don't exaggerate"), expressed as percent
// + calc(±4ch) rather than the original bare 2.1fr/1fr -- this only
// checks the split stays WIDE/narrow, not the exact numbers, since those
// are expected to keep getting fine-tuned.
func TestSharedCSS_StatusWideRow_KeepsWideNarrowSplit(t *testing.T) {
	i := strings.Index(sharedCSS, ".status-wide-row, .status-live {")
	if i < 0 {
		t.Fatal("no .status-wide-row, .status-live rule found in sharedCSS")
	}
	j := strings.Index(sharedCSS[i:], "}")
	if j < 0 {
		t.Fatal(".status-wide-row, .status-live rule never closes")
	}
	rule := sharedCSS[i : i+j]
	if strings.Contains(rule, "1fr 1fr") {
		t.Errorf(".status-wide-row, .status-live must keep a wide/narrow split, not an even one -- Status must stay 'much smaller' than Activity; rule: %s", rule)
	}
}

// TestSharedCSS_StatusWideRow_AccountsForItsOwnGap proves the explicit
// length tracks subtract this rule's own gap, so the row can't overflow
// its container to the right: the upper-right tiles repeatedly failed to
// line up with the tiles below them.
//
// `fr` tracks are FLEXIBLE: a browser sizing "1fr 1fr" subtracts the gaps
// from the available space first and divides only what's left, so
// .status-columns always fits exactly. Explicit length tracks (percent,
// calc(), px) do NOT -- each takes its stated size and the gap is added
// ON TOP, so tracks summing to exactly 100% plus a 1rem gap demand
// 100% + 1rem and the grid overflows by one gap-width. That overflow is
// exactly how far these rows stuck out past .status-footer and
// #status-bottom's rows, which are plain 100%-wide blocks.
//
// Two earlier attempts at this same report failed because of this:
// nudging the ratio (4ch -> 8ch) could never help, since the overflow
// came from the gap and not the split; and forcing #status-bottom to
// match this ratio only made the bottom row overflow identically, at the
// cost of its own explicitly-required 50/50 split.
func TestSharedCSS_StatusWideRow_AccountsForItsOwnGap(t *testing.T) {
	i := strings.Index(sharedCSS, ".status-wide-row, .status-live {")
	if i < 0 {
		t.Fatal("no .status-wide-row, .status-live rule found in sharedCSS")
	}
	j := strings.Index(sharedCSS[i:], "}")
	if j < 0 {
		t.Fatal(".status-wide-row, .status-live rule never closes")
	}
	rule := sharedCSS[i : i+j]

	// Only explicit-length tracks have this problem; fr tracks handle the
	// gap themselves, so the check only applies while calc()/% is in use.
	if !strings.Contains(rule, "calc(") {
		return
	}
	gap := "gap: 1rem"
	if !strings.Contains(rule, gap) {
		t.Fatalf("expected this rule to still declare %q -- if the gap changed, the track subtraction below must change with it; rule: %s", gap, rule)
	}
	if !strings.Contains(rule, "- 1rem") {
		t.Errorf("explicit calc() tracks must subtract this rule's own %q, or the row overflows its container to the right by one gap-width and stops lining up with .status-footer/#status-bottom; rule: %s", gap, rule)
	}
}

// TestSharedCSS_StatusColumns_StaysEvenSplit proves .status-columns (API
// usage/Uploaded today, Library/Total size, and Statistics' "By
// extension" tables) stays a plain, even 1fr/1fr split, NOT matched to
// .status-wide-row/.status-live's own asymmetric split above it. A
// scoped override forcing that match was tried once and reverted --
// direct, emphatic correction: "you fucked up the tiles at the bottom,
// that's the reason!!!!! they were 50-50 !!" The two rows above
// (Activity+Status, Uploading+Recent activity) are asymmetric on
// purpose; #status-bottom's rows have no such requirement.
func TestSharedCSS_StatusColumns_StaysEvenSplit(t *testing.T) {
	i := strings.Index(sharedCSS, ".status-columns { display: grid;")
	if i < 0 {
		t.Fatal("no .status-columns rule found in sharedCSS")
	}
	j := strings.Index(sharedCSS[i:], "}")
	if j < 0 || !strings.Contains(sharedCSS[i:i+j], "1fr 1fr") {
		t.Error(".status-columns must stay a plain 1fr 1fr split")
	}
	if strings.Contains(sharedCSS, "#status-bottom .status-columns") {
		t.Error("#status-bottom must not override .status-columns' split -- the bottom tiles stay 50/50, not matched to the asymmetric rows above")
	}
}

// TestStatusPageBody_NoLiveFoot proves the .live-foot footer (an
// always-present one-line strip pinned to the bottom of Uploading/
// Recent activity, meant to anchor real content at both top and bottom
// so leftover space in between reads as intentional) was removed, not
// left in place: it added a footer nobody wanted to both tiles.
func TestStatusPageBody_NoLiveFoot(t *testing.T) {
	if strings.Contains(sharedCSS, "live-foot") {
		t.Error("sharedCSS must not reference live-foot -- it was removed, unrequested")
	}
	if strings.Contains(statusPageBody, "live-foot") {
		t.Error("statusPageBody must not reference live-foot -- it was removed, unrequested")
	}
}

// TestPageShell_StatusPage_GetsFlexFillLayout_OtherPagesDoNot proves
// body.status-page (the flex layout that lets Uploading/Recent activity
// grow to fill leftover viewport space -- the original, explicit request:
// "UPLOADING AND RECENT ACTIVITY should take entire rest of height, they
// don't!") is applied ONLY to the Status page. Every other page (Browse,
// Statistics, Settings, ...) needs its own normal, unbounded document
// scroll -- a long table or a tall form has no reason to be squeezed into
// a flex column built for exactly one growing section.
func TestPageShell_StatusPage_GetsFlexFillLayout_OtherPagesDoNot(t *testing.T) {
	db := openTestDB(t)
	statusPage := pageShell(db, "Status", "status", "<p>body</p>", config.ThemeDark, false)
	if !strings.Contains(statusPage, `<body class="status-page">`) {
		t.Errorf("Status page missing body class=\"status-page\"; page:\n%s", statusPage)
	}

	browsePage := pageShell(db, "Browse", "browse", "<p>body</p>", config.ThemeDark, false)
	if strings.Contains(browsePage, `class="status-page"`) {
		t.Error("Browse page must scroll normally -- it should never get body.status-page's flex-fill layout")
	}
}

// TestSharedCSS_StatusPageMain_HasExplicitWidth proves the fix for a real,
// screenshotted regression: once main becomes a flex item of
// body.status-page, its own base rule's "margin: 0 auto" -- which relies
// on BLOCK layout's width:auto meaning "fill available width" -- instead
// suppresses flex's default stretch behavior and shrinks main to its
// content's width, undoing max-width:95vw and centering a much narrower
// page. An explicit width gives flexbox a definite size to honor instead
// of falling back to content-based sizing. Hit and fixed once already;
// this locks it in so a future edit to body.status-page can't drop it
// silently again.
func TestSharedCSS_StatusPageMain_HasExplicitWidth(t *testing.T) {
	i := strings.Index(sharedCSS, "body.status-page main {")
	if i < 0 {
		t.Fatal("no body.status-page main rule found in sharedCSS")
	}
	j := strings.Index(sharedCSS[i:], "}")
	if j < 0 {
		t.Fatal("body.status-page main rule never closes")
	}
	rule := sharedCSS[i : i+j]
	if !strings.Contains(rule, "width: 100%") {
		t.Errorf("body.status-page main must set an explicit width -- otherwise its own margin:0 auto suppresses flex's default stretch and shrinks it to content width; rule: %s", rule)
	}
}

// TestSharedCSS_UploadingFileRow_NeverWraps proves the fix for a real,
// screenshotted bug: a long Uploading row (percent + filename + byte
// counts + speed + ETA + a Skip button) wrapped onto a second line in the
// dense two-column layout. white-space:nowrap+overflow:hidden on the flex
// row itself clips anything too long instead of wrapping it.
func TestSharedCSS_UploadingFileRow_NeverWraps(t *testing.T) {
	i := strings.Index(sharedCSS, "#status-uploading .file-row {")
	if i < 0 {
		t.Fatal("no #status-uploading .file-row rule found in sharedCSS")
	}
	j := strings.Index(sharedCSS[i:], "}")
	if j < 0 {
		t.Fatal("#status-uploading .file-row rule never closes")
	}
	rule := sharedCSS[i : i+j]
	if !strings.Contains(rule, "white-space: nowrap") || !strings.Contains(rule, "overflow: hidden") {
		t.Errorf("#status-uploading .file-row must set white-space:nowrap and overflow:hidden; rule: %s", rule)
	}
}

// TestSharedCSS_UploadingNameCell_HasMinWidthZero proves the actual fix
// for the recurring width-instability bug: a flex (or grid) item's
// default min-width is "auto", which respects its content's intrinsic
// size regardless of overflow:hidden -- overflow only clips what's
// PAINTED, not what the box demands during layout -- so a long enough
// filename could still expand the row, and by extension the whole tile,
// poll to poll. min-width:0 is what actually lets nowrap content be
// clipped without influencing layout; this was the real, correct fix all
// along, arrived at only after a CSS-table detour (display:table-cell on
// a real <button> doesn't reliably participate in table layout across
// browsers, and broke row/cell association outright: "look how ugly it
// looks!!!!!!!") that solved the same symptom the wrong way.
func TestSharedCSS_UploadingNameCell_HasMinWidthZero(t *testing.T) {
	i := strings.Index(sharedCSS, "#status-uploading .file-row > span:last-child {")
	if i < 0 {
		t.Fatal("no #status-uploading .file-row > span:last-child rule found in sharedCSS")
	}
	j := strings.Index(sharedCSS[i:], "}")
	if j < 0 {
		t.Fatal("#status-uploading .file-row > span:last-child rule never closes")
	}
	rule := sharedCSS[i : i+j]
	if !strings.Contains(rule, "min-width: 0") {
		t.Errorf("the name cell must set min-width:0 -- without it, a long nowrap filename can still expand the row/tile despite overflow:hidden; rule: %s", rule)
	}
}

// TestSharedCSS_FileRowX_HasFixedWidth proves the cancel control's CSS
// keeps a fixed width whether it's the real X button or its spacer
// placeholder: a file already at 100% has nothing left to cancel, so the
// control is replaced rather than removed.
// Without a matching fixed width on both, the bar would start at a
// different x-position on a 100%-done row than an in-progress one.
func TestSharedCSS_FileRowX_HasFixedWidth(t *testing.T) {
	i := strings.Index(sharedCSS, "#status-uploading .file-row-x, #status-uploading .file-row-x-spacer {")
	if i < 0 {
		t.Fatal("no #status-uploading .file-row-x, #status-uploading .file-row-x-spacer rule found in sharedCSS")
	}
	j := strings.Index(sharedCSS[i:], "}")
	if j < 0 {
		t.Fatal(".file-row-x, .file-row-x-spacer rule never closes")
	}
	if !strings.Contains(sharedCSS[i:i+j], "width:") {
		t.Error(".file-row-x/.file-row-x-spacer must share a fixed width, or the bar starts at a different x-position depending on whether a row is done")
	}
}

// TestSharedCSS_UploadingLiveList_Is3To2GridSplit proves .live-list stays
// a fixed, fr-based CSS Grid split inside Uploading (never content-
// hugging/content-relative-cap, which is what actually drifted the width
// on earlier attempts) -- 3fr/2fr, a 60/40 split.
//
// align-items:start is ALSO asserted here, but it is NOT what fixes the
// "right column visibly emptier than the left" symptom --
// established after three earlier guesses
// (align-items:start itself, an even row split, a capacity cap at half)
// all failed to change the screenshot. The real cause: a short box and a
// grid-stretched-tall box are pixel-IDENTICAL when both share the same
// plain background with no border, and .live-list's own CONTAINER stays
// tall via body.status-page's flex-fill chain regardless of align-items
// on its grid ITEMS -- so this property has zero visual effect either
// way. Left in place as harmless (it's still technically correct grid
// behavior), not because it does anything; the actual fix is ghostRows
// (see its own doc comment) drawing faint placeholder rows into whatever
// capacity the column measures but doesn't yet have real content for.
func TestSharedCSS_UploadingLiveList_Is3To2GridSplit(t *testing.T) {
	i := strings.Index(sharedCSS, "#status-uploading .live-list {")
	if i < 0 {
		t.Fatal("no #status-uploading .live-list rule found in sharedCSS")
	}
	j := strings.Index(sharedCSS[i:], "}")
	if j < 0 {
		t.Fatal("#status-uploading .live-list rule never closes")
	}
	rule := sharedCSS[i : i+j]
	if !strings.Contains(rule, "display: grid") || !strings.Contains(rule, "grid-template-columns: 3fr 2fr") {
		t.Errorf("#status-uploading .live-list must be display:grid with a 3fr 2fr (60/40) split; rule: %s", rule)
	}
	if !strings.Contains(rule, "align-items: start") {
		t.Errorf("#status-uploading .live-list must set align-items:start (harmless, not load-bearing -- see this test's own doc comment for why); rule: %s", rule)
	}
}

// TestSharedCSS_UploadingFileRow_NotATable proves the CSS-table detour
// (display:table-cell on a real <button>, which doesn't reliably
// participate in table layout across browsers and visibly broke row/cell
// association -- "look how ugly it looks!!!!!!!") isn't reintroduced.
func TestSharedCSS_UploadingFileRow_NotATable(t *testing.T) {
	if strings.Contains(sharedCSS, "display: table") {
		t.Error("sharedCSS must not use display:table/table-cell for Uploading rows -- a real <button> inside one doesn't reliably participate in table layout across browsers; use flex instead")
	}
}

// TestSharedCSS_UploadingFileRow_MatchesRecentActivityFont proves
// Uploading rows inherit .file-row's own base font (ui-monospace,
// 0.85rem) UNCHANGED -- exactly matching Recent activity, not just
// visually approximating it. Two earlier attempts both got overridden
// away from this (first to a sans-serif face, then with a size bump to
// compensate for how much smaller that sans-serif face read) before a
// direct instruction settled it for good: "use it [.file-row's own
// ui-monospace] for UPLOADING with the same font size as RECENT ACTIVITY
// (0.85) -- but for the bar in UPLOADING, keep in sans-serif since
// monospace is ugly" (.bar/.bar-fill carry no text, and the Activity
// card's own Transfer line was never touched by this rule either way, so
// neither needs anything special to stay sans-serif).
func TestSharedCSS_UploadingFileRow_MatchesRecentActivityFont(t *testing.T) {
	i := strings.Index(sharedCSS, "#status-uploading .file-row {")
	if i < 0 {
		t.Fatal("no #status-uploading .file-row rule found in sharedCSS")
	}
	j := strings.Index(sharedCSS[i:], "}")
	if j < 0 {
		t.Fatal("#status-uploading .file-row rule never closes")
	}
	rule := sharedCSS[i : i+j]
	if strings.Contains(rule, "font-family:") || strings.Contains(rule, "font-size:") {
		t.Errorf("#status-uploading .file-row must NOT override font-family/font-size -- it should inherit .file-row's own base rule (ui-monospace, 0.85rem) unchanged, exactly matching Recent activity; rule: %s", rule)
	}
}

// TestSharedCSS_UploadingSplit_IsCapacityBasedNotEven proves the Uploading
// column split fills the left column (active files, then most-recently-
// done ones) up to a DOM-measured capacity before anything spills into
// the right one -- the original, explicit design ("we should always show
// the files being uploaded at the top of the left column... when we
// reach the max amount of files we can show on that left column we start
// pushing files to the right one"). An even split was tried in its place
// at one point and was wrong -- it discarded that explicitly-requested
// behavior to chase a bug (the right column visibly emptier than the
// left) whose actual cause was CSS Grid's own default align-items
// stretching both columns to match height regardless of row count; see
// TestSharedCSS_UploadingLiveList_Is3To2GridSplit's own rule for the fix
// that actually addresses that (align-items:start) without needing to
// give up this capacity-based split.
func TestSharedCSS_UploadingSplit_IsCapacityBasedNotEven(t *testing.T) {
	if !strings.Contains(statusPageBody, "function measureUploadingCapacity()") {
		t.Error("measureUploadingCapacity() must exist -- the Uploading column split fills the left column to a real measured capacity before spilling into the right one")
	}
	if strings.Contains(statusPageBody, "Math.ceil(rows.length / 2)") {
		t.Error("the Uploading column split must not be a plain even half -- that discards the left-first, active-pinned-top design")
	}
}

// TestSharedCSS_QuotaCard_CentersContentAndBottomsTheHint proves API
// usage today / Uploaded today share a class (.quota-card) that centers
// their main content (the bar row, or the stats-grid) within the card's
// full stretched height and, as a direct consequence, puts "Resets at
// midnight..." at the bottom of both cards, with the bar and the request
// count at the same height. Without this, Uploaded today's
// naturally-taller stats-grid content stretched
// the row (via .status-columns' own align-items:stretch), but API usage
// today's own bar row and hint stayed exactly where their un-stretched
// content put them, well above the bottom of the taller box.
func TestSharedCSS_QuotaCard_CentersContentAndBottomsTheHint(t *testing.T) {
	if !strings.Contains(statusPageBody, `class="card quota-card"`) {
		t.Fatal(`API usage today and Uploaded today must both carry class="card quota-card"`)
	}
	i := strings.Index(sharedCSS, ".quota-card {")
	if i < 0 {
		t.Fatal("no .quota-card rule found in sharedCSS")
	}
	j := strings.Index(sharedCSS[i:], "}")
	if j < 0 {
		t.Fatal(".quota-card rule never closes")
	}
	if !strings.Contains(sharedCSS[i:i+j], "display: flex") {
		t.Error(".quota-card must be a flex column so its main content can grow to fill the stretched card height")
	}
	if !strings.Contains(sharedCSS, ".quota-card .row {") || !strings.Contains(sharedCSS, ".quota-card .stats-grid {") {
		t.Error(".quota-card .row and .quota-card .stats-grid must both be styled to grow/center within the card")
	}
}

func TestPageShell_DuplicatesNavLink_ShowsCountAndReclaimable(t *testing.T) {
	db := openTestDB(t)
	dir := t.TempDir()
	seedDupGroup(t, db, dir, "hash-a", 100, 2) // reclaimable: 100*(2-1) = 100B

	page := pageShell(db, "Status", "status", "<p>body</p>", config.ThemeDark, false)
	if !strings.Contains(page, "Duplicates (1, 100B)") {
		t.Errorf("nav link missing/wrong count+reclaimable label; page:\n%s", page)
	}
}

func TestPageShell_NoDuplicates_NavLinkAbsent(t *testing.T) {
	db := openTestDB(t)
	page := pageShell(db, "Status", "status", "<p>body</p>", config.ThemeDark, false)
	if strings.Contains(page, "Duplicates") {
		t.Error("Duplicates nav link should be absent entirely when there are no duplicate groups")
	}
}

// TestRenderStatisticsPage_ByMediaType covers the card that replaced
// "Largest files" and "Extension outcomes", both removed on request. It
// shows each media type as a PAIR of bars -- total and still-pending --
// and sits between Overview and Sync progress, which was specified
// explicitly: "between overview and backup process (perhaps that's even
// a better place)".
func TestRenderStatisticsPage_ByMediaType(t *testing.T) {
	db := openTestDB(t)
	mustNoErr(t, db.EnsurePending("p-done", 1000, "image/jpeg", "/lib/a.jpg", nil))
	mustNoErr(t, db.MarkUploaded("p-done", "m1", ""))
	mustNoErr(t, db.EnsurePending("v-todo", 8000, "video/mp4", "/lib/c.mp4", nil))

	page, err := renderStatisticsPage(db, config.ThemeDark, false, engine.ActivityDaily)
	if err != nil {
		t.Fatal(err)
	}

	// The two removed cards must be gone, not merely emptied.
	for _, gone := range []string{"Largest files", "Extension outcomes"} {
		if strings.Contains(page, gone) {
			t.Errorf("%q card is still on the Statistics page; it was removed on request", gone)
		}
	}

	if !strings.Contains(page, "By media type") {
		t.Fatal("By media type card missing")
	}
	// One segmented bar per category: an uploaded segment and a pending
	// segment, each labelled with "N files in X" above it.
	if !strings.Contains(page, "mt-done") || !strings.Contains(page, "mt-pending") {
		t.Errorf("expected an uploaded and a pending segment; page:\n%s", page)
	}
	// Photos here are 1 of 1 uploaded, videos 1 of 1 pending, so both
	// segments must be at 100%% of their own category -- the per-category
	// scale is the point ("each bar is that category on its own scale").
	if !strings.Contains(page, ">100%<") {
		t.Errorf("segments must be a percentage of their OWN category total; page:\n%s", page)
	}
	if !strings.Contains(page, "files in ") {
		t.Errorf("each segment needs its own \"N files in X\" label above the bar; page:\n%s", page)
	}
	// A category with nothing pending must omit the segment entirely
	// rather than draw it at zero width: "either zero (not existence if
	// there is nothing to upload) or a minimum length". Photos are fully
	// uploaded here, so exactly one pending segment (videos) exists.
	//
	// The card now carries the bar TWICE -- the desktop grid and a
	// phone-only copy, each hidden at the other's width -- so the rule is
	// checked in each copy separately rather than across the whole page.
	gridAt := strings.Index(page, `class="mt-grid"`)
	phoneAt := strings.Index(page, `class="mt-phone"`)
	if gridAt < 0 || phoneAt < gridAt {
		t.Fatalf("expected the desktop grid followed by its phone copy (mt-grid at %d, mt-phone at %d)", gridAt, phoneAt)
	}
	syncAt := strings.Index(page, "<h2>Sync progress</h2>")
	desktop, phone := page[gridAt:phoneAt], page[phoneAt:syncAt]
	for name, part := range map[string]string{"desktop grid": desktop, "phone copy": phone} {
		if got := strings.Count(part, `class="mt-seg mt-pending"`); got != 1 {
			t.Errorf("%s: pending segments rendered = %d, want 1 (photos have none and must be omitted)", name, got)
		}
	}

	// Position: after Overview, before Sync progress.
	overview := strings.Index(page, "<h2>Overview</h2>")
	kinds := strings.Index(page, "<h2>By media type</h2>")
	backup := strings.Index(page, "<h2>Sync progress</h2>")
	if overview < 0 || kinds < 0 || backup < 0 {
		t.Fatalf("missing a card: overview=%d kinds=%d backup=%d", overview, kinds, backup)
	}
	if !(overview < kinds && kinds < backup) {
		t.Errorf("By media type must sit between Overview and Sync progress (got offsets %d, %d, %d)",
			overview, kinds, backup)
	}
}

func TestRenderOriginalsPage_AlreadyUploadedSection(t *testing.T) {
	db := openTestDB(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "originals", "old.jpg")
	mustNoErr(t, db.EnsurePending("hash-up", 250, "image/jpeg", path, nil))
	mustNoErr(t, db.UpsertFileSeen(path, "hash-up", 0, 250))
	mustNoErr(t, db.MarkUploaded("hash-up", "media-123", ""))

	page, err := renderOriginalsPage(db, config.Defaults(), 0, "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(page, `Already uploaded from an "originals" folder`) {
		t.Fatalf("Already-uploaded section missing; page:\n%s", page)
	}
	if !strings.Contains(page, "old.jpg") || !strings.Contains(page, "media-123") {
		t.Errorf("uploaded row/media item id not rendered; page:\n%s", page)
	}
	if !strings.Contains(page, "1 file(s), 250B") {
		t.Errorf("uploaded total not rendered correctly; page:\n%s", page)
	}
}

func TestRenderOriginalsPage_NothingUploadedFromOriginals_SectionAbsent(t *testing.T) {
	db := openTestDB(t)
	// A normal (non-originals-folder) uploaded file must not trigger the
	// section at all.
	mustNoErr(t, db.EnsurePending("hash-normal", 10, "image/jpeg", "/lib/2024/normal.jpg", nil))
	mustNoErr(t, db.UpsertFileSeen("/lib/2024/normal.jpg", "hash-normal", 0, 10))
	mustNoErr(t, db.MarkUploaded("hash-normal", "media-1", ""))

	page, err := renderOriginalsPage(db, config.Defaults(), 0, "")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(page, `Already uploaded from an "originals" folder`) {
		t.Error("section should be absent when nothing uploaded came from an originals folder")
	}
}

// TestRenderBrowsePage_UploadedNeedsReviewIgnored_Render proves the three
// Browse kinds added to close the "no way to see an uploaded/needs_review/
// ignored row in the GUI at all" gap each render their own tab, title, and
// matching row -- and that the Error column, meaningless for these kinds,
// stays hidden (matches the two failure kinds only).
func TestRenderBrowsePage_UploadedNeedsReviewIgnored_Render(t *testing.T) {
	db := openTestDB(t)
	mustNoErr(t, db.EnsurePending("h-up", 10, "image/jpeg", "/lib/uploaded-file.jpg", nil))
	mustNoErr(t, db.MarkUploaded("h-up", "media-1", ""))
	mustNoErr(t, db.EnsureNeedsReview("h-nr", 10, "image/jpeg", "/originals/review-file.jpg", nil))
	mustNoErr(t, db.EnsureNeedsReview("h-ig", 10, "image/jpeg", "/originals/ignored-file.jpg", nil))
	mustNoErr(t, db.ResolveNeedsReview("h-ig", false))

	cases := []struct {
		kind      engine.FileListKind
		wantFile  string
		wantTitle string
	}{
		{engine.FileListUploaded, "uploaded-file.jpg", "<title>Uploaded —"},
		{engine.FileListNeedsReview, "review-file.jpg", "<title>Needs review —"},
		{engine.FileListIgnored, "ignored-file.jpg", "<title>Ignored —"},
	}
	for _, c := range cases {
		page, err := renderBrowsePage(db, c.kind, "", "", false, 0, config.ThemeDark, false)
		if err != nil {
			t.Fatalf("%s: %v", c.kind, err)
		}
		if !strings.Contains(page, c.wantFile) {
			t.Errorf("%s: expected row for %s missing; page:\n%s", c.kind, c.wantFile, page)
		}
		if !strings.Contains(page, c.wantTitle) {
			t.Errorf("%s: expected page title %q not found; page:\n%s", c.kind, c.wantTitle, page)
		}
		if strings.Contains(page, "<th>Error</th>") {
			t.Errorf("%s: Error column should be hidden for this kind; page:\n%s", c.kind, page)
		}
	}
}

// TestStatusPageBody_NoGhostRows proves the ghost/slot-row mechanism is
// gone for good. History, in order: (1) shipped, then caused placeholder
// rows to appear UNDER a fully-empty card's own "nothing happening" text
// ("what are these grey lines i see???"); (2) fixed that, then a SECOND
// bug surfaced -- an unbounded capacity-measurement feedback loop that
// made the whole tile visibly resize under high concurrency ("the right
// column goes beyond the amount of lines it should go and the tile is
// actually resizing now"); (3) removed, root-caused (.live-list had no
// real height ceiling -- see TestSharedCSS_LiveList_HasRealMaxHeight),
// and reintroduced with per-column guards on the theory that the actual
// root cause was now fixed; (4) removed again, for good this time, per a
// direct, blunt aesthetic rejection with no ambiguity left to chase:
// "the ghost shadow is awful." Whatever technical bugs were fixed along
// the way, the feature itself was simply never wanted.
func TestStatusPageBody_NoGhostRows(t *testing.T) {
	for _, want := range []string{"ghostRows(", "recentGhostRows(", "ghostRowOpacity(", "file-row-ghost"} {
		if strings.Contains(statusPageBody, want) {
			t.Errorf("statusPageBody must not reference %q -- ghost rows were rejected outright (\"the ghost shadow is awful\"), not just buggy", want)
		}
	}
	if strings.Contains(sharedCSS, "file-row-ghost") {
		t.Error("sharedCSS must not define .file-row-ghost styles -- ghost rows were rejected outright")
	}
}

// TestSharedCSS_LiveList_HasNoMaxHeight proves .live-list does NOT cap
// its own height, and that the capacity-measurement runaway it used to
// guard against is instead clamped in JS.
//
// A "max-height: 34rem" lived here once, added as a ceiling for the
// feedback loop in measureUploadingCapacity/measureRecentCapacity (both
// read this element's own clientHeight to decide how many rows to render
// back into it). It stopped the loop, but it WAS the long-running "not
// using the entire tile" bug, and the arithmetic is exact: 34rem = 544px,
// a .file-row is ~30px, 544/30 = 18 rows -- precisely the 18 rows a
// screenshot showed before the blank band, inside a card the flex chain
// had stretched well past 544px. Capping the list below its own card
// makes the leftover structurally unfillable no matter how much real
// content exists.
//
// maxMeasuredCapacity replaces it: an absolute clamp on what the measure
// functions may return, which closes the same loop without constraining
// the visible layout at all.
func TestSharedCSS_LiveList_HasNoMaxHeight(t *testing.T) {
	i := strings.Index(sharedCSS, ".live-list { overflow-y: auto;")
	if i < 0 {
		t.Fatal("no .live-list rule found in sharedCSS")
	}
	j := strings.Index(sharedCSS[i:], "}")
	if j < 0 {
		t.Fatal(".live-list rule never closes")
	}
	if strings.Contains(sharedCSS[i:i+j], "max-height") {
		t.Error(".live-list must NOT cap its own height -- that caps the list below its own flex-stretched card and is the \"not using the entire tile\" bug; the measurement runaway is clamped in JS instead, see maxMeasuredCapacity")
	}

	// The JS-side clamp that replaced it must actually be applied, not
	// just declared -- both measure functions have to run through it.
	if !strings.Contains(statusPageBody, "function capacityCeiling(") {
		t.Fatal("statusPageBody must declare capacityCeiling -- the clamp that replaces .live-list's max-height")
	}
	if !strings.Contains(statusPageBody, "Math.min(capacityCeiling(pitch),") {
		t.Error("measureListCapacity must clamp its result through capacityCeiling")
	}
}

// TestStatusPageBody_UploadingRightColumn_CappedAtCapacity proves the
// right column is bounded by the SAME measured capacity as the left one
// (rows.slice(uploadingCapacity, uploadingCapacity*2)), not left
// unbounded (rows.slice(uploadingCapacity) alone, with no upper bound) --
// the unbounded version is what let the tile grow without limit under
// high concurrency, see TestStatusPageBody_NoGhostRows's own
// doc comment.
func TestStatusPageBody_UploadingRightColumn_CappedAtCapacity(t *testing.T) {
	if !strings.Contains(statusPageBody, "rows.slice(uploadingCapacity, uploadingCapacity * 2)") {
		t.Error("the right Uploading column must be capped at rows.slice(uploadingCapacity, uploadingCapacity * 2), not left unbounded")
	}
}

// seedDupGroup writes count real files sharing sha256 (so
// dupGroupsFiltered's on-disk existence check passes) each `size` bytes,
// registers the hash as uploaded (DuplicateGroups joins against uploads),
// and records every path in files_seen.
func seedDupGroup(t *testing.T, db *statedb.DB, dir, sha256 string, size int64, count int) {
	t.Helper()
	for i := 0; i < count; i++ {
		p := filepath.Join(dir, sha256+"-"+string(rune('a'+i))+".jpg")
		if err := os.WriteFile(p, make([]byte, size), 0o644); err != nil {
			t.Fatal(err)
		}
		mustNoErr(t, db.UpsertFileSeen(p, sha256, 0, size))
		if i == 0 {
			mustNoErr(t, db.EnsurePending(sha256, size, "image/jpeg", p, nil))
		}
	}
}

// TestRenderStatisticsPage_ActivityChartsAndSelector covers the
// "Uploaded over time" card: both charts render, the granularity
// selector marks the active view, and the window size follows the
// granularity: files and bytes per day, week or month.
func TestRenderStatisticsPage_ActivityChartsAndSelector(t *testing.T) {
	db := openTestDB(t)
	mustNoErr(t, db.EnsurePending("h-real", 2048, "image/jpeg", "/lib/real.jpg", nil))
	mustNoErr(t, db.MarkUploaded("h-real", "media-real", ""))

	cases := []struct {
		gran     engine.ActivityGranularity
		bars     int
		wantNote string
	}{
		{engine.ActivityDaily, 30, "Last 30 days."},
		{engine.ActivityWeekly, 12, "Last 12 weeks"},
		{engine.ActivityMonthly, 12, "Last 12 months."},
	}
	for _, c := range cases {
		page, err := renderStatisticsPage(db, config.ThemeDark, false, c.gran)
		if err != nil {
			t.Fatalf("%s: %v", c.gran, err)
		}
		if !strings.Contains(page, "Uploaded over time") {
			t.Fatalf("%s: the activity card is missing", c.gran)
		}
		if !strings.Contains(page, c.wantNote) {
			t.Errorf("%s: window note %q missing", c.gran, c.wantNote)
		}
		// Two charts (files and size) over the same window, so every
		// bucket is rendered exactly twice. Counted inside this card only:
		// capture-year bars carry a title too now (the phone layout shows
		// it on tap), so a page-wide count would include them.
		activity := page[strings.Index(page, "<h2>Uploaded over time</h2>"):strings.Index(page, "<h2>By capture year</h2>")]
		if n := strings.Count(activity, `<div class="year-bar" title=`); n != c.bars*2 {
			t.Errorf("%s: %d activity bars, want %d (%d buckets x 2 charts)", c.gran, n, c.bars*2, c.bars)
		}
		// Every bar carries its number, at every granularity -- the daily
		// view (30 bars) used to drop them, which is what prompted "can
		// you put the numbers on top of the bars, just like on the
		// CAPTURE BY YEAR ?". Dense charts shrink the label instead.
		if n := strings.Count(page, `<div class="year-bar-value"`); n < c.bars*2 {
			t.Errorf("%s: %d value labels, want %d (one above every bar in both charts)", c.gran, n, c.bars*2)
		}
		wantDense := c.bars > activityDenseBarLimit
		// Match the class ATTRIBUTE, not the bare name -- sharedCSS ships
		// the .year-chart-dense rule on every page, so a plain substring
		// search matches even when no chart actually uses the class.
		if got := strings.Contains(page, `class="year-chart year-chart-dense"`); got != wantDense {
			t.Errorf("%s: dense chart class = %v, want %v for %d bars", c.gran, got, wantDense, c.bars)
		}
		// The selected view must be the one marked active, so the tabs
		// can never disagree with what is actually plotted.
		wantActive := `<a href="/statistics?activity=` + string(c.gran) + `" class="active">`
		if !strings.Contains(page, wantActive) {
			t.Errorf("%s: selector does not mark this view active", c.gran)
		}
	}
}

// TestRenderStatisticsPage_BackupProgressCard covers the forecast card:
// a real projection when there is measured throughput, and an honest
// "no estimate" when there isn't -- a zero ETA must never render as
// "finishes now".
func TestRenderStatisticsPage_BackupProgressCard(t *testing.T) {
	db := openTestDB(t)
	mustNoErr(t, db.EnsurePending("h-done", 1024, "image/jpeg", "/lib/a.jpg", nil))
	mustNoErr(t, db.MarkUploaded("h-done", "media-a", ""))
	mustNoErr(t, db.EnsurePending("h-todo", 4096, "image/jpeg", "/lib/b.jpg", nil))

	page, err := renderStatisticsPage(db, config.ThemeDark, false, engine.ActivityDaily)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(page, "Sync progress") {
		t.Fatal("the Sync progress card is missing")
	}
	// The just-uploaded file is inside the 7-day window, so there IS a
	// measurable rate and the card must commit to a finish estimate.
	if !strings.Contains(page, "Projected finish") {
		t.Error("expected a projected finish once there is measured throughput")
	}
	if !strings.Contains(page, "Files left") {
		t.Error("expected the remaining file count on the card")
	}
}

// TestRenderStatisticsPage_BackupProgress_NoThroughput_ShowsNoEstimate is
// the other half: with nothing ever uploaded there is no rate to project
// from, and the card has to say so rather than render a fabricated date.
func TestRenderStatisticsPage_BackupProgress_NoThroughput_ShowsNoEstimate(t *testing.T) {
	db := openTestDB(t)
	mustNoErr(t, db.EnsurePending("h-only", 4096, "image/jpeg", "/lib/c.jpg", nil))

	page, err := renderStatisticsPage(db, config.ThemeDark, false, engine.ActivityDaily)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(page, "no measured rate to project from yet") {
		t.Error("expected the card to state plainly that there is no rate yet")
	}
	if strings.Contains(page, "Projected finish &middot;") {
		t.Error("a finish estimate was rendered with no measured throughput to base it on")
	}
}

// cssRule returns the declarations of the first rule whose selector text
// starts with sel. Crude but sufficient: sharedCSS is a hand-written,
// one-rule-per-line stylesheet with no nested blocks outside its media
// queries, and these tests only ever ask about top-level rules.
func cssRule(t *testing.T, sel string) string {
	t.Helper()
	i := strings.Index(sharedCSS, sel)
	if i < 0 {
		t.Fatalf("no %q rule found in sharedCSS", sel)
	}
	j := strings.Index(sharedCSS[i:], "}")
	if j < 0 {
		t.Fatalf("%q rule never closes", sel)
	}
	return sharedCSS[i : i+j]
}

// TestSharedCSS_StatusFooter_HalvesAlignWithTheCardsBelow locks in the
// geometry behind a visible misalignment: the footer strip's left half
// lined up with the card underneath it while its right half sat visibly
// left of the card underneath THAT one.
//
// Both halves are meant to align with .status-columns' two cards below.
// That only works if the footer reproduces those cards' insets exactly,
// which pins three quantities to values it does NOT own:
//
//   - the footer's gap must equal .status-columns' gap, or the two grids
//     put their midpoints in different places;
//   - the cells must carry .card's own horizontal padding, so text starts
//     where the card's text starts;
//   - the tile itself must carry NO horizontal padding, because padding
//     there insets the whole grid -- shifting the right track's start left
//     of the card boundary below it while leaving that cell's text with no
//     inset of its own. Two errors compounding in the same direction,
//     which is why the right half was so much worse than the left.
//
// Asserted against the .card/.status-columns rules rather than against
// literals, so changing either one fails here instead of silently
// un-aligning the footer.
func TestSharedCSS_StatusFooter_HalvesAlignWithTheCardsBelow(t *testing.T) {
	card := cssRule(t, ".card {")
	columns := cssRule(t, ".status-columns {")
	// .status-footer is declared across two rules -- the tile's own box,
	// then the grid it lays its two halves out on.
	footer := cssRule(t, ".status-footer { margin")
	grid := cssRule(t, ".status-footer { display: grid")
	cells := cssRule(t, ".status-footer > div {")

	// .card's padding is "<vertical> <horizontal>"; the horizontal half is
	// what the footer's cells have to match.
	cardPad := ""
	for _, decl := range strings.Split(card, ";") {
		if strings.Contains(decl, "padding:") {
			fields := strings.Fields(strings.SplitN(decl, ":", 2)[1])
			if len(fields) != 2 {
				t.Fatalf("expected .card's padding to be a two-value <vertical> <horizontal>, got %q", decl)
			}
			cardPad = fields[1]
		}
	}
	if cardPad == "" {
		t.Fatal("no padding declaration found on .card")
	}
	if !strings.Contains(cells, " "+cardPad+";") {
		t.Errorf("the footer's cells must use .card's own horizontal padding (%s) so their text starts where a card's text does; cells: %s", cardPad, cells)
	}

	if !strings.Contains(footer, "padding: 0;") {
		t.Errorf("the footer TILE must carry no padding of its own -- padding here insets the whole grid and drags the right half out of alignment with the card below it (the reported bug). Put the padding on the cells instead; tile: %s", footer)
	}

	gap := ""
	for _, decl := range strings.Split(columns, ";") {
		if strings.Contains(decl, "gap:") {
			gap = strings.TrimSpace(strings.SplitN(decl, ":", 2)[1])
		}
	}
	if gap == "" {
		t.Fatal("no gap declaration found on .status-columns")
	}
	if !strings.Contains(grid, "gap: "+gap+";") {
		t.Errorf("the footer's gap must equal .status-columns' gap (%s) or the two grids split at different midpoints and the right halves can't line up; grid: %s", gap, grid)
	}
}

// TestSharedCSS_NoPaddingOnFixedHeightBars guards a shipped bug:
// sharedCSS sets box-sizing: border-box on every element, so padding on a
// declared-height element is subtracted FROM that height rather than
// added to it. .mt-bar had height 1.6rem and padding-bottom 1.15rem for
// the gap between categories, leaving a 7px bar that sliced the
// percentage label in half.
//
// Spacing on a fixed-height bar has to be margin. Checked as a string
// because sharedCSS is a Go raw literal: no linter, compiler or test in
// this project otherwise reads it at all.
func TestSharedCSS_NoPaddingOnFixedHeightBars(t *testing.T) {
	for _, sel := range []string{".mt-bar", ".bar", ".kind-bar-track"} {
		i := strings.Index(sharedCSS, sel+" {")
		if i < 0 {
			continue // rule has since been removed; nothing to guard
		}
		rule := sharedCSS[i : i+strings.Index(sharedCSS[i:], "}")]
		if !strings.Contains(rule, "height:") {
			continue // not a fixed-height bar, the hazard does not apply
		}
		if strings.Contains(rule, "padding") {
			t.Errorf("%s declares both a height and padding; with border-box the padding comes out of the height and collapses the bar. Use margin for spacing. Rule: %s", sel, rule)
		}
	}
	// And the rule that actually spaces the media-type rows must stay a
	// margin, since it targets .mt-bar from outside its own declaration.
	i := strings.Index(sharedCSS, ".mt-name, .mt-bar, .mt-tot-b {")
	if i < 0 {
		t.Skip("media-type row spacing rule has been restructured")
	}
	rule := sharedCSS[i : i+strings.Index(sharedCSS[i:], "}")]
	if strings.Contains(rule, "padding") {
		t.Errorf("the media-type row gap must be margin, not padding -- padding here collapsed .mt-bar to 7px once already. Rule: %s", rule)
	}
}

// TestSplitClientID covers the Settings page's Client ID display: a
// 71-character ID with no break opportunities ran off a phone screen --
// "on settings the client id is too long" -- so the phone layout drops
// Google's constant tail. Only that exact tail may be dropped; anything
// else must come back whole, or the display could hide the part that
// actually distinguishes one client from another.
func TestSplitClientID(t *testing.T) {
	for _, c := range []struct{ in, prefix, suffix string }{
		{"123456789012-abcdefghij.apps.googleusercontent.com", "123456789012-abcdefghij", ".apps.googleusercontent.com"},
		{"some-other-provider-client", "some-other-provider-client", ""},
		{".apps.googleusercontent.com", ".apps.googleusercontent.com", ""}, // nothing unique left: keep it whole
		{"", "", ""},
	} {
		p, s := splitClientID(c.in)
		if p != c.prefix || s != c.suffix {
			t.Errorf("splitClientID(%q) = (%q, %q), want (%q, %q)", c.in, p, s, c.prefix, c.suffix)
		}
		if p+s != c.in {
			t.Errorf("splitClientID(%q): prefix+suffix = %q -- the split must never lose characters", c.in, p+s)
		}
	}
}

// TestSettingsPage_ClientIDKeepsTheFullValue guards the other half: the
// phone layout hides the suffix with CSS only, so the page must still
// carry the complete ID -- as continuous text for desktop, and in the
// tooltip.
func TestSettingsPage_ClientIDKeepsTheFullValue(t *testing.T) {
	db := openTestDB(t)
	id := "123456789012-abcdefghijklmnopqrstuvwxyz01234.apps.googleusercontent.com"
	dir := t.TempDir()
	old := auth.ClientSecretPath
	auth.ClientSecretPath = filepath.Join(dir, "client_secret.json")
	defer func() { auth.ClientSecretPath = old }()
	if err := os.WriteFile(auth.ClientSecretPath, []byte(`{"client_id":"`+id+`","client_secret":"x"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	srv := newTestServer(t, db, newFakeController(), Options{})
	resp, err := newTestClient(t).Get(srv.URL + "/settings")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	page := string(body)

	prefix := strings.TrimSuffix(id, ".apps.googleusercontent.com")
	want := `<span class="cid" title="` + id + `">` + prefix + `<span class="cid-suffix">.apps.googleusercontent.com</span></span>`
	if !strings.Contains(page, want) {
		t.Errorf("Client ID markup missing or changed; want %s", want)
	}
}

// TestPages_UseKBMBGBLabels renders the pages that show byte sizes and
// asserts no binary-prefix label (KiB/MiB/GiB/TiB) appears anywhere:
// sizes read as KB/MB/GB/TB in the tray and the CLI alike.
// units.Bytes has its own tests; this
// catches a page that formats a size some other way.
func TestPages_UseKBMBGBLabels(t *testing.T) {
	db := openTestDB(t)
	mustNoErr(t, db.EnsurePending("h-photo", 3<<20, "image/jpeg", "/lib/a.jpg", nil))
	mustNoErr(t, db.EnsurePending("h-video", 5<<30, "video/mp4", "/lib/b.mp4", nil))
	mustNoErr(t, db.MarkUploaded("h-photo", "m1", ""))

	stats, err := renderStatisticsPage(db, config.ThemeDark, false, engine.ActivityDaily)
	if err != nil {
		t.Fatal(err)
	}
	browse, err := renderBrowsePage(db, "pending", "", "", false, 0, config.ThemeDark, false)
	if err != nil {
		t.Fatal(err)
	}
	for name, page := range map[string]string{"statistics": stats, "browse": browse} {
		for _, bad := range []string{"KiB", "MiB", "GiB", "TiB"} {
			if strings.Contains(page, bad) {
				t.Errorf("%s page contains %q -- sizes are shown as KB/MB/GB/TB", name, bad)
			}
		}
	}
	if !strings.Contains(stats, "GB") {
		t.Error("statistics page shows no GB figure at all for a 5GB library -- the check above would pass vacuously")
	}
}
