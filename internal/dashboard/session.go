package dashboard

import (
	"fmt"
	"html/template"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gmizrahi/gpsync/internal/config"
	"github.com/gmizrahi/gpsync/internal/engine"
	"github.com/gmizrahi/gpsync/internal/statedb"
)

// rememberMeSeconds/shortSessionSeconds are the two lifetimes a login can
// have -- "remember me" checked gets a cookie that survives the browser
// closing (Max-Age set) and a matching long server-side expiry; unchecked
// gets a true session cookie (no Max-Age at all, so Go's http.Cookie
// zero-values it and the browser drops it when it closes) but STILL gets a
// bounded server-side expiry as defense in depth, since some browsers
// restore "session" cookies across a relaunch anyway.
const (
	rememberMeSeconds   = 30 * 24 * 3600
	shortSessionSeconds = 12 * 3600
)

// sessionCookieName is the dashboard's login-session cookie -- HttpOnly, no
// Secure flag (this dashboard is plain HTTP, and Secure would make the
// browser silently withhold the cookie entirely), SameSite=Lax (enough to
// stop it riding along on a cross-site navigation, and csrfMiddleware
// covers the rest for actual mutating requests).
const sessionCookieName = "gpsync_session"

// sessionAuthMiddleware replaces the dashboard's old HTTP Basic Auth
// prompt with a real login page. /login and /favicon.ico are always
// reachable -- the
// login page itself, and its browser-requested favicon, obviously can't
// require a session to render. Everything else needs a valid
// dashboard_sessions row named by the cookie; a page GET/HEAD without one
// is redirected to /login?next=<original path>, while a mutating request
// (a stale-tab form POST, or the JS polling hitting /api/*) just gets 401
// rather than being redirected into losing its POST body.
func sessionAuthMiddleware(h http.Handler, db *statedb.DB, cfg config.Config) http.Handler {
	if !cfg.DashboardAuthEnabled {
		return h
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/login" || r.URL.Path == "/favicon.ico" {
			h.ServeHTTP(w, r)
			return
		}
		valid := false
		if cookie, err := r.Cookie(sessionCookieName); err == nil {
			valid, _ = db.ValidateSession(cookie.Value)
		}
		if !valid {
			// /api/* is only ever hit by this page's own polling fetch()
			// calls, never a full-page navigation -- a redirect there would
			// be followed transparently and hand the JS an HTML login page
			// where it expects JSON. A plain 401 is the correct signal for
			// those; only a real page GET should be bounced to the login
			// form.
			isAPI := strings.HasPrefix(r.URL.Path, "/api/")
			if !isAPI && (r.Method == http.MethodGet || r.Method == http.MethodHead) {
				next := r.URL.Path
				if r.URL.RawQuery != "" {
					next += "?" + r.URL.RawQuery
				}
				http.Redirect(w, r, "/login?next="+url.QueryEscape(next), http.StatusSeeOther)
			} else {
				http.Error(w, "authentication required", http.StatusUnauthorized)
			}
			return
		}
		h.ServeHTTP(w, r)
	})
}

// loginPageHTML renders the login form -- reuses sharedCSS's tokens (see
// .login-wrap) rather than pageShell, since there's no nav/session to show
// before a login succeeds.
func loginPageHTML(next, errMsg, theme string) string {
	themeAttr := ""
	if theme == config.ThemeLight || theme == config.ThemeDark {
		themeAttr = fmt.Sprintf(` data-theme="%s"`, theme)
	}
	banner := ""
	if errMsg != "" {
		banner = fmt.Sprintf(`<div class="banner banner-err">%s</div>`, template.HTMLEscapeString(errMsg))
	}
	return fmt.Sprintf(`<!DOCTYPE html>
<html%s>
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Sign in — %s</title>
<link rel="icon" href="/favicon.ico">
<style>%s</style>
</head>
<body>
<div class="login-wrap">
  <h1>%s</h1>
  %s
  <div class="card">
    <form method="post" action="/login">
      <input type="hidden" name="next" value="%s">
      <div class="field">
        <label for="login-user">Username</label>
        <input type="text" id="login-user" name="username" autocomplete="username" autofocus required>
      </div>
      <div class="field">
        <label for="login-pass">Password</label>
        <input type="password" id="login-pass" name="password" autocomplete="current-password" required>
      </div>
      <div class="remember">
        <input type="checkbox" id="login-remember" name="remember">
        <label for="login-remember">Remember me</label>
      </div>
      <button type="submit">Log in</button>
    </form>
  </div>
</div>
</body>
</html>`, themeAttr, appName, sharedCSS, appName, banner, template.HTMLEscapeString(next))
}

