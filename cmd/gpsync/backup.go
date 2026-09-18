package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/gmizrahi/gpsync/internal/backup"
	"github.com/gmizrahi/gpsync/internal/config"
	"github.com/gmizrahi/gpsync/internal/statedb"
	"github.com/spf13/cobra"
)

func backupCmd() *cobra.Command {
	var destFlag string
	var keepFlag int
	cmd := &cobra.Command{
		Use:   "backup",
		Short: "Back up gpsync's ledger, settings and credentials",
		Long: "Creates a timestamped .zip of ~/.gpsync (the ledger, config.toml and the OAuth credentials) and deletes the oldest backups beyond --keep. It backs up gpsync's own data, not your photos.\n" +
			"\n" +
			"The archive contains a working sign-in token for your Google account. Keep it somewhere private.\n" +
			"\n" +
			"--dest and --keep are saved as the new defaults.\n" +
			"\n" +
			"Examples:\n" +
			"  gpsync backup\n" +
			"  gpsync backup --dest \"C:\\gpsync-backups\" --keep 10\n" +
			"  gpsync backup list",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load()
			if err != nil {
				return err
			}
			changed := false
			if destFlag != "" {
				cfg.BackupDir = destFlag
				changed = true
			}
			if cmd.Flags().Changed("keep") {
				cfg.BackupKeepCount = keepFlag
				changed = true
			}
			if changed {
				if err := config.Save(cfg); err != nil {
					return err
				}
			}

			db, err := statedb.Open()
			if err != nil {
				return err
			}
			defer db.Close()

			result, err := backup.Create(db, cfg.BackupDir, cfg.BackupKeepCount)
			if err != nil {
				return err
			}
			info, _ := os.Stat(result.Path)
			var size int64
			if info != nil {
				size = info.Size()
			}
			fmt.Printf("%s %s (%s)\n", colOK("Backed up to"), result.Path, humanBytes(size))
			fmt.Printf("  includes: %s\n", strings.Join(result.Files, ", "))
			if result.Unrestricted {
				// Not a failure: that destination has no per-user
				// permissions to set (a cloud-sync drive, a FAT stick). The
				// archive still holds credentials, so say so rather than
				// letting the usual owner-only guarantee be assumed.
				fmt.Printf("  %s %s has no per-user file permissions, so this archive could not be locked to your account\n",
					colWarn("note:"), cfg.BackupDir)
			}
			if hasCredentials(result.Files) {
				fmt.Printf("  %s this includes your OAuth credentials -- anyone with access to %s can access your Google Photos account\n",
					colWarn("note:"), cfg.BackupDir)
			}

			kept, err := backup.List(cfg.BackupDir)
			if err == nil {
				fmt.Printf("%d backup(s) kept in %s (keep limit: %d)\n", len(kept), cfg.BackupDir, cfg.BackupKeepCount)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&destFlag, "dest", "", "Backup folder (saved as the default)")
	cmd.Flags().IntVar(&keepFlag, "keep", 0, "Number of backups to keep (saved as the default)")
	cmd.AddCommand(backupListCmd())
	return cmd
}

func backupListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list [folder]",
		Short: "List backups in the backup folder, or in the given folder",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load()
			if err != nil {
				return err
			}
			dir := cfg.BackupDir
			if len(args) > 0 {
				dir = args[0]
			}
			if dir == "" {
				return fmt.Errorf("no backup destination configured -- run `gpsync backup --dest <folder>` once, or pass a folder here")
			}
			files, err := backup.List(dir)
			if err != nil {
				return err
			}
			if len(files) == 0 {
				fmt.Printf("No backups found in %s\n", dir)
				return nil
			}
			for i := len(files) - 1; i >= 0; i-- { // newest first
				info, _ := os.Stat(files[i])
				var size int64
				if info != nil {
					size = info.Size()
				}
				fmt.Printf("  %s  %s\n", files[i], colDim(humanBytes(size)))
			}
			return nil
		},
	}
}

func restoreCmd() *cobra.Command {
	var yesFlag bool
	cmd := &cobra.Command{
		Use:   "restore <backup-file>",
		Short: "Restore ~/.gpsync from a backup",
		Long: "Replaces the ledger, settings and credentials in ~/.gpsync with the contents of a backup. The current files are copied next to the backup first.\n" +
			"\n" +
			"Examples:\n" +
			"  gpsync backup list\n" +
			"  gpsync restore <backup-file>",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			path := args[0]
			if _, err := os.Stat(path); err != nil {
				return fmt.Errorf("backup file not found: %s", path)
			}
			if !yesFlag {
				fmt.Printf("This will OVERWRITE everything currently in %s with the contents of:\n  %s\n", statedb.StateDir, path)
				if !confirmPrompt("Continue?", false) {
					fmt.Println("Cancelled -- nothing was changed.")
					return nil
				}
			}
			result, err := backup.Restore(path)
			if err != nil {
				return err
			}
			fmt.Printf("%s %s\n", colOK("Restored from"), path)
			fmt.Printf("  restored: %s\n", strings.Join(result.Files, ", "))
			fmt.Printf("A copy of what was there before is saved at %s (in case you need to undo this)\n", result.PreRestoreDir)
			return nil
		},
	}
	cmd.Flags().BoolVar(&yesFlag, "yes", false, "Don't ask for confirmation")
	return cmd
}
