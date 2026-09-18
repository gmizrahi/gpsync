package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/gmizrahi/gpsync/internal/auth"
	"github.com/gmizrahi/gpsync/internal/config"
	"github.com/spf13/cobra"
)

func setupCmd() *cobra.Command {
	var checkOnly, gcloudScript bool
	cmd := &cobra.Command{
		Use:   "setup",
		Short: "Connect a Google account and configure gpsync",
		Long: "Walks through creating Google API credentials and signing in, then sets the source folder and upload defaults.\n" +
			"\n" +
			"Google requires every user to create their own credentials; there is no shared gpsync app to sign into. If you already use rclone with Google Photos, an existing remote can be imported instead, which skips the Google Cloud console entirely.\n" +
			"\n" +
			"--check verifies an existing installation and changes nothing. --gcloud-script prints the part of the Google Cloud setup that can be scripted; the OAuth consent screen and client creation have no API and stay manual.\n" +
			"\n" +
			"Examples:\n" +
			"  gpsync setup\n" +
			"  gpsync setup --check\n" +
			"  gpsync setup --gcloud-script > setup-gcp.sh",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			switch {
			case checkOnly && gcloudScript:
				return fmt.Errorf("--check and --gcloud-script do different things; pass one")
			case gcloudScript:
				printGcloudScript()
				return nil
			case checkOnly:
				return runSetupCheck()
			default:
				return runSetup()
			}
		},
	}
	cmd.Flags().BoolVar(&checkOnly, "check", false, "Report whether this installation can upload, and change nothing")
	cmd.Flags().BoolVar(&gcloudScript, "gcloud-script", false, "Print a gcloud script for the scriptable part of Google Cloud setup")
	return cmd
}

func runSetup() error {
	fmt.Println("gpsync setup — let's get you connected to Google Photos.")
	fmt.Println()

	if confPath, ok := auth.FindRcloneConf(); ok && !auth.HasCredentials() {
		remotes, err := auth.ListGooglePhotosRemotes(confPath)
		if err == nil && len(remotes) > 0 {
			fmt.Printf("Found an existing rclone Google Photos remote at %s.\n", confPath)
			fmt.Println("Importing it reuses your existing OAuth app, skipping the GCP console setup below.")
			fmt.Println()
			if confirmPrompt("Import it now instead of creating a new GCP project?", true) {
				return runImportRclone("", "")
			}
		}
	}

	fmt.Println("No existing rclone credential to import — setting up a dedicated GCP project.")
	fmt.Println()
	fmt.Println("Manual steps (Google doesn't expose OAuth-client creation via API):")
	fmt.Println("1. Open the OAuth consent screen page and configure it (External, add your own email as a test user):")
	fmt.Println("   https://console.cloud.google.com/apis/credentials/consent")
	fmt.Println("2. Open Credentials and create an OAuth client ID of type 'Desktop app':")
	fmt.Println("   https://console.cloud.google.com/apis/credentials")
	fmt.Println("3. Download the resulting client_secret_*.json file.")
	fmt.Println()
	if confirmPrompt("Open these two pages in your browser now?", true) {
		auth.OpenBrowser("https://console.cloud.google.com/apis/credentials/consent")
		auth.OpenBrowser("https://console.cloud.google.com/apis/credentials")
	}

	path := promptString("Path to the downloaded client_secret_*.json file", "")
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("file not found: %s", path)
	}
	cs, err := auth.ParseClientSecretJSON(data)
	if err != nil {
		return err
	}
	if err := auth.SaveClientSecret(cs); err != nil {
		return err
	}
	fmt.Printf("Saved credentials to %s\n", auth.ClientSecretPath)

	fmt.Println()
	fmt.Println("Opening browser to complete the consent flow...")
	if _, err := auth.GetHTTPClient(context.Background()); err != nil {
		return fmt.Errorf("authentication failed: %w", err)
	}
	fmt.Println("Authenticated successfully.")
	fmt.Println()

	return configureDefaults()
}

func configureDefaults() error {
	defaultFolder := "/mnt/c/Photos"
	if _, err := os.Stat(defaultFolder); err != nil {
		defaultFolder = `C:\Photos`
	}
	folder := promptString("Default source folder to sync", defaultFolder)
	defaults := config.Defaults()
	concurrency := promptInt("Default upload concurrency", defaults.Concurrency)
	quality := promptChoice("Upload quality", []string{"original", "space_saver"}, "original")

	cfg := defaults
	cfg.SourceFolders = []string{folder}
	cfg.Concurrency = concurrency
	cfg.UploadQuality = quality
	if err := config.Save(cfg); err != nil {
		return err
	}
	fmt.Println()
	fmt.Println("Config saved. Run `gpsync sync <folder>` to get started.")
	return nil
}

// ── import-rclone ──────────────────────────────────────────────────────

func importRcloneCmd() *cobra.Command {
	var confFlag, remoteFlag string
	cmd := &cobra.Command{
		Use:   "import-rclone",
		Short: "Import an existing rclone Google Photos remote's OAuth credentials",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runImportRclone(confFlag, remoteFlag)
		},
	}
	cmd.Flags().StringVar(&confFlag, "conf", "", "Path to rclone.conf (auto-detected if omitted)")
	cmd.Flags().StringVar(&remoteFlag, "remote", "", "Remote name to import (prompted if omitted and multiple exist)")
	return cmd
}

