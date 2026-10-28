package engine

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gmizrahi/gpsync/internal/config"
	"github.com/gmizrahi/gpsync/internal/statedb"
)

// Requests are summed in memory and reach the ledger on the first request
// a minute after the last save, or on Flush -- not one write per request.
func TestAPIUsageCounter_CountsInMemoryAndSavesAboutOnceAMinute(t *testing.T) {
	db := openTestDB(t)
	clock := time.Date(2026, 5, 10, 12, 0, 0, 0, time.Local)
	c := NewAPIUsageCounter()
	c.now = func() time.Time { return clock }
	c.lastFlush = clock

	for range 3 {
		c.Record(db, "app", APIEndpointThumb, APIResultMade, 1000)
	}
	c.Record(db, "", APIEndpointOther, "401", 13)
	if rows, _ := db.APIUsageSince("2000-01-01"); len(rows) != 0 {
		t.Fatalf("written before a minute passed: %+v", rows)
	}
	clock = clock.Add(61 * time.Second)
	c.Record(db, "app", APIEndpointThumb, APIResultMade, 1000) // this one saves
	rows, err := db.APIUsageSince("2000-01-01")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]statedb.APIUsageRow{}
	for _, r := range rows {
		got[r.TokenName+"/"+r.Endpoint+"/"+r.Result] = r
	}
	if r := got["app/thumb/made"]; r.Requests != 4 || r.Bytes != 4000 {
		t.Errorf("app/thumb/made = %+v; want 4 requests, 4000 bytes", r)
	}
	if r := got["/other/401"]; r.Requests != 1 {
		t.Errorf("/other/401 = %+v; want 1", r)
	}

	// A second save adds to the same day's rows rather than replacing them.
	c.Record(db, "app", APIEndpointThumb, APIResultMade, 1000)
	if err := c.Flush(db); err != nil {
		t.Fatal(err)
	}
	rows, _ = db.APIUsageSince("2000-01-01")
	for _, r := range rows {
		if r.Endpoint == APIEndpointThumb && r.Requests != 5 {
			t.Errorf("after a second save: %+v; want 5", r)
		}
	}
}

// Days older than APIUsageKeepDays are dropped when counts are saved.
func TestAPIUsageCounter_KeepsNinetyDays(t *testing.T) {
	db := openTestDB(t)
	now := time.Date(2026, 5, 10, 12, 0, 0, 0, time.Local)
	old := now.AddDate(0, 0, -APIUsageKeepDays-1).Format(time.DateOnly)
	kept := now.AddDate(0, 0, -APIUsageKeepDays).Format(time.DateOnly)
	if err := db.AddAPIUsage([]statedb.APIUsageRow{
		{Day: old, TokenName: "app", Endpoint: APIEndpointList, Result: APIResultOK, Requests: 1},
		{Day: kept, TokenName: "app", Endpoint: APIEndpointList, Result: APIResultOK, Requests: 2},
	}); err != nil {
		t.Fatal(err)
	}
	c := NewAPIUsageCounter()
	c.now = func() time.Time { return now }
	if err := c.Flush(db); err != nil {
		t.Fatal(err)
	}
	rows, _ := db.APIUsageSince("2000-01-01")
	if len(rows) != 1 || rows[0].Requests != 2 {
		t.Errorf("after pruning: %+v; want only the day %s", rows, kept)
	}
}

// The report: every stored token (used or not) and the CLI's own, what was
// served with zero rows left out, refusals only where there were some, the
// period's window, and the cache counting only its own files.
func TestBuildAPIUsageReport(t *testing.T) {
	db := openTestDB(t)
	now := time.Date(2026, 5, 10, 12, 0, 0, 0, time.Local)
	cfg := config.Defaults()
	if _, hasTokens, err := BuildAPIUsageReport(db, cfg, 30, now); err != nil || hasTokens {
		t.Fatalf("no tokens: hasTokens %v, %v; want false", hasTokens, err)
	}
	if _, err := CreateAPIToken(db, "app", []string{ScopeRead}, 30*24*time.Hour, true, now); err != nil {
		t.Fatal(err)
	}
	if _, err := CreateAPIToken(db, "idle", []string{ScopeRead}, 0, false, now); err != nil {
		t.Fatal(err)
	}
	today := now.Format(time.DateOnly)
	tooOld := now.AddDate(0, 0, -7).Format(time.DateOnly) // outside a 7-day window
	if err := db.AddAPIUsage([]statedb.APIUsageRow{
		{Day: today, TokenName: "app", Endpoint: APIEndpointList, Result: APIResultOK, Requests: 2, Bytes: 500},
		{Day: today, TokenName: "app", Endpoint: APIEndpointThumb, Result: APIResultCache, Requests: 5, Bytes: 5000},
		{Day: today, TokenName: "app", Endpoint: APIEndpointThumb, Result: APIResultMade, Requests: 1, Bytes: 1000},
		{Day: today, TokenName: "app", Endpoint: APIEndpointToken, Result: APIResultOK, Requests: 1, Bytes: 80},
		{Day: today, TokenName: APIUsageCLIName, Endpoint: APIEndpointControl, Result: APIResultOK, Requests: 1},
		{Day: today, TokenName: "", Endpoint: APIEndpointOther, Result: "401", Requests: 3, Bytes: 40},
		{Day: today, TokenName: "app", Endpoint: APIEndpointThumb, Result: "415", Requests: 1, Bytes: 30},
		{Day: tooOld, TokenName: "app", Endpoint: APIEndpointOriginal, Result: APIResultOK, Requests: 9, Bytes: 9e6},
	}); err != nil {
		t.Fatal(err)
	}
	thumbs := filepath.Join(statedb.StateDir, "thumbs", "256")
	if err := os.MkdirAll(thumbs, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, size := range map[string]int{strings.Repeat("a", 64) + ".jpg": 700, "not-ours.jpg": 5000} {
		if err := os.WriteFile(filepath.Join(thumbs, name), make([]byte, size), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	r, hasTokens, err := BuildAPIUsageReport(db, cfg, 7, now)
	if err != nil || !hasTokens {
		t.Fatalf("%v, %v", hasTokens, err)
	}
	if r.Requests != 14 || r.Refused != 4 || r.Bytes != 6580 {
		t.Errorf("totals: %d requests, %d refused, %d bytes; want 14, 4, 6580 (refusals' bytes not served)", r.Requests, r.Refused, r.Bytes)
	}
	var tokens []string
	for _, tu := range r.Tokens {
		tokens = append(tokens, tu.Name+"="+itoa(tu.Requests))
	}
	if got := strings.Join(tokens, " "); got != "app=10 idle=0 gpsync CLI=1" {
		t.Errorf("tokens: %s", got)
	}
	var served []string
	for _, s := range r.Served {
		served = append(served, s.Label+"="+itoa(s.Requests))
	}
	want := "Lists=2 Thumbnails=6 from the cache=5 made for the request=1 Token checks and rotations=1 Tray control=1"
	if got := strings.Join(served, " "); got != want {
		t.Errorf("served:\n got %s\nwant %s", got, want)
	}
	if len(r.Refusals) != 2 || r.Refusals[0].Status != "401" || r.Refusals[0].Requests != 3 || r.Refusals[1].Status != "415" {
		t.Errorf("refusals: %+v; want 401 x3 then 415 x1, nothing else", r.Refusals)
	}
	if r.CacheBytes != 700 || r.CacheMax != ThumbCacheMaxBytes {
		t.Errorf("cache: %d of %d; want only gpsync's own 700 bytes", r.CacheBytes, r.CacheMax)
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }
