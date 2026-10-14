# Progress logger for uploads and downloads: intent

Date: 2026-10-14. Branch `clean-dry-run`.

## What the user asked for

A progress logger that wraps backend access for uploading and downloading.
It reports whole percents only, so that it is not noisy.

## What exists today

| Command   | What the user sees during the transfer                                                                                                                              |
| --------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `send`    | Nothing at the default level. `Debugf` lines per volume (`backup/backup.go:1880-1893`), then the `Done.` summary on stdout (`backup/backup.go:815-845`).            |
| `receive` | Nothing at the default level. `Debugf("Downloading volume %s.")` (`backup/restore.go:952`) and `Debugf("Downloaded %s.")` (`backup/restore.go:1047`), then `Done.`. |

A 500 GiB send to S3 is silent for hours. The operator cannot tell a stalled
upload from a slow one without `--logLevel debug`, which prints a lot more
than progress.

## Facts the design rests on (verified)

- Every backend reads the volume through `files.VolumeInfo.Read`
  (`files/volumeinfo.go:188-203`): `file`, `gcs`, `b2` and `ssh` use
  `io.Copy(w, vol)`; `azure` uses `io.ReadFull(vol, buf)`
  (`backends/azure_backend.go:193`); `s3` wraps `vol` in a `reader`
  (`backends/aws_s3_backend.go:228-234`) and gives it to s3manager. One hook in
  `Read` sees every byte every backend takes.
- `--maxUploadSpeed` wraps `v.r` with the rate limiter before `Read`
  (`files/volumeinfo.go:340`, `:808`). A hook in `Read` counts after the
  throttle, so it reports what left the process.
- `Download` returns an `io.ReadCloser` (`backends/backends.go:44`). A counting
  wrapper around it sees every byte.
- The send pipeline is a chain, one `retryUploadChainer` per destination
  (`backup/backup.go:730`, the `Setup plumbing` loop). The
  `delete://` link is appended as the last destination when `--maxFileBuffer > 0`.
  Only the final manifest passes the chain (`IsManifest`); partial manifests
  go to the local cache only.
- A failed upload is retried whole: `volUploadWrapper` reopens the volume
  (`backup/backup.go:1911-1931`). A failed download is retried whole:
  `downloadSequenceVolume` calls `processSequence` again
  (`backup/restore.go:928-957`). So a per-object count must reset at each attempt.
- `vol.Size` and `vol.ZFSStreamBytes` are set before `Close`, and `Close`
  happens before the volume enters the chain in temp-file mode
  (`backup/backup.go:1483-1494`). In pipe mode (`--maxFileBuffer 0`) the
  backend reads while the splitter writes, so both are 0 until the upload ends.
- The raw stream estimate comes from `zfs send -n -P` via
  `zfs.GetZFSSendDryRun`. `checkManifestFits` (`backup/backup.go:1334`) already
  calls it once per send and throws the number away. zfs estimates the stream
  before compression and encryption, so the estimate is in raw stream bytes,
  the unit that `vol.ZFSStreamBytes` also counts.
- A receive knows the exact total: the manifest lists every volume's stored
  `Size` (`backup/restore.go:448-451`, `toDownload`).
- `--resume` keeps the verified volumes in `jobInfo.Volumes`
  (`backup/backup.go:1709`) before the pipeline starts.
- `backup/` type-asserts the backend to the optional interfaces `Sizer`,
  `Archiver` and `LegacyVolumeFinder` (`backends/backends.go:48-80`). A wrapper
  that embeds `backends.Backend` hides those, so it must never reach that code.
- The fake `zfs send -n -P` reports `FAKEZFS_STREAM_BYTES` as the size
  (`internal/fakezfs/fakezfs.go:399-406`), so e2e tests get an exact estimate.
- `config.JSONOutput` writes the summary to `config.Stdout`; the logger writes
  to stderr. Progress lines through the logger cannot break `--jsonOutput`.

## Decisions

