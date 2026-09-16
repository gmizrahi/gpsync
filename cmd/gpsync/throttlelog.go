package main

import (
	"encoding/csv"
	"fmt"
	"io"
	"strconv"
	"time"

	"github.com/gmizrahi/gpsync/internal/engine"
	"github.com/gmizrahi/gpsync/internal/statedb"
	"github.com/spf13/cobra"
)

func throttleLogCmd() *cobra.Command {
	var asCSV, analyze bool
	cmd := &cobra.Command{
		Use:   "throttle-log",
		Short: "Show when Google throttled uploads and how long recovery took",
		Long: "Lists each time Google throttled uploads: when it happened, the backoff step and wait, how much was uploaded just before, and how long until the next successful upload. Changes nothing.\n" +
			"\n" +
			"Examples:\n" +
			"  gpsync throttle-log\n" +
			"  gpsync throttle-log --analyze\n" +
			"  gpsync throttle-log --csv > throttles.csv",
		RunE: func(cmd *cobra.Command, args []string) error {
			db, err := statedb.Open()
			if err != nil {
				return err
			}
			defer db.Close()

			events, err := engine.BuildThrottleAnalysis(db)
			if err != nil {
				return err
			}
			if len(events) == 0 {
				fmt.Println("No throttle events recorded yet.")
				return nil
			}

			switch {
			case asCSV:
				return writeThrottleLogCSV(cmd.OutOrStdout(), events)
			case analyze:
				printThrottleLogAnalysis(events)
				return nil
			default:
				printThrottleLogReport(events)
				return nil
			}
		},
	}
	cmd.Flags().BoolVar(&asCSV, "csv", false, "Output CSV")
	cmd.Flags().BoolVar(&analyze, "analyze", false, "Recovery times grouped by backoff step (min, max, average)")
	return cmd
}

func fmtThrottleTime(at float64) string {
	return time.Unix(int64(at), 0).Format("2006-01-02 15:04:05")
}

func printThrottleLogReport(events []engine.ThrottleEventAnalysis) {
	var recoveries []time.Duration
	for i, ev := range events {
		fmt.Printf("%s  %s (backoff rung %d, scheduled wait %s)\n",
			colWarn(fmt.Sprintf("Throttle #%d", i+1)), fmtThrottleTime(ev.At), ev.Rung+1,
			time.Duration(ev.WaitSeconds*float64(time.Second)).Round(time.Second))
		fmt.Printf("  preceding clean window: %d file(s), %s\n", ev.CleanWindowFiles, humanBytes(ev.CleanWindowBytes))
		fmt.Printf("  message: %s\n", colDim(ev.Message))

		if !ev.RecoveryKnown {
			fmt.Printf("  recovery: %s\n", colWarn("no successful upload yet since this throttle"))
		} else {
			elapsed := time.Duration(ev.RecoverySeconds * float64(time.Second)).Round(time.Second)
			recoveries = append(recoveries, elapsed)
			fmt.Printf("  recovery: %s -- first successful upload at %s, %s after this throttle was detected\n",
				colOK(elapsed.String()), fmtThrottleTime(ev.RecoveredAt), elapsed)
		}
		fmt.Println()
	}

	if len(recoveries) == 0 {
		return
	}
	min, max, total := recoveries[0], recoveries[0], time.Duration(0)
	for _, r := range recoveries {
		if r < min {
			min = r
		}
		if r > max {
			max = r
		}
		total += r
	}
	avg := total / time.Duration(len(recoveries))
	fmt.Println(colHeader("Recovery time summary (throttle to next successful upload):"))
	fmt.Printf("  %d event(s) with a known recovery, min %s, max %s, avg %s\n",
		len(recoveries), min, max, avg)
}

func writeThrottleLogCSV(out io.Writer, events []engine.ThrottleEventAnalysis) error {
	w := csv.NewWriter(out)
	if err := w.Write([]string{
		"event", "at", "rung", "scheduled_wait_seconds",
		"clean_window_files", "clean_window_bytes", "message", "recovery_seconds",
	}); err != nil {
		return err
	}
	for i, ev := range events {
		recovery := ""
		if ev.RecoveryKnown {
			recovery = strconv.FormatFloat(ev.RecoverySeconds, 'f', 0, 64)
		}
		if err := w.Write([]string{
			strconv.Itoa(i + 1),
			fmtThrottleTime(ev.At),
			strconv.Itoa(ev.Rung),
			strconv.FormatFloat(ev.WaitSeconds, 'f', 0, 64),
			strconv.Itoa(ev.CleanWindowFiles),
			strconv.FormatInt(ev.CleanWindowBytes, 10),
			ev.Message,
			recovery,
		}); err != nil {
			return err
		}
	}
	w.Flush()
	return w.Error()
}

func printThrottleLogAnalysis(events []engine.ThrottleEventAnalysis) {
	stats := engine.RecoveryStatsByRung(events)
	if len(stats) == 0 {
		fmt.Println("No throttle event has a known recovery yet -- nothing to analyze.")
		return
	}
	fmt.Println(colHeader("Recovery time by backoff rung:"))
	for _, s := range stats {
		fmt.Printf("  rung %d: %d event(s), min %s, max %s, avg %s\n",
			s.Rung+1, s.Count,
			time.Duration(s.MinSeconds*float64(time.Second)).Round(time.Second),
			time.Duration(s.MaxSeconds*float64(time.Second)).Round(time.Second),
			time.Duration(s.AvgSeconds*float64(time.Second)).Round(time.Second))
	}
	fmt.Println()
	fmt.Println(colDim("A rung whose avg recovery lands well short of its own scheduled wait is spending time it doesn't need to; one whose avg lands close to or past it may need a longer schedule at that step."))
}

// ── backup / restore ──────────────────────────────────────────────────
