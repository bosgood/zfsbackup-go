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

package backup

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/op/go-logging"

	"github.com/someone1/zfsbackup-go/backends"
	"github.com/someone1/zfsbackup-go/files"
	"github.com/someone1/zfsbackup-go/log"
	"github.com/someone1/zfsbackup-go/zfs"
)

// Truly a useless backend
type mockBackend struct{}

func (m *mockBackend) Init(ctx context.Context, conf *backends.BackendConfig, opts ...backends.Option) error {
	return nil
}

func (m *mockBackend) Upload(ctx context.Context, vol *files.VolumeInfo) error {
	// make sure we can read the volume
	_, err := ioutil.ReadAll(vol)
	return err
}

func (m *mockBackend) List(ctx context.Context, prefix string) ([]string, error) {
	return nil, nil
}

func (m *mockBackend) Close() error { return nil }

func (m *mockBackend) PreDownload(ctx context.Context, objects []string) error { return nil }

func (m *mockBackend) Download(ctx context.Context, filename string) (io.ReadCloser, error) {
	return nil, nil
}

func (m *mockBackend) Delete(ctx context.Context, filename string) error { return nil }

type errTestFunc func(error) bool

func nilErrTest(e error) bool { return e == nil }

func TestRetryUploadChainer(t *testing.T) {
	_, goodVol, badVol, err := prepareTestVols()
	if err != nil {
		t.Fatalf("error preparing volumes for testing - %v", err)
	}

	testCases := []struct {
		vol   *files.VolumeInfo
		valid errTestFunc
	}{
		{
			vol:   goodVol,
			valid: nilErrTest,
		},
		{
			vol:   badVol,
			valid: os.IsNotExist,
		},
	}

	j := &files.JobInfo{
		MaxParallelUploads: 1,
		MaxBackoffTime:     5 * time.Second,
		MaxRetryTime:       1 * time.Minute,
	}

	for idx, testCase := range testCases {
		b := &mockBackend{}
		if err := b.Init(context.Background(), nil); err != nil {
			t.Errorf("%d: Expected error %v, got %v", idx, nil, err)
		} else {
			in := make(chan *files.VolumeInfo, 1)
			out, wg := retryUploadChainer(context.Background(), in, b, j, "mock://")
			in <- testCase.vol
			close(in)
			outVol := <-out
			if errResult := wg.Wait(); !testCase.valid(errResult) {
				t.Errorf("%d: error %v id not pass validation function", idx, errResult)
			} else if errResult == nil {
				// Verify we got the same vol we passed in!
				if outVol != testCase.vol {
					t.Errorf("did not get same volume passed in back out")
				}
			}
		}
	}
}

func snap(name string, t time.Time) files.SnapshotInfo {
	return files.SnapshotInfo{Name: name, CreationTime: t}
}

// fullManifest models a full backup manifest (no incremental source).
func fullManifest(s files.SnapshotInfo) *files.JobInfo {
	return &files.JobInfo{BaseSnapshot: s}
}

// incrManifest models an incremental backup manifest (target + source).
func incrManifest(target, source files.SnapshotInfo) *files.JobInfo {
	return &files.JobInfo{BaseSnapshot: target, IncrementalSnapshot: source}
}

// captureLogs points AppLogger at a buffer for the duration of the test so that
// user-facing messages can be asserted on. A fresh leveled backend defaults to
// DEBUG, so every level is captured. Stderr is restored on cleanup.
func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()

	buf := new(bytes.Buffer)
	log.AppLogger.SetBackend(logging.MultiLogger(logging.NewLogBackend(buf, "", 0)))
	t.Cleanup(func() {
		log.AppLogger.SetBackend(logging.MultiLogger(logging.NewLogBackend(os.Stderr, "", 0)))
	})
	return buf
}

