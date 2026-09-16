package main

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/gmizrahi/gpsync/internal/config"
	"github.com/gmizrahi/gpsync/internal/engine"
	"github.com/gmizrahi/gpsync/internal/quota"
	"github.com/gmizrahi/gpsync/internal/statedb"
	"github.com/gmizrahi/gpsync/internal/uploader"
	"github.com/gmizrahi/gpsync/internal/watcher"
	"github.com/spf13/cobra"
)

// runWatchCycle scans+uploads one folder and reports the outcome, but
// NEVER returns an error -- gpsync watch has to keep running regardless of
// what happens to any single folder (a disk hiccup, a network share
// dropping briefly, a throttle/quota abort), unlike gpsync sync/upload's
// one-shot commands where a real error correctly stops the whole
// invocation for a human to look at right away. An abort (throttle
// schedule exhausted, daily quota hit) isn't logged as an error here: it's
// expected, already reported by the dashboard/onThrottle machinery inside
// syncOneFolder, and the heartbeat ticker in watchCmd is what picks the
// folder back up later -- see the design note in .ai/ROADMAP.md's Phase 2
// section on why that's simpler and safer than teaching Uploader.Run()
// itself to keep cycling forever.
func runWatchCycle(db *statedb.DB, cfg config.Config, folder string, verbose bool, conc *quota.AdaptiveConcurrency, breaker *uploader.CircuitBreakerState) {
	_, _, _, _, err := syncOneFolder(db, cfg, folder, verbose, nil, conc, breaker)
	if err != nil && !errors.Is(err, uploader.ErrRunAborted) {
		fmt.Printf("  %s %v\n", colErr("error:"), err)
	}
	fmt.Println()
}

func runHeartbeat(db *statedb.DB, cfg config.Config, patterns []string, verbose bool, conc *quota.AdaptiveConcurrency, breaker *uploader.CircuitBreakerState) {
	pending, err := engine.HeartbeatFolders(db, patterns)
	if err != nil {
		printDBError("heartbeat: listing pending folders", err)
		return
	}
	if len(pending) == 0 {
		return
	}
	fmt.Printf("%s %d folder(s) with pending work\n", colHeader("Heartbeat:"), len(pending))
	for _, f := range pending {
		runWatchCycle(db, cfg, f.Path, verbose, conc, breaker)
	}
}

