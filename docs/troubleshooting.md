# Troubleshooting

Start with `gpsync doctor`. It checks the ledger for every problem gpsync
knows how to describe, and changes nothing unless you pass `--fix`.

## Uploads stopped and everything says "retryable"

Google throttled the account and gpsync exhausted its backoff ladder. This is
normal on a large library and not an error in the usual sense.

```sh
gpsync throttle-log --analyze   # how long recovery actually took, per rung
gpsync info                     # what is left
```

The watcher retries automatically on its next heartbeat (15 minutes by
default). `gpsync sync` picks up where it left off. Nothing is lost: files are
requeued, not failed.

## "Quota exceeded for quota 'concurrent write request'"

The same throttle, seen directly. Google does not document the limit. gpsync
backs off from 5 minutes up to an hour, then hands the work back to the next
run. Lowering `concurrency` in the config reduces how often you hit it.

## The daily quota ran out

10,000 API requests per calendar day, per Google Cloud project. `gpsync quota`
shows today's usage. It resets at midnight Pacific time. If something outside
gpsync used the same project, `gpsync quota add <n>` corrects the local count.

## A file will not upload

```sh
gpsync log --permanent    # why each permanently failed file failed
gpsync recheck            # report what could be retried or forgotten
gpsync recheck --retry    # queue failures whose format is now supported
```

Unsupported formats are refused before upload; `gpsync extensions` lists what
has been seen and how it fared.

## Files are missing from the ledger after I moved them

Scans flag entries whose file has gone, and a full pass decides what happened:
a moved file is repointed, a deleted one is confirmed missing.

```sh
gpsync recheck               # report only
gpsync recheck --revalidate  # re-check now, e.g. after reattaching a drive
gpsync recheck --missing     # forget files confirmed gone
```

`--missing` re-checks each file on disk before forgetting it, and refuses
entirely while any source folder is unavailable.

## The dashboard will not start

The configured port may be taken. On Windows, Hyper-V reserves blocks above
49152 — `netsh int ipv4 show excludedportrange protocol=tcp` shows them.
Pick a lower port in the config.

If it binds but you cannot reach it from another device: that is deliberate.
gpsync only binds a non-loopback address when dashboard login is enabled.

## Something is wrong with the ledger itself

```sh
gpsync doctor          # report
gpsync doctor --fix    # repair only the unambiguous problems
gpsync backup          # snapshot before any bigger surgery
gpsync restore <file>  # roll back to a snapshot
```

The ledger is the only record of what gpsync uploaded — Google's API cannot be
queried to rebuild it. Back it up.
