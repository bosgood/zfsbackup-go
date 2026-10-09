#!/bin/bash
# Exercises init-firewall.sh in a throwaway container, as root, with the
# working tree's script mounted over the image's copy. Run it through
# `make devcontainer-firewall-check`.
#
#   1. a first run, from a container with no rules;
#   2. a re-run, sampling the policies all along: none may ever be ACCEPT;
#   3. a re-run started by the dev user (through its sudoers rule) and killed
#      after 0.3s: the policies must stay DROP and an outside host blocked;
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
runuser -u dev -- bash -c '
    sudo -n /usr/local/bin/init-firewall.sh &
    pid=$!
    sleep 0.3
    kill -TERM "$pid" || exit 3
    wait "$pid"
    exit 0
' || fail "could not start and kill the re-run as dev"
policies_drop || fail "the killed re-run left a policy other than DROP: $(iptables -S | grep -- '^-P' | tr '\n' ' ')"
probe_blocked || fail "$PROBE is reachable after the killed re-run"

step "re-run after the kill"
"$FW" || fail "re-run after the kill failed"
policies_drop || fail "re-run after the kill left a policy other than DROP"
probe_blocked || fail "$PROBE is reachable after the re-run"

step "firewall check passed"
