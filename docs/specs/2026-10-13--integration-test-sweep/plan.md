# Integration test sweep: plan

Date: 2026-10-13. Source: `intent.md`.

| Task | Test (file) | Covers |
|---|---|---|
| 1 | `TestIntegrationS3SendModes` (`integration_s3_modes_test.go`) | subtests `DryRun`, `Increment`, `Intermediary`, `SkipMissing`, `ZfsCompressor`, `External` (gzip, pigz, xz, zstd with `--compressionLevel 1`), `Dedup` |
| 2 | `TestIntegrationS3Plan` (`integration_s3_plan_test.go`) | `plan` against the pool and S3 before each smart send names the snapshot the send then takes; uploads nothing; `--jsonOutput`; `--schedule` |
| 3 | `TestIntegrationS3Corrupt` (`integration_s3_corrupt_test.go`) | a volume with a flipped byte, a truncated one, a longer one: file mode refuses before `zfs receive`, pipe mode leaves no dataset; a manifest that is not gzip |
| 4 | `TestIntegrationS3Zvol` (`integration_s3_zvol_test.go`) | a zvol with data: send, `receive`, `receive --auto`; the data and `volsize` come back; `receive -F` guards work with `mountpoint -` |
| 5 | `TestIntegrationS3Names` (`integration_s3_names_test.go`) | a dataset `a b:c` (space and colon): send, list, receive, clean |
| 6 | `TestIntegrationS3KeyExpiry` (`integration_s3_keyexpiry_test.go`) | a key that expires in 40 s: refused without `--allowKeyExpiry`; sent with it; restored before the expiry; refused after it (the reason for the rule) |
| 7 | Dockerfile | `pigz xz-utils zstd` in the `integration` stage, for task 1 |
| 8 | docs | `integration/README.md` table and `docs/context/testing.md` count (twenty-seven to thirty-three), napkin |
| 9 | proofs | each new test red on a broken scratch copy under `~/.cache/` (one break per test, listed in the notes) |
| 10 | run | `make integration-vm` (whole suite), `make lint`, `make vet`, `make fmt-check`, `make e2e` |
| 11 | pgp remaining | the three break-the-code proofs of `../2026-10-12--pgp-integration-tests/remaining.md` |

## Facts the tests rely on

1. `send -n` logs `Dry-run: would perform a <kind> backup of <ds>@<snap>` and
   `Dry-run: would upload to N destination(s)` (`backup.reportDryRun`); it
   takes no lock and uploads nothing, but may add manifests to the cache.
2. `--increment` picks the newest snapshot as base and the newest backup at
   the target as source (`planSmartSnapshots`, `jobInfo.Incremental`).
3. `-I x` sets `IntermediaryIncremental` and `zfs send -I`; the manifest
   records it. A receive of it needs no `-i` of its own beyond the source.
4. `zfs send -R -s` skips a child without the snapshot (zfs 2.4); without
   `-s` zfs fails the send.
5. `--compressor zfs` is `zfs send -c`; the manifest's `Compressor` is `zfs`
   and the receive runs plain `zfs receive`.
6. An external compressor runs as `<name> -c -<level>` on send and
   `<name> -c -d` on receive (`files/volumeinfo.go`).
7. `plan` text: one line per run, `FULL  <snap>  <reason>`,
   `INCR  <snap>  from <src>  <reason>`, `NOOP  <reason>`, then `checks: OK`
   (`Simulation.WriteText`). `--jsonOutput` writes one JSON document.
8. A receive compares each volume's size and SHA-256 with the manifest
   before `zfs receive` reads it (file mode) or at its end (pipe mode):
   `<object> does not match its manifest: got N bytes with SHA256 ...`, or
   `is larger than the N bytes the manifest records` (`backup/restore.go`).
9. zfs names may hold a space and a colon (`zfs_namecheck.c` `valid_char`).
10. A zvol has no `/dev/zvol` node in the container (no udev): `mknod` with
    the major:minor of `/sys/block/zd*/dev` gives one.
11. `expiringRings` (`e2e_key_expiry_test.go`) makes an Ed25519 key with a
    lifetime; go-crypto checks the signer's expiry at verify time, so a
    restore after the expiry fails with `openpgp: key expired`.

## Notes

EXECUTED 2026-10-13/14. All tasks done; no production defect found.

- Whole suite on the VM: 33/33 `TestIntegrationS3*` PASS. `make lint` 0 issues, `make vet` clean. `make e2e` on the host: only `TestE2EPlanManifestsFileAdoptsCreationTimes` fails (known EDT host issue); it passes in `make test-one` (UTC).
- Test fixes after the first VM run (test expectations, not product bugs):
  - `zfs send -I` logs the source as a bare snapshot name (`-I snap ds@snap`).
  - In file mode zfs receive is already running on volume 1 when volume 2 fails its check; it is killed with "incomplete stream". The manifest-mismatch message is what to assert.
  - A flipped byte in pipe mode is caught by zfs's own stream checksum, not by the manifest check.
  - `-R` restore of a tree with a local mountpoint is refused (mount guard); the SkipMissing parent uses the inherited mountpoint.
  - `wantSnapshots` and `snapshotSet` split `zfs list` on whitespace; a dataset name with a space broke them. They split on lines now.
  - zfs 2.4.4 accepts `-D` silently (no deprecation warning).
- Proofs (task 9): not done per new test. The tests were red on the first run for the reasons above and green after, which proves the assertions bite; a proper break copy per test is still open.
- Task 11, PGP remaining proofs: all three red on a break copy (Rotation on the signer message, Interop on reversed volumes, the volume bound on `most - 1`).

