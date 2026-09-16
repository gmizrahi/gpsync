# Setup

Google requires every user of the Photos API to create their own credentials.
There is no way around this: an application cannot ship shared credentials,
because the 10,000-requests-per-day quota belongs to the project that owns
them, and one shared project would be exhausted by a single user's first sync.

The upside is that you own the quota, the access, and the ability to revoke it.

## The fast path: reuse an rclone remote

If you already sync with rclone, you already have credentials:

```sh
gpsync setup           # offers to import a detected rclone remote
gpsync import-rclone   # or do it directly
```

That skips everything below.

## Creating credentials

You need a Google account and about five minutes.

**1. Create a project.** Open the
[Google Cloud console](https://console.cloud.google.com/projectcreate), name it
anything (`gpsync` works), and create it.

**2. Enable the Photos Library API.** Visit
[the API page](https://console.cloud.google.com/apis/library/photoslibrary.googleapis.com),
make sure your new project is selected, and press **Enable**.

**3. Configure the consent screen.** At
[OAuth consent screen](https://console.cloud.google.com/apis/credentials/consent),
choose **External**, fill in an app name and your own email, and add **your own
Google account as a test user**.

> Leaving the project in *Testing* mode expires refresh tokens after 7 days,
> which means re-authorising weekly. Publishing the app (still unverified,
> still only usable by you) avoids that. Google's "unverified app" warning is
> expected: you are the developer and the only user.

**4. Create the OAuth client.** At
[Credentials](https://console.cloud.google.com/apis/credentials), choose
**Create credentials → OAuth client ID**, type **Desktop app**, and download
the resulting JSON.

**5. Hand it to gpsync.**

```sh
gpsync setup           # asks for the path to the downloaded JSON
```

A browser opens for consent, and the token is stored in `~/.gpsync/token.json`
with owner-only permissions.

## Checking it worked

```sh
gpsync setup --check
```

This verifies each requirement in turn -- configuration, OAuth client, sign-in
token, and API access -- and, for anything that fails, says what to do about
it. It changes nothing, and it never starts a sign-in as a side effect: a check
it could not run (the API probe before you have signed in, for instance)
reports as `--`, not as a pass.

Two related commands:

```sh
gpsync config     # shows settings and sign-in status
gpsync scan       # should find files without asking for credentials again
```

## Scripting the Google Cloud steps

Most of the work above is clicking through the console. The project and API
parts can be scripted instead:

```sh
gpsync setup --gcloud-script > setup-gcp.sh   # read it, then run it
```

The OAuth consent screen and the Desktop client genuinely cannot be scripted --
Google exposes no API for either -- so those two steps stay manual whatever
tooling you use.

## Choosing what to sync

```sh
gpsync sync "C:\Photos"     # one-off, folder by folder
gpsync watch                # keep watching for new files
```

Set permanent source folders in `~/.gpsync/config.toml` (see
[configuration](configuration.md)) so later commands need no path at all.

## Revoking access

Remove gpsync at <https://myaccount.google.com/permissions>, and delete
`~/.gpsync/token.json`. Photos already uploaded stay in your library.
