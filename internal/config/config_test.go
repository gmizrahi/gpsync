package config

import (
	"os"
	"path/filepath"
	"testing"
)

// TestSaveLoad_RoundTripsWatchSettings proves the two new gpsync-tray fields
// survive a real Save/Load cycle -- the settings page (internal/engine's
// ApplySettingsForm) depends on this working correctly.
func TestSaveLoad_RoundTripsWatchSettings(t *testing.T) {
	ConfigPath = filepath.Join(t.TempDir(), "config.toml")

	cfg := Defaults()
	cfg.WatchDebounceSeconds = 30
	cfg.WatchHeartbeatMinutes = 5
	if err := Save(cfg); err != nil {
		t.Fatal(err)
	}

	got, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if got.WatchDebounceSeconds != 30 {
		t.Errorf("WatchDebounceSeconds = %d, want 30", got.WatchDebounceSeconds)
	}
	if got.WatchHeartbeatMinutes != 5 {
		t.Errorf("WatchHeartbeatMinutes = %d, want 5", got.WatchHeartbeatMinutes)
	}
}

// TestLoad_MissingFile_FillsDefaults proves an old config.toml written
// before these fields existed (or no file at all) still gets the current
// 8s/15m behavior, not a zero value that would make gpsync-tray tick
// constantly.
func TestLoad_MissingFile_FillsDefaults(t *testing.T) {
	ConfigPath = filepath.Join(t.TempDir(), "does-not-exist.toml")

	got, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if got.WatchDebounceSeconds != 8 {
		t.Errorf("WatchDebounceSeconds = %d, want 8 (Defaults())", got.WatchDebounceSeconds)
	}
	if got.WatchHeartbeatMinutes != 15 {
		t.Errorf("WatchHeartbeatMinutes = %d, want 15 (Defaults())", got.WatchHeartbeatMinutes)
	}
}

// TestLoad_HandEditedZeroWatchSettings_ClampedToOne is the fix for a real,
// verified bug: engine.ApplySettingsForm has always enforced ">= 1" for a
// Settings-page submission, but Load() only fills in a key ABSENT from
// config.toml -- toml.Unmarshal overwrites a key that IS present verbatim,
// so a hand-edited "watch_heartbeat_minutes = 0" survived untouched.
// gpsync-tray's watch loop calls time.NewTicker(time.Duration(
// WatchHeartbeatMinutes) * time.Minute) unconditionally, and NewTicker
// panics on a non-positive duration -- silently killing the whole
// -H=windowsgui process with nothing reaching tray.log. Hand-editing
// config.toml is an explicitly supported path (see ApplySettingsForm's own
// doc comment), so this must be clamped at load time, not just validated
// at save time.
func TestLoad_HandEditedZeroWatchSettings_ClampedToOne(t *testing.T) {
	ConfigPath = filepath.Join(t.TempDir(), "config.toml")
	raw := "watch_debounce_seconds = 0\nwatch_heartbeat_minutes = 0\n"
	if err := os.WriteFile(ConfigPath, []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if got.WatchDebounceSeconds != 1 {
		t.Errorf("WatchDebounceSeconds = %d, want clamped to 1", got.WatchDebounceSeconds)
	}
	if got.WatchHeartbeatMinutes != 1 {
		t.Errorf("WatchHeartbeatMinutes = %d, want clamped to 1", got.WatchHeartbeatMinutes)
	}
}

// TestLoad_PublicBindWithoutAuth_FallsBackToLoopback covers the one
// setting that can expose this machine. The dashboard is not a read-only
// status page: it edits settings, browses the whole library, moves files
// when resolving duplicates, and runs backup/restore. Serving that
// unauthenticated on 0.0.0.0 hands every one of those to anything on the
// network.
//
// Clamped at LOAD time, not only validated at save time, for the same
// reason the watch settings above are: hand-editing config.toml is an
// explicitly supported path, and a rule that only the Settings page
// enforces is not a rule.
func TestLoad_PublicBindWithoutAuth_FallsBackToLoopback(t *testing.T) {
	cases := []struct {
		name      string
		raw       string
		wantAddr  string
		wantGuard bool
	}{
		{
			name:      "all interfaces, no auth",
			raw:       "dashboard_listen_addr = '0.0.0.0'\ndashboard_auth_enabled = false\n",
			wantAddr:  DashboardListenLocal,
			wantGuard: true,
		},
		{
			// A specific LAN interface is just as reachable as 0.0.0.0.
			name:      "specific LAN interface, no auth",
			raw:       "dashboard_listen_addr = '192.168.1.40'\ndashboard_auth_enabled = false\n",
			wantAddr:  DashboardListenLocal,
			wantGuard: true,
		},
		{
			// With auth in front of it, the user's choice stands.
			name:      "all interfaces WITH auth",
			raw:       "dashboard_listen_addr = '0.0.0.0'\ndashboard_auth_enabled = true\n",
			wantAddr:  DashboardListenAll,
			wantGuard: false,
		},
		{
			// Loopback needs no login and must not be touched.
			name:      "loopback, no auth",
			raw:       "dashboard_listen_addr = '127.0.0.1'\ndashboard_auth_enabled = false\n",
			wantAddr:  DashboardListenLocal,
			wantGuard: false,
		},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			ConfigPath = filepath.Join(t.TempDir(), "config.toml")
			if err := os.WriteFile(ConfigPath, []byte(tt.raw), 0o644); err != nil {
				t.Fatal(err)
			}
			got, err := Load()
			if err != nil {
				t.Fatal(err)
			}
			if got.DashboardListenAddr != tt.wantAddr {
				t.Errorf("DashboardListenAddr = %q, want %q", got.DashboardListenAddr, tt.wantAddr)
			}
			if got.DashboardBindGuarded != tt.wantGuard {
				t.Errorf("DashboardBindGuarded = %v, want %v -- the tray logs this to explain the change", got.DashboardBindGuarded, tt.wantGuard)
			}
		})
	}
}

// TestDefaults_BindLoopback pins the shipped default itself. This was
// 0.0.0.0 with auth off, which meant a fresh install served an
// unauthenticated file-moving UI to the whole LAN before anyone had
// chosen anything.
func TestDefaults_BindLoopback(t *testing.T) {
	d := Defaults()
	if d.DashboardAuthEnabled {
		t.Error("auth defaults on -- fine in itself, but then the first-run experience needs a password before the dashboard works")
	}
	if d.DashboardListenAddr != DashboardListenLocal {
		t.Errorf("default listen address = %q, want %q: with auth off by default, the default bind must not leave this machine",
			d.DashboardListenAddr, DashboardListenLocal)
	}
}

// TestBindNeedsAuth covers the predicate both config.Load and
// engine.ApplySettingsForm share, so the two can't drift apart.
func TestBindNeedsAuth(t *testing.T) {
	for addr, want := range map[string]bool{
		"":            false, // never configured
		"127.0.0.1":   false,
		"127.0.0.53":  false, // the whole 127/8 block is loopback
		"::1":         false,
		"localhost":   false,
		"0.0.0.0":     true,
		"::":          true,
		"192.168.1.5": true,
		"10.0.0.7":    true,
	} {
		if got := BindNeedsAuth(addr); got != want {
			t.Errorf("BindNeedsAuth(%q) = %v, want %v", addr, got, want)
		}
	}
}
