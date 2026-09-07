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
	"testing"
	"time"

	"github.com/someone1/zfsbackup-go/backends"
	"github.com/someone1/zfsbackup-go/files"
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

// bookmark models a ZFS bookmark, which is a valid incremental source but can
// never be a backup base.
func bookmark(name string, t time.Time) files.SnapshotInfo {
	return files.SnapshotInfo{Name: name, CreationTime: t, Bookmark: true}
}

// fullManifest models a full backup manifest (no incremental source).
func fullManifest(s files.SnapshotInfo) *files.JobInfo {
	return &files.JobInfo{BaseSnapshot: s}
}

// incrManifest models an incremental backup manifest (target + source).
func incrManifest(target, source files.SnapshotInfo) *files.JobInfo {
	return &files.JobInfo{BaseSnapshot: target, IncrementalSnapshot: source}
}

// nolint:funlen // table-driven test
func TestSelectSmartSnapshots(t *testing.T) {
	base := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	day := func(n int) time.Time { return base.Add(time.Duration(n) * 24 * time.Hour) }
	const window = 720 * time.Hour // 30 days
	const off = -1 * time.Minute    // "unset" fullIfOlderThan

	testCases := []struct {
		name        string
		jobInfo     files.JobInfo
		snapshots   []files.SnapshotInfo
		destBackups [][]*files.JobInfo
		wantErr     error
		wantBase    string
		wantIncr    string // empty => full backup (no incremental source)
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
			// A full backup is due, but the full suffix matches no snapshot at
			// all (typo, or the snapshotting tool stopped producing them). This
			// must fail rather than silently extending the incremental chain.
			name: "suffix: full due but no full candidate at all errors",
			jobInfo: files.JobInfo{
				FullIfOlderThan: window, FullSnapshotSuffix: "_monthy", IncrementalSnapshotSuffix: "_daily",
			},
			snapshots: []files.SnapshotInfo{
				snap("autosnap_d90_daily", day(90)),
				snap("autosnap_d1_monthly", day(1)),
			},
			destBackups: [][]*files.JobInfo{{fullManifest(snap("autosnap_d1_monthly", day(1)))}},
			wantErr:     ErrNoFullCandidate,
		},
		{
			// A full is due but the next monthly has not been taken yet, so the
			// newest full candidate is the one already backed up. Defer the full
			// and keep incrementing (the code logs a notice in this case).
			name: "suffix: full due but no newer full candidate defers to an incremental",
			jobInfo: files.JobInfo{
				FullIfOlderThan: window, FullSnapshotSuffix: "_monthly", IncrementalSnapshotSuffix: "_daily",
			},
			snapshots: []files.SnapshotInfo{
				snap("autosnap_d40_daily", day(40)),
				snap("autosnap_d1_monthly", day(1)),
			},
			destBackups: [][]*files.JobInfo{{fullManifest(snap("autosnap_d1_monthly", day(1)))}},
			wantBase:    "autosnap_d40_daily",
			wantIncr:    "autosnap_d1_monthly",
		},
		{
			// --increment must degrade to a full when its source was pruned,
			// matching the fullIfOlderThan path, instead of selecting a snapshot
			// that no longer exists and failing later in `zfs send`.
			name: "increment: pruned source falls back to a full",
			jobInfo: files.JobInfo{
				Incremental: true, FullIfOlderThan: off,
				FullSnapshotSuffix: "_monthly", IncrementalSnapshotSuffix: "_daily",
			},
			snapshots: []files.SnapshotInfo{
				snap("autosnap_d10_daily", day(10)),
				snap("autosnap_d1_monthly", day(1)),
			},
			destBackups: [][]*files.JobInfo{{fullManifest(snap("autosnap_d5_daily", day(5)))}},
			wantBase:    "autosnap_d1_monthly",
			wantIncr:    "", // fell back to a full
		},
		{
			name: "increment: pruned source with no full candidate errors",
			jobInfo: files.JobInfo{
				Incremental: true, FullIfOlderThan: off,
				FullSnapshotSuffix: "_monthly", IncrementalSnapshotSuffix: "_daily",
			},
			snapshots:   []files.SnapshotInfo{snap("autosnap_d10_daily", day(10))},
			destBackups: [][]*files.JobInfo{{fullManifest(snap("autosnap_d5_daily", day(5)))}},
			wantErr:     ErrNoFullCandidate,
		},
		{
			// A bookmark of the pruned source keeps the incremental chain alive.
			name: "increment: bookmarked source keeps the incremental",
			jobInfo: files.JobInfo{
				Incremental: true, FullIfOlderThan: off,
				FullSnapshotSuffix: "_monthly", IncrementalSnapshotSuffix: "_daily",
			},
			snapshots: []files.SnapshotInfo{
				snap("autosnap_d10_daily", day(10)),
				bookmark("autosnap_d5_daily", day(5)),
				snap("autosnap_d1_monthly", day(1)),
			},
			destBackups: [][]*files.JobInfo{{fullManifest(snap("autosnap_d5_daily", day(5)))}},
			wantBase:    "autosnap_d10_daily",
			wantIncr:    "autosnap_d5_daily",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			ji := tc.jobInfo
			err := selectSmartSnapshots(&ji, tc.snapshots, tc.destBackups)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("got err %v, want %v", err, tc.wantErr)
			}
			if tc.wantErr != nil {
				return
			}
			if ji.BaseSnapshot.Name != tc.wantBase {
				t.Errorf("BaseSnapshot = %q, want %q", ji.BaseSnapshot.Name, tc.wantBase)
			}
			if ji.IncrementalSnapshot.Name != tc.wantIncr {
				t.Errorf("IncrementalSnapshot = %q, want %q", ji.IncrementalSnapshot.Name, tc.wantIncr)
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
