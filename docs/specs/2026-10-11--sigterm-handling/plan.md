# SIGTERM and SIGINT handling: implementation plan

Date: 2026-10-11
Branch: `clean-dry-run` at `10fcf0c`
Source: finding F15 of the send-path review
(`docs/specs/2026-10-05--send-path-review/plan.md`, Task 11). This plan replaces
that task. Line numbers are at `10fcf0c`. Check them with `cs` before you edit.

## Intent

Today a SIGTERM or SIGINT ends the process at once. Go runs no deferred code on
the default action. A killed `send` leaves:

- its temp volume files under `<workingDirectory>/temp/zfsbackupNNN/`, up to
  `--maxFileBuffer` x `--volsize`, and nothing sweeps them;
- a stale lock file, recovered only by the pid-liveness check;
- S3 multipart uploads left open, billable until a lifecycle rule aborts them;
- a `zfs send` child that lives until its next write fails.

The fix is small because the plumbing exists. Every long operation already takes
a context, and nothing cancels it. This plan wires the two signals to that
context, gives the user one clear line, and maps the exit status.

What this plan does NOT do:

- It cannot cancel an S3 Glacier `RestoreObject` already issued. S3 has no API for
  that. A no-restore mode for `plan` and `send --dry-run` is a separate plan.
- It does nothing for SIGKILL or an OOM kill. The stale temp sweep and the
  `flock` lock (send-path Tasks 11 and 12) stay open and are listed under
  "Out of scope".

## Facts verified at `10fcf0c`

| Fact | Where |
| --- | --- |
| `main` creates a cancellable context and passes it to `cmd.Execute`. Nothing cancels it. | `main.go:30-33` |
| `Execute` runs `postRunCleanup` (removes the temp dir) when the command returns an error; `exitCode` returns 255 for an unknown error. | `cmd/root.go:78-104` |
| `Backup` releases the lock in a defer. | `backup/backup.go:463-467` |
| The pipeline is one errgroup; `group.Wait` returns the first error. A cancel is an error. No manifest is written after a failure. | `backup/backup.go:653` |
| `zfs send`, `zfs receive` and the external compressor run through `exec.CommandContext`, so a cancel kills them with SIGKILL. | `zfs/zfs.go:406,477`, `files/volumeinfo.go:408,664` |
| The send-side stop path kills and reaps the `zfs send` child, then aborts the volume. | `backup/backup.go:1300-1340` |
| Every retry loop uses `backoff.WithContext`. | `backup/backup.go:1607`, `backup/clean.go:591`, `backup/restore.go:483` |
| The S3 uploader keeps the default `LeavePartsOnError` (false). The SDK aborts the multipart upload when `Upload` fails, a cancel included. | `backends/aws_s3_backend.go:147` |
| The S3 Glacier wait loop selects on `ctx.Done()`. | `backends/aws_s3_backend.go`, `waitForRestores` |
| Cache writes are atomic (`WriteFileAtomic`), so a cancel mid-write leaves no truncated manifest. | `files/atomic.go` |
| The e2e harness runs commands in-process with `cmd.RootCmd.ExecuteContext(context.Background())`. A test can pass its own context. | `e2e_failure_test.go:104` |
| `--maxUploadSpeed` limits reads through `OpenVolume` for the `file://` backend (S3 bypasses it). A test can use it to make a send slow. | napkin, 2026-10-10 S3 audit |
| On a cancel today the user sees `Error waiting for zfs command to finish - signal: killed` and similar Error lines, and nothing that names the signal. | `backup/backup.go:1338`, `backup/restore.go:589-602` |

## Design

### 1. `cmd/signals.go`: `SignalContext`

```go
// SignalContext returns a context that is cancelled by the first SIGINT or SIGTERM.
// A second signal exits the process at once. stop releases the handler.
func SignalContext(parent context.Context) (ctx context.Context, stop func())
```

- `signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM)` on a channel with room for 2.
  Do not use `signal.NotifyContext`: it does not say which signal arrived, and
  the second-signal rule needs the channel.
- First signal: record it in a package var (`interruptedBy os.Signal`, under a
  mutex or an `atomic.Value`), log ONE line at Notice, cancel the context:

  `Received SIGTERM; stopping. Nothing more is uploaded. A send continues with --resume.`

  The last sentence only when the running command is `send`. Pass the command
  name in, or print the generic line for every command.
- Second signal: Notice `Received SIGTERM again; exiting now.` then
  `exitFunc(code)`. `exitFunc` is a package var set to `os.Exit`; tests replace it.
- `stop()`: `signal.Stop(ch)`, cancel.
- The Notice goes through `log.AppLogger`. `processFlags` configures the backend
  after `SignalContext` runs; a signal before that still prints through the
  default backend. Acceptable.

### 2. `main.go`

```go
ctx, stop := cmd.SignalContext(context.Background())
defer stop()
cmd.Execute(ctx)
```

### 3. Exit status

Decision: an interrupted run exits `128 + signal` (130 for SIGINT, 143 for
SIGTERM), the shell convention. systemd and cron wrappers already read it as
"killed by signal", not "failed".

