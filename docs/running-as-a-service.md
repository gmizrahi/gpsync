# Running gpsync in the background

On Windows, `gpsync-tray` does this for you: it sits in the notification area,
watches your folders and serves the dashboard. On Linux and macOS there is no
tray app, so run `gpsync dashboard` (watch loop plus web UI) as a user
service. Unit files for both live in [`packaging/`](../packaging).

## Linux (systemd)

```sh
mkdir -p ~/.config/systemd/user
cp packaging/gpsync.service ~/.config/systemd/user/
systemctl --user daemon-reload
systemctl --user enable --now gpsync

systemctl --user status gpsync     # is it running
journalctl --user -u gpsync -f     # follow the log
```

Run it as your own user, never root: gpsync reads your photo folders and
keeps credentials in `~/.gpsync`. To keep it running when you are not logged
in, enable lingering: `loginctl enable-linger $USER`.

The unit assumes `~/.local/bin/gpsync`. Edit `ExecStart` if you installed it
elsewhere, and swap `dashboard` for `watch` if you do not want the web UI.

## macOS (launchd)

```sh
cp packaging/com.github.gmizrahi.gpsync.plist ~/Library/LaunchAgents/
launchctl load ~/Library/LaunchAgents/com.github.gmizrahi.gpsync.plist

launchctl list | grep gpsync       # is it loaded
tail -f /tmp/gpsync.log            # follow the log
```

The plist assumes `/usr/local/bin/gpsync`; Homebrew on Apple silicon installs
to `/opt/homebrew/bin`, so adjust the path.

macOS guards `~/Pictures` and similar folders. The first run prompts for
access — grant it under **System Settings → Privacy & Security → Files and
Folders**. A launchd agent that starts before you approve it will fail to read
those folders until you do.

## Reaching the dashboard

It binds to loopback by default: <http://127.0.0.1:10924>. To reach it from
another device, enable login in Settings first — gpsync refuses to bind a
non-loopback address without authentication. There is no TLS; put it behind a
reverse proxy if you need HTTPS.

## Stopping it

```sh
systemctl --user disable --now gpsync                             # Linux
launchctl unload ~/Library/LaunchAgents/com.github.gmizrahi.gpsync.plist  # macOS
```
