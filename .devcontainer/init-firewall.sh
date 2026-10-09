#!/bin/bash
# Egress firewall for the zfsbackup-go dev container.
#
# Runs as root from devcontainer.json's postStartCommand (through a sudoers
# rule limited to this script). Default-denies all traffic except:
#   - loopback, DNS to the resolvers in /etc/resolv.conf (and Docker's
#     127.0.0.11), and the Docker host network (default gateway /24)
#   - GitHub, from the IP ranges published at https://api.github.com/meta
#   - the domains in ALLOWED_DOMAINS below
# Everything else is REJECTed so misconfigured tools fail fast instead of
# hanging. IPv6 is blocked entirely (apart from loopback and DNS) because the
# allowlist is IPv4-only; with IPv6 up and no working ip6tables the network is
# closed instead.
#
# Known-wide exits, kept on purpose: every GitHub range (not just this repo),
# sentry.io and statsig.com (Claude Code telemetry), vscode.blob.core.windows.net
# (an Azure CDN shared with other tenants), the whole host /24, and DNS: any
# name can be queried through the resolvers, so data can be tunnelled out in
# DNS queries.
#
# The policies never pass through ACCEPT. The first change sets them to DROP;
# a bootstrap chain then allows only DNS and api.github.com while the new
# allowlist is built into a spare ipset next to the enforced one; the sets are
# swapped and the whole filter table is replaced in one iptables-restore. Any
# failure, or a TERM/INT/HUP, from the first change to the end of the
# verification leaves the network CLOSED (loopback only) with the reason on
# stderr. Domains are resolved once, when this runs. If a CDN rotates addresses
# and something that used to work starts failing, or after a failure, re-run it:
#   sudo /usr/local/bin/init-firewall.sh
# `make devcontainer-firewall-check` exercises it in a throwaway container.
#
# Modeled on the reference sandbox in anthropics/claude-code/.devcontainer.
set -euo pipefail
IFS=$'\n\t'

ALLOWED_DOMAINS=(
    # Claude Code: API, OAuth login, telemetry/crash reports, native-installer updates
    api.anthropic.com
    claude.ai
    console.anthropic.com
    platform.claude.com
    statsig.com
    sentry.io
    downloads.claude.ai
    # Go module proxy + checksum database (go install / go get, gopls tool updates)
    proxy.golang.org
    sum.golang.org
    # VS Code server + extension marketplace
    update.code.visualstudio.com
    vscode.download.prss.microsoft.com
    marketplace.visualstudio.com
    vscode.blob.core.windows.net
    anthropic.gallerycdn.vsassets.io
    golang.gallerycdn.vsassets.io
)
# Without this one Claude Code cannot work at all, so failing to resolve it is fatal.
REQUIRED_DOMAIN=api.anthropic.com

log() { printf '[init-firewall] %s\n' "$*"; }

HAVE_IP6=0
if ip6tables -L -n >/dev/null 2>&1; then
    HAVE_IP6=1
fi
# IPv6 is up when an interface other than loopback has an IPv6 address.
IP6_UP=0
if awk '$6 != "lo" {found = 1} END {exit !found}' /proc/net/if_inet6 2>/dev/null; then
    IP6_UP=1
fi

# fail_closed leaves only loopback open and exits. It is the ERR, TERM, INT and
# HUP trap from the first change on, and what die() does in that window.
fail_closed() {
    local reason=$1
    trap - ERR
    trap '' TERM INT HUP
    set +e
    iptables -P INPUT DROP
    iptables -P FORWARD DROP
    iptables -P OUTPUT DROP
    iptables -F
    iptables -X
    iptables -A INPUT -i lo -j ACCEPT
    iptables -A OUTPUT -o lo -j ACCEPT
    if [ "$HAVE_IP6" = 1 ]; then
        ip6tables -P INPUT DROP
        ip6tables -P FORWARD DROP
        ip6tables -P OUTPUT DROP
        ip6tables -F
        ip6tables -X
        ip6tables -A INPUT -i lo -j ACCEPT
        ip6tables -A OUTPUT -o lo -j ACCEPT
    fi
    log "ERROR: firewall failed: $reason; network is closed; fix and re-run sudo /usr/local/bin/init-firewall.sh" >&2
    exit 1
}
ARMED=0
die() {
    if [ "$ARMED" = 1 ]; then
        fail_closed "$*"
    fi
    log "ERROR: $*" >&2
    exit 1
}

is_ipv4() { [[ "$1" =~ ^[0-9]{1,3}(\.[0-9]{1,3}){3}$ ]]; }
is_ipv4_cidr() { [[ "$1" =~ ^[0-9]{1,3}(\.[0-9]{1,3}){3}/[0-9]{1,2}$ ]]; }

