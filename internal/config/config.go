// Package config manages ~/.gpsync/config.toml — non-secret settings. Secrets
// live in client_secret.json and token.json (see internal/auth), never here.
package config

import (
	"net"
	"os"
	"path/filepath"
	"strings"

	"github.com/pelletier/go-toml/v2"

	"github.com/gmizrahi/gpsync/internal/extensions"
	"github.com/gmizrahi/gpsync/internal/statedb"
)

var ConfigPath = filepath.Join(statedb.StateDir, "config.toml")

type Config struct {
	SourceFolders         []string `toml:"source_folders"`
	Concurrency           int      `toml:"concurrency"`
	UploadQuality         string   `toml:"upload_quality"` // "original" | "space_saver"
	SpaceSaverMaxDim      int      `toml:"space_saver_max_dimension"`
	SpaceSaverJPEGQuality int      `toml:"space_saver_jpeg_quality"`
	// BackupDir/BackupKeepCount configure `gpsync backup` -- see internal/backup.
	// A backup archives EVERYTHING in ~/.gpsync, OAuth credentials
	// (client_secret.json, token.json) included -- they are live bearer
	// credentials for the user's Google Photos library. Choose the
	// destination accordingly: a synced cloud folder (e.g. Google Drive)
	// means those credentials get synced too. `gpsync backup`/`gpsync restore`
	// always print exactly which files were included, so this is never a
	// silent surprise.
	BackupDir       string `toml:"backup_dir"`
	BackupKeepCount int    `toml:"backup_keep_count"`
	// TrashDir is where `gpsync duplicates resolve` moves the copies you
	// choose not to keep. Flat by filename, not mirroring their original
	// path underneath it. Empty means unset, and that command refuses to
	// run until one is configured -- it never falls back to deleting.
	//
	// Pick somewhere OUTSIDE any scanned source folder: files under a
	// source folder would be picked up by the next scan and uploaded again.
	TrashDir string `toml:"trash_dir"`
	// DupesDir is where `gpsync precheck` moves files it finds already backed
	// up (matched by content hash against the ledger) out of a folder
	// you're about to copy into your library -- e.g. a phone-dump folder
	// that's mostly-but-not-entirely new photos. Separate from TrashDir
	// deliberately: these are triage candidates from an external folder,
	// not confirmed-unwanted copies already inside the library. Empty
	// means unset, and that command refuses to run until one is configured.
	DupesDir string `toml:"dupes_dir"`
	// SortSmallestFirst orders `gpsync upload`'s dispatch queue by ascending
	// file size instead of the default path order -- set per-run via
	// `--smallest-first`, in-memory only like the other flag overrides in
	// uploadCmd (Concurrency/UploadQuality), so this normally
	// stays false here regardless of what was passed on any given run.
	SortSmallestFirst bool `toml:"sort_smallest_first"`
	// MediaTypeFilter restricts scan/upload/sync to one media kind --
	// KindUnknown (empty string in config.toml) means no filtering, the
	// default. Set per-run via `--media-type photos|videos|all` on
	// scan/upload/sync, in-memory only like SortSmallestFirst above (not
	// meant to persist as a lasting preference, since "just photos this
	// time" is a per-run choice, not a standing one).
	MediaTypeFilter extensions.Kind `toml:"media_type_filter"`
	// ExtraSupported*/ExtraUnsupportedExtensions/ExtraIgnored* let you
	// extend gpsync's built-in extension/junk-file tables yourself, without
	// needing a code change for every extension your library happens to
	// contain that the built-in tables don't cover. Applied on top of the
	// built-in tables (internal/extensions, internal/scanner) at the start
	// of every command -- see applyExtensionOverrides in cmd/gpsync/main.go.
	// Extensions are matched case-insensitively, with or without a leading
	// dot ("PPT", "ppt", ".ppt" are all the same entry).
	//
	// ExtraSupportedPhotoExtensions/ExtraSupportedVideoExtensions force an
	// extension to be treated as a supported photo/video, attempted for
	// upload instead of gated -- also what makes it match `--media-type
	// photos`/`--media-type videos` filtering, since that filter goes by
	// the SAME kind tag. ExtraUnsupportedExtensions is the opposite: gate
	// an extension out before ever hashing it, same as the built-in
	// bmp/pdf/ppt/etc. table. A user entry always wins over a conflicting
	// built-in classification for the same extension.
	//
	// ExtraIgnoredFileNames/ExtraIgnoredDirNames extend the OS/app-junk
	// skip list (built-in: Thumbs.db, desktop.ini, .DS_Store, Picasa's
	// .picasaoriginals folder) with your own exact file/directory names to
	// skip entirely during a scan.
	//
	// Adding an extension to any of these lists automatically cleans up any
	// 'pending'/'failed_permanent' ledger rows it now matches, the next
	// time `gpsync scan`/`gpsync upload`/`gpsync sync` runs -- see
	// autoCleanExcluded in cmd/gpsync/main.go. No separate `gpsync recheck` step
	// needed.
	ExtraSupportedPhotoExtensions []string `toml:"extra_supported_photo_extensions"`
	ExtraSupportedVideoExtensions []string `toml:"extra_supported_video_extensions"`
	ExtraUnsupportedExtensions    []string `toml:"extra_unsupported_extensions"`
	ExtraIgnoredFileNames         []string `toml:"extra_ignored_file_names"`
	ExtraIgnoredDirNames          []string `toml:"extra_ignored_dir_names"`
	// WatchDebounceSeconds/WatchHeartbeatMinutes configure gpsync-tray's watch
	// engine (cmd/gpsync-tray) -- `gpsync watch`'s own --debounce/--heartbeat CLI
	// flags are separate and unaffected by these. Plain ints (not
	// time.Duration) because go-toml would otherwise round-trip a Duration
	// as a raw nanosecond count, unreadable/unwriteable by hand in
	// config.toml -- same reasoning as SpaceSaverMaxDim etc. above.
	WatchDebounceSeconds  int `toml:"watch_debounce_seconds"`
	WatchHeartbeatMinutes int `toml:"watch_heartbeat_minutes"`
	// Theme controls gpsync-tray's dashboard color scheme -- ThemeSystem (the
	// default) follows the OS/browser's prefers-color-scheme, ThemeLight/
	// ThemeDark pin it regardless. Stored in config.toml (not a browser
	// cookie/localStorage) because the dashboard's localhost port is
	// re-assigned by the OS on every gpsync-tray restart (net.Listen("tcp",
	// "127.0.0.1:0")) -- anything origin-scoped in the browser would be
	// unreachable again the next time the app starts on a different port.
	Theme string `toml:"theme"`
	// SyncStrategy controls the order gpsync-tray's watch engine uploads
	// within each folder cycle -- see the SyncStrategy* constants below.
	// `gpsync upload`'s own --smallest-first/--smallest-first-global CLI flags
	// are separate and unaffected.
	SyncStrategy string `toml:"sync_strategy"`

	// DashboardListenAddr/DashboardPort/DashboardAuth* control the tray's
	// HTTP dashboard server: the address and port it binds, whether a login
	// is required, and the credentials for it. Binding beyond 127.0.0.1
	// exposes an otherwise-unauthenticated control surface (Settings,
	// file browsing, pause/cancel) to the whole LAN, which is exactly
	// why DashboardAuth* exists alongside it.
	//
	// A change to any of these five fields only takes effect on the next
	// gpsync-tray restart -- the dashboard server is started once, in
	// onReady(), and deliberately not hot-restarted from inside its own
	// Settings-save request handler (racing a server rebind against the
	// very response that's telling the browser the save succeeded is a
	// real correctness hazard, not worth it for a rarely-changed
	// setting). handleSettingsSave banners this plainly when it detects
	// one of these five actually changed.
	DashboardListenAddr string `toml:"dashboard_listen_addr"`
	// DashboardPort is 0 until the server has actually bound a real port
	// once; from then on it's cached here so a restart reuses the SAME
	// port instead of a fresh OS-assigned one every time -- config.toml
	// is the only place that can survive a restart (see Theme's own doc
	// comment: the dashboard's port used to be reassigned by the OS on
	// every launch, so nothing origin-scoped in the browser could ever
	// be used for cross-restart state). Only an OS-picked port is cached:
	// a port the user set is never overwritten. If it cannot be bound, the
	// tray serves on a temporary port for that session and logs why -- it
	// used to re-cache the temporary port, which silently replaced the
	// user's choice (see dashboard.ListenPreferred).
	DashboardPort        int  `toml:"dashboard_port"`
	DashboardAuthEnabled bool `toml:"dashboard_auth_enabled"`
	// DashboardBindGuarded is set by Load when it had to pull
	// DashboardListenAddr back to loopback because the configured address
	// would have exposed an unauthenticated dashboard. Runtime-only
	// (`toml:"-"`): it describes what THIS load did, and persisting it
	// would make a one-off correction look like a user preference.
	DashboardBindGuarded bool   `toml:"-"`
	DashboardAuthUser    string `toml:"dashboard_auth_user"`
	// DashboardAuthPassHash is a bcrypt hash, never the raw password --
	// consistent with this package's own doc comment ("non-secret
	// settings... secrets never here"), since a bcrypt hash isn't
	// reversible to the original password the way a plaintext value
	// would be. Settings' password field is write-only (see
	// ApplySettingsForm): submitting it blank means "keep the existing
	// hash", never derived from what's already stored.
	DashboardAuthPassHash string `toml:"dashboard_auth_pass_hash"`

	// DashboardTLSMode selects where the dashboard's certificate comes
	// from, or TLSModeOff to serve plain HTTP. Deliberately one field
	// rather than a separate enabled/mode pair: two switches would allow
	// the nonsensical "enabled, but mode off", and a setting that encodes
	// one fact in two places drifts.
	//
	// Off by default. v0.1.0 shipped plain HTTP, so turning TLS on by
	// itself would change the scheme under anyone holding a saved http://
	// bookmark. Like the fields above, it takes effect on the next restart.
	DashboardTLSMode string `toml:"dashboard_tls_mode"`
	// DashboardTLSCertFile/DashboardTLSKeyFile are a certificate and key
	// the user manages -- mkcert, a home CA, or a real CA. Read in
	// TLSModeFiles and never written to: regenerating over a key gpsync
	// did not create would destroy it.
	DashboardTLSCertFile string `toml:"dashboard_tls_cert_file"`
	DashboardTLSKeyFile  string `toml:"dashboard_tls_key_file"`
	// DashboardTLSDomain is the public hostname for TLSModeAcme, e.g.
	// "dashboard.example.com". It must resolve to this machine from the
	// internet, or the ACME challenge cannot complete.
	//
	// Reaching that point means the dashboard is exposed to the internet
	// rather than just the LAN, which is a materially larger step: it
	// edits settings, browses the whole library, moves files resolving
	// duplicates, and runs backup/restore. TLSModeAcme therefore requires
	// DashboardAuthEnabled, on the same reasoning as BindNeedsAuth.
	DashboardTLSDomain string `toml:"dashboard_tls_domain"`
	// DashboardTLSGuarded is set by Load when it had to downgrade
	// TLSModeAcme because authentication was off. Runtime-only
	// (`toml:"-"`) for the same reason as DashboardBindGuarded: it
	// describes what THIS load corrected, and persisting it would turn a
	// one-off correction into a stored preference.
	DashboardTLSGuarded bool `toml:"-"`
}

