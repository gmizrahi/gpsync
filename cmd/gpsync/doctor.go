package main

import (
	"fmt"
	"time"

	"github.com/gmizrahi/gpsync/internal/engine"
	"github.com/gmizrahi/gpsync/internal/statedb"
	"github.com/spf13/cobra"
)

func doctorCmd() *cobra.Command {
	var fix bool
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Check the ledger for problems and repair the safe ones",
		Long: "Checks for:\n" +
			"  - a run still marked active after a crash\n" +
			"  - files waiting in the retry queue\n" +
			"  - files confirmed missing from disk\n" +
			"  - originals-review items that can be resolved automatically\n" +
			"  - permanent failures\n" +
			"  - queued files whose path now holds different content, because it was edited\n" +
			"\n" +
			"Without --fix it only reports, and is safe to run at any time. --fix repairs what needs no decision; for everything else the report names the command to use.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			db, err := statedb.Open()
			if err != nil {
				return err
			}
			defer db.Close()

			report := engine.Diagnose
			if fix {
				report = engine.Repair
			}
			rep, err := report(db, time.Now())
			if err != nil {
				return err
			}

			fmt.Println(colHeader("Ledger check:"))
			problems := rep.Problems()
			if len(problems) == 0 {
				fmt.Printf("  %s no problems found (%d checks)\n", colOK("✓"), len(rep.Findings))
				return nil
			}
			for _, f := range problems {
				mark := colWarn("!")
				if !f.Fixable {
					mark = colDim("·")
				}
				fmt.Printf("  %s %s\n", mark, f.Summary)
				if f.Remedy != "" {
					if fix && f.Fixable {
						fmt.Printf("      %s\n", colOK("fixed — "+f.Remedy))
					} else {
						fmt.Printf("      %s\n", colDim(f.Remedy))
					}
				}
			}
			if !fix {
				fixable := 0
				for _, f := range problems {
					if f.Fixable {
						fixable++
					}
				}
				if fixable > 0 {
					fmt.Printf("\n  %s\n", colDim(fmt.Sprintf("Re-run with --fix to repair %d of these automatically.", fixable)))
				}
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&fix, "fix", false, "Repair the problems that need no decision")
	return cmd
}
