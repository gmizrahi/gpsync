package engine

import (
	"bytes"
	"errors"
	"os"
	"regexp"
	"testing"
	"time"

	"github.com/gmizrahi/gpsync/internal/statedb"
)

var t0 = time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)

func mustCreate(t *testing.T, db *statedb.DB, name string, lifetime time.Duration, expires bool) string {
	t.Helper()
	tok, err := CreateAPIToken(db, name, []string{ScopeRead}, lifetime, expires, t0)
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

func authOK(t *testing.T, db *statedb.DB, tok string, at time.Time) Authenticated {
	t.Helper()
	a, err := AuthenticateAPIToken(db, tok, at)
	if err != nil {
		t.Fatalf("a valid token was refused at %v: %v", at, err)
	}
	return a
}

func authRefused(t *testing.T, db *statedb.DB, tok string, at time.Time, why string) {
	t.Helper()
	if _, err := AuthenticateAPIToken(db, tok, at); !errors.Is(err, ErrAPITokenInvalid) {
		t.Errorf("%s: err = %v, want ErrAPITokenInvalid", why, err)
	}
}

func TestAPIToken_FormatAndScope(t *testing.T) {
	db := openTestDB(t)
	tok := mustCreate(t, db, "app", 90*24*time.Hour, true)
	if !regexp.MustCompile(`^gps_[A-Za-z0-9_-]{43}$`).MatchString(tok) {
		t.Fatalf("token %q: want gps_ + 43 base64url characters (32 random bytes)", tok)
	}
	a := authOK(t, db, tok, t0)
	if a.Token.Name != "app" || !a.HasScope(ScopeRead) || a.HasScope(ScopeControl) {
		t.Errorf("authenticated as %+v; want name app with read and not control", a.Token)
	}
	if again := mustCreate(t, db, "other", time.Hour, true); again == tok {
		t.Error("two tokens came out identical")
	}
}

// Issue #67: a token must never appear in the database in clear. Checked in
// the ledger FILE itself, not through any query that might not look.
func TestAPIToken_NeverStoredInClear(t *testing.T) {
	db := openTestDB(t)
	tok := mustCreate(t, db, "app", time.Hour, true)
	a := authOK(t, db, tok, t0)
	rotated, err := RotateAPIToken(db, a, tok, t0)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(statedb.StateDBPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{tok, rotated, tok[len(APITokenPrefix):]} {
		if bytes.Contains(raw, []byte(secret)) {
			t.Errorf("the ledger file contains a token in clear: %q...", secret[:8])
		}
	}
	if !bytes.Contains(raw, []byte(HashAPIToken(rotated))) {
		t.Error("the ledger file doesn't contain the token's hash either -- the check above proves nothing")
	}
}

func TestAPIToken_WrongMalformedExpiredAndRevokedAreAllRefused(t *testing.T) {
	db := openTestDB(t)
	tok := mustCreate(t, db, "app", time.Hour, true)

	authRefused(t, db, "", t0, "empty")
	authRefused(t, db, "Bearer "+tok, t0, "the header's scheme passed through")
	authRefused(t, db, tok[:len(tok)-1], t0, "truncated")
	authRefused(t, db, "gps_"+regexp.MustCompile(`.`).ReplaceAllString(tok[4:], "A"), t0, "well-formed but unknown")

	authOK(t, db, tok, t0.Add(59*time.Minute))
	authRefused(t, db, tok, t0.Add(time.Hour), "expired at exactly its expiry")

	long := mustCreate(t, db, "long", 0, false)
	authOK(t, db, long, t0.AddDate(20, 0, 0))
	if err := RevokeAPIToken(db, "long"); err != nil {
		t.Fatal(err)
	}
	authRefused(t, db, long, t0, "revoked: must take effect on the very next request")
	if err := RevokeAPIToken(db, "long"); !errors.Is(err, ErrNoSuchAPIToken) {
		t.Errorf("revoking twice: err = %v, want ErrNoSuchAPIToken", err)
	}
}

func TestAPIToken_NamesAreValidatedAndUnique(t *testing.T) {
	db := openTestDB(t)
	for _, bad := range []string{"", "has space", "slash/name", "semi;colon", string(make([]byte, 65))} {
		if _, err := CreateAPIToken(db, bad, []string{ScopeRead}, time.Hour, true, t0); err == nil {
			t.Errorf("name %q was accepted", bad)
		}
	}
	mustCreate(t, db, "app", time.Hour, true)
	if _, err := CreateAPIToken(db, "app", []string{ScopeRead}, time.Hour, true, t0); !errors.Is(err, statedb.ErrAPITokenNameTaken) {
		t.Errorf("duplicate name: err = %v, want ErrAPITokenNameTaken", err)
	}
}

// The rotation handshake: the new token works, the old one keeps working
// for the grace period and then stops, and a retry with the old token --
// a client that lost the response -- gets a usable token without stretching
// the grace period.
func TestAPIToken_RotationHandshake(t *testing.T) {
	db := openTestDB(t)
	old := mustCreate(t, db, "app", 90*24*time.Hour, true)
	at := t0.Add(45 * 24 * time.Hour)

	newTok, err := RotateAPIToken(db, authOK(t, db, old, at), old, at)
	if err != nil {
		t.Fatal(err)
	}
	a := authOK(t, db, newTok, at)
	if a.ViaPrevious {
		t.Error("the new token authenticated as the previous one")
	}
	if want := float64(at.Add(90 * 24 * time.Hour).Unix()); a.Token.ExpiresAt.Float64 != want {
		t.Errorf("rotated expiry = %v, want a fresh lifetime from the rotation, %v", a.Token.ExpiresAt.Float64, want)
	}
	if prev := authOK(t, db, old, at.Add(APITokenGrace-time.Second)); !prev.ViaPrevious {
		t.Error("the old token, inside its grace period, did not authenticate as the previous one")
	}
	authRefused(t, db, old, at.Add(APITokenGrace), "the old token after its grace period")

	// The lost-response retry.
	old2 := mustCreate(t, db, "app2", 90*24*time.Hour, true)
	lost, err := RotateAPIToken(db, authOK(t, db, old2, at), old2, at)
	if err != nil {
		t.Fatal(err)
	}
	retryAt := at.Add(5 * time.Minute)
	retried, err := RotateAPIToken(db, authOK(t, db, old2, retryAt), old2, retryAt)
	if err != nil {
		t.Fatalf("a retry with the old token inside its grace period was refused: %v", err)
	}
	authOK(t, db, retried, retryAt)
	authRefused(t, db, lost, retryAt, "the never-received token after the retry replaced it")
	authRefused(t, db, old2, at.Add(APITokenGrace), "the old token: a retry must not extend its grace period")
}

func TestAPIToken_EphemeralIsUnlistedCleanedUpAndSwept(t *testing.T) {
	db := openTestDB(t)
	mustCreate(t, db, "app", time.Hour, true)
	tok, cleanup, err := MintEphemeralAPIToken(db, []string{ScopeControl}, 2*time.Minute, t0)
	if err != nil {
		t.Fatal(err)
	}
	if a := authOK(t, db, tok, t0); !a.HasScope(ScopeControl) || a.HasScope(ScopeRead) {
		t.Errorf("ephemeral scopes = %q, want exactly control", a.Token.Scopes)
	}
	list, err := db.ListAPITokens()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Name != "app" {
		t.Errorf("listing = %+v, want only the token a person created", list)
	}
	cleanup()
	authRefused(t, db, tok, t0, "an ephemeral token after its command cleaned up")

	// One whose command never cleaned up -- killed mid-run -- is swept by
	// the next mint once expired, and dead in the meantime anyway.
	stale, _, err := MintEphemeralAPIToken(db, []string{ScopeControl}, 2*time.Minute, t0)
	if err != nil {
		t.Fatal(err)
	}
	authRefused(t, db, stale, t0.Add(2*time.Minute), "an uncleaned ephemeral token after its ttl")
	if _, _, err := MintEphemeralAPIToken(db, []string{ScopeControl}, 2*time.Minute, t0.Add(3*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if t2, _, _ := db.FindAPITokenByHash(HashAPIToken(stale), float64(t0.Add(3*time.Minute).Unix())); t2 != nil {
		t.Error("the expired ephemeral token was not swept by the next mint")
	}
}

func TestParseScopesAndLifetime(t *testing.T) {
	if s, err := ParseScopes("control, read read"); err != nil || len(s) != 2 || s[0] != "read" || s[1] != "control" {
		t.Errorf("ParseScopes = %v, %v; want [read control]", s, err)
	}
	for _, bad := range []string{"", "write", "read,admin"} {
		if _, err := ParseScopes(bad); err == nil {
			t.Errorf("scopes %q were accepted", bad)
		}
	}
	for in, want := range map[string]time.Duration{"90d": 90 * 24 * time.Hour, "12h": 12 * time.Hour, "30m": 30 * time.Minute} {
		if d, ok, err := ParseAPITokenLifetime(in); err != nil || !ok || d != want {
			t.Errorf("lifetime %q = %v, %v, %v; want %v", in, d, ok, err, want)
		}
	}
	if _, ok, err := ParseAPITokenLifetime("never"); err != nil || ok {
		t.Errorf("never: ok=%v err=%v; want no expiry", ok, err)
	}
	for _, bad := range []string{"0d", "-1d", "90", "soon", "30s", "4000d"} {
		if _, _, err := ParseAPITokenLifetime(bad); err == nil {
			t.Errorf("lifetime %q was accepted", bad)
		}
	}
}

// A rotation renews the lifetime the token was CREATED with: a 30-day token
// gets another 30 days, not whatever the default setting says now, and one
// that never expires stays that way. Found in an end-to-end run, where a
// 30-day token came back from rotation with 90 days.
func TestAPIToken_RotationRenewsTheTokensOwnLifetime(t *testing.T) {
	db := openTestDB(t)
	short := mustCreate(t, db, "short", 30*24*time.Hour, true)
	at := t0.Add(20 * 24 * time.Hour)
	rotated, err := RotateAPIToken(db, authOK(t, db, short, at), short, at)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := authOK(t, db, rotated, at).Token.ExpiresAt.Float64, float64(at.Add(30*24*time.Hour).Unix()); got != want {
		t.Errorf("a 30-day token rotated to expire at %v, want 30 days from the rotation (%v)", time.Unix(int64(got), 0).UTC(), time.Unix(int64(want), 0).UTC())
	}

	forever := mustCreate(t, db, "forever", 0, false)
	rotatedForever, err := RotateAPIToken(db, authOK(t, db, forever, at), forever, at)
	if err != nil {
		t.Fatal(err)
	}
	if a := authOK(t, db, rotatedForever, at.AddDate(10, 0, 0)); a.Token.ExpiresAt.Valid {
		t.Errorf("a never-expiring token gained an expiry on rotation: %v", a.Token.ExpiresAt.Float64)
	}
}
