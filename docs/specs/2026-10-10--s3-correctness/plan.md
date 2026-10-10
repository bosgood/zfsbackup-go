# S3 backend correctness fixes: implementation plan

Date: 2026-10-10
Branch: `clean-dry-run`
Source: `report.md` in this directory. Read it first; it has the line numbers,
the failure scenarios, and the SDK facts each task relies on.

## Goal

Fix the two warnings (W1, W2) and the cheap notes (N1, N2, N3, N6, N7) from
the report. N4 and N5 are optional and come last. Every behavior change gets a
regression test that fails on the old code. Do the tasks in order; each is one
commit.

## Rules for this repo

Read `.claude/napkin.md` first. The rules that matter here:

- Build and test through the Makefile. Do not run `go test` on the host.
  - One test: `make test-one RUN=TestName PKG=./backends/`
  - Full suite: `make test-docker`
  - Format check: `make fmt-check`. Lint: `make lint`.
- A new regression test must FAIL on the old code. Prove it: stash the fix
  (`git stash push backends/aws_s3_backend.go`), run the test, see it fail,
  `git stash pop`, run it again, see it pass. A test that passes before and
  after proves nothing.
- Find code with `semble search` (concepts) and `cs` (symbols), as described in
  `.claude/skills/codesearch/SKILL.md`. Do not use grep to locate code.
- S3 tests use the mock client and uploader in
  `backends/aws_s3_backend_test.go` (`mockS3Client`, `mockS3Uploader`,
  `mockS3UploaderRecorder`) and the `restoredS3Client` pattern in
  `backends/s3_restore_expiry3_test.go`. Add new tests in NEW files named
  `backends/s3_<topic>_test.go`; do not grow the shared mock's behavior unless a
  task says so.
- PreDownload tests set `AWS_S3_RESTORE_POLL_INTERVAL=10ms` with `t.Setenv`.
- `config.BackupUploadBucket` is a package global. A test that sets it must
  restore it with `t.Cleanup`.
- Log level: user-facing lines use `Noticef`; `Debugf` is hidden.
- Add one `CHANGELOG.md` line per user-visible change, at the top, in the
  existing `* [FIX] area: ...` style.
- Another session may edit this repo. Run `git status` before you stage, and
  stage files by name.

## Task 1 (W1): make S3 uploads obey `--maxUploadSpeed`

Files: `backends/aws_s3_backend.go` `Upload` (lines 240-257).

Change: pass the volume to s3manager through the existing `reader{vol}`
adapter in BOTH branches, not only the pipe branch. The adapter exposes `Read`
only, so s3manager cannot take its `ReadAt` path and every byte goes through
`VolumeInfo.Read`, which is the rate-limited reader. Keep the MD5 branch
logic as it is: it keys on `vol.IsUsingPipe()` and `vol.Size`, not on the
reader type.

Do NOT fix this by rate-limiting `VolumeInfo.ReadAt` instead. The SDK hashes
each part before sending it (`computeBodyHashes`, see report N5), and the
custom handler hashes it again. With a limited `ReadAt` every part would pass
the limiter three times and the real upload speed would be a third of the
flag. With the `reader` adapter, s3manager copies each part into a pooled
buffer once (limited) and hashes the buffer (not limited).

Side effects to document in the commit message and in the `Upload` comment:

- s3manager no longer knows the total size, so it cannot raise `PartSize` for
  a volume of more than 10,000 parts. At the minimum `--uploadChunkSize` of
  5 MiB that is a 50 GiB volume; `--volsize` defaults to 200 MiB. Not a
  problem. Say so in the comment.
- s3manager buffers `Concurrency + 1` parts in memory
  (`MaxParallelUploads + 1` times `UploadChunkSize`; 50 MiB at the defaults).
  Pipe mode already paid this.

Test: `backends/s3_ratelimit_test.go`, `TestS3UploadBodyIsNotSeekable`.
Use a recording uploader like `mockS3UploaderRecorder` that keeps
`in.Body`. Upload a file-buffered volume from `prepareTestVols`. Assert that
`in.Body` does not implement `io.ReaderAt` and does not implement `io.Seeker`,
with a comment that says why: s3manager reads a `ReaderAt` body with
`SectionReader`, which skips the rate limiter. On the old code the body is
the `*VolumeInfo`, which has both, so the test fails.

Second test in the same file, `TestS3UploadRateLimited`, skipped under
`testing.Short()`: set `config.BackupUploadBucket` to
`ratelimit.NewBucketWithRate(1<<20, 1<<20)` (1 MiB/s), restore it in
`t.Cleanup`, upload a 4 MiB file-buffered volume through a recording uploader
that drains `in.Body` with `io.Copy(io.Discard, in.Body)`, and assert the
upload took at least 2 seconds. Keep the bound loose; CI is slow.

