//go:build windows

// Command gpsync-tray ("gpsync" in its own UI) is a Windows systray
// wrapper around the same watch engine `gpsync watch` uses (internal/engine,
// internal/watcher) -- runs in-process, no shelling out to gpsync.exe. Only
// the app-facing branding says "gpsync"; the CLI binary/commands
// stay `gpsync`/`gpsync.exe`/`gpsync-tray.exe` throughout. A visual
// duplicate-resolution mode remains a fast-follow, not built here.
package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/skratchdot/open-golang/open"

	"github.com/gmizrahi/gpsync/internal/config"
	"github.com/gmizrahi/gpsync/internal/engine"
	"github.com/gmizrahi/gpsync/internal/statedb"
	"github.com/gmizrahi/gpsync/internal/systray"
	"github.com/gmizrahi/gpsync/internal/version"
	"github.com/gmizrahi/gpsync/internal/watchctl"
)

// appName is the app-facing display name -- tray tooltip, dashboard
// title/header, favicon alt text. Never the CLI binary/command name.
const appName = "gpsync"

func main() {
	// Without this, `gpsync-tray.exe --help` printed nothing at all.
	// Attach BEFORE touching flag at all, so both flag.Parse's own -h/--help
	// handling and everything below actually reach the terminal this was
	// launched from, instead of writing to a Stdout/Stderr nothing is
	// watching.
	attachToParentConsole()

	showVersion := flag.Bool("version", false, "Print the version and exit")
	install := flag.Bool("install-autostart", false, "Register gpsync-tray to launch automatically at Windows sign-in, then exit")
	uninstall := flag.Bool("uninstall-autostart", false, "Remove the Windows sign-in autostart entry, then exit")
	flag.Usage = func() {
		out := flag.CommandLine.Output()
		fmt.Fprintf(out, "%s v%s\n\n", appName, version.Version)
		fmt.Fprintf(out, "Usage: %s [flags]\n\n", filepath.Base(os.Args[0]))
		fmt.Fprintln(out, "With no flags, starts the tray icon and watch engine as normal.")
		fmt.Fprintln(out, "\nFlags:")
		flag.PrintDefaults()
	}
	flag.Parse()

	if *showVersion {
		fmt.Printf("gpsync-tray version %s\n", version.Version)
		return
	}
	if *install || *uninstall {
		// This is a -H=windowsgui binary -- stdout only reaches an
		// interactive console some of the time (Go doesn't attach one
		// automatically even when launched from an existing terminal), so
		// the outcome is ALSO logged to tray.log the normal way, not just
		// printed. Neither flag starts the tray/watch engine at all;
		// both are pure one-shot actions.
		setUpLogging()
		if err := runAutostartFlag(*install); err != nil {
			fmt.Println("Error:", err)
			log.Printf("autostart flag: %v", err)
			os.Exit(1)
		}
		return
	}

	// Once, up front, before anything else touches the ledger or starts
	// the watch engine. Autostart launching a copy at sign-in and someone
	// separately double-clicking the exe would otherwise mean two
	// independent circuit breakers sharing one ledger. See
	// acquireSingleInstanceLock's own doc comment.
	if !acquireSingleInstanceLock() {
		setUpLogging()
		log.Println("another gpsync-tray instance is already running -- exiting")
		notifyAlreadyRunning()
		return
	}

	systray.Run(onReady, onExit)
}

// watchController's implementation moved to internal/watchctl (portable,
// no Windows dependencies of its own) so cmd/gpsync's `gpsync dashboard`
// command can share the exact same live-status-tracking type instead of a
// second, independently-maintained copy. watchctl.WatchController is a
// type alias here so every existing wc.* call site in this file (and
// dashboard.go before its own extraction) needed zero changes.
type watchController = watchctl.WatchController

