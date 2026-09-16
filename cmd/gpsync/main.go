// Command gpsync is a robust Google Photos sync engine: hash-based dedup,
// retry and quota awareness, replacing an ad-hoc rclone
// workflow.
package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/gmizrahi/gpsync/internal/config"
	"github.com/gmizrahi/gpsync/internal/engine"
	"github.com/gmizrahi/gpsync/internal/version"
)

// Version is the gpsync release version, shown by `gpsync --version` and in the
// help banner so it's always obvious which build you're running -- shared
// with gpsync-tray via internal/version, since neither binary can import
// the other.
var Version = version.Version

func main() {
	root := rootCmd()
	// A path ending in a backslash, run from Windows PowerShell 5.1, arrives
	// mangled -- see repairWindowsArgs. Only engages when an argument
	// contains a double quote, which no Windows path can.
	if fixed, ok := repairWindowsArgs(os.Args[1:], rawCommandLine()); ok {
		root.SetArgs(fixed)
	}
	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, colErr("Error:"), err)
		os.Exit(1)
	}
}

func rootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:     "gpsync",
		Version: Version,
		Short:   fmt.Sprintf("gpsync v%s — a robust Google Photos sync engine", Version),
		Long: "Syncs local photo and video folders to Google Photos.\n" +
			"\n" +
			"Files are identified by their content, so each one is uploaded once, however often it is renamed, moved or copied. Uploads stay within Google's daily quota and back off when throttled.\n" +
			"\n" +
			"Get started:\n" +
			"  gpsync setup    Create credentials and sign in\n" +
			"  gpsync sync     Scan and upload the configured source folders\n" +
			"  gpsync info     See what is synced, pending and failed",
		SilenceUsage:  true,
		SilenceErrors: true,
		// Installs the user's extension/ignore-list overrides from
		// config.toml (extra_supported_*/extra_unsupported_extensions/
		// extra_ignored_*) before ANY subcommand runs, so every command
		// classifies extensions the same way without each one having to
		// remember to load config and wire this up itself -- see
		// applyExtensionOverrides. Harmless/no-op when config.toml doesn't
		// exist yet or has none of these keys set (config.Load() returns
		// Defaults() with nil slices in that case).
		//
		// Also prints a one-line note (to stderr, so it never pollutes
		// piped/scripted stdout) when gpsync-tray's dashboard is currently
		// reachable, so a command run alongside the tray says so rather
		// than appearing to act alone. Applies to every command uniformly,
		// not just `gpsync info`.
		PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
			if err := applyExtensionOverrides(); err != nil {
				return err
			}
			if cfg, err := config.Load(); err == nil {
				if notice := trayRunningNotice(cfg); notice != "" {
					fmt.Fprintln(os.Stderr, colDim(notice))
				}
			}
			return nil
		},
	}
	root.SetVersionTemplate("gpsync version {{.Version}}\n")
	root.AddCommand(
		setupCmd(),
		importRcloneCmd(),
		configCmd(),
		syncCmd(),
		watchCmd(),
		dashboardCmd(),
		scanCmd(),
		uploadCmd(),
		markSyncedCmd(),
		qualityCmd(),
		infoCmd(),
		pendingCmd(),
		whatisCmd(),
		recheckCmd(),
		fixDatesCmd(),
		doctorCmd(),
		trayQuitCmd(),
		verifyCmd(),
		reuploadCmd(),
		cleanOriginalsCmd(),
		originalsUploadedCmd(),
		precheckCmd(),
		logCmd(),
		extensionsCmd(),
		duplicatesCmd(),
		quotaCmd(),
		throttleLogCmd(),
		backupCmd(),
		restoreCmd(),
		completionCmd(),
	)
	useShorthandFirstFlagOrder(root)
	return root
}

// applyExtensionOverrides loads config.toml and installs the user's own
// extension/ignore-list additions on top of the built-in tables in
// internal/extensions and internal/scanner (engine.ApplyExtensionOverrides
// -- shared with gpsync-tray, which needs the identical installation at
// startup and after a Settings save). This is what lets someone add an
// extension to extra_unsupported_extensions (or the ignore lists) in
// config.toml themselves, instead of needing a code change every time they
// hit one gpsync doesn't already know about.
func applyExtensionOverrides() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	engine.ApplyExtensionOverrides(cfg)
	return nil
}
