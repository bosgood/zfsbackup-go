# Integration tests: harden the six, add the production shape

Source: the audit of `docs/specs/2026-10-11--integration-test-inc/` on 2026-10-11.

The six `TestIntegrationS3*` tests pass. They leave these holes:

- Every restore reads the manifest from the sender's local cache, never from S3.
  No test restores on a host with an empty cache.
- The encrypted test checks `.pgp` in the object NAME. Nothing proves a volume's bytes are encrypted.
- `compareDirs` is `diff -rq`: file contents only. A received snapshot keeps the source's `guid`; nobody checks it.
- No real-zfs test runs the production flags (`--fullSnapshotSuffix _monthly --incrementalSnapshotSuffix _monthly`),
  sees a window-elapsed full, a creation-time tie, a deep dataset name, `--snapshotPrefix`, `--manifestPrefix`,
  `--fullPath`, `--lastPath`, `--unmounted`, `--origin`, two destinations, or a receive without `-F`.

- No test touches ZFS native encryption: `send --raw`, a raw chain from a bookmark, a backup run
  with the key not loaded, a restore that needs `zfs load-key`, a plaintext send of an encrypted
  dataset, raw + GPG stacked, or a chain where one run forgot `--raw`.

The production pool (`testdata/zfs/snap-navidrome-2026-09-24.json`) has 2330 snapshots on
`backup3/enc/user/app/navidrome` (5 name parts), 3-way ties on the 1st of each month,
96 two-way ties at midnight, and a full that rolls every 180 days. `backup3/enc` is a ZFS
native encryption root (confirmed 2026-10-11).

Encryption coverage today: plaintext volumes (five tests) and GPG volumes (`EncryptedChain`).
Native encryption: none.

One production change rides along (decided 2026-10-11): **a chain keeps one stream kind.**
An incremental must carry the same `--raw` as the backup it continues from; a full starts a
new chain. Today nothing checks it: a cron run that forgets `--raw` puts a plaintext
incremental on a raw chain and the program says nothing.

Goal: close the holes above with real zfs and MinIO, through `make integration-vm`.