CHANGELOG: `* [FIX] s3: uploads obey --maxUploadSpeed; they ignored it unless
--maxFileBuffer was 0`.

## Task 2 (W2): re-check already-restored copies after the wait

Files: `backends/aws_s3_backend.go` `PreDownload` (lines 292-394).

Change: restructure `PreDownload` into two helpers and a bounded loop.

1. `classifyRestores(ctx, keys, tier) (toRestore, readable []string, err)`:
   the current first pass (lines 300-357). Every key that takes the
   "already restored and not expiring soon" branch (line 316) goes into
   `readable`. Keys that take the `extend` branch also go into `readable`
   (their copy stays readable while S3 extends it).
2. `waitForRestores(ctx, toRestore)`: the current wait loop (lines 361-392),
   including the `Infof` line.
3. `PreDownload` becomes:

   ```
   const maxRounds = 3
   for round := 0; ; round++ {
       toRestore, readable, err := classifyRestores(ctx, keys, tier)
       if err != nil { return err }
       if len(toRestore) == 0 { return nil }
       if round == maxRounds { return fmt.Errorf("s3 backend: restored copies kept expiring during the wait; giving up after %d rounds", maxRounds) }
       if err := waitForRestores(ctx, toRestore); err != nil { return err }
       keys = readable
   }
   ```

   After a wait, the keys that were readable before it are classified again.
   One that now expires within the margin is extended; one whose copy expired
   during the wait is restored and waited for; one that is still fine is
   skipped. The loop ends when a round finds nothing to wait for. Round 2 and
   later only look at keys that were readable, so they are usually empty.

Also raise the comment on `restoreExpiryMargin` (line 427-430): say that the
margin covers the downloads after PreDownload, and that the re-check loop,
not the margin, covers the wait itself. Do not raise the margin to 48 hours;
that would re-restore copies needlessly on every run.

Test: `backends/s3_restore_recheck_test.go`,
`TestS3PreDownloadExtendsCopyThatExpiresDuringWait`. Build a client on
`mockS3Client` like `restoredS3Client`. Two keys:

