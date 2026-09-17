package dashboard

import (
	"fmt"
	"time"

	"github.com/gmizrahi/gpsync/internal/config"
)

// ErrTLSModeUnsupported is returned for a mode gpsync knows the name of but
// cannot serve in this build. It exists so the caller reports the mode
// rather than quietly serving something else: plain HTTP to someone who
// believes they enabled TLS is the outcome worth failing over.
var ErrTLSModeUnsupported = fmt.Errorf("dashboard TLS mode is not supported by this build")

// TLSSetup is how the dashboard's listener should be served.
type TLSSetup struct {
	// Enabled is false for config.TLSModeOff, where Files is unset and the
	// caller serves plain HTTP exactly as before.
	Enabled bool
	Files   TLSFiles
}

// TLSSetupFor resolves a config into the certificate the dashboard should
// serve with, generating the self-signed pair if that mode is selected and
// no usable one exists yet.
//
// dir is where a managed pair lives -- statedb.StateDir in production, a
// temporary directory in tests. hosts is what a generated certificate must
// cover; see DefaultCertHosts.
//
// An unrecognised mode is an error rather than a fallback to off. Both
// config.Load and engine.ApplySettingsForm already refuse to normalise one
// away, for the same reason: a typo must not silently downgrade the
// dashboard to cleartext.
func TLSSetupFor(cfg config.Config, dir string, hosts []string, now time.Time) (TLSSetup, error) {
	switch cfg.DashboardTLSMode {
	case "", config.TLSModeOff:
		return TLSSetup{}, nil

	case config.TLSModeSelfSigned:
		files, err := EnsureTLSFiles(dir, "", "", hosts, now)
		if err != nil {
			return TLSSetup{}, fmt.Errorf("preparing the dashboard certificate: %w", err)
		}
		return TLSSetup{Enabled: true, Files: files}, nil

	case config.TLSModeFiles:
		// EnsureTLSFiles refuses half a pair and never writes to a supplied
		// one, so nothing here can overwrite a key gpsync did not create.
		files, err := EnsureTLSFiles(dir, cfg.DashboardTLSCertFile, cfg.DashboardTLSKeyFile, hosts, now)
		if err != nil {
			return TLSSetup{}, fmt.Errorf("preparing the dashboard certificate: %w", err)
		}
		return TLSSetup{Enabled: true, Files: files}, nil

	case config.TLSModeAcme:
		// Deliberately an error, not a downgrade to self-signed: someone who
		// configured acme wants a publicly-trusted certificate, and quietly
		// handing them one their browser will warn about would look like
		// ACME had failed rather than never having been attempted.
		return TLSSetup{}, fmt.Errorf("%w: %q needs the ACME challenge listener, which is not wired up yet",
			ErrTLSModeUnsupported, config.TLSModeAcme)

	default:
		return TLSSetup{}, fmt.Errorf("unknown dashboard TLS mode %q (expected %q, %q, %q, or %q)",
			cfg.DashboardTLSMode, config.TLSModeOff, config.TLSModeSelfSigned,
			config.TLSModeFiles, config.TLSModeAcme)
	}
}

// BrowserScheme is what a URL printed for the user should start with, so
// the tray and the CLI cannot disagree about it.
func BrowserScheme(setup TLSSetup) string {
	if setup.Enabled {
		return "https"
	}
	return "http"
}
