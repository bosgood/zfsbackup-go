# Plan: file:// and ssh:// backend bug fixes

Source: adversarial review of `backends/file_backend.go` and
`backends/ssh_backend.go` (2026-09-29 session). Written for an implementing
agent. Line numbers are as of commit `9565fa1` on `clean-dry-run`; re-check
them before editing.

## Ground rules

- Work on branch `clean-dry-run` (current). One commit per task below; each
  commit must leave `make test-run PKG=./backends/` green, and run
  `make test-docker` before declaring the whole plan done.
- TDD: write the failing test first (it must reproduce the bug against the
  unfixed code), then fix. Each task names its test.
- Tasks are ordered so that the mechanical path refactor (Task 5) lands before
  the atomic-write rewrite (Task 7), which touches the same lines.
- Don't touch other backends (S3, GCS, Azure, B2) except where a task says
  so. If a fix obviously applies there too (e.g. ctx-aware semaphore in GCS),
  note it in the commit message; don't fix it here.

## Shared background

- `Backend` interface: `backends/backends.go`. Callers: `backup/sync.go:38`
  (`prepareBackend`, which calls `Init` and returns the backend **even when
  `Init` fails**), `backup/backup.go:771` (`volUploadWrapper`, which calls
  `vol.OpenVolume()` then `Upload` inside a `backoff.Retry`, so `Upload` may be
  called several times for the same object).
- `clean` and `restore` call `prepareBackend` with a **nil**
  `MaxParallelUploadBuffer`; they never call `Upload`, so that's fine, but
  don't add code to `Init` that touches the channel.
- `syncCache` (`backup/sync.go`) calls `List(ctx, j.ManifestPrefix)` and
  downloads everything it returns as a manifest. **Anything `List` returns
  under the manifest prefix gets parsed as a manifest**, which matters for the
  temp-file naming in Task 7.
- Shared backend test harness: `BackendTest` in `backends/backends_test.go:98`.
  `prepareTestVols` gives a `goodVol` (10 MiB random) and a `badVol` (closed and
  deleted, so reading it errors immediately).
- SSH test server: `startSftpServer` in `backends/ssh_backend_test.go:40` uses
  `sftp.NewServer(channel)`, which serves the **real local filesystem from
  the same process**. So "remote" and "local" paths are the same files, and a
  local-vs-remote mix-up (Task 1) can't be detected with it. pkg/sftp v1.13.5
  ships `sftp.NewRequestServer(channel, sftp.InMemHandler())` (in-memory FS,
  supports `posix-rename@openssh.com`). Use it for the new tests.

## Task 0: test infrastructure

Add to `backends/ssh_backend_test.go`:

- `startSftpServer` gains an option (or a sibling `startInMemSftpServer`) that
  serves `sftp.NewRequestServer(channel, sftp.InMemHandler())` instead of the
  real FS. Keep the existing server for `TestSSHBackend`.
- Replace the `t.Fatal` calls inside server goroutines with `t.Error` + return
  (`t.Fatal` from a non-test goroutine is invalid and hides failures).
- `TestSSHBackend` uses `os.Setenv("SSH_KNOWN_HOSTS", "ignore")` while
  `t.Parallel()`. Switch to `t.Setenv` (and drop `t.Parallel()` for tests
  that set env vars; `t.Setenv` panics in parallel tests).

Add to `backends/backends_test.go`:

- `failingVolume(t, n int)`: a `*files.VolumeInfo` whose reader returns `n`
  bytes then `errTest`. If `VolumeInfo` can't wrap an arbitrary reader, build
  it with `files.CreateSimpleVolume`, write `n` bytes, close, reopen, and
  `os.Truncate` the backing file after `OpenVolume` so the read comes up short;
  or add a small test-only hook in `files`. Pick whichever needs the least
  production-code change, and note it in the commit.

Commit: `backends: test infra for failure-path backend tests`.

## Task 1 (C1, critical): SSH failed upload removes a local file, leaves the remote partial

`ssh_backend.go:233`: the cleanup after a failed `io.Copy` calls
`os.Remove(destinationPath)` (the local FS) instead of the SFTP client.

**Test** `TestSSHUploadFailureCleansRemote` (in-mem server):
1. Create a local sentinel file at `<t.TempDir()>/sentinel/obj`.
2. Point the backend at the in-mem server with remote path
   `<t.TempDir()>/sentinel`, the same absolute path, so the local and remote
   paths collide. Create that dir on the in-mem server first (via a direct
   `sftp.Client`).
