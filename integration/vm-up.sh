#!/bin/bash
# Create, start and set up the NixOS Lima VM of make integration-vm.
# Usage: integration/vm-up.sh <instance name>
#
# The first start applies integration/nixos/ (zfs, Docker, make) with
# nixos-rebuild and restarts the VM into the new kernel. Later starts find
# /dev/zfs and only start the VM.
set -euo pipefail

VM=${1:?usage: vm-up.sh <instance name>}
DIR=$(cd "$(dirname "$0")" && pwd)

command -v limactl >/dev/null || { echo "integration-vm: install Lima first: brew install lima" >&2; exit 1; }

if ! limactl list -q | grep -qx -e "$VM"; then
	limactl create --name "$VM" --tty=false "$DIR/lima.yaml"
fi
if [ "$(limactl list --format '{{.Status}}' "$VM")" != Running ]; then
	limactl start "$VM"
fi

if limactl shell "$VM" -- test -e /dev/zfs; then
	exit 0
fi

echo "integration-vm: first start: applying integration/nixos (downloads zfs, Docker, a kernel; a few minutes)"
# Build from a copy: the home mount is read-only and nix writes flake.lock
# next to flake.nix when it is missing. path: keeps nix from asking git.
limactl shell "$VM" -- bash -c "rm -rf /tmp/zb-nixos && cp -r '$DIR/nixos' /tmp/zb-nixos"
limactl shell "$VM" -- sudo nixos-rebuild boot --flake "path:/tmp/zb-nixos#zfsbackup-$(limactl shell "$VM" -- uname -m)"
limactl restart "$VM"
limactl shell "$VM" -- test -e /dev/zfs || { echo "integration-vm: still no /dev/zfs after the rebuild" >&2; exit 1; }
limactl shell "$VM" -- docker info >/dev/null
