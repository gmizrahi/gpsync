package statedb

import "database/sql"

// RecordCrash appends one recovered-panic record -- append-only, like
// throttle_events, since this is a permanent incident log, not mutable
// state.
func (db *DB) RecordCrash(context, message string) error {
	_, err := db.conn.Exec(
		`INSERT INTO crash_log (at, context, message) VALUES (?,?,?)`,
		now(), context, message,
	)
	return err
}

// CrashRecord is one row of crash_log.
type CrashRecord struct {
	At      float64
	Context string
	Message string
}

// LastCrash returns the most recent recovered panic, if any -- ok is false
// if gpsync-tray has never recorded one. Backs the Status page's "last
// crash" row.
func (db *DB) LastCrash() (rec CrashRecord, ok bool, err error) {
	// Ordered by id, not at -- two crashes recorded in quick succession
	// (or a clock with coarse resolution) could tie on the wall-clock
	// timestamp; the autoincrement id is strictly insertion-ordered
	// regardless.
	err = db.conn.QueryRow(
		`SELECT at, context, message FROM crash_log ORDER BY id DESC LIMIT 1`,
	).Scan(&rec.At, &rec.Context, &rec.Message)
	if err == sql.ErrNoRows {
		return CrashRecord{}, false, nil
	}
	if err != nil {
		return CrashRecord{}, false, err
	}
	return rec, true, nil
}