3. Upload `failingVolume(t, 1 MiB)` as `obj`. Expect an error.
4. Assert the local sentinel **still exists**, and that a direct
   `sftpClient.Stat(remote/obj)` returns `os.ErrNotExist`.

**Fix**: `s.sftpClient.Remove(destinationPath)`. Delete the now-unused `os`
import only if nothing else uses it (the auth helpers do, so it stays).

Commit: `ssh backend: clean up failed uploads on the remote, not locally`.

## Task 2 (W1): `SSHBackend.Close` drops the ssh close error

`ssh_backend.go:270`: `if sshErr == nil && err == nil { err = sshErr }` is
inverted, so it only ever assigns nil.

**Test** `TestSSHCloseReportsSSHError`: Init against the in-mem server, close
the underlying `sshClient` out from under it (`s.sshClient.Close()` via the
concrete type), then call `Close()`. Before the fix it returns the sftp error
or nil, never the ssh error. Simplest assertion: close `sftpClient` first
manually and set it to nil, then close `sshClient` manually, then `Close()`
must return non-nil (double close of ssh.Client errors).

**Fix**: `if sshErr != nil && err == nil { err = sshErr }`.

Commit: `ssh backend: report ssh close errors`.

## Task 3 (W2, W3, N3): auth method construction

All in `buildAuthMethods` (`ssh_backend.go:53-102`).

1. **Default key list** (`:74-81`): `id_cdsa` is a typo and `id_ecdsa` is
   missing. Use `id_rsa`, `id_ecdsa`, `id_ed25519`, `id_dsa`. Drop `id_ecdsa_sk`
   and `id_ed25519_sk`: `ssh.ParsePrivateKey` can't use FIDO keys, and
   their presence only produces warnings. Add a comment saying so.
2. **Stale `SSH_AUTH_SOCK`** (`:56-62`): if `net.Dial` fails, log
   `Warningf("ssh backend: ssh-agent at %s unavailable, skipping - %v", ...)`
   and continue instead of returning the error.
3. **Agent connection leak**: the dialed `net.Conn` is never closed. Return it
   from `buildAuthMethods` (or store it on `SSHBackend` as `agentConn`) and
   close it in `Close()` and on every `Init` error path (see Task 6).
4. **N3**: drop the named results `(sshAuths []ssh.AuthMethod, err error)`.
   `err` is shadowed in three inner scopes. Use plain results.

**Tests** (table test `TestBuildAuthMethods`, using `t.Setenv` and a fake home
dir from `t.TempDir()`):
- Home with only `.ssh/id_ecdsa` (generate with `ecdsa.GenerateKey` +
  `ssh.MarshalPrivateKey`): expect one `PublicKeys` method. Fails before the fix.
- `SSH_AUTH_SOCK=/nonexistent.sock` plus a valid `id_ed25519`: expect no error
  and at least one auth method. Fails before the fix.

Commit: `ssh backend: fix default key names, tolerate stale ssh-agent socket`.

## Task 4 (W5): IPv6 host without port

`ssh_backend.go:179-182`: `strings.Contains(hostname, ":")` is true for
`[::1]`, so `:22` is never appended.

**Fix**:
```go
port := targetUrl.Port()
if port == "" {
	port = "22"
}
hostname := net.JoinHostPort(targetUrl.Hostname(), port)
```

**Test**: extract the address logic into `sshDialAddress(u *url.URL) string`
and table-test `host`, `host:2222`, `[::1]`, `[::1]:2222`, `user@host`.
Optionally add an end-to-end Init against the in-mem server listening on
`[::1]:0`, skipping if IPv6 loopback is unavailable.

Commit: `ssh backend: default port correctly for IPv6 hosts`.

## Task 5 (W9, W6): remote path handling

1. **`filepath` → `path` for remote paths.** SFTP paths are always `/`.
   Replace `filepath.Join`/`filepath.Dir`/`filepath.Separator` with
   `path.Join`/`path.Dir`/`"/"` at `ssh_backend.go:214, 215, 253, 285, 290`.
   `filepath` stays only for local key/known_hosts paths.
