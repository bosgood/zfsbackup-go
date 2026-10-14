# `file://` and `ssh://` on real zfs: plan

Date: 2026-10-14. Source: `intent.md`. Runs through `make integration-vm`
(the NixOS Lima VM, `integration/README.md`). Nothing here needs a new VM
image: `sshd` lives in the Docker image, not in the VM.

## Tasks

| Task | What                                                                                                                           | Covers                                                                                                                                                                                                                     |
| ---- | ------------------------------------------------------------------------------------------------------------------------------ | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| 1    | infra: `Dockerfile` `integration` stage, new `integration/sshd.sh`, new `integration/sshd_config`, `integration/entrypoint.sh` | `sshd` on `127.0.0.1:2222` with four users and three keys, before the test runs                                                                                                                                            |
| 2    | infra: default `RUN` pattern                                                                                                   | `TestIntegration(S3                                                                                                                                                                                                        | File | SSH)`in`Makefile:49`, `integration/compose.yaml:72`, `integration/entrypoint.sh:51`, and the comment at `Makefile:43` |
| 3    | helpers (`integration_local_test.go`)                                                                                          | `itSSHTarget`, `itFileTarget`, `itDestDataset`, `wantFilesAt`, `truncateFile`, `itSSHEnv`; skip rules                                                                                                                      |
| 4    | `TestIntegrationFile` (`integration_file_test.go`)                                                                             | the `TestIntegrationS3` steps against a `file://` destination that is a ZFS dataset; `list`; `clean`; subtests `Pipe`, `Spelling`                                                                                          |
| 5    | `TestIntegrationFileResume` (`integration_file_resume_test.go`)                                                                | a cut `zfs send`; a volume truncated by hand; a real SIGTERM mid-upload; `--resume` re-sends from the short volume (`Sizer`)                                                                                               |
| 6    | `TestIntegrationFileFull` (`integration_file_full_test.go`)                                                                    | `refquota` hit mid-send: the partial volume is removed, `--resume` after the quota is lifted; subtest `ReadOnly` (`zfs set readonly=on`)                                                                                   |
| 7    | `TestIntegrationSSH` (`integration_ssh_test.go`)                                                                               | the `TestIntegrationS3` steps over sftp with the default key and default `known_hosts`, `--maxParallelUploads 4` and 24+ volumes; `list`; `clean`; subtests `Pipe`, `Names`, `Default`                                     |
| 8    | `TestIntegrationSSHAuth` (`integration_ssh_auth_test.go`)                                                                      | subtests `DefaultKey`, `KeyFile`, `Agent`, `Password`, `Username`, `Port`, `HostKey`, `Redaction`, `BadURI`                                                                                                                |
| 9    | `TestIntegrationSSHResume` (`integration_ssh_resume_test.go`)                                                                  | task 5 over sftp, plus a lost remote volume and a lost remote manifest                                                                                                                                                     |
| 10   | `TestIntegrationSSHFull` (`integration_ssh_full_test.go`)                                                                      | `refquota` on the remote dataset: no partial volume may stay (expected red: `ssh_backend.go:239`), `--resume` after the quota is lifted                                                                                    |
| 11   | `TestIntegrationSSHTwoDestinations` (`integration_ssh_twodest_test.go`)                                                        | one send to `file://` and `ssh://`; complete-partial with a truncated volume at one of them; each restores alone                                                                                                           |
| 12   | fix                                                                                                                            | `s.sftpClient.Remove` at `backends/ssh_backend.go:239`, its own commit, after task 10 is red; `docs/context/backends.md`                                                                                                   |
| 13   | docs                                                                                                                           | `integration/README.md` (new section "sshd in the test container", table rows, environment table), `docs/context/testing.md` (count and the two new families), `.claude/napkin.md`                                         |
| 14   | proofs                                                                                                                         | one break per new test in a `~/.cache/` copy, listed below; `docker builder prune -f` after                                                                                                                                |
| 15   | run                                                                                                                            | `make integration-image`, `make integration-vm RUN=TestIntegrationSSH`, `RUN=TestIntegrationFile`, the whole suite, `make lint`, `make vet`, `make fmt-check` after `git add`, `make e2e`, `make test-one PKG=./backends/` |

Order: 1, 2, 3, then 7 as the smoke test of the infrastructure, then 8, 4, 5,
9, 6, 10, 12, 11, 13, 14, 15.

## Task 1: `sshd` in the test container

