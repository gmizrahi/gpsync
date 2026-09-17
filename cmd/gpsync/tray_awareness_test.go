package main

import (
	"crypto/tls"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gmizrahi/gpsync/internal/config"
	webdashboard "github.com/gmizrahi/gpsync/internal/dashboard"
	"github.com/gmizrahi/gpsync/internal/statedb"
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

// The scheme has to follow dashboard_tls_mode, or every request to a
// running tray goes to the wrong protocol.
func TestTrayDashboardBase_FollowsTLSMode(t *testing.T) {
	cfg := config.Defaults()
	cfg.DashboardPort = 8765

	if got := trayDashboardBase(cfg); got != "http://127.0.0.1:8765" {
		t.Errorf("base = %q, want http with TLS off", got)
	}
	for _, mode := range []string{config.TLSModeSelfSigned, config.TLSModeFiles, config.TLSModeAcme} {
		cfg.DashboardTLSMode = mode
		if got := trayDashboardBase(cfg); got != "https://127.0.0.1:8765" {
			t.Errorf("base for mode %q = %q, want https", mode, got)
		}
	}
}

// The real point of trayHTTPClient: against a dashboard serving the
// certificate gpsync generated, the probe must succeed by VERIFYING it,
// not by skipping verification. A default client cannot do this, which is
// what silently broke tray detection and `gpsync tray-quit` under TLS.
func TestTrayRunningNotice_SelfSignedDashboard_IsDetected(t *testing.T) {
	stateDir := t.TempDir()
	t.Setenv("GPSYNC_STATE_DIR", stateDir)
	// statedb.StateDir is resolved once at package init, so point it at the
	// scratch dir the certificate is actually written into.
	origStateDir := statedb.StateDir
	statedb.StateDir = stateDir
	t.Cleanup(func() { statedb.StateDir = origStateDir })

	files, err := webdashboard.EnsureTLSFiles(stateDir, "", "", webdashboard.DefaultCertHosts(), time.Now())
	if err != nil {
		t.Fatalf("EnsureTLSFiles: %v", err)
	}
	cert, err := tls.LoadX509KeyPair(files.CertPath, files.KeyPath)
	if err != nil {
		t.Fatalf("LoadX509KeyPair: %v", err)
	}

	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{cert}}
	srv.StartTLS()
	defer srv.Close()

	cfg := config.Defaults()
	cfg.DashboardTLSMode = config.TLSModeSelfSigned
	cfg.DashboardPort = srv.Listener.Addr().(*net.TCPAddr).Port

	if notice := trayRunningNotice(cfg); notice == "" {
		t.Fatal("trayRunningNotice = empty; the TLS dashboard was not detected")
	}
	if notice := trayRunningNotice(cfg); !strings.Contains(notice, "https://") {
		t.Errorf("notice = %q, want it to advertise https", notice)
	}
}

// Proves the success above came from real verification rather than from
// verification being switched off: a DIFFERENT self-signed certificate,
// which gpsync did not write into the state dir, must be rejected.
func TestTrayRunningNotice_ForeignCertificate_IsRejected(t *testing.T) {
	stateDir := t.TempDir()
	t.Setenv("GPSYNC_STATE_DIR", stateDir)
	origStateDir := statedb.StateDir
	statedb.StateDir = stateDir
	t.Cleanup(func() { statedb.StateDir = origStateDir })

	// The pair the client will trust.
	if _, err := webdashboard.EnsureTLSFiles(stateDir, "", "", webdashboard.DefaultCertHosts(), time.Now()); err != nil {
		t.Fatalf("EnsureTLSFiles (trusted): %v", err)
	}
	// A completely unrelated pair, standing in for an interceptor.
	foreign, err := webdashboard.EnsureTLSFiles(t.TempDir(), "", "", webdashboard.DefaultCertHosts(), time.Now())
	if err != nil {
		t.Fatalf("EnsureTLSFiles (foreign): %v", err)
	}
	cert, err := tls.LoadX509KeyPair(foreign.CertPath, foreign.KeyPath)
	if err != nil {
		t.Fatalf("LoadX509KeyPair: %v", err)
	}

	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{cert}}
	srv.StartTLS()
	defer srv.Close()

	cfg := config.Defaults()
	cfg.DashboardTLSMode = config.TLSModeSelfSigned
	cfg.DashboardPort = srv.Listener.Addr().(*net.TCPAddr).Port

	if notice := trayRunningNotice(cfg); notice != "" {
		t.Error("a certificate gpsync never issued was accepted; the client is not verifying")
	}
}
