# Configuration

gpsync reads `~/.gpsync/config.toml` (`C:\Users\<you>\.gpsync\config.toml`
on Windows). The file is written by `gpsync setup` and by the dashboard's
Settings page; you can also edit it by hand while nothing is running.

It is stored with owner-only permissions because it holds the dashboard
password hash.

## Settings

| Key | Type | What it controls |
|-----|------|------------------|
| `source_folders` | list | Folders gpsync scans and uploads when no path is given |
| `concurrency` | number | How many files upload in parallel; the adaptive limiter may lower it under throttling |
| `upload_quality` | text | `original` (default) or `space_saver`. See below |
| `space_saver_max_dimension` | number | Longest edge in pixels, when `upload_quality = "space_saver"` |
| `space_saver_jpeg_quality` | number | JPEG quality 1-100, when `upload_quality = "space_saver"` |
| `backup_dir` | text | Where `gpsync backup` writes its archives |
| `backup_keep_count` | number | How many backup archives to keep; older ones are pruned |
| `trash_dir` | text | Where `gpsync duplicates resolve` moves copies you do not keep |
| `dupes_dir` | text | Where `gpsync precheck` moves files already in the library |
| `sort_smallest_first` | true/false | Upload smaller files first instead of in path order |
| `media_type_filter` | extensions.Kind | Restrict every run to `photos` or `videos`; empty means both |
| `extra_supported_photo_extensions` | list | Treat these extensions as uploadable photos |
| `extra_supported_video_extensions` | list | Treat these extensions as uploadable videos |
| `extra_unsupported_extensions` | list | Never scan or upload these extensions |
| `extra_ignored_file_names` | list | File names to skip entirely |
| `extra_ignored_dir_names` | list | Directory names to skip entirely |
| `watch_debounce_seconds` | number | Quiet period after a folder changes before it is processed |
| `watch_heartbeat_minutes` | number | How often the watcher retries pending work regardless of activity |
| `theme` | text | Dashboard theme: `dark`, `light` or empty to follow the browser |
| `sync_strategy` | text | `folder_by_folder`, `smallest_first` or `photos_first` |
| `dashboard_listen_addr` | text | Address the dashboard binds to; non-loopback requires login |
| `dashboard_port` | number | Dashboard port (0 picks a free one) |
| `dashboard_auth_enabled` | true/false | Require a username and password for the dashboard |
| `-` | true/false |  |
| `dashboard_auth_user` | text | Dashboard username |
| `dashboard_auth_pass_hash` | text | bcrypt hash of the dashboard password; never the password itself |

## Notes

**`dashboard_listen_addr`** defaults to loopback. gpsync refuses to bind a
non-loopback address unless `dashboard_auth_enabled` is true, so the dashboard
cannot be exposed on a network without a login. There is no TLS support yet —
put it behind a reverse proxy if you need HTTPS.

**`dashboard_port`** is honoured exactly as written. On Windows, ports above
49152 can collide with Hyper-V's reserved ranges; pick something lower if the
dashboard fails to bind.

**Extension lists** take effect on the next scan. Adding an extension to
`extra_unsupported_extensions` also stops existing queued entries for it from
being retried.

## About `upload_quality`

`original` (the default) uploads your file byte for byte. `space_saver`
re-encodes it smaller first, so it uses less of your Google storage.

The name is unfortunate and worth explaining, because it does **not** mean
Google's own "Storage saver" tier:

- Google's Storage saver is server-side compression chosen in the Photos app.
  Since 1 June 2021 it counts against your 15 GB like anything else; only
  photos backed up before that date remain free.
- **The Library API has no quality option at all.** `mediaItems.batchCreate`
  accepts no such field, and Google states that media uploaded through the API
  is stored at original quality and counts toward your storage. The
  account-level toggle does not apply to API uploads.

So the only way for gpsync to use less of your quota is to send fewer bytes,
which is what `space_saver` does locally before uploading. Capture dates and
other EXIF are carried across to the re-encoded file, so photos still appear
under the date they were taken. TIFF is never re-encoded — see
[limitations](limitations.md).
