//go:build windows

package main

import (
	"fmt"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/gmizrahi/gpsync/internal/config"
	"github.com/gmizrahi/gpsync/internal/dashboard"
	"github.com/gmizrahi/gpsync/internal/statedb"
	"github.com/gmizrahi/gpsync/internal/systray"
)

// startDashboard binds the HTTP server to the interface/port
// config.Config.DashboardListenAddr/DashboardPort name (loopback unless
// dashboard auth is enabled -- see config.BindNeedsAuth), optionally
// protected by a session-cookie login (see internal/dashboard's
// sessionAuthMiddleware). A port the OS picked on first run is persisted
// so a restart reuses it; a port the user set is never overwritten (see
// dashboard.ListenPreferred).
//
// The actual route table/handler logic lives in internal/dashboard now
// (extracted so it's testable from WSL -- see that package's own doc
// comment) -- this function's only remaining job is the Windows-tied
// wiring: binding the real listener, and telling dashboard.Handler how to
// do the four things that used to pin it to this package (app branding,
// the embedded tray icon as favicon, the registry-backed autostart
// toggle, and cleanly ending the process after a destructive backup
// restore).
//
// Returns a full URL a LOCAL browser should use (always 127.0.0.1,
// regardless of which interface(s) the listener actually bound -- 0.0.0.0
// itself isn't a meaningful destination to navigate to) plus a shutdown
// func. The scheme is part of it deliberately: callers used to prepend
// "http://" themselves, which would open plain HTTP against a TLS
// listener the moment dashboard_tls_mode was turned on.
func startDashboard(db *statedb.DB, wc *watchController) (addr string, stop func(), err error) {
	cfg := wc.Config()
	if cfg.DashboardBindGuarded {
		// config.Load pulled this back to loopback: the configured
		// address would have served an unauthenticated dashboard to the
		// network, and that page edits settings, moves files and runs
		// backup/restore. Said out loud rather than silently corrected,
		// so it reads as a deliberate refusal and not as the setting
		// having been ignored.
		log.Printf("dashboard: %q needs authentication to be safe, so binding to %s instead -- "+
			"turn on dashboard auth in Settings to serve it on the network",
			config.DashboardListenAll, config.DashboardListenLocal)
	}
	host := cfg.DashboardListenAddr
	if host == "" {
		// Never DashboardListenAll: an unset address must fail closed.
		host = config.DashboardListenLocal
	}
	// 5s of retries covers a restart racing the previous instance's exit.
	// A port that stays unavailable longer than that (on this machine, a
	// Hyper-V port reservation) gets a temporary port for this session
	// only -- never written back over the configured one.
	res, err := dashboard.ListenPreferred(host, cfg.DashboardPort, 5*time.Second)
	if err != nil {
		return "", nil, err
	}
	ln, port := res.Listener, res.Port
	if res.FallbackReason != nil {
		log.Printf("dashboard: configured port %d is unavailable (%v) -- serving on %d for this session only; "+
			"config.toml still says %d. On Windows, Hyper-V/WSL reserves blocks of ports 49152-65535 and moves them "+
			"on reboot, so a port below 49152 avoids this.", cfg.DashboardPort, res.FallbackReason, port, cfg.DashboardPort)
	}
	if res.Persist {
		cfg.DashboardPort = port
		wc.SetConfig(cfg)
		if serr := config.Save(cfg); serr != nil {
			log.Printf("saving dashboard port: %v", serr)
		}
	}

	// stop (a named return) is captured by the QuitGracefully closure
	// below and assigned just after the server exists -- /api/quit has to
	// be able to shut down the very server that served it, which is only
	// knowable here.
	handler := dashboard.Handler(db, wc, dashboard.Options{
		AppName:        appName,
		Favicon:        iconIdleICO,
		Autostart:      autostartAdapter{},
		Shutdown:       func() { systray.RemoveIcon(); os.Exit(0) },
		QuitGracefully: func() { gracefulQuit(wc, db, stop) },
	})

	// Resolved before the server starts so a certificate problem is a
	// startup error with a reason, not a listener that accepts connections
	// and then fails every handshake.
	tlsSetup, err := dashboard.TLSSetupFor(cfg, statedb.StateDir, dashboard.DefaultCertHosts(), time.Now())
	if err != nil {
		ln.Close()
		return "", nil, fmt.Errorf("dashboard TLS: %w", err)
	}
	if tlsSetup.Enabled {
		if tlsSetup.Files.Generated {
			log.Printf("dashboard: generated a self-signed certificate at %s", tlsSetup.Files.CertPath)
		}
		// Logged every start, not just on generation: it is what the user
		// compares against the browser's warning, and they need it in
		// front of them at the moment they hit that prompt.
		log.Printf("dashboard: TLS certificate SHA-256 %s (expires %s)",
			tlsSetup.Files.Fingerprint, tlsSetup.Files.NotAfter.Format("2006-01-02"))
	}

	// See cmd/gpsync's dashboard command for why WriteTimeout stays unset.
	newServer := func() *http.Server {
		return &http.Server{
			Handler:           handler,
			ReadHeaderTimeout: 10 * time.Second,
			ReadTimeout:       30 * time.Second,
			IdleTimeout:       2 * time.Minute,
		}
	}

	srv := newServer()
	stop = func() { srv.Close() }
	go func() {
		if serveErr := srv.Serve(ln); serveErr != nil && serveErr != http.ErrServerClosed {
			log.Printf("dashboard server: %v", serveErr)
		}
	}()

	// HTTPS is an ADDITIONAL listener, not a replacement: the tray and the
	// CLI reach this dashboard over loopback, where plain HTTP costs
	// nothing and needs no certificate trusted. TLS is for reaching it
	// from elsewhere.
	if tlsSetup.Enabled {
		tlsRes, terr := dashboard.ListenPreferred(host, cfg.DashboardHTTPSPort, 5*time.Second)
		if terr != nil {
			log.Printf("dashboard: HTTPS listener could not bind (%v) -- HTTP is still serving on %d", terr, port)
		} else {
			if tlsRes.Persist {
				cfg.DashboardHTTPSPort = tlsRes.Port
				wc.SetConfig(cfg)
				if serr := config.Save(cfg); serr != nil {
					log.Printf("saving dashboard HTTPS port: %v", serr)
				}
			}
			tlsSrv := newServer()
			httpStop := stop
			stop = func() { httpStop(); tlsSrv.Close() }
			log.Printf("dashboard: HTTPS on port %d", tlsRes.Port)
			go func() {
				serveErr := tlsSrv.ServeTLS(tlsRes.Listener, tlsSetup.Files.CertPath, tlsSetup.Files.KeyPath)
				if serveErr != nil && serveErr != http.ErrServerClosed {
					log.Printf("dashboard HTTPS server: %v", serveErr)
				}
			}()
		}
	}

	// Always the HTTP URL: this is what the tray opens, and it is a
	// loopback address on the same machine.
	return fmt.Sprintf("http://127.0.0.1:%d", port), stop, nil
}
