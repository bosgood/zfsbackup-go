# backends/: the Backend interface

Read this for any change to a storage backend or `backends/backends.go`.

## Interface (`backends/backends.go`)

```go
Init(ctx, conf *BackendConfig, opts ...Option) error
Upload(ctx, vol *files.VolumeInfo) error
List(ctx, prefix string) ([]string, error)
Download(ctx, filename string) (io.ReadCloser, error)
PreDownload(ctx, objects []string) error   // e.g. Glacier restore
Delete(ctx, filename string) error
Close() error
```

`Archiver` is optional (`NotReadable(ctx, objects) ([]string, error)`). S3 implements it with HEAD requests only: the objects a GET cannot read before a `PreDownload`. `send --dry-run` and `plan` use it instead of `PreDownload`, which would start a Glacier restore (hours, and it costs money).

`Sizer` is optional. `file` and `ssh` implement it because they write under the final name and a killed upload leaves a truncated object. Object stores do not need it.

`GetBackendForURI` picks the backend from the URI scheme. Callers cannot inject a mock. Tests use `file://`.

`BackendConfig` carries `TargetURI` (canonical), `TypedURI` (as typed, for legacy key prefixes), `ManifestPrefix` (for `checkLegacyLayout`), upload chunk size, parallel upload limit, and backoff limits.

## Backends

| Scheme | File | Notes |
|---|---|---|
| `s3://` | `aws_s3_backend.go` | path-style; `AWS_S3_CUSTOM_ENDPOINT` for tests; a failed multipart upload is aborted by s3manager, a canceled one by `abortCanceledUpload` (new 30 s context: s3manager sends its abort with the canceled one, which never reaches S3); a bucket lifecycle rule `AbortIncompleteMultipartUpload` is the backstop for SIGKILL and a failed abort, because `List` (so `clean`) never sees parts; `PreDownload` restores Glacier objects for `restoreDays` (days to download every key at `AWS_S3_RESTORE_DOWNLOAD_RATE`, default 50 MiB/s, + 2, clamped 3..30; `AWS_S3_RESTORE_DAYS` overrides; a bad value fails `Init`, so every command, and `PreDownload`). It HEADs every key before the first restore |
| `gs://` | `gcs_backend.go` | |
| `azure://` | `azure_backend.go` | |
| `b2://` | `backblaze_b2_backend.go` | |
| `ssh://` | `ssh_backend.go` | sftp. Auth order: agent (`SSH_AUTH_SOCK`), then `SSH_KEY_FILE` or the default `~/.ssh/id_*` keys, then the password. The agent's keys and the key files are ONE `publickey` method (`PublicKeysCallback`): x/crypto tries each method name once, so as two methods the files were never offered after the server refused the agent's keys (`TestSSHBackendAgentThenKeyFile`). A failed upload removes the cut remote file with `sftpClient.Remove` (it used `os.Remove`, the local path, until 2026-10-14: `TestIntegrationSSHFull`). Real-sshd tests: `integration_ssh*_test.go` |
| `file://` | `file_backend.go` | used by all e2e and clean tests; a failed copy removes the cut file. Real-zfs tests: `integration_file*_test.go` (a dataset destination, `refquota`, `readonly=on`) |
| `delete://` | `delete_backend.go` | last link of the send chain; removes temp files (`e2e_delete_backend_test.go`) |

## Rules for a backend

- Upload must be retry-safe. The send pipeline retries with exponential backoff.
- `List` must return every object under the prefix, including manifests.
- Redact secrets in logs and errors (`redact3_test.go`).
- `parseObjectURI` (s3, gs, azure, b2) refuses a bucket name with `:` or `@` (`s3://user:password@bucket`): an SDK error quotes the bucket. The error does not echo the URI (`s3_review_test.go`).
- `Delete` errors are retried with backoff, then fail `clean`. `file://` returns the `os.Remove` error for a missing object.

## Tests

`backends/*_test.go` per backend. S3 without network: `httptest.Server` for ListObjectsV2 plus `AWS_S3_CUSTOM_ENDPOINT`, `AWS_REGION`, `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY`. Upload failure for `file://`: put a regular file at `<dest>/tank` so `MkdirAll` fails.
