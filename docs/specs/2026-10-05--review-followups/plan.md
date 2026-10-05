# Review follow-ups (2026-10-05) Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Fix the findings of the 2026-10-05 review of the destructive-op fixes: the local
cache moving under canonical URIs, legacy-layout data the Init check misses, `clean`
deleting what is not its own (nested destinations, running sends), and the fragile
name/separator/index handling around resume.

**Architecture:** The CLI keeps the destination spelling the user typed next to the
canonical one (`JobInfo.DestinationsAsTyped`). `getCacheDir` uses it to adopt cache
directories keyed by older spellings; object-store `Init` uses it to probe the key
prefixes older versions really wrote. `clean` gains three guards: it skips subtrees that
hold their own manifests, skips datasets whose send lock is held, and warns about
legacy-layout volumes it cannot reach. `Backup` pairs each backend with its URI in one
`destination` value instead of two parallel slices.

**Tech Stack:** Go 1.25, vendored deps, `nightlyone/lockfile`, in-process e2e tests
(`e2e_*_test.go`, fake zfs) and the fake S3 endpoint in `clean_s3_test.go`.

Line numbers are as of `f9e19a0` on `clean-dry-run`; re-check them before editing.

---

## Ground rules

- Branch `clean-dry-run`. One commit per task. Commit messages follow the existing style
  (`send: ...`, `clean: ...`, `backends: ...`).
- TDD: each task names a test that must fail on the unfixed code. Run it, see it fail,
  then fix. A test that passes first time is wrong (napkin, 2026-10-02).
- Fast loop: `make test-run PKG=./backup/ RUN='TestName'` (also `./backends/`, `./files/`,
  and `PKG=.` for the root e2e tests). After an in-process cobra test, run the whole
  package (`make test-run PKG=.`), not just the one test.
- Before declaring the plan done: `make test-docker` (about 3 minutes).
- Tests in this repo still use `io/ioutil`; match the file you are editing.

## Decisions made in this plan (review these first)

1. **Legacy volumes without a manifest are reported, not deleted, and only by `clean`.**
   They live outside the destination's prefix (`ptank/data|...` for `s3://b/p/`), and such
   a key cannot be told apart from a sibling destination `s3://b/ptank/` backing up pool
   `data`. `clean` logs a warning naming the key. It finds them only for datasets it
   knows from manifests, including the cached partial manifest of the interrupted send
   (which Task 5 carries over). A host with no cache and no manifest for that dataset
   stays blind.
2. **`clean` skips a dataset whose send lock is held; it does not fail.** The lock is a
   file in `os.TempDir()`, so this protects against a send on the same host only. A
   send from another host can still lose in-flight volumes; the runbook says so.
3. **A subtree holding its own manifests is another destination; `clean` skips all of
   it**, including volumes that might be this destination's own orphans. Never deleting
   an ambiguous object is the point.
4. **Logs drop the whole `user[:password]@` part of a URI.** `ssh://user:pw@host/path`
   is a supported spelling (`backends/ssh_backend.go:148`), so today's logs can print a
   password.

## Shared background

- Cache layout: `<workingDirectory>/cache/<md5(destination URI)>/<md5(manifest object name)>`.
  The md5 of the URI is computed in three places: `backup/sync.go:62` (`getCacheDir`),
  `backup/backup.go:642` (`saveManifest`) and `backup/backup.go:852` (`tryResume`).
- `backends.CanonicalURI` (`backends/backends.go:178`) is applied at the CLI
  (`cmd/root.go:433` `parseDestinations`, `cmd/clean.go:41`, `cmd/list.go:64`), so nothing
  below `cmd` ever sees what the user typed. `s3://b/` becomes `s3://b`; `s3://b/p`
  becomes `s3://b/p/`; `s3://b//p` becomes `s3://b/p/`.
- Versions before `b6c6dc6` used everything after `<bucket>/` **verbatim** as the key
  prefix (`strings.Join(uriParts[1:], "/")` in all four object-store backends). So
  `s3://b/p` wrote `pmanifests|...`, `s3://b//p` wrote `/pmanifests|...`, and
  `s3://b/p//` wrote `p//manifests|...`. Only `s3://b/p/` and `s3://b/` wrote today's layout.
- Who loses their cache today: users of `s3://b/` (layout unchanged, cache dir renamed,
  nothing warns them) and users of a legacy-layout spelling once they have moved their
  objects as Init tells them to.
- `JobInfo` fields tagged `json:"-"` are never in a manifest (`files/jobinfo.go:62-94`).
- `clean` cannot take a mock backend; use `file://` (`backup/clean_test.go`,
  `setupCleanTest`) or the fake S3 (`clean_s3_test.go`, `newFakeS3`, `backupSet`, `cleanS3`).
- `lockfile.TryLock` succeeds when the lock file names our own pid. To simulate another
  process, write the parent's pid into the file (`e2e_resend_test.go:125-129`).

---

### Task 1: gofmt

**Files:**

- Modify: `backup/backup.go:973` (double blank line after `verifiedVolumes`)
- Modify: `files/jobinfo_test.go:21` (double blank line before `package`)
- Modify: `backup/backup_test.go` (comment alignment on `const off`)
- Modify: `Makefile`

`make fmt` loops over `$(DIRS)`, which is not defined, so it checks nothing.

**Step 1:** Add a target after `fmt:` in `Makefile`:

```make
# Lists files gofmt would change (vendor/ and the tmp/ scratch clones excluded); fails if any.
fmt-check:
	@out="`git ls-files '*.go' | grep -v '^vendor/' | xargs gofmt -l`"; \
	if [ -n "$$out" ]; then echo "$$out"; echo "^ not gofmt-clean" && exit 1; fi
```

**Step 2:** Run `make fmt-check`. Expected: FAIL listing `backup/backup.go`,
`backup/backup_test.go`, `files/jobinfo_test.go`.

**Step 3:** `gofmt -w backup/backup.go backup/backup_test.go files/jobinfo_test.go`

**Step 4:** `make fmt-check` prints nothing and exits 0. Run it again at the end of every
later task.

**Step 5:** Commit: `gofmt: backup.go, two test files; add make fmt-check`.

---

### Task 2: one separator constant, one volume-name prefix

**Files:**

- Modify: `files/jobinfo.go:228-236`
- Modify: `files/jobinfo_test.go`
- Modify: `backup/backup.go:570`, `backup/backup.go:929`
- Modify: `backup/clean.go:39,147,155`
- Modify: `cmd/send.go:152,194`, `cmd/receive.go:129,148`

**Step 1: failing test** in `files/jobinfo_test.go`:

