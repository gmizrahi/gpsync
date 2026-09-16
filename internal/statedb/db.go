package statedb

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

func getStateDir() string {
	if v := os.Getenv("GPSYNC_STATE_DIR"); v != "" {
		return v
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ".gpsync"
	}
	return filepath.Join(home, ".gpsync")
}

var StateDir = getStateDir()

var StateDBPath = filepath.Join(StateDir, "state.sqlite")

type DB struct {
	conn *sql.DB
}

func Open() (*DB, error) {
	if err := os.MkdirAll(StateDir, 0o700); err != nil {
		return nil, err
	}
	// busy_timeout matters across PROCESSES, where SetMaxOpenConns(1) below
	// can't help: a scheduled `gpsync backup` and an interactive `gpsync sync`
	// are two separate gpsync.exe instances holding two separate SQLite
	// handles on the same file, and without a busy timeout the second one
	// to want the write lock fails instantly with "database is locked"
	// instead of simply waiting the moment out. 10s is far longer than any
	// write this program makes.
	conn, err := sql.Open("sqlite", StateDBPath+"?_pragma=busy_timeout(10000)")
	if err != nil {
		return nil, err
	}
	// SQLite only ever allows one writer at a time; database/sql's default
	// connection pool can open several physical connections to the same
	// file, and a second one trying to read or write while another holds
	// the lock gets an immediate "database is locked" error with no
	// busy_timeout set -- observed in practice as uploads getting misread
	// as quota-exhausted (Exhausted() below treats any read error as
	// exhaustion) under real upload concurrency, nothing to do with quota
	// at all. Capping to a single connection routes every query through
	// Go's own connection-pool queue instead, which never errors this way.
	conn.SetMaxOpenConns(1)
	// Must run before the schema below: it only CREATEs files_seen IF NOT
	// EXISTS, so it won't touch an already-existing table from an older gpsync
	// version that lacks the case-insensitive collation.
	if err := migrateFilesSeenCaseInsensitive(conn); err != nil {
		conn.Close()
		return nil, fmt.Errorf("migrating files_seen to case-insensitive paths: %w", err)
	}
	if _, err := conn.Exec(schema); err != nil {
		conn.Close()
		return nil, fmt.Errorf("applying schema: %w", err)
	}
	// Must run AFTER the schema: on a brand-new database the CREATE above
	// already includes these columns, and this no-ops.
	if err := migrateUploadsVerified(conn); err != nil {
		return nil, err
	}
	if err := migrateDropAlbumTables(conn); err != nil {
		conn.Close()
		return nil, fmt.Errorf("dropping album tables: %w", err)
	}
	if err := migrateRunProgressEndReason(conn); err != nil {
		conn.Close()
		return nil, fmt.Errorf("migrating run_progress end-reason columns: %w", err)
	}
	return &DB{conn: conn}, nil
}

func (db *DB) Close() error { return db.conn.Close() }

func now() float64 { return float64(time.Now().UnixNano()) / 1e9 }

// BackupTo writes a fully consistent snapshot of the database to destPath
// using SQLite's own VACUUM INTO, rather than a raw file copy: it goes
// through the connection's normal locking instead of touching the on-disk
// file directly, so it can never capture a half-written page, even with a
// write in flight. destPath must not already exist -- VACUUM INTO refuses
// to overwrite.
//
// Concurrency, precisely: within one process, Open's SetMaxOpenConns(1)
// serializes this behind any other query. Across two gpsync PROCESSES (a
// scheduled `gpsync backup` while an interactive `gpsync sync` is running), this
// now blocks up to the connection's 10s busy_timeout waiting for the other
// process to release its lock, instead of failing immediately with
// "database is locked". That covers any realistic gpsync write, but it is a
// timeout, not a guarantee: a genuinely stuck writer still surfaces as a
// lock error rather than hanging forever.
func (db *DB) BackupTo(destPath string) error {
	if err := os.Remove(destPath); err != nil && !os.IsNotExist(err) {
		return err
	}
	_, err := db.conn.Exec(`VACUUM INTO ?`, destPath)
	return err
}

// ── files_seen ───────────────────────────────────────────────────────────
