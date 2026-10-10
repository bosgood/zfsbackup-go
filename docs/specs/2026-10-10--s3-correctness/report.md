# S3 backend correctness audit

Date: 2026-10-10
Branch: `clean-dry-run`
Scope: `backends/aws_s3_backend.go`, read against `files/volumeinfo.go`,
`backup/restore.go`, `backup/sync.go`, and the vendored `aws-sdk-go`.
Method: adversarial review (Saboteur, New Hire, Security Auditor personas),
correctness only. Read-only; no code changed.
Verdict: CONCERNS. Two warnings, no criticals.

Earlier audits (`docs/specs/2026-10-10--high-risk-review/`,
`docs/specs/2026-10-08--adversarial-review/`) covered the restore state machine
and the key-prefix logic. Those items are not repeated here.

Line numbers refer to `backends/aws_s3_backend.go` at commit `746de24` unless
another file is named.

## Warnings

### W1. `--maxUploadSpeed` has no effect on S3 in file-buffered mode

Where: `Upload`, line 243 (`r = vol`).

What happens: `Upload` hands the `*files.VolumeInfo` itself to s3manager. A
`*VolumeInfo` implements `ReadAt` and `Seek`, so s3manager takes its
`readerAtSeeker` path (vendor `service/s3/s3manager/upload.go:469`) and reads
every part through an `io.SectionReader` (line 487 there). `SectionReader`
calls `ReadAt`, which goes straight to the `*os.File` at
`files/volumeinfo.go:277`. The rate limiter is wrapped around `v.r` only
(`files/volumeinfo.go:302` for files, `:742` for pipes), and only `Read` goes
through `v.r`.

Who is affected: every `send` to an `s3://` destination with
`--maxFileBuffer` greater than 0 (the default) and `--maxUploadSpeed` set.

Who is not: pipe mode (`--maxFileBuffer 0`), because `Upload` wraps the volume
in `reader{vol}` (line 256), which exposes `Read` only. GCS, Azure, B2 and SSH,
because they copy with `io.Copy(w, vol)`, which uses `Read`.

Failure: a user sets `--maxUploadSpeed 2000` to protect a shared uplink. S3
saturates the link. Nothing in the output says the limit was ignored.

### W2. A restored copy can expire inside PreDownload's own wait

Where: `PreDownload`, line 316 (the "already restored" skip) and lines
368 to 392 (the wait loop). `restoreExpiryMargin` is 24 hours (line 430).

What happens: the first pass skips a restored copy whose `expiry-date` is more
than 24 hours away. It then waits for every other key that needs a restore.
AWS documents DEEP_ARCHIVE Bulk restores as taking up to 48 hours. GLACIER
Bulk takes up to 12 hours, and the downloads after the wait add more. The
skipped copies are never looked at again.

Failure: a set has vol1 restored with 30 hours left and vol2 in DEEP_ARCHIVE
with no restore. PreDownload leaves vol1 alone, restores vol2 with the Bulk
tier, and waits up to 48 hours. It then returns nil. `Download` of vol1 fails
with `InvalidObjectState`. `processSequence` at `backup/restore.go:484` marks
only `fs.ErrNotExist` as permanent, so it retries that GET for
`--maxRetryTime` (12 hours by default) and then the receive fails. A re-run
restores vol1 and waits again.

## Notes

### N1. `List` dereferences SDK pointers with no nil check

Where: line 508 (`*obj.Key`), line 513 (`*resp.IsTruncated`), line 518
(`NextContinuationToken` passed on blindly).

The SDK's own paginator uses `aws.BoolValue`. AWS always sends both fields. An
S3-compatible server behind `AWS_S3_CUSTOM_ENDPOINT` that omits `IsTruncated`
panics `list`, `clean` and `send`. A server that sends `IsTruncated=true` with
no continuation token makes the loop fetch page one forever and grow the
result slice without bound.

### N2. `PreDownload` dereferences `*resp.StorageClass`

Where: lines 317, 325, 328, inside `Debugf` calls.

`needsRestore` (line 403) returns true on `ArchiveStatus` alone. A HEAD
response with `x-amz-archive-status` but no `x-amz-storage-class` panics. AWS
sends the class for every INTELLIGENT_TIERING object, so this needs a non-AWS
endpoint.

### N3. `Download` returns the raw SDK error for a missing key

Where: lines 469 to 478.

`processSequence` (`backup/restore.go:484`) marks only `fs.ErrNotExist` as
permanent. `PreDownload` HEADs every key first, so a key that is already
missing fails fast. A key that vanishes between that HEAD and the GET, for
example by a `clean` on another host, is retried for 12 hours instead of
failing at once. Only the file backend returns an `fs.ErrNotExist` today.

### N4. `Close` clears `client` and `uploader` without the mutex

Where: lines 481 to 485.

Every caller defers `Close` after the work ends (`backup/backup.go:418`,
`backup/restore.go:65`, `backup/clean.go:242`, `backup/list.go:55`), so
nothing hits this today. A later caller that uses the backend after `Close`
gets a nil-pointer panic rather than an error.

### N5. The two MD5 handlers duplicate the SDK

Where: `withContentMD5Header` (line 175), `withComputeMD5HashHandler`
(line 197), and the comment at line 259.

The vendored SDK installs `computeBodyHashes` on every PutObject and
UploadPart request (`service/s3/customizations.go:54`,
`service/s3/body_hash.go:30`). It sets `Content-MD5` and
`X-Amz-Content-Sha256` when the body is seekable, and keeps a header that is
already set. So each part is read three times: SDK hash, handler hash, send.
Not a correctness bug. The comment that says the handler "forces" the hash is
wrong. `withContentMD5Header` still has a use: it sends the MD5 that
`VolumeInfo` computed while writing, so S3 rejects a volume whose bytes
changed on disk between write and upload.

### N6. `logger.Log` has no format verbs

Where: line 73.

With `AWS_S3_ENABLE_DEBUG=true`, every SDK log line renders as
`s3 backend:%!(EXTRA ...)`.

### N7. Security: `AWS_S3_ENABLE_DEBUG` logs credentials

Where: lines 124 to 127.

The option enables `LogDebugWithRequestErrors`. The SDK then dumps the whole
request with `httputil.DumpRequestOut` (vendor `aws/client/logger.go:63`),
which includes the `Authorization` header and any `X-Amz-Security-Token`.
Opt-in, debug level only. README line 42 documents the option without this
warning.

## Checked and found correct

- The request limiter (line 185) cannot leak a slot: the Send handler list has
  no `AfterEachFn`, so the release handler runs even when `SendHandler` sets
  `r.Error` (vendor `aws/defaults/defaults.go:80-81`,
  `aws/request/handlers.go:265`).
- Retries reopen the volume (`volUploadWrapper`, `backup/backup.go:1631`), so
  a second attempt starts at offset 0.
- `UploadChunkSize` reaches the backend in bytes (`backup/sync.go:57`), and
  `files/jobinfo.go:251` keeps it between 5 and 100 MiB, so the single-part
  branch at line 244 is sound: a volume under `MinUploadPartSize` is always
  under `PartSize`.
- A zero `MaxParallelUploads` or `UploadChunkSize` (receive, clean, list) gets
  the s3manager defaults (`upload.go:409-413`).
- `firstKey` (line 164) short-circuits on the error before touching `resp`.
