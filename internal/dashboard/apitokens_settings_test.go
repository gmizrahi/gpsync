package dashboard

import (
	"io"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/gmizrahi/gpsync/internal/engine"
)

// A token is shown once: on the page the create request returns, marked
// no-store, and never again -- not on the next load of Settings, not in a
// redirect URL.
func TestSettings_APITokenIsShownExactlyOnce(t *testing.T) {
	db := openTestDB(t)
	srv := newTestServer(t, db, newFakeController(), Options{})
	c := newTestClient(t)

	resp := mustPostForm(t, c, srv.URL+"/settings/api-tokens", srv.URL, "token_name=photos-app&token_expires=30d")
	page, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("create: status %d -- it must answer with the page itself, never a redirect that could carry the token", resp.StatusCode)
	}
	if resp.Header.Get("Cache-Control") != "no-store" {
		t.Errorf("the page holding the token has Cache-Control %q, want no-store", resp.Header.Get("Cache-Control"))
	}
	tok := regexp.MustCompile(`gps_[A-Za-z0-9_-]{43}`).FindString(string(page))
	if tok == "" {
		t.Fatal("the create response does not show the new token")
	}
	if _, err := engine.AuthenticateAPIToken(db, tok, timeNow()); err != nil {
		t.Fatalf("the shown token does not work: %v", err)
	}

	again := mustGet(t, c, srv.URL+"/settings")
	later, _ := io.ReadAll(again.Body)
	again.Body.Close()
	if strings.Contains(string(later), tok) || strings.Contains(string(later), engine.APITokenPrefix) {
		t.Error("Settings shows the token again on a later load")
	}
	if !strings.Contains(string(later), "photos-app") {
		t.Error("the token is not listed by name")
	}
}

func TestSettings_RevokeStopsTheTokenAtOnce(t *testing.T) {
	db := openTestDB(t)
	srv := newTestServer(t, db, newFakeController(), Options{})
	c := newTestClient(t)
	tok := newToken(t, db, "app")

	resp := mustPostForm(t, c, srv.URL+"/settings/api-tokens/revoke", srv.URL, "token_name=app")
	resp.Body.Close()
	if loc := resp.Header.Get("Location"); resp.StatusCode != http.StatusSeeOther || !strings.Contains(loc, "token_revoked=app") {
		t.Fatalf("revoke: %d -> %q", resp.StatusCode, loc)
	}
	if r, _ := apiDo(t, c, http.MethodGet, srv.URL+"/api/v1/token", tok); r.StatusCode != http.StatusUnauthorized {
		t.Errorf("a token revoked from Settings still works: status %d", r.StatusCode)
	}
}

// Creating a token is a state change behind the dashboard's CSRF check: a
// page on another site must not be able to mint one.
func TestSettings_CrossSiteTokenCreateIsRefused(t *testing.T) {
	db := openTestDB(t)
	srv := newTestServer(t, db, newFakeController(), Options{})
	resp := mustPostForm(t, newTestClient(t), srv.URL+"/settings/api-tokens", "https://evil.example", "token_name=evil")
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("cross-site create: status %d, want 403", resp.StatusCode)
	}
	if list, _ := db.ListAPITokens(); len(list) != 0 {
		t.Errorf("a cross-site request created %d token(s)", len(list))
	}
}

func timeNow() time.Time { return time.Now() }
