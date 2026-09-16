package statedb

import "database/sql"

type FileSeen struct {
	Path        string
	SHA256      string
	Mtime       float64
	Size        int64
	LastScanned float64
}

func (db *DB) GetFileSeen(path string) (*FileSeen, error) {
	row := db.conn.QueryRow(`SELECT path, sha256, mtime, size, last_scanned FROM files_seen WHERE path=?`, path)
	var f FileSeen
	if err := row.Scan(&f.Path, &f.SHA256, &f.Mtime, &f.Size, &f.LastScanned); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	return &f, nil
}

func (db *DB) UpsertFileSeen(path, sha256 string, mtime float64, size int64) error {
	_, err := db.conn.Exec(
		`INSERT INTO files_seen (path, sha256, mtime, size, last_scanned) VALUES (?,?,?,?,?)
		 ON CONFLICT(path) DO UPDATE SET sha256=excluded.sha256, mtime=excluded.mtime,
		 size=excluded.size, last_scanned=excluded.last_scanned`,
		path, sha256, mtime, size, now(),
	)
	return err
}

// DeleteFileSeen drops the scan-cache row for one path. Used when a file is
// deleted from disk (`gpsync duplicates resolve`), so the cache doesn't keep
// describing a file that no longer exists.
//
// Leaving the row behind would be harmless -- a future scan simply won't
// rediscover the path, and if a DIFFERENT file later took that exact name
// the mtime/size check would miss and re-hash it -- but removing it is a
// single cheap statement, so there's no reason to leave the litter.
func (db *DB) DeleteFileSeen(path string) error {
	_, err := db.conn.Exec(`DELETE FROM files_seen WHERE path=?`, path)
	return err
}

// ── uploads (the dedup + audit ledger, keyed by content hash) ─────────────

// BasenameMatch is one file elsewhere in the library sharing a
// needs_review item's filename -- a candidate for being "the same photo,
// already tracked", shown to the user for visual confirmation before they
// decide whether to queue or ignore the originals-folder copy.
type BasenameMatch struct {
	SHA256 string
	Path   string
	Status string // "" if this hash has no uploads row at all (files_seen-only)
}

// FindByBasename searches the WHOLE library (not just the needs_review
// item's own immediate parent folder) for files sharing its filename,
// excluding the item's own hash -- broader than the immediate-parent match
// that's actually expected (an originals-folder backup's usual sibling is
// right next to it), so the review UI can show real visual confirmation
// rather than just trusting the expected location blindly. Matched via
// LIKE against files_seen.path (no basename index exists -- acceptable for
// an occasional, manually-triggered review page, not a hot path), reusing
// the same likeEscaper this file already uses for folder-prefix matching.
func (db *DB) FindByBasename(basename, excludeSHA256 string) ([]BasenameMatch, error) {
	// The separator must be escaped TOGETHER with basename, not
	// concatenated raw -- see originalsLikePattern's own doc comment for
	// the exact bug this fixes: a raw, unescaped Windows separator ('\')
	// collides with the SAME character used as this query's own
	// ESCAPE '\' character, silently breaking the match into matching
	// nothing (or the wrong thing) instead of erroring. This was a
	// SECOND, more serious instance of that bug: FindByBasename backs
	// BOTH the review UI's candidate list (BuildOriginalsReviewItem) AND
	// the auto-resolution sweep's "does a match exist elsewhere"
	// check (AutoResolveObviousOriginals) -- on Windows, an always-empty
	// result from this function meant AutoResolveObviousOriginals could
	// never reach its own genuinely-ambiguous case at all: every
	// originals-folder file without an immediate-parent sibling looked
	// like it had no match ANYWHERE, so it was auto-queued as a normal
	// upload instead of correctly being left for human review.
	pattern := "%" + likeEscaper.Replace(originalsFolderSep+basename)
	rows, err := db.conn.Query(
		`SELECT fs.sha256, fs.path, COALESCE(u.status, '') FROM files_seen fs
		 LEFT JOIN uploads u ON u.sha256 = fs.sha256
		 WHERE fs.path LIKE ? ESCAPE '\' AND fs.sha256 != ?
		 ORDER BY fs.path`,
		pattern, excludeSHA256,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []BasenameMatch
	for rows.Next() {
		var m BasenameMatch
		if err := rows.Scan(&m.SHA256, &m.Path, &m.Status); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// FileSeenPathsUnder returns every scan-cache path under one of prefixes.
func (db *DB) FileSeenPathsUnder(prefixes []string) ([]string, error) {
	if len(prefixes) == 0 {
		return nil, nil
	}
	where, args := likeAnyPrefix("path", prefixes)
	// #nosec G202 -- placeholder-only clause, values passed via args.
	rows, err := db.conn.Query(`SELECT path FROM files_seen WHERE `+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// FileSeenRow is one scan-cache entry.
type FileSeenRow struct {
	Path   string
	SHA256 string
	Mtime  float64
	Size   int64
}

// FilesSeenUnder returns every scan-cache entry under one of prefixes.
func (db *DB) FilesSeenUnder(prefixes []string) ([]FileSeenRow, error) {
	if len(prefixes) == 0 {
		return nil, nil
	}
	where, args := likeAnyPrefix("path", prefixes)
	// #nosec G202 -- placeholder-only clause, values passed via args.
	rows, err := db.conn.Query(`SELECT path, COALESCE(sha256, ''), COALESCE(mtime, 0), COALESCE(size, 0) FROM files_seen WHERE `+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []FileSeenRow
	for rows.Next() {
		var r FileSeenRow
		if err := rows.Scan(&r.Path, &r.SHA256, &r.Mtime, &r.Size); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