# Resolvers (127.0.0.11, Docker's embedded DNS, is reached over loopback and
# needs no rule of its own; its NAT rules are left alone).
resolvers4=()
resolvers6=()
while read -r ns; do
    if is_ipv4 "$ns"; then
        resolvers4+=("$ns")
    elif [[ "$ns" == *:* ]]; then
        resolvers6+=("$ns")
    fi
done < <(awk '/^nameserver/ {print $2}' /etc/resolv.conf 2>/dev/null || true)
if [ "${#resolvers4[@]}" -eq 0 ] && [ "${#resolvers6[@]}" -eq 0 ]; then
    die "no nameserver in /etc/resolv.conf"
fi

# Docker host network, so port forwarding and services published on the
# bridge keep working.
HOST_IP=$(ip route | awk '/^default/ {print $3; exit}')
[ -n "$HOST_IP" ] || die "could not determine the default gateway"
HOST_NETWORK="${HOST_IP%.*}.0/24"

# A host outside the allowlist that answers now, before enforcement (on a
# re-run the old rules already block it, so there may be none). The
# verification requires it to be blocked afterwards.
PROBE_HOST=
for host in google.com example.com; do
    if curl -sS --max-time 5 -o /dev/null "https://$host" 2>/dev/null; then
        PROBE_HOST=$host
        break
    fi
done

# From the first change on, a failure or a signal closes the network.
ARMED=1
trap 'fail_closed "$BASH_COMMAND failed at line $LINENO"' ERR
trap 'fail_closed "interrupted"' TERM INT HUP

if [ "$IP6_UP" = 1 ] && [ "$HAVE_IP6" = 0 ]; then
    die "IPv6 is up but ip6tables is unusable, so IPv6 cannot be filtered"
fi

iptables -P INPUT DROP
iptables -P FORWARD DROP
iptables -P OUTPUT DROP

# IPv6: nothing is allowlisted, so block it apart from loopback and DNS to the
# configured IPv6 resolvers. Final from the start.
if [ "$HAVE_IP6" = 1 ]; then
    {
        echo '*filter'
        echo ':INPUT DROP [0:0]'
        echo ':FORWARD DROP [0:0]'
        echo ':OUTPUT DROP [0:0]'
        echo '-A INPUT -i lo -j ACCEPT'
        echo '-A INPUT -m conntrack --ctstate ESTABLISHED,RELATED -j ACCEPT'
        echo '-A OUTPUT -o lo -j ACCEPT'
        for ns in "${resolvers6[@]}"; do
            echo "-A OUTPUT -d $ns -p udp --dport 53 -j ACCEPT"
            echo "-A OUTPUT -d $ns -p tcp --dport 53 -j ACCEPT"
        done
        echo '-A OUTPUT -j REJECT --reject-with icmp6-adm-prohibited'
        echo 'COMMIT'
    } | ip6tables-restore
else
    log "WARNING: ip6tables unavailable, IPv6 is not filtered (no IPv6 interface is up)"
fi

# Bootstrap: whatever the current rules are (none, the last allowlist, or
# closed after a failure), allow DNS and api.github.com for the fetch below.
# Both stay allowed in the final rules.
iptables -N fw-bootstrap 2>/dev/null || iptables -F fw-bootstrap
iptables -A fw-bootstrap -i lo -j ACCEPT
iptables -A fw-bootstrap -o lo -j ACCEPT
iptables -A fw-bootstrap -m conntrack --ctstate ESTABLISHED,RELATED -j ACCEPT
for ns in "${resolvers4[@]}"; do
    iptables -A fw-bootstrap -d "$ns" -p udp --dport 53 -j ACCEPT
    iptables -A fw-bootstrap -d "$ns" -p tcp --dport 53 -j ACCEPT
done
iptables -C INPUT -j fw-bootstrap 2>/dev/null || iptables -I INPUT 1 -j fw-bootstrap
iptables -C OUTPUT -j fw-bootstrap 2>/dev/null || iptables -I OUTPUT 1 -j fw-bootstrap
gh_api_ips=$(dig +noall +answer +time=3 +tries=2 A api.github.com | awk '$4 == "A" {print $5}') || true
[ -n "$gh_api_ips" ] || die "could not resolve api.github.com"
while read -r ip; do
    is_ipv4 "$ip" || die "invalid address from DNS for api.github.com: $ip"
    iptables -A fw-bootstrap -d "$ip" -p tcp --dport 443 -j ACCEPT
done <<< "$gh_api_ips"

# The new allowlist goes into a spare set; the enforced one is untouched.
ipset destroy allowed-domains-new 2>/dev/null || true
ipset create allowed-domains-new hash:net

