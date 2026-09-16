package main

import (
	"fmt"

	"github.com/gmizrahi/gpsync/internal/statedb"
	"github.com/spf13/cobra"
)

func duplicatesCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "duplicates",
		Short: "List files with the same content at more than one path",
		Long: "Lists groups of tracked files with identical content, largest reclaimable space first. Google Photos holds one copy either way; this helps you clean up local copies.\n" +
			"\n" +
			"Example:\n" +
			"  gpsync duplicates resolve    Choose which copy to keep, group by group",
		RunE: func(cmd *cobra.Command, args []string) error {
			db, err := statedb.Open()
			if err != nil {
				return err
			}
			defer db.Close()

			groups, err := db.DuplicateGroups()
			if err != nil {
				return err
			}
			if len(groups) == 0 {
				fmt.Println("No duplicate content found.")
				return nil
			}

			var totalReclaimable int64
			var totalExtraFiles int
			for i, g := range groups {
				extra := len(g.Paths) - 1
				reclaimable := g.Size * int64(extra)
				totalReclaimable += reclaimable
				totalExtraFiles += extra
				fmt.Printf("%s %s each  •  %d copies  •  %s reclaimable\n",
					colHeader(fmt.Sprintf("[%d]", i+1)), humanBytes(g.Size), len(g.Paths), colWarn(humanBytes(reclaimable)))
				for _, p := range g.Paths {
					fmt.Printf("    %s\n", p)
				}
			}

			fmt.Println()
			fmt.Printf("%d duplicate group(s), %d redundant file(s), %s reclaimable if you keep one copy of each.\n",
				len(groups), totalExtraFiles, colWarn(humanBytes(totalReclaimable)))
			fmt.Printf("%s\n", colDim("Run `gpsync duplicates resolve` to choose which copies to keep and move the rest to a trash folder."))
			return nil
		},
	}
	cmd.AddCommand(duplicatesResolveCmd())
	return cmd
}

// ── quota ──────────────────────────────────────────────────────────────
