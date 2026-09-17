# Limitations

Most of these come from the Google Photos API rather than from gpsync, and
they are worth knowing before you rely on the tool.

## gpsync cannot delete anything from Google Photos

The Photos API offers no delete operation to third-party applications. Nothing
gpsync (or rclone, or any similar tool) uploads can be removed
programmatically. Deleting duplicates or junk uploads is manual, in the Google
Photos web UI.

Deleted items also sit in the Photos trash for 60 days and keep counting
against your storage until the trash is emptied.

## gpsync cannot read your library

Since April 2025 Google has removed the read scopes that let an application
list or search a user's whole library. An application can only see items *it
uploaded itself*, and even that is unreliable in practice: listing the same
date range repeatedly can return different counts, and older uploads may not
appear at all.

gpsync therefore requests `photoslibrary.appendonly` — upload permission only.
`gpsync verify` exists but is inert on such a credential, because Google
answers a read of an unreadable item with 404, which is indistinguishable from
"this photo is gone".

The practical consequence: **gpsync's ledger, not Google, is the record of
what was uploaded.** Back it up (`gpsync backup`).

## Albums are not supported

Album support was implemented, then removed. `albums.batchAddMediaItems`
rejects media IDs that `mediaItems.batchCreate` minted seconds earlier
("Request contains an invalid media item id"), and albums the same client just
created return 404. Verified against a real library: roughly 16,000 outstanding
links, 115 that ever succeeded. If you want albums, pass `albumId` at creation
time instead of linking afterwards.

## Quota and throttling

Each Google Cloud project gets 10,000 API requests per day. Separately, Google
throttles "concurrent write requests" without documenting a rate; in practice a
heavy upload run hits it, and gpsync then climbs a backoff ladder from 5
minutes to an hour. Large libraries take days, and that is normal. See
`gpsync throttle-log --analyze` for your own history.

## Uploads are not byte-identical round trips

Downloading a photo back from Google Photos does not return the original file:
EXIF location is stripped and images are re-encoded. Google Photos is not a
byte-exact backup, and neither is anything built on this API.

## Storage quota is invisible

The API exposes no storage-usage figure, so gpsync cannot tell you how full
your account is. Check that in the Google One UI.

## Platform

The CLI builds and runs anywhere Go does. The tray app is Windows-only — it
uses the Win32 notification-area APIs directly. There is no macOS or Linux
tray build.

## TIFF files are always uploaded at original quality

`upload_quality = "space_saver"` downscales JPEG, PNG and WebP before
uploading. TIFF is deliberately excluded: downscaling it would mean decoding
it, and the pure-Go TIFF decoder gpsync depends on has a known crash on
crafted files with no fixed version available. Since TIFF is lossless,
uploading the original is the better result in any case.

TIFF files are still uploaded normally — only the downscaling step skips
them.
