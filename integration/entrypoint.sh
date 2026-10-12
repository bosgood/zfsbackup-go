#!/bin/bash
# Entrypoint of the `integration` image: make a pool on a file, run the
# integration test against it, destroy the pool.
#
# A privileged container sees the HOST's pools (pool names are global in the
# kernel). So this script refuses a pool name that already exists and
# destroys only the pool it made.
set -euo pipefail

POOL=${ZFSBACKUP_TEST_POOL:-zbtest}
export ZFSBACKUP_TEST_POOL=$POOL

# A NixOS host mounts /nix/store and names its own userland here, so zfs and
# the module are one build (integration/compose.nixos.yaml).
if [ -n "${ZFS_USERLAND:-}" ]; then
	export PATH="$ZFS_USERLAND:$PATH"
fi
echo "entrypoint: zfs userland $(command -v zfs): $(zfs version 2>&1 | tr '\n' ' ')"

if [ ! -e /dev/zfs ]; then
	echo "entrypoint: no /dev/zfs: the host kernel has no zfs module (see integration/README.md)" >&2
	exit 1
fi

if zpool list -H -o name | grep -qx -e "$POOL"; then
	echo "entrypoint: pool $POOL already exists; it may be the host's. Export or destroy it by hand, or set ZFSBACKUP_TEST_POOL." >&2
	exit 1
fi

# The zfs module opens a file vdev by its path in the HOST's mount namespace,
# not this container's. compose.yaml bind-mounts this directory at the same
# path on both sides; a path only the container has fails with "cannot
# create 'zbtest': no such pool or dataset".
IMG=/var/tmp/zfsbackup-it/$POOL.img
truncate -s 2G "$IMG"
zpool create -f -O mountpoint="/$POOL" -O compression=off "$POOL" "$IMG"
# Without this the pool stays imported in the host kernel after the container
# exits, and the next run stops at the check above.
trap 'zpool destroy -f "$POOL"; rm -f "$IMG"' EXIT

if [ -n "${AWS_S3_CUSTOM_ENDPOINT:-}" ]; then
	for _ in $(seq 30); do
		curl -sf "$AWS_S3_CUSTOM_ENDPOINT/minio/health/live" >/dev/null && break
		sleep 1
	done
fi

# No exec: the trap must run.
cd /src
status=0
zfsbackup-integration.test -test.v -test.count=1 -test.timeout 45m -test.run "${RUN:-TestIntegrationS3}" || status=$?
exit "$status"
