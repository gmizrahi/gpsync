# Security

## Reporting a vulnerability

Please report security issues privately through GitHub's
[security advisories](https://github.com/gmizrahi/gpsync/security/advisories/new)
rather than a public issue. I aim to acknowledge reports within a week.

Include what you did, what happened, and the gpsync version (`gpsync --version`).

## Threat model

gpsync runs locally and stores everything in `~/.gpsync`. The assets worth
protecting are your Google OAuth credentials and the local dashboard.

**Credentials.** `client_secret.json` and `token.json` are restricted to
your user account. On Linux and macOS that is mode 0600 inside a 0700
directory; Windows does not implement those mode bits, so gpsync applies an
ACL granting access to the owning account alone and blocking inherited
permissions. They are never logged. The
OAuth scope requested is `photoslibrary.appendonly`: uploads only, so a stolen
token cannot read, alter or delete your existing library. Revoke access at
<https://myaccount.google.com/permissions>. Note that `gpsync backup` archives
these files, so backup destinations deserve the same care.

**The dashboard.** It binds to loopback by default. gpsync refuses to bind a
non-loopback address unless dashboard login is enabled, so it cannot be
exposed on a LAN without authentication. When login is on: passwords are
hashed with bcrypt, usernames compared in constant time, sessions are
32 bytes from `crypto/rand`, cookies are `HttpOnly`/`SameSite=Lax` (and
`Secure` when served over TLS), repeated failures are rate-limited, mutating
requests carry CSRF origin checks, and the post-login redirect is restricted
to same-site paths.

There is **no TLS support yet**: serve it over loopback, or put it behind a
reverse proxy that terminates TLS.

**File access.** Endpoints that serve files (thumbnails, originals, backup
restore) validate the requested path against the ledger's known candidates
rather than trusting the request. Restoring an archive rejects entries whose
names would escape the state directory and caps how far any entry may
decompress.

## Verification

Every push runs `go vet`, the full test suite on Linux and Windows,
golangci-lint (including gosec), CodeQL, and `govulncheck`. Dependency
updates arrive through Dependabot. The suite includes regression tests for
the security properties above: path traversal, CSRF, session handling, login
lockout, open redirect and archive extraction.

## Scope

gpsync has no server component, no telemetry and no auto-update. It talks to
exactly one external service, the Google Photos API, using credentials you
create. See [PRIVACY.md](PRIVACY.md).
