package engine

import (
	"net/url"
	"reflect"
	"strings"
	"testing"

	"golang.org/x/crypto/bcrypt"

	"github.com/gmizrahi/gpsync/internal/config"
	"github.com/gmizrahi/gpsync/internal/extensions"
	"github.com/gmizrahi/gpsync/internal/scanner"
)

func validSettingsForm() url.Values {
	return url.Values{
		"source_folders":                   {"C:\\Photos\n\nC:\\Videos\n  \n"},
		"concurrency":                      {"4"},
		"media_type_filter":                {"photos"},
		"watch_debounce_seconds":           {"12"},
		"watch_heartbeat_minutes":          {"20"},
		"upload_quality":                   {"space_saver"},
		"space_saver_max_dimension":        {"3072"},
		"space_saver_jpeg_quality":         {"90"},
		"backup_dir":                       {"C:\\Backups"},
		"backup_keep_count":                {"5"},
		"trash_dir":                        {"C:\\Trash"},
		"dupes_dir":                        {"C:\\Dupes"},
		"extra_supported_photo_extensions": {"heic\nheif"},
		"extra_supported_video_extensions": {"insv"},
		"extra_unsupported_extensions":     {"psd"},
		"extra_ignored_file_names":         {"Thumbs.db\n.DS_Store"},
		"extra_ignored_dir_names":          {"@eaDir"},
		"theme":                            {"dark"},
		"sync_strategy":                    {"smallest_first"},
		"dashboard_listen_addr":            {"127.0.0.1"},
		"dashboard_port":                   {"51820"},
		"dashboard_auth_enabled":           {"on"},
		"dashboard_auth_user":              {"admin"},
		"dashboard_auth_password":          {"hunter2"},
	}
}

