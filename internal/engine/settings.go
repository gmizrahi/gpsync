package engine

import (
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"

	"golang.org/x/crypto/bcrypt"

	"github.com/gmizrahi/gpsync/internal/config"
	"github.com/gmizrahi/gpsync/internal/extensions"
	"github.com/gmizrahi/gpsync/internal/scanner"
)

// ApplyExtensionOverrides installs cfg's extension/ignore-list additions
// (ExtraSupportedPhotoExtensions/ExtraSupportedVideoExtensions/
// ExtraUnsupportedExtensions/ExtraIgnoredFileNames/ExtraIgnoredDirNames)
// into the internal/extensions and internal/scanner packages -- the
// process-global classification tables every scan/upload actually
// consults. `cmd/gpsync` has always called the equivalent of this from its
// PersistentPreRunE; gpsync-tray never did, at startup OR after a Settings
// save, so its own Settings page fields for these five overrides were
// silent no-ops until a code review caught it.
// Both process-global tables are plain package vars, so re-applying them
// takes effect immediately -- no watch-engine restart needed, unlike the
// five dashboard listen/auth fields that DO need one.
func ApplyExtensionOverrides(cfg config.Config) {
	extensions.ApplyUserOverrides(cfg.ExtraSupportedPhotoExtensions, cfg.ExtraSupportedVideoExtensions, cfg.ExtraUnsupportedExtensions)
	scanner.ApplyUserIgnoreOverrides(cfg.ExtraIgnoredFileNames, cfg.ExtraIgnoredDirNames)
}

