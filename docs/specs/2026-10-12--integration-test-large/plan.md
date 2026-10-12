# Integration test, a large send: implementation plan

Date: 2026-10-12
Branch: `clean-dry-run`
Source: `intent.md` in this directory.

## Goal

`TestIntegrationS3Large` in `integration_s3_large_test.go`, run by the default
`RUN=TestIntegrationS3` pattern.

| Step | Covers |
|---|---|
| send 1 GiB of random data, default flags | 6 volumes of about 200 MiB; each a multipart upload of 10 MiB parts |
| send the same in pipe mode (`--maxFileBuffer 0`) to a second bucket | multipart from a reader of unknown size |
| receive file-buffered from the first, in pipe mode from the second, each with an empty working directory | 200 MiB downloads, size + SHA-256 checks, the files compare equal, the guid matches |
| subtest `Replication`: `send -R` of a parent with two children, receive into a new parent | a replication stream restored as a tree |

## Facts that shape the plan

1. **Multipart proof is the ETag.** S3 and MinIO give a multipart object an
   ETag of `<hex>-<parts>`; a single PUT gives the content's MD5. With
   `--uploadChunkSize 10` (the default) a 200 MiB volume has 20 parts. The
   test HEADs every volume and checks the suffix and the part count.
2. **Volume sizes.** `sendStream` cuts when the compressed counter reaches
   `volsize - 50 KiB`; pgzip then flushes a few MiB of blocks. Random data
   does not compress. So every volume but the last is between 200 MiB - 50 KiB
   and some MiB more; 1 GiB gives 5 such volumes and one of about 24 MiB.
3. **The pool.** `integration/entrypoint.sh` makes the pool on a 2 GiB sparse
   file. The test needs the source and two restores (3 GiB) plus the
   replication tree. The file grows to 8 GiB; the VM disk is 30 GiB.
4. **A `-R` stream carries local properties.** A local `mountpoint` on the
   source would mount the restored tree over it (`TestIntegrationS3ReceiveSafety`,
   A4). The replication datasets inherit the pool's mountpoint instead, so
   the restored tree mounts under its own name.
5. **`zfs send -R` wants the snapshot on every child.** `zfs snapshot -r`.
   `snapshot` in `integration_s3_test.go` takes flags for that.

## Tasks

1. `integration/entrypoint.sh`: `truncate -s 8G`.
2. `snapshot(t, dataset, kind, flags...)`: pass flags to `zfs snapshot`.
3. `integration_s3_large_test.go`: the test, `wantMultipart`, `objectSizes`.
4. `integration/README.md`, `docs/context/testing.md`: the fourteenth test.
5. Run `make integration-vm RUN=TestIntegrationS3Large`; record the time.

## Notes

Executed 2026-10-12. First VM run green (`make integration-vm
RUN=TestIntegrationS3Large`, 15.5 s for the whole test):

| Step | Time | Result |
|---|---|---|
| send file-buffered, 1 GiB | 4.2 s | 6 volumes: five of 203 MiB, one of 11 MiB; ETags `-21` |
| send pipe mode, 1 GiB | 3.3 s | 6 volumes: 202/203 MiB and 13 MiB; ETags `-21` |
| receive file-buffered | 2.2 s | files equal, guid equal |
| receive pipe mode | 2.6 s | files equal, guid equal |
| `Replication` | 0.5 s | one 24 MiB volume of 3 parts; tree of 3 datasets restored |

Where the code differs from the plan: nothing. pgzip's flush adds about 3 MiB
to each full volume, within the 16 MiB slack.

Break proof: a scratch copy with `u.PartSize = 100 << 20` in the S3 backend
failed as planned: `vol1 (203 MiB) has ETag <md5>-3, want a multipart upload
of 21 parts`. Scratch copy deleted.

