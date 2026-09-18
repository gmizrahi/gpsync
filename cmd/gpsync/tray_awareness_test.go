package main

import (
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gmizrahi/gpsync/internal/config"
)

// freeTCPPort returns a port that is guaranteed unbound at the moment this
// returns -- bind an ephemeral listener, read the port the OS assigned,
// then close it immediately, rather than a hardcoded low port number that
// risks a permission-denied error (a different failure mode than
// "connection refused") or genuinely being in use on some machine.
func freeTCPPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	return port
}

// TestTrayDashboardURL_NoCachedPort_ReturnsEmpty covers gpsync-tray never
// having started even once -- DashboardPort stays 0 until a real port has
// actually been bound (see config.Config's own doc comment), so there's
// nothing to probe.
func TestTrayDashboardURL_NoCachedPort_ReturnsEmpty(t *testing.T) {
	cfg := config.Defaults()
	cfg.DashboardPort = 0
	if got := trayDashboardURL(cfg); got != "" {
		t.Errorf("trayDashboardURL = %q, want empty when DashboardPort is 0", got)
	}
}

// TestTrayDashboardURL_UsesLoopbackRegardlessOfListenAddr proves the probe
// always targets 127.0.0.1, never DashboardListenAddr literally -- "0.0.0.0"
// (the default) isn't itself a valid connect target.
func TestTrayDashboardURL_UsesLoopbackRegardlessOfListenAddr(t *testing.T) {
	cfg := config.Defaults()
	cfg.DashboardListenAddr = "0.0.0.0"
	cfg.DashboardPort = 8080
	want := "http://127.0.0.1:8080/api/status"
	if got := trayDashboardURL(cfg); got != want {
		t.Errorf("trayDashboardURL = %q, want %q", got, want)
	}
}

// TestTrayRunningNotice_DashboardReachable_ReturnsNotice proves detection
// works against a real listening server, not just the URL-building logic.
func TestTrayRunningNotice_DashboardReachable_ReturnsNotice(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cfg := config.Defaults()
	cfg.DashboardPort = srv.Listener.Addr().(*net.TCPAddr).Port

	// The production 300ms is tuned for a person waiting at a terminal, not
	// for a CI runner three times slower than usual -- where even this
	// loopback request to a server in THIS process has missed it, and the
	// probe then reported "not running". What is under test is that a real
	// listening server is detected, not how fast the machine is.
	withProbeTimeout(t, 10*time.Second)

	if notice := trayRunningNotice(cfg); notice == "" {
		t.Error("trayRunningNotice = empty, want a notice when the dashboard IS reachable")
	}
}

// TestTrayRunningNotice_AuthEnabled_StillDetected proves a 401 (the
// dashboard's own session-login rejecting an unauthenticated /api/status
// probe) still counts as "running" -- this only needs to prove something
// is listening and answering like gpsync-tray's own dashboard, not
// successfully read its body.
func TestTrayRunningNotice_AuthEnabled_StillDetected(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "authentication required", http.StatusUnauthorized)
	}))
	defer srv.Close()

	cfg := config.Defaults()
	cfg.DashboardPort = srv.Listener.Addr().(*net.TCPAddr).Port

	if notice := trayRunningNotice(cfg); notice == "" {
		t.Error("trayRunningNotice = empty, want a notice even for a 401 response")
	}
}

// TestTrayRunningNotice_NothingListening_ReturnsEmpty is the ordinary case
// (gpsync-tray isn't running): must return no notice, not an error or a
// misleading positive.
func TestTrayRunningNotice_NothingListening_ReturnsEmpty(t *testing.T) {
	cfg := config.Defaults()
	cfg.DashboardPort = freeTCPPort(t)

	if notice := trayRunningNotice(cfg); notice != "" {
		t.Errorf("trayRunningNotice = %q, want empty when nothing is listening", notice)
	}
}

// withProbeTimeout raises trayRunningNotice's probe timeout for one test.
func withProbeTimeout(t *testing.T, d time.Duration) {
	t.Helper()
	prev := trayProbeTimeout
	trayProbeTimeout = d
	t.Cleanup(func() { trayProbeTimeout = prev })
}
