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

`Sizer` is optional. `file` and `ssh` implement it because they write under the final name and a killed upload leaves a truncated object. Object stores do not need it.

`GetBackendForURI` picks the backend from the URI scheme. Callers cannot inject a mock. Tests use `file://`.

`BackendConfig` carries `TargetURI` (canonical), `TypedURI` (as typed, for legacy key prefixes), `ManifestPrefix` (for `checkLegacyLayout`), upload chunk size, parallel upload limit, and backoff limits.

## Backends

| Scheme | File | Notes |
|---|---|---|
| `s3://` | `aws_s3_backend.go` | path-style; `AWS_S3_CUSTOM_ENDPOINT` for tests; `PreDownload` restores Glacier objects |
| `gs://` | `gcs_backend.go` | |
| `azure://` | `azure_backend.go` | |
| `b2://` | `backblaze_b2_backend.go` | |
| `ssh://` | `ssh_backend.go` | |
| `file://` | `file_backend.go` | used by all e2e and clean tests |
| `delete://` | `delete_backend.go` | last link of the send chain; removes temp files |

## Rules for a backend

- Upload must be retry-safe. The send pipeline retries with exponential backoff.
- `List` must return every object under the prefix, including manifests.
- Redact secrets in logs and errors (`redact3_test.go`).
- `Delete` errors are retried with backoff, then fail `clean`. `file://` returns the `os.Remove` error for a missing object.

## Tests

`backends/*_test.go` per backend. S3 without network: `httptest.Server` for ListObjectsV2 plus `AWS_S3_CUSTOM_ENDPOINT`, `AWS_REGION`, `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY`. Upload failure for `file://`: put a regular file at `<dest>/tank` so `MkdirAll` fails.
