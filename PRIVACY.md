# Privacy

gpsync runs entirely on your own computer. It has no servers, no accounts and
no telemetry of any kind.

## What leaves your machine

Exactly one thing: the photos and videos you ask gpsync to upload, sent to
Google Photos over HTTPS using **your own** Google API credentials.

There is no analytics, crash reporting, update check or usage tracking. No
data is sent to the author or to any third party.

## What is stored, and where

Everything lives in `~/.gpsync` (`C:\Users\<you>\.gpsync` on Windows):

| File | Contents |
|------|----------|
| `state.sqlite` | The ledger: file paths, content hashes, sizes, capture dates, upload status and error messages |
| `config.toml` | Your settings, including the dashboard login hash if you enable it |
| `client_secret.json` | The OAuth client you created in your own Google Cloud project |
| `token.json` | The OAuth refresh/access token for your Google account |

`client_secret.json` and `token.json` are credentials: anyone who can read
them can upload to your Google Photos library. Keep them as private as you
would a password, and note that `gpsync backup` archives them alongside the
ledger — so treat those archives with the same care.

## Your Google credentials

gpsync ships no API credentials of its own. You create an OAuth client in your
own Google Cloud project, so quota, access and revocation all stay under your
control. You can revoke access at any time at
<https://myaccount.google.com/permissions>.

gpsync requests a single scope, `photoslibrary.appendonly`, which permits
uploading only. It cannot read, modify or delete anything already in your
Google Photos library.

## Uninstalling

Deleting `~/.gpsync` removes every trace of gpsync from your machine. Photos
already uploaded remain in your Google Photos library; gpsync has no way to
delete them, by design and by API limitation.
