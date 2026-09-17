package dashboard

import (
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"

	"github.com/gmizrahi/gpsync/internal/config"
	"github.com/gmizrahi/gpsync/internal/engine"
	"github.com/gmizrahi/gpsync/internal/statedb"
)

// Handler builds the dashboard's full route table wrapped in the
// CSRF/session-auth middleware, as a plain http.Handler -- a real
// net.Listener/http.Server (gpsync-tray's own startDashboard) or an
// httptest.Server/httptest.NewRecorder (tests) can both drive it
// identically. wc's accessor methods are called fresh on every request,
// not cached, so the page always reflects current state. See Options for
// the handful of things (app branding, the favicon, the registry-backed
// autostart toggle, how to end the process after a restore) that used to
// pin this code to Windows.
func Handler(db *statedb.DB, wc Controller, opts Options) http.Handler {
	if opts.AppName != "" {
		appName = opts.AppName
	}
	favicon = opts.Favicon
	autostartCtrl = opts.Autostart
	shutdown = opts.Shutdown
	quitGracefully = opts.QuitGracefully
	// Fresh per Handler call, deliberately not a package var -- see
	// loginAttemptTracker's own doc comment: two servers (or two tests)
	// must never share lockout state.
	loginAttempts := newLoginAttemptTracker()

	cfg := wc.Config()

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte(pageShell(db, "Status", "status", statusPageBody, wc.Config().Theme, wc.Config().DashboardAuthEnabled)))
	})
	mux.HandleFunc("/favicon.ico", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/x-icon")
		w.Write(favicon)
	})
	mux.HandleFunc("/login", func(w http.ResponseWriter, r *http.Request) {
		handleLogin(w, r, db, wc.Config(), loginAttempts)
	})
	mux.HandleFunc("/logout", func(w http.ResponseWriter, r *http.Request) {
		handleLogout(w, r, db)
	})
	mux.HandleFunc("/api/status", func(w http.ResponseWriter, r *http.Request) {
		resp, err := buildStatus(db, wc)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	})
	mux.HandleFunc("/api/toggle", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		// wc.Stop() blocks until the engine actually stops -- bounded now
		// (see watchController.Stop's doc comment) but can still take a
		// few real seconds if a file is mid-transfer. That's fine here:
		// this handler's own goroutine blocking doesn't hold up any other
		// concurrent request to the dashboard (net/http gives each request
		// its own goroutine), and the page's toggle button disables itself
		// for the round trip.
		if wc.IsRunning() {
			wc.Stop()
		} else {
			wc.Start()
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("/api/autostart-toggle", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		if autostartCtrl == nil {
			http.Error(w, "autostart not supported here", http.StatusNotImplemented)
			return
		}
		installed, err := autostartCtrl.Installed()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if installed {
			err = autostartCtrl.Uninstall()
		} else {
			exePath, exeErr := os.Executable()
			if exeErr != nil {
				http.Error(w, exeErr.Error(), http.StatusInternalServerError)
				return
			}
			err = autostartCtrl.Install(exePath)
		}
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("/api/retry-now", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		// Non-blocking, unlike /api/toggle -- RequestRetryNow just wakes
		// up a wait already in progress, or sets a flag the upload
		// pipeline checks at its own next pass boundary otherwise (see
		// uploader.SkipSignal's doc comment), so this always returns
		// immediately.
		wc.RequestRetryNow()
		w.WriteHeader(http.StatusNoContent)
	})
	// /api/quit ends the app the way the tray's Quit dialog does when
	// someone picks "No" -- in-flight uploads finish, no new ones start.
	// It exists so the binary can be swapped without cutting a transfer
	// off: ask the tray to finish, wait for it to exit, replace the file.
	//
	// Behind the same auth and CSRF middleware as every other mutating
	// endpoint. With the dashboard bound to loopback by default, "can
	// reach this port" already means "is on this machine", the same trust
	// boundary the tray menu itself sits behind.
	mux.HandleFunc("/api/quit", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		if quitGracefully == nil {
			http.Error(w, "this build has no graceful-quit hook", http.StatusNotImplemented)
			return
		}
		// Answer BEFORE quitting: the callback stops this very server, so
		// a caller waiting on the response would otherwise be racing the
		// shutdown that its own request is holding open.
		w.WriteHeader(http.StatusAccepted)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		go quitGracefully()
	})
	mux.HandleFunc("/api/skip-file", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		// Cancels ONE specific file's active byte-upload transfer, right
		// now, regardless of throttle state -- distinct from
		// /api/retry-now above (uploader.SkipSignal, which only affects
		// an in-progress circuit-breaker wait). See
		// watchController.CancelFile's doc comment.
		path := r.FormValue("path")
		if path == "" {
			http.Error(w, "missing path", http.StatusBadRequest)
			return
		}
		wc.CancelFile(path)
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("/settings", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			handleSettingsSave(w, r, wc)
			return
		}
		cfg, err := config.Load()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		q := r.URL.Query()
		page, err := renderSettingsPage(db, cfg, q.Get("saved") == "1", q.Get("restart") == "1", q.Get("error"))
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte(page))
	})
	mux.HandleFunc("/statistics", func(w http.ResponseWriter, r *http.Request) {
		// The granularity selector is a plain query param on a
		// server-rendered page (?activity=daily|weekly|monthly), matching
		// how Browse already carries its own sort/filter state -- no
		// client-side state to get out of sync with what's displayed, and
		// each view is a shareable, bookmarkable URL.
		activity := engine.ParseActivityGranularity(r.URL.Query().Get("activity"))
		page, err := renderStatisticsPage(db, wc.Config().Theme, wc.Config().DashboardAuthEnabled, activity)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte(page))
	})
	mux.HandleFunc("/browse", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		kind := engine.FileListKind(q.Get("type"))
		if kind == "" {
			kind = engine.FileListPending
		}
		offset, _ := strconv.Atoi(q.Get("offset"))
		sortKey := engine.SortKey(q.Get("sort"))
		sortDesc := q.Get("dir") == "desc"
		page, err := renderBrowsePage(db, kind, q.Get("q"), sortKey, sortDesc, offset, wc.Config().Theme, wc.Config().DashboardAuthEnabled)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte(page))
	})

	mux.HandleFunc("/backup", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		page, err := renderBackupPage(db, wc.Config(), q.Get("created") == "1", q.Get("files"), q.Get("error"))
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte(page))
	})
	mux.HandleFunc("/signin", func(w http.ResponseWriter, r *http.Request) {
		cfg := wc.Config()
		page, err := renderSignInPage(db, cfg, appName,
			r.URL.Query().Get("imported"), r.URL.Query().Get("error"))
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte(page))
	})
	mux.HandleFunc("/signin/import-rclone", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		handleSignInImportRclone(w, r, db, wc)
	})
	mux.HandleFunc("/backup/create", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		handleBackupCreate(w, r, db, wc)
	})
	mux.HandleFunc("/backup/restore", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			handleBackupRestore(w, r, db, wc)
			return
		}
		cfg := wc.Config()
		path, ok := backupFileFromRequest(cfg, r.URL.Query().Get("file"))
		if !ok {
			http.Redirect(w, r, "/backup?error="+url.QueryEscape("that backup no longer exists"), http.StatusSeeOther)
			return
		}
		page, err := renderBackupRestoreConfirmPage(db, filepath.Base(path), cfg.Theme, "", cfg.DashboardAuthEnabled)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte(page))
	})
	mux.HandleFunc("/duplicates", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		index, _ := strconv.Atoi(q.Get("i"))
		moved, _ := strconv.Atoi(q.Get("moved"))
		freed, _ := strconv.ParseInt(q.Get("freed"), 10, 64)
		page, err := renderDuplicatesPage(db, wc.Config(), index, moved, freed, q.Get("error"))
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte(page))
	})
	mux.HandleFunc("/duplicates/resolve", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		handleDuplicatesResolve(w, r, db, wc)
	})
	mux.HandleFunc("/duplicates/thumb", func(w http.ResponseWriter, r *http.Request) {
		handleDuplicatesThumb(w, r, db)
	})
	mux.HandleFunc("/originals", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		index, _ := strconv.Atoi(q.Get("i"))
		page, err := renderOriginalsPage(db, wc.Config(), index, q.Get("error"))
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte(page))
	})
	mux.HandleFunc("/originals/resolve", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		handleOriginalsResolve(w, r, db)
	})
	mux.HandleFunc("/originals/thumb", func(w http.ResponseWriter, r *http.Request) {
		handleOriginalsThumb(w, r, db)
	})
	mux.HandleFunc("/originals/raw", func(w http.ResponseWriter, r *http.Request) {
		handleOriginalsRaw(w, r, db)
	})

	return csrfMiddleware(sessionAuthMiddleware(mux, db, cfg))
}
