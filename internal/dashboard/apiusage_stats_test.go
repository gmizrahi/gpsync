package dashboard

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gmizrahi/gpsync/internal/engine"
)

// #76: real API requests show up on Statistics -- by token, by what was
// served (a thumbnail made, then from the cache, then unchanged for the
// client), and refusals by reason with zero rows left out. The card exists
// only once a token does, and nothing records which files were served.
func TestStatistics_APIUsageCard(t *testing.T) {
	db := openTestDB(t)
	dir := t.TempDir()
	photo, video := fmt.Sprintf("%064x", 1), fmt.Sprintf("%064x", 2)
	addMediaFile(t, db, photo, filepath.Join(dir, "p.jpg"), testJPEG(t, 600, 400), time.Now())
	addMediaFile(t, db, video, filepath.Join(dir, "v.mp4"), []byte("pretend video"), time.Now())
	srv := newTestServer(t, db, newFakeController(), Options{})
	c := newTestClient(t)

	stats := func() string {
		t.Helper()
		resp := mustGet(t, c, srv.URL+"/statistics")
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return string(b)
	}
	if page := stats(); strings.Contains(page, `id="api-usage"`) {
		t.Fatal("the API usage card shows with no API tokens")
	}

	tok := newToken(t, db, "trip-app")
	thumb := srv.URL + "/api/v1/media/" + photo + "/thumb"
	apiGet(t, c, srv.URL+"/api/v1/media", tok)
	resp, _ := apiGet(t, c, thumb, tok) // made
	apiGet(t, c, thumb, tok)            // from the cache
	apiGet(t, c, thumb, tok, "If-None-Match", resp.Header.Get("ETag"))
	apiGet(t, c, srv.URL+"/api/v1/media/"+video+"/thumb", tok) // 415
	apiGet(t, c, srv.URL+"/api/v1/media", "gps_"+strings.Repeat("A", 43))
	apiGet(t, c, srv.URL+"/api/v1/media", "gps_"+strings.Repeat("B", 43))

	page := stats()
	card := page[strings.Index(page, `id="api-usage"`):]
	card = card[:strings.Index(card, "Throttle recovery")]
	for _, want := range []string{
		"trip-app", "Lists", "Thumbnails", "from the cache", "made for the request", "unchanged, the program had it",
		"Bad, expired or revoked token (401)", "No thumbnail for this format (415)",
		`<div class="n">7</div><div class="l">Requests</div>`, `<div class="n">3</div><div class="l">Refused</div>`,
	} {
		if !strings.Contains(card, want) {
			t.Errorf("card lacks %q", want)
		}
	}
	for _, absent := range []string{"(403)", "(429)", "(404)", photo, video, "p.jpg"} {
		if strings.Contains(card, absent) {
			t.Errorf("card shows %q", absent)
		}
	}
	// The card sits just before Throttle recovery.
	if a, b := strings.Index(page, `id="api-usage"`), strings.Index(page, "<h2>Throttle recovery</h2>"); a < 0 || a > b || strings.Contains(page[a:b], "<h2>By extension") {
		t.Error("the card is not directly before Throttle recovery")
	}

	rows, err := db.APIUsageSince("2000-01-01")
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if strings.Contains(r.TokenName+r.Endpoint+r.Result, photo) {
			t.Errorf("a usage row names a file: %+v", r)
		}
	}
}

// The period tabs keep the upload chart's granularity, and its tabs keep
// the period.
func TestStatistics_APIUsagePeriodLinks(t *testing.T) {
	db := openTestDB(t)
	newToken(t, db, "trip-app")
	srv := newTestServer(t, db, newFakeController(), Options{})
	resp := mustGet(t, newTestClient(t), srv.URL+"/statistics?activity=weekly&api=7")
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	page := string(b)
	for _, want := range []string{
		`href="/statistics?activity=weekly&amp;api=7#api-usage" class="active">7 days`,
		`href="/statistics?activity=weekly&amp;api=90#api-usage">90 days`,
		`href="/statistics?activity=monthly&amp;api=7"`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("page lacks %s", want)
		}
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status %d", resp.StatusCode)
	}
}

// The per-command tokens gpsync's own CLI mints have random names and last
// one command, so they're counted as one "gpsync CLI" row, not one each.
func TestAPIUsage_CLITokensCountAsOne(t *testing.T) {
	db := openTestDB(t)
	srv := newTestServer(t, db, newFakeController(), Options{})
	c := newTestClient(t)
	for range 2 {
		tok, cleanup, err := engine.MintEphemeralAPIToken(db, []string{engine.ScopeRead}, time.Minute, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		apiGet(t, c, srv.URL+"/api/v1/token", tok)
		cleanup()
	}
	newToken(t, db, "trip-app")
	resp := mustGet(t, c, srv.URL+"/statistics") // saves the counts
	resp.Body.Close()
	rows, err := db.APIUsageSince("2000-01-01")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].TokenName != engine.APIUsageCLIName || rows[0].Requests != 2 {
		t.Errorf("usage rows = %+v; want one %q row of 2", rows, engine.APIUsageCLIName)
	}
}

// serveQuit answers before quitting by flushing; the usage wrapper must
// pass that through, or the quit route's 202 could be lost in the shutdown.
func TestUsageWriter_PassesFlushThrough(t *testing.T) {
	rec := httptest.NewRecorder()
	serveQuit(&usageWriter{ResponseWriter: rec}, func() {})
	if rec.Code != http.StatusAccepted || !rec.Flushed {
		t.Errorf("status %d, flushed %v; want 202, flushed", rec.Code, rec.Flushed)
	}
}
