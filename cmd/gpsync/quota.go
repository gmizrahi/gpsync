package main

import (
	"fmt"
	"strconv"

	"github.com/gmizrahi/gpsync/internal/quota"
	"github.com/gmizrahi/gpsync/internal/statedb"
	"github.com/spf13/cobra"
)

func quotaCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "quota [set <n> | add <n>]",
		Short: "Show or correct today's API request count",
		Long: "Shows today's API requests against the limit of 10,000 per day. Use set or add to count requests gpsync did not make, such as rclone using the same credentials.\n" +
			"\n" +
			"Examples:\n" +
			"  gpsync quota\n" +
			"  gpsync quota add 250\n" +
			"  gpsync quota set 4000",
		Args: cobra.MaximumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			db, err := statedb.Open()
			if err != nil {
				return err
			}
			defer db.Close()
			dq := quota.NewDailyQuota(db)

			if len(args) == 2 {
				n, err := strconv.Atoi(args[1])
				if err != nil {
					return fmt.Errorf("invalid number %q: %w", args[1], err)
				}
				if n < 0 {
					return fmt.Errorf("quota usage can't be negative")
				}
				switch args[0] {
				case "set":
					if err := dq.Set(n); err != nil {
						return err
					}
				case "add":
					if err := dq.Record(n); err != nil {
						return err
					}
				default:
					return fmt.Errorf("unknown subcommand %q — use 'set' or 'add'", args[0])
				}
			} else if len(args) != 0 {
				return fmt.Errorf("usage: gpsync quota [set <n> | add <n>]")
			}

			used, err := dq.Used()
			if err != nil {
				return err
			}
			fmt.Printf("Quota used today (%s, Pacific time): %s/%d\n",
				quota.PacificTodayStr(), colOK(fmt.Sprintf("%d", used)), dq.DailyLimit())
			return nil
		},
	}
}
