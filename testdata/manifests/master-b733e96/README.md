# Destination written by `master` (b733e96)

The version before the `clean-dry-run` branch wrote these objects. `TestE2EOldDestinationReads`
(root package) reads them back with the current code. Never regenerate them to match new code:
fix the reader.

Made with `make fixture-old REV=master` (`make build-rev` builds b733e96 in `golang:1.25-bookworm`,
then `testdata/manifests/make-fixture.sh` runs it):

```sh
zfsbackup send --zfsPath $shim --workingDirectory $wd --volsize 1 tank/data@a file://$out
zfsbackup send --zfsPath $shim --workingDirectory $wd --volsize 1 -i tank/data@a tank/data@b file://$out
```

The `zfs` shim lists `tank/data@a` created 1788220800 (2026-09-01T00:00:00Z) and `tank/data@b`
created 1790899200 (2026-10-02T00:00:00Z), and sends 65536 zero bytes for every `zfs send`.
Default compressor (gzip), no encryption, no signature, default `--separator` and
`--manifestPrefix`.
