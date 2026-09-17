package main

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gmizrahi/gpsync/internal/config"
	// Aliased: this package has its own `dashboard` type, the terminal
	// renderer in dashboard.go.
	webdashboard "github.com/gmizrahi/gpsync/internal/dashboard"
	"github.com/gmizrahi/gpsync/internal/engine"
	"github.com/gmizrahi/gpsync/internal/statedb"
	"github.com/spf13/cobra"
)

// trayDashboardURL builds the URL gpsync uses to probe whether gpsync-tray's
// dashboard is currently listening, from the same DashboardPort
// config.Config already caches and persists across gpsync-tray restarts (see
// its own doc comment). Returns "" if there's no cached port at all
// (DashboardPort == 0), meaning gpsync-tray has never started even once --
// nothing to probe.
//
// Always connects via 127.0.0.1, never DashboardListenAddr directly: that
// field is normally "0.0.0.0" ("all interfaces"), which isn't itself a
// valid connect target, and a loopback connection reaches the server
// regardless of which interface(s) it actually bound, except the uncommon
// case of a listen address that deliberately excludes loopback -- an
// acceptable simplification for what's only ever an informational check,
// never something that gates real functionality.
func trayDashboardURL(cfg config.Config) string {
	if cfg.DashboardPort == 0 {
		return ""
	}
	return trayDashboardBase(cfg) + "/api/status"
}

// trayDashboardBase is the scheme+host+port every request to a running
// tray starts from. The scheme has to follow dashboard_tls_mode: a
// hardcoded "http://" silently broke both the running-tray notice and
// `gpsync tray-quit` the moment TLS was switched on, which is exactly the
// command that exists so the binary can be replaced without cutting a
// transfer off.
func trayDashboardBase(cfg config.Config) string {
	scheme := "http"
	if cfg.DashboardTLSMode != "" && cfg.DashboardTLSMode != config.TLSModeOff {
		scheme = "https"
	}
	return fmt.Sprintf("%s://127.0.0.1:%d", scheme, cfg.DashboardPort)
}

// trayHTTPClient talks to the local tray, trusting the certificate gpsync
// itself generated rather than skipping verification.
//
// In self-signed mode nothing else trusts that certificate, so the client
// is given exactly it as its only root. InsecureSkipVerify would have been
// shorter and is the reflex here, but it accepts ANY certificate on that
// port -- strictly worse for no benefit, since we wrote this one and know
// where it lives. The generated certificate covers 127.0.0.1 (see
// DefaultCertHosts), so verification succeeds on the nose.
//
// files and acme modes get the system pool: those chain to a CA the
// machine already trusts. A user-supplied certificate that does not cover
// loopback will fail here -- correctly, and the callers report it rather
// than pretending the tray is absent.
func trayHTTPClient(cfg config.Config, timeout time.Duration) *http.Client {
	client := &http.Client{Timeout: timeout}
	if cfg.DashboardTLSMode != config.TLSModeSelfSigned {
		return client
	}
	pem, err := os.ReadFile(filepath.Join(statedb.StateDir, webdashboard.TLSCertFileName))
	if err != nil {
		return client
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return client
	}
	client.Transport = &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}
	return client
}

// trayRunningNotice checks whether gpsync-tray's dashboard is currently
// reachable and, if so, returns a one-line note to print, so a command run
// alongside the tray says so rather than appearing to act alone. A short
// timeout (not
// the default client's none-at-all) keeps this from noticeably delaying a
// command in the unusual case of something silently dropping the
// connection attempt rather than the near-instant "connection refused" an
// ordinary not-running gpsync-tray gets.
//
// ANY response at all -- even a 401 from Basic Auth -- is treated as
// "running": this only needs to prove something is listening and
// answering like gpsync-tray's own dashboard, not read its body.
func trayRunningNotice(cfg config.Config) string {
	url := trayDashboardURL(cfg)
	if url == "" {
		return ""
	}
	client := trayHTTPClient(cfg, 300*time.Millisecond)
	resp, err := client.Get(url)
	if err != nil {
		return ""
	}
	resp.Body.Close()
	return fmt.Sprintf("gpsync-tray is currently running (dashboard: %s) -- safe to run alongside; you may see overlapping activity.", trayDashboardBase(cfg))
}

