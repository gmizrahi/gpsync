package main

import (
	"bytes"
	"github.com/gmizrahi/gpsync/internal/statedb"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gmizrahi/gpsync/internal/config"
	webdash "github.com/gmizrahi/gpsync/internal/dashboard"
	"github.com/gmizrahi/gpsync/internal/engine"
)

// quitTestController is the smallest webdash.Controller the handler needs here.
type quitTestController struct{ webdash.Controller }

func (quitTestController) Config() config.Config { return config.Defaults() }

// runTrayQuit points gpsync at a dashboard served by h and runs `gpsync
// tray-quit`, returning what it printed.
func runTrayQuit(t *testing.T, h http.Handler) string {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	_, port, err := net.SplitHostPort(srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Defaults()
	if cfg.DashboardPort, err = atoiPort(port); err != nil {
		t.Fatal(err)
	}
	if err := config.Save(cfg); err != nil {
		t.Fatal(err)
	}
	r, w, _ := os.Pipe()
	stdout := os.Stdout
	os.Stdout = w
	cmd := trayQuitCmd()
	cmd.SetArgs(nil)
	runErr := cmd.Execute()
	w.Close()
	os.Stdout = stdout
	out, _ := io.ReadAll(r)
	if runErr != nil {
		t.Fatalf("tray-quit: %v\n%s", runErr, out)
	}
	return string(out)
}

func atoiPort(s string) (int, error) { return strconv.Atoi(s) }

// Issue #67: gpsync's own CLI uses /api/v1 with a token it mints for the
// one command -- no login, no configuration -- and nothing outlives it.
func TestTrayQuit_UsesASelfMintedTokenThatDoesNotOutliveTheCommand(t *testing.T) {
	db := openInfoTestDB(t)
	quit := make(chan struct{})
	var mu sync.Mutex
	var seen string
	real := webdash.Handler(db, quitTestController{}, webdash.Options{QuitGracefully: func() { close(quit) }})
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/control/quit" {
			mu.Lock()
			seen = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			mu.Unlock()
		}
		real.ServeHTTP(w, r)
	})

	out := runTrayQuit(t, h)
	if !strings.Contains(out, "finishing in-flight uploads") {
		t.Fatalf("tray-quit output: %q", out)
	}
	select {
	case <-quit:
	case <-time.After(5 * time.Second):
		t.Fatal("the tray was never asked to quit")
	}
	mu.Lock()
	token := seen
	mu.Unlock()
	if !strings.HasPrefix(token, engine.APITokenPrefix) {
		t.Fatalf("tray-quit reached /api/v1/control/quit with %q, want a gps_ bearer token", token)
	}
	if _, err := engine.AuthenticateAPIToken(db, token, time.Now()); err == nil {
		t.Error("the token tray-quit minted still works after the command finished")
	}
	if list, _ := db.ListAPITokens(); len(list) != 0 {
		t.Errorf("tray-quit left %d listed token(s) behind", len(list))
	}
}

// Mid-upgrade: a new CLI against an old tray, which has no /api/v1 and
// answers its old /api/quit instead.
func TestTrayQuit_FallsBackToTheOldRouteForAnOlderTray(t *testing.T) {
	openInfoTestDB(t)
	var hits bytes.Buffer
	old := http.NewServeMux()
	old.HandleFunc("/api/quit", func(w http.ResponseWriter, r *http.Request) {
		hits.WriteString("legacy ")
		w.WriteHeader(http.StatusAccepted)
	})
	out := runTrayQuit(t, old)
	if !strings.Contains(out, "finishing in-flight uploads") || hits.String() != "legacy " {
		t.Errorf("against an old tray: output %q, legacy route hit %q", out, hits.String())
	}
}

// A ledger that can't be opened is an error, not "the tray isn't running":
// only a refused connection means that. Reporting the comforting answer for
// the wrong reason would leave a running tray running.
func TestTrayQuit_ALedgerFailureIsAnErrorNotNotRunning(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("GPSYNC_STATE_DIR", dir)
	statedb.StateDir = dir
	statedb.StateDBPath = filepath.Join(dir, "state.sqlite")
	cfg := config.Defaults()
	cfg.DashboardPort = 1
	if err := config.Save(cfg); err != nil {
		t.Fatal(err)
	}
	// A directory where the database file should be: it can't be opened.
	if err := os.Mkdir(statedb.StateDBPath, 0o700); err != nil {
		t.Fatal(err)
	}
	cmd := trayQuitCmd()
	cmd.SetArgs(nil)
	cmd.SilenceUsage, cmd.SilenceErrors = true, true
	if err := cmd.Execute(); err == nil {
		t.Error("tray-quit reported success with an unopenable ledger")
	}
}
