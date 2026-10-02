# Intent: stop `clean`, re-sends, `--resume` and failed sends from destroying or corrupting backup sets

The 2026-09-29 adversarial review (S3, restore-chain focus) reproduced four
ways the tool destroys or silently corrupts data that a restore later depends
on. None of them need a bug in S3 or a hostile actor; they are reachable from
the runbook (`docs/runbook-first-backup.md`) with one typo or one interrupted
run.

1. **`clean` with the wrong URI deletes everything, manifests included.**
   Object-store backends (S3, GCS, Azure, B2) treat the URI path as a raw
   string prefix. Backups written to `s3://bucket/p/` and cleaned as
   `s3://bucket/p` list back as `/manifests|...` and `/tank|...`. The leading
   slash defeats the "is this a manifest" filter, so `clean` deletes the
   manifests and every volume. `clean s3://bucket` does the same to every
   destination in the bucket, and `clean s3://bucket/media` reaches into
   `media-photos/`. The runbook's "`clean` never deletes manifests" is false.
2. **Re-sending an existing set overwrites it in place.** Nothing checks the
   destination before uploading. `send --full` re-sends the newest `_monthly`
   even if it is already there as a full. Volume boundaries are not
   reproducible (three identical sends gave 3, 4 and 4 volumes), so a re-send
   that dies partway leaves the old manifest pointing at changed objects (2 of
   4 volumes failed SHA-256 at restore) and its partial manifest replaces this
   host's good cached copy, which is never re-downloaded. A later `clean` on
   that host then deletes the rest of the old set.
3. **`--resume` trusts the local cache.** It never checks that the volumes it
   skips still exist at the destination(s). After an interrupted send, a
   `clean` from another host deletes those volumes as orphans (no remote
   manifest exists yet); `--resume` then exits 0 with a manifest listing
   missing volumes. Resuming with an extra destination has the same effect:
   the new destination never receives the skipped volumes.
4. **Any failure inside the send pipeline hangs forever.** The final-manifest
   goroutine waits on a `sync.WaitGroup` with no context. An upload that
   exhausts its retries, or `zfs send` failing mid-stream, leaves a stuck
   process holding the dataset lock, so every later cron run fails to lock.

We want:

- Object-store URIs with and without a trailing slash to mean the same thing,
  and `clean` to refuse to delete anything it cannot prove belongs to a
  backup set under that exact destination.
- `send` to refuse to overwrite a backup set that already exists at any
  destination.
- `--resume` to verify, at every destination, that each volume it skips is
  still there, and to re-send from the first missing one.
- A failed send to exit non-zero, release the lock, and leave nothing that a
  later `clean` or `--resume` will misread.
- Regression tests for each, in the repo, using the existing fake-ZFS e2e
  harness and a fake S3 endpoint.
- The runbook corrected.

Success: the reproductions in `/tmp/zfsb-verify` (fake-S3 `clean`, upload
failure hang, non-reproducible volume split) become passing tests in the tree,
and the runbook's interim warnings ("don't run `clean`", "always include the
trailing slash", "don't `--full` an already-backed-up monthly") can be
removed.
