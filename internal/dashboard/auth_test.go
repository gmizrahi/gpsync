package dashboard

import (
	"net/http"
	"strings"
	"testing"

	"golang.org/x/crypto/bcrypt"

	"github.com/gmizrahi/gpsync/internal/config"
)

// authTestConfig returns a config with dashboard auth enabled for user
// "alice"/"correct-horse" -- shared by every test in this file.
func authTestConfig(t *testing.T) config.Config {
	t.Helper()
	hash, err := bcrypt.GenerateFromPassword([]byte("correct-horse"), bcrypt.DefaultCost)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Defaults()
	cfg.DashboardAuthEnabled = true
	cfg.DashboardAuthUser = "alice"
	cfg.DashboardAuthPassHash = string(hash)
	return cfg
}

func TestSessionAuth_UnauthenticatedPageGET_RedirectsToLogin(t *testing.T) {
	db := openTestDB(t)
	ctrl := newFakeController()
	ctrl.cfg = authTestConfig(t)
	srv := newTestServer(t, db, ctrl, Options{})
	client := newTestClient(t)

	resp := mustGet(t, client, srv.URL+"/settings")
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusSeeOther)
	}
	loc := resp.Header.Get("Location")
	if !strings.HasPrefix(loc, "/login") {
		t.Errorf("Location = %q, want it to start with /login", loc)
	}
	if !strings.Contains(loc, "next=") {
		t.Errorf("Location = %q, want a next= param so login returns to /settings", loc)
	}
}

func TestSessionAuth_UnauthenticatedAPIRequest_Gets401NotRedirect(t *testing.T) {
	db := openTestDB(t)
	ctrl := newFakeController()
	ctrl.cfg = authTestConfig(t)
	srv := newTestServer(t, db, ctrl, Options{})
	client := newTestClient(t)

	resp := mustGet(t, client, srv.URL+"/api/status")
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401 -- /api/* must never redirect a fetch() into an HTML login page", resp.StatusCode)
	}
}

func TestSessionAuth_Disabled_EveryRoutePassesThrough(t *testing.T) {
	db := openTestDB(t)
	ctrl := newFakeController() // config.Defaults() -- DashboardAuthEnabled is false
	srv := newTestServer(t, db, ctrl, Options{})
	client := newTestClient(t)

	resp := mustGet(t, client, srv.URL+"/settings")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200 when auth is disabled", resp.StatusCode)
	}
}

func TestLogin_CorrectCredentials_IssuesSessionAndRedirects(t *testing.T) {
	db := openTestDB(t)
	ctrl := newFakeController()
	ctrl.cfg = authTestConfig(t)
	srv := newTestServer(t, db, ctrl, Options{})
	client := newTestClient(t)

	resp := mustPostForm(t, client, srv.URL+"/login", srv.URL, "username=alice&password=correct-horse&next=%2Fsettings")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("login status = %d, want 303", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "/settings" {
		t.Errorf("Location = %q, want /settings (the submitted next)", loc)
	}

	// The session cookie should now let a page load through cleanly.
	resp2 := mustGet(t, client, srv.URL+"/settings")
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Errorf("status after login = %d, want 200", resp2.StatusCode)
	}
}

func TestLogin_WrongPassword_StaysOnLoginWithError(t *testing.T) {
	db := openTestDB(t)
	ctrl := newFakeController()
	ctrl.cfg = authTestConfig(t)
	srv := newTestServer(t, db, ctrl, Options{})
	client := newTestClient(t)

	resp := mustPostForm(t, client, srv.URL+"/login", srv.URL, "username=alice&password=wrong")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (the login page re-rendered with an error, not a redirect)", resp.StatusCode)
	}

	// And no session was granted -- a subsequent page load must still redirect.
	resp2 := mustGet(t, client, srv.URL+"/settings")
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusSeeOther {
		t.Errorf("status after failed login = %d, want 303 (still unauthenticated)", resp2.StatusCode)
	}
}

func TestLogin_OpenRedirect_NextIsSanitized(t *testing.T) {
	db := openTestDB(t)
	ctrl := newFakeController()
	ctrl.cfg = authTestConfig(t)
	srv := newTestServer(t, db, ctrl, Options{})
	client := newTestClient(t)

	resp := mustPostForm(t, client, srv.URL+"/login", srv.URL, "username=alice&password=correct-horse&next=https%3A%2F%2Fevil.example")
	defer resp.Body.Close()
	if loc := resp.Header.Get("Location"); loc != "/" {
		t.Errorf("Location = %q, want / (an absolute-URL next must be rejected)", loc)
	}
}

func TestLogout_ClearsSession(t *testing.T) {
	db := openTestDB(t)
	ctrl := newFakeController()
	ctrl.cfg = authTestConfig(t)
	srv := newTestServer(t, db, ctrl, Options{})
	client := newTestClient(t)

	mustPostForm(t, client, srv.URL+"/login", srv.URL, "username=alice&password=correct-horse").Body.Close()
	// Confirm we're actually in.
	if resp := mustGet(t, client, srv.URL+"/settings"); resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		t.Fatalf("setup: not authenticated after login")
	} else {
		resp.Body.Close()
	}

	logoutResp := mustGet(t, client, srv.URL+"/logout")
	defer logoutResp.Body.Close()
	if logoutResp.StatusCode != http.StatusSeeOther {
		t.Errorf("logout status = %d, want 303", logoutResp.StatusCode)
	}

	resp := mustGet(t, client, srv.URL+"/settings")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Errorf("status after logout = %d, want 303 (session should be gone)", resp.StatusCode)
	}
}
