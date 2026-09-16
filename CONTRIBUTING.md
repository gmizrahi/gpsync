# Contributing

Thanks for taking an interest. Issues and pull requests are both welcome.

## Reporting a bug

Include what you ran, what happened, and:

- `gpsync --version`
- the relevant part of `gpsync doctor`
- your OS and whether you were using the CLI or the tray app

Please do not paste `client_secret.json`, `token.json`, or the contents of
`~/.gpsync` — they contain credentials. Security issues go through
[SECURITY.md](SECURITY.md), not a public issue.

## Development

```sh
make check   # go vet, the full test suite, and a build — run this before pushing
make dist    # cross-compile the Windows binaries into dist/
```

Requires Go 1.26 or newer. There is no cgo anywhere, which is what lets the
Windows binaries cross-compile from Linux or macOS.

`gpsync-tray` is Windows-only: `cmd/gpsync-tray` sits behind a
`//go:build windows` constraint, so `go build ./...` on other platforms skips
it. Use `make dist` (or `GOOS=windows go build ./...`) to compile it.

## What the code expects of a change

- **Tests for behaviour, not for coverage.** Every bug fix gets a test that
  fails without the fix. The suite has no network access and no real
  credentials; upload paths are tested against an `httptest` server.
- **Comments explain why.** Long explanations of a non-obvious decision are
  welcome; narration of how the code came to be is not.
- **The ledger is the contract.** Anything that changes `internal/statedb`'s
  schema needs a migration, because users' databases are years old and large.
- **No telemetry, ever.** gpsync makes exactly one kind of outbound request:
  the Google Photos API, with the user's own credentials.

## Layout

| Path | Contents |
|------|----------|
| `cmd/gpsync` | The CLI |
| `cmd/gpsync-tray` | Windows tray app (Windows-only build) |
| `internal/engine` | Orchestration shared by CLI and tray |
| `internal/uploader` | The upload pipeline, retries, throttle handling |
| `internal/statedb` | SQLite ledger and migrations |
| `internal/scanner` | Walking folders, hashing, capture dates |
| `internal/dashboard` | The web UI |

## Before you open a pull request

`make check` passes, `gofmt` is clean, and — if you changed anything the
dashboard serves or the ledger stores — you have run it against a **copy** of
a real ledger, never the live one.
