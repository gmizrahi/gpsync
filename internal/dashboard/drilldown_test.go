package dashboard

import (
	"fmt"
	"html"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/gmizrahi/gpsync/internal/engine"
	"github.com/gmizrahi/gpsync/internal/statedb"
)

// Issue #53, end to end. The engine tests hold every bucket's list to its
// count; this holds the PAGE to it -- a link built for the wrong tab, or a
// chart label mapped to the wrong filter value, would pass those and fail
// here. It renders Statistics, follows every link to Browse through the
// real handler, and compares the list's total with the count printed in
// that same link's tooltip.
func TestStatistics_EveryLinkOpensExactlyTheFilesItCounts(t *testing.T) {
	db := openTestDB(t)
	seedDashboardLibrary(t, db)
	srv := newTestServer(t, db, newFakeController(), Options{})
	client := newTestClient(t)

	page := get(t, client, srv.URL+"/statistics")
	links := regexp.MustCompile(`<a class="[^"]*"? ?href="(/browse\?[^"]+)"[^>]*title="([^"]+)"|<a href="(/browse\?[^"]+)" title="([^"]+)"`).FindAllStringSubmatch(page, -1)
	count := regexp.MustCompile(`([0-9][0-9,]*) file`)
	total := regexp.MustCompile(`&middot; ([0-9]+) file\(s\) &middot;`)

	checked := map[string]bool{}
	for _, m := range links {
		href, title := m[1]+m[3], m[2]+m[4]
		href = html.UnescapeString(href)
		title = html.UnescapeString(title)
		c := count.FindStringSubmatch(title)
		if c == nil {
			t.Errorf("link %s has no count in its tooltip %q", href, title)
			continue
		}
		want, _ := strconv.Atoi(strings.ReplaceAll(c[1], ",", ""))
		list := get(t, client, srv.URL+href)
		got := total.FindStringSubmatch(list)
		if got == nil {
			t.Errorf("%s (%q): the list shows no filter banner", href, title)
			continue
		}
		if n, _ := strconv.Atoi(got[1]); n != want {
			t.Errorf("%s: the tooltip says %q, the list holds %d", href, title, n)
		}
		checked[strings.SplitN(href, "&", 2)[0]] = true
	}
	// Every kind of chart element must have been exercised, or a template
	// change that silently dropped one set of links would still pass.
	for _, want := range []string{"year=", "ext=", "kind=", "st=outstanding", "up="} {
		found := false
		for _, m := range links {
			if strings.Contains(html.UnescapeString(m[1]+m[3]), want) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("no Statistics link carries %q -- that chart lost its links", want)
		}
	}
	if len(links) < 10 {
		t.Errorf("only %d drill-down links on the page; the seeded library should produce many more", len(links))
	}
}

// A filter must survive what you do to the list: switching tab, sorting,
// paging, and the redirect after Re-check -- or a drill-down quietly
// becomes the whole tab.
func TestBrowse_FilterSurvivesTabsSortingAndRowActions(t *testing.T) {
	db := openTestDB(t)
	seedDashboardLibrary(t, db)
	srv := newTestServer(t, db, newFakeController(), Options{})
	client := newTestClient(t)

	page := get(t, client, srv.URL+"/browse?type=all&year=2019")
	// Only the list's own links: the top nav's plain "Browse" link is
	// rightly unfiltered.
	list := page[strings.Index(page, `<div class="browse-tabs">`):]
	for _, tab := range []string{"type=uploaded", "type=pending", "sort=size"} {
		re := regexp.MustCompile(`href="(/browse\?[^"]*` + regexp.QuoteMeta(tab) + `[^"]*)"`)
		m := re.FindStringSubmatch(list)
		if m == nil {
			t.Fatalf("no %s link on the filtered page", tab)
		}
		if !strings.Contains(html.UnescapeString(m[1]), "year=2019") {
			t.Errorf("the %s link drops the filter: %s", tab, m[1])
		}
	}
	if !strings.Contains(page, `<input type="hidden" name="year" value="2019">`) {
		t.Error("the search and row-action forms do not carry the filter")
	}

	sha := regexp.MustCompile(`name="sha256" value="([^"]+)"`).FindStringSubmatch(page)
	if sha == nil {
		t.Fatal("no row on the filtered page")
	}
	resp, err := client.PostForm(srv.URL+"/browse/recheck", map[string][]string{"sha256": {sha[1]}, "tab": {"all"}, "year": {"2019"}})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if loc := resp.Header.Get("Location"); !strings.HasPrefix(loc, "/browse?type=all&") || !strings.Contains(loc, "year=2019") {
		t.Errorf("Re-check returned to %q, want the filtered All list", loc)
	}
}

// The All tab mixes statuses, so it says which each row is.
func TestBrowse_AllTabShowsEachRowsStatus(t *testing.T) {
	db := openTestDB(t)
	seedDashboardLibrary(t, db)
	page, err := renderBrowsePage(db, "all", "", "", false, 0, emptyFilter(), "", false, nil, "", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{`data-label="Status">Uploaded<`, `data-label="Status">Pending<`, `data-label="Status">Failed<`} {
		if !strings.Contains(page, s) {
			t.Errorf("All tab: no row shows %s", s)
		}
	}
}

func seedDashboardLibrary(t *testing.T, db *statedb.DB) {
	t.Helper()
	exts := []string{"jpg", "png", "mp4", "bmp"}
	statuses := []string{"uploaded", "pending", "failed_retryable", "failed_permanent", "marked"}
	n := 0
	for y := 1990; y < 2022; y++ { // 32 years: the chart folds some into Older
		for i := 0; i < 2; i++ {
			n++
			sha := fmt.Sprintf("d%04d", n)
			path := fmt.Sprintf("/lib/%d/f%c.%s", y, 'a'+i, exts[n%len(exts)])
			st := statuses[n%len(statuses)]
			if st == "marked" {
				mustNoErr(t, db.EnsureMarkedSynced(sha, int64(n), "image/jpeg", path, nil))
				continue
			}
			mustNoErr(t, db.EnsurePending(sha, int64(n), "image/jpeg", path, nil))
			switch st {
			case "uploaded":
				mustNoErr(t, db.MarkUploaded(sha, "m"+sha, ""))
			case "failed_retryable":
				mustNoErr(t, db.MarkFailed(sha, false, "throttle", "429"))
			case "failed_permanent":
				mustNoErr(t, db.MarkFailed(sha, true, "unsupported", "nope"))
			}
		}
	}
	mustNoErr(t, db.EnsurePending("undated", 1, "image/jpeg", "/lib/misc/undated.jpg", nil))
}

func get(t *testing.T, client *http.Client, u string) string {
	t.Helper()
	resp, err := client.Get(u)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: %d", u, resp.StatusCode)
	}
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func emptyFilter() engine.BrowseFilter { return engine.BrowseFilter{} }
