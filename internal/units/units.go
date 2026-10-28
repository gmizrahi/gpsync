// Package units formats byte counts and times for people, once, for both the
// CLI and the tray dashboard.
package units

import (
	"fmt"
	"time"
)

// Bytes renders n with 1024-based magnitudes and KB / MB / GB / TB labels,
// e.g. "3.0KB", "286.2GB".
//
// The labels were KiB / MiB / GiB, which read as jargon outside the
// projects that use them. The MAGNITUDES
// deliberately stay 1024-based: that is the convention Windows Explorer uses
// for the very same files, so a folder reads the same size in gpsync as it
// does in Explorer, and no figure anywhere shifts -- only the label changes.
// The dashboard's own JavaScript formatter already worked this way.
//
// This used to be two identical copies, one in cmd/gpsync and one in
// internal/dashboard, justified by a comment claiming both were package
// main. Only cmd/gpsync is; one shared function keeps the CLI and the
// tray from disagreeing about units.
func Bytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%dB", n)
	}
	div, exp := int64(unit), 0
	for n2 := n / unit; n2 >= unit; n2 /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f%cB", float64(n)/float64(div), "KMGTPE"[exp])
}

// Relative renders t against now: "just now", "5 minutes ago", "2 hours
// ago", "3 days ago", or the same with "in" for a time ahead. Hours run to
// 48 so "yesterday evening" doesn't read as "1 day ago".
func Relative(t, now time.Time) string {
	d := now.Sub(t)
	future := d < 0
	if future {
		d = -d
	}
	var n int
	var unit string
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		n, unit = int(d/time.Minute), "minute"
	case d < 48*time.Hour:
		n, unit = int(d/time.Hour), "hour"
	default:
		n, unit = int(d/(24*time.Hour)), "day"
	}
	if n != 1 {
		unit += "s"
	}
	if future {
		return fmt.Sprintf("in %d %s", n, unit)
	}
	return fmt.Sprintf("%d %s ago", n, unit)
}