// TestApplySettingsForm_HappyPath proves a valid submission updates every
// exposed field correctly, INCLUDING filtering blank lines out of every
// list-shaped textarea (source folders, extension overrides).
func TestApplySettingsForm_HappyPath(t *testing.T) {
	current := config.Defaults()

	got, err := ApplySettingsForm(current, validSettingsForm())
	if err != nil {
		t.Fatalf("ApplySettingsForm error = %v, want nil", err)
	}

	wantFolders := []string{"C:\\Photos", "C:\\Videos"}
	if !reflect.DeepEqual(got.SourceFolders, wantFolders) {
		t.Errorf("SourceFolders = %v, want %v", got.SourceFolders, wantFolders)
	}
	if got.Concurrency != 4 {
		t.Errorf("Concurrency = %d, want 4", got.Concurrency)
	}
	if got.MediaTypeFilter != extensions.KindPhoto {
		t.Errorf("MediaTypeFilter = %v, want KindPhoto", got.MediaTypeFilter)
	}
	if got.WatchDebounceSeconds != 12 {
		t.Errorf("WatchDebounceSeconds = %d, want 12", got.WatchDebounceSeconds)
	}
	if got.WatchHeartbeatMinutes != 20 {
		t.Errorf("WatchHeartbeatMinutes = %d, want 20", got.WatchHeartbeatMinutes)
	}
	if got.UploadQuality != "space_saver" {
		t.Errorf("UploadQuality = %q, want space_saver", got.UploadQuality)
	}
	if got.SpaceSaverMaxDim != 3072 {
		t.Errorf("SpaceSaverMaxDim = %d, want 3072", got.SpaceSaverMaxDim)
	}
	if got.SpaceSaverJPEGQuality != 90 {
		t.Errorf("SpaceSaverJPEGQuality = %d, want 90", got.SpaceSaverJPEGQuality)
	}
	if got.BackupDir != "C:\\Backups" {
		t.Errorf("BackupDir = %q, want C:\\Backups", got.BackupDir)
	}
	if got.BackupKeepCount != 5 {
		t.Errorf("BackupKeepCount = %d, want 5", got.BackupKeepCount)
	}
	if got.TrashDir != "C:\\Trash" {
		t.Errorf("TrashDir = %q, want C:\\Trash", got.TrashDir)
	}
	if got.DupesDir != "C:\\Dupes" {
		t.Errorf("DupesDir = %q, want C:\\Dupes", got.DupesDir)
	}
	if want := []string{"heic", "heif"}; !reflect.DeepEqual(got.ExtraSupportedPhotoExtensions, want) {
		t.Errorf("ExtraSupportedPhotoExtensions = %v, want %v", got.ExtraSupportedPhotoExtensions, want)
	}
	if want := []string{"insv"}; !reflect.DeepEqual(got.ExtraSupportedVideoExtensions, want) {
		t.Errorf("ExtraSupportedVideoExtensions = %v, want %v", got.ExtraSupportedVideoExtensions, want)
	}
	if want := []string{"psd"}; !reflect.DeepEqual(got.ExtraUnsupportedExtensions, want) {
		t.Errorf("ExtraUnsupportedExtensions = %v, want %v", got.ExtraUnsupportedExtensions, want)
	}
	if want := []string{"Thumbs.db", ".DS_Store"}; !reflect.DeepEqual(got.ExtraIgnoredFileNames, want) {
		t.Errorf("ExtraIgnoredFileNames = %v, want %v", got.ExtraIgnoredFileNames, want)
	}
	if want := []string{"@eaDir"}; !reflect.DeepEqual(got.ExtraIgnoredDirNames, want) {
		t.Errorf("ExtraIgnoredDirNames = %v, want %v", got.ExtraIgnoredDirNames, want)
	}
	if got.Theme != "dark" {
		t.Errorf("Theme = %q, want dark", got.Theme)
	}
	if got.SyncStrategy != "smallest_first" {
		t.Errorf("SyncStrategy = %q, want smallest_first", got.SyncStrategy)
	}
	if got.DashboardListenAddr != "127.0.0.1" {
		t.Errorf("DashboardListenAddr = %q, want 127.0.0.1", got.DashboardListenAddr)
	}
	if got.DashboardPort != 51820 {
		t.Errorf("DashboardPort = %d, want 51820", got.DashboardPort)
	}
	if !got.DashboardAuthEnabled {
		t.Error("DashboardAuthEnabled = false, want true")
	}
	if got.DashboardAuthUser != "admin" {
		t.Errorf("DashboardAuthUser = %q, want admin", got.DashboardAuthUser)
	}
	if got.DashboardAuthPassHash == "" || got.DashboardAuthPassHash == "hunter2" {
		t.Errorf("DashboardAuthPassHash = %q, want a real bcrypt hash, never the raw password", got.DashboardAuthPassHash)
	}
	if err := bcrypt.CompareHashAndPassword([]byte(got.DashboardAuthPassHash), []byte("hunter2")); err != nil {
		t.Errorf("stored hash does not verify against the submitted password: %v", err)
	}
}

// TestApplySettingsForm_BlankDashboardPassword_KeepsExistingHash proves the
// password field is write-only: submitting it blank must never derive a
// new (empty-password) hash or wipe out whatever was already stored --
// the same convention every other secret-bearing form field follows.
func TestApplySettingsForm_BlankDashboardPassword_KeepsExistingHash(t *testing.T) {
	current := config.Defaults()
	current.DashboardAuthPassHash = "existing-hash-should-survive"

	form := validSettingsForm()
	form.Set("dashboard_auth_password", "")

	got, err := ApplySettingsForm(current, form)
	if err != nil {
		t.Fatalf("ApplySettingsForm error = %v, want nil", err)
	}
	if got.DashboardAuthPassHash != "existing-hash-should-survive" {
		t.Errorf("DashboardAuthPassHash = %q, want the existing hash left untouched", got.DashboardAuthPassHash)
	}
}

// TestApplySettingsForm_AuthEnabledWithoutCredentials_Errors proves you
// can't turn on dashboard auth (which is what makes binding beyond
// localhost safe at all) without an actual username and password.
func TestApplySettingsForm_AuthEnabledWithoutCredentials_Errors(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(url.Values)
	}{
		{"no username", func(v url.Values) { v.Set("dashboard_auth_user", "") }},
		{"no password ever set", func(v url.Values) {
			v.Set("dashboard_auth_user", "admin")
			v.Set("dashboard_auth_password", "")
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			current := config.Defaults() // DashboardAuthPassHash is "" here too
			form := validSettingsForm()
			tt.mutate(form)

			_, err := ApplySettingsForm(current, form)
			if err == nil {
				t.Fatalf("ApplySettingsForm error = nil, want an error for %s", tt.name)
			}
		})
	}
}