// nolint:funlen // table-driven test
func TestSelectSmartSnapshots(t *testing.T) {
	base := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	day := func(n int) time.Time { return base.Add(time.Duration(n) * 24 * time.Hour) }
	const window = 720 * time.Hour // 30 days
	const off = -1 * time.Minute   // "unset" fullIfOlderThan

	testCases := []struct {
		name        string
		jobInfo     files.JobInfo
		snapshots   []files.SnapshotInfo
		destBackups [][]*files.JobInfo
		wantErr     error  // sentinel error, matched with errors.Is
		wantErrCont string // substring of a non-sentinel error message
		wantBase    string
		wantIncr    string // empty => full backup (no incremental source)
		wantLogCont string // substring the logged output must contain
	}{
		{
			name:        "fullIfOlderThan: no prior full does a full",
			jobInfo:     files.JobInfo{FullIfOlderThan: window},
			snapshots:   []files.SnapshotInfo{snap("s2", day(2)), snap("s1", day(1))},
			destBackups: [][]*files.JobInfo{{}},
			wantBase:    "s2",
		},
		{
			name:        "fullIfOlderThan: recent full does an incremental",
			jobInfo:     files.JobInfo{FullIfOlderThan: window},
			snapshots:   []files.SnapshotInfo{snap("s2", day(2)), snap("s1", day(1))},
			destBackups: [][]*files.JobInfo{{fullManifest(snap("s1", day(1)))}},
			wantBase:    "s2",
			wantIncr:    "s1",
		},
		{
			name:        "fullIfOlderThan: last full older than window does a full",
			jobInfo:     files.JobInfo{FullIfOlderThan: window},
			snapshots:   []files.SnapshotInfo{snap("s2", day(40)), snap("s1", day(0))},
			destBackups: [][]*files.JobInfo{{fullManifest(snap("s1", day(0)))}},
			wantBase:    "s2",
		},
		{
			name:    "suffix: no prior full anchors on newest monthly, not hourly",
			jobInfo: files.JobInfo{FullIfOlderThan: window, FullSnapshotSuffix: "_monthly", IncrementalSnapshotSuffix: "_daily"},
			snapshots: []files.SnapshotInfo{
				snap("autosnap_hourly", day(10).Add(2*time.Hour)),
				snap("autosnap_daily", day(10)),
				snap("autosnap_monthly", day(1)),
			},
			destBackups: [][]*files.JobInfo{{}},
			wantBase:    "autosnap_monthly",
		},
		{
			name:    "suffix: incremental targets newest daily, ignoring hourly",
			jobInfo: files.JobInfo{FullIfOlderThan: window, FullSnapshotSuffix: "_monthly", IncrementalSnapshotSuffix: "_daily"},
			snapshots: []files.SnapshotInfo{
				snap("day5_hourly", day(5).Add(3*time.Hour)),
				snap("day5_daily", day(5)),
				snap("day1_monthly", day(1)),
			},
			destBackups: [][]*files.JobInfo{{fullManifest(snap("day1_monthly", day(1)))}},
			wantBase:    "day5_daily",
			wantIncr:    "day1_monthly",
		},
		{
			name:    "suffix: rolls onto a newer monthly once window elapses",
			jobInfo: files.JobInfo{FullIfOlderThan: window, FullSnapshotSuffix: "_monthly", IncrementalSnapshotSuffix: "_daily"},
			snapshots: []files.SnapshotInfo{
				snap("day32_hourly", day(32).Add(time.Hour)),
				snap("day32_monthly", day(32)),
				snap("day20_daily", day(20)),
				snap("day1_monthly", day(1)),
			},
			destBackups: [][]*files.JobInfo{{
				incrManifest(snap("day20_daily", day(20)), snap("day1_monthly", day(1))),
				fullManifest(snap("day1_monthly", day(1))),
			}},
			wantBase: "day32_monthly",
		},
		{
			name:    "suffix: pruned incremental source falls back to a full",
			jobInfo: files.JobInfo{FullIfOlderThan: window, FullSnapshotSuffix: "_monthly", IncrementalSnapshotSuffix: "_daily"},
			snapshots: []files.SnapshotInfo{
				snap("day10_monthly", day(10)),
				snap("day9_daily", day(9)),
				snap("day1_monthly", day(1)),
				// day5_daily (the last incremental source) has been pruned locally.
			},
			destBackups: [][]*files.JobInfo{{
				incrManifest(snap("day5_daily", day(5)), snap("day1_monthly", day(1))),
				fullManifest(snap("day1_monthly", day(1))),
			}},
			wantBase: "day10_monthly",
		},
		{
			name:    "fullIfOlderThan: nothing newer than last backup is a no-op",
			jobInfo: files.JobInfo{FullIfOlderThan: window},
			snapshots: []files.SnapshotInfo{
				snap("s_day5", day(5)),
				snap("s_day1", day(1)),
			},
			destBackups: [][]*files.JobInfo{{
				incrManifest(snap("s_day5", day(5)), snap("s_day1", day(1))),
				fullManifest(snap("s_day1", day(1))),
			}},
			wantErr: ErrNoOp,
		},
		{
			name:    "explicit full with suffix anchors on newest monthly",
			jobInfo: files.JobInfo{Full: true, FullIfOlderThan: off, FullSnapshotSuffix: "_monthly"},
			snapshots: []files.SnapshotInfo{
				snap("autosnap_hourly", day(10).Add(2*time.Hour)),
				snap("autosnap_daily", day(10)),
				snap("autosnap_monthly", day(1)),
			},
			destBackups: [][]*files.JobInfo{{}},
			wantBase:    "autosnap_monthly",
		},
		{
			name:        "explicit incremental increments from last backup",
			jobInfo:     files.JobInfo{Incremental: true, FullIfOlderThan: off},
			snapshots:   []files.SnapshotInfo{snap("s2", day(2)), snap("s1", day(1))},
			destBackups: [][]*files.JobInfo{{fullManifest(snap("s1", day(1)))}},
			wantBase:    "s2",
			wantIncr:    "s1",
		},
		{
			// The recovery full must not re-use a snapshot the destination
			// already holds a full for (object names carry no timestamp, so that
			// overwrites the backup in place), nor anchor on a snapshot outside
			// the full suffix. It waits for the next full candidate instead.
			name:    "suffix: pruned source with no newer monthly waits for one",
			jobInfo: files.JobInfo{FullIfOlderThan: window, FullSnapshotSuffix: "_monthly", IncrementalSnapshotSuffix: "_daily"},
			snapshots: []files.SnapshotInfo{
				snap("day10_daily", day(10)),
				snap("day9_daily", day(9)),
				snap("day1_monthly", day(1)),
				// day5_daily (the last incremental source) has been pruned locally.
			},
			destBackups: [][]*files.JobInfo{{
				incrManifest(snap("day5_daily", day(5)), snap("day1_monthly", day(1))),
				fullManifest(snap("day1_monthly", day(1))),
			}},
			wantErr: ErrNoOp,
		},
		{
			// The window has elapsed but there is no newer full-candidate, so keep
			// taking incrementals rather than re-uploading the full that is already
			// at the destination. Report it: until a newer monthly is taken, the
			// --fullIfOlderThan guarantee is not being met.
			name:    "suffix: window elapsed but no newer monthly stays incremental",
			jobInfo: files.JobInfo{FullIfOlderThan: window, FullSnapshotSuffix: "_monthly", IncrementalSnapshotSuffix: "_daily"},
			snapshots: []files.SnapshotInfo{
				snap("day40_daily", day(40)),
				snap("day1_monthly", day(1)),
			},
			destBackups: [][]*files.JobInfo{{fullManifest(snap("day1_monthly", day(1)))}},
			wantBase:    "day40_daily",
			wantIncr:    "day1_monthly",
			wantLogCont: "the next full waits for a full backup candidate newer than the last backup",
		},
		{
			name:      "multi-destination: in sync does an incremental",
			jobInfo:   files.JobInfo{FullIfOlderThan: window},
			snapshots: []files.SnapshotInfo{snap("s2", day(2)), snap("s1", day(1))},
			destBackups: [][]*files.JobInfo{
				{fullManifest(snap("s1", day(1)))},
				{fullManifest(snap("s1", day(1)))},
			},
			wantBase: "s2",
			wantIncr: "s1",
		},
		{
			name:      "multi-destination: differing last full is out of sync",
			jobInfo:   files.JobInfo{FullIfOlderThan: window},
			snapshots: []files.SnapshotInfo{snap("s3", day(3)), snap("s2", day(2)), snap("s1", day(1))},
			destBackups: [][]*files.JobInfo{
				{fullManifest(snap("s1", day(1)))},
				{fullManifest(snap("s2", day(2)))},
			},
			wantErrCont: "out of sync",
		},
		{
			name:      "multi-destination: differing last backup refuses the incremental",
			jobInfo:   files.JobInfo{FullIfOlderThan: window},
			snapshots: []files.SnapshotInfo{snap("s4", day(4)), snap("s3", day(3)), snap("s2", day(2)), snap("s1", day(1))},
			destBackups: [][]*files.JobInfo{
				{incrManifest(snap("s2", day(2)), snap("s1", day(1))), fullManifest(snap("s1", day(1)))},
				{incrManifest(snap("s3", day(3)), snap("s1", day(1))), fullManifest(snap("s1", day(1)))},
			},
			wantErrCont: "do not match",
		},
		{
			name:        "suffix typo matching no snapshot is an error",
			jobInfo:     files.JobInfo{FullIfOlderThan: window, FullSnapshotSuffix: "_montly", IncrementalSnapshotSuffix: "_daily"},
			snapshots:   []files.SnapshotInfo{snap("day1_daily", day(1))},
			destBackups: [][]*files.JobInfo{{}},
			wantErrCont: "full backup criteria",
		},
		{
			name:        "explicit full with no matching suffix is an error",
			jobInfo:     files.JobInfo{Full: true, FullIfOlderThan: off, FullSnapshotSuffix: "_monthly"},
			snapshots:   []files.SnapshotInfo{snap("day1_daily", day(1))},
			destBackups: [][]*files.JobInfo{{}},
			wantErrCont: "full backup criteria",
		},
		{
			name:        "explicit incremental with no matching suffix is an error",
			jobInfo:     files.JobInfo{Incremental: true, FullIfOlderThan: off, IncrementalSnapshotSuffix: "_daily"},
			snapshots:   []files.SnapshotInfo{snap("day1_monthly", day(1))},
			destBackups: [][]*files.JobInfo{{fullManifest(snap("day1_monthly", day(1)))}},
			wantErrCont: "incremental backup criteria",
		},
		{
			name:        "no snapshots at all is an error",
			jobInfo:     files.JobInfo{FullIfOlderThan: window},
			snapshots:   nil,
			destBackups: [][]*files.JobInfo{{}},
			wantErrCont: "no snapshots found",
		},
		{
			// Regression: a full is due but nothing matches the full criteria, so no
			// full can ever be taken - typically a mistyped --fullSnapshotSuffix on a
			// job that already has a full at the destination. Silently extending the
			// incremental chain hides that --fullIfOlderThan stopped being honored.
			name:    "suffix: full due with no full candidate is an error",
			jobInfo: files.JobInfo{FullIfOlderThan: window, FullSnapshotSuffix: "_monthly", IncrementalSnapshotSuffix: "_daily"},
			snapshots: []files.SnapshotInfo{
				snap("day40_daily", day(40)),
				snap("day1_daily", day(1)),
			},
			destBackups: [][]*files.JobInfo{{fullManifest(snap("day1_daily", day(1)))}},
			wantErrCont: "full backup is due",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			logs := captureLogs(t)
			ji := tc.jobInfo
			err := selectSmartSnapshots(&ji, tc.snapshots, tc.destBackups, ji.Resume)
			if tc.wantErr != nil || tc.wantErrCont != "" {
				if err == nil {
					t.Fatalf("got nil error, want one")
				}
				if tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
					t.Fatalf("got err %v, want %v", err, tc.wantErr)
				}
				if tc.wantErrCont != "" && !strings.Contains(err.Error(), tc.wantErrCont) {
					t.Fatalf("got err %q, want it to contain %q", err, tc.wantErrCont)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if ji.BaseSnapshot.Name != tc.wantBase {
				t.Errorf("BaseSnapshot = %q, want %q", ji.BaseSnapshot.Name, tc.wantBase)
			}
			if ji.IncrementalSnapshot.Name != tc.wantIncr {
				t.Errorf("IncrementalSnapshot = %q, want %q", ji.IncrementalSnapshot.Name, tc.wantIncr)
			}
			if tc.wantLogCont != "" && !strings.Contains(logs.String(), tc.wantLogCont) {
				t.Errorf("logs = %q, want them to contain %q", logs.String(), tc.wantLogCont)
			}
		})
	}
}