func onReady() {
	systray.SetTitle("")
	systray.SetTooltip(appName + " — syncs your library to Google Photos")

	// No console attached (built with -H=windowsgui) -- a log file is the
	// only error visibility short of the new Open Logs menu item below.
	logPath := setUpLogging()
	if logPath != "" {
		log.Printf("%s starting, logging to %s", appName, logPath)
	}

	cfg, err := config.Load()
	if err != nil {
		log.Printf("loading config: %v", err)
	}
	// See engine.ApplyExtensionOverrides's own doc comment: gpsync-tray never
	// installed the user's own extension/ignore-list overrides at all
	// before this, unlike cmd/gpsync's PersistentPreRunE.
	engine.ApplyExtensionOverrides(cfg)
	db, err := statedb.Open()
	if err != nil {
		log.Printf("opening ledger: %v", err)
		systray.Quit()
		return
	}
	// Silent-death visibility: a panic anywhere in the rest of onReady's
	// own (synchronous) setup gets logged and recorded instead of just
	// killing the process with nothing anywhere to show for it. See
	// recoverAndLog's own doc comment.
	defer recoverAndLog(db, "onReady")
	// Sessions that expired while gpsync-tray wasn't running to prune them
	// lazily (see statedb.ValidateSession) would otherwise just sit in
	// dashboard_sessions forever -- a startup-time sweep instead of a
	// timer, since nothing else in this process runs on a schedule this
	// short.
	if err := db.PruneExpiredSessions(); err != nil {
		log.Printf("pruning expired dashboard sessions: %v", err)
	}

	wc := watchctl.New(db, cfg)

	toggleItem := systray.AddMenuItem("Pause watching", "Pause or resume the watch engine")
	rescanItem := systray.AddMenuItem("Rescan Now", "Check the whole backlog immediately, without waiting for the next heartbeat")
	systray.AddSeparator()
	dashItem := systray.AddMenuItem("Open Dashboard", "Open the live status page in your browser")
	browseItem := systray.AddMenuItem("Browse Files", "See pending/failed files in your browser")
	settingsItem := systray.AddMenuItem("Settings", "Open the settings page in your browser")
	logsItem := systray.AddMenuItem("Open Logs", "Open the log file")
	systray.AddSeparator()
	quitItem := systray.AddMenuItem("Quit", "Stop watching and exit")

	addr, stopDashboard, err := startDashboard(db, wc)
	if err != nil {
		log.Printf("starting dashboard server: %v", err)
	}

	// Double-clicking the tray icon opens the dashboard, using the same
	// open logic as dashItem's click handler below --
	// addr is captured by reference (this closure only ever actually runs
	// well after startDashboard above has set it).
	systray.SetOnDoubleClick(func() {
		if addr == "" {
			log.Println("dashboard server never started successfully")
			return
		}
		if err := open.Start("http://" + addr); err != nil {
			log.Printf("opening dashboard (double-click): %v", err)
		}
	})

	wc.Start()
	systray.SetIcon(iconFor(computeTrayState(wc)))
	safeGo(db, "runTrayIconTicker", func() { runTrayIconTicker(wc) })

	safeGo(db, "menu-click loop", func() {
		for {
			select {
			case <-toggleItem.ClickedCh:
				if wc.IsRunning() {
					wc.Stop()
					toggleItem.SetTitle("Resume watching")
					rescanItem.Disable()
				} else {
					wc.Start()
					toggleItem.SetTitle("Pause watching")
					rescanItem.Enable()
				}
			case <-rescanItem.ClickedCh:
				wc.RescanNow()
			case <-dashItem.ClickedCh:
				if addr == "" {
					log.Println("dashboard server never started successfully")
					continue
				}
				if err := open.Start("http://" + addr); err != nil {
					log.Printf("opening dashboard: %v", err)
				}
			case <-browseItem.ClickedCh:
				if addr == "" {
					log.Println("dashboard server never started successfully")
					continue
				}
				if err := open.Start("http://" + addr + "/browse"); err != nil {
					log.Printf("opening browse: %v", err)
				}
			case <-settingsItem.ClickedCh:
				if addr == "" {
					log.Println("dashboard server never started successfully")
					continue
				}
				if err := open.Start("http://" + addr + "/settings"); err != nil {
					log.Printf("opening settings: %v", err)
				}
			case <-logsItem.ClickedCh:
				if logPath == "" {
					log.Println("no log file available to open")
					continue
				}
				if err := open.Start(logPath); err != nil {
					log.Printf("opening log file: %v", err)
				}
			case <-quitItem.ClickedCh:
				switch confirmQuit() {
				case quitCancelled:
					continue
				case quitNow:
					// Deliberately no OTHER cleanup -- immediate process
					// exit, matching cmd/gpsync's own Ctrl+C precedent
					// (handleInterrupts' os.Exit). Any upload mid-flight is
					// simply cut off; nothing was committed to the ledger
					// for it yet, so the next run resumes it cleanly.
					//
					// systray.RemoveIcon() IS still needed here, though:
					// os.Exit() terminates the process without ever
					// letting the systray message loop process a normal
					// Quit() (which only posts WM_CLOSE for that loop to
					// handle later) -- without this, the tray icon's
					// Shell_NotifyIcon entry is never deleted, and Windows
					// leaves the icon sitting in the notification area
					// until Explorer happens to notice the owning process
					// is gone, typically only on the next mouse hover over
					// it -- so without this the icon lingers after quitting
					// until the pointer passes over it. This is a single
					// synchronous Shell_NotifyIcon call,
					// not the multi-second wait "Quit gracefully" involves,
					// so "Quit Now" stays instant.
					systray.RemoveIcon()
					os.Exit(0)
				case quitGracefully:
					// wc.Stop() cancels the same ctx dispatchPass now
					// checks between files (see Uploader.Run's
					// AbortInterrupted branch): no new upload starts, but
					// whatever's already transferring finishes normally
					// before this returns -- which can take a few real
					// seconds (an in-flight file finishing), not the
					// instant "Quit now" is. Relabel/disable the item first
					// so the tray still reflects that something is
					// happening instead of looking frozen for that window.
					quitItem.SetTitle("Quitting…")
					quitItem.Disable()
					gracefulQuit(wc, db, stopDashboard)
					return
				}
			}
		}
	})
}