// The DashboardTLSMode values. TLSModeSelfSigned generates and manages a
// certificate under ~/.gpsync and is the only mode that needs no domain at
// all, which is why it suits the default deployment on loopback or a LAN
// address; TLSModeAcme is for a machine genuinely reachable by hostname
// from the internet.
const (
	TLSModeOff        = "off"
	TLSModeSelfSigned = "self-signed"
	TLSModeFiles      = "files"
	TLSModeAcme       = "acme"
)

// TLSModeValid reports whether mode is one gpsync understands. Exported so
// engine.ApplySettingsForm can reject exactly what Load would otherwise
// have to correct -- the same reason BindNeedsAuth is exported, and the
// same drift it exists to prevent.
func TLSModeValid(mode string) bool {
	switch mode {
	case TLSModeOff, TLSModeSelfSigned, TLSModeFiles, TLSModeAcme:
		return true
	}
	return false
}

const (
	ThemeSystem = "system"
	ThemeLight  = "light"
	ThemeDark   = "dark"
)

const (
	// SyncStrategyFolderByFolder: upload each folder's files in whatever
	// order ListPendingUnder returns them (source path order) -- the
	// original, and still default, behavior.
	SyncStrategyFolderByFolder = "folder_by_folder"
	// SyncStrategySmallestFirst: within each folder, smallest files
	// upload first (same mechanism as `gpsync upload --smallest-first`,
	// applied per folder-cycle rather than needing the flag).
	SyncStrategySmallestFirst = "smallest_first"
	// SyncStrategyPhotosFirst: within each folder, every photo uploads
	// before any video does (two passes per cycle when the folder has
	// both kinds and MediaTypeFilter isn't already narrowed to one).
	SyncStrategyPhotosFirst = "photos_first"
)

