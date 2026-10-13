//go:build integration

package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/someone1/zfsbackup-go/cmd"
)

// TestIntegrationS3CleanMany runs clean on 1100 orphans: more than one listing
// page of 1000, so clean must page to see the set's volumes and every orphan.
// Subtest Interrupted: a SIGTERM in the middle of 3000 deletes stops clean with
// an error, the backup intact; the next clean finishes.
func TestIntegrationS3CleanMany(t *testing.T) {
	pool := testPool(t)
	client, bucket, prefix := itBucket(t, "zfsbackup-it-clmany")
	target := "s3://" + bucket + "/" + prefix
	src := pool + "/srclmany"
	restored := pool + "/restoredClMany"
	restored2 := pool + "/restoredClMany2"
	destroyAfter(t, src, restored, restored2)
	work := t.TempDir()

	zfsRun(t, "create", "-o", "mountpoint=/"+src, src)
	writeFile(t, "/"+src, "a.bin", 3<<20)
	m := snapshot(t, src, "monthly")
	if logs, err := itSend(t, work, "--volsize", "1", "--compressor", "", src+"@"+m, target); err != nil {
		t.Fatalf("send: %v\n%s", err, logs)
	}
	volumes, manifests, list := countObjects(t, client, bucket, prefix)
	if volumes < 3 || manifests != 1 {
		t.Fatalf("%s holds %d volumes and %d manifests, want 3+ and 1:\n%s", target, volumes, manifests, list)
	}
	sent := objectNames(t, client, bucket, prefix)
	set := manifestFor(t, work, src, target, m)
	base := strings.TrimSuffix(set.Volumes[0].ObjectName, "vol1")
	orphans := func(from, to int) []string {
		keys := make([]string, 0, to-from)
		for i := from; i < to; i++ {
			keys = append(keys, prefix+base+"vol"+fmt.Sprint(i))
		}
		return keys
	}
	restore := func(dataset string) {
		t.Helper()
		requireVolumes(t, manifestFor(t, work, src, target, m), objectNames(t, client, bucket, prefix))
		if got := itList(t, work, target)[src]; len(got) != 1 {
			t.Fatalf("list: %v, want one backup", chainNames(got))
		}
		itReceive(t, t.TempDir(), "-F", src+"@"+m, target, dataset)
		wantGUIDs(t, src, dataset, m)
		compareDirs(t, "/"+src, "/"+dataset)
	}

	putMany(t, client, bucket, orphans(100, 1200), []byte("orphan!!"))
	if got := len(objectNames(t, client, bucket, prefix)); got != len(sent)+1100 {
		t.Fatalf("%s holds %d objects, want %d", target, got, len(sent)+1100)
	}

	logs, err := itClean(t, work, "--dry-run", target)
	if err != nil {
		t.Fatalf("clean --dry-run: %v\n%s", err, logs)
	}
	wantInLog(t, logs, "Dry-run: would delete 1100 objects")
	wantNotInLog(t, logs, "missing volume", "Pass the --force flag")

	logs, err = itClean(t, work, target)
	if err != nil {
		t.Fatalf("clean: %v\n%s", err, logs)
	}
	wantInLog(t, logs, "Starting to delete 1100 objects")
	wantObjectsAt(t, client, bucket, prefix, sent)
	restore(restored)

	t.Run("Interrupted", func(t *testing.T) {
		const n = 3000
		putMany(t, client, bucket, orphans(100, 100+n), []byte("orphan!!"))
		ctx, stop := cmd.SignalContext(context.Background())
		defer stop()
		type result struct {
			logs string
			err  error
		}
		done := make(chan result, 1)
		finished := make(chan struct{})
		go func() {
			defer close(finished)
			logs, err := itCleanCtx(ctx, t, work, target)
			done <- result{logs, err}
		}()
		// Never leave the clean running past the test, whatever fails below: its
		// deferred reset of the package-global flags would land in the next test's run.
		t.Cleanup(func() {
			stop()
			<-finished
		})

		// Signal when at least minDone deletes have happened and at least minLeft
		// orphans are left: the signal then lands in the middle of the run, with room
		// for the deletes between the last count and the Kill.
		const minDone, minLeft = 100, 1000
		deadline := time.Now().Add(time.Minute)
		left := n
		for left > n-minDone {
			select {
			case r := <-done:
				t.Fatalf("clean returned before %d deletes were seen (%v); the test cannot signal it:\n%s", minDone, r.err, r.logs)
			default:
			}
			if time.Now().After(deadline) {
				t.Fatalf("fewer than %d deletes in a minute (%d orphans left)", minDone, left)
			}
			time.Sleep(50 * time.Millisecond)
			left = len(objectNames(t, client, bucket, prefix)) - len(sent)
		}
		if left < minLeft {
			t.Fatalf("only %d of %d orphans left when %d deletes were first seen: too fast to signal in the middle; raise n", left, n, minDone)
		}
		// One signal only: a second one exits the test binary.
		if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
			t.Fatal(err)
		}
		r := <-done
		if r.err == nil {
			t.Fatalf("clean stopped by SIGTERM returned no error:\n%s", r.logs)
		}
		wantInLog(t, r.logs, "Received SIGTERM; stopping.")
		left = len(objectNames(t, client, bucket, prefix))
		t.Logf("SIGTERM: %v; %d of %d orphans left", r.err, left-len(sent), n)
		if left <= len(sent) {
			t.Errorf("after the SIGTERM no orphan is left: the deletes did not stop")
		}
		requireVolumes(t, manifestFor(t, work, src, target, m), objectNames(t, client, bucket, prefix))
		stop()

		if logs, err := itClean(t, work, target); err != nil {
			t.Fatalf("clean after the SIGTERM: %v\n%s", err, logs)
		}
		wantObjectsAt(t, client, bucket, prefix, sent)
		restore(restored2)
	})
}
