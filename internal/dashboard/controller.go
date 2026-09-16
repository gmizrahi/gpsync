package dashboard

import (
	"time"

	"github.com/gmizrahi/gpsync/internal/config"
	"github.com/gmizrahi/gpsync/internal/uploader"
)

// Controller is the seam between this package and gpsync-tray's concrete
// *watchController -- exactly the method set dashboard handlers actually
// call, confirmed by direct inventory of every wc.* call site in this
// package before the extraction. *watchController already implements this
// interface verbatim (it has zero Windows API calls of its own; only its
// construction site and the tray menu wiring are Windows-tied), so nothing
// about it needed to change except InFlightFiles' return type -- see
// InFlightFile below.
type Controller interface {
	Config() config.Config
	SetConfig(cfg config.Config)
	IsRunning() bool
	Start()
	Stop()
	Scanning() bool
	// FileProgress reports the current cycle's files: uploaded (confirmed,
	// never decreases within a cycle), uploading (being sent right now;
	// drops when a throttle fails them), and the cycle's total.
	FileProgress() (uploaded, uploading, total int)
	FolderProgress() (index, total int)
	CurrentFolder() string
	RunProgress() (bytesDone, bytesTotal int64, start time.Time)
	// RunPausedFor is how much of the current cycle was spent in throttle
	// backoff, including a pause still in progress. Speed and ETA exclude it.
	RunPausedFor() time.Duration
	InFlightFiles() map[string]uploader.InFlightFile
	RecentEvents() []uploader.ProgressEvent
	CurrentBackoff() *uploader.BackoffStatus
	RequestRetryNow()
	CancelFile(path string) bool
}

// AutostartController is the seam for the Windows registry Run-key toggle
// (see cmd/gpsync-tray/autostart.go) -- nil-able. When Options.Autostart is
// nil, /api/autostart-toggle answers 501 and the Status page's
// AutostartInstalled field just stays false, rather than this package ever
// touching the Windows registry (or anything platform-specific) directly.
type AutostartController interface {
	Installed() (bool, error)
	Install(exePath string) error
	Uninstall() error
}

// Options carries the handful of things that would otherwise pin this
// package to Windows -- app branding, the tray's embedded favicon, the
// registry-backed autostart toggle, and how to cleanly end the process
// after a destructive backup restore (systray.RemoveIcon()+os.Exit(0) on
// the real tray; a nil Shutdown just falls back to plain os.Exit(0), so a
// real restore is never silently a no-op).
type Options struct {
	AppName   string
	Favicon   []byte
	Autostart AutostartController
	Shutdown  func()
	// QuitGracefully ends the process the way the tray's Quit dialog does
	// when someone picks "No": stop starting new uploads, let whatever is
	// already transferring finish, then exit. Distinct from Shutdown,
	// which is the immediate-exit path a backup restore needs.
	//
	// It is called from its own goroutine after the HTTP response has
	// been written -- it takes real seconds (an in-flight file finishing)
	// and it shuts down the very server handling the request, so running
	// it inline would deadlock against that server's own graceful stop.
	//
	// Nil where the host has no such concept (any non-tray embedding),
	// in which case /api/quit answers 501 rather than pretending.
	QuitGracefully func()
}

// Package-level state set once by Handler() from the Options it's given --
// the pragmatic choice here (matching this package's own existing
// package-var-not-const style, e.g. sharedCSS/the *template.Template vars)
// since in practice exactly one dashboard server exists per process; a
// test calling Handler() sets these the same way a real caller does.
var (
	appName        = "gpsync"
	favicon        []byte
	autostartCtrl  AutostartController
	shutdown       func()
	quitGracefully func()
)
