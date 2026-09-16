package engine

import (
	"net/http"
	"net/url"
)

// CSRFOriginAllowed reports whether a request should be allowed through, as
// a defense against CSRF on gpsync-tray's dashboard. A code review found
// the gap: every state-changing endpoint (/api/toggle,
// /api/retry-now, /api/skip-file, /settings, /backup/create,
// /backup/restore, /duplicates/resolve) was a plain form POST with no CSRF
// protection at all, and this dashboard defaults to binding beyond
// 127.0.0.1. A malicious page visited anywhere on the same LAN could
// auto-submit a hidden form to one of these -- Basic Auth alone doesn't
// stop it, since a browser attaches cached credentials to a cross-site form
// submission targeting the same origin just as readily as a same-site one.
//
// GET/HEAD requests are always allowed (read-only by this dashboard's own
// convention, so there's nothing to forge). For any other method, a
// present Origin header (falling back to Referer if Origin is absent) must
// name the SAME host as reqHost -- a plain <form> submission (the only
// cross-site vector that matters here: no custom headers, no JSON body a
// simple auto-submitting form could send) cannot forge either header,
// since browsers set both from the page that's ACTUALLY submitting the
// request, never from anything script-controlled.
//
// A request missing BOTH headers is allowed through rather than rejected:
// real browsers reliably send at least one on any form/fetch/XHR
// submission, so an absence of both is far more likely to be a non-browser
// client (curl, a future API consumer, gpsync-tray calling itself) than a
// disguised attack, and this dashboard has no other API consumer today to
// justify the false sense of security either interpretation would
// otherwise buy.
func CSRFOriginAllowed(method, originHeader, refererHeader, reqHost string) bool {
	if method == http.MethodGet || method == http.MethodHead {
		return true
	}
	origin := originHeader
	if origin == "" {
		origin = refererHeader
	}
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	return err == nil && u.Host == reqHost
}