```go
// The prefix must be exactly the name minus the volume number: resume and manifest repair
// list a set's volumes by it.
func TestBackupVolumeObjectPrefix(t *testing.T) {
	j := &JobInfo{
		VolumeName:          "tank/data",
		BaseSnapshot:        SnapshotInfo{Name: "b"},
		IncrementalSnapshot: SnapshotInfo{Name: "a"},
		Separator:           DefaultSeparator,
		Compressor:          InternalCompressor,
	}
	prefix := j.BackupVolumeObjectPrefix()
	for _, n := range []int64{1, 10, 123} {
		name := j.BackupVolumeObjectName(n)
		if name != prefix+strconv.FormatInt(n, 10) {
			t.Errorf("BackupVolumeObjectName(%d) = %q, want prefix %q + number", n, name, prefix)
		}
		if _, _, _, got, ok := ParseBackupVolumeObjectName(name, DefaultSeparator); !ok || got != n {
			t.Errorf("ParseBackupVolumeObjectName(%q) = volume %d, %v", name, got, ok)
		}
	}
}
```

**Step 2:** `make test-run PKG=./files/ RUN=TestBackupVolumeObjectPrefix` fails to compile
(`DefaultSeparator`, `BackupVolumeObjectPrefix` undefined).

**Step 3: implement** in `files/jobinfo.go`, replacing `BackupVolumeObjectName`:

```go
// DefaultSeparator is the default for --separator.
const DefaultSeparator = "|"

// BackupVolumeObjectPrefix is the object name of every volume of this backup set up to,
// but not including, the volume number. Listing by it returns the set's volumes and nothing else.
func (j *JobInfo) BackupVolumeObjectPrefix() string {
	extensions := []string{"zstream"}

	nameParts, ext := j.volumeNameParts(false)
	extensions = append(extensions, ext...)
	extensions = append(extensions, "vol")

	return fmt.Sprintf("%s.%s", strings.Join(nameParts, j.Separator), strings.Join(extensions, "."))
}

func (j *JobInfo) BackupVolumeObjectName(volumeNumber int64) string {
	return j.BackupVolumeObjectPrefix() + strconv.FormatInt(volumeNumber, 10)
}
```

Then:

- `backup/backup.go:570` and `:929`: replace
  `strings.TrimSuffix(<j>.BackupVolumeObjectName(0), "0")` with `<j>.BackupVolumeObjectPrefix()`.
- `backup/clean.go`: delete `const defaultSeparator`, use `files.DefaultSeparator`.
- `cmd/send.go:152,194` and `cmd/receive.go:129,148`: replace the `"|"` literal with
  `files.DefaultSeparator`. Leave test files alone.

**Step 4:** `make test-run PKG=./files/` and `make test-run PKG=./backup/ RUN='TestClean'`
pass; `go build ./...` is clean.

**Step 5:** Commit: `files: BackupVolumeObjectPrefix and DefaultSeparator replace string surgery and copies`.

---

### Task 3: pair each backend with its URI; fix swapped names; keep credentials out of logs

**Files:**

- Modify: `backends/backends.go`, `backends/backends_test.go`
- Modify: `backends/ssh_backend.go:131`
- Modify: `backup/backup.go` (`Backup` 253-305 and 378-395, `refuseExistingSet` 510,
  `copyManifest` 553, `tryResume` 832-893, `verifiedVolumes` 922)
- Modify: `backup/clean.go`, `backup/list.go`, `backup/restore.go` (log lines only)

**Step 1: failing test** in `backends/backends_test.go`:

```go
func TestRedactURI(t *testing.T) {
	testCases := map[string]string{
		"ssh://user:secret@host:22/path": "ssh://host:22/path",
		"ssh://user@host/path":           "ssh://host/path",
		"ssh://host/path":                "ssh://host/path",
		"s3://bucket/p@q/":               "s3://bucket/p@q/",
		"s3://bucket":                    "s3://bucket",
		"file:///tmp/dest":               "file:///tmp/dest",
		"delete://":                      "delete://",
		"notauri":                        "notauri",
	}
	for in, want := range testCases {
		if got := RedactURI(in); got != want {
			t.Errorf("RedactURI(%q) = %q, want %q", in, got, want)
		}
	}
}
```

**Step 2:** `make test-run PKG=./backends/ RUN=TestRedactURI` fails to compile.

**Step 3: implement.** In `backends/backends.go`, after `CanonicalURI`:

```go
// RedactURI drops the userinfo ("user[:password]@") from a URI, for logs and error messages.
func RedactURI(uri string) string {
	scheme, rest, ok := strings.Cut(uri, "://")
	if !ok {
		return uri
	}
	authority, path, hasPath := strings.Cut(rest, "/")
	if at := strings.LastIndex(authority, "@"); at >= 0 {
		authority = authority[at+1:]
	}
	if !hasPath {
		return scheme + "://" + authority
	}
	return scheme + "://" + authority + "/" + path
}
```

`backends/ssh_backend.go:131`: log `RedactURI(s.conf.TargetURI)`.

In `backup/backup.go`, above `Backup`:

```go
// destination is one place a backup set is sent to: its URI and the backend initialized for it.
// Keeping them in one value means a backend can never be reported, or cached, under another's URI.
type destination struct {
	uri     string
	backend backends.Backend
}

// String is the URI as it may appear in logs.
func (d destination) String() string {
	return backends.RedactURI(d.uri)
}
```

Then, in `Backup`:

- `var usedBackends []backends.Backend` becomes `var dests []destination`; the deferred
  close loop calls `d.backend.Close()`.
- The init loop appends `destination{uri: uri, backend: backend}`.
- `refuseExistingSet(ctx, jobInfo, dests)` and `tryResume(ctx, jobInfo, dests)`.
- The `delete://` block (378-389) appends to `dests` (and still to
  `jobInfo.Destinations`, which `saveManifest` reads).
- The plumbing loop becomes:

```go
	for _, d := range dests {
		out, waitgroup := retryUploadChainer(ctx, channels[len(channels)-1], d.backend, jobInfo, d.uri)
```

`refuseExistingSet`, `copyManifest`, `tryResume`, `verifiedVolumes` take
`destinations []destination`. Inside them replace `destinations[i]` (a backend) with
`destinations[i].backend`, and every `jobInfo.Destinations[i]` / `j.Destinations[i]`
that is formatted into a log line or error with `destinations[i]` (the `%s` verb then
prints the redacted URI through `String`). `copyManifest` line 600 keeps the real URI for
the scheme: `strings.Split(destinations[idx].uri, "://")[0]`.

After this, `grep -n 'Destinations\[' backup/backup.go` must show no index expression
inside those four functions.

Swapped names, `backup/backup.go:888-889`:

```go
		oldCMDLine := strings.Join(oldCMD.Args, " ")
		currentCMDLine := strings.Join(currentCMD.Args, " ")
```

(and make the message read ``original `%s` != current `%s` `` to match the lines above it).
No test: the function only logs, and the returned error is the fixed string `option mismatch`.

Sweep the remaining destination URIs in log and error text. Find them with:

```bash
grep -n -E 'target|destination|Destinations' backup/*.go | grep -E 'log\.AppLogger|fmt\.Errorf' | grep -v _test
```

Wrap each formatted URI in `backends.RedactURI(...)`. In `backup/clean.go` the two
`filepath.Join(target, obj)` log arguments become
`filepath.Join(backends.RedactURI(target), obj)`. Do not touch arguments that are passed
to code (`prepareBackend`, `getCacheDir`).

