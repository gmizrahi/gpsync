//go:build windows

package main

import (
	"fmt"
	"log"
	"os"
	"runtime/debug"

	"github.com/gmizrahi/gpsync/internal/statedb"
)

// recoverAndLog is deferred at the top of onReady and every long-lived
// goroutine main.go spawns directly (the tray icon ticker, the menu-click
// loop) -- silent-death visibility, a real gap: two bugs this session
// already killed this -H=windowsgui process with NOTHING reaching
// tray.log at all (a hand-edited config.toml's watch_heartbeat_minutes=0
// panicking time.NewTicker, and a WaitGroup misuse panic from a
// Start/Stop race -- both since fixed at the root cause, so this is
// defense in depth for the NEXT unknown one, not a fix for either).
//
// Deliberately does NOT let the process keep running afterward: a panic
// means something is in an unknown state, and limping on risks a subtler,
// harder-to-diagnose failure than a clean, LOGGED exit. db may be nil (a
// panic before the ledger is even open yet) -- the crash is still logged
// to tray.log either way, just not persisted for the next launch's Status
// page to show.
func recoverAndLog(db *statedb.DB, context string) {
	r := recover()
	if r == nil {
		return
	}
	log.Printf("panic in %s: %v\n%s", context, r, debug.Stack())
	if db != nil {
		if err := db.RecordCrash(context, fmt.Sprintf("%v", r)); err != nil {
			log.Printf("recording crash: %v", err)
		}
	}
	os.Exit(1)
}

// safeGo launches fn in a new goroutine with recoverAndLog deferred --
// used for every top-level, long-lived goroutine in this package instead
// of a bare `go func() { ... }()`, so a panic in one gets logged and
// recorded instead of silently killing the whole process. NOT used for
// watchController's own internal per-cycle goroutines (Start/run, the
// heartbeat fan-in) -- those are already carefully reasoned about
// (Start/Stop's channel lifecycle was itself the subject of a real,
// fixed race) and adding recovery there risks papering over a real
// upload-pipeline bug rather than surfacing it.
func safeGo(db *statedb.DB, name string, fn func()) {
	go func() {
		defer recoverAndLog(db, name)
		fn()
	}()
}
