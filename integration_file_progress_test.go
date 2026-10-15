//go:build integration

package main

import (
	"strings"
	"testing"

	"github.com/dustin/go-humanize"

	"github.com/bosgood/zfsbackup-go/cmd"
	"github.com/bosgood/zfsbackup-go/config"
)

// TestIntegrationFileProgress checks the progress lines against real zfs, whose `zfs send -n -P`
// estimate is not exact (the fake's is): send and receive each print strictly increasing whole
// percents that end at one 100, in temp-file and in pipe mode. Throttled, the send lasts some
// seconds: then its lines carry the rate and the time left, and the 100% line the time taken.
func TestIntegrationFileProgress(t *testing.T) {
	pool := testPool(t)
	src := pool + "/progresssrc"
	destroyAfter(t, src)
	zfsRun(t, "create", "-o", "mountpoint=/"+src, src)
	writeFile(t, "/"+src, "a.bin", 6<<20)
	snap := snapshot(t, src, "daily")

	for _, tc := range []struct{ name, dest string }{
		{"TempFile", "progressfile"},
		{"Pipe", "progresspipe"},
		{"Throttled", "progressslow"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dest := newFileDest(t, pool, tc.dest)
			restored := pool + "/progressRestored" + tc.name
			destroyAfter(t, restored)
			args := []string{"--volsize", "1", "--compressor", "", "--full", src, dest.target}
			switch tc.name {
			case "Pipe":
				args = append([]string{"--maxFileBuffer", "0"}, args...)
			case "Throttled":
				// 2 MB/s: some 3 s for 6 MiB. The flag sets a global that nothing resets.
				oldBucket := config.BackupUploadBucket
				t.Cleanup(func() { config.BackupUploadBucket = oldBucket })
				args = append([]string{"--maxParallelUploads", "1", "--maxUploadSpeed", "2000"}, args...)
			}
			logs, err := itSend(t, t.TempDir(), args...)
			if err != nil {
				t.Fatalf("send: %v\n%s", err, logs)
			}
			label := "Uploading " + src + "@" + snap + " to " + dest.target
			lines := progressLines(logs, label)
			t.Logf("send: %d lines, the last: %q", len(lines), lines[len(lines)-1])
			wantProgressTo100(t, progressPercents(t, logs, label), logs)
			for _, l := range lines {
				if !strings.Contains(l, " of an estimated ") {
					t.Errorf("a send line without the estimate: %q", l)
				}
			}
			if tc.name == "Throttled" {
				var left []string
				for _, l := range lines {
					if strings.HasSuffix(l, " left") {
						left = append(left, l)
					}
				}
				if len(left) == 0 {
					t.Errorf("no line of a throttled send has the time left:\n%s", strings.Join(lines, "\n"))
				} else {
					t.Logf("send, with the time left: %q", left[len(left)/2])
				}
				if last := lines[len(lines)-1]; !pace100RE.MatchString(last) {
					t.Errorf("the 100%% line of a throttled send is %q, want a rate and the time taken", last)
				}
			}

			var stored uint64
			for _, v := range newestBackup(t, src, dest.target).Volumes {
				stored += v.Size
			}
			recv := []string{"-F", src + "@" + snap, dest.target, restored}
			if tc.name == "Pipe" {
				recv = append([]string{"--maxFileBuffer", "0"}, recv...)
			}
			logs, err = itRun(cmd.ResetReceiveJobInfo, "receive", t.TempDir(), recv...)
			if err != nil {
				t.Fatalf("receive: %v\n%s", err, logs)
			}
			label = "Downloading " + src + "@" + snap + " from " + dest.target
			lines = progressLines(logs, label)
			wantProgressTo100(t, progressPercents(t, logs, label), logs)
			t.Logf("receive: %d lines, the last: %q", len(lines), lines[len(lines)-1])
			if want := " of " + humanize.IBytes(stored) + ")"; !strings.Contains(lines[len(lines)-1], want) {
				t.Errorf("the last receive line is %q, want the volumes' %q", lines[len(lines)-1], want)
			}
			compareDirs(t, "/"+src, "/"+restored)
		})
	}
}
