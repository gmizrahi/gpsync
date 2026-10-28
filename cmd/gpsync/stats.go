package main

import (
	"fmt"
	"strconv"
	"time"

	"github.com/gmizrahi/gpsync/internal/config"
	"github.com/gmizrahi/gpsync/internal/engine"
	"github.com/gmizrahi/gpsync/internal/statedb"
	"github.com/gmizrahi/gpsync/internal/units"
	"github.com/spf13/cobra"
)

func statsCmd() *cobra.Command {
	var apiUsage bool
	var apiDays int
	cmd := &cobra.Command{
		Use:   "stats",
		Short: "Summarise the library: totals, media types, extensions, years",
		Long: "Prints what the dashboard's Statistics page shows: totals, a breakdown by media type and extension, and capture years.\n" +
			"\n" +
			"Reads the ledger only; it does not touch Google or walk your folders.\n" +
			"\n" +
			"With --api, prints the Statistics page's API usage instead: requests by token, what was served, refusals and the thumbnail cache. A running tray saves its counts about once a minute, so the last minute may not show yet.\n" +
			"\n" +
			"Examples:\n" +
			"  gpsync stats\n" +
			"  gpsync stats --api --days 7",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if apiUsage && engine.ParseAPIUsagePeriod(strconv.Itoa(apiDays)) != apiDays {
				return fmt.Errorf("--days must be 7, 30 or 90 (got %d)", apiDays)
			}
			db, err := statedb.Open()
			if err != nil {
				return err
			}
			defer db.Close()

			if apiUsage {
				cfg, err := config.Load()
				if err != nil {
					return err
				}
				r, hasTokens, err := engine.BuildAPIUsageReport(db, cfg, apiDays, time.Now())
				if err != nil {
					return err
				}
				printAPIUsage(r, hasTokens, time.Now())
				return nil
			}

			s, err := engine.BuildStatistics(db)
			if err != nil {
				return err
			}
			printStats(s)
			return nil
		},
	}
	cmd.Flags().BoolVar(&apiUsage, "api", false, "Show API usage instead of library statistics")
	cmd.Flags().IntVar(&apiDays, "days", 30, "With --api: the period, 7, 30 or 90 days")
	return cmd
}

func printAPIUsage(r engine.APIUsageReport, hasTokens bool, now time.Time) {
	fmt.Println(colHeader(fmt.Sprintf("API usage, last %d days", r.Days)))
	if !hasTokens {
		fmt.Println(colDim("No API tokens. Create one with `gpsync token create --name NAME`."))
		return
	}
	last := "never"
	if !r.LastRequest.IsZero() {
		last = units.Relative(r.LastRequest, now)
	}
	fmt.Printf("  Requests:      %d\n", r.Requests)
	fmt.Printf("  Served:        %s\n", units.Bytes(r.Bytes))
	fmt.Printf("  Refused:       %d\n", r.Refused)
	fmt.Printf("  Last request:  %s\n", last)

	fmt.Println()
	fmt.Println(colHeader("By token"))
	for _, t := range r.Tokens {
		used, expires := "never", "never"
		if !t.LastUsed.IsZero() {
			used = units.Relative(t.LastUsed, now)
		}
		switch {
		case !t.Stored:
			used, expires = "-", "-"
		case !t.ExpiresAt.IsZero() && !t.ExpiresAt.After(now):
			expires = "expired"
		case !t.ExpiresAt.IsZero():
			expires = units.Relative(t.ExpiresAt, now)
		}
		fmt.Printf("  %-20s %8d request(s)   last used %-14s expires %s\n", t.Name, t.Requests, used, expires)
	}

	fmt.Println()
	fmt.Println(colHeader("What was served"))
	if len(r.Served) == 0 {
		fmt.Println(colDim("  Nothing served in this period."))
	}
	for _, s := range r.Served {
		label := s.Label
		if s.Sub {
			label = "  " + label
		}
		size := units.Bytes(s.Bytes)
		if s.Bytes == 0 {
			size = "-"
		}
		fmt.Printf("  %-32s %8d   %10s\n", label, s.Requests, size)
	}

	fmt.Println()
	fmt.Println(colHeader("Thumbnail cache"))
	if r.CacheErr != nil {
		fmt.Printf("  Thumbnail folder setting is invalid: %v\n", r.CacheErr)
	} else {
		fmt.Printf("  %s: %s of %s\n", r.CacheDir, units.Bytes(r.CacheBytes), units.Bytes(r.CacheMax))
	}

	if len(r.Refusals) > 0 {
		fmt.Println()
		fmt.Println(colHeader("Refused"))
		for _, f := range r.Refusals {
			fmt.Printf("  %-56s %8d\n", f.Label(), f.Requests)
		}
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
