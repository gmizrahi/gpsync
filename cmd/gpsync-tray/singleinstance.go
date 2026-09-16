//go:build windows

package main

import (
	"log"

	"golang.org/x/sys/windows"
)

// Win32 MessageBox constants (winuser.h) not already defined in
// quitdialog.go -- same reasoning as there: long-stable ABI values, safe
// to hardcode.
const (
	mbOK       = 0x00000000
	mbIconInfo = 0x00000040
)

// singleInstanceMutexName is deliberately specific to this app, not a
// generic name another program could collide with -- Local\ scopes it to
// this login session, which is exactly what "don't run two copies for the
// same ~/.gpsync ledger" needs (a different Windows user's own gpsync-tray
// is a completely separate, unrelated instance).
const singleInstanceMutexName = `Local\GPSyncTraySingleInstance`

// acquireSingleInstanceLock reports whether this is the only running copy
// of gpsync-tray, which matters once autostart exists: autostart launching
// a copy at sign-in AND someone separately double-clicking the exe (easy
// to do without noticing the tray icon is already there) would
// otherwise mean two independent CircuitBreakerStates sharing one ledger --
// the exact "each folder got its own breaker and never climbed" bug
// already fixed once at folder scope (see uploader.CircuitBreakerState's
// own doc comment), just now at process scope. SQLite's own busy_timeout
// doesn't help here: the problem isn't lock contention on a query, it's
// two uncoordinated throttle/backoff decisions racing each other against
// the same "concurrent write request" quota.
//
// The mutex handle is deliberately never closed -- Windows releases it
// automatically the moment this process exits for ANY reason (clean quit,
// crash, killed from Task Manager), which is exactly the "is the OTHER
// copy actually still running" signal a manually-closed handle can't give
// for free.
func acquireSingleInstanceLock() bool {
	name, err := windows.UTF16PtrFromString(singleInstanceMutexName)
	if err != nil {
		log.Printf("single-instance check: building mutex name: %v", err)
		return true // fail open -- don't block a real launch over this
	}
	_, err = windows.CreateMutex(nil, false, name)
	if err == windows.ERROR_ALREADY_EXISTS {
		return false
	}
	if err != nil {
		log.Printf("single-instance check: CreateMutex: %v", err)
		return true // fail open -- same reasoning as above
	}
	return true
}

// notifyAlreadyRunning shows a plain informational MessageBox -- the
// common case this fires for is a human double-clicking the exe while it's
// already sitting in the tray, so a visible explanation matters here,
// unlike the rarer silent autostart-collision case this same check also
// covers.
func notifyAlreadyRunning() {
	text, err := windows.UTF16PtrFromString(appName + " is already running -- check your system tray icons.")
	if err != nil {
		return
	}
	caption, err := windows.UTF16PtrFromString(appName)
	if err != nil {
		return
	}
	windows.MessageBox(0, text, caption, mbOK|mbIconInfo)
}
