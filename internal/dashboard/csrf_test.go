package dashboard

import (
	"net/http"
	"testing"
)

// Auth is deliberately OFF in these tests -- CSRF is its own, independent
// layer (per engine.CSRFOriginAllowed's own doc comment: Basic Auth/a
// session cookie doesn't stop a cross-site form submission, since the
// browser attaches it regardless of which site the form lives on), so
// these prove csrfMiddleware rejects a forged cross-origin POST even with
// nothing else standing in the way.

func TestCSRF_CrossOriginPOST_Rejected(t *testing.T) {
	db := openTestDB(t)
	ctrl := newFakeController()
	srv := newTestServer(t, db, ctrl, Options{})
	client := newTestClient(t)

	resp := mustPostForm(t, client, srv.URL+"/api/toggle", "http://evil.example", "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("status = %d, want 403 for a cross-origin POST", resp.StatusCode)
	}
	if ctrl.started || ctrl.stopped {
		t.Error("the rejected request still reached the handler and toggled the engine")
	}
}

func TestCSRF_SameOriginPOST_Allowed(t *testing.T) {
	db := openTestDB(t)
	ctrl := newFakeController()
	srv := newTestServer(t, db, ctrl, Options{})
	client := newTestClient(t)

	resp := mustPostForm(t, client, srv.URL+"/api/toggle", srv.URL, "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("status = %d, want 204 for a same-origin POST", resp.StatusCode)
	}
	if !ctrl.started {
		t.Error("same-origin /api/toggle should have started the (not-running) engine")
	}
}

func TestCSRF_NoOriginOrReferer_Allowed(t *testing.T) {
	// A missing Origin/Referer is treated as a non-browser client (curl,
	// gpsync itself probing the dashboard), not a disguised attack -- see
	// CSRFOriginAllowed's own doc comment for why.
	db := openTestDB(t)
	ctrl := newFakeController()
	srv := newTestServer(t, db, ctrl, Options{})
	client := newTestClient(t)

	resp := mustPostForm(t, client, srv.URL+"/api/toggle", "", "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("status = %d, want 204 when neither Origin nor Referer is present", resp.StatusCode)
	}
}

func TestCSRF_GETIsAlwaysAllowed(t *testing.T) {
	db := openTestDB(t)
	ctrl := newFakeController()
	srv := newTestServer(t, db, ctrl, Options{})
	client := newTestClient(t)

	resp := mustGet(t, client, srv.URL+"/api/status")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200 -- GET is read-only, CSRF never applies", resp.StatusCode)
	}
}
