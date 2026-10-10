# ZFSBackup

DISCLAIMER: Backups are a high-sensitivity area. Never trust any backup tool without your own due diligence. If you have not tested restores, you do not know if you have good backups.

## Overview

`zfsbackup` stores ZFS snapshots on remote storage for the long term. It splits a `zfs send` stream into volumes, then optionally compresses, encrypts and signs each volume before it uploads it to one or more destinations. The ZFS stream format is committed and a future version of ZFS can receive it, as per the [man page](<https://www.freebsd.org/cgi/man.cgi?zfs(8)>). Each volume is checked with SHA256 and CRC32C checksums, on top of the integrity checks built into the compression formats, TLS, and the ZFS stream itself. Backup jobs survive network failures and can be stopped and resumed.

This project was inspired by the [duplicity project](http://duplicity.nongnu.org/).

This particular repo is a fork of the original at [someone1/zfsbackup-go](https://github.com/someone1/zfsbackup-go).

### Highlights

- Written in Go
- A single static binary. The only host requirement is the `zfs` command, plus any external compressor you select.
- Backup jobs are resumeable and resilient to network failures
- Backup files can be compressed and optionally encrypted and/or signed.
- Concurrent by design, enable multiple cores for parallel processing
- Configurable Operation - Limit bandwidth usage, space usage, CPU usage, etc.
- Backup to multiple destinations at once, just comma separate destination URIs
- Uses familiar ZFS send/receive options
- `plan` and `send --dry-run` show what a run would do before it writes anything

### Documentation

- [Runbook: the first monthly-only offsite backup](docs/runbook-first-backup.md) - keys, planning, the first run, scheduling, and signing-key rotation
- [Architecture](docs/architecture.md) - diagrams of the send and receive pipelines and the data model
- [Component notes](docs/context/README.md) - one page per package, for contributors
- [CHANGELOG](CHANGELOG.md)

### Supported Backends

- Google Cloud Storage (gs://)
  - Auth details: <https://developers.google.com/identity/protocols/application-default-credentials>
  - [99.999999999% durability](https://cloud.google.com/storage/docs/storage-classes) - Using erasure encodings
- Amazon AWS S3 (s3://) (Glacier supported indirectly via lifecycle rules)
  - Auth details: <https://pkg.go.dev/github.com/aws/aws-sdk-go/aws/session#hdr-Environment_Variables>
  - [99.999999999% durability](https://aws.amazon.com/s3/faqs/#data-protection) - Using replication and checksums on the data for integrity validation and repair
  - `AWS_S3_GLACIER_RESTORE_TIER`: `Standard`, `Bulk` or `Expedited`, the restore tier for objects in Glacier (default `Bulk`)
  - `AWS_S3_RESTORE_POLL_INTERVAL`: how often `receive` polls a pending Glacier restore, as a duration such as `30s` (default `1m`)
  - `AWS_S3_ENABLE_DEBUG`: set to `true` to log every request and retry of the AWS SDK. The dump includes the request's `Authorization` header and any `X-Amz-Security-Token`; do not share that log.
- Any S3 Compatible Storage Provider (e.g. Minio, StorageMadeEasy, Ceph, etc.)
  - Set the `AWS_S3_CUSTOM_ENDPOINT` environment variable to the compatible target API URI
- Azure Blob Storage (azure://)
  - Auth: Set the `AZURE_ACCOUNT_NAME` and `AZURE_ACCOUNT_KEY` environment variables to the appropriate values or if using SAS set `AZURE_SAS_URI` to a container authorized SAS URI
  - Point to a custom endpoint by setting the `AZURE_CUSTOM_ENDPOINT` environment variable
  - Although no durability target is provided, there is an in-depth explanation of their architecture [here](http://sigops.org/sosp/sosp11/current/2011-Cascais/printable/11-calder.pdf) - Using the Reed-Solomon erasure encoding and user-configurable redundancy settings
- BackBlaze B2 (b2://)
  - Auth: Set the `B2_ACCOUNT_ID` and `B2_ACCOUNT_KEY` environment variables to the appropriate values
  - [99.999999999% durability](https://help.backblaze.com/hc/en-us/articles/218485257-B2-Resiliency-Durability-and-Availability) - Using the Reed-Solomon erasure encoding
- Local file path (file://[relative|/absolute]/local/path)
- SSH/SFTP (ssh://)
  - Auth: username & password, public key or ssh-agent.
  - For username & password set the `SSH_USERNAME` and `SSH_PASSWORD` environment variables or use the url format: `ssh://username:password@example.org/remote/path`.
  - For public key auth set the `SSH_KEY_FILE` environment variable. By default zfsbackup tries `id_rsa`, `id_ecdsa`, `id_ecdsa_sk`, `id_ed25519`, `id_ed25519_sk` and `id_dsa` in the user's `~/.ssh` directory.
  - ssh-agent auth is activated when `SSH_AUTH_SOCK` exists.
  - By default zfsbackup also uses the known hosts file from the user's home directory. To disable host key checking set `SSH_KNOWN_HOSTS` to `ignore`. You can also specify the path to your own known hosts file.

### Compression

The compression algorithm built into the software is a parallel gzip ([pgzip](https://github.com/klauspost/pgzip)) compressor. There is support for 3rd party compressors so long as the binary is available on the host system and is compatible with the standard gzip binary command line options (e.g. xz, bzip2, lzma, etc.). `--compressor zfs` skips compression in zfsbackup and asks `zfs send -c` for a stream that keeps the dataset's own compression. Manifests always use the internal compressor.

### Encryption/Signing

The PGP algorithm is used for encryption/signing. The cipher used is AES-256.

## Installation

You need Go 1.25 or newer to build. The host that runs `send` or `receive` needs the `zfs` command.

```shell
git clone https://github.com/bosgood/zfsbackup-go.git
cd zfsbackup-go
go build -o zfsbackup .
```

The module path is still the upstream `github.com/someone1/zfsbackup-go`, so `go install github.com/bosgood/zfsbackup-go@latest` does not work. `make build` cross-compiles for several platforms with [gox](https://github.com/mitchellh/gox), which must be in `$GOPATH/bin`.

## Usage

### "Smart" Backup Options

Use the `--full` option to auto select the most recent snapshot on the target volume to do a full backup of:

```bash
./zfsbackup send --encryptTo user@domain.com --signFrom user@domain.com --publicKeyRingPath pubring.gpg.asc --secretKeyRingPath secring.gpg.asc --full Tank/Dataset gs://backup-bucket-target,s3://another-backup-target
```

Use the `--increment` option to auto select the most recent snapshot on the target volume to do an incremental snapshot of the most recent snapshot found in the target destination:

```bash
./zfsbackup send --encryptTo user@domain.com --signFrom user@domain.com --publicKeyRingPath pubring.gpg.asc --secretKeyRingPath secring.gpg.asc --increment Tank/Dataset gs://backup-bucket-target,s3://another-backup-target
```

Use the `--fullIfOlderThan` option to auto select the most recent snapshot on the target volume to do an incremental snapshot of the most recent snapshot found in the target destination, unless the last full backup is older than the provided duration, in which case do a full backup:

```bash
./zfsbackup send --encryptTo user@domain.com --signFrom user@domain.com --publicKeyRingPath pubring.gpg.asc --secretKeyRingPath secring.gpg.asc --fullIfOlderThan 720h Tank/Dataset gs://backup-bucket-target,s3://another-backup-target
```

`--fullSnapshotSuffix` and `--incrementalSnapshotSuffix` restrict the smart options to snapshots whose names end with a suffix, for example sanoid's `_monthly` or `_daily`. This anchors backups on long-lived snapshots that are not pruned before the next run. `--snapshotPrefix` restricts them to snapshots whose names start with a prefix.

### Planning a smart backup

`plan` makes the same decision as a smart `send` and prints what it would back up, and why, without sending anything. With `--schedule` it projects the runs over time as sanoid takes and prunes snapshots. Every plan ends with invariant checks: runs that fail, restore chains, duplicate sends, full cadence and restore depth, plus the opt-in `coverage:<suffix>`. `plan` exits 0 when they pass (a no-op included), 2 when one fails and 1 on other errors.

The examples take monthly fulls and monthly incrementals from sanoid's `_monthly` snapshots. Capture the pool's snapshots on the pool host (or leave out `--snapshots` to list them live):

```bash
zfs list -H -p -t snapshot,bookmark -o name,creation,type -S creation Tank/Dataset > snaps.txt
```

The next run against the real destination (its manifests are read, nothing is sent):

```bash
$ ./zfsbackup plan --fullIfOlderThan 4320h --fullSnapshotSuffix _monthly --incrementalSnapshotSuffix _monthly --snapshots snaps.txt Tank/Dataset gs://backup-bucket-target
next  FULL  autosnap_2026-09-01_00:00:00_monthly  no-previous-full
checks: OK
```

Daily runs until a date, with sanoid's retention policy taking and pruning snapshots in between (`location=` is the time zone of sanoid's snapshot names, this host's by default; `--manifests FILE` stands in for a destination):

```bash
$ ./zfsbackup plan --fullIfOlderThan 4320h --fullSnapshotSuffix _monthly --incrementalSnapshotSuffix _monthly --snapshots snaps.txt \
    --schedule "policy=hourly=48,daily=30,monthly=6,until=2027-10-01,every=24h,checks=coverage:_monthly" Tank/Dataset
2026-09-24T01:00:00Z  FULL  autosnap_2026-09-01_00:00:00_monthly  no-previous-full
2026-09-25T01:00:00Z..2026-09-30T01:00:00Z  NOOP x6  nothing-newer
2026-10-01T01:00:00Z  INCR  autosnap_2026-10-01_00:00:00_monthly  from autosnap_2026-09-01_00:00:00_monthly  newer-candidate
...
checks: OK
```

`--snapshots` also reads `zfs list` output converted to JSON: an array of rows such as `{"name": "Tank/Dataset@autosnap_2026-09-01_00:00:00_monthly"}`, each with an optional `creation` (epoch) and `type`. Other keys are ignored. A row without a creation time takes it from its sanoid name, read in the `location=` zone.

`send -n` (`--dry-run`) then shows the next run's `zfs send` command and size estimate without uploading anything. Scenario directories under `backup/testdata/scenarios` hold the same inputs as files (`flags`, `snapshots.txt` or `snapshots.json`, `manifests.txt`, `schedule`) with the expected output in `expected.txt`. The `prod-navidrome-*` scenarios link to a real pool's snapshots, captured under `testdata/zfs`. `make scenarios` checks them all and `make plan ARGS="..."` runs `plan` from the source tree.

### "Smart" Restore Options

Add `--auto` to let `receive` choose the backups to restore. Give a snapshot to restore to that snapshot, or give a filesystem/volume to restore to its newest backup. `receive` reads the manifests at the destination, links each incremental backup to its parent, and prefers a chain that starts at a snapshot already present on the local volume. When no such chain exists it starts from the full backup. It then restores the chain oldest first. `--auto` cannot be combined with `-i`/`--incremental`.

Note: snapshot comparisons work using the name of the snapshot. If you restored a snapshot to a different name, this application won't think it is available and it will break the restore process.

Auto-detect latest snapshot:

```bash
./zfsbackup receive --encryptTo user@domain.com --signFrom user@domain.com --publicKeyRingPath pubring.gpg.asc --secretKeyRingPath secring.gpg.asc --auto -d Tank/Dataset gs://backup-bucket-target Tank
```

Auto restore to snapshot provided:

```bash
./zfsbackup receive --encryptTo user@domain.com --signFrom user@domain.com --publicKeyRingPath pubring.gpg.asc --secretKeyRingPath secring.gpg.asc --auto -d Tank/Dataset@snapshot-20170201 gs://backup-bucket-target Tank
```

### Manual Options

Full backup example:

```bash
./zfsbackup send --encryptTo user@domain.com --signFrom user@domain.com --publicKeyRingPath pubring.gpg.asc --secretKeyRingPath secring.gpg.asc Tank/Dataset@snapshot-20170101 gs://backup-bucket-target
```

Incremental backup example:

```bash
./zfsbackup send --encryptTo user@domain.com --signFrom user@domain.com --publicKeyRingPath pubring.gpg.asc --secretKeyRingPath secring.gpg.asc -i Tank/Dataset@snapshot-20170101 Tank/Dataset@snapshot-20170201 gs://backup-bucket-target,s3://another-backup-target
```

Full restore example:

```bash
./zfsbackup receive --encryptTo user@domain.com --signFrom user@domain.com --publicKeyRingPath pubring.gpg.asc --secretKeyRingPath secring.gpg.asc -d Tank/Dataset@snapshot-20170201 gs://backup-bucket-target Tank
```

Incremental restore example:

```bash
./zfsbackup receive --encryptTo user@domain.com --signFrom user@domain.com --publicKeyRingPath pubring.gpg.asc --secretKeyRingPath secring.gpg.asc -d -F -i Tank/Dataset@snapshot-20170101 Tank/Dataset@snapshot-20170201 gs://backup-bucket-target Tank
```

### Listing backups

`list` prints the backup sets whose manifests are at a destination. `--volumeName` filters by dataset (a trailing `*` matches a prefix), and `--before` and `--after` filter by backup time:

```bash
./zfsbackup list --encryptTo user@domain.com --signFrom user@domain.com --publicKeyRingPath pubring.gpg.asc --secretKeyRingPath secring.gpg.asc --volumeName 'Tank/*' gs://backup-bucket-target
```

### Cleaning a destination

`clean` deletes backup volumes at a destination that no manifest there lists, such as the volumes of a `send` that was interrupted and never resumed. It never deletes manifests or objects it cannot parse as a backup volume, refuses a destination that has objects but no manifests under `--manifestPrefix`, and leaves alone any dataset a `send` on this host is working on. Run it with `-n`/`--dry-run` first to see what it would delete:

```bash
./zfsbackup clean --dry-run gs://backup-bucket-target
./zfsbackup clean gs://backup-bucket-target
```

- `--force` also deletes broken backup sets at the destination, manifest included, when volumes the manifest lists are missing.
- `--cleanLocal` also deletes cached manifests that are not at the destination, and their volumes at the destination. Those cached manifests are what `send --resume` continues from, so this destroys resume state.

### Exit codes

| Code | Meaning |
|---|---|
| `0` | success, or a smart `send` or `plan` found nothing new to back up |
| `2` | `plan` checks failed, or the last backup at the destination is newer than the pool |
| `1` | any other `plan` error |
| `255` | any other error |

### Notes

- Create keyring files:

```bash
gpg2 --gen-key
gpg2 --output public.pgp --armor --export test@example.com
gpg2 --output private.pgp --armor --export-secret-key test@example.com
```

- PGP Passphrase will be prompted during execution if it is not found in the `PGP_PASSPHRASE` environment variable.
- `receive` runs the external decompressor a backup's manifest names only if it is one of gzip, pigz, bzip2, pbzip2, lbzip2, xz, pxz, lzma, zstd, pzstd, lz4 or lzop; for any other, pass the same name to `receive --compressor` if you trust it.
- `--maxFileBuffer=0` will disable parallel uploading for some backends, multiple destinations, and upload hash verification but will use virtually no disk space.
- `--trustSigner <email|fingerprint>` (repeatable, with `--signFrom`) also accepts manifests and volumes signed by that key from the public keyring, so backups signed before a signing-key rotation still verify. See [Rotating the signing key](docs/runbook-first-backup.md#7-rotating-the-signing-key).
- `receive --maxFileBuffer=0` streams each volume into `zfs receive` before its checksum and signature are checked, so it cannot be combined with `--signFrom`.
- A duration string is a possibly signed sequence of decimal numbers, each with optional fraction and a unit suffix, such as "300ms", "-1.5h" or "2h45m". Valid time units are "ns", "us" (or "µs"), "ms", "s", "m", "h".

### Help Output

`make help CMD=<command>` prints the help below from the working tree.

```shell
$ ./zfsbackup --help
zfsbackup is a tool used to do off-site backups of ZFS volumes.
It leverages the built-in snapshot capabilities of ZFS in order to export ZFS
volumes for long-term storage.

zfsbackup uses the "zfs send" command to export, and optionally compress, sign,
encrypt, and split the send stream to files that are then transferred to a
destination of your choosing.

Usage:
  zfsbackup [command]

Available Commands:
  clean       Clean deletes backup volumes at the destination that no manifest there lists.
  completion  Generate the autocompletion script for the specified shell
  help        Help about any command
  list        List all backup sets found at the provided target.
  plan        plan shows what a smart backup would send, without sending anything.
  receive     receive will restore a snapshot of a ZFS volume similar to how the "zfs recv" command works.
  send        send will backup of a ZFS volume similar to how the "zfs send" command works.
  version     Print the version of zfsbackup in use and relevant compile information

Flags:
      --encryptTo string           the email of the user to encrypt the data to from the provided public keyring.
  -h, --help                       help for zfsbackup
      --jsonOutput                 dump results as a JSON string - on success only
      --logLevel string            this controls the verbosity level of logging. Possible values are critical, error, warning, notice, info, debug. (default "notice")
      --manifestPrefix string      the prefix to use for all manifest files. (default "manifests")
      --numCores int               number of CPU cores to utilize. Do not exceed the number of CPU cores on the system. (default 2)
      --publicKeyRingPath string   the path to the PGP public key ring
      --secretKeyRingPath string   the path to the PGP secret key ring
      --signFrom string            the email of the user to sign on behalf of from the provided private keyring.
      --trustSigner stringArray    also accept manifests and volumes signed by this key (an email or fingerprint in the public keyring), e.g. the key --signFrom named before a rotation. Repeatable; needs --signFrom.
      --workingDirectory string    the working directory path for zfsbackup. (default "~/.zfsbackup")
      --zfsPath string             the path to the zfs executable. (default "zfs")

Use "zfsbackup [command] --help" for more information about a command.
```

`send` options:

```shell
$ ./zfsbackup send --help
send runs "zfs send" for the snapshot given (or the one --full, --increment or
--fullIfOlderThan selects), splits the stream into volumes of --volsize MiB, and
optionally compresses, encrypts and signs each one before uploading it to every
destination URI. A manifest that lists the volumes is uploaded last. Use "receive"
to restore a backup, "list" to see the backups at a destination, and "plan" or
--dry-run to see what a run would send without uploading anything.

Usage:
  zfsbackup send [flags] filesystem|volume|snapshot uri(s)

Flags:
      --compressionLevel int               the compression level to use with the compressor. Valid values are between 1-9. (default 6)
      --compressor string                  specify to use the internal (parallel) gzip implementation or an external binary (e.g. gzip, bzip2, pigz, lzma, xz, etc.) Syntax must be similar to the gzip compression tool) to compress the stream for storage. Please take into consideration time, memory, and CPU usage for any of the compressors used. All manifests utilize the internal compressor. If value is zfs, the zfs stream will be created compressed. See the -c flag on zfs send for more information. (default "internal")
  -D, --deduplication                      See the -D flag for zfs send for more information.
  -n, --dry-run                            Do not upload anything; only validate and log what would be backed up.
      --full                               set this flag to take a full backup of the specified volume using the most recent snapshot.
      --fullIfOlderThan duration           set this flag to do an incremental backup of the most recent snapshot from the most recent snapshot found in the target, unless the last full backup is older than this duration, in which case do a full backup. (default -1m0s)
      --fullSnapshotSuffix string          When set, full backups (including those triggered by fullIfOlderThan) are taken from the newest snapshot whose name ends with this suffix (e.g. "_monthly"). Use this to anchor fulls on long-lived snapshots that outlive the fullIfOlderThan window.
  -h, --help                               help for send
      --increment                          set this flag to do an incremental backup of the most recent snapshot from the most recent snapshot found in the target.
  -i, --incremental string                 See the -i flag on zfs send for more information
      --incrementalSnapshotSuffix string   When set, incremental backups target the newest snapshot whose name ends with this suffix (e.g. "_daily"), ignoring more frequent snapshots (e.g. hourly) that would otherwise be picked and pruned before the next run.
  -I, --intermediary string                See the -I flag on zfs send for more information
      --maxBackoffTime duration            the maximum delay you'd want a worker to sleep before retrying an upload. (default 30m0s)
      --maxFileBuffer int                  the maximum number of files to have active during the upload process. Should be set to at least the number of max parallel uploads. Set to 0 to bypass local storage and upload straight to your destination - this will limit you to a single destination and disable any hash checks for the upload where available. (default 5)
      --maxParallelUploads int             the maximum number of uploads to run in parallel. (default 4)
      --maxRetryTime duration              the maximum time that can elapse when retrying a failed upload. Use 0 for no limit. (default 12h0m0s)
      --maxUploadSpeed uint                the maximum upload speed (in KB/s) the program should use between all upload workers. Use 0 for no limit
  -p, --properties                         See the -p flag on zfs send for more information.
  -w, --raw                                See the -w flag on zfs send for more information.
  -R, --replication                        See the -R flag on zfs send for more information
      --resume                             set this flag to true when you want to try and resume a previously canceled or failed backup. It is up to the caller to ensure the same command line arguments are provided between the original backup and the resumed one.
      --separator string                   the separator to use between object component names. (default "|")
  -s, --skip-missing                       See the -s flag on zfs send for more information
      --snapshotPrefix string              Only consider snapshots starting with the given snapshot prefix
      --uploadChunkSize int                the chunk size, in MiB, to use when uploading. A minimum of 5MiB and maximum of 100MiB is enforced. (default 10)
      --volsize uint                       the maximum size (in MiB) a volume should be before splitting to a new volume. Note: zfsbackup will try its best to stay close/under this limit but it is not guaranteed. (default 200)

Global Flags:
      --encryptTo string           the email of the user to encrypt the data to from the provided public keyring.
      --jsonOutput                 dump results as a JSON string - on success only
      --logLevel string            this controls the verbosity level of logging. Possible values are critical, error, warning, notice, info, debug. (default "notice")
      --manifestPrefix string      the prefix to use for all manifest files. (default "manifests")
      --numCores int               number of CPU cores to utilize. Do not exceed the number of CPU cores on the system. (default 2)
      --publicKeyRingPath string   the path to the PGP public key ring
      --secretKeyRingPath string   the path to the PGP secret key ring
      --signFrom string            the email of the user to sign on behalf of from the provided private keyring.
      --trustSigner stringArray    also accept manifests and volumes signed by this key (an email or fingerprint in the public keyring), e.g. the key --signFrom named before a rotation. Repeatable; needs --signFrom.
      --workingDirectory string    the working directory path for zfsbackup. (default "~/.zfsbackup")
      --zfsPath string             the path to the zfs executable. (default "zfs")
```

`receive` options:

```shell
$ ./zfsbackup receive --help
receive will restore a snapshot of a ZFS volume similar to how the "zfs recv" command works.

Usage:
  zfsbackup receive [flags] filesystem|volume|snapshot-to-restore uri local_volume

Flags:
      --auto                      Automatically restore to the snapshot provided, or to the latest snapshot of the volume provided, from the full backup of that snapshot when there is one; cannot be used with the --incremental flag.
      --compressor string         an external decompressor to run although it is not one of the known ones (gzip, pigz, bzip2, pbzip2, lbzip2, xz, pxz, lzma, zstd, pzstd, lz4, lzop). The backup's manifest names its compressor; a manifest naming any other program is refused unless this flag names the same one.
  -F, --force                     See the -F flag for zfs recv for more information.
  -d, --fullPath                  See the -d flag on zfs recv for more information
  -h, --help                      help for receive
  -i, --incremental string        Used to specify the snapshot target to restore from.
  -e, --lastPath                  See the -e flag for zfs recv for more information.
      --maxBackoffTime duration   the maximum delay you'd want a worker to sleep before retrying an download. (default 30m0s)
      --maxFileBuffer int         the maximum number of volumes to download in parallel and hold on local storage before zfs receive takes them. Each volume's size and SHA-256 are checked against its manifest before zfs receive reads any of it (with --signFrom the manifest's signature is checked first, so this checks the volume against what you signed); the volume's own signature is checked as zfs receive reads it, at its end. Set to 0 to stream each volume straight into zfs receive with no local storage: its bytes reach zfs receive before they are checked, so 0 cannot be used with --signFrom. (default 5)
      --maxRetryTime duration     the maximum time that can elapse when retrying a failed download. Use 0 for no limit. (default 12h0m0s)
  -o, --origin string             See the -o flag on zfs recv for more information.
      --separator string          the separator to use between object component names (used only for the initial manifest we are looking for). (default "|")
  -u, --unmounted                 See the -u flag for zfs recv for more information.

Global Flags:
      --encryptTo string           the email of the user to encrypt the data to from the provided public keyring.
      --jsonOutput                 dump results as a JSON string - on success only
      --logLevel string            this controls the verbosity level of logging. Possible values are critical, error, warning, notice, info, debug. (default "notice")
      --manifestPrefix string      the prefix to use for all manifest files. (default "manifests")
      --numCores int               number of CPU cores to utilize. Do not exceed the number of CPU cores on the system. (default 2)
      --publicKeyRingPath string   the path to the PGP public key ring
      --secretKeyRingPath string   the path to the PGP secret key ring
      --signFrom string            the email of the user to sign on behalf of from the provided private keyring.
      --trustSigner stringArray    also accept manifests and volumes signed by this key (an email or fingerprint in the public keyring), e.g. the key --signFrom named before a rotation. Repeatable; needs --signFrom.
      --workingDirectory string    the working directory path for zfsbackup. (default "~/.zfsbackup")
      --zfsPath string             the path to the zfs executable. (default "zfs")
```

`list` options:

```shell
$ ./zfsbackup list --help
List all backup sets found at the provided target.

Usage:
  zfsbackup list [flags] uri

Flags:
      --after string        Filter results to only this backups after this specified date & time (format: yyyy-MM-ddTHH:mm:ss, parsed in local TZ)
      --before string       Filter results to only this backups before this specified date & time (format: yyyy-MM-ddTHH:mm:ss, parsed in local TZ)
  -h, --help                help for list
      --volumeName string   Filter results to only this volume name, can end with a '*' to match as only a prefix

Global Flags:
      --encryptTo string           the email of the user to encrypt the data to from the provided public keyring.
      --jsonOutput                 dump results as a JSON string - on success only
      --logLevel string            this controls the verbosity level of logging. Possible values are critical, error, warning, notice, info, debug. (default "notice")
      --manifestPrefix string      the prefix to use for all manifest files. (default "manifests")
      --numCores int               number of CPU cores to utilize. Do not exceed the number of CPU cores on the system. (default 2)
      --publicKeyRingPath string   the path to the PGP public key ring
      --secretKeyRingPath string   the path to the PGP secret key ring
      --signFrom string            the email of the user to sign on behalf of from the provided private keyring.
      --trustSigner stringArray    also accept manifests and volumes signed by this key (an email or fingerprint in the public keyring), e.g. the key --signFrom named before a rotation. Repeatable; needs --signFrom.
      --workingDirectory string    the working directory path for zfsbackup. (default "~/.zfsbackup")
      --zfsPath string             the path to the zfs executable. (default "zfs")
```

`clean` options:

```shell
$ ./zfsbackup clean --help
Clean deletes backup volumes at the destination that no manifest lists, for the datasets
that have manifests there. It never deletes manifests or objects it cannot parse as a
backup volume, refuses a destination with objects but no manifests under
--manifestPrefix, and leaves alone any dataset a send on this host is working on.

--force also deletes broken backup sets (manifest included) at the destination whose
volumes are missing. It leaves cached manifests that are not at the destination alone.
--cleanLocal also deletes cached manifests that are not at the destination, and their
volumes at the destination; those manifests are what --resume continues from. Cached
manifests of another --manifestPrefix are never deleted.

Usage:
  zfsbackup clean [flags] uri

Flags:
      --cleanLocal   Delete cached manifests that are not at the destination, and delete their volumes at the destination.
  -n, --dry-run      Do not delete anything; only log what would be deleted.
      --force        Also delete broken backup sets at the destination (sets where volumes expected in the manifest file are not found), manifest included. Cached manifests that are not at the destination are left alone; see --cleanLocal. Use with caution.
  -h, --help         help for clean

Global Flags:
      --encryptTo string           the email of the user to encrypt the data to from the provided public keyring.
      --jsonOutput                 dump results as a JSON string - on success only
      --logLevel string            this controls the verbosity level of logging. Possible values are critical, error, warning, notice, info, debug. (default "notice")
      --manifestPrefix string      the prefix to use for all manifest files. (default "manifests")
      --numCores int               number of CPU cores to utilize. Do not exceed the number of CPU cores on the system. (default 2)
      --publicKeyRingPath string   the path to the PGP public key ring
      --secretKeyRingPath string   the path to the PGP secret key ring
      --signFrom string            the email of the user to sign on behalf of from the provided private keyring.
      --trustSigner stringArray    also accept manifests and volumes signed by this key (an email or fingerprint in the public keyring), e.g. the key --signFrom named before a rotation. Repeatable; needs --signFrom.
      --workingDirectory string    the working directory path for zfsbackup. (default "~/.zfsbackup")
      --zfsPath string             the path to the zfs executable. (default "zfs")
```

`plan` options:

```shell
$ ./zfsbackup plan --help
plan runs the smart backup decision of "send" (--full, --increment or
--fullIfOlderThan, with --snapshotPrefix and the snapshot suffixes) and prints
what the next run would back up and why. With --schedule it projects the runs
over time as sanoid takes and prunes snapshots. The output ends with invariant
checks (restore chains, duplicate sends, full cadence, ...): plan exits 0 when
they pass, 2 when any fails, and 1 on other errors.

Snapshots come from the pool (zfs list, see --zfsPath) unless --snapshots
names a listing. Destination URIs are read for the backups already there,
syncing the local manifest cache; --manifests lists them in a file instead.
Without either, the destination is empty.

Text output matches the scenario goldens (backup/testdata/scenarios), so
"plan ... > expected.txt" records a new one.

Usage:
  zfsbackup plan [flags] volume [uri(s)]

Flags:
      --full                               set this flag to take a full backup of the specified volume using the most recent snapshot.
      --fullIfOlderThan duration           set this flag to do an incremental backup of the most recent snapshot from the most recent snapshot found in the target, unless the last full backup is older than this duration, in which case do a full backup. (default -1m0s)
      --fullSnapshotSuffix string          When set, full backups (including those triggered by fullIfOlderThan) are taken from the newest snapshot whose name ends with this suffix (e.g. "_monthly"). Use this to anchor fulls on long-lived snapshots that outlive the fullIfOlderThan window.
  -h, --help                               help for plan
      --increment                          set this flag to do an incremental backup of the most recent snapshot from the most recent snapshot found in the target.
      --incrementalSnapshotSuffix string   When set, incremental backups target the newest snapshot whose name ends with this suffix (e.g. "_daily"), ignoring more frequent snapshots (e.g. hourly) that would otherwise be picked and pruned before the next run.
      --manifests string                   read the backups already at the destinations from this file instead of destination URIs: one "<base>" (full) or "<source> to <base>" (incremental) per line, with "---" between destinations.
      --resume                             plan as "send --resume" would: complete a backup set missing at some destinations even where its volumes are not all there.
      --schedule string                    project runs over time, e.g. "policy=hourly=36,daily=30,monthly=3,until=2027-10-01T00:00:00Z,every=24h". Also from= (default: an hour after the newest snapshot), snapshot-delay= (how long after a boundary sanoid takes its snapshots, e.g. 3m), skip=<from>..<until> (no runs in that range; repeatable), checks=coverage:_monthly, and location= for sanoid names in a time zone other than this host's (e.g. location=UTC).
      --snapshotPrefix string              Only consider snapshots starting with the given snapshot prefix
      --snapshots string                   read the snapshots from this file (- for stdin) instead of the pool: the output of "zfs list -H -p -t snapshot,bookmark -o name,creation,type -S creation <volume>", bare sanoid names, or a JSON array of rows with a name and optionally a creation epoch and a type.

Global Flags:
      --encryptTo string           the email of the user to encrypt the data to from the provided public keyring.
      --jsonOutput                 dump results as a JSON string - on success only
      --logLevel string            this controls the verbosity level of logging. Possible values are critical, error, warning, notice, info, debug. (default "notice")
      --manifestPrefix string      the prefix to use for all manifest files. (default "manifests")
      --numCores int               number of CPU cores to utilize. Do not exceed the number of CPU cores on the system. (default 2)
      --publicKeyRingPath string   the path to the PGP public key ring
      --secretKeyRingPath string   the path to the PGP secret key ring
      --signFrom string            the email of the user to sign on behalf of from the provided private keyring.
      --trustSigner stringArray    also accept manifests and volumes signed by this key (an email or fingerprint in the public keyring), e.g. the key --signFrom named before a rotation. Repeatable; needs --signFrom.
      --workingDirectory string    the working directory path for zfsbackup. (default "~/.zfsbackup")
      --zfsPath string             the path to the zfs executable. (default "zfs")
```
