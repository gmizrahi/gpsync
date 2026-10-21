package dashboard

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// assertUnframeable fails unless a response forbids every page from
// framing it -- by the standard header and by the legacy one.
func assertUnframeable(t *testing.T, what string, resp *http.Response) {
	t.Helper()
	if got := resp.Header.Get("Content-Security-Policy"); !strings.Contains(got, "frame-ancestors 'none'") {
		t.Errorf("%s (%d): Content-Security-Policy = %q, want frame-ancestors 'none'", what, resp.StatusCode, got)
	}
	if got := resp.Header.Get("X-Frame-Options"); got != "DENY" {
		t.Errorf("%s (%d): X-Frame-Options = %q, want DENY", what, resp.StatusCode, got)
	}
}

// Issue #57. Every kind of response must refuse to be framed, not only the
// pages that render: a framed login form is a phishing risk of its own, and
// a header added per-page would be one forgotten page away from a hole.
// Login off, as installed by default -- the case with no cookie to withhold.
func TestFraming_EveryResponseForbidsIt_LoginOff(t *testing.T) {
	srv := newTestServer(t, openTestDB(t), newFakeController(), Options{})
	client := newTestClient(t)

	for _, path := range []string{"/", "/statistics", "/browse?type=all", "/settings", "/backup",
		"/duplicates", "/originals", "/signin", "/login", "/api/status", "/favicon.ico", "/no-such-page"} {
		resp := mustGet(t, client, srv.URL+path)
		resp.Body.Close()
		assertUnframeable(t, "GET "+path, resp)
	}

	// A cross-site POST, rejected by csrfMiddleware before any handler runs.
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/browse/forget", strings.NewReader(url.Values{"sha256": {"x"}}.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "https://evil.example")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-site POST: status %d, want 403 -- the test is not reaching the rejection it means to check", resp.StatusCode)
	}
	assertUnframeable(t, "rejected cross-site POST", resp)
}

// Login on: the redirect to the login page and the API's 401 are produced
// by the auth middleware, not a page handler, and must carry it too.
func TestFraming_EveryResponseForbidsIt_LoginOn(t *testing.T) {
	ctrl := newFakeController()
	ctrl.cfg = authTestConfig(t)
	srv := newTestServer(t, openTestDB(t), ctrl, Options{})
	client := newTestClient(t)

	resp := mustGet(t, client, srv.URL+"/settings")
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("GET /settings logged out: status %d, want the 303 to login", resp.StatusCode)
	}
	assertUnframeable(t, "redirect to login", resp)

	resp = mustGet(t, client, srv.URL+"/api/status")
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("GET /api/status logged out: status %d, want 401", resp.StatusCode)
	}
	assertUnframeable(t, "API 401", resp)

	resp = mustGet(t, client, srv.URL+"/login")
	resp.Body.Close()
	assertUnframeable(t, "login page", resp)
}
