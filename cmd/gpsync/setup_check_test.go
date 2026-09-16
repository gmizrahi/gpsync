package main

import (
	"net/http"
	"strings"
	"testing"
)

// TestClassifyAPIProbe covers the distinction the check exists to make: a
// project with the Photos Library API switched off looks, at a glance, like
// the 403 that gpsync's append-only credential is SUPPOSED to receive. One
// is a setup problem to report, the other is a healthy installation, and
// reporting either as the other sends someone chasing the wrong thing.
func TestClassifyAPIProbe(t *testing.T) {
	const disabledBody = `{"error":{"code":403,` +
		`"message":"Photos Library API has not been used in project 123456 before or it is disabled.",` +
		`"status":"PERMISSION_DENIED"}}`
	const appendOnlyBody = `{"error":{"code":403,` +
		`"message":"Request had insufficient authentication scopes.","status":"PERMISSION_DENIED"}}`
	// The same condition reported the other way round: status carries the
	// reason rather than the message. This shape used to fall through to
	// "unexpected response".
	const serviceDisabledStatus = `{"error":{"code":403,"message":"forbidden","status":"SERVICE_DISABLED"}}`

	for _, tc := range []struct {
		name       string
		status     int
		body       string
		wantOK     bool
		wantDetail string
		wantRemedy string
	}{
		{
			name: "token accepted outright", status: http.StatusOK, body: `{"mediaItems":[]}`,
			wantOK: true, wantDetail: "token accepted",
		},
		{
			name: "API not enabled for the project", status: http.StatusForbidden, body: disabledBody,
			wantOK: false, wantDetail: "not enabled", wantRemedy: enableAPIURL,
		},
		{
			name: "API not enabled, reported via status", status: http.StatusForbidden, body: serviceDisabledStatus,
			wantOK: false, wantDetail: "not enabled", wantRemedy: enableAPIURL,
		},
		{
			name: "append-only scope refused a read, which is expected", status: http.StatusForbidden, body: appendOnlyBody,
			wantOK: true, wantDetail: "expected",
		},
		{
			name: "stored token rejected", status: http.StatusUnauthorized, body: `{"error":{"code":401}}`,
			wantOK: false, wantDetail: "rejected the stored token", wantRemedy: "sign in again",
		},
		{
			name: "anything else is reported, not guessed at", status: http.StatusInternalServerError, body: `{}`,
			wantOK: false, wantDetail: "unexpected response: HTTP 500",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyAPIProbe(tc.status, []byte(tc.body))
			if got.ok != tc.wantOK {
				t.Errorf("ok = %v, want %v (detail: %q)", got.ok, tc.wantOK, got.detail)
			}
			if !strings.Contains(got.detail, tc.wantDetail) {
				t.Errorf("detail = %q, want it to contain %q", got.detail, tc.wantDetail)
			}
			if tc.wantRemedy != "" && !strings.Contains(got.remedy, tc.wantRemedy) {
				t.Errorf("remedy = %q, want it to contain %q", got.remedy, tc.wantRemedy)
			}
			// A failing check without a remedy leaves the reader stuck.
			if !got.ok && got.remedy == "" {
				t.Error("a failed check must say what to do about it")
			}
		})
	}
}

// TestClassifyAPIProbe_MalformedBodyStillClassifies proves a non-JSON body
// (an HTML error page from a proxy, say) cannot panic or be misread as
// success -- it falls through to the reported-but-not-guessed-at branch.
func TestClassifyAPIProbe_MalformedBodyStillClassifies(t *testing.T) {
	got := classifyAPIProbe(http.StatusBadGateway, []byte("<html>502 Bad Gateway</html>"))
	if got.ok {
		t.Errorf("a 502 must not read as success: %+v", got)
	}
	if !strings.Contains(got.detail, "502") {
		t.Errorf("detail = %q, want it to name the status", got.detail)
	}
}
