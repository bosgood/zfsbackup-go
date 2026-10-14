# Progress logger for uploads and downloads: plan

Date: 2026-10-14. Source: `intent.md`. Read it first; the decisions there are
not repeated here.

## Tasks

| Task | What                                     | Files                                                                |
| ---- | ---------------------------------------- | -------------------------------------------------------------------- |
| 1    | read hook on a volume                    | `files/volumeinfo.go`, `files/volumeinfo_progress_test.go` (new)     |
| 2    | the tracker and the two wrappers         | `backends/progress.go` (new), `backends/progress_test.go` (new)      |
| 3    | `checkManifestFits` returns the estimate | `backup/backup.go`                                                   |
| 4    | wire the upload wrapper into `Backup`    | `backup/backup.go`                                                   |
| 5    | wire the download wrapper into `receive` | `backup/restore.go`                                                  |
| 6    | e2e tests with the fake zfs              | `e2e_progress_test.go` (new)                                         |
| 7    | docs                                     | `docs/context/backends.md`, `send.md`, `receive.md`, `data-model.md` |
| 8    | break proofs                             | a scratch copy, see below                                            |
| 9    | run                                      | Makefile targets, see below                                          |

Order: 1, 2, 3, 4, 5, 6, 7, 8, 9. Tasks 1 and 2 can be one commit; 3, 4 and 5
one commit; 6 one commit; 7 one commit.

## Task 1: read hook on a volume

`files/volumeinfo.go`:

- Add a field `onRead func(n int)` to `VolumeInfo` (unexported, next to
  `counter`).
- Add `func (v *VolumeInfo) SetReadHook(fn func(n int))`. It sets the field.
  `nil` clears it.
- In `Read` (`files/volumeinfo.go:188`), after `i, err := v.r.Read(p)`: if
  `v.onRead != nil && i > 0 { v.onRead(i) }`. Before the pgp/EOF handling, so
  the hook sees the bytes of the last read too.
- The hook is set and cleared by the wrapper around one `Upload` call. A
  volume is read by one destination at a time (the chain hands it on after the
  upload returns), so the field needs no lock. Say so in the field comment.

Test (`files/volumeinfo_progress_test.go`): create a volume with
`CreateSimpleVolume`, write 1 MiB, `Close`, `OpenVolume`, set a hook that sums
`n`, `io.Copy(io.Discard, vol)`, and check the sum equals `vol.Size`. Then
`SetReadHook(nil)` and read again: the sum does not change. Set
`config.BackupTempdir = t.TempDir()` (napkin: helpers outside a cobra run must).

## Task 2: the tracker and the two wrappers

`backends/progress.go` (new). All of it is in `backends` because it wraps
`backends.Backend` and the package already imports `files` and `log`.

### `Progress`

```go
// Progress counts the bytes of one transfer and logs each whole percent once.
type Progress struct {
    mu       sync.Mutex
    label    string            // "Uploading tank/data@snap to s3://b/p" or "Downloading ... from ..."
    total    uint64            // the denominator; 0 turns the tracker off
    estimate bool              // total is the zfs estimate: cap at 99 until Complete
    done     uint64            // bytes of finished objects (and resumed volumes)
    inflight map[string]uint64 // object name -> bytes this attempt has transferred
    weight   map[string]uint64 // receive: object name -> expected stored size
    last     int               // last whole percent printed, -1 before the first line
    complete bool
    // Rate and time left. seeded is what resumed volumes contributed to done at
    // construction; wire is the stored bytes this run moved. now is time.Now,
    // replaced by tests.
    start  time.Time
    seeded uint64
    wire   uint64
    now    func() time.Time
}

func NewUploadProgress(label string, estimate uint64, resumed []*files.VolumeInfo) *Progress
func NewDownloadProgress(label string, volumes []*files.VolumeInfo) *Progress
```

- `NewUploadProgress`: `total = estimate`, `estimate = true`, `done = Σ
resumed[i].ZFSStreamBytes`, `seeded = done`. `label` is built by the caller
  (task 4). Both constructors set `start = now()` and `now = time.Now`.
- `NewDownloadProgress`: `total = Σ volumes[i].Size`, `estimate = false`,
  `weight[name] = Size` for each. Only names in `weight` are counted; a
  `Download` of any other name (a manifest) passes through unchanged.
- `func (p *Progress) current() uint64`: `done + Σ inflight`, computed under
  `mu`.
