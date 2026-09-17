package main

import (
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	// Aliased: this package has its own `dashboard` type, the terminal
	// renderer in dashboard.go.
	webdashboard "github.com/gmizrahi/gpsync/internal/dashboard"

	"github.com/gmizrahi/gpsync/internal/config"
	"github.com/gmizrahi/gpsync/internal/statedb"
	"github.com/gmizrahi/gpsync/internal/watchctl"
	"github.com/spf13/cobra"
)

// dashboardCmd is `gpsync watch`'s live-web-dashboard sibling -- the same
// baseline+watch loop, but driven by watchctl.WatchController (the exact
// type gpsync-tray's systray wrapper uses) instead of gpsync watch's own
// colored-terminal implementation, so it can serve internal/dashboard's
// full page/status/settings UI over HTTP instead of printing to a
// terminal. Deliberately a SEPARATE command rather than a `--dashboard`
// flag on `gpsync watch` itself: the two have genuinely different
// architectures (a live-status-tracking controller vs. plain callback
// closures), and keeping them separate means `gpsync watch` -- the
// existing, well-exercised command -- is completely unaffected by this
// one's addition.
func dashboardCmd() *cobra.Command {
	var listenAddr string
	var port int
	cmd := &cobra.Command{
		Use:   "dashboard [folders...]",
		Short: "Sync continuously with a web dashboard",
		Long: "Runs the same loop as gpsync watch and serves the gpsync-tray dashboard over HTTP until stopped with Ctrl+C. Uses the configured source folders if none are given.\n" +
			"\n" +
			"The address, port and login come from config.toml. The dashboard listens only on this computer unless dashboard login is enabled.\n" +
			"\n" +
			"Examples:\n" +
			"  gpsync dashboard\n" +
			"  gpsync dashboard --port 10925",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load()
			if err != nil {
				return err
			}
			patterns := args
			if len(patterns) == 0 {
				patterns = cfg.SourceFolders
			}
			if len(patterns) == 0 {
				return fmt.Errorf("no folders given and none configured — run `gpsync setup` or pass folders explicitly")
			}
			// watchctl.WatchController reads cfg.SourceFolders internally
			// (unlike gpsync watch's own command, which threads patterns
			// through its own local closures directly) -- folders given
			// as positional args must be written back here, or a
			// command-line-only invocation would watch nothing at all.
			cfg.SourceFolders = patterns
			if listenAddr != "" {
				cfg.DashboardListenAddr = listenAddr
			}
			if port != 0 {
				cfg.DashboardPort = port
			}
			// --listen is applied AFTER config.Load, so it would bypass the
			// load-time guard that keeps an unauthenticated dashboard off
			// the network. Same rule as the Settings page, enforced here.
			if config.BindNeedsAuth(cfg.DashboardListenAddr) && !cfg.DashboardAuthEnabled {
				return fmt.Errorf("listening on %s would expose the dashboard to the network without authentication -- "+
					"enable dashboard auth first, or listen on %s", cfg.DashboardListenAddr, config.DashboardListenLocal)
			}

			db, err := statedb.Open()
			if err != nil {
				return err
			}
			defer db.Close()
			defer handleInterrupts(db)()
			// Same startup-time sweep gpsync-tray's own onReady does --
			// a long-lived `gpsync dashboard` invocation is exactly the
			// case sessions accumulate for otherwise (nothing else here
			// prunes them lazily except an actual login attempt).
			if err := db.PruneExpiredSessions(); err != nil {
				fmt.Fprintf(os.Stderr, "pruning expired dashboard sessions: %v\n", err)
			}

			ctrl := watchctl.New(db, cfg)
			// Pins this controller to `patterns` for its whole lifetime --
			// otherwise the dashboard's own Settings page (loading a
			// fresh config.Config from disk, which for a CLI-only
			// invocation has an empty SourceFolders) silently overwrites
			// this on ANY save, restarts the engine, and it gives up with
			// "no source folders configured" and no error visible in the
			// UI. See SetSourceFoldersOverride's own doc comment.
			ctrl.SetSourceFoldersOverride(patterns)
			ctrl.Start()
			defer ctrl.Stop()

			host := cfg.DashboardListenAddr
			if host == "" {
				// Fail closed, matching gpsync-tray.
				host = config.DashboardListenLocal
			}
			ln, err := net.Listen("tcp", fmt.Sprintf("%s:%d", host, cfg.DashboardPort))
			if err != nil {
				return fmt.Errorf("binding dashboard listener: %w", err)
			}
			actualPort := ln.Addr().(*net.TCPAddr).Port

			// Resolved before serving so a certificate problem is a clear
			// startup error, not a listener that accepts connections and
			// then fails every handshake.
			tlsSetup, err := webdashboard.TLSSetupFor(cfg, statedb.StateDir, webdashboard.DefaultCertHosts(), time.Now())
			if err != nil {
				return fmt.Errorf("dashboard TLS: %w", err)
			}

			handler := webdashboard.Handler(db, ctrl, webdashboard.Options{AppName: "gpsync"})
			// Header/idle timeouts bound a stalled or slow-loris client.
			// WriteTimeout stays unset on purpose: the dashboard serves
			// full-resolution originals, and a large video download must
			// not be cut off mid-transfer.
			srv := &http.Server{
				Handler:           handler,
				ReadHeaderTimeout: 10 * time.Second,
				ReadTimeout:       30 * time.Second,
				IdleTimeout:       2 * time.Minute,
			}
			go func() {
				if serveErr := srv.Serve(ln); serveErr != nil && serveErr != http.ErrServerClosed {
					fmt.Fprintf(os.Stderr, "dashboard server: %v\n", serveErr)
				}
			}()
			defer srv.Close()

			// An additional listener, not a replacement -- see the tray's
			// startDashboard for why loopback stays plain HTTP.
			httpsPort := 0
			if tlsSetup.Enabled {
				tlsLn, terr := net.Listen("tcp", fmt.Sprintf("%s:%d", host, cfg.DashboardHTTPSPort))
				if terr != nil {
					return fmt.Errorf("binding dashboard HTTPS listener: %w", terr)
				}
				httpsPort = tlsLn.Addr().(*net.TCPAddr).Port
				tlsSrv := &http.Server{
					Handler:           handler,
					ReadHeaderTimeout: 10 * time.Second,
					ReadTimeout:       30 * time.Second,
					IdleTimeout:       2 * time.Minute,
				}
				go func() {
					serveErr := tlsSrv.ServeTLS(tlsLn, tlsSetup.Files.CertPath, tlsSetup.Files.KeyPath)
					if serveErr != nil && serveErr != http.ErrServerClosed {
						fmt.Fprintf(os.Stderr, "dashboard HTTPS server: %v\n", serveErr)
					}
				}()
				defer tlsSrv.Close()
			}

			fmt.Println(colDim(strings.Repeat("─", separatorWidth)))
			fmt.Printf("%s http://127.0.0.1:%d — Ctrl+C to stop.\n", colHeader("Dashboard:"), actualPort)
			if tlsSetup.Enabled {
				fmt.Printf("%s https://%s:%d\n", colHeader("     HTTPS:"), host, httpsPort)
				if !tlsSetup.Files.Supplied {
					// Printed every run, not only when generated: this is
					// what the user compares against the browser's
					// warning, and they need it in front of them at that
					// prompt.
					fmt.Printf("%s %s\n", colHeader("Certificate:"), tlsSetup.Files.Fingerprint)
				}
			}
			fmt.Printf("%s %d folder tree(s) configured.\n", colHeader("Watching:"), len(patterns))

			// Runs until handleInterrupts' own signal handler calls
			// os.Exit directly (the same Ctrl+C convention every other
			// long-running command in this file already uses, e.g.
			// gpsync watch's own RunWatchLoop call with a nil interrupt
			// channel -- there is nothing graceful to return TO here).
			select {}
		},
	}
	cmd.Flags().StringVar(&listenAddr, "listen", "", "Listen address for this run, e.g. 127.0.0.1")
	cmd.Flags().IntVar(&port, "port", 0, "Port for this run (0 picks a free port)")
	return cmd
}

// ── scan ────────────────────────────────────────────────────────────────
