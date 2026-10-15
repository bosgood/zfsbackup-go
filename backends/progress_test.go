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

package backends

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/op/go-logging"

	"github.com/bosgood/zfsbackup-go/config"
	"github.com/bosgood/zfsbackup-go/files"
	"github.com/bosgood/zfsbackup-go/log"
)

const mib = 1 << 20

// captureLog sends the logger to a buffer, each line prefixed with its level.
func captureLog(t *testing.T) *syncBuffer {
	t.Helper()
	buf := new(syncBuffer)
	format := logging.MustStringFormatter("%{level} %{message}")
	log.AppLogger.SetBackend(logging.AddModuleLevel(logging.NewBackendFormatter(logging.NewLogBackend(buf, "", 0), format)))
	t.Cleanup(func() {
		log.AppLogger.SetBackend(logging.MultiLogger(logging.NewLogBackend(os.Stderr, "", 0)))
	})
	return buf
}

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) lines(level string) []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []string
	for _, l := range strings.Split(b.buf.String(), "\n") {
		if rest, ok := strings.CutPrefix(l, level+" "); ok {
			out = append(out, rest)
		}
	}
	return out
}

var percentRE = regexp.MustCompile(`: (\d+)% \(`)

// percents are the percents of the Notice lines, in order.
func percents(t *testing.T, buf *syncBuffer) []int {
	t.Helper()
	var out []int
	for _, l := range buf.lines("NOTICE") {
		m := percentRE.FindStringSubmatch(l)
		if m == nil {
			t.Fatalf("a Notice line is not a progress line: %q", l)
		}
		n, _ := strconv.Atoi(m[1])
		out = append(out, n)
	}
	return out
}

// wantIncreasing fails unless got is strictly increasing and ends at last.
func wantIncreasing(t *testing.T, got []int, last int) {
	t.Helper()
	for i := 1; i < len(got); i++ {
		if got[i] <= got[i-1] {
			t.Fatalf("percents not strictly increasing at %d: %v", i, got)
		}
	}
	if len(got) == 0 || got[len(got)-1] != last {
		t.Fatalf("percents %v, want them to end at %d", got, last)
	}
}

func volumeTempdir(t *testing.T) {
	t.Helper()
	old := config.BackupTempdir
	config.BackupTempdir = t.TempDir()
	t.Cleanup(func() { config.BackupTempdir = old })
}

// testVolume is a closed volume of size stored bytes and raw stream bytes, opened for reading.
func testVolume(t *testing.T, name string, size, raw uint64) *files.VolumeInfo {
	t.Helper()
	vol, err := files.CreateSimpleVolume(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = vol.DeleteVolume() })
	if _, err = vol.Write(bytes.Repeat([]byte{'x'}, int(size))); err != nil {
		t.Fatal(err)
	}
	if err = vol.Close(); err != nil {
		t.Fatal(err)
	}
	vol.ObjectName = name
	vol.ZFSStreamBytes = raw
	if err = vol.OpenVolume(); err != nil {
		t.Fatal(err)
	}
	return vol
}

func manifestVolume(t *testing.T, final bool) *files.VolumeInfo {
	t.Helper()
	vol := testVolume(t, "manifests|x", 1024, 0)
	vol.IsManifest = true
	vol.IsFinalManifest = final
	return vol
}

// fakeBackend reads a volume to EOF in 4 KiB pieces on Upload and serves objects on Download.
type fakeBackend struct {
	Backend
	mu       sync.Mutex
	uploads  []string
	objects  map[string][]byte
	onUpload func(vol *files.VolumeInfo, attempt int) error // optional; an error fails the upload
	failRead map[string]int                                 // Download: fail this many first readers at half
}