`Dockerfile` `integration` stage: add `openssh-server openssh-client` to the
`apt-get install` line. `openssh-client` gives `ssh-keygen`, `ssh-keyscan`,
`ssh-agent`, `ssh-add`.

`integration/sshd.sh` (new, called by `entrypoint.sh` after the pool exists,
before the test):

1. `mkdir -p /run/sshd`; `ssh-keygen -A` (host keys).
2. Users (`useradd -m`): `keyonly`, `agentonly`, `pwonly`. Root stays as is.
   `echo 'pwonly:zfsbackup-pw' | chpasswd`.
3. Keys, all Ed25519, no passphrase:
   - `/root/.ssh/id_ed25519`: the default key. Its `.pub` goes to
     `/root/.ssh/authorized_keys`.
   - `/root/ssh-keys/keyonly_ed25519`: `.pub` to `/home/keyonly/.ssh/authorized_keys` only.
   - `/root/ssh-keys/agentonly_ed25519`: `.pub` to `/home/agentonly/.ssh/authorized_keys` only.
   - `pwonly` has no `authorized_keys`.
     Modes 0700 on the `.ssh` dirs, 0600 on the files, owned by the user.
4. `/usr/sbin/sshd -f /src/integration/sshd_config -E /var/tmp/sshd.log`.
   Wait until `127.0.0.1:2222` accepts (a loop on `ssh-keyscan`, 30 tries).
5. `ssh-keyscan -p 2222 127.0.0.1 > /root/.ssh/known_hosts`. The entries have
   the form `[127.0.0.1]:2222 ssh-ed25519 ...`; `knownhosts.New` matches a
   host with a port that way.
6. Export for the test binary: `ZFSBACKUP_IT_SSH_HOST=127.0.0.1`,
   `ZFSBACKUP_IT_SSH_PORT=2222`, `ZFSBACKUP_IT_SSH_KEYS=/root/ssh-keys`,
   `ZFSBACKUP_IT_SSH_PASSWORD=zfsbackup-pw`.
7. `set -euo pipefail`: a failed step fails the run. A run must never skip the
   ssh tests silently inside the container.

`integration/sshd_config` (new):

```
Port 2222
ListenAddress 127.0.0.1
HostKey /etc/ssh/ssh_host_ed25519_key
PermitRootLogin prohibit-password
PubkeyAuthentication yes
PasswordAuthentication yes
KbdInteractiveAuthentication no
UsePAM no
StrictModes yes
Subsystem sftp /usr/lib/openssh/sftp-server
LogLevel VERBOSE
PidFile /var/tmp/sshd.pid
```

`UsePAM no`: the container has no PAM session setup; `sshd` then checks
`/etc/shadow` itself. `LogLevel VERBOSE` names the key fingerprint each login
used, which the `Auth` subtests can check in `/var/tmp/sshd.log` if an
assertion on the client side is not enough.

Not in the entrypoint: `ssh-agent`. `SSH_AUTH_SOCK` set for every test would
put agent auth first in every `Init`. The `Agent` subtest starts its own.

## Task 2: the default `RUN` pattern

`-test.run` takes a Go regexp. `TestIntegration(S3|File|SSH)` matches the
three families and not the old `TestIntegration` (Azurite). Change the
default in the three places of the table and the Makefile comment. Check the
quoting: `Makefile:51` wraps the value in single quotes, the compose file in
double quotes, the entrypoint in `"${RUN:-...}"`. None of them splits on `|`.
Test the change with `make integration-image` (no VM) and the first VM run.

## Task 3: helpers (`integration_local_test.go`, tag `integration`)

| Helper                                                                       | Does                                                                                                                                            |
| ---------------------------------------------------------------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------- |
| `itSSHEnv(t) (host, port, keys, password string)`                            | reads the four env vars; `t.Skip` when `ZFSBACKUP_IT_SSH_PORT` is unset (a host `make integration` run keeps compiling and skips)               |
| `itDestDataset(t, pool, name string, props ...string) (dataset, dir string)` | `zfs create -o mountpoint=/<pool>/<name> [-o prop]... <pool>/<name>`, `destroyAfter`; returns the dataset and its directory                     |
| `itFileTarget(t, dir) string`                                                | `"file://" + dir`                                                                                                                               |
| `itSSHTarget(t, user, dir string) string`                                    | `chown -R user dir` (as root, `os/user.Lookup` + `os.Chown`), returns `ssh://<user>@127.0.0.1:2222<dir>`; a password variant takes `user:pass@` |
| `wantFilesAt(t, dir string, want []string)`                                  | the regular files under `dir`, relative, sorted, must be `want` (the `file`/`ssh` twin of `wantObjectsAt`)                                      |
| `fileNames(t, dir) []string`                                                 | the `objectNames` twin                                                                                                                          |
| `truncateFile(t, path string, size int64)`                                   | `os.Truncate`, fails the test on error                                                                                                          |
| `sshdLogHas(t, want string) bool`                                            | reads `/var/tmp/sshd.log` for an assertion on the server side (`Accepted publickey for keyonly`, `Accepted password for pwonly`)                |

