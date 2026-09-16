//go:build windows

package main

import (
	"os"

	"golang.org/x/sys/windows"
)

// attachParentProcess is ATTACH_PARENT_PROCESS from wincon.h -- (DWORD)-1.
const attachParentProcess = ^uint32(0)

var (
	kernel32                  = windows.NewLazySystemDLL("Kernel32.dll")
	procAttachConsole         = kernel32.NewProc("AttachConsole")
	procSetConsoleCtrlHandler = kernel32.NewProc("SetConsoleCtrlHandler")
)

// attachToParentConsole lets a -H=windowsgui binary's stdout/stderr reach
// an interactive console it was launched from. Without it,
// `.\gpsync-tray.exe --help` printed nothing at all: the GUI subsystem
// never gets a console attached automatically, launched from an existing
// terminal or not, so --help/--version output (and flag.Parse's own
// automatic -h handling) would otherwise go to a Stdout/Stderr with
// nowhere to be seen. golang.org/x/sys/windows doesn't wrap AttachConsole,
// so this calls Kernel32.dll directly -- the same "no cgo, raw Win32
// syscall via NewLazySystemDLL" pattern internal/systray already uses
// throughout. Failing (double-clicked from Explorer, no parent console at
// all) is the common, expected case, not an error -- systray.Run's normal
// tray startup is completely unaffected either way.
func attachToParentConsole() {
	r, _, _ := procAttachConsole.Call(uintptr(attachParentProcess))
	if r == 0 {
		return
	}
	ignoreConsoleCtrlC()
	if f, err := os.OpenFile("CONOUT$", os.O_RDWR, 0); err == nil {
		os.Stdout = f
		os.Stderr = f
	}
}

// ignoreConsoleCtrlC makes the tray immune to Ctrl+C in the console it was
// launched from. Interrupting an unrelated CLI command in that console
// used to kill the tray too.
//
// The cause is self-inflicted: attaching to the parent console above (for
// --help/--version output) makes this a console process, and Windows
// delivers CTRL_C_EVENT to EVERY process attached to that console, not
// just the foreground one. So starting the tray with `.\gpsync-tray.exe`
// from PowerShell and later interrupting an unrelated `gpsync upload` in
// the same window killed the background sync too -- silently, and the
// tray's own crash log records nothing, because being signalled is not a
// crash.
//
// SetConsoleCtrlHandler(NULL, TRUE) is the documented way to ignore it:
// a NULL handler with Add=TRUE tells Windows this process ignores
// CTRL_C_EVENT outright. Deliberately narrow -- it does NOT cover
// CTRL_BREAK_EVENT or CTRL_CLOSE_EVENT, so closing the console window
// still takes the tray with it, which is the behaviour someone closing
// that window would expect. Quit is via the tray menu, as it always was.
//
// Called only on the attach path: with no parent console there is no
// console signal to ignore, and nothing to do.
func ignoreConsoleCtrlC() {
	// SetConsoleCtrlHandler(HandlerRoutine=NULL, Add=TRUE)
	procSetConsoleCtrlHandler.Call(0, 1)
}
