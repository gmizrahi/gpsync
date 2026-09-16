package statedb

import "database/sql"

type RunProgress struct {
	RunType      string
	Status       string
	FoldersTotal int
	FoldersDone  int
	FilesTotal   int
	FilesDone    int
	StartedAt    float64
	UpdatedAt    float64
	// EndReason is one of the EndReason* constants, or "" for a run that
	// predates this tracking or is still going. EndDetail is the
	// human-readable explanation that goes with it.
	EndReason string
	EndDetail string
}

func (db *DB) RunStart(runType string, foldersTotal, filesTotal int) error {
	t := now()
	_, err := db.conn.Exec(
		// end_reason/end_detail are cleared explicitly: this row is
		// overwritten in place by every run, so without that a new run would
		// carry the PREVIOUS run's abort reason around with it and `gpsync
		// info` would report a stale failure against a run going fine.
		`INSERT INTO run_progress (id, run_type, status, folders_total, folders_done, files_total, files_done, started_at, updated_at, end_reason, end_detail)
		 VALUES (1, ?, 'running', ?, 0, ?, 0, ?, ?, NULL, NULL)
		 ON CONFLICT(id) DO UPDATE SET run_type=excluded.run_type, status='running',
		   folders_total=excluded.folders_total, folders_done=0,
		   files_total=excluded.files_total, files_done=0,
		   started_at=excluded.started_at, updated_at=excluded.updated_at,
		   end_reason=NULL, end_detail=NULL`,
		runType, foldersTotal, filesTotal, t, t,
	)
	return err
}

func (db *DB) RunUpdate(foldersDone, filesDone *int) error {
	if foldersDone == nil && filesDone == nil {
		return nil
	}
	if foldersDone != nil && filesDone != nil {
		_, err := db.conn.Exec(`UPDATE run_progress SET folders_done=?, files_done=?, updated_at=? WHERE id=1`,
			*foldersDone, *filesDone, now())
		return err
	}
	if filesDone != nil {
		_, err := db.conn.Exec(`UPDATE run_progress SET files_done=?, updated_at=? WHERE id=1`, *filesDone, now())
		return err
	}
	_, err := db.conn.Exec(`UPDATE run_progress SET folders_done=?, updated_at=? WHERE id=1`, *foldersDone, now())
	return err
}

// How a run ended. Stored in run_progress.end_reason.
const (
	EndReasonCompleted         = "completed"
	EndReasonAbortedThrottle   = "aborted_throttle"
	EndReasonAbortedDailyQuota = "aborted_daily_quota"
	EndReasonInterrupted       = "interrupted"
	// EndReasonAbortedNoEligibleFiles: every pending file in scope was
	// excluded by the current media-type filter/sync-strategy settings, so
	// nothing in this lap could ever be dispatched -- see
	// uploader.AbortNoEligibleFiles.
	EndReasonAbortedNoEligibleFiles = "aborted_no_eligible_files"
)

// RunFinish marks the run complete. Equivalent to RunEnd(EndReasonCompleted, "").
func (db *DB) RunFinish() error {
	return db.RunEnd(EndReasonCompleted, "")
}

// RunEnd closes out the current run with how it ended and why.
//
// Every exit path should call this, including the ones that stop early:
// without it an aborted run is indistinguishable from a completed one, and
// a killed run leaves status='running' forever with nothing recorded about
// what happened. `gpsync info` reads this back (see RunGet) to tell the user
// whether the last run actually finished.
func (db *DB) RunEnd(reason, detail string) error {
	status := "done"
	if reason != EndReasonCompleted {
		status = "stopped"
	}
	_, err := db.conn.Exec(
		`UPDATE run_progress SET status=?, end_reason=?, end_detail=?, updated_at=? WHERE id=1`,
		status, reason, detail, now(),
	)
	return err
}

func (db *DB) RunGet() (*RunProgress, error) {
	row := db.conn.QueryRow(`SELECT run_type, status, folders_total, folders_done, files_total, files_done, started_at, updated_at, end_reason, end_detail FROM run_progress WHERE id=1`)
	var r RunProgress
	// end_reason/end_detail are NULL both for rows written before this
	// tracking existed and for a run still in progress -- neither is an
	// error, so scan through NullString and leave the fields empty.
	var reason, detail sql.NullString
	err := row.Scan(&r.RunType, &r.Status, &r.FoldersTotal, &r.FoldersDone, &r.FilesTotal, &r.FilesDone, &r.StartedAt, &r.UpdatedAt, &reason, &detail)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	r.EndReason, r.EndDetail = reason.String, detail.String
	return &r, err
}

// PruneExpiredSessions deletes every already-expired session row --
// ValidateSession already prunes lazily on lookup, so this is just a
// startup-time sweep (called once from gpsync-tray's onReady) to keep
// dashboard_sessions from accumulating rows for sessions that expired
// while the app wasn't running to notice.
func (db *DB) PruneExpiredSessions() error {
	_, err := db.conn.Exec(`DELETE FROM dashboard_sessions WHERE expires_at < ?`, now())
	return err
}
