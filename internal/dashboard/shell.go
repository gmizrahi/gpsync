package dashboard

import (
	_ "embed"
	"fmt"

	"github.com/gmizrahi/gpsync/internal/config"
	"github.com/gmizrahi/gpsync/internal/statedb"
)

// sharedCSS is used by every page this server renders -- card-based
// sections, color-coded status badges, light and dark themes both fully
// defined as one token set each. Plain CSS custom properties, no
// framework, no build step -- consistent with every other dependency
// choice in this project.
//
// Dark is the base :root palette (this app shipped dark-only first, and
// it's the one already confirmed working on real Windows) -- light is an
// override, applied either by @media (prefers-color-scheme: light) when
// Theme is "system" (config.ThemeSystem, the default: no data-theme
// attribute at all, see pageShell) or unconditionally by
// :root[data-theme="light"] when the user picks Light explicitly in
// Settings. :root[data-theme="dark"] is listed too even though it matches
// the base already, so an explicit Dark choice is never silently dependent
// on nothing else having overridden it.
//
//go:embed assets/dashboard.css
var sharedCSS string

// pageShell wraps body in the chrome every page shares: a title tag, top
// bar with the app name and a three-item nav, shared CSS. active picks
// which nav link gets the "current page" style. theme is a config.Theme*
// value -- config.ThemeSystem (or anything unrecognized) stamps no
// data-theme attribute at all, letting sharedCSS's prefers-color-scheme
// media query decide; Light/Dark stamp the attribute explicitly, which
// always wins over the media query (see sharedCSS's own doc comment).
//
// db is used ONLY to decide whether the Duplicates/Originals nav links
// show at all: a tab that leads to an empty page is noise, so each is
// hidden until there is something to resolve. Both checks are
// deliberately cheap, since this runs on EVERY page load, not just the
// pages themselves: db.DuplicateGroups() (not the disk-rechecked
// dupGroupsFiltered, which is reserved for the duplicates page itself --
// an edge case where every group turns out disk-stale would just show the
// tab leading to a "no duplicates found" page, an acceptable tradeoff for
// not doing a filesystem stat sweep on every single page load) and
// db.CountsByStatus()["needs_review"] (already the cheapest possible
// count query).
func pageShell(db *statedb.DB, pageTitle, active, body, theme string, authEnabled bool) string {
	navLink := func(href, label, id string) string {
		cls := ""
		if id == active {
			cls = ` class="active"`
		}
		return fmt.Sprintf(`<a href="%s"%s>%s</a>`, href, cls, label)
	}
	hasDuplicates := false
	duplicatesLabel := "Duplicates"
	if groups, err := db.DuplicateGroups(); err == nil {
		hasDuplicates = len(groups) > 0
		if hasDuplicates {
			var reclaimable int64
			for _, g := range groups {
				reclaimable += g.Size * int64(len(g.Paths)-1)
			}
			duplicatesLabel = fmt.Sprintf("Duplicates (%d, %s)", len(groups), humanBytes(reclaimable))
		}
	}
	hasOriginalsReview := false
	if counts, err := db.CountsByStatus(); err == nil {
		hasOriginalsReview = counts["needs_review"] > 0
	}
	// Statistics sits right after Status: it is the page people reach for
	// once they have seen what is happening right now.
	nav := navLink("/", "Status", "status") + " " +
		navLink("/statistics", "Statistics", "statistics") + " " +
		navLink("/browse?type=pending", "Browse", "browse") + " " +
		navLink("/backup", "Backup", "backup") + " "
	if hasDuplicates {
		nav += navLink("/duplicates", duplicatesLabel, "duplicates") + " "
	}
	if hasOriginalsReview {
		nav += navLink("/originals", "Originals", "originals") + " "
	}
	nav += navLink("/settings", "Settings", "settings")
	// Logout only makes sense once there's a session to end, so it is
	// hidden entirely when auth is off.
	if authEnabled {
		nav += ` <a href="/logout">Logout</a>`
	}
	themeAttr := ""
	if theme == config.ThemeLight || theme == config.ThemeDark {
		themeAttr = fmt.Sprintf(` data-theme="%s"`, theme)
	}
	// Browse's table wants real width for six columns of file data.
	// Status now has TWO side-by-side two-column grids (Library/Total
	// Size, Uploading/Recent activity) that read cramped -- filenames
	// wrapping mid-word -- at the old single-column card width, a real
	// report: "make it wider, you have more room... now you are cutting
	// the names of the files". Settings stays a narrower single-column
	// form, which reads better unstretched. See main.wide/main.status-wide
	// in sharedCSS.
	mainClass := ""
	switch active {
	case "browse", "duplicates", "originals":
		mainClass = ` class="wide"`
	case "status", "statistics":
		mainClass = ` class="status-wide"`
	}
	// bodyClass: only the Status page gets body.status-page's flex-fill
	// layout (see sharedCSS) -- every other page (Browse, Statistics,
	// Settings, ...) keeps its normal, unbounded document scroll, which a
	// long table or a tall form still needs.
	bodyClass := ""
	if active == "status" {
		bodyClass = ` class="status-page"`
	}
	return fmt.Sprintf(`<!DOCTYPE html>
<html%s>
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>%s — %s</title>
<link rel="icon" href="/favicon.ico">
<style>%s</style>
</head>
<body%s>
<div class="topbar">
  <h1>%s</h1>
  <div class="nav">%s</div>
</div>
<main%s>%s</main>
<script>(function(){if(!window.matchMedia||!matchMedia('(max-width: 600px)').matches)return;var a=document.querySelector('.nav a.active');if(a&&a.scrollIntoView)a.scrollIntoView({block:'nearest',inline:'center'});})();</script>
</body>
</html>`, themeAttr, pageTitle, appName, sharedCSS, bodyClass, appName, nav, mainClass, body)
}

// statusPageBody is deliberately still JS-driven (unlike the settings page
// below): it polls /api/status and re-renders live, which a server-
// rendered page can't do without a full reload.
//
//go:embed assets/status.html
var statusPageBody string