- `exitCode` (`cmd/root.go:94`) checks `interruptedBy` FIRST. When a signal was
  received, return `128 + int(sig)` whatever the error is. The error after a
  cancel is often wrapped (`zfs send failed: signal: killed`), not
  `context.Canceled`, so do not match on the error.
- Alternative, if the user prefers: keep 255 and skip the doc changes below.
  Then monitoring cannot tell an interruption from a failure.

### 4. Quiet the cancel path

After a cancel, every stage reports its own death at Error level. Keep ONE
Notice (the signal line) and demote the rest. Rule: a line that fires only
because `ctx.Err() != nil` goes to Debug. Find them by running test T2 and
reading its log. Known candidates:

- `backup/backup.go:1338` "Error waiting for zfs command to finish"
- `backup/backup.go:1300-1325` "Could not kill ..." / "Could not release ..."
- the upload workers' retry-exhausted lines in `retryUploadChainer`
- `backup/restore.go:589-602` for `receive`

Do not touch lines that also fire without a cancel.

### 5. `receive` and `clean`

- `receive`: the cancel kills `zfs receive`. Real ZFS discards a partial,
  non-resumable receive (the tool does not use `zfs receive -s`). Nothing to
  clean. The Notice for `receive` says: `Run the receive again from the start.`
- `clean`: the cancel stops between deletes. Deletes are idempotent. Verify that
  the per-dataset locks in `Clean` are released on the error path (defers at
  `backup/clean.go:303-325`), not only on success.

### 6. Not done on purpose

- `Setpgid` on `zfs send` (the old Task 11 idea). `CommandContext` kills the
  direct child; `zfs send` and the compressor are direct children and have no
  children of their own. Not needed.
- A stale temp sweep at startup. Needed for SIGKILL and OOM, not for this plan.
  Keep it in the send-path plan.

## Tests, written first

### T1: `cmd/signals_test.go`

`TestSignalContextFirstSignalCancels`:

1. Replace `exitFunc` with one that records the code into a channel. Restore it
   with `t.Cleanup`.
2. `ctx, stop := SignalContext(context.Background())`; `defer stop()`.
3. `syscall.Kill(os.Getpid(), syscall.SIGTERM)`.
4. `ctx.Done()` closes within 2 s. `exitFunc` was NOT called.
5. Captured log holds the Notice with `SIGTERM`.

`TestSignalContextSecondSignalExits`: same, kill twice; `exitFunc` gets 143.

`TestExitCodeAfterSignal`: set `interruptedBy` to SIGINT, call `exitCode(sendCmd,
errors.New("x"))`, want 130. Reset it after.

Notes: the test process receives a real signal. It survives only because
`Notify` is registered first. Register before `Kill`, never the other way. Do
not run these tests with `t.Parallel()`.

### T2: `e2e_signal_test.go`, `TestE2ESendSIGTERMStopsCleanly`

Package `main`, uses `newE2EEnv`, `guarded`, `destObjects`, `env.lockFile`,
`env.checkRestores`.

1. Add `env.sendCtx(ctx, args...)`, the same as `env.send` with the context as a
   parameter. Have `env.send` call it with `context.Background()`.
2. `FAKEZFS_STREAM_BYTES` = 4 MiB, `--volsize 1`, `--maxUploadSpeed` about
   256 KiB/s, so the send takes about 16 s and uploads a volume every 4 s.
3. `ctx, stop := cmd.SignalContext(context.Background())`; `defer stop()`.
4. Run `env.sendCtx(ctx, ...)` in a goroutine under `guarded`.
5. Poll the destination until the first `.vol1` object exists (at most 10 s).
   Then `syscall.Kill(os.Getpid(), syscall.SIGTERM)`. Once only.
6. Assert:
   - the send returned a non-nil error;
   - the log has the Notice `Received SIGTERM; stopping`;
   - no object under `manifests|` at the destination;
   - `env.lockFile(dataset)` does not exist;
   - `<env.work>/temp` has no entries;
   - a second send with `--resume` (plain `env.send`) succeeds and logs
     `Resuming from volume`;
   - `env.checkRestores` restores the set and the bytes match.
7. Break-the-code check: run T2 against a tree without Task 1. Without
   `Notify`, the SIGTERM kills the test binary and `go test` reports
   `signal: terminated`. That is the expected failure. Record it in the commit.

If the timing is flaky under host load (napkin: e2e timings move 7 s to 43 s),
add a fake knob `FAKEZFS_STREAM_DELAY_MS` (sleep between writes in `zfs send`)
and drop `--maxUploadSpeed`. Prefer the knob only when needed; it also
exercises the "cancel kills the zfs child" path.

### T3, optional: `TestE2EReceiveSIGTERMStops`

Same shape, against `receive`. Assert the error, the Notice, and that the
fake's `FAKEZFS_RECEIVE_LOG` has no entry for the set. Low value; do it only if
Task 4 touches `restore.go`.

## Tasks and commit order

