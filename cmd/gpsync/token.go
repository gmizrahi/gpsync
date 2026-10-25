package main

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/gmizrahi/gpsync/internal/config"
	"github.com/gmizrahi/gpsync/internal/engine"
	"github.com/gmizrahi/gpsync/internal/statedb"
)

// tokenCmd manages API tokens (issue #67): the same create, list and revoke
// Settings offers, so the API is fully usable without the dashboard.
func tokenCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "token",
		Short: "Create, list and revoke API tokens",
		Long: "API tokens let a program on this machine call gpsync's HTTP API (/api/v1) with\n" +
			"\"Authorization: Bearer <token>\". A token is shown once, when it's created: gpsync keeps\n" +
			"only a hash of it. A program can rotate its own token before it expires, so it never\n" +
			"needs a new one pasted in.",
		Args: cobra.NoArgs,
	}
	cmd.AddCommand(tokenCreateCmd(), tokenListCmd(), tokenRevokeCmd())
	return cmd
}

func tokenCreateCmd() *cobra.Command {
	var name, expires, scopes string
	cmd := &cobra.Command{
		Use:   "create --name NAME",
		Short: "Create an API token and print it, once",
		Long: "Prints the new token -- and only the token -- on stdout, so it can go straight into a\n" +
			"file:  gpsync token create --name photos-app > token.txt\n" +
			"Without --expires the token lives for api_token_lifetime (90d unless changed in Settings).",
		Example: "  gpsync token create --name photos-app\n  gpsync token create --name script --expires 12h\n  gpsync token create --name kiosk --expires never --scope read",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load()
			if err != nil {
				return err
			}
			if expires == "" {
				expires = cfg.APITokenLifetime
			}
			lifetime, expiring, err := engine.ParseAPITokenLifetime(expires)
			if err != nil {
				return err
			}
			scopeList, err := engine.ParseScopes(scopes)
			if err != nil {
				return err
			}
			db, err := statedb.Open()
			if err != nil {
				return err
			}
			defer db.Close()
			token, err := engine.CreateAPIToken(db, name, scopeList, lifetime, expiring, time.Now())
			if errors.Is(err, statedb.ErrAPITokenNameTaken) {
				return fmt.Errorf("a token named %q already exists -- revoke it first, or pick another name", name)
			}
			if err != nil {
				return err
			}
			fmt.Fprintf(os.Stderr, "Created token %q (scopes: %s, expires: %s). It is shown only now -- store it somewhere safe:\n",
				name, strings.Join(scopeList, " "), expires)
			fmt.Println(token)
			return nil
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "a name for the token, unique; used to list and revoke it")
	cmd.Flags().StringVar(&expires, "expires", "", `lifetime, like 90d, 12h or never (default: the api_token_lifetime setting)`)
	cmd.Flags().StringVar(&scopes, "scope", engine.ScopeRead, "what the token may do: read, control, or both (read,control)")
	_ = cmd.MarkFlagRequired("name")
	return cmd
}

func tokenListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List API tokens (never the tokens themselves)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			db, err := statedb.Open()
			if err != nil {
				return err
			}
			defer db.Close()
			tokens, err := db.ListAPITokens()
			if err != nil {
				return err
			}
			if len(tokens) == 0 {
				fmt.Println("No API tokens. Create one with: gpsync token create --name NAME")
				return nil
			}
			now := float64(time.Now().Unix())
			when := func(v float64) string { return time.Unix(int64(v), 0).Format("2006-01-02 15:04") }
			tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "NAME\tSCOPES\tCREATED\tLAST USED\tEXPIRES")
			for _, t := range tokens {
				used, exp := "never", "never"
				if t.LastUsedAt.Valid {
					used = when(t.LastUsedAt.Float64)
				}
				if t.ExpiresAt.Valid {
					exp = when(t.ExpiresAt.Float64)
					if t.ExpiresAt.Float64 <= now {
						exp += " (expired)"
					}
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", t.Name, t.Scopes, when(t.CreatedAt), used, exp)
			}
			return tw.Flush()
		},
	}
}

func tokenRevokeCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "revoke NAME",
		Short: "Revoke an API token; anything using it is refused at once",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			db, err := statedb.Open()
			if err != nil {
				return err
			}
			defer db.Close()
			if err := engine.RevokeAPIToken(db, args[0]); errors.Is(err, engine.ErrNoSuchAPIToken) {
				return fmt.Errorf("no API token is named %q (see gpsync token list)", args[0])
			} else if err != nil {
				return err
			}
			fmt.Printf("Revoked token %q. Anything still using it is now refused.\n", args[0])
			return nil
		},
	}
}