func (f *fakeBackend) Upload(_ context.Context, vol *files.VolumeInfo) error {
	f.mu.Lock()
	attempt := 0
	for _, n := range f.uploads {
		if n == vol.ObjectName {
			attempt++
		}
	}
	f.uploads = append(f.uploads, vol.ObjectName)
	hook := f.onUpload
	f.mu.Unlock()
	if hook != nil {
		if err := hook(vol, attempt); err != nil {
			return err
		}
	}
	buf := make([]byte, 4096)
	for {
		_, err := vol.Read(buf)
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

func (f *fakeBackend) Download(_ context.Context, name string) (io.ReadCloser, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	data, ok := f.objects[name]
	if !ok {
		return nil, fmt.Errorf("no object %s", name)
	}
	if f.failRead[name] > 0 {
		f.failRead[name]--
		return io.NopCloser(io.MultiReader(bytes.NewReader(data[:len(data)/2]), errReader{})), nil
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("connection reset") }

// clockPerMiB makes p's clock advance step per MiB on the wire. now is called with p.mu held.
func clockPerMiB(p *Progress, step time.Duration) {
	start := p.start
	p.now = func() time.Time { return start.Add(time.Duration(p.wire) * step / mib) }
}

func upload(t *testing.T, b Backend, vols ...*files.VolumeInfo) {
	t.Helper()
	for _, vol := range vols {
		if err := b.Upload(context.Background(), vol); err != nil {
			t.Fatal(err)
		}
	}
}

func volumes(t *testing.T, n int, size, raw uint64) []*files.VolumeInfo {
	t.Helper()
	var vols []*files.VolumeInfo
	for i := range n {
		vols = append(vols, testVolume(t, fmt.Sprintf("vol%d", i), size, raw))
	}
	return vols
}

func TestProgressWholePercentsOnce(t *testing.T) {
	volumeTempdir(t)
	buf := captureLog(t)
	p := NewUploadProgress("Uploading ds@s to file:///x", 4*mib, nil)
	b := WithUploadProgress(&fakeBackend{}, p)
	upload(t, b, volumes(t, 4, mib, mib)...)
	upload(t, b, manifestVolume(t, true))

	got := percents(t, buf)
	if len(got) != 100 {
		t.Fatalf("got %d lines %v, want 1..100", len(got), got)
	}
	for i, pct := range got {
		if pct != i+1 {
			t.Fatalf("line %d is %d%%, want %d%%: %v", i, pct, i+1, got)
		}
	}
	if l := buf.lines("NOTICE")[99]; !strings.HasPrefix(l, "Uploading ds@s to file:///x: 100% (4.0 MiB of an estimated 4.0 MiB)") {
		t.Errorf("the last line is %q", l)
	}
}

func TestProgressConcurrentReaders(t *testing.T) {
	volumeTempdir(t)
	buf := captureLog(t)
	p := NewUploadProgress("up", 8*mib, nil)
	b := WithUploadProgress(&fakeBackend{}, p)
	var wg sync.WaitGroup
	for _, vol := range volumes(t, 8, mib, mib) {
		wg.Go(func() {
			if err := b.Upload(context.Background(), vol); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	upload(t, b, manifestVolume(t, true))
	wantIncreasing(t, percents(t, buf), 100)
}

func TestProgressRetryNeverGoesBack(t *testing.T) {
	volumeTempdir(t)
	buf := captureLog(t)
	p := NewUploadProgress("up", 4*mib, nil)
	vols := volumes(t, 4, mib, mib)
	var retryStart uint64 = 1
	var retryTracked bool
	fake := &fakeBackend{onUpload: func(vol *files.VolumeInfo, attempt int) error {
		if vol.ObjectName != "vol2" {
			return nil
		}
		if attempt == 0 {
			if _, err := io.CopyN(io.Discard, vol, mib/2); err != nil {
				return err
			}
			return errors.New("upload cut")
		}
		p.mu.Lock()
		retryStart, retryTracked = p.inflight["vol2"]
		p.mu.Unlock()
		return nil
	}}
	b := WithUploadProgress(fake, p)
	upload(t, b, vols[0], vols[1])
	if err := b.Upload(context.Background(), vols[2]); err == nil {
		t.Fatal("the first upload of vol2 did not fail")
	}
	// As volUploadWrapper does: reopen and try again.
	if err := vols[2].Close(); err != nil {
		t.Fatal(err)
	}
	if err := vols[2].OpenVolume(); err != nil {
		t.Fatal(err)
	}
	upload(t, b, vols[2], vols[3])
	upload(t, b, manifestVolume(t, true))

	if !retryTracked || retryStart != 0 {
		t.Errorf("the retry of vol2 started at %d (tracked %v), want 0", retryStart, retryTracked)
	}
	wantIncreasing(t, percents(t, buf), 100)
}

func TestProgressEstimateTooLow(t *testing.T) {
	volumeTempdir(t)
	buf := captureLog(t)
	p := NewUploadProgress("up", 2*mib, nil)
	b := WithUploadProgress(&fakeBackend{}, p)
	upload(t, b, volumes(t, 4, mib, mib)...)
	got := percents(t, buf)
	wantIncreasing(t, got, 99)
	upload(t, b, manifestVolume(t, true))
	got = percents(t, buf)
	wantIncreasing(t, got, 100)
	if l := buf.lines("NOTICE")[len(got)-1]; !strings.Contains(l, "(2.0 MiB of an estimated 2.0 MiB)") {
		t.Errorf("the 100%% line is %q, want the bytes capped at the estimate", l)
	}
	p.Complete()
	if again := percents(t, buf); len(again) != len(got) {
		t.Errorf("a second Complete printed again: %v", again)
	}
}

func TestProgressPipeMode(t *testing.T) {
	volumeTempdir(t)
	buf := captureLog(t)
	p := NewUploadProgress("up", 2*mib, nil)
	b := WithUploadProgress(&fakeBackend{}, p)
	vol, err := files.CreateSimpleVolume(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	vol.ObjectName = "vol0"
	wrote := make(chan error, 1)
	go func() {
		if _, err := vol.Write(bytes.Repeat([]byte{'x'}, mib)); err != nil {
			wrote <- err
			return
		}
		if got := percents(t, buf); len(got) != 0 {
			wrote <- fmt.Errorf("lines while the pipe volume was read: %v", got)
			return
		}
		vol.ZFSStreamBytes = mib // as the splitter does: before the Close that ends the read
		wrote <- vol.Close()
	}()
	upload(t, b, vol)
	if err := <-wrote; err != nil {
		t.Fatal(err)
	}
	if got := percents(t, buf); len(got) != 1 || got[0] != 50 {
		t.Fatalf("percents %v, want one line at 50 after the upload", got)
	}
	if p.wire != mib {
		t.Errorf("wire is %d, want the %d bytes read", p.wire, mib)
	}
}

func TestProgressResumeSeeds(t *testing.T) {
	volumeTempdir(t)
	buf := captureLog(t)
	resumed := []*files.VolumeInfo{{ZFSStreamBytes: mib}, {ZFSStreamBytes: mib}}
	p := NewUploadProgress("up", 4*mib, resumed)
	b := WithUploadProgress(&fakeBackend{}, p)
	upload(t, b, volumes(t, 2, mib, mib)...)
	upload(t, b, manifestVolume(t, true))
	got := percents(t, buf)
	wantIncreasing(t, got, 100)
	if got[0] < 50 {
		t.Errorf("the first line is %d%%, want 50 or more: %v", got[0], got)
	}
}

func TestProgressNoTotal(t *testing.T) {
	volumeTempdir(t)
	buf := captureLog(t)
	p := NewUploadProgress("up", 0, nil)
	b := WithUploadProgress(&fakeBackend{}, p)
	upload(t, b, volumes(t, 2, mib, mib)...)
	upload(t, b, manifestVolume(t, true))
	if got := buf.lines("NOTICE"); len(got) != 0 {
		t.Errorf("Notice lines with no total: %q", got)
	}
	if got := buf.lines("DEBUG"); len(got) != 1 || !strings.Contains(got[0], "no estimate") {
		t.Errorf("Debug lines %q, want one that says there is no estimate", got)
	}
}

func TestProgressManifestNotCounted(t *testing.T) {
	volumeTempdir(t)
	buf := captureLog(t)
	p := NewUploadProgress("up", 4*mib, nil)
	b := WithUploadProgress(&fakeBackend{}, p)
	upload(t, b, manifestVolume(t, false))
	if got := percents(t, buf); len(got) != 0 {
		t.Fatalf("a manifest upload printed %v", got)
	}
	if p.wire != 0 {
		t.Errorf("a manifest upload counted %d bytes on the wire", p.wire)
	}
	upload(t, b, manifestVolume(t, true))
	if got := percents(t, buf); len(got) != 1 || got[0] != 100 {
		t.Errorf("percents %v, want one line at 100 for the final manifest", got)
	}
}

func downloadAll(t *testing.T, b Backend, names ...string) {
	t.Helper()
	for _, name := range names {
		rc, err := b.Download(context.Background(), name)
		if err != nil {
			t.Fatal(err)
		}
		_, err = io.CopyBuffer(io.Discard, struct{ io.Reader }{rc}, make([]byte, 4096))
		if cerr := rc.Close(); err == nil && cerr != nil {
			err = cerr
		}
		if err != nil {
			t.Fatal(err)
		}
	}
}

func downloadFixture(n int, stored uint64) (*fakeBackend, []*files.VolumeInfo, []string) {
	fake := &fakeBackend{objects: map[string][]byte{"manifests|x": []byte("manifest")}, failRead: map[string]int{}}
	var vols []*files.VolumeInfo
	var names []string
	for i := range n {
		name := fmt.Sprintf("vol%d", i)
		fake.objects[name] = bytes.Repeat([]byte{'x'}, int(stored))
		vols = append(vols, &files.VolumeInfo{ObjectName: name, Size: mib})
		names = append(names, name)
	}
	return fake, vols, names
}

func TestDownloadProgressWholePercents(t *testing.T) {
	buf := captureLog(t)
	fake, vols, names := downloadFixture(4, mib)
	b := WithDownloadProgress(fake, NewDownloadProgress("Downloading ds@s from file:///x", vols))

	rc, err := b.Download(context.Background(), "manifests|x")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := rc.(*countingReader); ok {
		t.Error("the manifest's download was counted")
	}
	_ = rc.Close()

	downloadAll(t, b, names...)
	got := percents(t, buf)
	if len(got) != 100 {
		t.Fatalf("got %d lines %v, want 1..100", len(got), got)
	}
	for i, pct := range got {
		if pct != i+1 {
			t.Fatalf("line %d is %d%%, want %d%%: %v", i, pct, i+1, got)
		}
	}
	if l := buf.lines("NOTICE")[99]; !strings.HasPrefix(l, "Downloading ds@s from file:///x: 100% (4.0 MiB of 4.0 MiB)") {
		t.Errorf("the last line is %q", l)
	}
}

func TestDownloadProgressRetry(t *testing.T) {
	buf := captureLog(t)
	fake, vols, names := downloadFixture(4, mib)
	fake.failRead["vol2"] = 1
	p := NewDownloadProgress("down", vols)
	b := WithDownloadProgress(fake, p)

	downloadAll(t, b, names[0], names[1])
	rc, err := b.Download(context.Background(), "vol2")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = io.Copy(io.Discard, rc); err == nil {
		t.Fatal("the first read of vol2 did not fail")
	}
	_ = rc.Close()
	p.mu.Lock()
	cut := p.inflight["vol2"]
	p.mu.Unlock()
	if cut != mib/2 {
		t.Fatalf("after a cut read vol2 is at %d in flight, want %d", cut, mib/2)
	}

	rc, err = b.Download(context.Background(), "vol2")
	if err != nil {
		t.Fatal(err)
	}
	p.mu.Lock()
	reset := p.inflight["vol2"]
	p.mu.Unlock()
	if reset != 0 {
		t.Errorf("the retry of vol2 starts at %d, want 0", reset)
	}
	if _, err = io.Copy(io.Discard, rc); err != nil {
		t.Fatal(err)
	}
	_ = rc.Close()
	downloadAll(t, b, names[3])
	wantIncreasing(t, percents(t, buf), 100)
}

func TestDownloadProgressOversizeObject(t *testing.T) {
	buf := captureLog(t)
	fake, vols, names := downloadFixture(4, mib+1)
	p := NewDownloadProgress("down", vols)
	b := WithDownloadProgress(fake, p)
	downloadAll(t, b, names[:3]...)
	if got := percents(t, buf); got[len(got)-1] >= 100 {
		t.Fatalf("percents %v reach 100 before the last object", got)
	}
	downloadAll(t, b, names[3])
	got := percents(t, buf)
	wantIncreasing(t, got, 100)
	if p.done != 4*mib {
		t.Errorf("done is %d, want each object capped at its size: %d", p.done, 4*mib)
	}
}

func TestProgressRateAndETA(t *testing.T) {
	volumeTempdir(t)
	buf := captureLog(t)
	p := NewUploadProgress("up", 4*mib, nil)
	clockPerMiB(p, time.Second)
	b := WithUploadProgress(&fakeBackend{}, p)
	upload(t, b, volumes(t, 4, mib, mib)...)
	upload(t, b, manifestVolume(t, true))

	lines := buf.lines("NOTICE")
	if strings.Contains(lines[0], "/s") {
		t.Errorf("the first line, under 2 s, has a rate: %q", lines[0])
	}
	if want := "up: 50% (2.0 MiB of an estimated 4.0 MiB), 1.0 MiB/s, about 2s left"; lines[49] != want {
		t.Errorf("the line at 2 s is %q, want %q", lines[49], want)
	}
	if want := "up: 100% (4.0 MiB of an estimated 4.0 MiB), 1.0 MiB/s, took 4s"; lines[99] != want {
		t.Errorf("the 100%% line is %q, want %q", lines[99], want)
	}
}

// The plan had 1 s per MiB here, but then the 75% line is at 1 s, before any line has a pace.
func TestProgressRateExcludesResumed(t *testing.T) {
	volumeTempdir(t)
	buf := captureLog(t)
	resumed := []*files.VolumeInfo{{ZFSStreamBytes: 2 * mib}}
	p := NewUploadProgress("up", 4*mib, resumed)
	clockPerMiB(p, 2*time.Second)
	b := WithUploadProgress(&fakeBackend{}, p)
	upload(t, b, volumes(t, 2, mib, mib)...)
	upload(t, b, manifestVolume(t, true))

	var at75, at100 string
	for _, l := range buf.lines("NOTICE") {
		switch {
		case strings.HasPrefix(l, "up: 75% "):
			at75 = l
		case strings.HasPrefix(l, "up: 100% "):
			at100 = l
		}
	}
	// This run moved 1 MiB in 2 s: 1 MiB left is 2 s. With the seeded 2 MiB it would be 1 s.
	if !strings.HasSuffix(at75, ", 512 KiB/s, about 2s left") {
		t.Errorf("the 75%% line is %q, want 512 KiB/s and 2s left", at75)
	}
	if !strings.HasSuffix(at100, ", 512 KiB/s, took 4s") {
		t.Errorf("the 100%% line is %q, want 512 KiB/s and took 4s", at100)
	}
}

func TestProgressETAUsesRawBytes(t *testing.T) {
	volumeTempdir(t)
	buf := captureLog(t)
	p := NewUploadProgress("up", 8*mib, nil)
	clockPerMiB(p, time.Second)
	b := WithUploadProgress(&fakeBackend{}, p)
	upload(t, b, volumes(t, 4, mib, 2*mib)...) // compressed 2:1

	// At 50% 4 MiB raw is up, 2 MiB stored, 2 s: 4 MiB raw left at 2 MiB/s raw is 2 s.
	// From the wire rate (1 MiB/s) it would be 4 s.
	if want := "up: 50% (4.0 MiB of an estimated 8.0 MiB), 1.0 MiB/s, about 2s left"; buf.lines("NOTICE")[49] != want {
		t.Errorf("the 50%% line is %q, want %q", buf.lines("NOTICE")[49], want)
	}
}

func TestProgressNoETAAt99(t *testing.T) {
	volumeTempdir(t)
	buf := captureLog(t)
	p := NewUploadProgress("up", 2*mib, nil)
	clockPerMiB(p, time.Second)
	b := WithUploadProgress(&fakeBackend{}, p)
	// A pipe volume counts when its upload returns: the first line is at 3 s, past the estimate.
	vol, err := files.CreateSimpleVolume(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	vol.ObjectName = "vol0"
	wrote := make(chan error, 1)
	go func() {
		if _, err := vol.Write(bytes.Repeat([]byte{'x'}, 3*mib)); err != nil {
			wrote <- err
			return
		}
		vol.ZFSStreamBytes = 3 * mib
		wrote <- vol.Close()
	}()
	upload(t, b, vol)
	if err := <-wrote; err != nil {
		t.Fatal(err)
	}
	lines := buf.lines("NOTICE")
	if want := "up: 99% (2.0 MiB of an estimated 2.0 MiB), 1.0 MiB/s"; len(lines) != 1 || lines[0] != want {
		t.Errorf("lines %q, want one: %q", lines, want)
	}
}

// The wrapper hides the optional interfaces from a type assertion: it must stay on the data path.
func TestProgressWrapperHidesOptionalInterfaces(t *testing.T) {
	captureLog(t)
	if _, ok := Backend(&FileBackend{}).(Sizer); !ok {
		t.Fatal("FileBackend is not a Sizer: this test no longer pins anything")
	}
	if _, ok := WithUploadProgress(&FileBackend{}, NewUploadProgress("up", 1, nil)).(Sizer); ok {
		t.Error("the upload wrapper is a Sizer")
	}
	if _, ok := WithDownloadProgress(&FileBackend{}, NewDownloadProgress("down", nil)).(Sizer); ok {
		t.Error("the download wrapper is a Sizer")
	}
}