1. **Where.** A wrapper type in `backends/` (`backends/progress.go`) that
   embeds `backends.Backend` and intercepts `Upload` and `Download` only. It is
   handed to the data path only: `retryUploadChainer` for a send, the download
   workers for a receive. Everything else (`verifiedVolumes`, `syncCache`,
   `clean`, `list`, `plan`) keeps the raw backend, so the optional interfaces
   still resolve.
2. **One tracker per transfer.** A send gets one per real destination (not the
   `delete://` link). A receive gets one per manifest. Each prints its own
   lines, named by `backends.RedactURI(destination)`.
3. **Unit of progress.**
   - send: raw zfs stream bytes against the `zfs send -n -P` estimate. A volume
     in flight contributes `read / Size × ZFSStreamBytes` (temp-file mode) or 0
     until its upload returns (pipe mode). A finished volume contributes
     `ZFSStreamBytes`. Resumed volumes count as finished from the start.
   - receive: stored bytes against the sum of the manifest's volume sizes. A
     volume in flight contributes `min(read, Size)`.
4. **Whole percents, printed once.** A line is printed only when the whole
   percent is higher than the last one printed. A retry that resets a volume's
   count can lower the fraction; it never prints a lower line. At most 100
   lines per transfer.
5. **99 until the set is complete.** The send estimate can be low. The send
   tracker caps at 99 while volumes flow and prints 100 when the final
   manifest's upload returns without error at that destination. The receive
   total is exact, so 100 falls out of the arithmetic when the last byte arrives.
6. **No total, no lines.** When zfs cannot estimate the stream (a Warning
   already says so) or the total is 0, the tracker is off; one `Debugf` says
   why.
7. **Level and flag.** `Noticef`, as every user-facing line (rule 5 in
   `AGENTS.md`). No new flag: `--logLevel warning` silences it, as it silences
   every Notice. Manifests are never counted and never reported.
8. **Rate and time left, from the same data.** The tracker knows the bytes
   it counted and the time since it started. So each line also carries:
   - the **rate on the wire**: stored bytes the backend read or returned,
     divided by the time since the tracker started. A cumulative average, not
     a window: it is stable and needs no extra state. Resumed volumes are not
     counted (they were not transferred by this run).
   - the **time left**: the remaining progress units (raw bytes for send,
     stored bytes for receive) divided by the progress-unit rate of this run.
     For send the two rates differ (raw vs stored); the ETA uses the raw one,
     so compression does not skew it.
     Both are omitted on a line printed less than 2 s after the start, or with
     nothing transferred yet (a rate from almost no data is noise). The time left
     is omitted when the remaining is 0 (sitting at 99 on a low estimate). The
     100% line carries the average rate and the total time instead.
9. **Format.**
   ```
   Uploading tank/data@snap to s3://bucket/p: 37% (12 GiB of an estimated 32 GiB), 48 MiB/s, about 7m left
   Downloading tank/data@snap from s3://bucket/p: 37% (12 GiB of 32 GiB), 48 MiB/s, about 7m left
   Uploading tank/data@snap to s3://bucket/p: 100% (32 GiB of an estimated 32 GiB), 47 MiB/s, took 11m20s
   ```
   `humanize.IBytes` for the sizes and the rate. Time left is rounded to the
   second under a minute, to the minute above. "an estimated" only when the
   total is the zfs estimate.

## Out of scope

- Progress for `clean`, `list`, `plan`, `send --dry-run`, manifest sync, and
  the volume hashing a `--resume` does (`hashObject`).
- A progress bar on a TTY. Logs only.

## Risks

- The zfs estimate is below the real stream: the display sits at 99 until the
  manifest lands. Documented in the line text ("an estimated").
- The rate is a cumulative average: after a stall it recovers slowly, and
  the time left with it. A windowed rate would need a ring of samples; not
  asked for, and the whole-percent cadence means a line comes seldom anyway.
- A mutex per `Read` call. `Read` is called per backend chunk (tens of KiB and
  up), so this is negligible; the unit test runs with `-race`.
- Tools that parse the log see up to 100 new Notice lines per destination.
  That is the requested behavior.
