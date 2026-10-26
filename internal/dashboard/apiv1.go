package dashboard

import (
	"encoding/json"
	"errors"
	"log"
	"net"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/gmizrahi/gpsync/internal/config"
	"github.com/gmizrahi/gpsync/internal/engine"
	"github.com/gmizrahi/gpsync/internal/statedb"
)

// APIv1Prefix is where the token-authenticated API begins (issue #67). The
// dashboard's own /api/* endpoints -- /api/status, /api/toggle, /api/quit
// and the rest -- are internal to its page and keep cookie auth; nothing
// under /api/v1 ever looks at a cookie, and nothing outside it at a token.
const APIv1Prefix = "/api/v1/"

// isAPIv1 reports whether a request belongs to the token API.
func isAPIv1(r *http.Request) bool {
	return strings.HasPrefix(r.URL.Path, APIv1Prefix) || r.URL.Path == strings.TrimSuffix(APIv1Prefix, "/")
}

// apiRoute is one /api/v1 endpoint: the method it accepts, the scope it
// requires, and the handler, which receives the authenticated token.
type apiRoute struct {
	method  string
	scope   string
	handler func(w http.ResponseWriter, r *http.Request, a engine.Authenticated, presented string)
}

// apiV1Handler serves /api/v1. Every request is, in order:
//
//  1. from loopback, or refused with 403 -- even when the dashboard listens
//     on every interface, and over the HTTPS listener too: the API is for
//     programs on this machine;
//  2. carrying a valid bearer token, or refused with 401 -- always, whether
//     or not dashboard login is on;
//  3. carrying the route's scope, or refused with 403.
//
// No CSRF check: nothing here is authenticated by anything a browser sends
// on its own. No CORS headers either, so a page in a browser can't read a
// response; callers are other programs.
func apiV1Handler(db *statedb.DB, quit func(), currentConfig func() config.Config) http.Handler {
	failures := newLoginAttemptTracker()
	thumbs := engine.NewThumbCache("", engine.ThumbCacheMaxBytes)
	routes := map[string]apiRoute{
		"/api/v1/token": {http.MethodGet, engine.ScopeRead, func(w http.ResponseWriter, r *http.Request, a engine.Authenticated, _ string) {
			writeTokenInfo(w, a.Token, "")
		}},
		"/api/v1/token/rotate": {http.MethodPost, "", func(w http.ResponseWriter, r *http.Request, a engine.Authenticated, presented string) {
			// No scope: rotating is something a token may always do to
			// itself, whatever it is otherwise allowed to do.
			handleRotate(w, db, a, presented)
		}},
		"/api/v1/control/quit": {http.MethodPost, engine.ScopeControl, func(w http.ResponseWriter, r *http.Request, a engine.Authenticated, _ string) {
			serveQuit(w, quit)
		}},
		"/api/v1/media": {http.MethodGet, engine.ScopeRead, func(w http.ResponseWriter, r *http.Request, _ engine.Authenticated, _ string) {
			handleMediaList(w, r, db)
		}},
		mediaItemRoute: {http.MethodGet, engine.ScopeRead, func(w http.ResponseWriter, r *http.Request, _ engine.Authenticated, _ string) {
			handleMediaOriginal(w, r, db)
		}},
		mediaThumbRoute: {http.MethodGet, engine.ScopeRead, func(w http.ResponseWriter, r *http.Request, _ engine.Authenticated, _ string) {
			handleMediaThumb(w, r, db, thumbs, currentConfig())
		}},
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		if !fromLoopback(r) {
			http.Error(w, "the API answers on this machine only", http.StatusForbidden)
			return
		}
		ip := clientIP(r)
		presented, ok := bearerToken(r)
		var a engine.Authenticated
		var err error
		if ok {
			a, err = engine.AuthenticateAPIToken(db, presented, time.Now())
		} else {
			err = engine.ErrAPITokenInvalid
		}
		if err != nil {
			if !errors.Is(err, engine.ErrAPITokenInvalid) {
				log.Printf("api: looking up a token failed: %v", err)
				http.Error(w, "internal error", http.StatusInternalServerError)
				return
			}
			// Every local client shares the loopback address, so a
			// lockout must not shut out the others: it applies only to
			// failing requests, and a valid token is still served below.
			// What it limits is a client that keeps failing -- guessing a
			// 256-bit token is not a threat a counter could stop anyway.
			if failures.locked(ip) {
				http.Error(w, "too many failed attempts", http.StatusTooManyRequests)
				return
			}
			failures.recordFailure(ip)
			reason := "missing or malformed Authorization header"
			if ok {
				reason = "invalid, expired or revoked token"
			}
			if failures.locked(ip) {
				log.Printf("api auth: %s from %s; further failures from there are refused for a while", reason, logSafe(ip))
			} else {
				log.Printf("api auth: %s from %s", reason, logSafe(ip))
			}
			w.Header().Set("WWW-Authenticate", `Bearer realm="gpsync"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}

		key, sha := apiRouteKey(r.URL.Path)
		route, found := routes[key]
		if !found || path.Clean(r.URL.Path) != r.URL.Path {
			http.NotFound(w, r)
			return
		}
		if r.Method != route.method {
			w.Header().Set("Allow", route.method)
			http.Error(w, route.method+" only", http.StatusMethodNotAllowed)
			return
		}
		if route.scope != "" && !a.HasScope(route.scope) {
			w.Header().Set("WWW-Authenticate", `Bearer realm="gpsync", error="insufficient_scope", scope="`+route.scope+`"`)
			http.Error(w, "this token lacks the "+route.scope+" scope", http.StatusForbidden)
			return
		}
		if sha != "" {
			r.SetPathValue("sha256", sha)
		}
		route.handler(w, r, a, presented)
	})
}

const (
	mediaItemRoute  = "/api/v1/media/{sha256}"
	mediaThumbRoute = "/api/v1/media/{sha256}/thumb"
)

// apiRouteKey maps a request path to its key in the route table, and returns
// the {sha256} segment of a per-file route. The segment is passed on as
// sent; the handlers refuse one that isn't a content hash with 400.
func apiRouteKey(p string) (key, sha string) {
	rest, ok := strings.CutPrefix(p, "/api/v1/media/")
	if !ok || rest == "" {
		return p, ""
	}
	sha, sub, _ := strings.Cut(rest, "/")
	switch sub {
	case "":
		// ".../{sha256}/" lands here too, and is refused by the router's
		// path.Clean check.
		return mediaItemRoute, sha
	case "thumb":
		return mediaThumbRoute, sha
	}
	return p, ""
}

// fromLoopback reports whether the request came from this machine. The
// dashboard has no proxy in front of it, so RemoteAddr is the peer itself.
func fromLoopback(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// bearerToken extracts the token from "Authorization: Bearer <token>". The
// scheme is case-insensitive (RFC 7235); the token is passed on as sent,
// and the token check refuses anything not shaped like a gpsync token.
func bearerToken(r *http.Request) (string, bool) {
	h := r.Header.Get("Authorization")
	scheme, token, found := strings.Cut(h, " ")
	if !found || !strings.EqualFold(scheme, "Bearer") || token == "" {
		return "", false
	}
	return token, true
}

type tokenInfoJSON struct {
	Name      string   `json:"name"`
	Scopes    []string `json:"scopes"`
	ExpiresAt *string  `json:"expires_at"` // RFC 3339; null: never
	Token     string   `json:"token,omitempty"`
}

func writeTokenInfo(w http.ResponseWriter, t statedb.APIToken, newToken string) {
	info := tokenInfoJSON{Name: t.Name, Scopes: strings.Fields(t.Scopes), Token: newToken}
	if t.ExpiresAt.Valid {
		s := time.Unix(int64(t.ExpiresAt.Float64), 0).UTC().Format(time.RFC3339)
		info.ExpiresAt = &s
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(info)
}

// handleRotate trades the presented token for a new one, renewing the
// lifetime it was created with.
func handleRotate(w http.ResponseWriter, db *statedb.DB, a engine.Authenticated, presented string) {
	now := time.Now()
	newToken, err := engine.RotateAPIToken(db, a, presented, now)
	if errors.Is(err, engine.ErrAPITokenInvalid) {
		w.Header().Set("WWW-Authenticate", `Bearer realm="gpsync"`)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if err != nil {
		log.Printf("api: rotating token %q failed: %v", a.Token.Name, err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	log.Printf("api: token %q rotated", a.Token.Name)
	t := a.Token
	if t.Lifetime.Valid {
		t.ExpiresAt.Float64 = float64(now.Add(time.Duration(t.Lifetime.Float64 * float64(time.Second))).Unix())
	}
	writeTokenInfo(w, t, newToken)
}