Reuse, do not copy: `itSend`, `itReceive`, `itList`, `itClean`,
`newestBackup`, `manifestFor`, `cachedFiles`, `snapshot`, `writeFile`,
`compareDirs`, `wantSnapshots`, `wantGUIDs`, `destroyAfter`, `zfsRun`,
`zfsGet`, `wantInLog`, `wantNotInLog`. The cut-send wrapper of
`integration_s3_resume_test.go` moves to the shared helper file if it is
local to that file; the SIGTERM pattern of `TestIntegrationS3ReviewMultipartAbort`
(`cmd.SignalContext` before `syscall.Kill`, `config.BackupUploadBucket`
restored in `t.Cleanup`) is copied as a helper `sendThenSIGTERM`.

Every ssh test takes `--maxRetryTime 10s` (after the `2m` of `itRunCtx`; the
last value of a repeated flag wins), so a failure case ends in seconds.

## The tests

Each test makes its own datasets under the pool (`destroyAfter`) and its own
destination directory or dataset. Whole-chain restores use an empty working
directory (`t.TempDir()`) so the manifests come from the destination. Every
restored snapshot checks its `guid` against the source.

### Task 4: `TestIntegrationFile`

Destination: `itDestDataset(t, pool, "filedest")`, target `file:///<pool>/filedest`.

1. `src` with 3 MiB, snapshots monthly, daily, hourly; the smart flags of
   `TestIntegrationS3` (`--volsize 1 --compressor ""`).
2. Full: the monthly. `wantFilesAt` shows 3+ volumes and one manifest, as
   regular files under the dataset; their sizes equal the manifest's `Size`.
3. Incremental to the newest daily; noop.
4. `receive -F` of the full; `receive --auto` onto it; `receive --auto` into
   an empty dataset. `compareDirs` each time.
5. `list --jsonOutput`: two backups.
6. `clean`: an orphan volume file and a `notes.txt`; `--dry-run` names the
   orphan and deletes nothing; a real `clean` deletes the orphan and keeps
   `notes.txt` with the "unrecognized object" line.
7. Subtest `Pipe`: `--maxFileBuffer 0` send and receive, as in S3.
8. Subtest `Spelling`: a second send of the same dataset with
   `file:///<pool>/filedest/` (trailing slash) and `--workingDirectory
t.TempDir()`: `CanonicalURI` leaves `file://` alone, so the cache key
   differs; `syncCache` reads the manifests from the directory and the smart
   send is a noop. Proves `file://` works with no cache at all.

### Task 5: `TestIntegrationFileResume`

Destination as in task 4. 24 MiB random data, `--volsize 1 --compressor ""`.

1. A cut `zfs send` (the wrapper): some volumes land, the send fails. The
   cached partial manifest lists them; the destination holds exactly those
   files (`wantFilesAt`).
2. `truncateFile` volume 2 to half its size. `--resume`: the log has
   `Volume <vol2> at <dest> is <n> bytes, want <m>; it and later volumes will
be re-sent.` (`backup/backup.go:1781`). The send completes. Every volume at
   the destination has the manifest's size.
3. Restore from an empty working directory; `compareDirs`; `wantGUIDs`.
4. Subtest `SIGTERM`: a new dataset, `--maxUploadSpeed` so a volume takes
   seconds, `sendThenSIGTERM` after volume 1 is cached. Afterwards the
   destination holds a file under volume 2's final name that is shorter than
   its eventual size (the reason for `Sizer`). `--resume` re-sends it with the
   Notice above; restore matches.

### Task 6: `TestIntegrationFileFull`

Destination `itDestDataset(t, pool, "fulldest", "refquota=8M")`. 20 MiB data.

1. `send --full --maxRetryTime 10s`: fails. The error chain holds the quota
   error (zfs returns `EDQUOT`, "disk quota exceeded"; record the exact text
   of the first run in the Notes). `wantFilesAt` holds only whole volumes: the
   file backend removed the partial (`file_backend.go:96`). The cached partial
   manifest exists.