**Step 4:** `make test-run PKG=./backends/ RUN='TestRedactURI|TestCanonicalURI'`,
`make test-run PKG=./backup/`, `make test-run PKG=. RUN='TestE2E'` pass. The e2e tests
assert on log text (`already exists`, `missing at`); they use `file://` URIs, which
`RedactURI` leaves unchanged.

**Step 5:** Commit: `send: one destination value per backend; redact URI credentials in logs; fix swapped resume-mismatch names`.

---

### Task 4: carry the typed spelling of each destination

Tasks 5 and 6 both need what the user typed.

**Files:**

- Modify: `files/jobinfo.go:84`
- Modify: `cmd/root.go:431-439`, `cmd/send.go:211`, `cmd/receive.go:178`,
  `cmd/plan.go:120,160`, `cmd/clean.go:41`, `cmd/list.go:64`
- Modify: `backends/backends.go:61-70`, `backup/sync.go:38-48`
- Test: `cmd/root_test.go` (create if absent, `package cmd`)

**Step 1: failing test:**

```go
func TestParseDestinationsKeepsTypedSpelling(t *testing.T) {
	dests, typed := parseDestinations("s3://b//p,s3://c/,file:///tmp/d")
	want := []string{"s3://b/p/", "s3://c", "file:///tmp/d"}
	if !reflect.DeepEqual(dests, want) {
		t.Errorf("destinations = %q, want %q", dests, want)
	}
	wantTyped := map[string]string{"s3://b/p/": "s3://b//p", "s3://c": "s3://c/", "file:///tmp/d": "file:///tmp/d"}
	if !reflect.DeepEqual(typed, wantTyped) {
		t.Errorf("typed = %q, want %q", typed, wantTyped)
	}
}
```

**Step 2:** `make test-run PKG=./cmd/ RUN=TestParseDestinationsKeepsTypedSpelling` fails
to compile (one return value).

**Step 3: implement.**

`files/jobinfo.go`, next to `Destinations`:

```go
	// DestinationsAsTyped maps each of Destinations (canonical URIs) to the spelling given on
	// the command line. Older versions keyed the local cache and the object key prefix by it.
	DestinationsAsTyped map[string]string `json:"-"`
```

`cmd/root.go`:

```go
// parseDestinations splits a comma-separated destination argument and canonicalizes each URI,
// so that e.g. s3://bucket/prefix and s3://bucket/prefix/ name the same destination and cache.
// typed maps each canonical URI back to the spelling given.
func parseDestinations(arg string) (destinations []string, typed map[string]string) {
	destinations = strings.Split(arg, ",")
	typed = make(map[string]string, len(destinations))
	for idx, raw := range destinations {
		destinations[idx] = backends.CanonicalURI(raw)
		typed[destinations[idx]] = raw
	}
	return destinations, typed
}
```

Callers:

- `cmd/send.go:211`, `cmd/receive.go:178`:
  `jobInfo.Destinations, jobInfo.DestinationsAsTyped = parseDestinations(args[1])`
- `cmd/clean.go:41`, `cmd/list.go:64`:
  `jobInfo.Destinations, jobInfo.DestinationsAsTyped = parseDestinations(args[0])`
  (a URI containing a comma was never valid for these; behavior for one URI is unchanged)
- `cmd/plan.go:120,160`: read how each loop uses the destination. Where it builds or
  fills a `JobInfo` that reaches `backup` code, set `DestinationsAsTyped` on it too;
  otherwise `destinations, _ := parseDestinations(args[1])`.

`backends/backends.go`, in `BackendConfig`:

```go
	// TypedURI is TargetURI as the user spelled it, before CanonicalURI. Object-store backends
	// derive from it the key prefix an older version would have used (see legacyPrefixes).
	// Empty means the same as TargetURI.
	TypedURI string
```

`backup/sync.go` `prepareBackend`: add `TypedURI: j.DestinationsAsTyped[backendURI],` to
the config literal (a nil map reads as "").

**Step 4:** `go build ./...`; `make test-run PKG=./cmd/`; `make test-run PKG=.` pass.

**Step 5:** Commit: `cmd: keep the typed spelling of each destination next to the canonical one`.

---

### Task 5: adopt cache directories keyed by older spellings

**Files:**

- Modify: `backends/backends.go`, `backends/backends_test.go`
- Modify: `backup/sync.go:60-70`
- Modify: `backup/backup.go:155,269,640-643,851-853`, `backup/clean.go:69`,
  `backup/list.go:57`, `backup/restore.go:68,209`
- Create: `backup/sync_test.go`
- Modify: `clean_s3_test.go`

**Step 1: failing unit tests.**

`backends/backends_test.go`:

```go
func TestLegacySpellings(t *testing.T) {
	testCases := []struct {
		canonical, typed string
		want             []string
	}{
		{"s3://b", "s3://b/", []string{"s3://b/"}},
		{"s3://b", "s3://b", []string{"s3://b/"}},
		{"s3://b/p/", "s3://b/p", []string{"s3://b/p"}},
		{"s3://b/p/", "", []string{"s3://b/p"}},
		{"s3://b/p/", "s3://b//p", []string{"s3://b/p", "s3://b//p"}},
		{"gs://b/p/", "gs://b/p//", []string{"gs://b/p", "gs://b/p//"}},
		{"file:///tmp/d", "file:///tmp/d", nil},
		{"ssh://h/path", "", nil},
	}
	for _, c := range testCases {
		if got := LegacySpellings(c.canonical, c.typed); !reflect.DeepEqual(got, c.want) {
			t.Errorf("LegacySpellings(%q, %q) = %q, want %q", c.canonical, c.typed, got, c.want)
		}
	}
}
```

`backup/sync_test.go` (copy the license header from `backup/sync.go`):

