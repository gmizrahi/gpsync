# HTTP API

gpsync serves a small HTTP API, under `/api/v1/`, for **programs on the same
machine**: a script, or a companion app. It is served by whichever gpsync
process runs the dashboard (the tray app, or `gpsync dashboard`), on the same
port.

A program can list the photos and videos gpsync knows, by capture time,
and fetch a thumbnail or the original of each.

## Authentication

Every request carries a token:

```
Authorization: Bearer gps_...
```

- **A token is always required**, whether or not the dashboard itself has
  login on.
- **Loopback only.** A request from any address other than this machine's
  loopback is refused with `403`, even when the dashboard listens on every
  interface.
- **The API and the dashboard don't share credentials.** A dashboard login
  doesn't open the API, and a token doesn't open the dashboard.
- **There are no CORS headers**, so a web page can't read API responses.
  Call the API from a program, not from browser JavaScript.

A token carries **scopes**. A route that needs a scope the token lacks
answers `403` with `WWW-Authenticate: Bearer error="insufficient_scope"`.

| Scope | Allows |
|---|---|
| `read` | Read-only queries: the token itself and the media endpoints |
| `control` | Actions on the running tray app, such as quitting it |

## Getting a token

Create one with the CLI:

```
gpsync token create --name photos-app > token.txt
```

Or from the dashboard: **Settings › API tokens**.

A token is shown **once**, when it's created. gpsync keeps only its SHA-256,
so a lost token can't be recovered: revoke it and create another.
`gpsync token create` writes only the token to stdout, so it can go straight
into a file.

| | |
|---|---|
| Lifetime | `--expires 90d`, `12h`, `30m` or `never`. Without `--expires`, the `api_token_lifetime` setting applies (90 days unless changed). |
| Scopes | `--scope read` (the default), `control`, or `read,control`. Tokens created from Settings can only read. |
| List | `gpsync token list`, or Settings: name, scopes, created, last used, expiry. Never the token itself. |
| Revoke | `gpsync token revoke NAME`, or Settings. The very next request with that token is refused. |

## Endpoints

### `GET /api/v1/token`

Describes the token making the request. Scope: `read`.

```json
{"name": "photos-app", "scopes": ["read"], "expires_at": "2027-01-23T12:00:00Z"}
```

`expires_at` is `null` for a token that never expires.

### `POST /api/v1/token/rotate`

Trades the token making the request for a new one. Any token may rotate
itself, whatever its scopes. The new token has the same name and scopes,
and its lifetime is renewed: a token created for 30 days gets another 30
days, and one that never expires stays that way.

```json
{"name": "photos-app", "scopes": ["read"], "expires_at": "2027-01-23T12:00:00Z", "token": "gps_..."}
```

The old token keeps working for **10 minutes**. If the response is lost,
retry with the old token: the retry returns another new token, which
replaces the one you never received, and does not extend those 10 minutes.

**So a program never needs a new token pasted in.** Check `expires_at` now
and then. Once about half the lifetime has passed, rotate, write the new
token to your secrets file atomically, and then start using it.

### `POST /api/v1/control/quit`

Asks the tray app to quit gracefully: uploads in progress finish, and
nothing new starts. Answers `202` before shutting down. Scope: `control`.

`gpsync tray-quit` uses this route, with a token it mints for itself (see
below).

### `GET /api/v1/media`

Lists files by capture time, oldest first. Scope: `read`.

| Parameter | |
|---|---|
| `from` | Optional. RFC 3339, such as `2024-05-01T00:00:00+02:00`. Files captured at or after it. |
| `to` | Optional. RFC 3339. Files captured before it. |
| `limit` | Optional. Items per page, 1 to 1000 (default 200). |
| `cursor` | Optional. The `next` value of the previous page. |

```json
{
  "items": [
    {"sha256": "1c22…f031", "captured_at": "2024-05-01T09:12:44+02:00",
     "kind": "photo", "mime_type": "image/jpeg", "has_thumb": true}
  ],
  "next": "MTcxNDU0…"
}
```

