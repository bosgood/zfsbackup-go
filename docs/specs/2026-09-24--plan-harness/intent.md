# Intent: a testing environment that turns snapshot names into action plans

We are about to point `zfsbackup send --fullIfOlderThan 4320h
--fullSnapshotSuffix _monthly --incrementalSnapshotSuffix _monthly` at a real
sanoid-managed pool and a real offsite destination. A wrong decision is
expensive: a redundant full is a multi-hour upload we pay to store, and an
orphaned incremental chain is only discovered at restore time.

`selectSmartSnapshots` (backup/backup.go) is pure, and `TestSelectSmartSnapshots`
covers single decisions. The bugs we have actually hit are *sequence* bugs:
they only show up on the run after the `fullIfOlderThan` window elapses, or
after sanoid prunes the last incremental source. `send --dry-run` shows the
next step against the live pool but says nothing about the following year.

We want an environment that:

1. Takes in snapshot names, either pasted from
   `zfs list -H -p -t snapshot,bookmark -o name,creation,type -S creation <ds>`
   on the real pool or generated from a sanoid retention policy.
2. Takes in what is already at the destination (existing manifests) and the
   exact `send` flags.
3. Produces the action plan for the next run (FULL of X / INCREMENTAL X from Y
   / NO-OP, with a reason) and, optionally, the projected sequence of runs over
   a horizon, with automatic checks (chain integrity, no duplicate sends, every
   monthly backed up exactly once, full cadence within the window).
4. Feeds the same scenarios through the real `send` pipeline, using a fake
   `zfs` binary and a `file://` destination, so the manifests the real code
   writes and reads back produce the same plan as the simulator.

Success: before the first real run we can show, from the real pool's snapshot
listing and the real destination's contents, the exact sequence of backups the
tool will take for the next year, and every scenario in the repo passes in
`make test-docker`.