```go
package backup

import (
	"crypto/md5" // nolint:gosec // MD5 not used for cryptographic purposes here
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/someone1/zfsbackup-go/config"
	"github.com/someone1/zfsbackup-go/files"
)

func cacheDirOf(uri string) string {
	// nolint:gosec // MD5 not used for cryptographic purposes here
	return filepath.Join(config.WorkingDir, "cache", fmt.Sprintf("%x", md5.Sum([]byte(uri))))
}

func writeCacheFile(t *testing.T, uri, name, content string) {
	t.Helper()
	if err := os.MkdirAll(cacheDirOf(uri), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cacheDirOf(uri), name), []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
}

// Before URIs were canonicalized the cache was keyed by the URI as typed. Manifests cached
// under such a spelling (the resume state of an interrupted send, among others) must follow
// the destination to its canonical cache directory.
func TestGetCacheDirAdoptsLegacySpellings(t *testing.T) {
	testCases := []struct {
		name, canonical, typed, legacy string
	}{
		{"bucket root typed with a slash", "s3://b", "s3://b/", "s3://b/"},
		{"bucket root, slash typed only before the upgrade", "s3://b", "s3://b", "s3://b/"},
		{"prefix typed without a slash", "s3://b/p/", "s3://b/p", "s3://b/p"},
		{"doubled slash", "s3://b/p/", "s3://b//p", "s3://b//p"},
	}
	for _, c := range testCases {
		t.Run(c.name, func(t *testing.T) {
			oldWorkingDir := config.WorkingDir
			config.WorkingDir = t.TempDir()
			t.Cleanup(func() { config.WorkingDir = oldWorkingDir })

			writeCacheFile(t, c.legacy, "only-legacy", "legacy")
			writeCacheFile(t, c.legacy, "both", "legacy")
			writeCacheFile(t, c.canonical, "both", "canonical")

			j := &files.JobInfo{DestinationsAsTyped: map[string]string{c.canonical: c.typed}}
			dir, err := getCacheDir(j, c.canonical)
			if err != nil {
				t.Fatal(err)
			}
			if dir != cacheDirOf(c.canonical) {
				t.Errorf("cache dir = %s, want %s", dir, cacheDirOf(c.canonical))
			}
			if got, _ := os.ReadFile(filepath.Join(dir, "only-legacy")); string(got) != "legacy" {
				t.Errorf("legacy-only manifest was not adopted (content %q)", got)
			}
			// The canonical copy was synced from the destination or written since the upgrade.
			if got, _ := os.ReadFile(filepath.Join(dir, "both")); string(got) != "canonical" {
				t.Errorf("canonical manifest was replaced by the legacy copy (content %q)", got)
			}
			if _, serr := os.Stat(cacheDirOf(c.legacy)); !os.IsNotExist(serr) {
				t.Errorf("legacy cache dir still exists (stat: %v)", serr)
			}
			// Idempotent.
			if _, err = getCacheDir(j, c.canonical); err != nil {
				t.Errorf("second getCacheDir: %v", err)
			}
		})
	}
}
```

**Step 2:** `make test-run PKG=./backends/ RUN=TestLegacySpellings` and
`make test-run PKG=./backup/ RUN=TestGetCacheDirAdoptsLegacySpellings` fail to compile.

**Step 3: implement.**

`backends/backends.go`, after `CanonicalURI`:

```go
// LegacySpellings returns the other spellings of a canonical destination URI under which an
// older version, which keyed the local cache by the URI as typed, may have cached manifests:
// the object-store URI with its trailing slash toggled, and the spelling typed now.
func LegacySpellings(canonical, typed string) []string {
	var spellings []string
	if scheme, rest, ok := strings.Cut(canonical, "://"); ok && objectStoreSchemes[scheme] {
		if strings.HasSuffix(rest, "/") {
			spellings = append(spellings, strings.TrimSuffix(canonical, "/"))
		} else {
			spellings = append(spellings, canonical+"/")
		}
	}
	if typed != "" && typed != canonical && (len(spellings) == 0 || spellings[0] != typed) {
		spellings = append(spellings, typed)
	}
	return spellings
}
```

`backup/sync.go`, replacing `getCacheDir`:

```go
// cacheDirFor is where the manifests of the destination backendURI are cached.
func cacheDirFor(backendURI string) string {
	// nolint:gosec // MD5 not used for cryptographic purposes here
	return filepath.Join(config.WorkingDir, "cache", fmt.Sprintf("%x", md5.Sum([]byte(backendURI))))
}

// getCacheDir creates the cache directory of backendURI (a canonical URI) and first moves into
// it whatever an older version cached under another spelling of the same destination.
func getCacheDir(j *files.JobInfo, backendURI string) (string, error) {
	dest := cacheDirFor(backendURI)
	oerr := os.MkdirAll(dest, os.ModePerm)
	if oerr != nil {
		return "", fmt.Errorf("could not create cache directory %s due to an error: %v", dest, oerr)
	}

	for _, spelling := range backends.LegacySpellings(backendURI, j.DestinationsAsTyped[backendURI]) {
		if err := adoptCacheDir(cacheDirFor(spelling), dest); err != nil {
			return "", fmt.Errorf("could not move the cache of %s to %s due to an error: %v", backends.RedactURI(spelling), dest, err)
		}
	}

	return dest, nil
}

// adoptCacheDir moves the cached manifests in from to to and removes from. A manifest cached in
// both keeps to's copy: it was synced from the destination or written since the upgrade.
func adoptCacheDir(from, to string) error {
	entries, err := os.ReadDir(from)
	if os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return err
	}
	moved := 0
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		src, dst := filepath.Join(from, entry.Name()), filepath.Join(to, entry.Name())
		if _, serr := os.Stat(dst); serr == nil {
			if err = os.Remove(src); err != nil {
				return err
			}
			continue
		}
		if err = os.Rename(src, dst); err != nil {
			return err
		}
		moved++
	}
	log.AppLogger.Noticef("Moved %d cached manifests from %s to %s: the cache is now keyed by the canonical URI.", moved, from, to)
	if err = os.Remove(from); err != nil {
		log.AppLogger.Debugf("Could not remove the old cache directory %s: %v", from, err)
	}
	return nil
}
```

Update the six `getCacheDir` callers to pass their `*files.JobInfo` first
(`backup/backup.go:155,269`, `backup/clean.go:69`, `backup/list.go:57`,
`backup/restore.go:68,209`).

Remove the two other md5-of-URI copies:

- `backup/backup.go:640-643` (`saveManifest`):
  `dest := filepath.Join(cacheDirFor(destination), safeManifestFile)`
- `backup/backup.go:851-853` (`tryResume`):
  `origManiPath := filepath.Join(cacheDirFor(destination), safeManifestFile)`

`Backup` calls `getCacheDir` for every destination (line 269) before `tryResume`, so the
partial manifest is in place by the time resume reads it.

**Step 4:** both unit tests pass.

**Step 5: end-to-end regression** in `clean_s3_test.go`. First two helper refactors:

- `backupSet(t, prefix, volume)` becomes a wrapper around a new
  `backupSetAt(t, prefix, volume, snapshot string)` holding the current body, with
  `BaseSnapshot: files.SnapshotInfo{Name: snapshot}`.
- `cleanS3(t, args...)` becomes a wrapper around a new `cleanS3In(t, work string, args ...string)`
  that passes `--workingDirectory work`.

Then:

```go
// TestCleanS3KeepsCacheAcrossCanonicalization: an interrupted send to s3://bucket/ left its
// volume at the destination and its partial manifest only in the cache, which the old version
// keyed by the URI as typed. clean must still find that manifest and keep the volume.
func TestCleanS3KeepsCacheAcrossCanonicalization(t *testing.T) {
	objects := backupSet(t, "", "tank/data")
	work := t.TempDir()
	// nolint:gosec // MD5 not used for cryptographic purposes here
	cacheDir := filepath.Join(work, "cache", fmt.Sprintf("%x", md5.Sum([]byte("s3://bucket/"))))
	if err := os.MkdirAll(cacheDir, 0700); err != nil {
		t.Fatal(err)
	}
	for name, data := range backupSetAt(t, "", "tank/data", "autosnap_2026-10-01_00:00:00_monthly") {
		if !strings.HasPrefix(name, "manifests|") {
			objects[name] = data
			continue
		}
		// nolint:gosec // MD5 not used for cryptographic purposes here
		cached := filepath.Join(cacheDir, fmt.Sprintf("%x", md5.Sum([]byte(name))))
		if err := ioutil.WriteFile(cached, data, 0600); err != nil {
			t.Fatal(err)
		}
	}

	for _, uri := range []string{"s3://bucket/", "s3://bucket"} {
		fake := newFakeS3(t, objects)
		logs, err := cleanS3In(t, work, "--dry-run=true", uri)
		if err != nil {
			t.Fatalf("clean %s: %v\n%s", uri, err, logs)
		}
		if strings.Contains(logs, "Would delete") || len(fake.deleted) != 0 {
			t.Errorf("clean %s would delete the interrupted send's volume\n%s", uri, logs)
		}
	}
}
```

