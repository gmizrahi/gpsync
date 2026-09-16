package statedb

// RecordThrottleEvent appends one row to throttle_events -- called every
// time the circuit breaker detects a REAL "concurrent write request"
// throttle (never the separate, documented daily-quota case). Append-only
// by design: this table exists purely as a permanent history to analyze
// later, so it's never updated or pruned.
func (db *DB) RecordThrottleEvent(rung int, waitSeconds float64, message string) error {
	_, err := db.conn.Exec(
		`INSERT INTO throttle_events (at, rung, wait_seconds, message) VALUES (?,?,?,?)`,
		now(), rung, waitSeconds, message,
	)
	return err
}

// ThrottleEvent is one recorded circuit-breaker throttle.
type ThrottleEvent struct {
	ID          int64
	At          float64
	Rung        int
	WaitSeconds float64
	Message     string
}

// ThrottleEvents returns every recorded throttle, oldest first.
func (db *DB) ThrottleEvents() ([]ThrottleEvent, error) {
	rows, err := db.conn.Query(`SELECT id, at, rung, wait_seconds, message FROM throttle_events ORDER BY at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ThrottleEvent
	for rows.Next() {
		var e ThrottleEvent
		if err := rows.Scan(&e.ID, &e.At, &e.Rung, &e.WaitSeconds, &e.Message); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
