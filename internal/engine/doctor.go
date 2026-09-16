package engine

import (
	"fmt"
	"time"

	"github.com/gmizrahi/gpsync/internal/scanner"
	"github.com/gmizrahi/gpsync/internal/statedb"
)

// stuckRunAfter is how long a run_progress row may sit at status='running'
// without an update before Diagnose calls it stale. run_progress is a
// singleton row rewritten in place, and nothing clears it if the process
// dies between writes -- a Ctrl+C, a crash, or a kill leaves it claiming a
// run is still going forever, which then shows on the dashboard as a
// permanently "Active run".
//
// Generous on purpose: a legitimately active run updates this row often,
// but a long circuit-breaker pause (the ladder tops out at a 1h rung) is
// silent by design, so anything tighter than that would flag a perfectly
// healthy throttled run as broken.
const stuckRunAfter = 3 * time.Hour

// Doctor check IDs. Stable strings rather than an enum so a report can
// name a check in output, and so a --fix filter could target one later.
const (
	CheckStuckRun          = "stuck-run"
	CheckRetryableBacklog  = "retryable-backlog"
	CheckMissingSourceFile = "missing-source-file"
	CheckOriginalsReview   = "originals-auto-resolvable"
	CheckPermanentFailures = "permanent-failures"
)

// Finding is one problem (or one clean bill of health) from Diagnose.
type Finding struct {
	Check string
	// Count is how many rows/items the check matched. Zero means healthy.
	Count int
	// Summary is a one-line human description of what was found.
	Summary string
	// Remedy explains what --fix will do, or -- when Fixable is false --
	// what the user should do instead. Always populated when Count > 0,
	// because a report that names a problem without saying what to do
	// about it just creates anxiety.
	Remedy string
	// Fixable marks the checks Repair can act on. Deliberately false for
	// anything ambiguous: the ones left alone need either a human
	// decision or a command with its own richer options.
	Fixable bool
}

// Report is the whole diagnosis, healthy checks included.
type Report struct {
	Findings []Finding
	Checked  time.Time
}

// Problems returns only the findings that actually matched something.
func (r Report) Problems() []Finding {
	var out []Finding
	for _, f := range r.Findings {
		if f.Count > 0 {
			out = append(out, f)
		}
	}
	return out
}

// Healthy reports whether every check came back clean.
func (r Report) Healthy() bool { return len(r.Problems()) == 0 }

