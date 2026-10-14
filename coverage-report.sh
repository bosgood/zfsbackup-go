#!/bin/sh
# Print a markdown table of statement coverage by package from one or more Go
# cover profiles (go test -coverpkg=./... -coverprofile=...), one column per
# profile. The column name is the profile's file name without .out.
#   sh coverage-report.sh tmp/coverage/unit.out tmp/coverage/e2e.out
# With -f the rows are files, not packages (the README's backends table):
#   sh coverage-report.sh -f tmp/coverage/*.out | grep "backends/"
# A block covered by any test binary of a profile counts as covered; the same
# block appears once per test binary when several packages are tested at once.
# Rows are sorted by package through sort(1): BSD awk has no asorti.
set -eu
[ $# -ge 1 ] || { echo "usage: $0 profile.out [profile.out ...]" >&2; exit 2; }
group=pkg
[ "$1" = -f ] && { group=file; shift; }
mod=$(sed -n 's/^module //p' go.mod)
awk -v mod="$mod" -v group="$group" '
FNR == 1 { f++; name = FILENAME; sub(/.*\//, "", name); sub(/\.out$/, "", name); col[f] = name }
/^mode:/ { next }
{
	split($1, a, ":"); file = a[1]; key = file ":" a[2]
	pkg = file; if (group == "pkg") sub(/\/[^\/]*$/, "", pkg); sub("^" mod, "", pkg); sub(/^\//, "", pkg)
	if (pkg == "") pkg = "(root)"
	nstmt[key] = $2; pkgof[key] = pkg; pkgs[pkg] = 1
	if ($3 > 0) cov[f, key] = 1
}
END {
	for (key in nstmt) {
		pkg = pkgof[key]; n = nstmt[key]; tot[pkg] += n; T += n
		for (i = 1; i <= f; i++) if (cov[i, key]) { hit[i, pkg] += n; H[i] += n }
	}
	printf "0\t| %s | Statements |", (group == "pkg" ? "Package" : "File"); for (i = 1; i <= f; i++) printf " %s |", col[i]; printf "\n"
	printf "0\t|---|---:|"; for (i = 1; i <= f; i++) printf "---:|"; printf "\n"
	for (pkg in pkgs) {
		printf "1 %s\t| `%s` | %d |", pkg, pkg, tot[pkg]
		for (i = 1; i <= f; i++) printf " %.1f%% |", 100 * hit[i, pkg] / tot[pkg]
		printf "\n"
	}
	printf "2\t| **all** | %d |", T
	for (i = 1; i <= f; i++) printf " **%.1f%%** |", 100 * H[i] / T
	printf "\n"
}' "$@" | LC_ALL=C sort -s -t '	' -k1,1 | cut -f 2-
