//go:build integration

package main

import (
	"slices"
	"strings"
	"testing"
)

// TestIntegrationS3CleanEncrypted runs clean at a destination of encrypted
// manifests with no rings, with --force and no rings, and with another key's
// rings: each refuses before it deletes anything, says to pass the secret key
// ring, never says to delete the manifest, and keeps its cached copy. With the
// right rings clean deletes the orphan, and the chain restores.
func TestIntegrationS3CleanEncrypted(t *testing.T) {
	pool := testPool(t)
	client, bucket, prefix := itBucket(t, "zfsbackup-it-clenc")
	target := "s3://" + bucket + "/" + prefix
	restored := pool + "/restoredClEnc"
	destroyAfter(t, restored)
	work := t.TempDir()
	rings := writeRings(t, newKey(t))
	c := buildChain(t, pool, work, target, "clenc", rings...)
	orphan := orphanOf(t, client, bucket, prefix, manifestFor(t, work, c.src, target, c.daily3, rings...).Volumes[0].ObjectName, 77)
	littered := objectNames(t, client, bucket, prefix)
	manifests := 0
	for _, n := range littered {
		if strings.Contains(n, ".manifest") {
			manifests++
		}
	}

	refused := func(name string, args ...string) {
		t.Helper()
		fresh := t.TempDir()
		logs, err := itClean(t, fresh, append(args, target)...)
		if err == nil {
			t.Fatalf("clean %s succeeded, want a refusal:\n%s", name, logs)
		}
		wantInLog(t, logs, "could not read the manifest", "--secretKeyRingPath", "Do not delete it unless you know it is not a backup")
		// Refused for the manifest it cannot read, not later for having none: a clean
		// that skipped it would refuse too, but would delete once one manifest reads.
		wantNotInLog(t, logs, "delete that object from the destination", "objects but no manifests")
		wantObjectsAt(t, client, bucket, prefix, littered)
		// syncCache downloads every manifest before clean reads one: the one it cannot
		// read must keep its copy too.
		if got := cachedFiles(t, fresh); len(got) != manifests {
			t.Errorf("clean %s left %d cached manifests, want all %d: the copy of the one it could not read stays", name, len(got), manifests)
		}
	}
	refused("with no rings")
	refused("--force with no rings", "--force")
	refused("with another key's rings", writeRings(t, newKey(t))...)

	if logs, err := itClean(t, work, append(slices.Clone(rings), target)...); err != nil {
		t.Fatalf("clean with the rings: %v\n%s", err, logs)
	}
	cleaned := slices.DeleteFunc(slices.Clone(littered), func(n string) bool { return n == orphan })
	wantObjectsAt(t, client, bucket, prefix, cleaned)
	for _, snap := range []string{c.monthly, c.daily2, c.daily3} {
		requireVolumes(t, manifestFor(t, work, c.src, target, snap, rings...), cleaned)
	}
	itReceive(t, t.TempDir(), append(slices.Clone(rings), "--auto", "-F", c.src, target, restored)...)
	wantSnapshots(t, restored, c.monthly, c.daily2, c.daily3)
	wantGUIDs(t, c.src, restored, c.monthly, c.daily2, c.daily3)
	compareDirs(t, "/"+c.src, "/"+restored)
}
