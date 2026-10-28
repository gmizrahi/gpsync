package statedb

// APIUsageRow is one api_usage row, or a sum of them.
type APIUsageRow struct {
	Day       string // YYYY-MM-DD, local
	TokenName string
	Endpoint  string
	Result    string
	Requests  int64
	Bytes     int64
}

// AddAPIUsage adds counts to their rows, creating any that don't exist, in
// one transaction: the API's in-memory counters are flushed this way about
// once a minute rather than written per request.
func (db *DB) AddAPIUsage(rows []APIUsageRow) error {
	if len(rows) == 0 {
		return nil
	}
	tx, err := db.conn.Begin()
	if err != nil {
		return err
	}
	for _, r := range rows {
		if _, err := tx.Exec(`INSERT INTO api_usage (day, token_name, endpoint, result, requests, bytes) VALUES (?,?,?,?,?,?)
			 ON CONFLICT(day, token_name, endpoint, result) DO UPDATE SET requests = requests + excluded.requests, bytes = bytes + excluded.bytes`,
			r.Day, r.TokenName, r.Endpoint, r.Result, r.Requests, r.Bytes); err != nil {
			_ = tx.Rollback()
			return err
		}
	}
	return tx.Commit()
}

// APIUsageSince sums the rows from day onwards (YYYY-MM-DD, inclusive) by
// token name, endpoint and result.
func (db *DB) APIUsageSince(day string) ([]APIUsageRow, error) {
	rows, err := db.conn.Query(`SELECT token_name, endpoint, result, SUM(requests), SUM(bytes) FROM api_usage
		 WHERE day >= ? GROUP BY token_name, endpoint, result ORDER BY token_name, endpoint, result`, day)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []APIUsageRow
	for rows.Next() {
		var r APIUsageRow
		if err := rows.Scan(&r.TokenName, &r.Endpoint, &r.Result, &r.Requests, &r.Bytes); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// PruneAPIUsage drops the rows before day (YYYY-MM-DD).
func (db *DB) PruneAPIUsage(before string) error {
	_, err := db.conn.Exec(`DELETE FROM api_usage WHERE day < ?`, before)
	return err
}