// TestApplySettingsForm_PreservesSortSmallestFirst is the whole point of
// starting from `current`: SortSmallestFirst is the one field this form
// deliberately never exposes (it's a per-run --smallest-first flag
// override, not a standing preference -- see its doc comment in
// config.Config), so a save must never touch it either way.
func TestApplySettingsForm_PreservesSortSmallestFirst(t *testing.T) {
	current := config.Defaults()
	current.SortSmallestFirst = true

	got, err := ApplySettingsForm(current, validSettingsForm())
	if err != nil {
		t.Fatalf("ApplySettingsForm error = %v, want nil", err)
	}
	if !got.SortSmallestFirst {
		t.Error("SortSmallestFirst = false, want unchanged true -- this form must never touch it")
	}
}

// TestApplySettingsForm_InvalidField_ReturnsCurrentUnchanged covers one
// invalid value per validated field -- each must reject the whole
// submission and hand back `current` completely untouched, never a
// partially-applied Config.
func TestApplySettingsForm_InvalidField_ReturnsCurrentUnchanged(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(url.Values)
	}{
		{"concurrency not a number", func(v url.Values) { v.Set("concurrency", "abc") }},
		{"concurrency zero", func(v url.Values) { v.Set("concurrency", "0") }},
		{"concurrency negative", func(v url.Values) { v.Set("concurrency", "-1") }},
		{"unknown media type", func(v url.Values) { v.Set("media_type_filter", "bogus") }},
		{"debounce not a number", func(v url.Values) { v.Set("watch_debounce_seconds", "soon") }},
		{"debounce zero", func(v url.Values) { v.Set("watch_debounce_seconds", "0") }},
		{"heartbeat not a number", func(v url.Values) { v.Set("watch_heartbeat_minutes", "often") }},
		{"heartbeat zero", func(v url.Values) { v.Set("watch_heartbeat_minutes", "0") }},
		{"unknown upload quality", func(v url.Values) { v.Set("upload_quality", "bogus") }},
		{"space saver max dimension zero", func(v url.Values) { v.Set("space_saver_max_dimension", "0") }},
		{"space saver jpeg quality zero", func(v url.Values) { v.Set("space_saver_jpeg_quality", "0") }},
		{"space saver jpeg quality over 100", func(v url.Values) { v.Set("space_saver_jpeg_quality", "101") }},
		{"backup keep count zero", func(v url.Values) { v.Set("backup_keep_count", "0") }},
		{"unknown theme", func(v url.Values) { v.Set("theme", "bogus") }},
		{"unknown sync strategy", func(v url.Values) { v.Set("sync_strategy", "bogus") }},
		{"dashboard listen addr not an IP", func(v url.Values) { v.Set("dashboard_listen_addr", "bogus") }},
		{"dashboard port not a number", func(v url.Values) { v.Set("dashboard_port", "bogus") }},
		{"dashboard port negative", func(v url.Values) { v.Set("dashboard_port", "-1") }},
		{"dashboard port over 65535", func(v url.Values) { v.Set("dashboard_port", "70000") }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			current := config.Defaults()
			current.Concurrency = 6 // a known, distinct sentinel from any test's bad value

			form := validSettingsForm()
			tt.mutate(form)

			got, err := ApplySettingsForm(current, form)
			if err == nil {
				t.Fatalf("ApplySettingsForm error = nil, want an error for %s", tt.name)
			}
			if !reflect.DeepEqual(got, current) {
				t.Errorf("ApplySettingsForm returned a modified Config on error, want current unchanged verbatim")
			}
		})
	}
}

