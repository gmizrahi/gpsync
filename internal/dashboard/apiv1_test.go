package dashboard

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gmizrahi/gpsync/internal/engine"
	"github.com/gmizrahi/gpsync/internal/statedb"
)

func newToken(t *testing.T, db *statedb.DB, name string, scopes ...string) string {
	t.Helper()
	if len(scopes) == 0 {
		scopes = []string{engine.ScopeRead}
	}
	tok, err := engine.CreateAPIToken(db, name, scopes, time.Hour, true, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

func apiDo(t *testing.T, client *http.Client, method, url, token string) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequest(method, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

// Issue #67's acceptance: a valid token reaches an API route; a missing,
// wrong, expired or revoked one gets 401 with no detail -- the same 401 for
// all of them, so the response says nothing about which.
func TestAPIv1_ValidTokenGetsIn_AndEveryBadOneGetsTheSame401(t *testing.T) {
	db := openTestDB(t)
	srv := newTestServer(t, db, newFakeController(), Options{})
	c := newTestClient(t)
	good := newToken(t, db, "app")

	resp, body := apiDo(t, c, http.MethodGet, srv.URL+"/api/v1/token", good)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("valid token: %d %s", resp.StatusCode, body)
	}
	var info struct {
		Name      string   `json:"name"`
		Scopes    []string `json:"scopes"`
		ExpiresAt *string  `json:"expires_at"`
	}
	if err := json.Unmarshal([]byte(body), &info); err != nil || info.Name != "app" || len(info.Scopes) != 1 || info.Scopes[0] != "read" || info.ExpiresAt == nil {
		t.Errorf("GET /api/v1/token = %s (%v); want app, [read] and an expiry", body, err)
	}

	expired, err := engine.CreateAPIToken(db, "old", []string{engine.ScopeRead}, time.Minute, true, time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	revoked := newToken(t, db, "gone")
	if err := engine.RevokeAPIToken(db, "gone"); err != nil {
		t.Fatal(err)
	}
	for why, tok := range map[string]string{"missing": "", "wrong": "gps_" + strings.Repeat("A", 43), "malformed": "not-a-token", "expired": expired, "revoked": revoked} {
		resp, body := apiDo(t, c, http.MethodGet, srv.URL+"/api/v1/token", tok)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s token: status %d, want 401", why, resp.StatusCode)
		}
		if strings.TrimSpace(body) != "unauthorized" || !strings.HasPrefix(resp.Header.Get("WWW-Authenticate"), "Bearer") {
			t.Errorf("%s token: body %q, WWW-Authenticate %q; want a bare 'unauthorized' and a Bearer challenge", why, body, resp.Header.Get("WWW-Authenticate"))
		}
	}
}

// A dashboard session doesn't authenticate the API, and a token doesn't
// authenticate the dashboard -- with dashboard login ON, where both exist.
func TestAPIv1_CookieAndTokenEachOnlyOpenTheirOwnHalf(t *testing.T) {
	db := openTestDB(t)
	ctrl := newFakeController()
	ctrl.cfg = authTestConfig(t)
	srv := newTestServer(t, db, ctrl, Options{})
	browser := newTestClient(t)
	resp := mustPostForm(t, browser, srv.URL+"/login", srv.URL, "username=alice&password=correct-horse&next=%2Fsettings")
	resp.Body.Close()
	if r := mustGet(t, browser, srv.URL+"/settings"); r.StatusCode != http.StatusOK {
		t.Fatalf("precondition: the logged-in browser can't reach /settings (%d)", r.StatusCode)
	}

	if r, _ := apiDo(t, browser, http.MethodGet, srv.URL+"/api/v1/token", ""); r.StatusCode != http.StatusUnauthorized {
		t.Errorf("a dashboard session reached /api/v1: status %d, want 401", r.StatusCode)
	}

	tok := newToken(t, db, "app")
	program := newTestClient(t)
	if r, _ := apiDo(t, program, http.MethodGet, srv.URL+"/settings", tok); r.StatusCode != http.StatusSeeOther {
		t.Errorf("a bearer token opened the dashboard page: status %d, want the 303 to login", r.StatusCode)
	}
	if r, _ := apiDo(t, program, http.MethodGet, srv.URL+"/api/status", tok); r.StatusCode != http.StatusUnauthorized {
		t.Errorf("a bearer token reached the dashboard's own /api/status: status %d, want 401", r.StatusCode)
	}
}

// With dashboard login OFF -- the default -- the dashboard is open, but the
// API still demands a token.
func TestAPIv1_RequiresATokenEvenWithDashboardLoginOff(t *testing.T) {
	srv := newTestServer(t, openTestDB(t), newFakeController(), Options{})
	if r, _ := apiDo(t, newTestClient(t), http.MethodGet, srv.URL+"/api/v1/token", ""); r.StatusCode != http.StatusUnauthorized {
		t.Errorf("no token, login off: status %d, want 401", r.StatusCode)
	}
}

// The API is for programs on this machine, even when the dashboard listens
// on every interface. httptest.Server always dials from loopback, so this
// drives the handler with a foreign RemoteAddr directly.
func TestAPIv1_RefusesNonLoopbackEvenWithAValidToken(t *testing.T) {
	db := openTestDB(t)
	h := Handler(db, newFakeController(), Options{})
	tok := newToken(t, db, "app")
	for addr, want := range map[string]int{"192.0.2.10:5555": http.StatusForbidden, "[2001:db8::1]:5555": http.StatusForbidden,
		"127.0.0.1:5555": http.StatusOK, "[::1]:5555": http.StatusOK} {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/token", nil)
		req.RemoteAddr = addr
		req.Header.Set("Authorization", "Bearer "+tok)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != want {
			t.Errorf("from %s: status %d, want %d", addr, rec.Code, want)
		}
	}
}

func TestAPIv1_ScopeIsEnforcedPerRoute(t *testing.T) {
	db := openTestDB(t)
	quit := make(chan struct{})
	srv := newTestServer(t, db, newFakeController(), Options{QuitGracefully: func() { close(quit) }})
	c := newTestClient(t)

	resp, _ := apiDo(t, c, http.MethodPost, srv.URL+"/api/v1/control/quit", newToken(t, db, "reader"))
	if resp.StatusCode != http.StatusForbidden || !strings.Contains(resp.Header.Get("WWW-Authenticate"), "insufficient_scope") {
		t.Errorf("a read token on control/quit: status %d, %q; want 403 insufficient_scope", resp.StatusCode, resp.Header.Get("WWW-Authenticate"))
	}
	select {
	case <-quit:
		t.Fatal("a read token made the tray quit")
	default:
	}
	if resp, _ := apiDo(t, c, http.MethodPost, srv.URL+"/api/v1/control/quit", newToken(t, db, "ctl", engine.ScopeControl)); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("a control token on control/quit: status %d, want 202", resp.StatusCode)
	}
	select {
	case <-quit:
	case <-time.After(5 * time.Second):
		t.Fatal("the quit hook was never called")
	}
}

func TestAPIv1_RotationOverHTTP(t *testing.T) {
	db := openTestDB(t)
	srv := newTestServer(t, db, newFakeController(), Options{})
	c := newTestClient(t)
	old := newToken(t, db, "app")

	resp, body := apiDo(t, c, http.MethodPost, srv.URL+"/api/v1/token/rotate", old)
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("rotate: %d, Cache-Control %q, %s", resp.StatusCode, resp.Header.Get("Cache-Control"), body)
	}
	var got struct {
		Token     string  `json:"token"`
		Name      string  `json:"name"`
		ExpiresAt *string `json:"expires_at"`
	}
	if err := json.Unmarshal([]byte(body), &got); err != nil || !strings.HasPrefix(got.Token, engine.APITokenPrefix) || got.Name != "app" || got.ExpiresAt == nil {
		t.Fatalf("rotate response %s (%v); want a new gps_ token for app with an expiry", body, err)
	}
	for which, tok := range map[string]string{"new": got.Token, "old, inside its grace period": old} {
		if r, _ := apiDo(t, c, http.MethodGet, srv.URL+"/api/v1/token", tok); r.StatusCode != http.StatusOK {
			t.Errorf("the %s token: status %d, want 200", which, r.StatusCode)
		}
	}
}

