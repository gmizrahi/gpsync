package statedb

import (
	"database/sql"
	"errors"
	"strings"
)

// APIToken is one row of api_tokens: everything about a bearer token except
// the token itself, which is never stored.
type APIToken struct {
	Name      string
	Scopes    string // space-separated
	CreatedAt float64
	ExpiresAt sql.NullFloat64 // NULL: never expires
	// Lifetime is the lifetime the token was created with, in seconds,
	// which a rotation renews. NULL: never expires.
	Lifetime   sql.NullFloat64
	LastUsedAt sql.NullFloat64
	RotatedAt  sql.NullFloat64
	Ephemeral  bool
}

// ErrAPITokenNameTaken reports that a token by that name already exists.
var ErrAPITokenNameTaken = errors.New("a token with that name already exists")

// #nosec G101 -- a list of column names, not a credential.
const apiTokenCols = `name, scopes, created_at, expires_at, lifetime_seconds, last_used_at, rotated_at, ephemeral`

func scanAPIToken(row interface{ Scan(...any) error }) (*APIToken, error) {
	var t APIToken
	var eph int
	if err := row.Scan(&t.Name, &t.Scopes, &t.CreatedAt, &t.ExpiresAt, &t.Lifetime, &t.LastUsedAt, &t.RotatedAt, &eph); err != nil {
		return nil, err
	}
	t.Ephemeral = eph != 0
	return &t, nil
}

// InsertAPIToken stores a new token under its hash.
func (db *DB) InsertAPIToken(t APIToken, hash string) error {
	eph := 0
	if t.Ephemeral {
		eph = 1
	}
	_, err := db.conn.Exec(
		`INSERT INTO api_tokens (name, token_hash, scopes, created_at, expires_at, lifetime_seconds, ephemeral) VALUES (?,?,?,?,?,?,?)`,
		t.Name, hash, t.Scopes, t.CreatedAt, t.ExpiresAt, t.Lifetime, eph,
	)
	if err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed: api_tokens.name") {
		return ErrAPITokenNameTaken
	}
	return err
}

// ListAPITokens returns every token a person created, by name. The CLI's
// per-command ephemeral tokens are not listed: they exist for seconds.
func (db *DB) ListAPITokens() ([]APIToken, error) {
	rows, err := db.conn.Query(`SELECT ` + apiTokenCols + ` FROM api_tokens WHERE ephemeral = 0 ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []APIToken
	for rows.Next() {
		t, err := scanAPIToken(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *t)
	}
	return out, rows.Err()
}

// DeleteAPIToken removes a token, current and previous hash alike, so the
// next request carrying either is refused. Reports whether one existed.
func (db *DB) DeleteAPIToken(name string) (bool, error) {
	res, err := db.conn.Exec(`DELETE FROM api_tokens WHERE name = ?`, name)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// FindAPITokenByHash looks a presented token up by its hash: as the
// token's current hash, or as its previous one while that is still within
// its grace period. viaPrevious says which, since a rotation retried with
// the previous token is handled differently.
func (db *DB) FindAPITokenByHash(hash string, now float64) (t *APIToken, viaPrevious bool, err error) {
	row := db.conn.QueryRow(`SELECT `+apiTokenCols+`, token_hash = ? FROM api_tokens
		WHERE token_hash = ? OR (prev_hash = ? AND prev_valid_until > ?)`, hash, hash, hash, now)
	var current int
	var tok APIToken
	var eph int
	err = row.Scan(&tok.Name, &tok.Scopes, &tok.CreatedAt, &tok.ExpiresAt, &tok.Lifetime, &tok.LastUsedAt, &tok.RotatedAt, &eph, &current)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	tok.Ephemeral = eph != 0
	return &tok, current == 0, nil
}

// TouchAPIToken records a use, at most once per minInterval seconds: a
// client polling every second must not turn every read into a write.
func (db *DB) TouchAPIToken(name string, now, minInterval float64) error {
	_, err := db.conn.Exec(`UPDATE api_tokens SET last_used_at = ?
		WHERE name = ? AND (last_used_at IS NULL OR last_used_at <= ?)`, now, name, now-minInterval)
	return err
}

// RotateAPIToken replaces a token's hash with newHash and a fresh expiry.
//
// Rotated with the CURRENT token, that token becomes the previous one,
// valid until prevValidUntil, so a client that lost the response can retry.
// Rotated with the PREVIOUS token -- that retry -- only the current hash is
// replaced (it was never received) and the grace period is NOT extended, so
// a retry cannot keep an old token alive indefinitely.
//
// Reports false when presentedHash no longer matches, for instance because
// the token was revoked or rotated again in between.
func (db *DB) RotateAPIToken(name, presentedHash string, viaPrevious bool, newHash string, newExpires sql.NullFloat64, prevValidUntil, now float64) (bool, error) {
	var res sql.Result
	var err error
	if viaPrevious {
		res, err = db.conn.Exec(`UPDATE api_tokens SET token_hash = ?, expires_at = ?, rotated_at = ?
			WHERE name = ? AND prev_hash = ? AND prev_valid_until > ?`,
			newHash, newExpires, now, name, presentedHash, now)
	} else {
		res, err = db.conn.Exec(`UPDATE api_tokens SET prev_hash = token_hash, prev_valid_until = ?,
			token_hash = ?, expires_at = ?, rotated_at = ? WHERE name = ? AND token_hash = ?`,
			prevValidUntil, newHash, newExpires, now, name, presentedHash)
	}
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// SweepAPITokens deletes expired ephemeral tokens and forgets previous
// hashes whose grace period is over. Tokens a person created are never
// deleted here, even expired: they stay listed, marked expired, until
// revoked -- an expired token silently vanishing would look like it had
// never been created.
func (db *DB) SweepAPITokens(now float64) error {
	if _, err := db.conn.Exec(`DELETE FROM api_tokens WHERE ephemeral = 1 AND expires_at <= ?`, now); err != nil {
		return err
	}
	_, err := db.conn.Exec(`UPDATE api_tokens SET prev_hash = NULL, prev_valid_until = NULL
		WHERE prev_valid_until IS NOT NULL AND prev_valid_until <= ?`, now)
	return err
}
