package dashboard

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gmizrahi/gpsync/internal/config"
)

// Off must stay exactly what every install does today: no certificate
// touched, nothing written, plain HTTP.
func TestTLSSetupFor_Off(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Defaults()

	got, err := TLSSetupFor(cfg, dir, testHosts(), time.Now())
	if err != nil {
		t.Fatalf("TLSSetupFor: %v", err)
	}
	if got.Enabled {
		t.Error("Enabled = true, want false for the off mode")
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("wrote %d file(s) with TLS off; nothing should be generated", len(entries))
	}
	if s := BrowserScheme(got); s != "http" {
		t.Errorf("BrowserScheme = %q, want http", s)
	}
}

// An empty mode behaves as off. config.Load normalises this, but the
// resolver must not depend on having been called through Load -- the CLI
// builds a config with per-run overrides too.
func TestTLSSetupFor_EmptyModeIsOff(t *testing.T) {
	cfg := config.Defaults()
	cfg.DashboardTLSMode = ""

	got, err := TLSSetupFor(cfg, t.TempDir(), testHosts(), time.Now())
	if err != nil {
		t.Fatalf("TLSSetupFor: %v", err)
	}
	if got.Enabled {
		t.Error("Enabled = true, want false for an empty mode")
	}
}

func TestTLSSetupFor_SelfSignedGeneratesAndReuses(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Defaults()
	cfg.DashboardTLSMode = config.TLSModeSelfSigned
	now := time.Now()

	first, err := TLSSetupFor(cfg, dir, testHosts(), now)
	if err != nil {
		t.Fatalf("first TLSSetupFor: %v", err)
	}
	if !first.Enabled || !first.Files.Generated {
		t.Fatalf("Enabled=%v Generated=%v, want true/true on a first run", first.Enabled, first.Files.Generated)
	}
	if first.Files.Fingerprint == "" {
		t.Error("Fingerprint is empty; it is what the user verifies at the trust prompt")
	}
	if s := BrowserScheme(first); s != "https" {
		t.Errorf("BrowserScheme = %q, want https", s)
	}

	second, err := TLSSetupFor(cfg, dir, testHosts(), now)
	if err != nil {
		t.Fatalf("second TLSSetupFor: %v", err)
	}
	if second.Files.Generated {
		t.Error("Generated = true on the second call; restarting must not invalidate established trust")
	}
	if second.Files.Fingerprint != first.Files.Fingerprint {
		t.Error("fingerprint changed across restarts")
	}
}

func TestTLSSetupFor_FilesModeUsesTheConfiguredPair(t *testing.T) {
	supplied := t.TempDir()
	managed := t.TempDir()
	now := time.Now()

	seed, err := EnsureTLSFiles(supplied, "", "", testHosts(), now)
	if err != nil {
		t.Fatalf("seeding a stand-in user certificate: %v", err)
	}

	cfg := config.Defaults()
	cfg.DashboardTLSMode = config.TLSModeFiles
	cfg.DashboardTLSCertFile = seed.CertPath
	cfg.DashboardTLSKeyFile = seed.KeyPath

	got, err := TLSSetupFor(cfg, managed, testHosts(), now)
	if err != nil {
		t.Fatalf("TLSSetupFor: %v", err)
	}
	if !got.Enabled || !got.Files.Supplied || got.Files.Generated {
		t.Errorf("Enabled=%v Supplied=%v Generated=%v, want true/true/false",
			got.Enabled, got.Files.Supplied, got.Files.Generated)
	}
	if got.Files.CertPath != seed.CertPath {
		t.Errorf("CertPath = %q, want the configured %q", got.Files.CertPath, seed.CertPath)
	}
	if entries, _ := os.ReadDir(managed); len(entries) != 0 {
		t.Errorf("generated %d file(s) despite a configured pair", len(entries))
	}
}

// A configured path that does not exist must be reported. Falling back to a
// generated certificate would serve one the user never asked for while
// theirs sat unused behind a typo.
func TestTLSSetupFor_FilesModeMissingPair_IsAnError(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Defaults()
	cfg.DashboardTLSMode = config.TLSModeFiles
	cfg.DashboardTLSCertFile = filepath.Join(dir, "absent.pem")
	cfg.DashboardTLSKeyFile = filepath.Join(dir, "absent.key")

	if _, err := TLSSetupFor(cfg, dir, testHosts(), time.Now()); err == nil {
		t.Fatal("err = nil, want an error naming the missing certificate")
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Error("generated a fallback certificate instead of reporting the missing one")
	}
}

// acme is a known mode this build cannot serve yet. It must say so rather
// than downgrade: someone who configured acme wants a publicly-trusted
// certificate, and silently giving them a self-signed one would look like
// ACME had failed rather than never having run.
func TestTLSSetupFor_AcmeIsReportedNotDowngraded(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Defaults()
	cfg.DashboardTLSMode = config.TLSModeAcme
	cfg.DashboardTLSDomain = "dashboard.example.com"

	got, err := TLSSetupFor(cfg, dir, testHosts(), time.Now())
	if err == nil {
		t.Fatal("err = nil, want acme reported as unsupported in this build")
	}
	if !errors.Is(err, ErrTLSModeUnsupported) {
		t.Errorf("error %v does not wrap ErrTLSModeUnsupported, so a caller cannot distinguish it", err)
	}
	if got.Enabled {
		t.Error("Enabled = true alongside an error")
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Error("generated a self-signed certificate as a silent fallback for acme")
	}
}

// A typo must be named, not absorbed into off -- the same rule config.Load
// and ApplySettingsForm already apply.
func TestTLSSetupFor_UnknownMode_IsAnError(t *testing.T) {
	cfg := config.Defaults()
	cfg.DashboardTLSMode = "selfsigned"

	if _, err := TLSSetupFor(cfg, t.TempDir(), testHosts(), time.Now()); err == nil {
		t.Fatal("err = nil, want an unknown mode reported rather than treated as off")
	}
}