func watchCmd() *cobra.Command {
	var concurrencyFlag int
	var verboseFlag bool
	var debounceFlag time.Duration
	var heartbeatFlag time.Duration
	cmd := &cobra.Command{
		Use:   "watch [folders...]",
		Short: "Sync continuously as files change",
		Long: "Uploads changes as they happen, and catches up on existing folders in the background, until stopped with Ctrl+C. Uses the configured source folders if none are given. New subfolders are watched automatically.\n" +
			"\n" +
			"A changed folder is processed once it has been quiet for --debounce. Every --heartbeat, pending files are retried, which also recovers from throttling and the daily quota.\n" +
			"\n" +
			"Examples:\n" +
			"  gpsync watch\n" +
			"  gpsync watch --heartbeat 30m",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load()
			if err != nil {
				return err
			}
			if concurrencyFlag > 0 {
				cfg.Concurrency = concurrencyFlag
			}
			patterns := args
			if len(patterns) == 0 {
				patterns = cfg.SourceFolders
			}
			if len(patterns) == 0 {
				return fmt.Errorf("no folders given and none configured — run `gpsync setup` or pass folders explicitly")
			}

			db, err := statedb.Open()
			if err != nil {
				return err
			}
			defer db.Close()
			// Ctrl+C at any point -- during the baseline pass or anywhere
			// in the watch loop below -- records whatever cycle is
			// currently running as interrupted and exits. The same handler
			// every other command already uses: it fires from its own
			// goroutine, so it works correctly even though this command's
			// own main loop never returns on its own.
			defer handleInterrupts(db)()

			// conc/breaker are constructed once for this whole invocation
			// and shared across every folder-cycle (baseline, live, and
			// heartbeat alike) -- for gpsync watch even more than gpsync sync,
			// since a backlog drain can cross MANY folder boundaries in
			// rapid succession while a real throttle is still ongoing.
			// Without sharing breaker specifically, each new folder's
			// Uploader.Run() call started a brand-new circuit breaker at
			// rung 0 and got throttled again almost immediately -- visible
			// as "retrying every 5s forever" instead of actually climbing
			// the backoff ladder. See CircuitBreakerState's doc comment.
			conc := quota.NewAdaptiveConcurrency(cfg.Concurrency)
			breaker := uploader.NewCircuitBreakerState()

			// ── start watching FIRST ──
			// A large pending backlog can take a long time to churn
			// through folder by folder; starting the watcher before that
			// backlog pass means a file dropped in today is noticed and
			// debounced immediately, not only after every other folder in
			// the library has already been caught up.
			roots, err := engine.ResolveWatchRoots(patterns)
			if err != nil {
				return err
			}
			if len(roots) == 0 {
				return fmt.Errorf("none of the given folder(s) exist: %s", strings.Join(patterns, ", "))
			}
			w, err := watcher.New(roots, debounceFlag)
			if err != nil {
				return fmt.Errorf("starting the filesystem watcher: %w", err)
			}
			defer w.Close()

			fmt.Println(colDim(strings.Repeat("─", separatorWidth)))
			fmt.Printf("%s %d folder tree(s), debounce %s, heartbeat every %s. Ctrl+C to stop.\n",
				colHeader("Watching:"), len(roots), debounceFlag, heartbeatFlag)

			heartbeat := time.NewTicker(heartbeatFlag)
			defer heartbeat.Stop()

			// backlog is the one-time baseline catch-up queue, worked
			// through one folder per idle loop iteration -- interleaved
			// with live changes, not run to completion first. Dispatch
			// still never overlaps (this whole loop is single-threaded:
			// backlog work and live work take turns, never run at once),
			// which is what keeps the circuit breaker's "a throttle pauses
			// the WHOLE pipeline" guarantee intact -- that guarantee is
			// per Uploader.Run() call, so two concurrent calls (one for
			// backlog, one for a live change) would each have their own
			// breaker state and neither would know the other just got
			// throttled.
			backlog, err := engine.DiscoverSyncUnits(patterns)
			if err != nil {
				return err
			}
			if len(backlog) > 0 {
				fmt.Printf("%s %d folder(s), worked through in the background -- live changes always go first.\n",
					colHeader("Backlog:"), len(backlog))
			}

			engine.RunWatchLoop(
				func(folder string) {
					fmt.Printf("%s %s\n", colHeader("Changed:"), folder)
					runWatchCycle(db, cfg, folder, verboseFlag, conc, breaker)
				},
				func(folder string, remaining int) {
					fmt.Printf("%s %s\n", colHeader(fmt.Sprintf("Backlog (%d left):", remaining)), folder)
					runWatchCycle(db, cfg, folder, verboseFlag, conc, breaker)
					if remaining == 0 {
						fmt.Println(colOK("Backlog catch-up complete."))
						resolveMissingFiles(db, cfg, false)
					}
				},
				func() {
					runHeartbeat(db, cfg, patterns, verboseFlag, conc, breaker)
					resolveMissingFiles(db, cfg, true)
				},
				func(werr error) { fmt.Printf("  %s %v\n", colWarn("watch:"), werr) },
				backlog, w.Ready(), heartbeat.C, w.Errors(), nil,
			)
			return nil
		},
	}
	cmd.Flags().IntVar(&concurrencyFlag, "concurrency", 0, "Parallel uploads (overrides config)")
	cmd.Flags().BoolVar(&verboseFlag, "verbose", false, "Show per-file progress and status")
	cmd.Flags().DurationVar(&debounceFlag, "debounce", 8*time.Second, "Wait this long after a folder's last change")
	cmd.Flags().DurationVar(&heartbeatFlag, "heartbeat", 15*time.Minute, "How often to retry pending files")
	return cmd
}