2. `zfs set refquota=none`. `--resume`: completes with no re-send Notice for
   the whole volumes. Restore matches.
3. Subtest `ReadOnly`: a destination dataset with a finished backup;
   `zfs set readonly=on`. A new send fails fast (`EROFS`, `--maxRetryTime
10s`) and leaves no file. `list` works. `receive --auto` into a new dataset
   works and matches. `clean --dry-run` works; a real `clean` with an orphan
   present fails on the delete and the orphan stays. `zfs set readonly=off`
   in `t.Cleanup` before `destroyAfter` runs (register it after).

### Task 7: `TestIntegrationSSH`

Target `ssh://root@127.0.0.1:2222/<pool>/sshdest` on
`itDestDataset(t, pool, "sshdest")`. No `SSH_*` variable set: the default key
`/root/.ssh/id_ed25519` and the default `/root/.ssh/known_hosts` do the work.

1. 24 MiB random data; `--volsize 1 --compressor "" --maxParallelUploads 4`.
   Smart full, incremental, noop as in task 4.
2. After the full: `wantFilesAt` shows 24+ volumes and a manifest with the
   manifest's sizes; `sshdLogHas("Accepted publickey for root")`.
3. Three receives, `list`, `clean` as in task 4 (the orphan is written with
   `os.WriteFile` into the remote dir).
4. Subtest `Pipe`: `--maxFileBuffer 0` both ways.
5. Subtest `Names`: dataset `na me:1`, send and `receive --auto` into
   `re stored:1`; the remote paths carry the space and the colon.
6. Subtest `Default`: 256 MiB with the default flags (gzip, `--volsize 200`):
   two volumes; restore matches. Keeps the run under a minute on loopback.

### Task 8: `TestIntegrationSSHAuth`

One small dataset (1 MiB, one snapshot) sent once per subtest to a fresh
remote directory; each subtest asserts `send` succeeds or fails as expected
and, on success, `list` sees the backup. `t.Setenv` for every `SSH_*`
variable, including `SSH_AUTH_SOCK=""` unless the subtest wants the agent.