- `func (p *Progress) report()`: under `mu`. If `total == 0` return. `pct :=
int(current * 100 / total)`; if `pct > 99 && estimate && !complete` then
  `pct = 99`; if `pct > 100` then `pct = 100`. If `pct > last`: `last = pct`,
  `Noticef("%s: %d%% (%s of %s)%s", label, pct, humanize.IBytes(current capped
at total), totalText, p.pace(pct))` where `totalText` is `"an estimated " +
humanize.IBytes(total)` when `estimate`, else `humanize.IBytes(total)`.
- `func (p *Progress) pace(pct int) string`: under `mu` (called by `report`).
  `elapsed := now().Sub(start)`. If `elapsed < 2*time.Second || wire == 0`
  return `""`. `rate := float64(wire) / elapsed.Seconds()` (stored bytes/s).
  If `pct == 100` return `", <rate>/s, took <elapsed rounded>"`. Else
  `moved := current - seeded` (progress units this run); if `moved == 0`
  return `", <rate>/s"`. `left := (total - current) / (moved / elapsed)`; if
  `total <= current` return `", <rate>/s"`; else return `", <rate>/s, about
<left rounded> left"`. `<rate>` is `humanize.IBytes(uint64(rate))`.
  Rounding (`roundDuration`): under a minute to the second, else to the
  minute; `time.Duration.String()` after that (`7m0s` is fine; strip a
  trailing `0s` for readability: `7m`).
- `wire` is fed by both wrappers: the upload read hook adds `n` to it for
  every volume (also in pipe mode, where the hook adds nothing to `inflight`),
  and the download reader adds `n` before the cap. So the wire rate in pipe
  mode is live even though the percent moves per volume.
- `func (p *Progress) Complete()`: `complete = true`, `done = total`,
  `inflight = nil`, `report()`. Prints 100 once, with the rate and `took`. A
  second call prints nothing.
- Printing from inside `mu` is fine: the logger does not call back.

### `uploadProgress`

```go
type uploadProgress struct {
    Backend
    p *Progress
}

func WithUploadProgress(b Backend, p *Progress) Backend
```

`Upload(ctx, vol)`:

1. `vol.IsManifest`: call `b.Upload`; if it returns nil and
   `vol.IsFinalManifest`, call `p.Complete()`. Return the error. Manifests are
   never counted (`ZFSStreamBytes` is 0 for them anyway).
2. Else: under `mu`, `inflight[vol.ObjectName] = 0` (a retry starts over).
   Take `size := vol.Size` and `raw := vol.ZFSStreamBytes` once; both are 0 in
   pipe mode.
3. `vol.SetReadHook(func(n int) { ... })`: under `mu`, `wire += n`; add `n`
   to a per-call `read` counter; if `size > 0`, `inflight[name] = min(read,
size) * raw / size`; `report()`. In pipe mode (`size == 0`) the hook adds
   to `wire` only.