// Failures are throttled -- but every local client shares the loopback
// address, so a client that keeps failing must not lock out one with a
// valid token.
func TestAPIv1_FailuresAreThrottledWithoutLockingOutValidTokens(t *testing.T) {
	db := openTestDB(t)
	srv := newTestServer(t, db, newFakeController(), Options{})
	c := newTestClient(t)
	good := newToken(t, db, "app")
	bad := "gps_" + strings.Repeat("B", 43)
	for i := 0; i < maxLoginFailures; i++ {
		if r, _ := apiDo(t, c, http.MethodGet, srv.URL+"/api/v1/token", bad); r.StatusCode != http.StatusUnauthorized {
			t.Fatalf("failure %d: status %d, want 401", i+1, r.StatusCode)
		}
	}
	if r, _ := apiDo(t, c, http.MethodGet, srv.URL+"/api/v1/token", bad); r.StatusCode != http.StatusTooManyRequests {
		t.Errorf("a failing request after %d failures: status %d, want 429", maxLoginFailures, r.StatusCode)
	}
	if r, _ := apiDo(t, c, http.MethodGet, srv.URL+"/api/v1/token", good); r.StatusCode != http.StatusOK {
		t.Errorf("a VALID token during the lockout: status %d, want 200 -- one bad local client locked out the others", r.StatusCode)
	}
}

// Issue #67: a token never appears in a log. Checked by capturing the log
// across everything that logs about tokens.
func TestAPIv1_TokensNeverReachTheLog(t *testing.T) {
	var buf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(prev) })

	db := openTestDB(t)
	srv := newTestServer(t, db, newFakeController(), Options{})
	c := newTestClient(t)
	tok := newToken(t, db, "app")
	_, body := apiDo(t, c, http.MethodPost, srv.URL+"/api/v1/token/rotate", tok)
	for i := 0; i < maxLoginFailures+1; i++ {
		apiDo(t, c, http.MethodGet, srv.URL+"/api/v1/token", tok[:len(tok)-1]+"Z")
	}
	resp := mustPostForm(t, c, srv.URL+"/settings/api-tokens", srv.URL, "token_name=viaui&token_expires=default")
	page, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	logged := buf.String()
	if !strings.Contains(logged, "api auth:") || !strings.Contains(logged, `"viaui" created`) || !strings.Contains(logged, "rotated") {
		t.Fatalf("expected failure, create and rotate events in the log; got:\n%s", logged)
	}
	if strings.Contains(logged, engine.APITokenPrefix) {
		t.Errorf("a token reached the log:\n%s", logged)
	}
	if !strings.Contains(body, engine.APITokenPrefix) || !strings.Contains(string(page), engine.APITokenPrefix) {
		t.Error("precondition: the rotate response and the create page should each have carried a token")
	}
}