// handleLogin renders the login form (GET) and processes it (POST). On
// success, issues a session cookie and redirects to next (sanitized by
// engine.LoginRedirectTarget against an open redirect) rather than always
// bouncing to Status, so a session that expired mid-browse lands back
// where it was.
func handleLogin(w http.ResponseWriter, r *http.Request, db *statedb.DB, cfg config.Config, attempts *loginAttemptTracker) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if r.Method != http.MethodPost {
		next := engine.LoginRedirectTarget(r.URL.Query().Get("next"))
		w.Write([]byte(loginPageHTML(next, "", cfg.Theme)))
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	next := engine.LoginRedirectTarget(r.FormValue("next"))
	ip := clientIP(r)
	// Checked BEFORE the credential compare -- see loginAttemptTracker's
	// own doc comment: this caps the bcrypt CPU a burst of guesses can
	// burn, not just the guess count.
	if attempts.locked(ip) {
		log.Printf("dashboard login: %s is locked out after %d recent failures", ip, maxLoginFailures)
		w.WriteHeader(http.StatusTooManyRequests)
		w.Write([]byte(loginPageHTML(next, "Too many failed attempts — try again later.", cfg.Theme)))
		return
	}
	user := r.FormValue("username")
	pass := r.FormValue("password")
	remember := r.FormValue("remember") == "on"
	if !engine.CredentialsValid(user, pass, cfg.DashboardAuthUser, cfg.DashboardAuthPassHash) {
		attempts.recordFailure(ip)
		// Auth event logging, per this project's own standing security
		// rules -- the username is logged (it's not a secret and helps
		// spot a real attack), the password never is.
		log.Printf("dashboard login: failed attempt for user %q from %s", user, ip)
		w.Write([]byte(loginPageHTML(next, "Invalid username or password.", cfg.Theme)))
		return
	}
	attempts.recordSuccess(ip)
	log.Printf("dashboard login: %q signed in from %s", user, ip)
	token, err := engine.GenerateSessionToken()
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	ttl := shortSessionSeconds
	maxAge := 0 // unset -- a true session cookie, gone when the browser closes
	if remember {
		ttl = rememberMeSeconds
		maxAge = rememberMeSeconds
	}
	if err := db.CreateSession(token, float64(time.Now().Unix())+float64(ttl)); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    token,
		Path:     "/",
		MaxAge:   maxAge,
		HttpOnly: true,
		// Secure only when the request actually arrived over TLS: the
		// dashboard is plain HTTP on loopback by default, and a Secure
		// cookie would never be sent back in that case.
		Secure:   r.TLS != nil,
		SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(w, r, next, http.StatusSeeOther)
}

// handleLogout ends the current session (if any -- a missing/already-gone
// cookie is not an error, just nothing to do) and sends the browser back
// to the login page.
//
// Deliberately a plain GET, not a CSRF-protected POST -- csrfMiddleware
// only ever checks non-GET/HEAD requests (see CSRFOriginAllowed), so this
// route is NOT actually covered by it, and a page anywhere on the LAN
// could force a logout via a bare `<img src="/logout">`. That's an
// accepted tradeoff, not an oversight: forcing someone's session to end
// exposes nothing and changes no data, so it doesn't meet the bar CSRF
// protection exists for here (an arbitrary-file-move vulnerability is
// what earned every mutating POST route its protection
// -- see handleDuplicatesResolve's own doc comment). Making this a POST
// behind a confirmation would add real complexity for a low-severity
// annoyance.
func handleLogout(w http.ResponseWriter, r *http.Request, db *statedb.DB) {
	if cookie, err := r.Cookie(sessionCookieName); err == nil {
		if err := db.DeleteSession(cookie.Value); err != nil {
			// The cookie is cleared below regardless, but a session row that
			// outlives a logout is worth saying out loud.
			log.Printf("dashboard logout: could not delete session: %v", err)
		}
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   r.TLS != nil,
		SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// csrfMiddleware wraps h with the CSRF check in engine.CSRFOriginAllowed --
// deliberately a plain, cross-platform function there (not inline here),
// since this whole file is //go:build windows and can't be unit-tested
// from this environment at all.
func csrfMiddleware(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !engine.CSRFOriginAllowed(r.Method, r.Header.Get("Origin"), r.Header.Get("Referer"), r.Host) {
			http.Error(w, "cross-origin request rejected", http.StatusForbidden)
			return
		}
		h.ServeHTTP(w, r)
	})
}

// ── Backup ─────────────────────────────────────────────────────────────
