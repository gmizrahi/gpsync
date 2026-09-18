# Command reference

Every command supports `--help`, which carries the same text as this page plus
its flags. Run `gpsync <command> --help` for the authoritative version.


## Getting connected

### `gpsync setup`

Interactive GCP + OAuth setup wizard.

### `gpsync import-rclone`

Import an existing rclone Google Photos remote's OAuth credentials.

| Flag | Meaning |
|------|---------|
| `--conf string` | Path to rclone.conf (auto-detected if omitted) |
| `--remote string` | Remote name to import (prompted if omitted and multiple exist) |

### `gpsync config`

Show current settings and auth status.


## Everyday syncing

### `gpsync scan`

Find new and changed files and queue them for upload.

Scans the given folders, or the configured source folders, and queues new and changed files. Unchanged files are not hashed again. Deleted and moved files are detected as well; see gpsync recheck.

```sh
gpsync scan
gpsync scan "C:\Photos\2026" --media-type videos
```

| Flag | Meaning |
|------|---------|
| `--concurrency int` | Files hashed in parallel (default 2; raise it for SSDs) |
| `--media-type string` | Only photos or videos (default: all) |

### `gpsync sync`

Scan and upload folders.

Scans each folder that contains files and uploads what is pending, one folder at a time. Uses the configured source folders if none are given; folders and globs are accepted.

A file that was already uploaded is never sent again, whatever its name or location.

```sh
gpsync sync
gpsync sync "C:\Photos\2026"
gpsync sync "C:\Photos\2026\2026_0*" --media-type photos
```

| Flag | Meaning |
|------|---------|
| `--concurrency int` | Parallel uploads (overrides config) |
| `--force` | Upload everything in these folders again, including files already uploaded or marked synced |
| `--media-type string` | Only photos or videos (default: all) |
| `--verbose` | List every file's status at the end |

### `gpsync upload`

Upload pending files.

Uploads all pending files, or only those under the given folders. Stays within the daily quota and retries after throttling.

--smallest-first uploads smaller files first within each folder; --smallest-first-global does so across all folders.

```sh
gpsync upload
gpsync upload "C:\Photos\2026" --media-type photos
gpsync upload --smallest-first-global
```

| Flag | Meaning |
|------|---------|
| `--concurrency int` | Parallel uploads (overrides config) |
| `--media-type string` | Only photos or videos this run (default: all) |
| `--quality string` | Upload quality this run: original or space_saver (default: config) |
| `--smallest-first` | Smallest files first, within each folder |
| `--smallest-first-global` | Smallest files first, across all folders |

### `gpsync watch`

Sync continuously as files change.

Uploads changes as they happen, and catches up on existing folders in the background, until stopped with Ctrl+C. Uses the configured source folders if none are given. New subfolders are watched automatically.

A changed folder is processed once it has been quiet for --debounce. Every --heartbeat, pending files are retried, which also recovers from throttling and the daily quota.

```sh
gpsync watch
gpsync watch --heartbeat 30m
```

| Flag | Meaning |
|------|---------|
| `--concurrency int` | Parallel uploads (overrides config) |
| `--debounce duration` | Wait this long after a folder's last change (default 8s) |
| `--heartbeat duration` | How often to retry pending files (default 15m0s) |
| `--verbose` | Show per-file progress and status |

### `gpsync dashboard`

Sync continuously with a web dashboard.

Runs the same loop as gpsync watch and serves the gpsync-tray dashboard over HTTP until stopped with Ctrl+C. Uses the configured source folders if none are given.

The address, port and login come from config.toml. The dashboard listens only on this computer unless dashboard login is enabled.

```sh
gpsync dashboard
gpsync dashboard --port 10925
```

| Flag | Meaning |
|------|---------|
| `--listen string` | Listen address for this run, e.g. 127.0.0.1 |
| `--port int` | Port for this run (0 picks a free port) |


## Seeing what is in the ledger

### `gpsync info`

Show what is synced, pending and failed, and what to do next.

### `gpsync pending`

List files waiting to be uploaded, by folder.

Lists pending files grouped by folder, in upload order: the whole ledger, or only the given folders.

```sh
gpsync pending --summary
gpsync pending "C:\Photos\2026"
```

| Flag | Meaning |
|------|---------|
| `--summary` | Per-folder counts and totals only |

### `gpsync log`

Show failed uploads and their errors.