2. **Root remote path** (`:135-139`): `ssh://host/` currently yields
   `remotePath == ""`, so `Stat("")` hits the SFTP working dir (usually
   `$HOME`), uploads become relative, and `List` trims the wrong prefix. Keep
   `remotePath = "/"` for the root case:
   ```go
   s.remotePath = path.Clean(targetUrl.Path)  // "/" stays "/", "/a/b/" -> "/a/b"
   if targetUrl.Path == "" { return ErrInvalidURI }
   ```
   and in `List` compute the trim prefix as
   `strings.TrimSuffix(s.remotePath, "/") + "/"`.
3. **File backend root path** (`file_backend.go:137`): the same bug. With
   `localPath == "/"`, `TrimPrefix(path, "//")` never matches. Use
   `filepath.Rel(f.localPath, path)` instead of `TrimPrefix`.

**Tests**:
- `TestSSHRootRemotePath`: in-mem server, URI `ssh://test:password@addr/`,
  Upload `goodVol` as `a/b.vol`, assert `List(ctx, "a/")` returns `["a/b.vol"]`
  and a direct `Stat("/a/b.vol")` succeeds.
- `TestFileListRootRelative`: unit-test the trimming helper with base `/`
  (don't walk the real `/`): extract `relObjectName(base, p string)`.

Commit: `backends: use slash paths for sftp; fix root-path targets`.

## Task 6 (W7): SSH Init leaks connections on failure

`ssh_backend.go:184-204`: when `sftp.NewClient` or `Stat` fails, or the path
isn't a directory, `sshClient` (and the agent conn from Task 3) stay open.
`prepareBackend` doesn't `Close` on Init error.

**Fix** (both layers):
- In `SSHBackend.Init`, use a named `err` with
  `defer func() { if err != nil { _ = s.Close() } }()` placed after the
  agent/ssh dial.
- In `backup/sync.go:prepareBackend`, on `Init` error call `backend.Close()`
  and return `nil, err` (callers already treat a non-nil error as fatal;
  verify by grepping the three call sites: `backup.go:144, 353`,
  `clean.go:49`, `restore.go:60`).

**Test** `TestSSHInitClosesOnBadPath`: in-mem server, URI with a remote path
that doesn't exist. After `Init` fails, assert `b.sshClient == nil` and
`b.sftpClient == nil` (white-box, same package).

Commit: `ssh backend: close connections when Init fails`.

## Task 7 (C2, critical): atomic, durable uploads

Both backends `Create` (truncate) the final path and stream into it. A crash,
kill, or dropped connection leaves a truncated object under a valid name; a
retry or re-send truncates a previously good object; the file backend never
fsyncs.

**Design**:
- Temp name: `<final>.zfsbackup-partial`, in the same directory as the final
  object (same filesystem, so rename is atomic). Define
  `const partialUploadSuffix = ".zfsbackup-partial"` in `backends/backends.go`.
- **file**: `os.OpenFile(tmp, O_WRONLY|O_CREATE|O_TRUNC, 0o600)` → `io.Copy`
  → `w.Sync()` → `w.Close()` → `os.Rename(tmp, final)` → fsync the parent
  directory (`os.Open(dir)`; `d.Sync()`; ignore `EINVAL` on platforms that
  don't support directory sync). On any error, remove `tmp` and leave `final`
  untouched.
- **ssh**: `sftpClient.OpenFile(tmp, O_WRONLY|O_CREATE|O_TRUNC)` → `io.Copy`
  → `Close()` → `sftpClient.PosixRename(tmp, final)`; if the server lacks the
  extension (`*sftp.StatusError` with `SSH_FX_OP_UNSUPPORTED`), fall back to
  `Remove(final)` (ignore not-exist) + `Rename(tmp, final)` and log at Debug
  that the fallback isn't atomic. `sftp.File` has no fsync without the
  `fsync@openssh.com` extension; call `w.Sync()` and ignore
  `SSH_FX_OP_UNSUPPORTED`.
- **`List` must skip `*.zfsbackup-partial`** in both backends. Otherwise
  `syncCache` downloads a half-written manifest and fails to parse it, and
  `clean` sees orphans as unknown objects. Leftover partials from a crash
  are overwritten (O_TRUNC) by the next attempt for that object; sweeping
  stale partials is out of scope (log it in the commit message as a follow-up).

**Tests** (run against both backends via a shared helper, in-mem server for ssh):
- `TestUploadFailureKeepsExistingObject`: Upload `goodVol` as `obj`; then
  Upload `failingVolume` as `obj`. Download `obj` and assert it still equals
  the `goodVol` payload. Fails before the fix (truncated / removed).
- `TestUploadFailureLeavesNoVisibleObject`: Upload `failingVolume` as `new`;
  `List(ctx, "")` must not contain `new` or `new.zfsbackup-partial`.
- `TestListSkipsPartials`: create `x.zfsbackup-partial` directly in the
  target; `List` must not return it.

Commit: `backends(file,ssh): write uploads via temp file + rename; fsync file backend`.

## Task 8 (W8): honor ctx in Upload

1. **Semaphore**: replace `f.conf.MaxParallelUploadBuffer <- true` (file
   `:75`, ssh `:209`) with the Azure pattern (`azure_backend.go:186-189`):
   ```go
   select {
   case <-ctx.Done():
   	return ctx.Err()
   case f.conf.MaxParallelUploadBuffer <- true:
   }
   defer func() { <-f.conf.MaxParallelUploadBuffer }()
   ```
2. **Copy**: wrap the source in a ctx-aware reader
   (`type ctxReader struct{ ctx; r io.Reader }` whose `Read` returns
   `ctx.Err()` once the ctx is done) and put it in `backends/backends.go` so
   both backends share it. For ssh, also close the `sftp.File` from a
   goroutine on `ctx.Done()` so a write blocked on a stalled TCP connection
   unblocks: `stop := context.AfterFunc(ctx, func() { _ = w.Close() })`;
   `defer stop()` (Go ≥1.21; go.mod is on 1.25).

**Tests**:
- `TestUploadCtxCancelledWhileWaitingForSlot`: buffer `make(chan bool, 1)`
  pre-filled; cancel the ctx; `Upload` must return `context.Canceled` within
  1s. Run it for both backends. Before the fix it blocks forever (guard
  with a `time.After` that `t.Fatal`s).
- `TestUploadCtxCancelledMidCopy`: a volume reader that blocks after the
  first 1 MiB until the test closes a channel; cancel the ctx; Upload returns
  `context.Canceled` and (from Task 7) no visible object remains.

Commit: `backends(file,ssh): make Upload cancellable`.

## Task 9 (W4): don't send the key passphrase as a login password

`SSH_PASSWORD` (or the URL password) is used both to decrypt private keys
(`buildSshSigner`) and as `ssh.Password` auth (`:98-100`). A user who sets it
only to unlock their key sends the passphrase to the server, which a
malicious or MITM'd server can collect.

**Design** (keeps compatibility):
- New env var `SSH_KEY_PASSPHRASE`, used only for key decryption.
- If `SSH_KEY_PASSPHRASE` is unset, keep falling back to `SSH_PASSWORD` for
  key decryption, but if that fallback actually decrypts a key, log a
  `Warningf` saying `SSH_PASSWORD` is also sent as a login password and to
  use `SSH_KEY_PASSPHRASE` instead.
- `ssh.Password(password)` keeps using only `SSH_PASSWORD`/URL password.
- Change `buildAuthMethods(userHomeDir, password)` to take
  `(userHomeDir, password, keyPassphrase string)`.

**Tests** in `TestBuildAuthMethods`: encrypted ed25519 key (via
`ssh.MarshalPrivateKeyWithPassphrase`) with `SSH_KEY_PASSPHRASE` set and
`SSH_PASSWORD` unset → one `PublicKeys` method, no `Password` method.

**Docs**: README.md:42-45. Document `SSH_KEY_PASSPHRASE`.

Commit: `ssh backend: separate key passphrase from login password`.

## Task 10 (S1, S2): don't leak credentials; warn on disabled host checks

1. **Redact URIs in logs**: `ssh_backend.go:131` and `backup/sync.go:39` log
   the raw URI, including `user:password@`. Add
   `func redactURI(s string) string` in `backends/backends.go` (parse with
   `url.Parse`; on success return `u.Redacted()`; on failure return
   `"<unparseable uri>"`, since an unparseable URI may still contain the
   password). Use it at both sites. Grep for other `TargetURI`/`backendURI`
   log sites (`rg -n 'Infof|Debugf|Errorf|Noticef|Warningf' backup cmd | rg -i 'uri|target|destination'`)
   and redact those too.
2. **README**: recommend `SSH_PASSWORD` over the URL form, noting that the URL
   is visible in `ps` and shell history.
3. **Host key ignore**: when `SSH_KNOWN_HOSTS=ignore`, log
   `Warningf("ssh backend: host key checking DISABLED (SSH_KNOWN_HOSTS=ignore)")`.
   When the default `~/.ssh/known_hosts` doesn't exist, wrap the error:
   `fmt.Errorf("ssh backend: known_hosts file %s not found (set SSH_KNOWN_HOSTS): %w", ...)`.

**Tests**: `TestRedactURI` table (`ssh://u:p@h/x` → `ssh://u:xxxxx@h/x`,
`file:///x` unchanged, `s3://bucket/prefix` unchanged, garbage → placeholder).

Commit: `backends: redact credentials in logged URIs; warn when host keys are ignored`.

## Task 11 (S3): restrictive permissions for backup data

`file_backend.go:83, 88`: `MkdirAll(..., os.ModePerm)` (0777) and `os.Create`
(0666). Under umask 022, volumes are world-readable, which matters for backups
taken without `--encryptTo`.

**Fix**: directories `0o700`, files `0o600` (the temp-file `OpenFile` from
Task 7 already uses `0o600`; change `MkdirAll` here). For ssh, after
`MkdirAll` and after creating the temp file, call `sftpClient.Chmod(..., 0o700/0o600)`;
log at Debug and continue if the server rejects chmod.

This changes behavior: existing deployments that read backups as another
user will break. Call it out in the commit message and README.

**Test** `TestFileUploadPermissions`: upload, `os.Stat` the object and its
parent dir, assert `Mode().Perm()` is `0o600`/`0o700`. Skip on Windows.

Commit: `backends(file,ssh): create backup objects 0600 / dirs 0700`.

## Task 12 (S4, N1): path containment and List robustness

1. **Containment**: add `func (f *FileBackend) objectPath(name string) (string, error)`
   that `filepath.Join`s and then verifies, with `filepath.Rel`, that the
   result doesn't start with `..`; return an error otherwise. Use it in
   `Upload`, `Download`, `Delete`. Do the same for ssh with `path.Join` +
   a `strings.HasPrefix(result, root+"/")` check (or `result == root`).
   Object names never legitimately contain `..` segments
   (`files/jobinfo.go` builds them from dataset/snapshot names joined by `|`).
2. **List**:
   - Skip a directory named `.zfs` at the walk root (`return filepath.SkipDir`).
     With `snapdir=visible` the walk otherwise descends into every snapshot.
   - On a per-entry walk error for an entry **other than the root**, log a
     `Warningf` and skip it (`filepath.SkipDir` for dirs, `nil` for files)
     instead of aborting the whole listing. A root error still returns.
     Same for ssh: `w.Err()` → if `w.Path() != root`, warn and
     `w.SkipDir()`/continue.

**Tests**:
- `TestObjectPathRejectsTraversal`: `Download(ctx, "../outside")` and
  `Delete(ctx, "a/../../outside")` return an error, and a sentinel file
  outside the root is untouched.
- `TestFileListSkipsDotZfs`: create `<root>/.zfs/snapshot/s1/obj` and
  `<root>/obj`; `List(ctx, "")` returns only `obj`.
- `TestFileListToleratesUnreadableSubdir`: `chmod 000` a subdir (skip if
  running as root); `List` still returns the other objects.

Commit: `backends(file,ssh): reject path traversal; make List skip .zfs and unreadable entries`.

## Out of scope / follow-ups

- Sweeping stale `*.zfsbackup-partial` files (e.g. from `clean`).
- `List` performance: it walks the whole tree for every prefix query.
  Narrowing the walk to the deepest directory component of `prefix` is a
  straightforward optimization but changes nothing functionally.
- The same ctx-unaware semaphore exists in `gcs_backend.go:106`.
- `fsync@openssh.com` availability varies by server; durability on the ssh
  backend is only as good as the remote server's write-back behavior.

## Verification checklist

- [ ] Every task's new test fails on the parent commit and passes after.
- [ ] `make test-run PKG=./backends/` green after each commit.
- [ ] `make test-run PKG=./backup/` green after Tasks 6, 7 and 10 (they touch
      `backup/sync.go` or `List` semantics used by `syncCache`).
- [ ] `make test-docker` green at the end.
- [ ] `go vet ./...` clean; no new `golangci-lint` findings in touched files.
- [ ] README.md SSH section updated (Tasks 9, 10, 11).
- [ ] Manual smoke test against a real sshd (e.g. the devcontainer, or
      `docker run linuxserver/openssh-server`): send, kill the process
      mid-upload, confirm no truncated object is visible under its final name
      and that a re-run completes.