// Diagnose runs every ledger invariant check. Read-only -- it never
// modifies anything, so it is always safe to run against a live ledger
// while the tray is uploading.
//
// The checks are drawn from bugs this project actually hit, each of which
// previously needed its own bespoke one-off fix: a run_progress row stuck
// at "running" after a Ctrl+C, a failed_retryable backlog that nothing
// would ever sweep back (which silently idled the app for over an hour),
// pending rows for files that have since been deleted or moved, and
// originals-review items that became auto-resolvable once their sibling
// was scanned.
func Diagnose(db *statedb.DB, now time.Time) (Report, error) {
	rep := Report{Checked: now}

	run, err := db.RunGet()
	if err != nil {
		return rep, err
	}
	stuck := Finding{Check: CheckStuckRun, Summary: "no stale run row"}
	if run != nil && run.Status == "running" {
		idle := now.Sub(time.Unix(int64(run.UpdatedAt), 0))
		if idle > stuckRunAfter {
			stuck.Count = 1
			stuck.Summary = fmt.Sprintf("a %q run has been marked active with no update for %s", run.RunType, roundDuration(idle))
			stuck.Remedy = "close it out as interrupted, so it stops showing as an active run"
			stuck.Fixable = true
		} else {
			stuck.Summary = fmt.Sprintf("a %q run is active (updated %s ago)", run.RunType, roundDuration(idle))
		}
	}
	rep.Findings = append(rep.Findings, stuck)

	counts, err := db.CountsByStatus()
	if err != nil {
		return rep, err
	}

	retryable := Finding{Check: CheckRetryableBacklog, Summary: "no files waiting in the retry bucket"}
	if n := counts["failed_retryable"]; n > 0 {
		retryable.Count = n
		retryable.Summary = fmt.Sprintf("%d file(s) sitting in failed_retryable", n)
		retryable.Remedy = "requeue them as pending (this also happens automatically at the start of every upload run)"
		retryable.Fixable = true
	}
	rep.Findings = append(rep.Findings, retryable)

	// Files gone from disk are flagged by scans and confirmed by
	// ResolveMissingFiles, which already rules out moves and unavailable
	// drives -- doctor only reports the result. Forgetting them is
	// gpsync recheck --missing, which re-checks each file first.
	mc, err := db.MissingCounts()
	if err != nil {
		return rep, err
	}
	missingFinding := Finding{Check: CheckMissingSourceFile, Summary: "no files confirmed missing from disk"}
	if mc.Confirmed > 0 {
		missingFinding.Count = mc.Confirmed
		missingFinding.Summary = fmt.Sprintf("%d file(s) confirmed missing from disk", mc.Confirmed)
		missingFinding.Remedy = "run `gpsync recheck --missing` to forget them"
	}
	rep.Findings = append(rep.Findings, missingFinding)

	// AutoResolveObviousOriginals is the same sweep the Originals page and
	// `gpsync info` already run at display time; counting it here tells
	// the user whether anything is sitting resolvable without them having
	// opened either.
	resolvable, err := countAutoResolvableOriginals(db)
	if err != nil {
		return rep, err
	}
	orig := Finding{Check: CheckOriginalsReview, Summary: "no originals-review items are auto-resolvable"}
	if resolvable > 0 {
		orig.Count = resolvable
		orig.Summary = fmt.Sprintf("%d originals-review item(s) can be resolved automatically", resolvable)
		orig.Remedy = "resolve them (each has an edited sibling right beside it, so the outcome isn't a judgement call)"
		orig.Fixable = true
	}
	rep.Findings = append(rep.Findings, orig)

	// Not fixable here: `gpsync recheck` judges these one at a time.
	permFinding := Finding{Check: CheckPermanentFailures, Summary: "no permanent failures"}
	if n := counts["failed_permanent"]; n > 0 {
		permFinding.Count = n
		permFinding.Summary = fmt.Sprintf("%d file(s) marked permanently failed", n)
		permFinding.Remedy = "run `gpsync recheck` to see which can be retried (--retry) or forgotten (--unsupported)"
	}
	rep.Findings = append(rep.Findings, permFinding)

	return rep, nil
}

// Repair applies the fixable findings from a fresh diagnosis and returns
// the report describing what it acted on, with each fixed Finding's Count
// set to how many items were actually repaired.
//
// Re-diagnoses rather than taking a Report parameter on purpose: a report
// can be minutes old by the time a human reads it and types --fix, and
// acting on a stale one risks "fixing" something a running upload already
// resolved.
func Repair(db *statedb.DB, now time.Time) (Report, error) {
	rep, err := Diagnose(db, now)
	if err != nil {
		return rep, err
	}
	for i, f := range rep.Findings {
		if f.Count == 0 || !f.Fixable {
			continue
		}
		switch f.Check {
		case CheckStuckRun:
			if err := db.RunEnd(statedb.EndReasonInterrupted, "closed out by gpsync doctor: no progress recorded for over "+roundDuration(stuckRunAfter)); err != nil {
				return rep, err
			}
		case CheckRetryableBacklog:
			n, err := db.RequeueRetryable()
			if err != nil {
				return rep, err
			}
			rep.Findings[i].Count = int(n)
		case CheckOriginalsReview:
			sum, err := AutoResolveObviousOriginals(db)
			if err != nil {
				return rep, err
			}
			rep.Findings[i].Count = sum.IgnoredCount
		}
	}
	return rep, nil
}

// countAutoResolvableOriginals asks how many needs_review items WOULD be
// auto-resolved, without resolving them -- the same immediate-parent
// sibling test AutoResolveObviousOriginals uses, so the count Diagnose
// reports and the number Repair acts on can't disagree.
func countAutoResolvableOriginals(db *statedb.DB) (int, error) {
	items, err := db.NeedsReviewItems()
	if err != nil {
		return 0, err
	}
	var n int
	for _, it := range items {
		seen, err := db.GetFileSeen(scanner.SiblingPath(it.FirstSourcePath))
		if err != nil {
			return 0, err
		}
		if seen != nil {
			n++
		}
	}
	return n, nil
}

func roundDuration(d time.Duration) string {
	switch {
	case d >= time.Hour:
		return fmt.Sprintf("%.0fh", d.Hours())
	case d >= time.Minute:
		return fmt.Sprintf("%.0fm", d.Minutes())
	default:
		return fmt.Sprintf("%.0fs", d.Seconds())
	}
}