// trayQuitCmd asks a running gpsync-tray to shut down gracefully -- in-flight
// uploads finish, no new ones start -- the same thing the tray's own Quit
// dialog does when you pick "No". Asked for as the way to swap the binary
// without cutting a transfer off: "why don't you tell it to exit gracefully
// ... and then you replace the binary and ask me to reload it".
//
// Talks to the dashboard over loopback, which is also why this is a CLI
// command rather than something a build script curls: the port is whatever
// the tray bound last run, and only config.toml knows it.
// trayQuitSession mints a short-lived dashboard session so `gpsync
// tray-quit` can authenticate against a tray that has auth enabled. Five
// minutes is far longer than the single request needs and short enough
// that a crash between here and the request leaves nothing useful behind.
func trayQuitSession() (string, error) {
	db, err := statedb.Open()
	if err != nil {
		return "", err
	}
	defer db.Close()
	token, err := engine.GenerateSessionToken()
	if err != nil {
		return "", err
	}
	if err := db.CreateSession(token, float64(time.Now().Add(5*time.Minute).Unix())); err != nil {
		return "", err
	}
	return token, nil
}

func trayQuitCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "tray-quit",
		Short: "Ask gpsync-tray to finish current uploads and exit",
		Long: "Asks a running gpsync-tray to exit gracefully: uploads in progress finish and nothing new starts. Use it before replacing gpsync-tray.exe.\n" +
			"\n" +
			"Exits with 0 if the tray was asked to quit or was not running.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load()
			if err != nil {
				return err
			}
			if cfg.DashboardPort == 0 {
				fmt.Println("gpsync-tray does not appear to have run yet (no dashboard port recorded) -- nothing to quit.")
				return nil
			}
			url := trayDashboardBase(cfg) + "/api/quit"
			req, err := http.NewRequest(http.MethodPost, url, nil)
			if err != nil {
				return err
			}
			// Authenticate by minting our own session row rather than by
			// asking for the dashboard password. Writing to state.sqlite
			// is a STRICTLY STRONGER credential than that password --
			// anyone who can do it could also rewrite the stored hash --
			// so this grants nothing that local filesystem access didn't
			// already grant, and it keeps a password off the command line
			// and out of shell history. Best-effort: with auth switched
			// off the endpoint takes the request either way, so a failure
			// to mint one is not worth aborting over.
			if cfg.DashboardAuthEnabled {
				if token, terr := trayQuitSession(); terr != nil {
					fmt.Printf("  %s could not create a dashboard session (%v) -- trying without one\n", colWarn("note:"), terr)
				} else {
					req.AddCookie(&http.Cookie{Name: "gpsync_session", Value: token})
				}
			}
			// A generous timeout: the response is written BEFORE the
			// shutdown starts (see the handler), so this is only waiting
			// on the request itself, not on uploads draining.
			client := &http.Client{Timeout: 5 * time.Second}
			resp, err := client.Do(req)
			if err != nil {
				fmt.Println("gpsync-tray is not running -- nothing to quit.")
				return nil
			}
			defer resp.Body.Close()
			switch resp.StatusCode {
			case http.StatusAccepted:
				fmt.Println("gpsync-tray is finishing in-flight uploads, then exiting. Watch for the tray icon to disappear.")
				return nil
			case http.StatusUnauthorized:
				return fmt.Errorf("the dashboard rejected this session -- use the tray menu's Quit instead")
			case http.StatusNotFound:
				// The running tray predates this endpoint. Unavoidable
				// exactly once, since the build that answers /api/quit is
				// the one that can't be installed until the old one exits.
				return fmt.Errorf("the running gpsync-tray is older than this command -- quit it from the tray menu once, " +
					"and every version after that can be replaced with this")
			default:
				body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
				return fmt.Errorf("gpsync-tray refused the quit request: HTTP %d %s", resp.StatusCode, strings.TrimSpace(string(body)))
			}
		},
	}
}
