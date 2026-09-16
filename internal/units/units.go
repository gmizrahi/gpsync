// Package units formats byte counts for people, once, for both the CLI and
// the tray dashboard.
package units

import "fmt"

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
