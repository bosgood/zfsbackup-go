#!/bin/bash
# Starts sshd on 127.0.0.1:2222 for the ssh:// integration tests. Sourced by
# entrypoint.sh after the pool exists, so the exports reach the test binary.
#
# Users and keys (Ed25519, no passphrase):
#   root       /root/.ssh/id_ed25519, the default key the backend finds by itself
#   keyonly    /root/ssh-keys/keyonly_ed25519 only
#   agentonly  /root/ssh-keys/agentonly_ed25519 only (the test adds it to its own agent)
#   pwonly     the password zfsbackup-pw only
#   jailed     root's default key; sftp only, chrooted to /srv/sshjail
#              (sshd_config), so its remote paths are not the test's local paths
# No ssh-agent here: SSH_AUTH_SOCK set for every test would put agent auth
# first in every Init.
set -euo pipefail

mkdir -p /run/sshd
ssh-keygen -A

for u in keyonly agentonly pwonly jailed; do
	id "$u" >/dev/null 2>&1 || useradd -m -s /bin/sh "$u"
done
echo 'pwonly:zfsbackup-pw' | chpasswd
# With UsePAM no, sshd refuses every login of an account whose password field
# starts with "!" (locked), keys too. useradd leaves it so: unlock without a password.
usermod -p '*' root
usermod -p '*' keyonly
usermod -p '*' agentonly
usermod -p '*' jailed
# A chroot and every directory above it must be root's and writable by no one else
# (not under /var/tmp, mode 1777).
install -d -m 0755 -o root -g root /srv /srv/sshjail

# key <file> <user>: a new key at <file>, its public half in <user>'s authorized_keys.
key() {
	local file=$1 user=$2 home
	home=$(getent passwd "$user" | cut -d: -f6)
	rm -f "$file" "$file.pub"
	ssh-keygen -q -t ed25519 -N '' -C "$user@zfsbackup-it" -f "$file"
	install -d -m 0700 -o "$user" -g "$user" "$home/.ssh"
	cat "$file.pub" >"$home/.ssh/authorized_keys"
	chown "$user:$user" "$home/.ssh/authorized_keys"
	chmod 0600 "$home/.ssh/authorized_keys"
}
install -d -m 0700 /root/.ssh /root/ssh-keys
key /root/.ssh/id_ed25519 root
key /root/ssh-keys/keyonly_ed25519 keyonly
key /root/ssh-keys/agentonly_ed25519 agentonly
install -d -m 0700 -o jailed -g jailed /home/jailed/.ssh
install -m 0600 -o jailed -g jailed /root/.ssh/id_ed25519.pub /home/jailed/.ssh/authorized_keys

/usr/sbin/sshd -f /src/integration/sshd_config -E /var/tmp/sshd.log
ready=
for _ in $(seq 30); do
	if ssh-keyscan -p 2222 -t ed25519 127.0.0.1 >/root/.ssh/known_hosts 2>/dev/null && [ -s /root/.ssh/known_hosts ]; then
		ready=1
		break
	fi
	sleep 1
done
if [ -z "$ready" ]; then
	echo "sshd.sh: sshd does not answer on 127.0.0.1:2222:" >&2
	cat /var/tmp/sshd.log >&2 || true
	exit 1
fi
chmod 0600 /root/.ssh/known_hosts
echo "sshd.sh: sshd on 127.0.0.1:2222; known_hosts: $(cut -d' ' -f1,2 /root/.ssh/known_hosts)"

export ZFSBACKUP_IT_SSH_HOST=127.0.0.1
export ZFSBACKUP_IT_SSH_PORT=2222
export ZFSBACKUP_IT_SSH_KEYS=/root/ssh-keys
export ZFSBACKUP_IT_SSH_PASSWORD=zfsbackup-pw
export ZFSBACKUP_IT_SSH_JAIL=/srv/sshjail
