//go:build integration

package main

import (
	"errors"
	"os/exec"
	"strings"
	"testing"

	"github.com/bosgood/zfsbackup-go/backup"
	"github.com/bosgood/zfsbackup-go/cmd"
)

// chain is what buildChain made: a dataset with three backups at a target,
// full -> incremental -> incremental.
type chain struct {
	src                     string   // <pool>/src<suffix>
	monthly, daily2, daily3 string   // snapshot names, oldest first
	daily2Clone             string   // mount path of a clone of src@daily2
	smart                   []string // the send flags of every link, extra included
}

// buildChain makes <pool>/src<suffix> and sends three smart backups of it to
// target: a full of monthly, daily2 from monthly, daily3 from daily2. extra
// goes to every send and list (key flags). It clones src@daily2 before daily3
// changes the files, and destroys the clone and src when the test ends.
func buildChain(t *testing.T, pool, work, target, suffix string, extra ...string) chain {
	t.Helper()
	c := chain{src: pool + "/src" + suffix}
	clone := c.src + "d2"
	destroyAfter(t, clone, c.src)
	// No compressor: the volume counter sees compressed bytes; see TestIntegrationS3.
	c.smart = append([]string{
		"--fullIfOlderThan", "4320h", "--fullSnapshotSuffix", "_monthly",
		"--incrementalSnapshotSuffix", "_daily", "--volsize", "1", "--compressor", "",
	}, extra...)

	zfsRun(t, "create", "-o", "mountpoint=/"+c.src, c.src)
	writeFile(t, "/"+c.src, "a.bin", 3<<20)
	c.monthly = snapshot(t, c.src, "monthly")
	snapshot(t, c.src, "daily")
	snapshot(t, c.src, "hourly")
	c.sendLink(t, work, target, extra, c.monthly+"<-")

	writeFile(t, "/"+c.src, "b.bin", 1<<20)
	c.daily2 = snapshot(t, c.src, "daily")
	snapshot(t, c.src, "hourly")
	c.sendLink(t, work, target, extra, c.monthly+"<-", c.daily2+"<-"+c.monthly)
	c.daily2Clone = cloneOf(t, pool, c.src, c.daily2, strings.TrimPrefix(clone, pool+"/"))

	writeFile(t, "/"+c.src, "c.bin", 1<<20)
	c.daily3 = snapshot(t, c.src, "daily")
	snapshot(t, c.src, "hourly")
	c.sendLink(t, work, target, extra, c.monthly+"<-", c.daily2+"<-"+c.monthly, c.daily3+"<-"+c.daily2)
	return c
}

// sendLink runs the smart send and fails unless target then lists the backups
// of c.src as want ("base<-source", oldest first).
func (c chain) sendLink(t *testing.T, work, target string, extra []string, want ...string) {
	t.Helper()
	if logs, err := itSend(t, work, append(c.smart, c.src, target)...); err != nil {
		t.Fatalf("send: %v\n%s", err, logs)
	}
	got := chainNames(itList(t, work, append(extra, target)...)[c.src])
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("after the send %s holds %v, want %v", target, got, want)
	}
}

// wantNoDataset fails if dataset exists.
func wantNoDataset(t *testing.T, dataset string) {
	t.Helper()
	if exec.Command("zfs", "list", dataset).Run() == nil {
		t.Fatalf("%s exists, want none", dataset)
	}
}

// TestIntegrationS3Chain restores a chain of three backups: the middle link
// into an empty dataset (only with --auto), then on to the newest, and the
// whole chain at once.
func TestIntegrationS3Chain(t *testing.T) {
	pool := testPool(t)
	_, bucket, prefix := itBucket(t, "zfsbackup-it-chain")
	target := "s3://" + bucket + "/" + prefix
	restoredMid := pool + "/restoredMid"
	restoredAll := pool + "/restoredAll"
	destroyAfter(t, restoredMid, restoredAll)
	work := t.TempDir()
	c := buildChain(t, pool, work, target, "")

	// The middle link alone needs its source, monthly, on the receiving side.
	if logs, err := itRun(cmd.ResetReceiveJobInfo, "receive", work, "-F", c.src+"@"+c.daily2, target, restoredMid); err == nil {
		t.Fatalf("receive of %s into an empty dataset succeeded, want an error:\n%s", c.daily2, logs)
	}
	wantNoDataset(t, restoredMid)

	// --auto walks back to the full and stops at the snapshot asked for.
	itReceive(t, work, "--auto", "-F", c.src+"@"+c.daily2, target, restoredMid)
	wantSnapshots(t, restoredMid, c.monthly, c.daily2)
	wantGUIDs(t, c.src, restoredMid, c.monthly, c.daily2)
	compareDirs(t, c.daily2Clone, "/"+restoredMid)

	// On to the newest: one more step.
	itReceive(t, work, "--auto", "-F", c.src, target, restoredMid)
	wantSnapshots(t, restoredMid, c.monthly, c.daily2, c.daily3)
	wantGUIDs(t, c.src, restoredMid, c.monthly, c.daily2, c.daily3)
	compareDirs(t, "/"+c.src, "/"+restoredMid)

	// The whole chain into an empty dataset, on a host with an empty cache:
	// the manifests come from the destination, not from the sender's cache.
	itReceive(t, t.TempDir(), "--auto", "-F", c.src, target, restoredAll)
	wantSnapshots(t, restoredAll, c.monthly, c.daily2, c.daily3)
	wantGUIDs(t, c.src, restoredAll, c.monthly, c.daily2, c.daily3)
	compareDirs(t, "/"+c.src, "/"+restoredAll)

	if logs, err := itSend(t, work, append(c.smart, c.src, target)...); !errors.Is(err, backup.ErrNoOp) {
		t.Fatalf("noop: got %v, want %v\n%s", err, backup.ErrNoOp, logs)
	}
}

