package main

import (
	"fmt"

	"github.com/gmizrahi/gpsync/internal/engine"
	"github.com/gmizrahi/gpsync/internal/statedb"
	"github.com/gmizrahi/gpsync/internal/units"
	"github.com/spf13/cobra"
)

func statsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "stats",
		Short: "Summarise the library: totals, media types, extensions, years",
		Long: "Prints what the dashboard's Statistics page shows: totals, a breakdown by media type and extension, and capture years.\n" +
			"\n" +
			"Reads the ledger only; it does not touch Google or walk your folders.\n" +
			"\n" +
			"Examples:\n" +
			"  gpsync stats",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			db, err := statedb.Open()
			if err != nil {
				return err
			}
			defer db.Close()

			s, err := engine.BuildStatistics(db)
			if err != nil {
				return err
			}
			printStats(s)
			return nil
		},
	}
}

func printStats(s engine.Statistics) {
	fmt.Println(colHeader("Overview"))
	fmt.Printf("  Files tracked:  %d\n", s.TotalFiles)
	fmt.Printf("  Total size:     %s\n", units.Bytes(s.TotalBytes))
	fmt.Printf("  Average size:   %s\n", units.Bytes(int64(s.AvgBytes)))
	fmt.Printf("  Extensions:     %d\n", s.ExtensionCount)

	if s.TotalFiles == 0 {
		fmt.Println()
		fmt.Println(colDim("Nothing tracked yet — run `gpsync scan` first."))
		return
	}

	fmt.Println()
	fmt.Println(colHeader("By media type"))
	for _, k := range s.ByKind {
		// Uploaded and pending are the two real segments; anything left
		// over is failed_permanent or needs_review, which is neither, so
		// it is reported rather than folded into one of them.
		other := k.Count - k.UploadedCount - k.PendingCount
		line := fmt.Sprintf("  %-8s %6d file(s)  %10s   %d uploaded, %d outstanding",
			k.Label+":", k.Count, units.Bytes(k.Bytes), k.UploadedCount, k.PendingCount)
		if other > 0 {
			line += fmt.Sprintf(", %d neither", other)
		}
		fmt.Println(line)
	}

	fmt.Println()
	fmt.Println(colHeader("By extension — count"))
	printCountStats(s.ByExtCount, false)

	fmt.Println()
	fmt.Println(colHeader("By extension — size"))
	printCountStats(s.ByExtSize, true)

	fmt.Println()
	fmt.Println(colHeader("By capture year"))
	printCountStats(s.ByYear, false)
	fmt.Println(colDim("  Years come from a folder or filename date where the path has one, otherwise the capture date on file."))
}

func printCountStats(rows []engine.CountStat, bySize bool) {
	if len(rows) == 0 {
		fmt.Println(colDim("  (none)"))
		return
	}
	width := 0
	for _, r := range rows {
		if len(r.Label) > width {
			width = len(r.Label)
		}
	}
	for _, r := range rows {
		value := fmt.Sprintf("%d file(s)", r.Count)
		if bySize {
			value = units.Bytes(r.Bytes)
		}
		fmt.Printf("  %-*s  %s\n", width+1, r.Label+":", value)
	}
}
