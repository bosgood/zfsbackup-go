# prune subcommand

The `zfsbackup prune` subcommand is used to remove older backup sets (and manifests) that match certain criteria.

## Usage

To remove all backups older than a given time (30d):

```shell
zfsbackup prune --olderThan 720h --dry-run
(output of what would be removed)
```


## Notes

This is related to `zfsbackup clean` functionality, except that it is only affecting manifests and data that ARE completed and managed. One implementation idea could be to delete the manifests, and then let `clean` functionality take over.
