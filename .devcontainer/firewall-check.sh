#!/bin/bash
# Exercises init-firewall.sh in a throwaway container, as root, with the
# working tree's script mounted over the image's copy. Run it through
# `make devcontainer-firewall-check`.
#
#   1. a first run, from a container with no rules;
#   2. a re-run, sampling the policies all along: none may ever be ACCEPT;
#   3. a re-run started by the dev user (through its sudoers rule) and killed
#      after 0.3s: its trap must close the network (exit 1, the "interrupted"
#      message, loopback-only rules), so an outside host is blocked;
#   4. a re-run from the closed state that the kill may leave.
#
# The probe host must be reachable from containers on this machine without
# the firewall (some hosts are not, see the napkin's host network quirk).
set -uo pipefail

FW=/usr/local/bin/init-firewall.sh
PROBE=https://google.com

fail() {
    echo "FAIL: $*" >&2
    exit 1
}
step() { printf '\n--- %s ---\n' "$*"; }
probe_blocked() { ! curl -sS --max-time 5 -o /dev/null "$PROBE" 2>/dev/null; }
policies_drop() {
    local rules c
    rules=$(iptables -S) || return 1
    for c in INPUT FORWARD OUTPUT; do
        grep -qx -- "-P $c DROP" <<< "$rules" || return 1
    done
}

step "probe before any rules"
curl -sS --max-time 10 -o /dev/null "$PROBE" || fail "$PROBE is unreachable without a firewall; pick another probe host"

step "first run"
"$FW" || fail "first run failed"
policies_drop || fail "first run left a policy other than DROP: $(iptables -S | grep -- '^-P')"
probe_blocked || fail "$PROBE is reachable after the first run"

step "re-run, sampling the policies"
seen=$(mktemp)
(
    while :; do
        { iptables -S; ip6tables -S 2>/dev/null; } | grep -E -- '^-P [A-Z]+ ACCEPT' >> "$seen"
    done
) &
sampler=$!
"$FW"
rc=$?
kill "$sampler"
wait "$sampler" 2>/dev/null
[ "$rc" = 0 ] || fail "re-run failed"
[ ! -s "$seen" ] || fail "a policy was ACCEPT during the re-run: $(sort -u "$seen" | tr '\n' ' ')"

step "re-run by the dev user, killed after 0.3s"
# The allowlist from step 2 already has DROP policies and blocks the probe, so
# those alone do not show the kill did anything: check that the trap ran.
killed_log=$(mktemp)
chown dev "$killed_log"
rc=$(runuser -u dev -- bash -c '
    sudo -n /usr/local/bin/init-firewall.sh > "$1" 2>&1 &
    pid=$!
    sleep 0.3
    kill -TERM "$pid" || exit 3
    wait "$pid"
    echo "$?"
' _ "$killed_log") || fail "could not start and kill the re-run as dev"
cat "$killed_log"
[ "$rc" = 1 ] || fail "the killed re-run exited $rc, want 1 from its trap (killed before the trap was set, or after it finished?)"
grep -q 'firewall failed: interrupted; network is closed' "$killed_log" \
    || fail "the killed re-run did not report closing the network"
loopback_only=$'-P INPUT DROP\n-P FORWARD DROP\n-P OUTPUT DROP\n-A INPUT -i lo -j ACCEPT\n-A OUTPUT -o lo -j ACCEPT'
[ "$(iptables -S)" = "$loopback_only" ] \
    || fail "the killed re-run did not leave loopback only: $(iptables -S | tr '\n' ' ')"
if ip6tables -L -n >/dev/null 2>&1; then
    [ "$(ip6tables -S)" = "$loopback_only" ] \
        || fail "the killed re-run did not leave IPv6 loopback only: $(ip6tables -S | tr '\n' ' ')"
fi
policies_drop || fail "the killed re-run left a policy other than DROP: $(iptables -S | grep -- '^-P' | tr '\n' ' ')"
probe_blocked || fail "$PROBE is reachable after the killed re-run"

step "re-run after the kill"
"$FW" || fail "re-run after the kill failed"
policies_drop || fail "re-run after the kill left a policy other than DROP"
probe_blocked || fail "$PROBE is reachable after the re-run"

step "firewall check passed"