func Defaults() Config {
	return Config{
		SourceFolders:                 nil,
		Concurrency:                   6,
		UploadQuality:                 "original",
		SpaceSaverMaxDim:              2048,
		SpaceSaverJPEGQuality:         85,
		BackupDir:                     "",
		BackupKeepCount:               10,
		TrashDir:                      "",
		DupesDir:                      "",
		SortSmallestFirst:             false,
		MediaTypeFilter:               extensions.KindUnknown,
		ExtraSupportedPhotoExtensions: nil,
		ExtraSupportedVideoExtensions: nil,
		ExtraUnsupportedExtensions:    nil,
		ExtraIgnoredFileNames:         nil,
		ExtraIgnoredDirNames:          nil,
		WatchDebounceSeconds:          8,
		WatchHeartbeatMinutes:         15,
		Theme:                         ThemeSystem,
		SyncStrategy:                  SyncStrategyFolderByFolder,
		DashboardListenAddr:           DashboardListenLocal,
		DashboardPort:                 0,
		DashboardAuthEnabled:          false,
		DashboardTLSMode:              TLSModeOff,
	}
}

// DashboardListenAll/DashboardListenLocal are the two DashboardListenAddr
// values gpsync-tray's Settings page offers -- a specific interface IP is
// also accepted (stored and used as-is), these are just the two common
// named choices.
const (
	DashboardListenAll   = "0.0.0.0"
	DashboardListenLocal = "127.0.0.1"
)

