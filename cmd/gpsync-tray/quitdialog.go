//go:build windows

package main

import (
	"log"

	"golang.org/x/sys/windows"
)

// Win32 MessageBox constants (winuser.h) -- golang.org/x/sys/windows wraps
// MessageBoxW itself but doesn't expose named constants for its flags or
// return values, so these are hardcoded here. They're long-stable ABI
// values (unchanged since Windows 3.1), not something that could shift
// under us.
const (
	mbYesNoCancel  = 0x00000003
	mbIconQuestion = 0x00000020
	mbDefButton2   = 0x00000100 // focus "No" (the graceful option) by default

	idYes    = 6
	idNo     = 7
	idCancel = 2
)

// quitChoice is the user's answer to confirmQuit's dialog.
type quitChoice int

const (
	quitCancelled quitChoice = iota
	quitNow
	quitGracefully
)

// confirmQuit shows a native, ownerless MessageBox asking whether to quit
// immediately or let in-flight uploads finish first: Quit used to stop
// everything outright, with no way to drain the queue.
// golang.org/x/sys/windows.MessageBox
// wraps MessageBoxW directly -- pure Go syscall, no cgo, the same story as
// every other Windows-specific piece of this project. It blocks the
// calling goroutine until dismissed, which is correct here: it's a modal
// confirmation, not a progress display.
func confirmQuit() quitChoice {
	text, err := windows.UTF16PtrFromString(
		"Quit now, or finish in-progress uploads first?\n\n" +
			"Yes = Quit now (any upload in progress is cut off)\n" +
			"No = Finish in-progress uploads, then quit (no new uploads start)\n" +
			"Cancel = Don't quit")
	if err != nil {
		log.Printf("quit confirmation dialog: building message text: %v", err)
		return quitCancelled
	}
	caption, err := windows.UTF16PtrFromString(appName)
	if err != nil {
		log.Printf("quit confirmation dialog: building caption: %v", err)
		return quitCancelled
	}
	ret, err := windows.MessageBox(0, text, caption, mbYesNoCancel|mbIconQuestion|mbDefButton2)
	if err != nil {
		log.Printf("quit confirmation dialog: %v", err)
		return quitCancelled
	}
	switch ret {
	case idYes:
		return quitNow
	case idNo:
		return quitGracefully
	default:
		return quitCancelled
	}
}
