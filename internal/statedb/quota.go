package statedb

import "database/sql"

func (db *DB) QuotaUsed(datePT string) (int, error) {
	row := db.conn.QueryRow(`SELECT requests_used FROM quota_daily WHERE date_pt=?`, datePT)
	var n int
	err := row.Scan(&n)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	return n, err
}

func (db *DB) QuotaIncrement(datePT string, n int) error {
	_, err := db.conn.Exec(
		`INSERT INTO quota_daily (date_pt, requests_used) VALUES (?,?)
		 ON CONFLICT(date_pt) DO UPDATE SET requests_used=requests_used+?`,
		datePT, n, n,
	)
	return err
}

// QuotaSet overwrites (not adds to) a date's tracked usage -- for manually
// correcting drift, e.g. other requests against the same GCP project that
// gpsync's own tracking never saw (another gpsync instance run in parallel,
// rclone using the same imported OAuth client, manual API calls).
func (db *DB) QuotaSet(datePT string, n int) error {
	_, err := db.conn.Exec(
		`INSERT INTO quota_daily (date_pt, requests_used) VALUES (?,?)
		 ON CONFLICT(date_pt) DO UPDATE SET requests_used=excluded.requests_used`,
		datePT, n,
	)
	return err
}

// ── extension stats ────────────────────────────────────────────────────