func runImportRclone(confPath, remote string) error {
	if confPath == "" {
		p, ok := auth.FindRcloneConf()
		if !ok {
			return fmt.Errorf("no rclone.conf found — pass --conf explicitly")
		}
		confPath = p
	}
	remotes, err := auth.ListGooglePhotosRemotes(confPath)
	if err != nil {
		return err
	}
	if len(remotes) == 0 {
		return fmt.Errorf("no Google Photos remotes found in %s", confPath)
	}
	if remote == "" {
		if len(remotes) == 1 {
			remote = remotes[0]
		} else {
			fmt.Printf("Found remotes: %s\n", strings.Join(remotes, ", "))
			remote = promptChoice("Which remote?", remotes, remotes[0])
		}
	}

	fmt.Printf("About to copy the OAuth client_id/client_secret and refresh token for remote %q\n", remote)
	fmt.Printf("from %s into:\n", confPath)
	fmt.Printf("  %s\n", auth.ClientSecretPath)
	fmt.Printf("  %s\n", auth.TokenPath)
	if !confirmPrompt("Continue?", true) {
		fmt.Println("Cancelled — nothing was written.")
		return nil
	}

	msg, err := auth.ImportRemote(confPath, remote)
	if err != nil {
		return err
	}
	fmt.Println(msg)

	fmt.Println()
	fmt.Println("Imported:")
	fmt.Printf("  Remote:     %s (from %s)\n", remote, confPath)
	if clientID, ok := auth.CurrentClientID(); ok {
		fmt.Printf("  Client ID:  %s\n", clientID)
	}
	fmt.Printf("  Token:      saved to %s (refresh token present, not shown)\n", auth.TokenPath)
	fmt.Println()

	return configureDefaults()
}

// ── config ─────────────────────────────────────────────────────────────

func configCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "config",
		Short: "Show current settings and auth status",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runShowConfig()
		},
	}
}

func runShowConfig() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	if _, err := os.Stat(config.ConfigPath); err == nil {
		fmt.Printf("Config file: %s\n", config.ConfigPath)
	} else {
		fmt.Printf("Config file: %s (not created yet — showing defaults; run `gpsync setup` or `gpsync import-rclone`)\n", config.ConfigPath)
	}
	fmt.Println()
	fmt.Println("Settings:")
	if len(cfg.SourceFolders) == 0 {
		fmt.Println("  Source folders:   (none configured)")
	} else {
		fmt.Printf("  Source folders:   %s\n", strings.Join(cfg.SourceFolders, ", "))
	}
	fmt.Printf("  Concurrency:      %d\n", cfg.Concurrency)
	fmt.Printf("  Upload quality:   %s\n", cfg.UploadQuality)
	if cfg.UploadQuality == "space_saver" {
		fmt.Printf("    max dimension:  %d px\n", cfg.SpaceSaverMaxDim)
		fmt.Printf("    JPEG quality:   %d\n", cfg.SpaceSaverJPEGQuality)
	}
	if cfg.BackupDir != "" {
		fmt.Printf("  Backup folder:    %s (keeping %d)\n", cfg.BackupDir, cfg.BackupKeepCount)
	} else {
		fmt.Println("  Backup folder:    (none configured -- run `gpsync backup --dest <folder>`)")
	}

	if len(cfg.ExtraSupportedPhotoExtensions) > 0 || len(cfg.ExtraSupportedVideoExtensions) > 0 ||
		len(cfg.ExtraUnsupportedExtensions) > 0 || len(cfg.ExtraIgnoredFileNames) > 0 || len(cfg.ExtraIgnoredDirNames) > 0 {
		fmt.Println()
		fmt.Println("Extension overrides (config.toml, on top of the built-in tables):")
		if len(cfg.ExtraSupportedPhotoExtensions) > 0 {
			fmt.Printf("  Extra supported (photo): %s\n", strings.Join(cfg.ExtraSupportedPhotoExtensions, ", "))
		}
		if len(cfg.ExtraSupportedVideoExtensions) > 0 {
			fmt.Printf("  Extra supported (video): %s\n", strings.Join(cfg.ExtraSupportedVideoExtensions, ", "))
		}
		if len(cfg.ExtraUnsupportedExtensions) > 0 {
			fmt.Printf("  Extra unsupported:       %s\n", strings.Join(cfg.ExtraUnsupportedExtensions, ", "))
		}
		if len(cfg.ExtraIgnoredFileNames) > 0 {
			fmt.Printf("  Extra ignored files:     %s\n", strings.Join(cfg.ExtraIgnoredFileNames, ", "))
		}
		if len(cfg.ExtraIgnoredDirNames) > 0 {
			fmt.Printf("  Extra ignored dirs:      %s\n", strings.Join(cfg.ExtraIgnoredDirNames, ", "))
		}
	}

	fmt.Println()
	fmt.Println("Auth:")
	if auth.HasCredentials() {
		fmt.Printf("  Client secret:    %s (present)\n", auth.ClientSecretPath)
		if clientID, ok := auth.CurrentClientID(); ok {
			fmt.Printf("  Client ID:        %s\n", clientID)
		}
	} else {
		fmt.Printf("  Client secret:    %s (missing — run `gpsync setup` or `gpsync import-rclone`)\n", auth.ClientSecretPath)
	}
	if auth.HasToken() {
		fmt.Printf("  Token:            %s (present)\n", auth.TokenPath)
	} else {
		fmt.Printf("  Token:            %s (missing — will trigger the consent flow on first use)\n", auth.TokenPath)
	}

	return nil
}

// ── sync ───────────────────────────────────────────────────────────────

func hasCredentials(files []string) bool {
	for _, f := range files {
		if f == "client_secret.json" || f == "token.json" {
			return true
		}
	}
	return false
}
