package dashboard

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// templateSources is every file a page's markup can come from: the Go
// templates in this package and the static assets.
func templateSources(t *testing.T) map[string]string {
	t.Helper()
	out := map[string]string{}
	goFiles, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	assets, err := filepath.Glob("assets/*.html")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range append(goFiles, assets...) {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		out[f] = string(b)
	}
	return out
}

var classAttr = regexp.MustCompile(`class="([^"]*)"`)

// Issue #51. Browse's action cell carried class="row-actions", and no CSS
// rule for .row-actions existed, so Re-check and Forget fell back to two
// full-size primary buttons stacked in separate block forms -- every row
// twice as tall as its content. Nothing caught it, because a class with no
// rule is not an error anywhere: the markup is valid, the CSS is valid, and
// the page renders, just wrongly.
//
// So every class a template uses must be styled by dashboard.css. The one
// unstyled class this found besides it -- "prose", unused since the first
// commit -- was dropped rather than excused.
func TestTemplates_EveryClassHasACSSRule(t *testing.T) {
	for file, src := range templateSources(t) {
		for _, m := range classAttr.FindAllStringSubmatch(src, -1) {
			v := m[1]
			// Built at runtime -- `class="' + cls + '"` in script, or a
			// {{template}} expression -- so there is no literal to check.
			if strings.ContainsAny(v, "'+{") {
				continue
			}
			for _, cls := range strings.Fields(v) {
				rule := regexp.MustCompile(`\.` + regexp.QuoteMeta(cls) + `(?:[^\w-]|$)`)
				if !rule.MatchString(sharedCSS) {
					t.Errorf("%s: class %q has no rule in dashboard.css", file, cls)
				}
			}
		}
	}
}

// Issue #51. Skip, Restore, Go Back and Decide Later were written
// <a href><button type="button">…</button></a>. A button inside a link is
// invalid HTML -- interactive content may not nest -- and keyboard users
// tab through two focus stops per control. A link that should look like a
// button takes the button's class instead.
func TestTemplates_NoButtonInsideALink(t *testing.T) {
	nested := regexp.MustCompile(`<a\b[^>]*>\s*<button`)
	for file, src := range templateSources(t) {
		if loc := nested.FindStringIndex(src); loc != nil {
			t.Errorf("%s: <button> nested inside <a>: %q", file, src[loc[0]:min(loc[1]+40, len(src))])
		}
	}
}

// Issue #51. Browse showed two identical folder dropdowns, one per folder
// action, which ended at different points because each took the width its
// own button left -- and nothing tied either one to its action. Both
// actions now share one picker; the second button reaches /mark-synced
// through formaction/formmethod, still by GET.
func TestBrowseToolbar_OneFolderPickerForBothActions(t *testing.T) {
	db := openTestDB(t)
	page, err := renderBrowsePage(db, "pending", "", "", false, 0, "", false, []string{`C:\Photos`, `D:\More`}, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(page, `<select name="folder"`); n != 1 {
		t.Errorf("Browse renders %d folder pickers, want exactly 1", n)
	}
	if !strings.Contains(page, `formaction="/mark-synced" formmethod="get"`) {
		t.Error("Mark as already synced must still go to /mark-synced by GET -- that page is its typed confirmation step")
	}
}