| # | Commit | Files | Test |
| --- | --- | --- | --- |
| 1 | `cmd: SignalContext cancels on SIGINT/SIGTERM; a second signal exits` | `cmd/signals.go`, `cmd/signals_test.go` | T1 |
| 2 | `main: run every command under SignalContext; exit 128+signal when interrupted` | `main.go`, `cmd/root.go` (`exitCode`) | T1 (`TestExitCodeAfterSignal`) |
| 3 | `send: a SIGTERM stops the pipeline cleanly; --resume continues` | `e2e_signal_test.go`, `e2e_test.go` (`sendCtx`), the Debug demotions of Design 4 | T2 |
| 4 | `docs: signals, exit codes, systemd and S3 lifecycle notes` | see below | `make help CMD=send` unchanged |
| 5 | napkin | `.claude/napkin.md` | |

Test and fix go in the same commit. Each commit message ends with the
attribution line from the session's system reminder.

### Task 4 docs

- `README.md`, after the `send` section: one paragraph. SIGINT or SIGTERM stops
  the run, releases the lock, removes temp files, uploads no manifest; exit
  130/143; `--resume` continues a send; a second signal exits at once.
- `docs/runbook-first-backup.md`, step 5: a systemd unit for the cron job
  should set `TimeoutStopSec=120` (the default 90 s is fine too; name the
  trade-off: an upload worker mid-request may take seconds to notice). Add the
  S3 lifecycle rule `AbortIncompleteMultipartUpload` after 1 day as the backstop
  for SIGKILL and OOM.
- `docs/context/cli.md` and `docs/architecture.md` (section 2, exit codes):
  add 130 and 143.
- `CHANGELOG.md`: `[ADD] signals: SIGINT or SIGTERM stops any command cleanly
  (lock released, temp files removed, no manifest written, S3 multipart upload
  aborted); exit 130 or 143; a second signal exits at once`.

## Verification

```
make test-run PKG=./cmd/ RUN=TestSignal
make test-run PKG=. RUN=TestE2ESendSIGTERM
make test-run PKG=.            # whole root package: in-process runs leak state
make test-one RUN=TestE2ESendSIGTERMStopsCleanly PKG=.
make test-docker
make fmt-check                 # after git add of the new files
make lint
```

## Risks

- **A signal in the test process.** `Kill` before `Notify` ends the test binary.
  Order is everything; `SignalContext` must return before the test sends.
- **Second signal in tests.** Never send twice in T2. `exitFunc` is replaced
  only in T1.
- **In-process state leaks.** `interruptedBy` is a package var. Reset it in
  `stop()` and in `ResetSendJobInfo`, or the next in-process run exits 143.
- **go-logging level.** Assert on the Notice only; root e2e logs hide Debug.
- **Host load.** T2 depends on timing. The 10 s poll and `guarded`'s 30 s
  guard give room; use the fake knob if it still flakes.

## Out of scope, tracked elsewhere

- Stale temp sweep at startup and the `flock` lock: send-path plan Tasks 11
  (remainder) and 12.
- A no-restore mode for `plan` and `send --dry-run` against Glacier manifests:
  new plan, not written yet.
- `zfs receive -s` (resumable receive).

## Execution notes (2026-10-11)

Executed as `a2c0636`, `7abff64`, `7a3aa70` and the docs commit after them.
Differences from the plan:

- `exitCode`: a command that returns no error exits 0 even after a signal (it
  finished before it saw the cancel). Every error after a signal is 128+signal,
  `ErrNoOp` included.
- `cmd.Run(ctx)` is new: `Execute` without `os.Exit`. The e2e `sendCtx` calls it,
  so a failed in-process send removes its temp dir, as `main` does. `Run` also
  calls `SetContext(ctx)` on every subcommand: cobra sets a subcommand's context
  only when it is nil, so an in-process run kept the first run's (canceled)
  context. Production runs once and never saw this.
- The command name for the hint comes from `processFlags` (`setRunningCmd`).
  A signal before `processFlags` prints the generic line.
- `interruptedBy` is reset in `stop()` and in `resetRootFlags` (so every
  `Reset*JobInfo`), not only `ResetSendJobInfo`.
- T2 waits for the cached partial manifest, not for `.vol1` at the destination:
  the file backend writes the volume under its final name from the first byte,
  so `.vol1` exists before any volume is done, and the resume then found
  nothing (`No previous manifest file exists`). T2 also sets
  `--maxFileBuffer 1`; with the default 5, a 4 MiB stream is fully read before
  the signal and the "cancel kills zfs send" path does not run.
- Design 4, what T2's debug log showed: only three lines echo the cancel, all in
  `sendStream`. Demoted: the stream read error and "Error waiting for zfs
  command to finish" (when the parent context is canceled), and "Could not kill
  zfs send command" when the error is `os.ErrProcessDone` (it fired on every
  death of `zfs send` by a signal, cancel or not). The upload workers logged
  nothing at Error. `restore.go` is unchanged, so T3 was not written; the same
  `os.ErrProcessDone` line exists there for `zfs receive`.
- `clean`: the per-dataset lock defers run on every return. Verified, no change.
- The fake knob `FAKEZFS_STREAM_DELAY_MS` was not needed (T2 took 15-20 s on the
  host, 3 runs, no flake).
