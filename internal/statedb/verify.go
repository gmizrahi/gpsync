package statedb

// VerifyCandidate is one uploaded item due for an existence check.
type VerifyCandidate struct {
	SHA256      string
	MediaItemID string
	Path        string
}

// UploadsToVerify returns items that were really uploaded (so there's a
// media item id to ask about), least-recently-verified first, so repeated
// sampling rotates through the library instead of re-checking the same
// few. Never-verified rows come first.
func (db *DB) UploadsToVerify(limit int) ([]VerifyCandidate, error) {
	rows, err := db.conn.Query(
		`SELECT sha256, google_media_item_id, COALESCE(first_source_path, '')
		   FROM uploads
		  WHERE status='uploaded' AND google_media_item_id IS NOT NULL
		  ORDER BY verified_at IS NOT NULL, verified_at
		  LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []VerifyCandidate
	for rows.Next() {
		var c VerifyCandidate
		if err := rows.Scan(&c.SHA256, &c.MediaItemID, &c.Path); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// MarkVerified records the outcome of an existence check. A non-empty
// verifyError is kept alongside the timestamp so a miss stays visible
// rather than being indistinguishable from a pass.
func (db *DB) MarkVerified(sha256 string, at float64, verifyError string) error {
	_, err := db.conn.Exec(
		`UPDATE uploads SET verified_at=?, verify_error=? WHERE sha256=?`,
		at, verifyError, sha256)
	return err
}

// ClearVerificationVerdicts wipes every recorded verification result.
// Exists because the first release of `gpsync verify` wrote FALSE ones:
// with an append-only credential Google answers mediaItems.get with 404,
// so 50 files that demonstrably existed were recorded as missing. Bad
// data about whether a backup exists is worse than no data.
func (db *DB) ClearVerificationVerdicts() (int64, error) {
	res, err := db.conn.Exec(`UPDATE uploads SET verified_at=NULL, verify_error=NULL
	                           WHERE verified_at IS NOT NULL OR verify_error IS NOT NULL`)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// VerificationSummary counts how much of the library has ever been
// confirmed to still exist remotely, and how many checks came back bad.
func (db *DB) VerificationSummary() (verified, failed, total int, lastAt float64, err error) {
	row := db.conn.QueryRow(
		`SELECT
		   COUNT(*),
		   COALESCE(SUM(CASE WHEN verified_at IS NOT NULL THEN 1 ELSE 0 END), 0),
		   COALESCE(SUM(CASE WHEN verify_error IS NOT NULL AND verify_error != '' THEN 1 ELSE 0 END), 0),
		   COALESCE(MAX(verified_at), 0)
		 FROM uploads WHERE status='uploaded' AND google_media_item_id IS NOT NULL`)
	err = row.Scan(&total, &verified, &failed, &lastAt)
	return verified, failed, total, lastAt, err
}