Verify it against the old code: `git stash` the `backup/sync.go` change (or check the test
into a scratch copy per the napkin) and confirm it fails with `Would delete`. Then run
`make test-run PKG=. RUN='TestCleanS3'`: PASS.

**Step 6:** Commit: `cache: adopt manifests cached under a pre-canonical spelling of the destination`.

---

### Task 6: the legacy-layout check probes the prefix older versions really used

**Files:**

- Modify: `backends/backends.go:153-173`, `backends/backends_test.go`
- Modify: `backends/aws_s3_backend.go:105,150`, `backends/gcs_backend.go:66,89`,
  `backends/azure_backend.go:69,124`, `backends/backblaze_b2_backend.go:62,99`
- Modify: `clean_s3_test.go` (`TestCleanS3Prefixes` table)

**Step 1: failing tests.**

`backends/backends_test.go`:

```go
func TestLegacyPrefixes(t *testing.T) {
	testCases := []struct {
		uri, typed string
		want       []string
	}{
		{"s3://b/p/", "", []string{"p"}},
		{"s3://b/p/", "s3://b/p/", []string{"p"}},
		{"s3://b/p/", "s3://b/p", []string{"p"}},
		{"s3://b/p/", "s3://b//p", []string{"p", "/p"}},
		{"s3://b/p/", "s3://b/p//", []string{"p", "p//"}},
		{"s3://b/p/q/", "s3://b/p/q", []string{"p/q"}},
		{"s3://b", "s3://b/", nil},
		{"s3://b", "s3://b//", []string{"/"}},
	}
	for _, c := range testCases {
		if got := legacyPrefixes(c.uri, c.typed, AWSS3BackendPrefix); !reflect.DeepEqual(got, c.want) {
			t.Errorf("legacyPrefixes(%q, %q) = %q, want %q", c.uri, c.typed, got, c.want)
		}
	}
}
```

`clean_s3_test.go`, new `TestCleanS3Prefixes` case:

```go
		{
			// s3://bucket//p used to write "/pmanifests|...", not "pmanifests|...".
			name:    "legacy layout behind a doubled slash",
			objects: backupSet(t, "/p", "tank/data"),
			args:    []string{"s3://bucket//p"},
			errText: "/pmanifests|tank/data|",
		},
```

**Step 2:** the unit test fails to compile; the clean case fails (clean succeeds with
nothing to do).

**Step 3: implement** in `backends/backends.go`, replacing `checkLegacyLayout`:

```go
// legacyPrefixes returns the key prefixes under which a version from before prefixes were
// normalized could have written the destination uri (canonical): the path with no trailing
// slash ("p" for s3://b/p/), and the path exactly as typed when that is something else again
// ("/p" for s3://b//p). Those versions used everything after "<bucket>/" verbatim.
func legacyPrefixes(uri, typedURI, scheme string) []string {
	_, rawPrefix, prefix, err := parseObjectURI(uri, scheme)
	if err != nil {
		return nil
	}
	var legacy []string
	add := func(old string) {
		if old == "" || old == prefix {
			return
		}
		for _, have := range legacy {
			if have == old {
				return
			}
		}
		legacy = append(legacy, old)
	}
	add(rawPrefix)
	if rest := strings.TrimPrefix(typedURI, scheme+"://"); rest != typedURI {
		_, typedPath, _ := strings.Cut(rest, "/")
		add(typedPath)
	}
	return legacy
}

// checkLegacyLayout fails when the destination holds manifests written before object-store
// prefixes were normalized, i.e. at "<legacy prefix><ManifestPrefix>..." for one of legacy
// (see legacyPrefixes). firstKey must return the first key in the bucket starting with the
// given (bucket-absolute) prefix, or "" if there is none.
func checkLegacyLayout(conf *BackendConfig, legacy []string, prefix string, firstKey func(prefix string) (string, error)) error {
	if conf.ManifestPrefix == "" {
		return nil
	}
	for _, old := range legacy {
		key, err := firstKey(old + conf.ManifestPrefix)
		if err != nil {
			return err
		}
		if key == "" {
			continue
		}
		return fmt.Errorf(
			"found %s, a backup written by an older version that used the URI path %q as a raw key prefix; "+
				"move every object starting with %q to %q and retry",
			key, old, old, prefix,
		)
	}
	return nil
}
```

In each of the four backends' `Init`:

- `bucket, _, prefix, err := parseObjectURI(...)` (the raw prefix is now only used inside
  `legacyPrefixes`).
- Add a field `legacy []string` to the backend struct and set
  `x.legacy = legacyPrefixes(conf.TargetURI, conf.TypedURI, <SchemeConst>)` right after
  the prefix (Task 7 reads it).
- The final call becomes `checkLegacyLayout(conf, x.legacy, prefix, func(keyPrefix string) ...)`
  with the closure body unchanged.

Check the existing `TestCleanS3Prefixes` "legacy layout" case still passes: its expected
text `pmanifests|tank/data|` is the found key, which the message still prints first. If a
test asserts on `under p/`, update it to the new wording.

**Step 4:** `make test-run PKG=./backends/ RUN='TestLegacyPrefixes|TestParseObjectURI'`,
`make test-run PKG=. RUN='TestCleanS3'` pass.

**Step 5:** Commit: `backends: the legacy-layout check probes the key prefix as it was typed`.

---

### Task 7: clean reports legacy-layout volumes that have no manifest

**Files:**

- Modify: `backends/backends.go`, `backends/backends_test.go`
- Modify: the four object-store backends
- Modify: `backup/clean.go` (after the `datasets` map is built, about line 152)
- Modify: `clean_s3_test.go`

**Step 1: failing tests.**

`backends/backends_test.go`:

```go
func TestFindLegacyVolume(t *testing.T) {
	keys := []string{"p/tank/data|a.zstream.gz.vol1", "ptank/data|old.zstream.gz.vol1", "ptank/other|x.txt"}
	firstKey := func(prefix string) (string, error) {
		for _, k := range keys {
			if strings.HasPrefix(k, prefix) {
				return k, nil
			}
		}
		return "", nil
	}
	got, err := findLegacyVolume([]string{"p"}, []string{"tank/data"}, []string{"|"}, firstKey)
	if err != nil || got != "ptank/data|old.zstream.gz.vol1" {
		t.Errorf("findLegacyVolume = %q, %v", got, err)
	}
	// Not a volume name, and a dataset nobody asked about: nothing to report.
	for _, dataset := range []string{"tank/other", "tank/missing"} {
		if got, err = findLegacyVolume([]string{"p"}, []string{dataset}, []string{"|", ""}, firstKey); err != nil || got != "" {
			t.Errorf("findLegacyVolume(%s) = %q, %v; want nothing", dataset, got, err)
		}
	}
}
```