| Subtest      | Setup                                                                                                                                                                                                                              | Expect                                                                                                                                                                                                       |
| ------------ | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| `DefaultKey` | nothing set; user `root`                                                                                                                                                                                                           | success; `sshd.log` names root's key                                                                                                                                                                         |
| `KeyFile`    | `SSH_KEY_FILE=/root/ssh-keys/keyonly_ed25519`; user `keyonly`                                                                                                                                                                      | success. Then `SSH_KEY_FILE` unset, same user: refused (root's default key is not authorized for `keyonly`); the error names no password                                                                     |
| `Agent`      | `ssh-agent -a <tmp>/sock` started by the test (killed in `t.Cleanup`), `ssh-add /root/ssh-keys/agentonly_ed25519`, `SSH_AUTH_SOCK=<sock>`, `SSH_KEY_FILE=/root/.ssh/id_ed25519` (not authorized for `agentonly`); user `agentonly` | success only through the agent; `sshd.log` names the agentonly key. Then `SSH_AUTH_SOCK=""`: refused                                                                                                         |
| `Password`   | user `pwonly`; `SSH_PASSWORD=zfsbackup-pw`                                                                                                                                                                                         | success (the default key is offered first and refused; then the password). URI form `ssh://pwonly:zfsbackup-pw@...`: success. Wrong password: refused                                                        |
| `Username`   | `SSH_USERNAME=pwonly`, `SSH_PASSWORD` set, URI without a user                                                                                                                                                                      | success as `pwonly`. URI user `root` with `SSH_USERNAME=pwonly`: the URI wins (root, key auth)                                                                                                               |
| `Port`       | URI without `:2222`                                                                                                                                                                                                                | `Init` adds `:22` (`ssh_backend.go:187`); nothing listens: the error is a dial error within the 30 s timeout, not a hang                                                                                     |
| `HostKey`    | `SSH_KNOWN_HOSTS=<file with another host's key for [127.0.0.1]:2222>`                                                                                                                                                              | refused with the `knownhosts` mismatch error, before any auth (`sshd.log` has no `Accepted`). `SSH_KNOWN_HOSTS=<missing file>`: `Init` fails and the error names the file. `SSH_KNOWN_HOSTS=ignore`: success |
| `Redaction`  | `ssh://pwonly:zfsbackup-pw@...`, `--logLevel debug`, send, `list`, `clean --dry-run`, and a send with a wrong password                                                                                                             | `wantNotInLog(zfsbackup-pw)` on every log; no file under the working directory holds the password (`grep -r` of `work`)                                                                                      |
| `BadURI`     | `ssh://127.0.0.1:2222` (no path); a remote path that is a regular file; a remote path that does not exist                                                                                                                          | `ErrInvalidURI` for the first two; a stat error for the third. The backend never creates the top directory: documented, asserted                                                                             |

### Task 9: `TestIntegrationSSHResume`

Task 5 over sftp (`ssh://root@...`), plus:

- a remote volume deleted by hand: `--resume` logs `Volume <n> missing at
<dest>; it and later volumes will be re-sent.` and completes;
- the remote manifest deleted after a finished send: a smart send sees no
  backup at the target and makes a full (as `TestIntegrationS3Resume` shows
  for S3);
- subtest `SIGTERM`: the truncated remote object under its final name, then
  `--resume`. `Sizer.Size` runs over sftp here.

### Task 10: `TestIntegrationSSHFull`

Remote dir on `itDestDataset(t, pool, "sshfull", "refquota=8M")`. 20 MiB.

1. `send --full --maxRetryTime 10s` fails. Assert: no file under the remote
   dir is shorter than the manifest's size for that volume, and no file exists
   for a volume the cached partial manifest does not list. This is the right
   behavior and is EXPECTED RED at HEAD: `ssh_backend.go:239` removes a local
   path, so the truncated remote volume stays, with the Warning "Error
   deleting failed upload file". Record both in the Notes, then task 12.
2. After the fix: `zfs set refquota=none`, `--resume` completes, restore
   matches. Also assert the Warning is gone from the log.

### Task 11: `TestIntegrationSSHTwoDestinations`

One send with `file:///<pool>/dest-a,ssh://root@127.0.0.1:2222/<pool>/dest-b`.

1. Both hold the same object names (`wantFilesAt` twice, same list).
2. `truncateFile` volume 1 at `dest-b` only; a `send --resume` of the same
   set reports `dest-b` out of sync with the complete-partial error of
   `backup/backup.go:1161` ("is not the one the manifest ... describes",
   with the byte counts) and uploads nothing new to `dest-a`.
3. Put the volume back (copy from `dest-a`); each destination restores alone
   from an empty working directory; `compareDirs` and `wantGUIDs`.

## Facts the tests rely on

1. `FileBackend.Init` (`backends/file_backend.go:43`) needs an existing
   directory; `Upload` creates subdirectories, writes under the final name,
   and removes the file after a failed copy; `List` walks the whole tree and
   fails on the first walk error; `Delete` returns the `os.Remove` error.
2. `SSHBackend.Init`: remote path = the URI path with a trailing slash
   trimmed, empty refused unless `/`; user: URI, then `SSH_USERNAME`, then
   `user.Current()`; password: URI, then `SSH_PASSWORD`. Auth order
   (`ssh_backend.go:55`): agent if `SSH_AUTH_SOCK` is non-empty, then
   `SSH_KEY_FILE` or the six default `~/.ssh` keys, then the password.
   `known_hosts` (`ssh_backend.go:109`): `SSH_KNOWN_HOSTS` path, `ignore`, or
   `~/.ssh/known_hosts`. `:22` is added when the host has no port
   (`ssh_backend.go:187`). Dial timeout 30 s. The remote path is `Stat`ed and
   must be a directory.
3. `~` is `user.Current().HomeDir`, from `/etc/passwd` (cgo or pure Go
   alike): `/root` in the container whatever `$HOME` says. The entrypoint
   places the default key and `known_hosts` there.
4. `verifiedVolumes` (`backup/backup.go:1781`) re-sends from the first
   volume that is missing or whose `Size` differs; Notice texts above. The
   complete-partial check (`backup/backup.go:1161`) stats first and hashes
   only when the size matches; a mismatch is the "not the one the manifest
   ... describes" error.
5. `ssh_backend.go:239` is the expected defect (intent.md). `pkg/sftp`'s
   client is safe for concurrent use: one session carries the four parallel
   uploads.
6. pflag: the last value of a repeated scalar flag wins, so `--maxRetryTime
10s` after `itRunCtx`'s `2m` takes effect.
7. zfs: `refquota` gives `EDQUOT` to writers, `readonly=on` gives `EROFS`,
   also to root. Through `sftp-server` the client sees an SFTP status whose
   text may differ; the tests assert on the outcome (files, manifests,
   restore), not on the text, until the first run records it.
8. `ssh-keyscan -p 2222 127.0.0.1` writes `[127.0.0.1]:2222` entries;
   `golang.org/x/crypto/ssh/knownhosts` normalizes a host with a non-22 port
   to that form.
9. `-test.run` is a Go regexp: `TestIntegration(S3|File|SSH)` does not
   match the old `TestIntegration`.
10. Integration tests are not parallel: `t.Setenv` is safe; one `sshd`
    serves all tests in turn.
11. `t.Cleanup` is LIFO: a `zfs set readonly=off` that must run before
    `destroyAfter` is registered after it.

## Task 14: break proofs

One break per test, in a `git archive HEAD | tar -x` copy under `~/.cache/`,
run with `make -C <copy> integration-vm RUN=<test>`. The test must go red.

| Test                                | Break                                                                                                                                    |
| ----------------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------- |
| `TestIntegrationFile`               | `FileBackend.List` drops every name that ends in `.manifest`: `list` sees no backup                                                      |
| `TestIntegrationFileResume`         | `verifiedVolumes`: skip the `Size` check (`sizer = nil`): the truncated volume is kept and the restore fails its SHA-256 check           |
| `TestIntegrationFileFull`           | `FileBackend.Upload`: do not `os.Remove` on a failed copy: a short volume stays                                                          |
| `TestIntegrationSSH`                | `SSHBackend.Upload`: `filepath.Join(s.remotePath, filepath.Base(vol.ObjectName))`: a name with a `/` lands elsewhere; or drop `MkdirAll` |
| `TestIntegrationSSHAuth`            | `buildHostKeyCallback`: always `InsecureIgnoreHostKey`: `HostKey` passes where it must refuse; `Redaction`: log the URI unredacted once  |
| `TestIntegrationSSHResume`          | as `FileResume`                                                                                                                          |
| `TestIntegrationSSHFull`            | HEAD itself is the break (task 10)                                                                                                       |
| `TestIntegrationSSHTwoDestinations` | complete-partial: skip the `Size` branch                                                                                                 |

Then `limactl shell zfsbackup -- docker builder prune -f` (the napkin: break
copies fill the VM disk).

## Task 15: run and verify

1. `make integration-image` on the host (OrbStack): compiles and vets the
   tagged tests, builds the image with `openssh-server`. No VM.
2. Before a VM run: `limactl shell zfsbackup -- docker ps` (another session's
   run?) and `limactl shell zfsbackup -- df -h /`.
3. `make integration-vm RUN=TestIntegrationSSH` (all `SSH*` tests), then
   `RUN=TestIntegrationFile`, then the whole suite with the new default.
4. `make lint`, `make vet`; `git add` the new files, then `make fmt-check`;
   `make e2e` on the host (`TestE2EPlanManifestsFileAdoptsCreationTimes` is
   the known EDT failure); `make test-one PKG=./backends/` after task 12.
5. Expected added time on the VM: about two to three minutes (`Default` and
   the two 24 MiB streams dominate; all on loopback).

## Risks and unknowns

- `sshd` may refuse to start in the container (`/run/sshd` missing, host key
  modes, `UsePAM`). The entrypoint fails the run in that case; the fix is in
  `sshd.sh` or `sshd_config`, not in a test.
- `PermitRootLogin prohibit-password` plus `StrictModes yes` needs
  `/root/.ssh` at 0700 and `authorized_keys` at 0600, owned by root.
- The `Agent` subtest leaves an `ssh-agent` process if its cleanup does not
  run; `t.Cleanup` kills it by pid from `ssh-agent -D`'s output or runs it
  with `exec.CommandContext`.
- The exact SFTP error text for `EDQUOT` and `EROFS` is unknown until the
  first run. Tasks 6 and 10 assert on state, and the Notes record the text.
- `useradd` and `chpasswd` come from `passwd`, present in `ubuntu:24.04`.
  Check in the image build, not at run time.
- Tests that run `send` with a wrong password or a wrong host key produce
  `Failed password` and key-mismatch lines in `sshd.log`; `sshdLogHas` must
  search for the exact `Accepted ... for <user>` line of its own subtest.

## Out of scope

- `azure://`, `gs://`, `b2://` (the next sweep).
- A slow or lossy network between client and `sshd`.
- The `Close` error drop at `ssh_backend.go:285` (unit test, separate change).
- The old `TestIntegration` (Azurite).

## Notes

(Filled in as the tasks run: first-run error texts, red tests, fixes.)
