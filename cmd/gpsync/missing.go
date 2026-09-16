package main

import (
	"fmt"
	"strings"
	"time"

	"github.com/gmizrahi/gpsync/internal/config"
	"github.com/gmizrahi/gpsync/internal/engine"
	"github.com/gmizrahi/gpsync/internal/statedb"
)

// runHeartbeat re-checks the pending backlog scoped to patterns and
// retries whatever's still there, regardless of why (an aborted run, a
// crash, anything) -- see runWatchCycle's doc comment for why this is the
// entire mechanism gpsync watch has for recovering from a throttle/quota
// give-up, on purpose.
func resolveMissingFiles(db *statedb.DB, cfg config.Config, onlyIfUndecided bool) {
	res, ran, err := engine.ResolveMissingIfNeeded(db, cfg.SourceFolders, onlyIfUndecided, time.Now())
	switch {
	case err != nil:
		printDBError("checking missing files", err)
		return
	case !ran || (!res.Changed() && !res.Skipped):
		return
	case res.Skipped:
		fmt.Printf("%s not checked: %s\n", colWarn("Missing files:"), res.SkipReason)
		return
	}
	var parts []string
	if res.Confirmed > 0 {
		parts = append(parts, colWarn(fmt.Sprintf("%d confirmed missing", res.Confirmed)))
	}
	if res.Relocated > 0 {
		parts = append(parts, fmt.Sprintf("%d moved (ledger updated)", res.Relocated))
	}
	if res.Reappeared > 0 {
		parts = append(parts, fmt.Sprintf("%d back in place", res.Reappeared))
	}
	fmt.Printf("%s %s\n", colHeader("Missing files:"), strings.Join(parts, ", "))
	if res.Confirmed > 0 {
		fmt.Println("  Run `gpsync recheck --missing` to review and forget them.")
	}
}