# GitHub (git over HTTPS/SSH, the API, raw/objects CDN).
log "Fetching GitHub IP ranges"
gh_meta=$(curl -fsS --max-time 15 https://api.github.com/meta) || die "could not fetch https://api.github.com/meta"
echo "$gh_meta" | jq -e '.web and .api and .git' >/dev/null || die "unexpected response from https://api.github.com/meta"
gh_count=0
while read -r cidr; do
    is_ipv4_cidr "$cidr" || die "invalid CIDR from GitHub meta: $cidr"
    ipset add -exist allowed-domains-new "$cidr"
    gh_count=$((gh_count + 1))
done < <(echo "$gh_meta" | jq -r '(.web + .api + .git)[]' | grep -v ':' | aggregate -q)
[ "$gh_count" -gt 0 ] || die "no IPv4 ranges found in GitHub meta"
log "Allowed $gh_count GitHub ranges"

# Allowlisted domains, resolved now.
resolve_and_allow() {
    local domain=$1 ips ip
    ips=$(dig +noall +answer +time=3 +tries=2 A "$domain" | awk '$4 == "A" {print $5}') || true
    [ -n "$ips" ] || return 1
    while read -r ip; do
        is_ipv4 "$ip" || die "invalid address from DNS for $domain: $ip"
        ipset add -exist allowed-domains-new "$ip"
    done <<< "$ips"
    log "Allowed $domain: $(echo "$ips" | tr '\n' ' ')"
}
unresolved=()
for domain in "${ALLOWED_DOMAINS[@]}"; do
    resolve_and_allow "$domain" || unresolved+=("$domain")
done
if [ "${#unresolved[@]}" -gt 0 ]; then
    (IFS=' '; log "WARNING: could not resolve: ${unresolved[*]}")
    for domain in "${unresolved[@]}"; do
        [ "$domain" = "$REQUIRED_DOMAIN" ] && die "$REQUIRED_DOMAIN did not resolve; is DNS working?"
    done
fi

# Enforce: swap the new set in, then replace the whole filter table (which
# also drops the bootstrap chain) in one step, policies held at DROP.
ipset create -exist allowed-domains hash:net
ipset swap allowed-domains-new allowed-domains
ipset destroy allowed-domains-new
{
    echo '*filter'
    echo ':INPUT DROP [0:0]'
    echo ':FORWARD DROP [0:0]'
    echo ':OUTPUT DROP [0:0]'
    echo '-A INPUT -i lo -j ACCEPT'
    echo '-A OUTPUT -o lo -j ACCEPT'
    for ns in "${resolvers4[@]}"; do
        echo "-A OUTPUT -d $ns -p udp --dport 53 -j ACCEPT"
        echo "-A OUTPUT -d $ns -p tcp --dport 53 -j ACCEPT"
        echo "-A INPUT -s $ns -p udp --sport 53 -j ACCEPT"
    done
    echo "-A INPUT -s $HOST_NETWORK -j ACCEPT"
    echo "-A OUTPUT -d $HOST_NETWORK -j ACCEPT"
    echo '-A INPUT -m conntrack --ctstate ESTABLISHED,RELATED -j ACCEPT'
    echo '-A OUTPUT -m conntrack --ctstate ESTABLISHED,RELATED -j ACCEPT'
    echo '-A OUTPUT -m set --match-set allowed-domains dst -j ACCEPT'
    echo '-A OUTPUT -j REJECT --reject-with icmp-admin-prohibited'
    echo 'COMMIT'
} | iptables-restore
(IFS=' '; log "DNS allowed to: ${resolvers4[*]} ${resolvers6[*]}")
log "Host network: $HOST_NETWORK"

# Verification. A failure here closes the network too. The outside host must
# be refused by the REJECT rule, not just unreachable (some hosts are
# unreachable from containers with no firewall at all), so its packet counter
# has to move.
log "Verifying"
reject_packets() { iptables -L OUTPUT -v -x -n | awk '$3 == "REJECT" {print $1}'; }
probe=${PROBE_HOST:-example.com}
before=$(reject_packets)
if curl -sS --max-time 5 -o /dev/null "https://$probe" 2>/dev/null; then
    die "verification failed: https://$probe is reachable"
fi
after=$(reject_packets)
[ "$after" -gt "$before" ] || die "verification failed: https://$probe was not refused by the firewall"
curl -sS --max-time 10 -o /dev/null https://api.github.com/zen 2>/dev/null \
    || die "verification failed: https://api.github.com is unreachable"
trap - ERR TERM INT HUP
ARMED=0
log "Firewall active: $probe blocked, api.github.com reachable"

# The Anthropic API is allowlisted by now, so reaching it depends on the host's
# network rather than on these rules. A VPN on the host that routes Anthropic's
# range through its tunnel while Docker's traffic bypasses it is the usual
# cause. Warn instead of failing so the container still starts.
curl -sS --max-time 10 -o /dev/null https://api.anthropic.com/ 2>/dev/null \
    || log "WARNING: https://api.anthropic.com is unreachable from this container (host VPN or network?). Claude Code needs it."
