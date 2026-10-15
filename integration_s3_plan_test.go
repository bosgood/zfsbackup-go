//go:build integration

// Copyright © 2016 Prateek Malhotra (someone1@gmail.com)
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in
// all copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
// THE SOFTWARE.

package main

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/bosgood/zfsbackup-go/cmd"
)

// TestIntegrationS3Plan runs plan against the real pool and S3 before each
// smart send: the run it predicts is the one send then makes. plan uploads
// nothing and takes no lock. Also --jsonOutput and --schedule on real input.
func TestIntegrationS3Plan(t *testing.T) {
	pool := testPool(t)
	client, bucket, prefix := itBucket(t, "zfsbackup-it-plan")
	target := "s3://" + bucket + "/" + prefix
	src := pool + "/plansrc"
	work := t.TempDir()
	newSource(t, src, 2<<20)
	m1 := snapshot(t, src, "monthly")
	d1 := snapshot(t, src, "daily")
	snapshot(t, src, "hourly")
	smart := []string{"--fullIfOlderThan", "4320h", "--fullSnapshotSuffix", "_monthly", "--incrementalSnapshotSuffix", "_daily"}

	plan := func(args ...string) string {
		t.Helper()
		out, logs, err := itRunOut(cmd.ResetSendJobInfo, "plan", work, "zfs", append(slices.Clone(smart), args...)...)
		if err != nil {
			t.Fatalf("plan %s: %v\n%s", strings.Join(args, " "), err, logs)
		}
		return out
	}
	wantPlan := func(out, want string) {
		t.Helper()
		if !strings.Contains(out, "  "+want) || !strings.Contains(out, "checks: OK") {
			t.Fatalf("plan printed:\n%s\nwant a line with %q and checks: OK", out, want)
		}
	}
	send := func() {
		t.Helper()
		mustSend(t, work, append(append(slices.Clone(smart), plainSend...), src, target)...)
	}

	// Nothing at the destination: a full of the monthly.
	wantPlan(plan(src, target), "FULL  "+m1)
	if volumes, manifests, list := countObjects(t, client, bucket, prefix); volumes+manifests != 0 {
		t.Fatalf("plan uploaded %d volumes and %d manifests:\n%s", volumes, manifests, list)
	}
	send()
	if got := newestBackup(t, src, target); got.BaseSnapshot.Name != m1 || got.IncrementalSnapshot.Name != "" {
		t.Fatalf("send made %s from %q, plan said FULL %s", got.BaseSnapshot.Name, got.IncrementalSnapshot.Name, m1)
	}

	// The daily after it: an incremental from the monthly; the hourly is skipped.
	wantPlan(plan(src, target), "INCR  "+d1+"  from "+m1)
	send()
	if got := newestBackup(t, src, target); got.BaseSnapshot.Name != d1 || got.IncrementalSnapshot.Name != m1 {
		t.Fatalf("send made %s from %q, plan said INCR %s from %s", got.BaseSnapshot.Name, got.IncrementalSnapshot.Name, d1, m1)
	}
	wantPlan(plan(src, target), "NOOP")

	// A new daily: plan and send agree again.
	writeFile(t, "/"+src, "b.bin", 1<<20)
	d2 := snapshot(t, src, "daily")
	wantPlan(plan(src, target), "INCR  "+d2+"  from "+d1)
	send()
	if got := newestBackup(t, src, target); got.BaseSnapshot.Name != d2 || got.IncrementalSnapshot.Name != d1 {
		t.Fatalf("send made %s from %q, plan said INCR %s from %s", got.BaseSnapshot.Name, got.IncrementalSnapshot.Name, d2, d1)
	}

	// --jsonOutput is one JSON document with the runs.
	var doc map[string]json.RawMessage
	if out := plan("--jsonOutput", src, target); json.Unmarshal([]byte(out), &doc) != nil || len(doc) == 0 {
		t.Fatalf("plan --jsonOutput printed no JSON object:\n%s", out)
	}

	// --schedule projects runs from the real snapshots and manifests.
	until := time.Now().UTC().Add(3 * 24 * time.Hour).Format(time.RFC3339)
	out := plan("--schedule", "policy=hourly=36,daily=30,monthly=3,until="+until+",every=24h", src, target)
	if !strings.Contains(out, "checks: OK") {
		t.Fatalf("plan --schedule printed:\n%s\nwant checks: OK", out)
	}

	volumes, manifests, list := countObjects(t, client, bucket, prefix)
	if manifests != 3 {
		t.Fatalf("after three sends and the plans: %d volumes and %d manifests, want 3 manifests:\n%s", volumes, manifests, list)
	}
	restoreAll(t, src, target, pool+"/plan-restored", m1, d1, d2)
}
