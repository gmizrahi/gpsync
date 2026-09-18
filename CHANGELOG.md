# Changelog

All notable changes to gpsync are documented here.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

## [0.3.2]

A bug fix: backups to a cloud-sync drive failed outright.

### Fixed

- **Backups to a cloud-sync drive work again.** Backing up to `G:\My Drive`
  failed outright with "The parameter is incorrect" and wrote nothing: Google
  Drive's virtual drive has no ACLs, so the call that locks the archive to
  your account returns `ERROR_INVALID_PARAMETER` there, and that was treated
  as fatal. A destination that has no per-user permissions at all -- a
  cloud-sync folder, a FAT stick -- now gets the backup anyway, and both the
  CLI and the dashboard say that this archive could not be locked down, since
  it contains your OAuth credentials. A filesystem that *has* permissions and
  refuses is still a hard failure. (#24)

## [0.3.1]

A bug fix: a photo edited in place could be uploaded twice, with the ledger
then claiming the original had been uploaded when it never was.

### Fixed

- **An edited file no longer leaves a ledger row that uploads the wrong
  bytes.** Editing a photo in place -- a crop, a rotate, an exposure fix --
  changes its hash but keeps its path, which left the old hash's row still
  naming that path. The uploader only checks that the path exists, so that row
  would send the *edited* content under the *original* hash: the photo uploaded
  twice, and the record of the original marked uploaded when it never was.
  Every "is it still there" check was path-based, and the path was still there.
  (#22)
  - **Re-check now compares content, not existence.** A row whose path holds
    different content is reported as replaced and flagged missing, instead of
    "back on disk".
  - **`gpsync doctor` gained an invariant for it.** Queued rows pointing at a
    path that now holds different content are reported and, under `--fix`,
    forgotten -- which is what stops the wrong-bytes upload. Already-uploaded
    rows are left alone: that content really was sent, and the row is history
    worth keeping.

## [0.3.0]

Everything that needed a terminal can now be done from the dashboard, and
signing in no longer needs one at all.

### Added

- **Sign in from the dashboard.** A new page imports an existing rclone Google
  Photos remote, accepts a `client_secret.json` upload, or runs Google's
  consent flow — so installing the tray and never opening a shell is now a
  complete path. Previously the Account card could only report that you were
  not signed in. (#15, #16, closes #14)
- **Sync one folder on demand**, from Browse. Queued behind whatever the watch
  engine is already doing, and the confirmation says so. (#19)
- **Re-check and Forget, per file**, on every Browse row. Re-check reports
  whether a file is still on disk; Forget drops the ledger entry and leaves the
  file alone. (#19)
- **Fix drifted capture dates** from Statistics, where the wrong years are
  visible. The button reports how many it would correct before you press it.
  (#19)
- **Move queued originals-folder files into review** from the Originals page,
  instead of `gpsync clean-originals`. (#19)
- **Mark a folder as already synced** from the dashboard, behind a typed
  confirmation — it tells gpsync never to upload those files, so it is gated
  like a restore. (#19)
- **`gpsync stats`** — the Statistics view as a CLI command, for Linux and
  macOS where there is no tray. (#19, closes #18)

### Fixed

- **Releases no longer publish a partial asset set.** Cutting v0.2.0 failed
  twice, each time leaving a different subset of files attached while reporting
  every upload as successful. Assets are now published with `gh` and the run
  fails unless every one is present, non-empty and uploaded. (#17, closes #13)
- Published `checksums.txt` listed a file that was never a release asset: the
  checksum step globbed `gpsync-*` inside the build directory, which also holds
  the raw tray binary the Windows zip is built from. (#17)

### Security

- Folder and redirect inputs on the new dashboard actions are no longer
  accepted as free-form values. Redirect targets are built server-side from a
  fixed list, and a submitted folder selects one of the configured source
  folders rather than supplying a path — so nothing from a request reaches the
  scanner, the watch loop, or a `Location` header. Found by CodeQL. (#19)
- Google's consent flow can only be started from the machine running gpsync.
  Its callback redirects to `127.0.0.1` there, so a request from another device
  is refused with that explanation rather than started and left to fail
  silently. (#16)
- An uploaded `client_secret.json` is size-capped, stored owner-only, and a Web
  OAuth client is refused by name — it cannot use the loopback redirect. The
  refusal never echoes the secret the file contained. (#16)

## [0.2.0]

### Added

- **HTTPS for the dashboard**, off by default. `dashboard_tls_mode` is `off`,
  `self-signed` or `files`; `dashboard_https_port` is the TLS listener's port.
  HTTPS runs *alongside* HTTP rather than replacing it, so the tray keeps
  opening the dashboard locally over loopback with no certificate to trust.
- `self-signed` generates a certificate once into `~/.gpsync`, reuses it on
  every later start, and logs its SHA-256 fingerprint so the one-time browser
  warning can be checked before it is trusted. `files` serves a certificate you
  supply and never writes to it.

### Fixed

- **The watcher debounced the wrong folder on Windows.** A directory event was
  only filtered on create, so Windows' extra write on a parent directory fell
  through and debounced that directory's *parent* — for a top-level folder,
  the entire source root. Syncing could stall well beyond the debounce window.
- **Capture dates were lost when "space saver" was on.** Downscaling re-encoded
  the image and dropped its EXIF, so those uploads showed the upload date in
  Google Photos instead of the date taken. The original EXIF is now carried
  across, with orientation normalised so rotation is not applied twice.
- **TIFF files are always uploaded as the original** rather than downscaled,
  which also avoids a decoder panic in the image library.
- The Settings page now exposes the TLS fields it had already been parsing.
  Without inputs for them, an absent field read as empty, so saving Settings
  for any unrelated reason would have switched HTTPS back off.

### Security

- `client_secret.json`, `token.json` and backup archives are now readable only
  by the owner on Windows, set through an explicit ACL. Windows has no POSIX
  mode bits, so the 0600 these files already used on Linux and macOS had no
  effect there.

## [0.1.0] — first public release

gpsync syncs local photo and video folders to Google Photos and keeps a
durable ledger of what has been sent, so an interrupted run resumes instead
of starting over and a file is never uploaded twice.

### Added

**Syncing**

- Content-hash (SHA-256) deduplication: a file is uploaded once however often
  it is renamed, moved or copied, because identity is the content and not the
  path.
- `gpsync sync`, `gpsync scan` and `gpsync upload` for one-shot work, folder by
  folder, with `--media-type photos|videos|all` and glob arguments.
- `gpsync watch` for continuous syncing, reacting to filesystem events with a
  per-folder debounce and a periodic heartbeat that re-checks the backlog.
- Scanning and uploading run concurrently, so pending files start uploading
  without waiting for a full library walk to finish.
- `--smallest-first` and `--smallest-first-global` upload ordering.
- Optional local "space saver" downscaling before upload.

**Staying inside Google's limits**

- A run-wide circuit breaker that pauses the whole pipeline when Google
  throttles, on an escalating schedule up to one hour, and resumes on its own.
- Adaptive concurrency that cold-starts at one and backs off on real 429s
  rather than assuming a safe rate.
- Daily quota tracking against Google's midnight-Pacific reset.
- `gpsync throttle-log`, `--analyze` and `--csv` to see what throttling
  actually cost and how long recovery really took.

**Knowing what happened**

- A local SQLite ledger recording every file's status, failure reason,
  capture date, recorded remote quality and verification result.
- `gpsync info`, `gpsync pending`, `gpsync log`, `gpsync extensions` and
  `gpsync whatis` for reading it.
- `gpsync doctor` checks every ledger invariant this project has had a real
  bug against, with `--fix` for the unambiguous subset.
- Deleted and moved files are tracked: a moved file is repointed, a deleted one
  is confirmed missing only after a full pass proves it is gone.
- `gpsync recheck` reports missing files and permanent failures, with
  `--revalidate`, `--missing`, `--unsupported`, `--retry` and `--all` to act.

**Reviewing and correcting**

- `gpsync duplicates resolve` moves redundant copies to a trash folder, never
  deletes them.
- `gpsync precheck` sets aside files already synced before importing a folder.
- An "originals" folder review workflow, so pre-edit backup copies are never
  auto-queued.
- `gpsync fix-dates` backfills capture dates from path when EXIF is absent and
  the filesystem timestamp has drifted.
- `gpsync mark-synced` records files uploaded by other tools, with `--quality`
  and `--unmark`.
- `gpsync reupload` re-sends files recorded as storage-saver quality so Google
  merges the original in place.

**Interfaces**

- A Windows tray application with a web dashboard: live progress, statistics,
  file browsing, duplicate and originals review, backup and settings.
- `gpsync dashboard` serves the same web dashboard from the CLI on any platform.
- The dashboard is laid out for phones as well as desktop.

**Its own data**

- `gpsync backup` and `gpsync restore` snapshot and restore the ledger, settings
  and credentials.

### Security

- The dashboard binds to loopback unless authentication is enabled; a public
  bind without authentication is refused and pulled back to loopback.
- Session-cookie login with bcrypt password hashing, rate limiting and lockout
  on repeated failures.
- CSRF origin checking on every mutating endpoint.
- OAuth credentials are stored only in `~/.gpsync` at mode 0600, never in
  `config.toml`, and are never logged or printed.
- Archive extraction on restore is guarded against path traversal and
  decompression bombs.
- Continuous integration runs `go vet`, `golangci-lint`, `govulncheck` and
  CodeQL, with Dependabot enabled.

### Known limitations

These come from the Google Photos API itself, not from gpsync. See
[docs/limitations.md](docs/limitations.md).

- gpsync cannot delete anything from your Google Photos library; the API is
  append-only for third-party tools.
- gpsync cannot read back photos it did not upload.
- Album support was removed: the API rejected the very media item IDs it had
  just issued.
- Each user must create their own Google API credentials. See
  [docs/setup.md](docs/setup.md).

[Unreleased]: https://github.com/gmizrahi/gpsync/compare/v0.3.2...HEAD
[0.3.2]: https://github.com/gmizrahi/gpsync/compare/v0.3.1...v0.3.2
[0.3.1]: https://github.com/gmizrahi/gpsync/compare/v0.3.0...v0.3.1
[0.3.0]: https://github.com/gmizrahi/gpsync/compare/v0.2.0...v0.3.0
[0.2.0]: https://github.com/gmizrahi/gpsync/compare/v0.1.0...v0.2.0
[0.1.0]: https://github.com/gmizrahi/gpsync/releases/tag/v0.1.0