4. `err := b.Upload(ctx, vol)`; `vol.SetReadHook(nil)`.
5. If `err == nil`: under `mu`, `delete(inflight, name)`, `done +=
vol.ZFSStreamBytes` (read now: in pipe mode it is set by the splitter before
   `Close`, which is what ends the backend's read), `report()`. If `err != nil`:
   under `mu`, `delete(inflight, name)`; no `report()` (nothing goes up).
6. Return `err`.

`uint64` arithmetic: `read * raw / size` can overflow for a volume over
16 EiB. Not a concern; note it in a comment and compute in `float64` if the
reviewer prefers.

### `downloadProgress`

```go
type downloadProgress struct {
    Backend
    p *Progress
}

func WithDownloadProgress(b Backend, p *Progress) Backend
```

`Download(ctx, name)`:

1. `weight[name]` absent: return `b.Download(ctx, name)` unchanged.
2. Else `rc, err := b.Download(ctx, name)`; on error return it.
3. Under `mu`, `inflight[name] = 0` (a retry starts over).
4. Return `&countingReader{rc, p, name}` whose `Read` adds `n` to `wire` and
   to `inflight[name]` capped at `weight[name]`, then `report()`. Its `Close`
   closes `rc` and, under `mu`, if `inflight[name] == weight[name]` moves it to
   `done` (`done += weight; delete(inflight, name)`). A short read (a cut
   download) stays in `inflight` at its last value until the retry resets it.

No `Complete()` for a download: when every volume is in `done`, `current ==
total` and `report()` prints 100.

### Why not wrap `PreDownload`, `List`, `Delete`, `Close`

They are embedded and pass through. The wrappers do not implement `Sizer`,
`Archiver` or `LegacyVolumeFinder`, on purpose: the code that needs them never
sees a wrapper (tasks 4 and 5 hand the wrapper to the data path only). Put
that in the type comment.

### Unit tests (`backends/progress_test.go`)

Capture the log as `backends/s3_logger_test.go:37-40` does (a `NewLogBackend`
on a buffer, restored in `t.Cleanup`). Use an in-package fake `Backend` that
records calls, reads `vol` to EOF on `Upload`, and serves `[]byte` objects on
`Download`. Volumes come from `files.CreateSimpleVolume` with
`config.BackupTempdir = t.TempDir()`.

| Test                                         | Checks                                                                                                                                                                                                                                |
| -------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `TestProgressWholePercentsOnce`              | 4 volumes of 1 MiB, estimate 4 MiB, the fake reads in 4 KiB pieces. The Notice lines are `1%`..`99%` then `100%` after the final manifest, each once, in order. No other percents.                                                    |
| `TestProgressConcurrentReaders`              | 8 volumes uploaded from 8 goroutines through one wrapper. Lines strictly increasing, each once. Run under `make test-race PKG=./backends/` (or the race target that exists).                                                          |
| `TestProgressRetryNeverGoesBack`             | The fake fails the first `Upload` of volume 2 after half its bytes. The lines never repeat or drop. Volume 2's second attempt starts at 0 inside the tracker (inspect `inflight`).                                                    |
| `TestProgressEstimateTooLow`                 | Estimate 2 MiB, stream 4 MiB. The last line before the manifest is `99%`, then `100%` once at `Complete`.                                                                                                                             |
| `TestProgressPipeMode`                       | A pipe volume (`CreateSimpleVolume(ctx, true)`); the fake reads while a goroutine writes 1 MiB and sets `ZFSStreamBytes` before `Close`. No line during the read; one line after.                                                     |
| `TestProgressResumeSeeds`                    | `resumed` of 2 volumes with `ZFSStreamBytes` 1 MiB each, estimate 4 MiB. The first line printed is `50%` or more; no line below 50.                                                                                                   |
| `TestProgressNoTotal`                        | Estimate 0: no Notice line, one Debug line.                                                                                                                                                                                           |
| `TestProgressManifestNotCounted`             | A manifest volume (`IsManifest`, not final) uploads; no line. The final manifest prints `100%`.                                                                                                                                       |
| `TestDownloadProgressWholePercents`          | 4 objects of 1 MiB, read by `io.Copy` in 4 KiB pieces. `1%`..`100%`, each once. A `Download` of `manifests                                                                                                                            | x` (not in the manifest) is not counted and returns the fake's reader. |
| `TestDownloadProgressRetry`                  | The first reader of object 2 returns an error at half. `Close` leaves it in flight; a second `Download` resets it; lines never repeat.                                                                                                |
| `TestDownloadProgressOversizeObject`         | An object of `Size + 1` bytes: the count caps at `Size`; the line reaches `100%` only when every object is done, and never `101%`.                                                                                                    |
| `TestProgressRateAndETA`                     | A fake `now` that advances 1 s per 1 MiB read; 4 volumes of 1 MiB, estimate 4 MiB. The first line (under 2 s) has no `MiB/s`. A line at 2 s says `1.0 MiB/s, about 2s left`. The `100%` line says `1.0 MiB/s, took 4s` and no `left`. |
| `TestProgressRateExcludesResumed`            | `resumed` of 2 MiB, estimate 4 MiB, fake clock 1 s per MiB: the ETA at 75% is `1s left`, not from a rate that counts the seeded 2 MiB. The `took` on the 100% line is 2 s.                                                            |
| `TestProgressETAUsesRawBytes`                | Upload: volumes whose `Size` is half their `ZFSStreamBytes` (compressed 2:1). The rate is in stored bytes; the ETA is remaining raw ÷ raw rate, so it matches the fake clock, not half of it.                                         |
| `TestProgressNoETAAt99`                      | Estimate too low, sitting at 99: the line has a rate and no `left`.                                                                                                                                                                   |
| `TestProgressWrapperHidesOptionalInterfaces` | `WithUploadProgress(&FileBackend{}, p)` is not a `Sizer`. This pins the trap: the test name tells the reader why the wrapper must stay on the data path.                                                                              |

## Task 3: `checkManifestFits` returns the estimate

`backup/backup.go:1334`: change the signature to
`func checkManifestFits(ctx, j) (streamBytes uint64, err error)`. Return 0 and
nil when zfs cannot estimate (the Warning stays). Return `streamBytes, nil` on
success, `0, err` on the refusal. Two callers: `Backup` (`backup/backup.go:635`)
keeps the value in a local `streamEstimate`; `dryRunRefusals`
(`backup/backup.go:384`) discards it. `reportDryRun` calls `GetZFSSendDryRun`
itself (`backup/backup.go:347`) and does not change.

## Task 4: wire the upload wrapper into `Backup`

In the `Setup plumbing` loop (`backup/backup.go:730`):

```go
for _, d := range dests {
    b := d.backend
    if d.uri != deleteURI {
        label := fmt.Sprintf("Uploading %s@%s to %s", jobInfo.VolumeName, jobInfo.BaseSnapshot.Name, backends.RedactURI(d.uri))
        b = backends.WithUploadProgress(b, backends.NewUploadProgress(label, streamEstimate, jobInfo.Volumes))
    }
    out, waitgroup := retryUploadChainer(ctx, channels[len(channels)-1], b, jobInfo, d.uri)
    ...
}
```

- `deleteURI` is the `backends.DeleteBackendPrefix + "://"` value the loop
  above appends; name it once.
- `jobInfo.Volumes` at this point holds the resumed volumes (`backup/backup.go:1709`) or is
  empty. The tracker reads `ZFSStreamBytes` from them at construction, so the
  later appends by the finisher do not matter.
- `d.backend` itself stays raw in `dests`: `refuseExistingSet`, `tryResume`,
  `verifiedVolumes` (`Sizer`) and `uploadManifest` ran before, or run on the
  raw value. Check that nothing after the loop reads `dests[i].backend` for a
  data transfer; `copyManifest` is not on this path.
- `--dry-run` returns before the loop: no tracker is built.
- Multiple destinations: each tracker prints its own lines, labeled with its
  URI. The chain means the second lags the first; that is what the operator
  sees.

Verify the order of `ZFSStreamBytes` and `Close` in pipe mode once more
(`backup/backup.go:1483-1487`): `ZFSStreamBytes` is written, then `Close`
closes the pipe, which ends the backend's read, which ends `Upload`. The
wrapper reads `ZFSStreamBytes` after `Upload` returns. Keep it that way; a
comment at line 1484 should say the upload wrapper depends on the order.

## Task 5: wire the download wrapper into `receive`

`backup/restore.go`, after `PreDownload` (line 453) and before the workers
start (line 484):

```go
label := fmt.Sprintf("Downloading %s@%s from %s", manifest.VolumeName, manifest.BaseSnapshot.Name, backends.RedactURI(target))
dl := backends.WithDownloadProgress(backend, backends.NewDownloadProgress(label, manifest.Volumes))
```

Pass `dl` to `downloadSequenceVolume` (line 495) and nothing else.
`readCachedManifest`, `explainMissingManifest` and `PreDownload` keep the raw
`backend`. `AutoRestore` calls `receive` once per link, so each link gets its
own tracker and its own `100%`.

`manifest.VolumeName` and `BaseSnapshot.Name` are the manifest's, so the label
names the backup, not the `-d`/`-e` mapped target.

## Task 6: e2e tests (`e2e_progress_test.go`, root package)

Use `newE2EEnv(t)` and the helpers of `e2e_test.go`. Assert on Notice+ lines
only (napkin). A helper `progressPercents(logs, label) []int` extracts the
percents of lines that start with `label + ": "`.

| Test                             | Setup                                                                                                 | Checks                                                                                                                                                                                                                                                                                                                                              |
| -------------------------------- | ----------------------------------------------------------------------------------------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `TestE2EProgressSend`            | `FAKEZFS_STREAM_BYTES` = 4 MiB, `--volsize 1`, `--compressor ""`, `--maxParallelUploads 1`            | percents strictly increasing, last is 100, 100 appears once, the line text holds `an estimated 4.0 MiB`; the 100% line matches `, [0-9.]+ [KMG]?i?B/s, took [0-9hms]+$` (a fast send may finish under 2 s: then assert the line has no `left` and no `took`, and say so in the test); the same send at `--logLevel warning` prints no progress line |
| `TestE2EProgressResume`          | `FAKEZFS_FAIL_AFTER_BYTES` cuts the first send; `--resume` finishes it (`e2e_resume_test.go` pattern) | the first percent of the resumed run is at least the kept fraction; no percent below it; last is 100                                                                                                                                                                                                                                                |
| `TestE2EProgressReceive`         | receive the set of `TestE2EProgressSend` into a new working dir                                       | label starts with `Downloading`, text holds `of 4.0 MiB` with no `estimated`, strictly increasing, last 100 once                                                                                                                                                                                                                                    |
| `TestE2EProgressTwoDestinations` | two `file://` destinations                                                                            | each URI has its own lines, both reach 100 once; no line names the `delete://` link                                                                                                                                                                                                                                                                 |
| `TestE2EProgressPipe`            | `--maxFileBuffer 0` on send and on receive                                                            | strictly increasing, last 100; the send prints no partial-volume percents between volume boundaries (each line's bytes is a multiple of a volume's raw bytes)                                                                                                                                                                                       |
| `TestE2EProgressJSONOutput`      | `--jsonOutput`, capture `config.Stdout`                                                               | stdout is one JSON object and nothing else                                                                                                                                                                                                                                                                                                          |
| `TestE2EProgressNoEstimate`      | `FAKEZFS_DRYRUN_OUTPUT=garbage`                                                                       | the send works, the estimate Warning is there, and there is no progress line                                                                                                                                                                                                                                                                        |

With 4 MiB raw and `--volsize 1` the splitter cuts at 1 MiB − 50 KiB, so the
set is 5 volumes. The percents per line depend on the backend's read size and
must not be asserted exactly; only the order, the uniqueness and the end.

Run the whole root package (`make e2e`) and look for tests that assert an
exact Notice log: the new lines are Notice. `wantNotInLog` users in
`log_assert_test.go` are fine; a test that compares a whole log buffer is not,
and must get the lines added or the capture scoped.

## Task 7: docs

- `docs/context/backends.md`: a section "Progress wrapper". The two
  constructors, what is counted, the 99/100 rule, and the trap in one line:
  "`WithUploadProgress` and `WithDownloadProgress` hide `Sizer`, `Archiver` and
  `LegacyVolumeFinder`; hand them to the data path only (`retryUploadChainer`,
  `downloadSequenceVolume`)." One line on the rate (wire bytes, cumulative) and
  the time left (progress units, this run's bytes only).
- `docs/context/send.md`, Pipeline: one bullet after the forwarder: each real
  destination's chainer gets `WithUploadProgress`; the estimate comes from
  `checkManifestFits`.
- `docs/context/receive.md`, step 5: the workers download through
  `WithDownloadProgress`; the total is the manifest's volume sizes.
- `docs/context/data-model.md`, `VolumeInfo`: `SetReadHook` and who sets it.
- `README.md`: it has no section on output or log levels (only "Help
  Output"), so nothing to add. Skip.
- `.claude/napkin.md`: under "Code facts that are easy to get wrong": "A
  `backends.Backend` wrapper hides `Sizer`/`Archiver`/`LegacyVolumeFinder` from
  type assertions: wrap at the data path only." Under "Open plans": mark this
  spec executed.

## Task 8: break proofs

Each in a scratch copy (`rsync -a --exclude .git --exclude tmp ./
/tmp/zb-break/`, edit there, `make -C /tmp/zb-break ...`). A test that passes
the first time proves nothing (napkin).

| Break                                                       | Must fail                                                       |
| ----------------------------------------------------------- | --------------------------------------------------------------- |
| remove the `v.onRead(i)` call in `Read`                     | `TestE2EProgressSend` (no lines), `files` hook test             |
| `report()` prints on every call, not only when `pct > last` | `TestProgressWholePercentsOnce`                                 |
| drop the 99 cap                                             | `TestProgressEstimateTooLow`                                    |
| do not reset `inflight[name]` at `Upload` start             | `TestProgressRetryNeverGoesBack` (a repeated or higher line)    |
| compute the ETA from `wire` instead of the raw bytes        | `TestProgressETAUsesRawBytes`                                   |
| count the seeded bytes in the rate                          | `TestProgressRateExcludesResumed`                               |
| hand the wrapper to `tryResume` instead of the chainer      | `TestE2EResumeReSendsTruncatedVolume` (the `Sizer` path breaks) |
| count the manifest's bytes                                  | `TestProgressManifestNotCounted`                                |
| pass the raw backend to `downloadSequenceVolume`            | `TestE2EProgressReceive`                                        |

## Task 9: run

In this order, each its own call (napkin: never gate on a piped run):

```
make test-run PKG=./files/ RUN=TestVolumeReadHook
make test-run PKG=./backends/ RUN='TestProgress|TestDownloadProgress'
make test-one PKG=./backends/ RUN='TestProgress|TestDownloadProgress'
make e2e
make test-docker
make lint
make vet
git add <new files>; make fmt-check
```

`make test-race` (about 20 minutes) once, for `TestProgressConcurrentReaders`.
Optional, to see the lines on real zfs: `make integration-vm
RUN=TestIntegrationFile` (check the VM is free first: napkin, "Two sessions
share ONE compose project").

## Not in this plan

- A `--progress` or `--quiet` flag. `--logLevel` covers it.
- A windowed (recent) rate. The rate is the average of this run.
- Progress for `clean`, `list`, `plan`, dry runs, and manifest sync.
