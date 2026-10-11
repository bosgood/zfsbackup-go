#!/bin/sh
# Writes a destination with an older zfsbackup binary, for TestE2EOldDestinationReads.
# Usage: make-fixture.sh <old zfsbackup binary> <output dir>
# Run it through `make fixture-old REV=<rev>`: the binary is a Linux build from `make build-rev`.
set -eu
old=$1
out=$2

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
shim=$tmp/zfs
export SHIM_LOG=$tmp/shim.log

# zfs shim: one dataset, two snapshots, a fixed stream. Logs every call.
# 1788220800 is 2026-09-01T00:00:00Z; 1790899200 is 2026-10-02T00:00:00Z.
cat > "$shim" <<'SHIM'
#!/bin/sh
echo "$@" >> "$SHIM_LOG"
case "$1" in
  list) printf 'tank/data@b\t1790899200\tsnapshot\ntank/data@a\t1788220800\tsnapshot\n' ;;
  get)  case "$*" in *@b) echo 1790899200 ;; *) echo 1788220800 ;; esac ;;
  send) head -c 65536 /dev/zero ;;
  *)    exit 1 ;;
esac
SHIM
chmod +x "$shim"

rm -rf "$out"
mkdir -p "$out" "$tmp/wd"
if ! "$old" send --zfsPath "$shim" --workingDirectory "$tmp/wd" --volsize 1 tank/data@a "file://$out" ||
	! "$old" send --zfsPath "$shim" --workingDirectory "$tmp/wd" --volsize 1 -i tank/data@a tank/data@b "file://$out"; then
	echo "make-fixture: a send failed; the shim saw:" >&2
	cat "$SHIM_LOG" >&2
	exit 1
fi
echo "make-fixture: the shim saw:"
cat "$SHIM_LOG"
