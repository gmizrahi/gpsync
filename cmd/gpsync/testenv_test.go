package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gmizrahi/gpsync/internal/config"
	"github.com/gmizrahi/gpsync/internal/statedb"
)

// statedb.StateDir, statedb.StateDBPath and config.ConfigPath are computed
// once, when the packages load, from GPSYNC_STATE_DIR or the home folder.
// Setting the environment variable in a test changes none of them. Tests
// that did only that, or redirected the ledger but not the config, read and
// wrote the developer's own ~/.gpsync.

// TestMain points the whole package at a temporary state folder before any
// test runs, so a test that forgets isolateState still can't reach the real
// one.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "gpsync-cmd-test-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	setStateDir(dir)
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// isolateState gives one test its own empty state folder, for the ledger and
// the config alike, and returns it.
func isolateState(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("GPSYNC_STATE_DIR", dir)
	prevState, prevDB, prevConfig := statedb.StateDir, statedb.StateDBPath, config.ConfigPath
	setStateDir(dir)
	t.Cleanup(func() { statedb.StateDir, statedb.StateDBPath, config.ConfigPath = prevState, prevDB, prevConfig })
	return dir
}

func setStateDir(dir string) {
	statedb.StateDir = dir
	statedb.StateDBPath = filepath.Join(dir, "state.sqlite")
	config.ConfigPath = filepath.Join(dir, "config.toml")
}

// The safety net itself: the package's paths are not the developer's.
func TestStateIsIsolated(t *testing.T) {
	home, _ := os.UserHomeDir()
	real := filepath.Join(home, ".gpsync")
	for name, p := range map[string]string{"StateDir": statedb.StateDir, "StateDBPath": statedb.StateDBPath, "ConfigPath": config.ConfigPath} {
		if p == real || strings.HasPrefix(p, real+string(filepath.Separator)) {
			t.Errorf("%s = %s, inside the real %s", name, p, real)
		}
	}
}
