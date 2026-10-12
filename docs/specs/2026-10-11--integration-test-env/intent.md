# Integration test env

Let's construct an integration test env. It will run ZFS in a Docker container and use the actual S3 backend to upload and test restores.

## Requirements

- Sanoid-style named snapshots locally in ZFS
- S3 backup test for ZFS send
- S3 backup test for ZFS receive