`clean_s3_test.go`, new `TestCleanS3Prefixes` case:

```go
		{
			// An old full to s3://bucket/p, interrupted before its manifest: "p" + volume name.
			name: "legacy volume without a manifest",
			objects: merge(backupSet(t, "p/", "tank/data"), map[string][]byte{
				"ptank/data|autosnap_2026-08-01_00:00:00_monthly.zstream.gz.vol1": []byte("volume"),
			}),
			args:    []string{"s3://bucket/p/"},
			logText: "looks like a backup volume written by an older version",
		},
```

Before relying on that literal key, print `backupSet`'s volume key once and make the
extension (`.zstream.gz.vol1` here) match what `BackupVolumeObjectName` produces.

**Step 2:** unit test fails to compile; the clean case fails (log lacks the text).

**Step 3: implement.**

`backends/backends.go`:

```go
// LegacyVolumeFinder is implemented by the object-store backends. FindLegacyVolume returns a key
// that looks like a volume of one of datasets written under a pre-normalization key prefix
// (see legacyPrefixes), or "". Such volumes are outside the destination's prefix: List never
// returns them and clean cannot delete them.
type LegacyVolumeFinder interface {
	FindLegacyVolume(ctx context.Context, datasets, separators []string) (string, error)
}

// findLegacyVolume probes "<legacy prefix><dataset><separator>" for every combination and
// returns the first key found that parses as a backup volume once the legacy prefix is removed.
func findLegacyVolume(legacy, datasets, separators []string, firstKey func(prefix string) (string, error)) (string, error) {
	for _, old := range legacy {
		for _, dataset := range datasets {
			for _, sep := range separators {
				if sep == "" {
					continue
				}
				key, err := firstKey(old + dataset + sep)
				if err != nil {
					return "", err
				}
				if key == "" {
					continue
				}
				if _, _, _, _, ok := files.ParseBackupVolumeObjectName(key[len(old):], sep); ok {
					return key, nil
				}
			}
		}
	}
	return "", nil
}
```

In each object-store backend, turn the `checkLegacyLayout` closure into a method and add
the finder. S3:

```go
// firstKey returns the first key in the bucket that starts with keyPrefix, or "".
func (a *AWSS3Backend) firstKey(ctx context.Context, keyPrefix string) (string, error) {
	resp, lerr := a.client.ListObjectsV2WithContext(ctx, &s3.ListObjectsV2Input{
		Bucket:  aws.String(a.bucketName),
		MaxKeys: aws.Int64(1),
		Prefix:  aws.String(keyPrefix),
	})
	if lerr != nil || len(resp.Contents) == 0 {
		return "", lerr
	}
	return aws.StringValue(resp.Contents[0].Key), nil
}

// FindLegacyVolume implements LegacyVolumeFinder.
func (a *AWSS3Backend) FindLegacyVolume(ctx context.Context, datasets, separators []string) (string, error) {
	return findLegacyVolume(a.legacy, datasets, separators, func(p string) (string, error) { return a.firstKey(ctx, p) })
}
```

and `Init` ends with
`return checkLegacyLayout(conf, a.legacy, prefix, func(p string) (string, error) { return a.firstKey(ctx, p) })`.
GCS, Azure and B2 get the same two methods, with the bodies of their existing closures
(`gcs_backend.go:89-97`, `azure_backend.go:124-133`, `backblaze_b2_backend.go:99-105`).

`backup/clean.go`, right after the loop that fills `separators` and `datasets`:

```go
	// Volumes an older version wrote under a raw key prefix are outside this destination: say so,
	// since nothing here will ever list or delete them.
	if finder, ok := backend.(backends.LegacyVolumeFinder); ok {
		names := make([]string, 0, len(datasets))
		for dataset := range datasets {
			names = append(names, dataset)
		}
		sort.Strings(names)
		key, ferr := finder.FindLegacyVolume(ctx, names, separators)
		if ferr != nil {
			log.AppLogger.Errorf("Could not check %s for volumes in the old key layout due to error - %v", backends.RedactURI(target), ferr)
			return ferr
		}
		if key != "" {
			log.AppLogger.Warningf(
				"Found %s, which looks like a backup volume written by an older version outside of %s. "+
					"clean cannot delete it. If no manifest lists it (an interrupted send), delete it and the volumes next to it by hand.",
				key, backends.RedactURI(target),
			)
		}
	}
```

(`backup/clean.go` needs the `backends` and `sort` imports.) `separators` holds
duplicates; that only costs repeated probes, so dedupe it where it is built:

```go
	separators := []string{files.DefaultSeparator}
	addSeparator := func(sep string) {
		for _, have := range separators {
			if have == sep {
				return
			}
		}
		separators = append(separators, sep)
	}
	addSeparator(jobInfo.Separator)
	// ... in the manifests loop: addSeparator(manifest.Separator)
```

The "sibling prefix" case of `TestCleanS3Prefixes` asserts which prefixes were listed. It
has no manifests at `media/`, so no dataset probes happen; confirm it still passes.

**Step 4:** `make test-run PKG=./backends/ RUN=TestFindLegacyVolume`,
`make test-run PKG=. RUN='TestCleanS3'`, `make test-run PKG=./backup/ RUN=TestClean` pass.

**Step 5:** Commit: `clean: warn about old-layout volumes that no manifest lists`.

---

### Task 8: clean leaves nested destinations alone

**Files:**

- Modify: `backup/clean.go` (candidate loop, lines 157-177)
- Modify: `clean_s3_test.go`

The collision: `s3://b` backs up `tank/data`; `s3://b/tank/` backs up pool `data`. The
second writes `tank/data|<snap>.zstream.gz.vol1`, which from the bucket root parses as a
volume of `tank/data`. No manifest at the root lists it, so root `clean` deletes it. The
tell is the nested destination's manifest, visible from the root as `tank/manifests|data|...`.

**Step 1: failing test**, new `TestCleanS3Prefixes` case:

```go
		{
			// s3://bucket/tank/ backs up pool "data": its volume keys are "tank/data|...",
			// which from the bucket root parse as volumes of tank/data.
			name: "nested destination named like a dataset",
			objects: merge(
				backupSet(t, "", "tank/data"),
				backupSetAt(t, "tank/", "data", "autosnap_2026-10-01_00:00:00_monthly"),
			),
			args:    []string{"s3://bucket"},
			logText: "is another destination",
		},
```

**Step 2:** `make test-run PKG=. RUN=TestCleanS3Prefixes` fails: `clean ... deleted` /
`Would delete tank/data|autosnap_2026-10-01...`.

**Step 3: implement** in `backup/clean.go`. Add:

```go
// nestedDestinations returns "<dir>/" for every listed object that is the manifest of another
// destination nested below this one ("<dir>/<manifestPrefix>...manifest..."). Objects under such
// a directory belong to that destination, whatever their names parse as from here.
func nestedDestinations(objects []string, manifestPrefix string) []string {
	var roots []string
	for _, obj := range objects {
		if strings.HasPrefix(obj, manifestPrefix) {
			continue
		}
		idx := strings.Index(obj, "/"+manifestPrefix)
		if idx < 0 || !strings.Contains(obj[idx:], ".manifest") {
			continue
		}
		roots = append(roots, obj[:idx+1])
	}
	return roots
}
```

and in `Clean`, before the candidate loop:

```go
	nested := nestedDestinations(allObjects, manifestPrefix)
```

then inside the loop, after the own-manifest `continue` and before `parseBackupVolume`:

```go
		if root := nestedUnder(obj, nested); root != "" {
			log.AppLogger.Noticef("Skipping %s: %s holds its own manifests, so it is another destination.", obj, root)
			skipped++
			continue
		}
```

with

```go
func nestedUnder(obj string, roots []string) string {
	for _, root := range roots {
		if strings.HasPrefix(obj, root) {
			return root
		}
	}
	return ""
}
```

The existing case "bucket root with a nested destination" expects the log
`no manifest at this destination is for dataset p/tank/data`; that object is now skipped
earlier with the new message. Change that case's `logText` to `is another destination`.

**Step 4:** `make test-run PKG=. RUN='TestCleanS3'` and
`make test-run PKG=./backup/ RUN=TestClean` pass.

**Step 5:** Commit: `clean: a subtree with its own manifests is another destination`.

---

### Task 9: clean stays away from a running send

**Files:**

- Modify: `backup/backup.go:275-297`
- Modify: `backup/clean.go`
- Modify: `backup/clean_test.go`

**Step 1: extract the lock helper** (no behavior change) in `backup/backup.go`:

```go
// volumeLock is the lock a send holds while it works on volume. clean takes it too, so that it
// never judges the volumes of a set that is still being uploaded.
func volumeLock(volume string) (lock lockfile.Lockfile, path string, err error) {
	// nolint:gosec // MD5 not used for cryptographic purposes
	path = filepath.Join(os.TempDir(), fmt.Sprintf("zfsbackup.%x.lck", md5.Sum([]byte(volume))))
	lock, err = lockfile.New(path)
	return lock, path, err
}
```

`Backup` uses it: `lock, lockFilePath, lferr := volumeLock(jobInfo.VolumeName)`. Run
`make test-run PKG=. RUN='TestE2EChecksExistingSetUnderLock|TestE2EUploadFailureExits'`: PASS.

**Step 2: failing test** in `backup/clean_test.go`:

```go
// TestCleanSkipsDatasetOfRunningSend: a running send has uploaded volumes that no manifest
// lists yet. While its lock is held, clean must not treat them as orphans.
func TestCleanSkipsDatasetOfRunningSend(t *testing.T) {
	targetDir, jobInfo := setupCleanTest(t)

	set := &files.JobInfo{
		VolumeName:     "tank/data",
		BaseSnapshot:   files.SnapshotInfo{Name: "a"},
		ManifestPrefix: "manifests",
		Separator:      "|",
		Compressor:     files.InternalCompressor,
	}
	live := set.BackupVolumeObjectName(1)
	set.Volumes = []*files.VolumeInfo{{ObjectName: live, VolumeNumber: 1}}
	writeTestManifest(t, targetDir, set)
	writeTestObject(t, targetDir, live)
	inFlight := writeTestObject(t, targetDir, set.BackupVolumeObjectName(2))

	// Another live process (our parent) holds the send lock.
	_, lockPath, err := volumeLock("tank/data")
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(lockPath, []byte(fmt.Sprintf("%d\n", os.Getppid())), 0600); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(lockPath)

	if cerr := Clean(context.Background(), jobInfo, false, false); cerr != nil {
		t.Fatalf("Clean returned error - %v", cerr)
	}
	if _, serr := os.Stat(inFlight); serr != nil {
		t.Fatalf("clean deleted a volume of a dataset whose send lock is held - %v", serr)
	}

	// Once the send is done, the same volume is an orphan again.
	if err = os.Remove(lockPath); err != nil {
		t.Fatal(err)
	}
	if cerr := Clean(context.Background(), jobInfo, false, false); cerr != nil {
		t.Fatalf("Clean returned error - %v", cerr)
	}
	if _, serr := os.Stat(inFlight); !os.IsNotExist(serr) {
		t.Fatalf("expected the orphan to be deleted once the lock is free, got stat error - %v", serr)
	}
	if _, serr := os.Stat(lockPath); !os.IsNotExist(serr) {
		t.Errorf("clean left its lock file behind (stat: %v)", serr)
	}
}
```

**Step 3:** `make test-run PKG=./backup/ RUN=TestCleanSkipsDatasetOfRunningSend` fails at
`clean deleted a volume of a dataset whose send lock is held`.

**Step 4: implement** in `Clean`, after `datasets` is complete and before the candidate loop:

```go
	// A running send has volumes at the destination that its cached manifest does not list yet.
	// Hold each dataset's send lock for the rest of the run; where a send holds it, leave that
	// dataset alone. This only sees sends on this host: the lock is a local file.
	busy := make(map[string]bool)
	for dataset := range datasets {
		lock, lockPath, lerr := volumeLock(dataset)
		if lerr != nil {
			log.AppLogger.Errorf("Cannot init lock for %s. reason: %v", dataset, lerr)
			return lerr
		}
		if lerr = lock.TryLock(); lerr != nil {
			log.AppLogger.Noticef(
				"A send of %s appears to be running (%s: %v); leaving its volumes alone. Run clean again when it is done.",
				dataset, lockPath, lerr,
			)
			busy[dataset] = true
			continue
		}
		defer func() {
			if uerr := lock.Unlock(); uerr != nil {
				log.AppLogger.Warningf("Could not release lock %s: %v", lockPath, uerr)
			}
		}()
	}
```

In the candidate loop, after the `!datasets[dataset]` check:

```go
		if busy[dataset] {
			continue
		}
```

In the manifest loop (`for _, manifest := range decodedManifests {`), first line:

```go
		// Not judged at all, so --force cannot remove a set a running send is still completing.
		if busy[manifest.VolumeName] {
			continue
		}
```

Any `TryLock` error counts as busy: declining to delete is the safe direction, and the
notice prints the reason.

**Step 5:** the new test passes; `make test-run PKG=./backup/ RUN=TestClean` and
`make test-run PKG=.` pass.

**Step 6:** Fix the comment on `verifiedVolumes` (`backup/backup.go:918-921`): it says
volumes past the resume point are "removed by clean once the manifest lands". Append:
"clean skips this dataset while a send on this host holds its lock."

**Step 7:** Commit: `clean: take each dataset's send lock; skip datasets a send is working on`.

---

### Task 10: no goroutine parked on the volume counter after a failed send

**Files:**

- Modify: `backup/backup.go:337-338,350-368,422,440-451`
- Modify: `backup/backup_test.go`
- Modify: `e2e_failure_test.go`

`maniwg` is a `sync.WaitGroup`; the final-manifest goroutine waits on it through a helper
goroutine (`backup.go:445-448`) that never returns when the pipeline fails.

