package statedb

import "database/sql"

// CreateSession records a new dashboard login session, valid until
// expiresAt (a Unix-epoch float, same units as every other timestamp in
// this package -- see now()). Called once per successful login; the token
// itself (see engine.GenerateSessionToken) is the only thing the browser
// gets back, as an HttpOnly cookie.
func (db *DB) CreateSession(token string, expiresAt float64) error {
	_, err := db.conn.Exec(
		`INSERT INTO dashboard_sessions (token, created_at, expires_at) VALUES (?,?,?)`,
		token, now(), expiresAt,
	)
	return err
}

// ValidateSession reports whether token names a session that still exists
// and hasn't expired -- called on every dashboard request once auth is
// enabled. An expired row is deleted on the way out, so this doubles as
// lazy cleanup: nothing sweeps dashboard_sessions on a timer, since every
// row is either checked (and pruned if stale) or simply never looked at
// again once its browser session ends.
func (db *DB) ValidateSession(token string) (bool, error) {
	var expiresAt float64
	err := db.conn.QueryRow(`SELECT expires_at FROM dashboard_sessions WHERE token = ?`, token).Scan(&expiresAt)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if expiresAt < now() {
		if _, derr := db.conn.Exec(`DELETE FROM dashboard_sessions WHERE token = ?`, token); derr != nil {
			return false, derr
		}
		return false, nil
	}
	return true, nil
}

// DeleteSession removes one session -- called on logout. A no-op, not an
// error, if the token is already gone (an expired session logged out
// after ValidateSession already pruned it, or a double-submitted logout).
func (db *DB) DeleteSession(token string) error {
	_, err := db.conn.Exec(`DELETE FROM dashboard_sessions WHERE token = ?`, token)
	return err
}
