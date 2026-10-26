package dashboard

import (
	"errors"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gmizrahi/gpsync/internal/config"
	"github.com/gmizrahi/gpsync/internal/engine"
	"github.com/gmizrahi/gpsync/internal/statedb"
)

// apiTokenRow is one token in Settings' list. Never the token itself: only
// its hash is stored, so there is nothing to show.
type apiTokenRow struct {
	Name     string
	Scopes   string
	Created  string
	LastUsed string
	Expires  string
	Expired  bool
}

// apiTokenNotice carries what the API tokens card reports after an action.
// NewToken is set exactly once, on the page the create request itself
// returns: never in a redirect, so it can't end up in a URL, a log line or
// the browser's history.
type apiTokenNotice struct {
	NewName  string
	NewToken string
	Message  string
	Error    string
}

// apiTokenExpiryChoices are the create form's expiry options, and the only
// values the create handler accepts from it. "default" takes the
// api_token_lifetime setting. The CLI accepts any lifetime; Settings accepts
// only what it offers.
var apiTokenExpiryChoices = []string{"default", "30d", "90d", "365d", "never"}

// logSafe neutralizes line breaks in a request-derived value before it is
// logged, so a submitted value can't forge a log line. Values the server
// has already validated pass through it too: one rule for everything a
// request supplies, rather than a judgement per call site.
func logSafe(s string) string {
	return logLineBreaks.Replace(s)
}

var logLineBreaks = strings.NewReplacer("\n", `\n`, "\r", `\r`)

func apiTokenRows(db *statedb.DB, now time.Time) ([]apiTokenRow, error) {
	tokens, err := db.ListAPITokens()
	if err != nil {
		return nil, err
	}
	when := func(v float64) string { return time.Unix(int64(v), 0).Format("2006-01-02 15:04") }
	rows := make([]apiTokenRow, 0, len(tokens))
	for _, t := range tokens {
		r := apiTokenRow{Name: t.Name, Scopes: t.Scopes, Created: when(t.CreatedAt), LastUsed: "never", Expires: "never"}
		if t.LastUsedAt.Valid {
			r.LastUsed = when(t.LastUsedAt.Float64)
		}
		if t.ExpiresAt.Valid {
			r.Expires = when(t.ExpiresAt.Float64)
			r.Expired = t.ExpiresAt.Float64 <= float64(now.Unix())
		}
		rows = append(rows, r)
	}
	return rows, nil
}

// handleAPITokenCreate makes a read-scoped token from Settings and returns
// the page with the token on it, once. Settings creates read tokens only;
// the CLI can grant other scopes.
func handleAPITokenCreate(w http.ResponseWriter, r *http.Request, db *statedb.DB, cfg config.Config) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	name := strings.TrimSpace(r.PostForm.Get("token_name"))
	// Only the form's own options -- matched exactly, and from here on the
	// matched CONSTANT is used, never the submitted string. The lifetime
	// parser trims its input, so before this a value like "\n90d" parsed as
	// 90 days and carried its newline into the log (CodeQL go/log-injection).
	choice := ""
	for _, c := range apiTokenExpiryChoices {
		if r.PostForm.Get("token_expires") == c {
			choice = c
		}
	}
	notice := apiTokenNotice{}
	var err error
	lifetimeText := cfg.APITokenLifetime
	switch choice {
	case "":
		err = errors.New("choose one of the offered expiry options")
	case "default":
	default:
		lifetimeText = choice
	}
	var lifetime time.Duration
	var expires bool
	if err == nil {
		lifetime, expires, err = engine.ParseAPITokenLifetime(lifetimeText)
	}
	if err == nil {
		var token string
		token, err = engine.CreateAPIToken(db, name, []string{engine.ScopeRead}, lifetime, expires, time.Now())
		if err == nil {
			log.Printf("api: token %q created from the dashboard (scope read, expires %s)", logSafe(name), logSafe(lifetimeText))
			notice.NewName, notice.NewToken = name, token
		}
	}
	if err != nil {
		if errors.Is(err, statedb.ErrAPITokenNameTaken) {
			err = errors.New("a token named " + name + " already exists -- revoke it first, or pick another name")
		}
		notice.Error = err.Error()
	}
	page, rerr := renderSettingsPage(db, cfg, false, false, "", notice)
	if rerr != nil {
		http.Error(w, rerr.Error(), http.StatusInternalServerError)
		return
	}
	// The response itself holds the token: never cache it anywhere.
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if notice.Error != "" {
		w.WriteHeader(http.StatusBadRequest)
	}
	w.Write([]byte(page))
}

// handleAPITokenRevoke deletes a token; the next request carrying it is
// refused.
func handleAPITokenRevoke(w http.ResponseWriter, r *http.Request, db *statedb.DB) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	name := r.PostForm.Get("token_name")
	v := url.Values{}
	if err := engine.RevokeAPIToken(db, name); err != nil {
		v.Set("token_error", err.Error())
	} else {
		log.Printf("api: token %q revoked from the dashboard", logSafe(name))
		v.Set("token_revoked", name)
	}
	http.Redirect(w, r, "/settings?"+v.Encode()+"#api-tokens", http.StatusSeeOther)
}
