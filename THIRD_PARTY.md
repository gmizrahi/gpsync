# Third-party notices

gpsync is distributed under the MIT License (see [LICENSE](LICENSE)). It links
the open-source modules listed below, each under its own license. Full license
texts ship with each module and can be read with `go mod download` or on each
project's homepage.

## Trademarks

Google, Google Photos, Google Drive and Google Cloud are trademarks of Google
LLC. gpsync is an independent project and is **not** affiliated with,
endorsed, sponsored or reviewed by Google LLC. References to those names
describe what gpsync interoperates with, nothing more. gpsync contains no
Google source code and bundles no Google credentials; it uses the public
Google Photos APIs with credentials you create yourself.

rclone is a trademark of its respective owner; gpsync can import credentials
from an existing rclone remote but is otherwise unrelated to that project.

## Forked source

`internal/systray` is a Windows-only fork of
[github.com/getlantern/systray](https://github.com/getlantern/systray) v1.2.2,
licensed under Apache-2.0. The upstream license and a statement of changes are
in [internal/systray/LICENSE](internal/systray/LICENSE) and
[internal/systray/NOTICE](internal/systray/NOTICE).

## Linked modules


### Apache-2.0

- cloud.google.com/go/compute/metadata
- github.com/spf13/cobra
- gopkg.in/ini.v1

### BSD-3-Clause

- github.com/fsnotify/fsnotify
- github.com/google/uuid
- github.com/remyoudompheng/bigfft
- github.com/rwcarlsen/goexif
- github.com/spf13/pflag
- golang.org/x/crypto
- golang.org/x/image
- golang.org/x/oauth2
- golang.org/x/sys
- modernc.org/libc
- modernc.org/mathutil
- modernc.org/memory

### MIT

- github.com/disintegration/imaging
- github.com/dustin/go-humanize
- github.com/fatih/color
- github.com/mattn/go-colorable
- github.com/mattn/go-isatty
- github.com/ncruces/go-strftime
- github.com/pelletier/go-toml/v2
- github.com/skratchdot/open-golang
- modernc.org/sqlite

## Test-only dependencies

Modules used only by the test suite are not linked into the released binaries
and are therefore not distributed with gpsync.

