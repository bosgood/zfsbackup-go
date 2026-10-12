# Integration test: a large send with the default flags

Add one real-ZFS + MinIO test that moves about 1 GiB with the flags production
uses: `--volsize 200`, `--maxFileBuffer 5`, `--maxParallelUploads 4`, the
internal compressor.

Source: the coverage review of 2026-10-12. Every `TestIntegrationS3*` test
sends with `--volsize 1` and at most 3 MiB of data. No object reaches the
5 MiB threshold at which the AWS uploader switches to a multipart upload, so
the path every production volume takes (CreateMultipartUpload, 4 parallel
UploadPart calls of 10 MiB, per-part Content-MD5, CompleteMultipartUpload)
never runs against a real S3 server. Pipe mode (`--maxFileBuffer 0`) has the
same gap: a 1 MiB pipe volume fits in the first chunk and goes up in one PUT.

Also untested with real zfs: a replication stream (`send -R`) of a parent with
children, received as a tree.

Out of scope: S3 listings past 1000 keys, throttling, Glacier, the O(n^2)
per-volume manifest save (noted in the review; separate work).
