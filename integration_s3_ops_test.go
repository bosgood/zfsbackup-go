//go:build integration

package main

import (
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/someone1/zfsbackup-go/files"
)

// TestIntegrationS3ListClean runs list with its filters, and clean against a
// healthy destination, one with an orphan volume and an unrecognized object,
// and one with a broken backup set.
func TestIntegrationS3ListClean(t *testing.T) {
	pool := testPool(t)
	client, bucket, prefix := itBucket(t, "zfsbackup-it-ops")
	target := "s3://" + bucket + "/" + prefix
	restoredOps := pool + "/restoredOps"
	restoredOps2 := pool + "/restoredOps2"
	destroyAfter(t, restoredOps, restoredOps2)
	work := t.TempDir()
	c := buildChain(t, pool, work, target, "ops")
	chain := strings.Join([]string{c.monthly + "<-", c.daily2 + "<-" + c.monthly, c.daily3 + "<-" + c.daily2}, " ")

	hourLater := time.Now().Add(time.Hour).Format("2006-01-02T15:04:05")
	for _, tc := range []struct {
		args []string
		want string // the chain of c.src, "" = no datasets at all
	}{
		{nil, chain},
		{[]string{"--volumeName", c.src}, chain},
		{[]string{"--volumeName", pool + "/s*"}, chain},
		{[]string{"--volumeName", pool + "/nothing"}, ""},
		{[]string{"--after", hourLater}, ""},
		{[]string{"--before", hourLater}, chain},
	} {
		got := itList(t, work, append(tc.args, target)...)
		switch {
		case tc.want == "" && len(got) != 0:
			t.Errorf("list %v: got %d datasets, want none", tc.args, len(got))
		case tc.want != "" && (len(got) != 1 || strings.Join(chainNames(got[c.src]), " ") != tc.want):
			t.Errorf("list %v: got %d datasets, %s holds %v; want 1, %s", tc.args, len(got), c.src, chainNames(got[c.src]), tc.want)
		}
	}

	clean := func(args ...string) string {
		t.Helper()
		logs, err := itClean(t, work, append(args, target)...)
		if err != nil {
			t.Fatalf("clean %v: %v\n%s", args, err, logs)
		}
		return logs
	}
	wantLog := func(logs string, want ...string) {
		t.Helper()
		for _, w := range want {
			if !strings.Contains(logs, w) {
				t.Errorf("want %q in the log:\n%s", w, logs)
			}
		}
	}
	wantObjects := func(want []string) {
		t.Helper()
		if got := objectNames(t, client, bucket, prefix); strings.Join(got, "\n") != strings.Join(want, "\n") {
			t.Fatalf("objects at %s:\n%s\nwant:\n%s", target, strings.Join(got, "\n"), strings.Join(want, "\n"))
		}
	}

	healthy := objectNames(t, client, bucket, prefix)
	wantLog(clean("--dry-run"), "Dry-run: would delete 0 objects")
	wantObjects(healthy)

	// An orphan: a volume name of the daily3 set that its manifest does not list.
	vol1 := manifestFor(t, work, c.src, target, c.daily3).Volumes[0].ObjectName
	orphan := strings.TrimSuffix(vol1, "vol1") + "vol77"
	if orphan == vol1+"vol77" {
		t.Fatalf("volume 1 of %s is %s, want a name ending in vol1", c.daily3, vol1)
	}
	putObject(t, client, bucket, prefix+orphan, []byte("orphan"))
	putObject(t, client, bucket, prefix+"notes.txt", []byte("not a volume"))
	littered := objectNames(t, client, bucket, prefix)
	wantLog(clean("--dry-run"),
		"Would delete "+strings.TrimSuffix(target, "/")+"/"+orphan+".",
		"Skipping unrecognized object notes.txt")
	wantObjects(littered)

	clean()
	cleaned := append(slices.Clone(healthy), "notes.txt")
	sort.Strings(cleaned)
	wantObjects(cleaned)
	for _, snap := range []string{c.monthly, c.daily2, c.daily3} {
		requireVolumes(t, manifestFor(t, work, c.src, target, snap), cleaned)
	}
	itReceive(t, work, "--auto", "-F", c.src, target, restoredOps)
	wantSnapshots(t, restoredOps, c.monthly, c.daily2, c.daily3)
	compareDirs(t, "/"+c.src, "/"+restoredOps)

	// A broken set: clean keeps it unless --force.
	daily3 := manifestFor(t, work, c.src, target, c.daily3)
	deleteObject(t, client, bucket, prefix+daily3.Volumes[1].ObjectName)
	broken := objectNames(t, client, bucket, prefix)
	wantLog(clean(), "missing volume "+daily3.Volumes[1].ObjectName)
	wantObjects(broken)
	if got := itList(t, work, target)[c.src]; len(got) != 3 {
		t.Fatalf("list after clean without --force: %v, want 3 backups", chainNames(got))
	}

	clean("--force")
	var want []string
	for _, n := range broken {
		if !strings.Contains(n, c.daily3) {
			want = append(want, n)
		}
	}
	wantObjects(want)
	if got := itList(t, work, target)[c.src]; strings.Join(chainNames(got), " ") != c.monthly+"<- "+c.daily2+"<-"+c.monthly {
		t.Fatalf("list after clean --force: %v, want monthly and daily2", chainNames(got))
	}
	for _, snap := range []string{c.monthly, c.daily2} {
		requireVolumes(t, manifestFor(t, work, c.src, target, snap), want)
	}
	itReceive(t, work, "--auto", "-F", c.src, target, restoredOps2)
	wantSnapshots(t, restoredOps2, c.monthly, c.daily2)
	compareDirs(t, c.daily2Clone, "/"+restoredOps2)
}

// requireVolumes fails unless every volume of b is in names.
func requireVolumes(t *testing.T, b *files.JobInfo, names []string) {
	t.Helper()
	for _, v := range b.Volumes {
		if !slices.Contains(names, v.ObjectName) {
			t.Fatalf("volume %s of %s is not at the destination", v.ObjectName, b.BaseSnapshot.Name)
		}
	}
}
