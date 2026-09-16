package main

import (
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/gmizrahi/gpsync/internal/quota"
	"github.com/gmizrahi/gpsync/internal/statedb"
	"github.com/gmizrahi/gpsync/internal/uploader"
)

// printLastRunOutcome reports how the previous run ended, but ONLY when
// that's worth saying: a run that completed normally is the expected case
// and printing it every time would just be noise to scroll past. An
// abandoned or interrupted run is different -- it means work is outstanding
// for a reason the user may need to act on, and until now the only trace of
// it was files quietly sitting in the ledger.
func printLastRunOutcome(run *statedb.RunProgress) {
	if run == nil || run.EndReason == "" || run.EndReason == statedb.EndReasonCompleted {
		return
	}
	label, colorize := "stopped", colWarn
	switch run.EndReason {
	case statedb.EndReasonAbortedThrottle:
		label, colorize = "aborted (throttle)", colErr
	case statedb.EndReasonAbortedDailyQuota:
		label, colorize = "aborted (daily quota)", colErr
	case statedb.EndReasonInterrupted:
		label, colorize = "interrupted", colWarn
	case statedb.EndReasonAbortedNoEligibleFiles:
		label, colorize = "no eligible files", colWarn
	}
	when := time.Unix(int64(run.UpdatedAt), 0).Format("2006-01-02 15:04:05")
	fmt.Printf("%s %s — %s\n", colHeader("Last run:"), colorize(label), colDim(when))
	if run.EndDetail != "" {
		fmt.Printf("  %s\n", run.EndDetail)
	}
}

// markRunInterrupted records that a run was cut short by a signal. Split
// out from the handler so the DB write is testable without real signal
// delivery.
//
// It only annotates a run that is actually in progress: a Ctrl+C at an idle
// moment (or during a command that never called RunStart) must not rewrite
// the outcome of whatever ran last.
func markRunInterrupted(db *statedb.DB) {
	run, err := db.RunGet()
	if err != nil || run == nil || run.Status != "running" {
		return
	}
	detail := fmt.Sprintf("stopped by Ctrl+C after %d of %d file(s)", run.FilesDone, run.FilesTotal)
	_ = db.RunEnd(statedb.EndReasonInterrupted, detail)
}

// interruptExitCode is the conventional 128+SIGINT.
const interruptExitCode = 130

// handleInterrupts records an interrupted run in the ledger before the
// process dies, and returns a func that uninstalls the handler.
//
// Without this, Ctrl+C left run_progress stuck at status='running' forever
// with nothing recorded about why -- indistinguishable, later, from a run
// still going or one that crashed. It deliberately does NOT try to drain
// in-flight uploads first: anything already sent is either committed to the
// ledger or still pending, both of which the next run resumes from
// correctly, so a fast exit loses nothing. The single DB write just queues
// behind whatever is in flight (statedb caps the pool at one connection),
// and completes before the exit.
func handleInterrupts(db *statedb.DB) func() {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM)
	done := make(chan struct{})
	go func() {
		select {
		case <-ch:
			markRunInterrupted(db)
			fmt.Printf("\n%s recorded in the ledger — re-run the same command to continue where this left off.\n",
				colWarn("Interrupted:"))
			os.Exit(interruptExitCode)
		case <-done:
		}
	}()
	return func() {
		signal.Stop(ch)
		close(done)
	}
}

// printRunAborted explains a deliberately-stopped run. This is expected
// behavior, not a crash, so it never goes through main()'s "Error: ..."
// path: the user gets a plain account of what happened, what was left, and
// the fact that simply re-running picks it all back up.
func printRunAborted(err error, foldersDone, foldersTotal int) {
	fmt.Println()
	fmt.Println(colDim(strings.Repeat("─", separatorWidth)))

	var abort *uploader.AbortError
	if errors.As(err, &abort) {
		switch abort.Reason {
		case uploader.AbortDailyQuota:
			fmt.Printf("%s %s\n", colErr("Stopped: daily API quota exhausted."), abort.Detail)
			fmt.Printf("  Quota resets in ~%.1fh (midnight Pacific Time).\n", quota.SecondsUntilPacificMidnight()/3600)
		default:
			fmt.Println(colErr("Stopped: Google is still rate-limiting uploads."))
			fmt.Printf("  Paused and retried through all %d backoff steps (%s in total) and it never cleared.\n",
				abort.Steps, abort.Waited.Round(time.Second))
			if abort.Detail != "" {
				fmt.Printf("  Last response: %s\n", colDim(singleLine(abort.Detail, 100)))
			}
		}
		if abort.Remaining > 0 {
			fmt.Printf("  %s file(s) in the current folder were left unfinished.\n",
				colWarn(fmt.Sprintf("%d", abort.Remaining)))
		}
	} else {
		fmt.Printf("%s %v\n", colErr("Stopped:"), err)
	}

	if remaining := foldersTotal - foldersDone; remaining > 0 {
		fmt.Printf("  %s\n", colWarn(fmt.Sprintf("Skipping the remaining %d folder(s) — they would hit the same limit.", remaining)))
	}
	fmt.Printf("  %s\n", colDim("Nothing is lost: everything unfinished is still queued in the ledger. Re-run the same command later to continue."))
}

// ── watch ──────────────────────────────────────────────────────────────
