# Runbook: the first monthly-only offsite backup

This walks a dataset from "never backed up" to a daily cron job that sends only
sanoid's `_monthly` snapshots: a full backup every six months and a monthly
incremental in between. `zfsbackup plan` shows every decision before anything
is uploaded. See [the plan harness spec](specs/2026-09-24--plan-harness/plan.md)
for how the planner and its checks work.

The production flags used throughout:

```bash
FLAGS="--fullIfOlderThan 4320h --fullSnapshotSuffix _monthly --incrementalSnapshotSuffix _monthly"
DS=pool/dataset                      # the dataset to back up
URI=s3://bucket/prefix               # the offsite destination
```

`4320h` is 180 days (Go durations have no `d` unit). Once it has elapsed since
the last full, the next full is taken of the first monthly newer than the last
backup, so fulls land every 181-184 days, always on the 1st.

Run every command with the working directory the cron job will use
(`--workingDirectory`, default `~/.zfsbackup`). `plan` and `send` then share
the local manifest cache.

## 1. Capture the pool and the retention policy

On the pool host:

```bash
zfs list -H -p -t snapshot,bookmark -o name,creation,type -S creation $DS > snaps.txt
grep -E '^\s*(hourly|daily|weekly|monthly|yearly)\s*=' /etc/sanoid/sanoid.conf
```

Note the counts of the template the dataset uses. Sanoid's defaults are
`hourly=48, daily=90, monthly=6`. On `ypool` on 2026-09-24 the pool showed
49 hourlies, 30 dailies and 6 monthlies, which matches `hourly=48,daily=30,monthly=6`:
sanoid keeps a snapshot until it is older than count x period, so count+1 are
often visible.

Sanoid names snapshots in the host's local time. If the host is not on UTC,
pass `location=<zone>` (e.g. `location=America/New_York`) in `--schedule` below.
Otherwise the simulated sanoid snapshots are taken, and the run times rendered,
at UTC wall-clock times.

## 2. Review a year of runs

```bash
zfsbackup plan $FLAGS --snapshots snaps.txt \
  --schedule "policy=hourly=48,daily=30,monthly=6,until=$(date -d '+400 days' +%F),every=24h,checks=coverage:_monthly,location=America/New_York" \
  $DS
```

Expect one `FULL ... no-previous-full` on the first run, then an `INCR` on each
1st of the month (`from` the previous monthly), `NOOP ... nothing-newer` on
every other day, and a `FULL ... window-elapsed` on the first 1st after each
180-day window. The output must end in `checks: OK`; `plan` exits 2 otherwise.
The checks:

| Check | Fails when |
|---|---|
| `no-errors` | a run fails (for example no snapshot matches a suffix), as `send` would |
| `chain-links` | an incremental's source is not backed up at the destination |
| `source-present` | an incremental is sent from a snapshot no longer on the pool |
| `no-duplicate-send` | a snapshot already backed up at the destination is sent again |
| `no-orphan-full` | the backup after a full does not chain from it |
| `full-cadence` | fulls are not one window apart (± one month and one run), or a full is overdue |
| `restore-depth` | restoring the newest backup takes more incrementals than fit in a window |
| `coverage:_monthly` | a monthly is backed up zero times (from the first run on) or more than once |

`source-pruned` in the output means sanoid pruned the last backup's snapshot
before the next run could send from it. The retention is too short for the run
cadence, or the host was down too long. A `source-pruned` no-op waits for the
next monthly: `send` logs a warning and exits 0.

## 3. Plan the first run against the real destination

```bash
zfsbackup plan $FLAGS $DS $URI
```

This lists the live snapshots and reads the manifests already at the
destination (read-only). If the manifests are encrypted or signed, pass the
same PGP flags as `send`. Expect:

```
next  FULL  autosnap_<newest monthly>_monthly  no-previous-full
checks: OK
```

If the destination holds manifests from earlier attempts, the plan chains from
them (`INCR ... from ...`, or `NOOP`). Decide whether to keep them. To start over,
delete them at the destination **and** their cached copies under
`<workingDirectory>/cache/<md5 of the URI>/`. `zfsbackup clean` never deletes
manifests (only `clean --force` removes whole broken sets). The planner ignores
a cached manifest that is gone from the destination, but `clean` still treats
it as live and keeps the volumes it lists.

## 4. Dry-run the send

```bash
zfsbackup send -n $FLAGS $DS $URI
```

It logs `Dry-run: would perform a full backup of $DS@autosnap_..._monthly`, the
`zfs send` command line and a size estimate (`zfs send -n -P`). The snapshot
must match step 3.

## 5. Run it, then schedule it

```bash
zfsbackup send $FLAGS $DS $URI
```

A full backup can take hours. If it is interrupted, rerun it with `--resume`
and the same flags. Then schedule the same command daily. The exit status is 0
both when it uploads a backup and when there is nothing new (`Nothing new to
back up.`, the normal case on most days), so any other status is a real
failure.

The next day, `zfsbackup send -n $FLAGS $DS $URI` must report `Nothing new to
back up.` and exit 0. On the 1st of the next month it must plan an incremental
from the monthly sent in step 5. Around each six-month mark, `send` logs that
the last full backup is older than the window. The next full waits for the
first monthly newer than the last backup.

## 6. Keep the real snapshot names under test

Commit the capture from step 1 as a scenario, so every change to the planner
is checked against the real pool's names from now on:

```bash
mkdir backup/testdata/scenarios/prod-<dataset>
cp snaps.txt backup/testdata/scenarios/prod-<dataset>/snapshots.txt
printf '%s\n' $FLAGS > backup/testdata/scenarios/prod-<dataset>/flags
printf '%s\n' "policy=hourly=48,daily=30,monthly=6" "until=<first run + 1 year>" \
  "every=24h" "checks=coverage:_monthly" "location=America/New_York" \
  > backup/testdata/scenarios/prod-<dataset>/schedule
make scenarios-update   # writes expected.txt; review it like step 2
make scenarios
```

The scenario's `expected.txt` must show fulls about every six months, every
`_monthly` exactly once, and `checks: OK`.