- `vol1`: GLACIER. Every HEAD returns `ongoing-request="false"` with an
  `expiry-date`. Before the first HEAD of `vol2` the date is 30 hours out.
  After `vol2` reports restored, the date is 1 hour out. (Use a counter on
  the client: the first N HEADs of `vol1` return the far date, later ones the
  near one; or flip a flag when `vol2`'s restore is "done".)
- `vol2`: DEEP_ARCHIVE, no restore header. The first HEAD after its
  RestoreObject returns `ongoing-request="true"`, the next one
  `ongoing-request="false"`.

Record every RestoreObject call. Assert: `PreDownload` returns nil, `vol2`
got exactly one RestoreObject, and `vol1` got exactly one RestoreObject,
issued after `vol2`'s. On the old code `vol1` gets none, so the test fails.

Second test, `TestS3PreDownloadRestoresCopyThatExpiredDuringWait`: same
shape, but after the wait `vol1`'s HEAD returns no restore header at all
(the copy is gone). Assert `vol1` gets a RestoreObject with `Days` set and
that `PreDownload` waits for it (its HEAD after the restore must show
`ongoing-request="false"` before `PreDownload` returns).

Third test, `TestS3PreDownloadGivesUpAfterThreeRounds`: `vol1`'s copy is
gone on every HEAD that follows a wait, and every RestoreObject "completes"
on the next HEAD but the copy is gone again the HEAD after. Assert an error
that mentions rounds, and that `PreDownload` returns within a bounded number
of RestoreObject calls.

Keep the existing tests green: `TestS3PreDownload`,
`TestS3PreDownloadRestoreError`, `TestS3PreDownloadExtendsExpiringRestore`,
`TestS3PreDownloadNilRestoreHeader`, and the root
`TestE2ES3ReceiveRestoresDeepArchive` and `TestE2ES3CompletePartialThawsFirst`
(find them with `cs --only-declarations`).

CHANGELOG: `* [FIX] s3: receive re-checks already-restored Glacier copies
after waiting for the others, and extends or restores any that expired
meanwhile; a DEEP_ARCHIVE Bulk restore can take longer than the 24-hour
margin`.

Also update `docs/runbook-first-backup.md` near line 196 if it describes the
margin; one sentence.

## Task 3 (N1, N2): nil-safe SDK fields

Files: `backends/aws_s3_backend.go` `List` (lines 507-521) and `PreDownload`
(lines 317, 325, 328).

Change:

- `List`: `aws.StringValue(obj.Key)`, `aws.BoolValue(resp.IsTruncated)`. When
  `IsTruncated` is true and `NextContinuationToken` is nil, return
  `fmt.Errorf("s3 backend: the listing of %q is truncated but has no continuation token", a.prefix+prefix)`
  instead of fetching again.
- `PreDownload`: `aws.StringValue(resp.StorageClass)` in the three `Debugf`
  calls. Store it once in a local.

Test: `backends/s3_nilsafe_test.go`.

- `TestS3ListToleratesMissingIsTruncated`: a client whose `ListObjectsV2`
  returns `Contents` and a nil `IsTruncated`. Assert the keys come back and
  nothing panics. Fails on the old code with a nil-pointer panic (the test
  binary crashes; that counts).
- `TestS3ListTruncatedWithoutToken`: `IsTruncated` true, token nil. Assert an
  error, and that the client was called exactly once.
- `TestS3PreDownloadArchiveStatusWithoutStorageClass`: HEAD returns
  `ArchiveStatus: ARCHIVE_ACCESS` and nil `StorageClass`; RestoreObject
  succeeds; the next HEAD returns nil `ArchiveStatus`. Assert nil error.

No CHANGELOG line; AWS never sends these shapes.

## Task 4 (N3): a missing object is a permanent download error

Files: `backends/aws_s3_backend.go` `Download` (lines 469-478).

Change: when `GetObjectWithContext` fails with an `awserr.Error` whose code is
`s3.ErrCodeNoSuchKey`, return an error that satisfies
`errors.Is(err, fs.ErrNotExist)` and still prints the SDK message. Pattern:

```go
type notFoundError struct{ err error }
func (e *notFoundError) Error() string          { return e.err.Error() }
func (e *notFoundError) Unwrap() error          { return e.err }
func (e *notFoundError) Is(target error) bool   { return target == fs.ErrNotExist }
```

Check with `cs --only-usages ErrNotExist` whether `backends/` already has such
a type (the ssh backend compares against `os.ErrNotExist`); reuse it if so.

Test: `backends/s3_notfound_test.go`, `TestS3DownloadMissingKeyIsNotExist`: a
client whose `GetObject` returns
`awserr.New(s3.ErrCodeNoSuchKey, "The specified key does not exist.", nil)`.
Assert `errors.Is(err, fs.ErrNotExist)` and that `err.Error()` contains
`NoSuchKey`. Fails on the old code.

CHANGELOG: `* [FIX] s3: receive stops at once when a volume is gone from the
bucket, instead of retrying the download for --maxRetryTime`.

## Task 5 (N6, N7): debug logger

Files: `backends/aws_s3_backend.go` lines 72-74 and 124-127; `README.md`
line 42.

Change:

- `logger.Log`: `log.AppLogger.Debugf("s3 backend: %s", fmt.Sprint(args...))`.
- README line 42: add "The dump includes the request's `Authorization`
  header and any `X-Amz-Security-Token`; do not share that log."

Test: `TestS3DebugLoggerFormats` in `backends/s3_logger_test.go`: capture the
logger output (see the napkin for the leveled-backend capture pattern, under
"Patterns That Work"), call `logger{}.Log("a", 1)`, assert the line contains
`s3 backend: a 1` and not `EXTRA`. Fails on the old code.

CHANGELOG: `* [FIX] s3: AWS_S3_ENABLE_DEBUG lines no longer carry a
"%!(EXTRA ...)" suffix; README warns that the dump includes credentials`.

## Task 6 (N5, optional): drop the redundant hash handler

Files: `backends/aws_s3_backend.go` lines 197-216, 253, 259.

Change: delete `withComputeMD5HashHandler` and its `append` at line 253. Keep
`withContentMD5Header`: it sends the MD5 that `VolumeInfo` computed while
writing, which the SDK cannot know. Rewrite the comment at line 259: the SDK
sets `Content-MD5` and `X-Amz-Content-Sha256` on every PutObject and
UploadPart itself (`vendor/github.com/aws/aws-sdk-go/service/s3/body_hash.go`).

Only do this if `TestS3Backend` (the end-to-end test against a MinIO target,
`backends/aws_s3_backend_test.go:634`) can run; it needs the env it checks
for in `getOptions`. If it cannot run here, skip this task and say so.

No test to add; `TestS3Upload` keeps covering the `hex.DecodeString` branch.

## Task 7 (N4, optional): Close under the mutex

Files: `backends/aws_s3_backend.go` lines 481-485.

Change: take `a.mutex` in `Close`. Nothing else. Do not add a "closed" error
path; no caller needs it.

## Done when

- `make test-docker`, `make fmt-check`, and `make lint` pass.
- Each task is one commit whose message names the report item (W1, W2, ...).
- Each regression test was shown to fail on the old code (say so in the
  commit message with the test name).
- `CHANGELOG.md` has the lines above.
- Add a line to `.claude/napkin.md` under "Resolved Defects" for W1 and W2.
