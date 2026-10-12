# Integration test: incremental chains and operations

Extend the real-ZFS + S3 integration test (`make integration-vm`,
`make integration-docker`) so it covers what `TestIntegrationS3` leaves out:

- a chain of 3 or more links (full -> incremental -> incremental)
- an incremental chain with encryption and signing
- a restore of a middle link (not the newest)
- a bookmark as the incremental source
- `list`, `clean`, and resume after a failed send

Source: the coverage review of 2026-10-11 (chain of 2 links only; `TestIntegration`
and `TestEncryptionAndSign` need `tank/data@c` and Azurite, so the env never runs them).
