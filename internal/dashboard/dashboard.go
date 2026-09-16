// Package dashboard serves gpsync's web UI: the live Status page, plus
// Statistics, Browse, Settings, Backup, Duplicates and Originals.
//
// It is deliberately free of build tags and of any Windows-specific code, so
// it can be exercised by tests on any platform. Everything platform-bound
// reaches it through two seams: Controller, the live sync state it renders
// (implemented by the tray's watch controller, and by a stub in
// cmd/dashboard-preview), and Options, which carries the app name, favicon,
// autostart toggle and shutdown hook.
//
// One file per page, each holding that page's data types, template, render
// function and handlers; handler.go wires the routes, shell.go renders the
// page chrome, session.go handles login, and format.go holds the shared
// display helpers. The stylesheet and the Status page's markup and script
// are embedded from assets/.
package dashboard
