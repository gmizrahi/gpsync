package engine

import (
	"net/http"
	"testing"
)

// TestCSRFOriginAllowed_ReadOnlyMethodsAlwaysPass proves GET/HEAD are never
// blocked -- this dashboard's own convention is that only a mutating
// method can change anything, so there's nothing to forge on these.
func TestCSRFOriginAllowed_ReadOnlyMethodsAlwaysPass(t *testing.T) {
	if !CSRFOriginAllowed(http.MethodGet, "https://evil.example", "", "gpsync-tray.local:8080") {
		t.Error("GET with a cross-origin Origin header must still pass")
	}
	if !CSRFOriginAllowed(http.MethodHead, "https://evil.example", "", "gpsync-tray.local:8080") {
		t.Error("HEAD with a cross-origin Origin header must still pass")
	}
}

// TestCSRFOriginAllowed_SameOriginPOST_Passes is the ordinary case: the
// dashboard's own pages submitting their own forms.
func TestCSRFOriginAllowed_SameOriginPOST_Passes(t *testing.T) {
	if !CSRFOriginAllowed(http.MethodPost, "http://192.168.1.50:8080", "", "192.168.1.50:8080") {
		t.Error("a same-origin Origin header must pass")
	}
}

// TestCSRFOriginAllowed_CrossOriginPOST_Rejected is the actual exploit
// shape this fixes: a page on a different origin (anywhere on the same
// LAN, since this dashboard defaults to binding beyond 127.0.0.1)
// auto-submitting a form at the dashboard.
func TestCSRFOriginAllowed_CrossOriginPOST_Rejected(t *testing.T) {
	if CSRFOriginAllowed(http.MethodPost, "http://attacker.example", "", "192.168.1.50:8080") {
		t.Error("a cross-origin Origin header must be rejected")
	}
}

// TestCSRFOriginAllowed_FallsBackToReferer covers a request with no Origin
// header at all but a same-origin Referer -- some older browsers/simple
// navigations omit Origin on a same-origin POST but always send Referer.
func TestCSRFOriginAllowed_FallsBackToReferer(t *testing.T) {
	if !CSRFOriginAllowed(http.MethodPost, "", "http://192.168.1.50:8080/settings", "192.168.1.50:8080") {
		t.Error("a same-origin Referer must pass when Origin is absent")
	}
	if CSRFOriginAllowed(http.MethodPost, "", "http://attacker.example/evil", "192.168.1.50:8080") {
		t.Error("a cross-origin Referer must be rejected when Origin is absent")
	}
}

// TestCSRFOriginAllowed_OriginWinsOverReferer proves Origin, when present,
// is authoritative -- Referer is only ever a fallback for when Origin is
// missing entirely, never blended with it.
func TestCSRFOriginAllowed_OriginWinsOverReferer(t *testing.T) {
	if !CSRFOriginAllowed(http.MethodPost, "http://192.168.1.50:8080", "http://attacker.example/evil", "192.168.1.50:8080") {
		t.Error("a same-origin Origin must pass regardless of what Referer says")
	}
}

// TestCSRFOriginAllowed_BothHeadersAbsent_Allowed is the deliberate,
// documented tradeoff: a request with neither header is let through, since
// a real browser reliably sends at least one on any real submission, and
// this dashboard has no other API consumer to protect by rejecting it.
func TestCSRFOriginAllowed_BothHeadersAbsent_Allowed(t *testing.T) {
	if !CSRFOriginAllowed(http.MethodPost, "", "", "192.168.1.50:8080") {
		t.Error("a request with neither Origin nor Referer must be allowed through")
	}
}

// TestCSRFOriginAllowed_MalformedOrigin_Rejected guards against a
// malformed Origin header being treated as "no header" (and therefore
// allowed) -- it must fail closed, not open.
func TestCSRFOriginAllowed_MalformedOrigin_Rejected(t *testing.T) {
	if CSRFOriginAllowed(http.MethodPost, "://not a valid url", "", "192.168.1.50:8080") {
		t.Error("a malformed Origin header must be rejected, not treated as absent")
	}
}
