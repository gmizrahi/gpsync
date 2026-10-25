package engine

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/gmizrahi/gpsync/internal/statedb"
)

// API bearer tokens (issue #67). A token is `gps_` followed by 32 random
// bytes, base64url: 256 bits of entropy, and a prefix that makes one easy
// to recognize in a file it should not be in.
//
// Only the token's SHA-256 is stored. With that much entropy a fast hash is
// enough -- there is nothing to brute-force -- and the hash can be the
// lookup key itself: timing differences in finding a row by hash reveal
// nothing a caller can use, and there is no secret left to compare.

// APITokenPrefix starts every API token.
const APITokenPrefix = "gps_"

// Scopes a token can carry. Every /api/v1 route requires one.
const (
	// ScopeRead: read-only queries, such as the media API.
	ScopeRead = "read"
	// ScopeControl: actions on the running tray, such as quitting it.
	ScopeControl = "control"
)

var knownScopes = []string{ScopeRead, ScopeControl}

// APITokenGrace is how long a rotated-out token keeps working, so a client
// that lost the rotate response can retry with the token it still has.
const APITokenGrace = 10 * time.Minute

// apiTokenTouchInterval bounds how often a token's last-used time is
// written: a client polling every second must not make every read a write.
const apiTokenTouchInterval = 60 // seconds

// ErrAPITokenInvalid is the one error a failed authentication reports:
// missing, malformed, unknown, expired and revoked tokens look identical to
// the caller, so a 401 never says which.
var ErrAPITokenInvalid = errors.New("invalid API token")

// ErrNoSuchAPIToken reports a revoke of a name that has no token.
var ErrNoSuchAPIToken = errors.New("no API token by that name")

var apiTokenNameRe = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

// apiTokenRe matches the exact shape a token has. A malformed bearer value
// is refused before it is hashed or looked up -- which only spares the
// lookup: its hash would match nothing anyway, so the outcome is the same
// (a break-check removing this changes no test result).
var apiTokenRe = regexp.MustCompile(`^gps_[A-Za-z0-9_-]{43}$`)

// HashAPIToken is the form a token is stored and looked up in.
func HashAPIToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func newAPIToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return APITokenPrefix + base64.RawURLEncoding.EncodeToString(b), nil
}

// ParseScopes validates a comma- or space-separated scope list and returns
// it normalized: known scopes only, deduplicated, in a fixed order.
func ParseScopes(s string) ([]string, error) {
	var out []string
	for _, f := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' }) {
		if !slices.Contains(knownScopes, f) {
			return nil, fmt.Errorf("unknown scope %q (known: %s)", f, strings.Join(knownScopes, ", "))
		}
		if !slices.Contains(out, f) {
			out = append(out, f)
		}
	}
	if len(out) == 0 {
		return nil, errors.New("a token needs at least one scope")
	}
	slices.SortFunc(out, func(a, b string) int { return slices.Index(knownScopes, a) - slices.Index(knownScopes, b) })
	return out, nil
}

// ParseAPITokenLifetime reads "90d", "12h", "30m" or "never". ok is false
// for "never": a token that lives until revoked.
func ParseAPITokenLifetime(s string) (d time.Duration, ok bool, err error) {
	s = strings.TrimSpace(strings.ToLower(s))
	if s == "never" {
		return 0, false, nil
	}
	if n, found := strings.CutSuffix(s, "d"); found {
		days, err := strconv.Atoi(n)
		if err != nil || days <= 0 || days > 3650 {
			return 0, false, fmt.Errorf("lifetime %q: want a number of days from 1 to 3650, like 90d", s)
		}
		return time.Duration(days) * 24 * time.Hour, true, nil
	}
	d, err = time.ParseDuration(s)
	if err != nil || d < time.Minute {
		return 0, false, fmt.Errorf("lifetime %q: want something like 90d, 12h, 30m, or never", s)
	}
	return d, true, nil
}

func expiryAt(now time.Time, lifetime time.Duration, expires bool) sql.NullFloat64 {
	if !expires {
		return sql.NullFloat64{}
	}
	return sql.NullFloat64{Float64: float64(now.Add(lifetime).Unix()), Valid: true}
}

// CreateAPIToken makes a named token and returns it. This is the only time
// the token exists anywhere but in the caller's hands: only its hash is
// stored.
func CreateAPIToken(db *statedb.DB, name string, scopes []string, lifetime time.Duration, expires bool, now time.Time) (string, error) {
	if !apiTokenNameRe.MatchString(name) {
		return "", fmt.Errorf("token name %q: use 1-64 letters, digits, '.', '_' or '-'", name)
	}
	return insertAPIToken(db, name, scopes, lifetime, expires, false, now)
}

