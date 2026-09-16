# Dashboard

The tray app (`gpsync-tray`, Windows) and the `gpsync dashboard` command both
serve the same web UI. It is read-mostly: everything it shows comes from the
ledger, and the few actions it offers are the ones that are safe from a
browser.

Open it at `http://127.0.0.1:<port>` — the tray's menu has a direct link, and
double-clicking the tray icon opens it too.

## Pages

**Status** — live activity: which folder is being processed, how many files
are uploading, uploaded and total, transfer rate and ETA (measured over time
actually spent transferring, so throttle pauses do not distort it), the
current backoff countdown with a Retry Now button, recent per-file results,
library totals and today's API usage.

**Statistics** — totals by media type, sync progress, uploads over time, and
capture-year distribution, with a throttle history table.

**Browse** — every file in the ledger, filtered by status (pending, uploaded,
failed, needs review, ignored), searchable by path or hash, sortable, paged.

**Duplicates** — groups of files with identical content, largest reclaimable
space first, with a resolver that moves the copies you do not keep to the
trash folder.

**Originals** — files inside "originals" folders that a photo editor left
behind, so you can queue or ignore them individually.

**Backup** — snapshot gpsync's own data (ledger, settings, credentials) and
restore it. This backs up gpsync's state, not your photos.

**Settings** — everything in `config.toml`, including source folders, upload
quality, concurrency, the sync strategy, theme, and the dashboard's own
address, port and login.

## Access and safety

By default the dashboard listens on loopback only. To reach it from another
device, enable login in Settings first: gpsync refuses to bind a non-loopback
address without it.

When login is enabled, sessions are cookie-based, passwords are bcrypt-hashed,
repeated failures are rate-limited, and every mutating request is CSRF-checked.
There is no TLS yet — use a reverse proxy if you need HTTPS.

## Phone layout

The UI adapts to phone widths: navigation becomes a swipeable row, cards stack,
the file tables become cards, and charts drop to tap-for-values. Nothing is
hidden that the desktop layout shows.