// TestIntegrationS3EncryptedChain is TestIntegrationS3Chain with every send
// encrypted to and signed with one key: list and receive work with that key's
// rings, and fail with another key's or with none.
func TestIntegrationS3EncryptedChain(t *testing.T) {
	pool := testPool(t)
	client, bucket, prefix := itBucket(t, "zfsbackup-it-enc")
	target := "s3://" + bucket + "/" + prefix
	restoredEnc := pool + "/restoredEnc"
	restoredEncMid := pool + "/restoredEncMid"
	destroyAfter(t, restoredEnc, restoredEncMid)
	work := t.TempDir()
	rings := writeRings(t, newKey(t))
	c := buildChain(t, pool, work, target, "enc", rings...)

	// Volumes are .pgp.vol<n>, manifests .manifest.gz.pgp.
	names := objectNames(t, client, bucket, prefix)
	if len(names) == 0 {
		t.Fatalf("no objects at %s", target)
	}
	for _, n := range names {
		if !strings.Contains(n, ".pgp") {
			t.Errorf("object %s is not encrypted; all objects: %v", n, names)
		}
	}
	// The bytes too: a volume and a manifest start with an encrypted session key.
	vol1 := manifestFor(t, work, c.src, target, c.daily3, rings...).Volumes[0].ObjectName
	wantPGPVolume(t, vol1, objectBytes(t, client, bucket, prefix+vol1))
	var manifest string
	for _, n := range names {
		if strings.Contains(n, ".manifest") && strings.Contains(n, c.daily3) {
			manifest = n
		}
	}
	if manifest == "" {
		t.Fatalf("no manifest of %s in %v", c.daily3, names)
	}
	wantPGPVolume(t, manifest, objectBytes(t, client, bucket, prefix+manifest))

	got := chainNames(itList(t, work, append(rings, target)...)[c.src])
	if want := []string{c.monthly + "<-", c.daily2 + "<-" + c.monthly, c.daily3 + "<-" + c.daily2}; strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("list holds %v, want %v", got, want)
	}

	// Another key's rings: the manifests do not open. The advice is to pass the
	// right ring, never to delete the manifest.
	wrong := writeRings(t, newKey(t))
	_, logs, err := itRunOut(cmd.ResetListJobInfo, "list", t.TempDir(), "zfs", append(append([]string{"--jsonOutput"}, wrong...), target)...)
	if err == nil {
		t.Fatalf("list with another key's rings succeeded, want an error:\n%s", logs)
	}
	if !strings.Contains(logs, "--secretKeyRingPath") || strings.Contains(logs, "delete that object") {
		t.Errorf("list with another key's rings: want advice to pass --secretKeyRingPath and none to delete:\n%s", logs)
	}

	if logs, err := itRun(cmd.ResetReceiveJobInfo, "receive", work, "--auto", "-F", c.src, target, restoredEnc); err == nil {
		t.Fatalf("receive without rings succeeded, want an error:\n%s", logs)
	}
	wantNoDataset(t, restoredEnc)

	// From an empty cache: the encrypted manifests come from the destination.
	itReceive(t, t.TempDir(), append(rings, "--auto", "-F", c.src, target, restoredEnc)...)
	wantSnapshots(t, restoredEnc, c.monthly, c.daily2, c.daily3)
	wantGUIDs(t, c.src, restoredEnc, c.monthly, c.daily2, c.daily3)
	compareDirs(t, "/"+c.src, "/"+restoredEnc)

	itReceive(t, work, append(rings, "--auto", "-F", c.src+"@"+c.daily2, target, restoredEncMid)...)
	wantSnapshots(t, restoredEncMid, c.monthly, c.daily2)
	wantGUIDs(t, c.src, restoredEncMid, c.monthly, c.daily2)
	compareDirs(t, c.daily2Clone, "/"+restoredEncMid)
}
