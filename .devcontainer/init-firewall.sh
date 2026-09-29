#!/bin/bash
# Egress firewall for the zfsbackup-go dev container.
#
# Runs as root from devcontainer.json's postStartCommand (through a sudoers
# rule limited to this script). Default-denies all traffic except:
#   - loopback, DNS, and the Docker host network (default gateway /24)
#   - GitHub, from the IP ranges published at https://api.github.com/meta
#   - the domains in ALLOWED_DOMAINS below
# Everything else is REJECTed so misconfigured tools fail fast instead of
# hanging. IPv6 is blocked entirely (apart from loopback and DNS) because the
# allowlist is IPv4-only.
#
# Domains are resolved once, when this runs. If a CDN rotates addresses and
# something that used to work starts failing, re-run it:
#   sudo /usr/local/bin/init-firewall.sh
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
die() { log "ERROR: $*" >&2; exit 1; }

is_ipv4() { [[ "$1" =~ ^[0-9]{1,3}(\.[0-9]{1,3}){3}$ ]]; }
is_ipv4_cidr() { [[ "$1" =~ ^[0-9]{1,3}(\.[0-9]{1,3}){3}/[0-9]{1,2}$ ]]; }

# Docker's embedded DNS (127.0.0.11, used on user-defined networks) depends on
# NAT rules that the flush below would destroy. Save them first.
DOCKER_DNS_RULES=$(iptables-save -t nat 2>/dev/null | grep '127\.0\.0\.11' || true)

iptables -F
iptables -X
iptables -t nat -F
iptables -t nat -X
iptables -t mangle -F
iptables -t mangle -X
ipset destroy allowed-domains 2>/dev/null || true

if [ -n "$DOCKER_DNS_RULES" ]; then
    log "Restoring Docker embedded-DNS NAT rules"
    iptables -t nat -N DOCKER_OUTPUT 2>/dev/null || true
    iptables -t nat -N DOCKER_POSTROUTING 2>/dev/null || true
    echo "$DOCKER_DNS_RULES" | xargs -L 1 iptables -t nat
fi

# Baseline: loopback and DNS.
iptables -A INPUT -i lo -j ACCEPT
iptables -A OUTPUT -o lo -j ACCEPT
iptables -A OUTPUT -p udp --dport 53 -j ACCEPT
iptables -A OUTPUT -p tcp --dport 53 -j ACCEPT
iptables -A INPUT -p udp --sport 53 -j ACCEPT

# Docker host network, so port forwarding and services published on the
# bridge keep working.
HOST_IP=$(ip route | awk '/^default/ {print $3; exit}')
[ -n "$HOST_IP" ] || die "could not determine the default gateway"
HOST_NETWORK="${HOST_IP%.*}.0/24"
log "Host network: $HOST_NETWORK"
iptables -A INPUT -s "$HOST_NETWORK" -j ACCEPT
iptables -A OUTPUT -d "$HOST_NETWORK" -j ACCEPT

ipset create allowed-domains hash:net

# GitHub (git over HTTPS/SSH, the API, raw/objects CDN).
log "Fetching GitHub IP ranges"
gh_meta=$(curl -fsS --max-time 15 https://api.github.com/meta) || die "could not fetch https://api.github.com/meta"
echo "$gh_meta" | jq -e '.web and .api and .git' >/dev/null || die "unexpected response from https://api.github.com/meta"
gh_count=0
while read -r cidr; do
    is_ipv4_cidr "$cidr" || die "invalid CIDR from GitHub meta: $cidr"
    ipset add -exist allowed-domains "$cidr"
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
        ipset add -exist allowed-domains "$ip"
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

# Default deny, then the allow rules.
iptables -P INPUT DROP
iptables -P FORWARD DROP
iptables -P OUTPUT DROP
iptables -A INPUT -m conntrack --ctstate ESTABLISHED,RELATED -j ACCEPT
iptables -A OUTPUT -m conntrack --ctstate ESTABLISHED,RELATED -j ACCEPT
iptables -A OUTPUT -m set --match-set allowed-domains dst -j ACCEPT
iptables -A OUTPUT -j REJECT --reject-with icmp-admin-prohibited

# IPv6: nothing is allowlisted, so block it apart from loopback and DNS.
if ip6tables -L -n >/dev/null 2>&1; then
    ip6tables -F
    ip6tables -X
    ip6tables -P INPUT DROP
    ip6tables -P FORWARD DROP
    ip6tables -P OUTPUT DROP
    ip6tables -A INPUT -i lo -j ACCEPT
    ip6tables -A OUTPUT -o lo -j ACCEPT
    ip6tables -A OUTPUT -p udp --dport 53 -j ACCEPT
    ip6tables -A OUTPUT -p tcp --dport 53 -j ACCEPT
    ip6tables -A INPUT -m conntrack --ctstate ESTABLISHED,RELATED -j ACCEPT
    ip6tables -A OUTPUT -j REJECT --reject-with icmp6-adm-prohibited
else
    log "WARNING: ip6tables unavailable, IPv6 is not filtered"
fi

log "Verifying"
if curl -sS --max-time 5 -o /dev/null https://example.com 2>/dev/null; then
    die "verification failed: https://example.com is reachable"
fi
curl -sS --max-time 10 -o /dev/null https://api.github.com/zen 2>/dev/null \
    || die "verification failed: https://api.github.com is unreachable"
log "Firewall active: example.com blocked, api.github.com reachable"

# The Anthropic API is allowlisted by now, so reaching it depends on the host's
# network rather than on these rules. A VPN on the host that routes Anthropic's
# range through its tunnel while Docker's traffic bypasses it is the usual
# cause. Warn instead of failing so the container still starts.
curl -sS --max-time 10 -o /dev/null https://api.anthropic.com/ 2>/dev/null \
    || log "WARNING: https://api.anthropic.com is unreachable from this container (host VPN or network?). Claude Code needs it."
