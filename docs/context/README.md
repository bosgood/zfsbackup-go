# docs/context

One file per component. Open the file for the component a task touches. `AGENTS.md` links here.

| File | Component |
|---|---|
| [cli.md](cli.md) | `cmd/`: commands, flags, exit codes, globals |
| [send.md](send.md) | `backup.Backup`: smart selection, resume, upload pipeline |
| [receive.md](receive.md) | `backup.Receive`, `AutoRestore` |
| [clean.md](clean.md) | `backup.Clean` |
| [plan.md](plan.md) | `plan` simulator, checks, golden scenarios |
| [data-model.md](data-model.md) | `files/`, `zfs/`, object names, working dir |
| [backends.md](backends.md) | `Backend` interface and the seven backends |
| [testing.md](testing.md) | Makefile targets, test layers, `fakezfs` |

Keep each file short. Put the "why" of a decision in `docs/specs/`, not here. Update a file when its component changes.
