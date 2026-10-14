# `file://` and `ssh://` on real zfs: intent

Date: 2026-10-14. Branch `clean-dry-run`. Follows
`../2026-10-13--integration-test-sweep/intent.md`, which left "the other
backends" open. This spec takes the two that need no cloud account:
`backends/file_backend.go` and `backends/ssh_backend.go`. Azure, gs and b2
stay open.

## What runs today

| Backend   | Tests that run it                                                                                                                                           | What never ran                                                                                                                                                                                                                                                                                                                                                                           |
| --------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `file://` | every root e2e test (fake zfs); `backup/clean_test.go`, `restore_test.go`; `backends.TestFileBackend` (`BackendTest`: Init, Upload, List, Download, Delete) | real `zfs send` into a `file://` destination; a destination that is a ZFS dataset; a full destination (`refquota`); a read-only destination; a truncated volume left by a real kill                                                                                                                                                                                                      |
| `ssh://`  | `backends.TestSSHBackend` only: an in-process `pkg/sftp` server, password auth, `SSH_KNOWN_HOSTS=ignore`                                                    | a real OpenSSH `sshd`; public key auth (`SSH_KEY_FILE`, the six default `~/.ssh` keys); ssh-agent auth; a real `known_hosts` check, a wrong host key; `SSH_USERNAME`; a port in the URI; `send`, `receive`, `list`, `clean` end to end over sftp; `--resume` with a truncated remote volume (`Sizer`); many volumes on one sftp session; a full remote; password redaction in a real run |

The `Sizer` interface exists for these two backends only (they write a volume
under its final name from the first byte). Its two callers,
`verifiedVolumes` (`backup/backup.go:1781`, `--resume`) and the complete-partial
check (`backup/backup.go:1161`), ran only with the fake zfs and `file://`
(`TestE2EResumeReSendsTruncatedVolume`). Never with `ssh://`.

## Holes

| Hole                                                                               | Why it matters                                                                                                                                  |
| ---------------------------------------------------------------------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------- |
| `send`/`receive`/`list`/`clean` over a real `sshd`                                 | the only test server is `pkg/sftp` in-process; OpenSSH's sftp-server answers differently (errors, `Walk` order, `MkdirAll` on an existing path) |
| public key auth: `SSH_KEY_FILE` and the default `~/.ssh/id_*` files                | the README documents it as the main way; no test ever used a key                                                                                |
| ssh-agent auth                                                                     | documented; never run                                                                                                                           |
| `known_hosts`: default file, `SSH_KNOWN_HOSTS=<file>`, a wrong key, a missing file | the only safety check of the transport; only `ignore` was ever tested                                                                           |
| `SSH_USERNAME`, `SSH_PASSWORD`, `user:pass@` in the URI, a port in the URI         | the precedence rules in `Init` are untested                                                                                                     |
| password redaction in a real run                                                   | `RedactURI` has unit tests; no test proves a real `send` with `ssh://u:p@...` at `--logLevel debug` leaks nothing                               |
| `--resume` after a truncated volume, over sftp                                     | `Sizer.Size` over sftp never ran                                                                                                                |
| a SIGTERM during an upload to `file://` or `ssh://`                                | the documented reason for `Sizer`; only the fake saw it                                                                                         |
| a full `file://` destination (`refquota`)                                          | the file backend removes its partial on a failed copy; never shown with a real `EDQUOT`                                                         |
| a full `ssh://` remote                                                             | expected defect, see below                                                                                                                      |
| a read-only `file://` destination (`zfs set readonly=on`)                          | `send` must fail; `list` and `receive` must still work                                                                                          |
| a `file://` destination that is a ZFS dataset                                      | the common "second pool or USB disk" setup                                                                                                      |
| `file://` + `ssh://` as two destinations of one send                               | the complete-partial `Sizer` check at `backup/backup.go:1161` never ran with a `Sizer`                                                          |
| `--maxParallelUploads 4` with many volumes on one sftp session                     | concurrent `Create`/`Write` on one `sftp.Client`                                                                                                |
| a dataset name with a space and a colon over sftp                                  | the object name becomes a remote path                                                                                                           |

## Defects to expect

1. `backends/ssh_backend.go:239`: after a failed `io.Copy`, `Upload` calls
   `os.Remove(destinationPath)`. That is the LOCAL filesystem. The truncated
   remote file stays, and the log gets a Warning "Error deleting failed upload
   file ... no such file or directory". The fix is `s.sftpClient.Remove`.
   `TestIntegrationSSHFull` is written for the right behavior and is expected
   red until the fix lands in its own commit.
2. `backends/ssh_backend.go:285`: `Close` keeps the ssh client's close error
   only when it is nil (`sshErr == nil && err == nil`). The error is always
   dropped. A unit test matter; noted here, not part of this plan.

## Design decision: `sshd` runs inside the test container

The `zfs` container of `integration/compose.yaml` gets `openssh-server` and
starts `sshd` on `127.0.0.1:2222` before the test binary runs.

Why not a second compose service:

- The test must read and tamper with remote files (truncate a volume, drop a
  manifest, count objects). In one container that is `os.Stat` and
  `os.Truncate`; across containers it needs a shared volume.
- The remote directory can be a dataset of the test pool, so `refquota` and
  `readonly=on` apply to the sftp server too.
- `SSHBackend.Init` reads the default keys and `known_hosts` from
  `user.Current().HomeDir`, which is `/root` in the container. The entrypoint
  can place `/root/.ssh/id_ed25519` and `/root/.ssh/known_hosts`, so the
  unmodified default path runs. A second service needs a committed keypair or
  a bootstrap volume for `authorized_keys`.
- No port is published; the Lima VM needs no change.

Cost: the transport is loopback, so no test sees a slow or lost network. That
is acceptable: the backend has no network-specific code beyond `ssh.Dial`.

## Goal

Seven new `TestIntegrationFile*` and `TestIntegrationSSH*` tests in new files,
one shared helper file; `sshd` in the `integration` image; the default `RUN`
pattern takes the new names; `integration/README.md` and
`docs/context/testing.md` updated; the whole suite green on the VM. The ssh
`Upload` defect gets its fix in a separate commit, after the test that shows it
is red. Everything else is tests and test infrastructure.