// TestApplySettingsForm_AllMediaType_MapsToKindUnknown proves the "all"
// dropdown option round-trips to "no filter" (KindUnknown), matching how
// --media-type all already behaves on the CLI side.
func TestApplySettingsForm_AllMediaType_MapsToKindUnknown(t *testing.T) {
	form := validSettingsForm()
	form.Set("media_type_filter", "all")

	got, err := ApplySettingsForm(config.Defaults(), form)
	if err != nil {
		t.Fatalf("ApplySettingsForm error = %v, want nil", err)
	}
	if got.MediaTypeFilter != extensions.KindUnknown {
		t.Errorf("MediaTypeFilter = %v, want KindUnknown", got.MediaTypeFilter)
	}
}

// TestApplyExtensionOverrides_InstallsBothTables is the fix for a real,
// verified bug: gpsync-tray's Settings page lets you edit five extension/
// ignore-list overrides (ExtraSupportedPhotoExtensions/
// ExtraSupportedVideoExtensions/ExtraUnsupportedExtensions/
// ExtraIgnoredFileNames/ExtraIgnoredDirNames), but unlike cmd/gpsync (whose
// PersistentPreRunE always calls the equivalent of this), gpsync-tray never
// installed them into internal/extensions/internal/scanner at all -- a
// save reported success but silently changed nothing. Proves BOTH target
// tables get installed from one Config, not just one of the two.
func TestApplyExtensionOverrides_InstallsBothTables(t *testing.T) {
	t.Cleanup(func() {
		extensions.ApplyUserOverrides(nil, nil, nil)
		scanner.ApplyUserIgnoreOverrides(nil, nil)
	})

	if got := extensions.Classify("clip.insv"); got != extensions.Unknown {
		t.Fatalf("precondition failed: Classify(clip.insv) = %q, want Unknown", got)
	}
	if scanner.IsIgnoredDirName("MyBackupFolder") {
		t.Fatal("precondition failed: MyBackupFolder must not already be ignored")
	}

	cfg := config.Defaults()
	cfg.ExtraSupportedVideoExtensions = []string{"insv"}
	cfg.ExtraIgnoredDirNames = []string{"MyBackupFolder"}

	ApplyExtensionOverrides(cfg)

	if got := extensions.Classify("clip.insv"); got != extensions.Supported {
		t.Errorf("Classify(clip.insv) = %q, want Supported after ApplyExtensionOverrides", got)
	}
	if !scanner.IsIgnoredDirName("MyBackupFolder") {
		t.Error("IsIgnoredDirName(MyBackupFolder) = false, want true after ApplyExtensionOverrides")
	}
}

// TestApplySettingsForm_PublicBindWithoutAuth_IsRejected is the primary
// guard for the same rule config.Load backstops: the Settings page must
// never be able to create an unauthenticated dashboard reachable from the
// network. Rejected with a message rather than silently corrected --
// someone deliberately choosing "local network" deserves to be told why it
// didn't take, and what to do instead.
func TestApplySettingsForm_PublicBindWithoutAuth_IsRejected(t *testing.T) {
	current := config.Defaults()
	form := validSettingsForm()
	form.Set("dashboard_listen_addr", config.DashboardListenAll)
	form.Del("dashboard_auth_enabled")

	_, err := ApplySettingsForm(current, form)
	if err == nil {
		t.Fatal("ApplySettingsForm accepted 0.0.0.0 with auth off -- that serves an unauthenticated file-moving UI to the LAN")
	}
	for _, want := range []string{"authentication", config.DashboardListenLocal} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error must say how to fix it (missing %q): %v", want, err)
		}
	}

	// The same address WITH auth is a legitimate, supported choice.
	form.Set("dashboard_auth_enabled", "on")
	got, err := ApplySettingsForm(current, form)
	if err != nil {
		t.Fatalf("0.0.0.0 with auth enabled must be allowed: %v", err)
	}
	if got.DashboardListenAddr != config.DashboardListenAll {
		t.Errorf("DashboardListenAddr = %q, want the requested %q", got.DashboardListenAddr, config.DashboardListenAll)
	}

	// And loopback without auth stays fine -- nothing off-machine can
	// reach it, so requiring a login there would be friction for nothing.
	local := validSettingsForm()
	local.Set("dashboard_listen_addr", config.DashboardListenLocal)
	local.Del("dashboard_auth_enabled")
	if _, err := ApplySettingsForm(current, local); err != nil {
		t.Errorf("loopback without auth must be allowed: %v", err)
	}
}

