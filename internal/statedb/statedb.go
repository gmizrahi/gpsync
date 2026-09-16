// Package statedb is the SQLite ledger — the authoritative record of what's
// been uploaded, what failed and why, and live progress for in-flight
// scan/upload runs.
//
// Identity for dedup is content hash (sha256), not path: the same photo
// under any filename or location is recognized as already uploaded. This is
// what lets gpsync answer "was this file already synced?" reliably.
package statedb

import (
	_ "modernc.org/sqlite"
)