func onExit() {}

// gracefulQuit is the "finish what you started" shutdown: stop starting new
// uploads, let whatever is already transferring complete, then close up and
// exit. Shared by the tray's Quit dialog ("No") and the dashboard's
// /api/quit, which exists so the binary can be replaced without cutting a
// transfer off: ask the running tray to finish, wait for it to exit, then
// swap the file.
//
// wc.Stop() cancels the same ctx dispatchPass checks between files (see
// Uploader.Run's AbortInterrupted branch), so it returns only once the
// in-flight file is done -- real seconds, not instant.
//
// Ordering matters: the dashboard server is stopped BEFORE the database is
// closed, or a request still being served could reach a closed *statedb.DB.
// Reached from /api/quit on its own goroutine, after that handler has
// already written its response -- stopDashboard() waits for in-flight
// requests to finish, so calling it from inside one would wait on itself.
func gracefulQuit(wc *watchController, db *statedb.DB, stopDashboard func()) {
	wc.Stop()
	if stopDashboard != nil {
		stopDashboard()
	}
	db.Close()
	systray.Quit()
}

// trayState is the tray icon's live status. Priority matters: checked in
// this exact order by computeTrayState, first match wins.
type trayState int

const (
	stateIdle trayState = iota
	stateSyncing
	stateThrottled
	statePaused
)

// computeTrayState reads wc's existing accessors (all already built for
// the dashboard) to decide what the icon should show right now.
// Throttled is checked before syncing deliberately: setCurrentFolder isn't
// cleared until a whole cycle -- including any circuit-breaker pause
// inside it -- finishes, so a folder stays "current" for the entire
// backoff wait too. Checking syncing first would show a throttled pause as
// "actively syncing", which says nothing about why no bytes are moving.
func computeTrayState(wc *watchController) trayState {
	if !wc.IsRunning() {
		return statePaused
	}
	if wc.CurrentBackoff() != nil {
		return stateThrottled
	}
	if wc.CurrentFolder() != "" {
		return stateSyncing
	}
	return stateIdle
}

func iconFor(s trayState) []byte {
	switch s {
	case statePaused:
		return iconPausedICO
	case stateThrottled:
		return iconThrottledICO
	case stateSyncing:
		return iconSyncingICO
	default:
		return iconIdleICO
	}
}

// runTrayIconTicker updates the tray icon roughly once a second, only
// calling SetIcon on an actual state change (not every tick) -- runs for
// the lifetime of the process, same as the menu's own event-handling
// goroutine; nothing to stop it on Quit since the process exits anyway.
func runTrayIconTicker(wc *watchController) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	last := computeTrayState(wc)
	for range ticker.C {
		if s := computeTrayState(wc); s != last {
			systray.SetIcon(iconFor(s))
			last = s
		}
	}
}

// setUpLogging redirects the standard logger to ~/.gpsync/tray.log, the same
// state directory everything else in this project already uses. Returns
// the path, or "" if it couldn't be set up (falls back to the log
// package's default -- discarded with no console attached, but better
// than a crash over logging itself).
func setUpLogging() string {
	if err := os.MkdirAll(statedb.StateDir, 0o755); err != nil {
		return ""
	}
	path := filepath.Join(statedb.StateDir, "tray.log")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return ""
	}
	log.SetOutput(f)
	log.SetFlags(log.Ldate | log.Ltime)
	return path
}
