# Intent: fix the 20 findings of the plan -> send -> restore review

Fix every finding in `findings.md`. Each fix gets a regression test that fails on the old
code (the probes in `tests/*.go.txt` are the start). Fixes 7-10 and 14 depend on real zfs
behavior; they get a fake-zfs test now and a VM check listed in `plan.md`.

Decisions taken without the user (change them here if wrong):

- 1: explicit `-i` refuses when the destination holds a backup of a snapshot with the
  source's name but another creation time or GUID. No backup of the source: Notice.
- 3: a smart send whose newest backup's chain does not reach a full is `source-pruned`
  (a full), with a Warning that names the missing link.
- 4: backups and candidates dated more than a day after now are ignored by the planner,
  with a Warning.
- 10: `receive -F` refuses when zfs would destroy something (local snapshots newer than
  the incremental source, a target without snapshots, a `-R` incremental into a tree)
  unless the new flag `--allowDestroy` is also given.
- 16: `--volsize 0` is refused; with an `s3://` destination, `--volsize` above
  `10000 x --uploadChunkSize` is refused.
- 18: the lock file also records the boot id; a lock from another boot is stale.
- 20: the full window has a slack of 10 minutes (sanoid's second jitter).
