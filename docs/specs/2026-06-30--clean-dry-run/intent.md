# Intent: dry-run support for `zfsbackup clean`

We're going to implement dry-run support for `zfsbackup clean`. Read through
`cmd/clean.go` and `backup/clean.go` and look for the places where actual
mutating operations are performed. We'll have a `-n` / `--dry-run` flag that
will show what WOULD have been performed without the flag.