| Flag | Meaning |
|------|---------|
| `--permanent` | Only permanent failures |

### `gpsync extensions`

Show the file extensions seen and how their uploads went.

### `gpsync whatis`

Identify a file by its content.

Hashes the file and shows which tracked file has the same content and what gpsync did with it; useful for a photo downloaded from Google Photos. Renamed copies match; copies whose bytes changed do not. Changes nothing.

Example: gpsync whatis "C:\Users\me\Downloads\IMG_1234.jpg"

### `gpsync quota`

Show or correct today's API request count.

Shows today's API requests against the limit of 10,000 per day. Use set or add to count requests gpsync did not make, such as rclone using the same credentials.

```sh
gpsync quota
gpsync quota add 250
gpsync quota set 4000
```

### `gpsync throttle-log`

Show when Google throttled uploads and how long recovery took.

Lists each time Google throttled uploads: when it happened, the backoff step and wait, how much was uploaded just before, and how long until the next successful upload. Changes nothing.

```sh
gpsync throttle-log
gpsync throttle-log --analyze
gpsync throttle-log --csv > throttles.csv
```

| Flag | Meaning |
|------|---------|
| `--analyze` | Recovery times grouped by backoff step (min, max, average) |
| `--csv` | Output CSV |


## Checking and repairing

### `gpsync recheck`

Review missing files and permanent failures, and clean them up.

Without flags, reports what needs attention and changes nothing.

Missing files are ledger entries whose file is no longer on disk. Scans flag them; they are confirmed once no moved copy exists under the source folders. Forgetting an entry never touches Google Photos.

```sh
gpsync recheck                 Report only
gpsync recheck --revalidate    Re-check flagged files, e.g. after reattaching a drive
gpsync recheck --missing       Forget files confirmed missing
gpsync recheck --retry         Queue failures that can be retried
gpsync recheck --all           Same as --missing --unsupported --retry
```

| Flag | Meaning |
|------|---------|
| `--all` | Same as --missing --unsupported --retry |
| `--missing` | Forget files confirmed missing (each is re-checked first) |
| `--retry` | Queue permanent failures whose format is supported for another attempt |
| `--revalidate` | Re-check flagged files; clear those back on disk or moved |
| `--unsupported` | Forget permanent failures with an unsupported format (files are not touched) |

### `gpsync doctor`

Check the ledger for problems and repair the safe ones.

Checks for: - a run still marked active after a crash - files waiting in the retry queue - files confirmed missing from disk - originals-review items that can be resolved automatically - permanent failures - queued files whose path now holds different content, because it was edited

Without --fix it only reports, and is safe to run at any time. --fix repairs what needs no decision; for everything else the report names the command to use.

| Flag | Meaning |
|------|---------|
| `--fix` | Repair the problems that need no decision |

### `gpsync verify`

Check that uploaded files still exist in Google Photos.

Looks up a sample of uploaded files in Google Photos by media item ID, least recently checked first, so repeated runs cover the whole library. A file reported missing was deleted in Google Photos or never stored. Network and permission errors are reported as inconclusive.

Example: gpsync verify --sample 200

| Flag | Meaning |
|------|---------|
| `--force` | Try even without read access, e.g. with an rclone-imported token |
| `--sample int` | Number of files to check (default 50) |

### `gpsync fix-dates`

Correct capture dates from the dates in folder and file names.

Files without readable EXIF get their capture date from the file's modified time, which copying and backups can change. This reads the date from the path instead (a 2002 or 2002_07 folder, or a name such as IMG-20220701-WA0060.jpg) and updates the stored date wherever the year differs.

Safe to run repeatedly.

| Flag | Meaning |
|------|---------|
| `--dry-run` | Show what would change without writing |


## Duplicates and originals

### `gpsync duplicates`

List files with the same content at more than one path.

Lists groups of tracked files with identical content, largest reclaimable space first. Google Photos holds one copy either way; this helps you clean up local copies.

Example: gpsync duplicates resolve Choose which copy to keep, group by group

### `gpsync precheck`

Set aside files already synced before importing a folder.

Hashes every file under <folder> and moves those already synced into the dupes folder, leaving only new files. Run it on a phone backup before copying it into your library. Nothing is added to the ledger.

Moved files keep their names; a name that is already taken gets a suffix, such as photo_1.jpg. Set the dupes folder once with --dupes-dir; it must be outside every source folder.

