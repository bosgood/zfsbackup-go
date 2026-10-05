# Runbook: the first monthly-only offsite backup

This walks a dataset from "never backed up" to a cron job that sends only
sanoid's `_monthly` snapshots: a full backup every six months and a monthly
incremental in between. The job can run daily or weekly; only the runs that
see a new monthly upload anything. `zfsbackup plan` shows every decision before anything
is uploaded. See [the plan harness spec](specs/2026-09-24--plan-harness/plan.md)
for how the planner and its checks work.

The production flags used throughout:

```bash
FLAGS="--fullIfOlderThan 4320h --fullSnapshotSuffix _monthly --incrementalSnapshotSuffix _monthly"
DS=pool/dataset                      # the dataset to back up
URI=s3://bucket/prefix/              # the offsite destination
```

For S3, GCS, Azure and B2 the path after the bucket is a directory:
`s3://bucket/prefix` and `s3://bucket/prefix/` are the same destination, and
neither reaches into `s3://bucket/prefix-other/`. Versions before 2026-10
concatenated the path without a `/` (objects named `prefixmanifests|...` and
`prefixpool/dataset|...`). Against such a destination every command now fails
with `found prefixmanifests|..., a backup written by an older version`; move
every object whose name starts with `prefix` to `prefix/` and rerun.

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
at UTC wall-clock times. For a capture without creation times, such as
`zfs list` output converted to JSON (see `testdata/zfs`), the times also come
from the snapshot names, read in that zone.

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
them (`INCR ... from ...`, or `NOOP`). Decide whether to keep them. To start
over, delete them at the destination **and** their cached copies under
`<workingDirectory>/cache/<md5 of the canonical URI>/`. The canonical URI is
what the logs print: an object-store prefix always ends in `/`
(`s3://bucket/prefix/`) and a bucket root never does (`s3://bucket`), however
you typed it. A cache directory left by an older version under another spelling
is moved there on the next run (`Moved N cached manifests ...`). Plain
`zfsbackup clean` never deletes manifests, nor any object it does not recognize
as a backup volume written by this tool for a dataset that has a manifest at
that destination (it lists those as skipped). It also skips everything under a
directory that holds its own manifests (another destination nested below this
one), and every dataset that a `send` on this host is working on right now (`A
send of ... appears to be running`). It cannot see a `send` running on another
host: do not run `clean` against a destination while another machine is sending
to it. If it warns that a key `looks like a backup volume written by an older
version`, that volume sits outside the destination's prefix and has to be
deleted by hand. It refuses to run against a destination that has objects but no
manifests at all, which usually means a wrong URI. Only `clean --force` deletes
whole broken sets, manifest included. The planner ignores a cached manifest that
is gone from the destination, but `clean` still treats it as live and keeps the
volumes it lists.

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
and the same flags. A failed upload or a failed `zfs send` makes `send` exit
non-zero and release its lock; it never uploads a manifest for a partial set.
`--resume` continues from the local cache, but first checks that every volume
it would skip still exists at every destination: it resumes from the first
volume missing anywhere (`Volume ... missing at ...; it and later volumes will
be re-sent`) and starts over when none can be verified, for example after
adding a destination. Then schedule the same command daily or weekly.

What `send` refuses to do:

- overwrite a backup set whose manifest is already at any destination
  (`backup set ... already exists ...; refusing to overwrite it`); an explicit
  `--full` of a monthly that is already backed up as a full is a no-op
  (`Nothing new to back up.`, exit 0);
- skip volumes on `--resume` that it cannot see at every destination;
- resume against a `zfs send` stream shorter than the cached manifest records.

The exit status is 0 both when it uploads a backup and when there is nothing new
(`Nothing new to back up.`, the normal case on most runs), so any other status
is a real failure.

A weekly job works the same way with a pool that keeps only three monthlies
(`monthly=3`): the run on the first day after each 1st sends the new monthly as
an incremental from the previous one, the other runs are no-ops, and each full
rolls on the first run after the first 1st past the window. The previous
monthly is still on the pool then, because sanoid destroys a monthly only once
it is older than 3 x 31 days and more than three remain. The scenario
`prod-navidrome-weekly-3-monthlies` shows a year of Sunday runs against the
real pool. Check a different cadence or retention with `every=168h` (or the
cron interval) and the pool's `monthly=` count in step 2.

The chain tolerates missed runs for about two months. Once the last backed-up
monthly is pruned (the third 1st after it was taken, plus a day or two), the
next run cannot send an incremental and instead sends a full of the newest
monthly, then continues from that; nothing stalls, but the full costs one
extra upload. `monthly-only-gap-2-months` and `monthly-only-gap-3-months`
show both sides of that line.

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

A JSON capture stays in `testdata/zfs`, and the scenario links to it as
`snapshots.json` instead of copying it. `prod-navidrome-year`,
`prod-navidrome-weekly-3-monthlies` and `prod-navidrome-first-run` do this for
`backup3/enc/user/app/navidrome`, and their schedules also set `volume=` to the
dataset:

```bash
ln -s ../../../../testdata/zfs/<capture>.json backup/testdata/scenarios/prod-<dataset>/snapshots.json
```
