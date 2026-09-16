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
| `upload_quality` | text | `original` or `space_saver`; space saver downscales before upload |
| `space_saver_max_dimension` | number | Longest edge, in pixels, for space-saver uploads |
| `space_saver_jpeg_quality` | number | JPEG quality (1-100) for space-saver uploads |
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