```sh
gpsync precheck "C:\Phone backup" --dupes-dir "C:\Dupes"
gpsync precheck "C:\Phone backup" --dry-run
```

| Flag | Meaning |
|------|---------|
| `--dry-run` | Show what would move without moving anything |
| `--dupes-dir string` | Folder to move duplicates into (saved as the default) |

### `gpsync originals-uploaded`

List uploaded files that came from an "originals" folder.

Lists files already uploaded from inside an "originals" folder. These are likely duplicates of their edited versions in Google Photos. Each line says whether an edited copy is tracked next to the folder; if it is, the file is almost certainly a duplicate.

gpsync cannot delete from Google Photos; remove duplicates there by hand. Changes nothing.

### `gpsync clean-originals`

Move queued files from "originals" folders into review.

Finds pending and retryable files inside an "originals" folder (the pre-edit copies some photo editors keep) and moves them into review. Review them in gpsync-tray's Originals tab, where each one can be queued or ignored.

Shows what would change unless --commit is given.

| Flag | Meaning |
|------|---------|
| `--commit` | Apply the changes |


## Correcting the ledger

### `gpsync mark-synced`

Mark files as already in Google Photos, without uploading.

Hashes the given files and records them as uploaded, without contacting Google. Use it for files uploaded some other way, such as the Google Photos app or rclone.

Accepts folders (including subfolders), files and globs; quote globs in PowerShell. A folder is marked as it is now: files added later are scanned as new. Unsupported and system files are skipped.

--unmark queues the files for upload again, whether they were marked or actually uploaded.

```sh
gpsync mark-synced "C:\Photos\2019"
gpsync mark-synced "C:\Photos\2022\SummerTrip\*.mp4" --quality original
gpsync mark-synced "C:\Photos\2019" --unmark
```

| Flag | Meaning |
|------|---------|
| `--concurrency int` | Files hashed in parallel (default 2; raise it for SSDs) |
| `--quality string` | Quality of the existing copies: original or storage-saver (default: upload_quality in config) |
| `--unmark` | Queue the files for upload again |

### `gpsync quality`

Record the quality of files already in Google Photos.

Records whether uploaded files are stored as original or storage-saver. Google Photos does not report this, so gpsync relies on what you record here. gpsync reupload uses it to replace storage-saver copies.

Applies to every uploaded file, or only those under the given folders.

Example: gpsync quality storage-saver "C:\Photos\2021"

### `gpsync reupload`

Queue storage-saver files to be replaced by originals.

Queues every uploaded file recorded as storage-saver (see gpsync quality) for upload again. Google Photos replaces the existing copy instead of adding a second one.

Shows what would be queued unless --commit is given. Pass folders to limit it.

```sh
gpsync reupload
gpsync reupload "C:\Photos\2021" --commit
```

| Flag | Meaning |
|------|---------|
| `--commit` | Queue the files |


## gpsync's own data

### `gpsync backup`

Back up gpsync's ledger, settings and credentials.

Creates a timestamped .zip of ~/.gpsync (the ledger, config.toml and the OAuth credentials) and deletes the oldest backups beyond --keep. It backs up gpsync's own data, not your photos.

The archive contains a working sign-in token for your Google account. Keep it somewhere private.

--dest and --keep are saved as the new defaults.

```sh
gpsync backup
gpsync backup --dest "C:\gpsync-backups" --keep 10
gpsync backup list
```

| Flag | Meaning |
|------|---------|
| `--dest string` | Backup folder (saved as the default) |
| `--keep int` | Number of backups to keep (saved as the default) |

### `gpsync restore`

Restore ~/.gpsync from a backup.

Replaces the ledger, settings and credentials in ~/.gpsync with the contents of a backup. The current files are copied next to the backup first.

```sh
gpsync backup list
gpsync restore <backup-file>
```

| Flag | Meaning |
|------|---------|
| `--yes` | Don't ask for confirmation |


## Other

### `gpsync tray-quit`

Ask gpsync-tray to finish current uploads and exit.

Asks a running gpsync-tray to exit gracefully: uploads in progress finish and nothing new starts. Use it before replacing gpsync-tray.exe.

Exits with 0 if the tray was asked to quit or was not running.

### `gpsync completion`

Generate the autocompletion script for the specified shell.

### `gpsync help`

Help about any command.

Help provides help for any command in the application. Simply type gpsync help [path to command] for full details.

