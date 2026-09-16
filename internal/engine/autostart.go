package engine

import "fmt"

// AutostartValueName is the name gpsync-tray registers itself under in
// Windows' per-user Run key (HKCU\Software\Microsoft\Windows\CurrentVersion\Run)
// -- shared between the windows-only install/uninstall/check code and
// anything (a test, a future doctor-style command) that needs to name the
// same value without duplicating the literal string.
const AutostartValueName = "GPSync"

// AutostartCommand returns the exact string that should be written as the
// Run-key value's data for exePath -- always double-quoted, so a path
// containing spaces (a real case: anything under "C:\Program Files\...")
// is treated by Windows as one argument, not split at the first space.
func AutostartCommand(exePath string) string {
	return fmt.Sprintf(`"%s"`, exePath)
}