// The form omits every TLS key today, which is exactly what an older
// browser-cached page submits. That must mean "off", not an error.
func TestApplySettingsForm_NoTLSFields_MeansOff(t *testing.T) {
	got, err := ApplySettingsForm(config.Defaults(), validSettingsForm())
	if err != nil {
		t.Fatalf("ApplySettingsForm error = %v, want nil", err)
	}
	if got.DashboardTLSMode != config.TLSModeOff {
		t.Errorf("DashboardTLSMode = %q, want %q when the form carries no TLS fields", got.DashboardTLSMode, config.TLSModeOff)
	}
}

// A typo must be named. Accepting it and serving plain HTTP would leave
// someone believing the dashboard was encrypted when it was not.
func TestApplySettingsForm_UnknownTLSMode_IsRejected(t *testing.T) {
	form := validSettingsForm()
	form.Set("dashboard_tls_mode", "selfsigned")

	current := config.Defaults()
	got, err := ApplySettingsForm(current, form)
	if err == nil {
		t.Fatal("err = nil, want a refusal naming the valid modes")
	}
	if got.DashboardTLSMode != current.DashboardTLSMode {
		t.Error("a rejected submission still altered the config")
	}
}

// Half a pair cannot work: gpsync would have to invent the missing half,
// and every handshake would fail looking like a corrupt file.
func TestApplySettingsForm_FilesModeNeedsBothPaths(t *testing.T) {
	for _, tc := range []struct{ name, cert, key string }{
		{"cert without key", "C:\\certs\\gpsync.crt", ""},
		{"key without cert", "", "C:\\certs\\gpsync.key"},
		{"neither", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			form := validSettingsForm()
			form.Set("dashboard_tls_mode", config.TLSModeFiles)
			form.Set("dashboard_tls_cert_file", tc.cert)
			form.Set("dashboard_tls_key_file", tc.key)

			if _, err := ApplySettingsForm(config.Defaults(), form); err == nil {
				t.Fatal("err = nil, want a refusal for an incomplete pair")
			}
		})
	}
}

func TestApplySettingsForm_AcmeNeedsADomain(t *testing.T) {
	form := validSettingsForm()
	form.Set("dashboard_tls_mode", config.TLSModeAcme)
	form.Set("dashboard_tls_domain", "")

	if _, err := ApplySettingsForm(config.Defaults(), form); err == nil {
		t.Fatal("err = nil, want a refusal: ACME cannot issue for no hostname")
	}
}

// ACME means the dashboard is reachable from the internet, not just the
// LAN. That surface edits settings, browses the library, moves files and
// runs backup/restore, so a login is not optional -- the same rule the
// listen-address check applies, at a larger scale.
func TestApplySettingsForm_AcmeWithoutAuth_IsRejected(t *testing.T) {
	form := validSettingsForm()
	form.Set("dashboard_tls_mode", config.TLSModeAcme)
	form.Set("dashboard_tls_domain", "dashboard.example.com")
	form.Del("dashboard_auth_enabled")

	if _, err := ApplySettingsForm(config.Defaults(), form); err == nil {
		t.Fatal("err = nil, want a refusal: an internet-reachable dashboard requires auth")
	}
}

func TestApplySettingsForm_AcmeWithDomainAndAuth_IsAccepted(t *testing.T) {
	form := validSettingsForm()
	form.Set("dashboard_tls_mode", config.TLSModeAcme)
	form.Set("dashboard_tls_domain", "dashboard.example.com")

	got, err := ApplySettingsForm(config.Defaults(), form)
	if err != nil {
		t.Fatalf("ApplySettingsForm error = %v, want nil", err)
	}
	if got.DashboardTLSMode != config.TLSModeAcme || got.DashboardTLSDomain != "dashboard.example.com" {
		t.Errorf("mode/domain = %q/%q, want acme/dashboard.example.com", got.DashboardTLSMode, got.DashboardTLSDomain)
	}
}
