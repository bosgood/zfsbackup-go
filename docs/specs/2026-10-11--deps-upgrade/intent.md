# Intent: upgrade the Go dependencies

Date: 2026-10-11
Branch: `clean-dry-run` at `10fcf0c`
Status: plan written, not executed

## Why now

- Every module in `go.mod` is from September 2022 or older. Upstream
  (`someone1/zfsbackup-go` master) has the same versions, so there is nothing
  to cherry-pick.
- `govulncheck` on the current tree reports **26 vulnerabilities from 5
  modules** that our code reaches: `x/net` (HTTP/2, 9 entries), `x/crypto`
  (ssh and agent, 13 entries), `grpc` (2), `x/text` (1), `protobuf` (1).
  The `ssh://` backend and every cloud backend are on the call paths.
- `go.mod` says `go 1.25`. Go 1.25 left support when Go 1.27 shipped
  (August 2026). The newest `golang.org/x/*` and Google modules need Go 1.26.
- `aws-sdk-go` v1 reached end of support on 2025-07-31. Its newest release
  marks every package deprecated. We stay on v1 in this change (see scope).

## Goal

After the change:

1. Every direct dependency is at its newest release that builds with the
   chosen Go toolchain. `vendor/` matches `go.mod`.
2. `make test-docker`, `make lint`, `make fmt-check` and `make e2e` pass.
3. `make vulncheck` (new target) reports only GO-2026-5932
   (`x/crypto/openpgp` is unmaintained by design; `docs/specs/2026-09-29--pgp-ed25519/`
   replaces that package).
4. The upgrade is repeatable: Makefile targets do the `go get`, `go mod tidy`,
   `go mod vendor` and `govulncheck` in the pinned image, because the host
   cannot reach `proxy.golang.org`.

## Scope

In:

- All modules in `go.mod`, including indirect ones.
- The Go toolchain bump to 1.26 (recommended; `plan.md` has a fallback
  that stays on 1.25).
- The three Azure calls that no longer compile, one lint exclusion, one
  dependency pin with an upstream fix, one test-server fix.
- New Makefile targets and their docs.

Out (each is its own spec later):

- Migrating `aws-sdk-go` v1 to v2.
- Migrating `azure-storage-blob-go` (last release 2022) to the track-2 SDK.
- Replacing `x/crypto/openpgp` (existing spec `2026-09-29--pgp-ed25519`).
- Replacing `kurin/blazer` (B2, last release 2019) or `op/go-logging` (2016).
- Any behavior change in `send`, `receive`, `list`, `clean`, `plan`.

## Success check

Run, in this order, and record the output in `plan.md`:

```
make test-docker
make lint
make fmt-check
make vulncheck
make e2e
make test-race        # once, ~20 min
```