func Load() (Config, error) {
	cfg := Defaults()
	data, err := os.ReadFile(ConfigPath)
	if os.IsNotExist(err) {
		return cfg, nil
	}
	if err != nil {
		return cfg, err
	}
	if err := toml.Unmarshal(data, &cfg); err != nil {
		return cfg, err
	}
	// Defaults() only fills in a key that's ABSENT from config.toml --
	// toml.Unmarshal overwrites present keys verbatim, so a hand-edited
	// `watch_heartbeat_minutes = 0` (or watch_debounce_seconds) survives
	// here even though engine.ApplySettingsForm has always enforced >= 1
	// for a Settings-page submission. The bug this prevents: gpsync-tray's
	// watch loop calls time.NewTicker(time.Duration(WatchHeartbeatMinutes)
	// * time.Minute) unconditionally -- NewTicker panics on a non-positive
	// duration, silently killing the whole -H=windowsgui process (no
	// console, and the panic happens before anything reaches tray.log) the
	// moment gpsync-tray next tries to start watching. Clamped here, not just
	// validated at Settings-save time, so hand-editing config.toml (an
	// explicitly supported path -- see ApplySettingsForm's own doc
	// comment on never wiping a hand-configured field) can't reach that
	// crash. WatchDebounceSeconds=0 wouldn't panic (time.AfterFunc(0, ...)
	// is legal) but would defeat debouncing entirely -- clamped for the
	// same reason, not just the panic risk.
	if cfg.WatchHeartbeatMinutes < 1 {
		cfg.WatchHeartbeatMinutes = 1
	}
	if cfg.WatchDebounceSeconds < 1 {
		cfg.WatchDebounceSeconds = 1
	}
	// Same "clamp what a hand-edited file can ask for" discipline as the
	// two above, applied to the one setting that can expose this machine
	// to the network. The dashboard is NOT a read-only status page: it
	// edits settings, browses the whole library, moves files when
	// resolving duplicates, and runs backup/restore. Serving that
	// unauthenticated on 0.0.0.0 hands every one of those to anything on
	// the LAN.
	//
	// Downgraded rather than refused, so a misconfiguration can never
	// leave someone with no dashboard at all -- it binds to loopback and
	// says so. ApplySettingsForm rejects the same combination up front,
	// so this is the backstop for a hand-edited config.toml (an
	// explicitly supported path), not the primary guard.
	if BindNeedsAuth(cfg.DashboardListenAddr) && !cfg.DashboardAuthEnabled {
		cfg.DashboardListenAddr = DashboardListenLocal
		cfg.DashboardBindGuarded = true
	}
	// A config.toml predating this field is already handled: the key is
	// absent, so toml.Unmarshal leaves Defaults()' "off" in place.
	//
	// This covers the case that is NOT handled that way -- a hand-edited
	// `dashboard_tls_mode = ""`, where the key IS present, so Unmarshal
	// overwrites the default with an empty string. Hand-editing config.toml
	// is a supported path (the same reason the watch-interval clamp above
	// exists), and an empty value plainly means "not configured".
	//
	// An UNRECOGNISED value is deliberately left alone rather than
	// normalised to "off": serving plain HTTP to someone who believes they
	// turned TLS on is the worst outcome available, so the server reports
	// it rather than silently continuing.
	if strings.TrimSpace(cfg.DashboardTLSMode) == "" {
		cfg.DashboardTLSMode = TLSModeOff
	}
	// Backstop for a hand-edited config.toml, mirroring the bind clamp
	// above -- ApplySettingsForm already refuses this pair through the UI,
	// so this is the path for someone editing the file directly.
	//
	// Downgraded to self-signed rather than refused or switched off.
	// Refusing would leave no dashboard at all, and switching TLS off
	// would serve plain HTTP on a host the user clearly meant to expose.
	// Self-signed keeps the traffic encrypted, and nothing is lost by not
	// attempting ACME: the bind clamp above has already pulled the
	// listener back to loopback, so the challenge could not have
	// completed regardless.
	if cfg.DashboardTLSMode == TLSModeAcme && !cfg.DashboardAuthEnabled {
		cfg.DashboardTLSMode = TLSModeSelfSigned
		cfg.DashboardTLSGuarded = true
	}
	return cfg, nil
}

// BindNeedsAuth reports whether addr would expose the dashboard past this
// machine. Exported so engine.ApplySettingsForm rejects exactly what Load
// would otherwise have to correct -- two copies of this rule would drift. Loopback and the empty value (never configured) are safe on
// their own; everything else -- 0.0.0.0, ::, a specific LAN interface --
// reaches other hosts and therefore needs a login in front of it.
func BindNeedsAuth(addr string) bool {
	addr = strings.TrimSpace(addr)
	if addr == "" || addr == "localhost" {
		return false
	}
	if ip := net.ParseIP(addr); ip != nil && ip.IsLoopback() {
		return false
	}
	return true
}

func Save(cfg Config) error {
	if err := os.MkdirAll(statedb.StateDir, 0o700); err != nil {
		return err
	}
	data, err := toml.Marshal(cfg)
	if err != nil {
		return err
	}
	// config.toml carries the dashboard password hash: owner-only.
	return os.WriteFile(ConfigPath, data, 0o600)
}
