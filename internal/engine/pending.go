package engine

import (
	"path/filepath"
	"sort"

	"github.com/gmizrahi/gpsync/internal/statedb"
)

// PendingFolder is one folder's share of the pending backlog, derived
// purely from the ledger.
type PendingFolder struct {
	Path  string
	Files int
	Bytes int64
	// Rows are this folder's pending entries, in the order
	// ListPendingUnder returned them (ORDER BY first_source_path), which is
	// already the order `gpsync pending` wants to print. Only that command
	// reads them; the upload loop needs just the counts.
	Rows []statedb.Upload
}

// PendingFolders groups everything currently pending (optionally scoped to
// the given folder prefixes) by containing folder, sorted by path. Unlike
// `gpsync sync`'s estimateBatchTotals this touches no disk at all and is not
// an estimate: ListPendingUnder already knows exactly which files are
// outstanding and how big each one is.
func PendingFolders(db *statedb.DB, scope []string) ([]PendingFolder, error) {
	rows, err := db.ListPendingUnder(scope)
	if err != nil {
		return nil, err
	}
	byFolder := map[string]*PendingFolder{}
	for _, row := range rows {
		dir := filepath.Dir(row.FirstSourcePath)
		f, ok := byFolder[dir]
		if !ok {
			f = &PendingFolder{Path: dir}
			byFolder[dir] = f
		}
		f.Files++
		f.Bytes += row.Size
		f.Rows = append(f.Rows, row)
	}
	out := make([]PendingFolder, 0, len(byFolder))
	for _, f := range byFolder {
		out = append(out, *f)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

// HeartbeatFolders is the folder list a watch heartbeat retries: every
// folder with pending work, after first putting failed_retryable files back
// to pending.
//
// Without the requeue a heartbeat could never recover from a throttle
// give-up. Aborting marks every remaining file failed_retryable, and
// PendingFolders only lists 'pending', so the heartbeat found nothing to do
// and never started the cycle that would requeue them: 103 files sat
// retryable indefinitely after a give-up at the end of the backoff
// ladder, until the tray was restarted.
func HeartbeatFolders(db *statedb.DB, scope []string) ([]PendingFolder, error) {
	if _, err := db.RequeueRetryable(); err != nil {
		return nil, err
	}
	return PendingFolders(db, scope)
}
