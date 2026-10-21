package dashboard

import "net/http"

// frameDenyMiddleware forbids every other page from framing the dashboard
// (issue #57). Without it a page could load the dashboard in an invisible
// <iframe> and line a decoy button up over a real one -- Forget, Restore,
// Sync this folder now -- so the user's click lands on the dashboard
// (clickjacking).
//
// Neither existing defence stops that. csrfMiddleware accepts a POST whose
// Origin is the dashboard's own, and a click inside the frame IS a click on
// the dashboard's own page. The SameSite=Lax session cookie is withheld from
// a cross-site frame, but login is off by default, so there is often no
// cookie to withhold -- and a page on another port of the same host is the
// same site anyway.
//
// It wraps the whole handler, outermost, so every response carries the
// headers: pages, the login page (a framed login form is its own phishing
// risk), redirects, 401s and errors. frame-ancestors is the standard control
// and X-Frame-Options covers browsers that predate it. The dashboard never
// frames itself, so 'none' / DENY costs nothing.
func frameDenyMiddleware(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", "frame-ancestors 'none'")
		w.Header().Set("X-Frame-Options", "DENY")
		h.ServeHTTP(w, r)
	})
}