// ApplySettingsForm parses gpsync-tray's settings page submission into an
// updated Config, starting from current so any field a future, smaller
// form revision stops exposing still survives a save untouched -- a
// settings save must never silently wipe out something the user
// configured by hand in config.toml. Validates as it goes; on the first
// problem found, current is returned unmodified alongside the error, so a
// caller never has to guess how much of a bad submission already "took".
//
// The one field deliberately never exposed here is SortSmallestFirst --
// per its own doc comment in config.Config, it's meant to be a per-run
// --smallest-first flag override, not a standing preference, so there's
// nothing for a persistent settings page to sensibly control.
//
// Deliberately doesn't check that any of the folder-shaped fields
// (source folders, backup/trash/dupes dirs) exist on disk -- the same
// commands that use them already handle a missing folder at the point
// they actually need it (ResolveWatchRoots, `gpsync backup`, etc.); that's a
// runtime concern, not a save-time validation one.
func ApplySettingsForm(current config.Config, values url.Values) (config.Config, error) {
	cfg := current

	cfg.SourceFolders = splitNonEmptyLines(values.Get("source_folders"))

	concurrency, err := strconv.Atoi(strings.TrimSpace(values.Get("concurrency")))
	if err != nil || concurrency < 1 {
		return current, fmt.Errorf("concurrency must be a whole number, 1 or more (got %q)", values.Get("concurrency"))
	}
	cfg.Concurrency = concurrency

	kind, err := extensions.ParseKind(values.Get("media_type_filter"))
	if err != nil {
		return current, err
	}
	cfg.MediaTypeFilter = kind

	debounce, err := strconv.Atoi(strings.TrimSpace(values.Get("watch_debounce_seconds")))
	if err != nil || debounce < 1 {
		return current, fmt.Errorf("debounce must be a whole number of seconds, 1 or more (got %q)", values.Get("watch_debounce_seconds"))
	}
	cfg.WatchDebounceSeconds = debounce

	heartbeatMin, err := strconv.Atoi(strings.TrimSpace(values.Get("watch_heartbeat_minutes")))
	if err != nil || heartbeatMin < 1 {
		return current, fmt.Errorf("heartbeat must be a whole number of minutes, 1 or more (got %q)", values.Get("watch_heartbeat_minutes"))
	}
	cfg.WatchHeartbeatMinutes = heartbeatMin

	quality := strings.TrimSpace(values.Get("upload_quality"))
	if quality != "original" && quality != "space_saver" {
		return current, fmt.Errorf(`upload quality must be "original" or "space_saver" (got %q)`, quality)
	}
	cfg.UploadQuality = quality

	maxDim, err := strconv.Atoi(strings.TrimSpace(values.Get("space_saver_max_dimension")))
	if err != nil || maxDim < 1 {
		return current, fmt.Errorf("space saver max dimension must be a whole number, 1 or more (got %q)", values.Get("space_saver_max_dimension"))
	}
	cfg.SpaceSaverMaxDim = maxDim

	jpegQuality, err := strconv.Atoi(strings.TrimSpace(values.Get("space_saver_jpeg_quality")))
	if err != nil || jpegQuality < 1 || jpegQuality > 100 {
		return current, fmt.Errorf("space saver JPEG quality must be a whole number from 1 to 100 (got %q)", values.Get("space_saver_jpeg_quality"))
	}
	cfg.SpaceSaverJPEGQuality = jpegQuality

	// Backup/trash/dupes dirs: plain strings, empty means unset -- exactly
	// how config.Config already documents them (the commands that use each
	// one refuse to run until it's configured, they never fall back to
	// deleting/guessing).
	cfg.BackupDir = strings.TrimSpace(values.Get("backup_dir"))

	keepCount, err := strconv.Atoi(strings.TrimSpace(values.Get("backup_keep_count")))
	if err != nil || keepCount < 1 {
		return current, fmt.Errorf("backup keep count must be a whole number, 1 or more (got %q)", values.Get("backup_keep_count"))
	}
	cfg.BackupKeepCount = keepCount

	cfg.TrashDir = strings.TrimSpace(values.Get("trash_dir"))
	cfg.DupesDir = strings.TrimSpace(values.Get("dupes_dir"))

	cfg.ExtraSupportedPhotoExtensions = splitNonEmptyLines(values.Get("extra_supported_photo_extensions"))
	cfg.ExtraSupportedVideoExtensions = splitNonEmptyLines(values.Get("extra_supported_video_extensions"))
	cfg.ExtraUnsupportedExtensions = splitNonEmptyLines(values.Get("extra_unsupported_extensions"))
	cfg.ExtraIgnoredFileNames = splitNonEmptyLines(values.Get("extra_ignored_file_names"))
	cfg.ExtraIgnoredDirNames = splitNonEmptyLines(values.Get("extra_ignored_dir_names"))

	theme := strings.TrimSpace(values.Get("theme"))
	switch theme {
	case config.ThemeSystem, config.ThemeLight, config.ThemeDark:
		cfg.Theme = theme
	default:
		return current, fmt.Errorf("theme must be %q, %q, or %q (got %q)", config.ThemeSystem, config.ThemeLight, config.ThemeDark, theme)
	}

	syncStrategy := strings.TrimSpace(values.Get("sync_strategy"))
	switch syncStrategy {
	case config.SyncStrategyFolderByFolder, config.SyncStrategySmallestFirst, config.SyncStrategyPhotosFirst:
		cfg.SyncStrategy = syncStrategy
	default:
		return current, fmt.Errorf("sync strategy must be %q, %q, or %q (got %q)",
			config.SyncStrategyFolderByFolder, config.SyncStrategySmallestFirst, config.SyncStrategyPhotosFirst, syncStrategy)
	}

	// Dashboard listen address, port and auth credentials.
	// None of this takes effect until the next gpsync-tray restart -- see
	// config.Config's own doc comment on these fields for why.
	listenAddr := strings.TrimSpace(values.Get("dashboard_listen_addr"))
	if net.ParseIP(listenAddr) == nil {
		return current, fmt.Errorf("dashboard listen address must be a valid IP (e.g. %q for this device only, %q for the local network) (got %q)",
			config.DashboardListenLocal, config.DashboardListenAll, listenAddr)
	}
	cfg.DashboardListenAddr = listenAddr

	portStr := strings.TrimSpace(values.Get("dashboard_port"))
	port, err := strconv.Atoi(portStr)
	if err != nil || port < 0 || port > 65535 {
		return current, fmt.Errorf("dashboard port must be a whole number from 0 (auto-assign) to 65535 (got %q)", portStr)
	}
	cfg.DashboardPort = port

	cfg.DashboardAuthEnabled = values.Get("dashboard_auth_enabled") != ""
	cfg.DashboardAuthUser = strings.TrimSpace(values.Get("dashboard_auth_user"))
	// Write-only, like every password field: a blank submission means
	// "leave the stored hash alone", never re-derived from what's
	// already there. Only a genuinely non-empty new password gets
	// hashed and replaces it.
	if pw := values.Get("dashboard_auth_password"); pw != "" {
		hash, herr := bcrypt.GenerateFromPassword([]byte(pw), bcrypt.DefaultCost)
		if herr != nil {
			return current, fmt.Errorf("hashing dashboard password: %w", herr)
		}
		cfg.DashboardAuthPassHash = string(hash)
	}
	if cfg.DashboardAuthEnabled && (cfg.DashboardAuthUser == "" || cfg.DashboardAuthPassHash == "") {
		return current, fmt.Errorf("enabling dashboard auth requires both a username and a password")
	}
	// The dashboard is not a read-only status page -- it edits settings,
	// browses the whole library, moves files resolving duplicates, and
	// runs backup/restore. Exposing that past this machine without a
	// login hands all of it to anything on the network, so the two
	// settings are refused as a pair rather than silently accepted.
	// config.Load has a matching backstop for a hand-edited config.toml;
	// this is the one that gives a person a sentence they can act on.
	if !cfg.DashboardAuthEnabled && config.BindNeedsAuth(cfg.DashboardListenAddr) {
		return current, fmt.Errorf("listening on %s exposes the dashboard to the network, so it requires authentication: "+
			"either turn on dashboard auth (with a username and password) or set the listen address to %s for this device only",
			cfg.DashboardListenAddr, config.DashboardListenLocal)
	}

	// Dashboard TLS. An unrecognised mode is named rather than quietly
	// treated as "off" -- serving plain HTTP to someone who believes they
	// switched HTTPS on is the worst outcome available here.
	tlsMode := strings.TrimSpace(values.Get("dashboard_tls_mode"))
	if tlsMode == "" {
		tlsMode = config.TLSModeOff
	}
	if !config.TLSModeValid(tlsMode) {
		return current, fmt.Errorf("dashboard TLS mode must be %q, %q, or %q (got %q)",
			config.TLSModeOff, config.TLSModeSelfSigned, config.TLSModeFiles, tlsMode)
	}
	cfg.DashboardTLSMode = tlsMode
	cfg.DashboardTLSCertFile = strings.TrimSpace(values.Get("dashboard_tls_cert_file"))
	cfg.DashboardTLSKeyFile = strings.TrimSpace(values.Get("dashboard_tls_key_file"))

	switch cfg.DashboardTLSMode {
	case config.TLSModeFiles:
		// Half a pair cannot work: gpsync would have to invent the missing
		// half, and every handshake would fail looking like a corrupt file
		// rather than the configuration mistake it is.
		if cfg.DashboardTLSCertFile == "" || cfg.DashboardTLSKeyFile == "" {
			return current, fmt.Errorf("TLS mode %q needs both a certificate file and a key file", config.TLSModeFiles)
		}
	}

	return cfg, nil
}

// splitNonEmptyLines is the one-path-per-line parsing every list-shaped
// textarea on the settings form uses (source folders, each extension
// override list): trimmed, blank lines dropped.
func splitNonEmptyLines(s string) []string {
	var out []string
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			out = append(out, line)
		}
	}
	return out
}