func prepareTestVols() (payload []byte, goodVol, badVol *files.VolumeInfo, err error) {
	payload = make([]byte, 10*1024*1024)
	if _, err = rand.Read(payload); err != nil {
		return
	}
	reader := bytes.NewReader(payload)
	goodVol, err = files.CreateSimpleVolume(context.Background(), false)
	if err != nil {
		return
	}
	_, err = io.Copy(goodVol, reader)
	if err != nil {
		return
	}
	err = goodVol.Close()
	if err != nil {
		return
	}

	badVol, err = files.CreateSimpleVolume(context.Background(), false)
	if err != nil {
		return
	}
	err = badVol.Close()
	if err != nil {
		return
	}

	err = badVol.DeleteVolume()

	return payload, goodVol, badVol, err
}

// fakeZFS installs a stub `zfs` binary for the duration of the test. It answers
// the two commands reportDryRun issues: the snapshot listing (`zfs list`) and the
// size estimate (`zfs send -n -P`). Anything else exits non-zero.
func fakeZFS(t *testing.T, listing string) {
	t.Helper()

	dir, err := ioutil.TempDir("", "zfsbackup-fake-zfs")
	if err != nil {
		t.Fatalf("could not create temp dir - %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })

	listPath := filepath.Join(dir, "snapshots")
	if werr := ioutil.WriteFile(listPath, []byte(listing), 0600); werr != nil {
		t.Fatalf("could not write snapshot listing - %v", werr)
	}

	script := "#!/bin/sh\ncase \"$1\" in\nlist) cat " + listPath + " ;;\nsend) printf 'size\\t123456\\n' ;;\n*) exit 1 ;;\nesac\n"
	zfsPath := filepath.Join(dir, "zfs")
	// nolint:gosec // the stub must be executable
	if werr := ioutil.WriteFile(zfsPath, []byte(script), 0700); werr != nil {
		t.Fatalf("could not write fake zfs - %v", werr)
	}

	old := zfs.ZFSPath
	zfs.ZFSPath = zfsPath
	t.Cleanup(func() { zfs.ZFSPath = old })
}

// TestBackupDryRun verifies that Backup with dryRun=true validates the selected
// snapshots and writes nothing to the destination.
func TestBackupDryRun(t *testing.T) {
	const creation = 1700000000
	fakeZFS(t, "tank/data@snap1\t1700000000\tsnapshot\n")

	targetDir, err := ioutil.TempDir("", "zfsbackup-dryrun-target")
	if err != nil {
		t.Fatalf("could not create temp target dir - %v", err)
	}
	defer os.RemoveAll(targetDir)

	jobInfo := &files.JobInfo{
		VolumeName:   "tank/data",
		Destinations: []string{"file://" + targetDir},
		BaseSnapshot: files.SnapshotInfo{Name: "snap1", CreationTime: time.Unix(creation, 0)},
	}

	if berr := Backup(context.Background(), jobInfo, true); berr != nil {
		t.Fatalf("dry-run Backup returned error - %v", berr)
	}

	entries, rerr := ioutil.ReadDir(targetDir)
	if rerr != nil {
		t.Fatalf("could not read target dir - %v", rerr)
	}
	if len(entries) != 0 {
		t.Fatalf("dry-run wrote %d file(s) to the destination, want 0", len(entries))
	}
}

// TestBackupDryRunMissingSnapshot verifies the dry-run fails loudly when the
// selected base snapshot is not present locally.
func TestBackupDryRunMissingSnapshot(t *testing.T) {
	fakeZFS(t, "tank/data@other\t1700000000\tsnapshot\n")

	jobInfo := &files.JobInfo{
		VolumeName:   "tank/data",
		Destinations: []string{"file:///nonexistent"},
		BaseSnapshot: files.SnapshotInfo{Name: "snap1", CreationTime: time.Unix(1700000000, 0)},
	}

	err := Backup(context.Background(), jobInfo, true)
	if err == nil {
		t.Fatal("got nil error for a missing base snapshot, want one")
	}
	if !strings.Contains(err.Error(), "base snapshot does not exist") {
		t.Fatalf("got err %q, want it to name the missing base snapshot", err)
	}
}

func TestRedactURI(t *testing.T) {
	testCases := []struct {
		in   string
		want string
	}{
		{"s3://bucket/prefix", "s3://bucket/prefix"},
		{"file:///tmp/backups", "file:///tmp/backups"},
		{"ssh://user@example.org/path", "ssh://user@example.org/path"},
		{"ssh://user:hunter2@example.org/path", "ssh://user:xxxxx@example.org/path"},
	}
	for _, tc := range testCases {
		if got := redactURI(tc.in); got != tc.want {
			t.Errorf("redactURI(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestJoinURI guards against filepath.Join, which collapses the "//" in a scheme.
func TestJoinURI(t *testing.T) {
	if got, want := joinURI("s3://bucket/prefix", "obj"), "s3://bucket/prefix/obj"; got != want {
		t.Errorf("joinURI = %q, want %q", got, want)
	}
	if got, want := joinURI("s3://bucket/prefix/", "obj"), "s3://bucket/prefix/obj"; got != want {
		t.Errorf("joinURI with trailing slash = %q, want %q", got, want)
	}
	if got, want := joinURI("ssh://u:pw@host/p", "obj"), "ssh://u:xxxxx@host/p/obj"; got != want {
		t.Errorf("joinURI did not redact = %q, want %q", got, want)
	}
}

func TestPendingVolumes(t *testing.T) {
	p := newPendingVolumes(1)
	p.add()
	p.done()
	select {
	case <-p.zero:
		t.Fatal("zero closed with one volume still pending")
	default:
	}
	p.done()
	select {
	case <-p.zero:
	default:
		t.Fatal("zero not closed at count 0")
	}
	// A straggler after a failed pipeline must not panic.
	p.done()
}