**Step 1: failing tests.**

`backup/backup_test.go`:

```go
func TestPendingVolumes(t *testing.T) {
	p := newPendingVolumes(1)
	p.add()
	p.done()
	select {
	case <-p.zero:
		t.Fatal("zero closed with one volume still pending")
	default:
	}
	p.done()
	select {
	case <-p.zero:
	default:
		t.Fatal("zero not closed at count 0")
	}
	// A straggler after a failed pipeline must not panic.
	p.done()
}
```

`e2e_failure_test.go`:

```go
// TestE2EFailedSendLeavesNoWaiter: a failed send must not leave a goroutine parked waiting for
// volumes that will never finish. Harmless for one CLI run, but in-process runs add up.
func TestE2EFailedSendLeavesNoWaiter(t *testing.T) {
	env := newE2EEnv(t)
	env.writeSnapshots(t, "tank/data", []files.SnapshotInfo{{Name: "a", CreationTime: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)}})
	if err := ioutil.WriteFile(filepath.Join(env.dest, "tank"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	logs, err := guarded(t, func() (string, error) {
		return env.send("--maxRetryTime", "2s", "--maxBackoffTime", "1s", "tank/data@a", "file://"+env.dest)
	})
	if err == nil {
		t.Fatalf("send succeeded although no volume could be uploaded:\n%s", logs)
	}
	parked := func() string {
		for _, g := range strings.Split(backupGoroutines(), "\n\n") {
			if strings.Contains(g, "backup.Backup.") && strings.Contains(g, "sync.(*WaitGroup).Wait") {
				return g
			}
		}
		return ""
	}
	deadline := time.Now().Add(5 * time.Second)
	for parked() != "" && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if g := parked(); g != "" {
		t.Errorf("a failed send left a goroutine waiting on the volume counter:\n%s", g)
	}
}
```

**Step 2:** the unit test fails to compile. Run the e2e test alone
(`make test-run PKG=. RUN=TestE2EFailedSendLeavesNoWaiter`): it must FAIL on the current
code. If it passes, print `backupGoroutines()` after the failed send and adjust the two
substrings to match the leaked goroutine's frames before going on.

**Step 3: implement** in `backup/backup.go`:

```go
// pendingVolumes counts the volumes still in the pipeline. zero is closed when the count first
// reaches 0. Unlike a sync.WaitGroup it can be waited on in a select, so nothing stays parked
// on it when the pipeline fails and the count never gets there.
type pendingVolumes struct {
	mu   sync.Mutex
	n    int
	zero chan struct{}
	once sync.Once
}

func newPendingVolumes(n int) *pendingVolumes {
	return &pendingVolumes{n: n, zero: make(chan struct{})}
}

func (p *pendingVolumes) add() {
	p.mu.Lock()
	p.n++
	p.mu.Unlock()
}

func (p *pendingVolumes) done() {
	p.mu.Lock()
	p.n--
	n := p.n
	p.mu.Unlock()
	if n == 0 {
		p.once.Do(func() { close(p.zero) })
	}
}
```

In `Backup`:

- `var maniwg sync.WaitGroup; maniwg.Add(1)` becomes `pending := newPendingVolumes(1)`
  (the 1 is the forwarding goroutine, as before).
- `defer maniwg.Done()` → `defer pending.done()`; `maniwg.Add(1)` → `pending.add()`;
  `maniwg.Done()` (line 422) → `pending.done()`.
- The final-manifest goroutine's wait becomes:

```go
		select {
		case <-pending.zero:
		case <-ctx.Done():
			return ctx.Err()
		}
		// The forwarder also counts down when it stops on a cancelled ctx, so both cases can be
		// ready at once; a failed pipeline must never get as far as a final manifest.
		if err := ctx.Err(); err != nil {
			return err
		}
```

and the `allUploaded` channel and its helper goroutine are deleted. Update the comment
above it (`maniwg may never reach zero` → `the count may never reach zero`).

The `ctx.Err()` guard is not from the review. Reading the code, the forwarder's deferred
count-down on cancellation can bring the count to zero while `ctx` is done, and `select`
then picks at random; I have not reproduced a bad outcome from it.

**Step 4:** `make test-run PKG=./backup/ RUN=TestPendingVolumes` and
`make test-run PKG=.` pass (the whole root package: the hang tests exercise this path).

**Step 5:** Commit: `send: a failed send leaves no goroutine waiting on the volume counter`.

---

### Task 11: runbook

**Files:**

- Modify: `docs/runbook-first-backup.md:102-111,138-148`

**Step 1:** Line 103-104. Replace

```
delete them at the destination **and** their cached copies under
`<workingDirectory>/cache/<md5 of the URI>/`.
```

with

```
delete them at the destination **and** their cached copies under
`<workingDirectory>/cache/<md5 of the canonical URI>/`. The canonical URI is
what the logs print: an object-store prefix always ends in `/`
(`s3://bucket/prefix/`) and a bucket root never does (`s3://bucket`), however
you typed it. A cache directory left by an older version under another
spelling is moved there on the next run (`Moved N cached manifests ...`).
```

**Step 2:** The bullet list at 138-148. The last bullet runs into the exit-status
paragraph. End the bullet and start a paragraph:

```
- resume against a `zfs send` stream shorter than the cached manifest records.

The exit status is 0 both when it uploads a backup and when there is nothing new
(`Nothing new to back up.`, the normal case on most runs), so any other status
is a real failure.
```

**Step 3:** In the `clean` paragraph of section 3 (after "it lists those as skipped"), add:

```
It also skips everything under a directory that holds its own manifests
(another destination nested below this one), and every dataset that a `send`
on this host is working on right now (`A send of ... appears to be running`).
It cannot see a `send` running on another host: do not run `clean` against a
destination while another machine is sending to it. If it warns that a key
`looks like a backup volume written by an older version`, that volume sits
outside the destination's prefix and has to be deleted by hand.
```

**Step 4:** Re-read section 3 and 5 rendered (`glow` or a Markdown preview) to check the
list ends where intended.

**Step 5:** Commit: `docs: runbook for the canonical cache, clean's new guards, and a list that ran on`.

---

### Task 12: verify the whole plan

1. `make fmt-check`: no output.
2. `go build ./... && go vet ./backup/ ./backends/ ./files/ ./cmd/ .`
3. `make test-docker`: all packages `ok`.
4. `grep -rn 'TrimSuffix(.*BackupVolumeObjectName' --include='*.go' backup cmd files` and
   `grep -rn 'defaultSeparator' --include='*.go' .`: no hits outside `vendor/`.
5. Update `.claude/napkin.md` "clean facts": clean now takes the per-dataset send lock
   (same host only), skips nested destinations, and the cache is keyed by the canonical
   URI with legacy spellings adopted in `getCacheDir`.

## Not in this plan

- A `send` on another host racing `clean`. That needs a lock at the destination.
- Deleting legacy-layout volumes automatically (decision 1).
- `receive` hanging on a missing volume, and the open WARNINGs from the 2026-10-05
  adversarial review.
