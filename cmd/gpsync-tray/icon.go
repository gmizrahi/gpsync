//go:build windows

package main

import _ "embed"

// The 4 tray icon states (see trayState in main.go) -- one shared glyph (a
// white up-arrow "sync/upload" badge), 4 fill colors, generated once via a
// throwaway local script (not part of this build) and committed as static
// assets, same pattern as the original single placeholder icon.ico this
// replaces. No runtime image generation, no new dependency.
var (
	//go:embed icon_idle.ico
	iconIdleICO []byte

	//go:embed icon_syncing.ico
	iconSyncingICO []byte

	//go:embed icon_throttled.ico
	iconThrottledICO []byte

	//go:embed icon_paused.ico
	iconPausedICO []byte
)