func insertAPIToken(db *statedb.DB, name string, scopes []string, lifetime time.Duration, expires, ephemeral bool, now time.Time) (string, error) {
	token, err := newAPIToken()
	if err != nil {
		return "", err
	}
	rec := statedb.APIToken{
		Name: name, Scopes: strings.Join(scopes, " "), CreatedAt: float64(now.Unix()),
		ExpiresAt: expiryAt(now, lifetime, expires), Ephemeral: ephemeral,
	}
	if expires {
		rec.Lifetime = sql.NullFloat64{Float64: lifetime.Seconds(), Valid: true}
	}
	err = db.InsertAPIToken(rec, HashAPIToken(token))
	if err != nil {
		return "", err
	}
	return token, nil
}

// RevokeAPIToken deletes a token by name. It takes effect on the next
// request: every request looks its token up afresh.
func RevokeAPIToken(db *statedb.DB, name string) error {
	ok, err := db.DeleteAPIToken(name)
	if err != nil {
		return err
	}
	if !ok {
		return ErrNoSuchAPIToken
	}
	return nil
}

// Authenticated is what a valid token resolves to.
type Authenticated struct {
	Token statedb.APIToken
	// ViaPrevious: the request used the token's previous value, still
	// within its grace period after a rotation.
	ViaPrevious bool
}

// HasScope reports whether the token carries scope.
func (a Authenticated) HasScope(scope string) bool {
	return slices.Contains(strings.Fields(a.Token.Scopes), scope)
}

// AuthenticateAPIToken resolves a presented token. Every way it can fail
// returns ErrAPITokenInvalid, so a caller cannot tell an unknown token from
// an expired or revoked one. A success records the use, coarsely.
func AuthenticateAPIToken(db *statedb.DB, presented string, now time.Time) (Authenticated, error) {
	if !apiTokenRe.MatchString(presented) {
		return Authenticated{}, ErrAPITokenInvalid
	}
	nowF := float64(now.Unix())
	t, viaPrev, err := db.FindAPITokenByHash(HashAPIToken(presented), nowF)
	if err != nil {
		return Authenticated{}, err
	}
	if t == nil || (t.ExpiresAt.Valid && t.ExpiresAt.Float64 <= nowF) {
		return Authenticated{}, ErrAPITokenInvalid
	}
	if err := db.TouchAPIToken(t.Name, nowF, apiTokenTouchInterval); err != nil {
		return Authenticated{}, err
	}
	return Authenticated{Token: *t, ViaPrevious: viaPrev}, nil
}

// RotateAPIToken trades the presented token for a new one with the same
// name and scopes, renewing the lifetime the token was created with -- a
// token created for 30 days gets another 30, not whatever the default
// setting says now -- and one that never expires stays that way. The
// presented token keeps working for APITokenGrace afterwards (see
// statedb.RotateAPIToken for what a retry with it does).
func RotateAPIToken(db *statedb.DB, a Authenticated, presented string, now time.Time) (string, error) {
	lifetime := time.Duration(a.Token.Lifetime.Float64 * float64(time.Second))
	expires := a.Token.Lifetime.Valid
	token, err := newAPIToken()
	if err != nil {
		return "", err
	}
	ok, err := db.RotateAPIToken(a.Token.Name, HashAPIToken(presented), a.ViaPrevious, HashAPIToken(token),
		expiryAt(now, lifetime, expires), float64(now.Add(APITokenGrace).Unix()), float64(now.Unix()))
	if err != nil {
		return "", err
	}
	if !ok {
		return "", ErrAPITokenInvalid
	}
	return token, nil
}

// MintEphemeralAPIToken creates a short-lived token for one CLI command:
// gpsync's own CLI talks to the running tray through /api/v1 like any other
// client, but needs no login and no configuration, because it can write the
// ledger and mint its own token. Nothing secret outlives the command --
// cleanup deletes the token, and an expired one is swept anyway.
func MintEphemeralAPIToken(db *statedb.DB, scopes []string, ttl time.Duration, now time.Time) (token string, cleanup func(), err error) {
	if err := db.SweepAPITokens(float64(now.Unix())); err != nil {
		return "", nil, err
	}
	suffix := make([]byte, 6)
	if _, err := rand.Read(suffix); err != nil {
		return "", nil, err
	}
	name := "cli-" + hex.EncodeToString(suffix)
	token, err = insertAPIToken(db, name, scopes, ttl, true, true, now)
	if err != nil {
		return "", nil, err
	}
	// A failed delete is not worth failing the command over: the token
	// expires within ttl and the next mint sweeps it.
	return token, func() { _, _ = db.DeleteAPIToken(name) }, nil
}