- `sha256` identifies the file by its content, and is what the other media
  endpoints take.
- `kind` is `photo` or `video`, or `""` for a file whose extension gpsync no
  longer recognizes.
- `has_thumb` says whether the thumbnail endpoint can make one: not for
  videos, nor for HEIC and RAW photos.
- `next` is `null` on the last page. Pass it back as `cursor` to get the
  following page; paging neither skips nor repeats a file, even when several
  share a capture time.

Every file gpsync scanned is listed — uploaded or not yet — except files
that are ignored, waiting for a review decision, or confirmed missing.

**What `captured_at` means.** It's in this machine's time zone, and comes
from, in order:

1. the photo's EXIF `DateTimeOriginal` (or `DateTime`), read as this
   machine's local time, since EXIF usually doesn't record a time zone;
2. a date in the file's path, such as `2024-05-01` or `20240501`, at noon
   UTC; a bare year means January 1st;
3. the file's modification time.

gpsync doesn't record which of these a file's time came from.

### `GET /api/v1/media/{sha256}/thumb`

A JPEG thumbnail. Scope: `read`.

`size` is the long edge in pixels, rounded up to 256, 512 or 1024
(default 256). A thumbnail is made from the local original the first time
it's requested, never in advance, and cached within 1 GiB.

The cache is in `~/.gpsync/thumbs` unless the `thumbnail_dir` setting
(Settings › API) names another folder. That folder can be inside a source
folder, such as `D:\Pictures\.thumbnails`. Scans skip it, by its exact
path, and gpsync marks a dot-named folder it creates as hidden on Windows.
It can't be a source folder or contain one, since scans would then skip
your photos. gpsync only reads or removes the files it made there,
`<size>/<sha256>.jpg`; anything else in the folder is left alone. A file's
thumbnail never
changes, so responses carry `Cache-Control: private, max-age=31536000,
immutable` and an `ETag`; a request with a matching `If-None-Match` gets
`304`.

| Status | |
|---|---|
| `404` | Unknown or unlisted file, or its original isn't available (see below). |
| `415` | The format has no thumbnail (`has_thumb` is `false`). |

### `GET /api/v1/media/{sha256}`

The original file's bytes, with its `Content-Type`. Supports `Range`
requests, so a video can be streamed. Cached like a thumbnail. Scope:
`read`.

**When an original isn't available.** gpsync serves a file only from a
scanned path where it is still unchanged: same size and modification time
as when it was scanned. A file that was moved, deleted or edited since
answers `404` until the next scan. An edited file then gets a new
`sha256`, as a new item.

Nothing under `/api/v1/media` changes gpsync's database.

## Errors

| Status | Meaning |
|---|---|
| `400` | A parameter the route can't use, such as a malformed `from`, `cursor` or `sha256`. The body says which. |
| `401` | Missing, malformed, unknown, expired or revoked token — deliberately indistinguishable. Comes with `WWW-Authenticate: Bearer`. |
| `403` | Not from loopback, or the token lacks the route's scope. |
| `404` | No such route, or no such file (media). |
| `405` | Wrong method; `Allow` says which. |
| `429` | Too many failed attempts from this address. A request with a valid token is still served. |

Every response is `Cache-Control: no-store` and `X-Content-Type-Options:
nosniff`, except a media thumbnail or original, which is cached as described
above.

Failed attempts and token events are logged, to `tray.log` for the tray
app. Tokens themselves are never logged.

## How gpsync's own CLI authenticates

CLI commands that talk to a running tray, such as `gpsync tray-quit`, need
no login and no configuration. The CLI can already write gpsync's database,
which is a stronger credential than any token. So for each command it mints
a token that is scoped to just what the command needs, valid for two
minutes, and deleted when the command ends. Nothing secret stays on disk in
between.
