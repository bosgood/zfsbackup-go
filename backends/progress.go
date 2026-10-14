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
	"context"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/dustin/go-humanize"

	"github.com/someone1/zfsbackup-go/files"
	"github.com/someone1/zfsbackup-go/log"
)

// paceAfter is how long a tracker runs before its lines carry a rate and the time left: a rate
// from almost no data is noise.
const paceAfter = 2 * time.Second

// Progress counts the bytes of one transfer and logs each whole percent once, at Notice.
type Progress struct {
	mu       sync.Mutex
	label    string            // "Uploading tank/data@snap to s3://b/p" or "Downloading ... from ..."
	total    uint64            // the denominator; 0 turns the tracker off
	estimate bool              // total is the zfs estimate: cap at 99 until Complete
	done     uint64            // bytes of finished objects (and resumed volumes)
	inflight map[string]uint64 // object name -> bytes this attempt has transferred
	weight   map[string]uint64 // receive: object name -> expected stored size
	last     int               // last whole percent printed; 0 before the first line, so 0% never prints
	complete bool
	// Rate and time left. seeded is what resumed volumes contributed to done at construction;
	// wire is the stored bytes this run moved. now is time.Now, replaced by tests.
	start  time.Time
	seeded uint64
	wire   uint64
	now    func() time.Time
}

func newProgress(label string) *Progress {
	return &Progress{
		label:    label,
		inflight: make(map[string]uint64),
		weight:   make(map[string]uint64),
		start:    time.Now(),
		now:      time.Now,
	}
}

// NewUploadProgress tracks a send to one destination in raw zfs stream bytes against estimate,
// the `zfs send -n -P` size. The resumed volumes count as done from the start. An estimate of 0
// turns the tracker off.
func NewUploadProgress(label string, estimate uint64, resumed []*files.VolumeInfo) *Progress {
	p := newProgress(label)
	p.total = estimate
	p.estimate = true
	for _, vol := range resumed {
		p.done += vol.ZFSStreamBytes
	}
	p.seeded = p.done
	if p.total == 0 {
		log.AppLogger.Debugf("%s: no progress lines, zfs gave no estimate of the stream size.", label)
	}
	return p
}

// NewDownloadProgress tracks a receive of volumes in stored bytes. Only their names are counted.
func NewDownloadProgress(label string, volumes []*files.VolumeInfo) *Progress {
	p := newProgress(label)
	for _, vol := range volumes {
		p.total += vol.Size
		p.weight[vol.ObjectName] = vol.Size
	}
	if p.total == 0 {
		log.AppLogger.Debugf("%s: no progress lines, the volumes hold no bytes.", label)
	}
	return p
}

// current is the progress so far. Call with mu held.
func (p *Progress) current() uint64 {
	c := p.done
	for _, n := range p.inflight {
		c += n
	}
	return c
}

// report prints a line when the whole percent is higher than the last one printed. Call with mu
// held. The logger does not call back, so printing under mu is safe.
func (p *Progress) report() {
	if p.total == 0 {
		return
	}
	current := p.current()
	pct := int(current * 100 / p.total)
	if pct > 99 && p.estimate && !p.complete {
		pct = 99
	}
	if pct > 100 {
		pct = 100
	}
	if pct <= p.last {
		return
	}
	p.last = pct
	shown, totalText := current, humanize.IBytes(p.total)
	if shown > p.total {
		shown = p.total
	}
	if p.estimate {
		totalText = "an estimated " + totalText
	}
	log.AppLogger.Noticef("%s: %d%% (%s of %s)%s", p.label, pct, humanize.IBytes(shown), totalText, p.pace(pct, current))
}

// pace is the rate on the wire and the time left, or the time taken at 100. Call with mu held.
// The rate counts the stored bytes this run moved; the time left divides the remaining progress
// units by this run's progress-unit rate, so neither resumed volumes nor compression skew it.
func (p *Progress) pace(pct int, current uint64) string {
	elapsed := p.now().Sub(p.start)
	if elapsed < paceAfter || p.wire == 0 {
		return ""
	}
	rate := ", " + humanize.IBytes(uint64(float64(p.wire)/elapsed.Seconds())) + "/s"
	if pct == 100 {
		return rate + ", took " + roundDuration(elapsed)
	}
	if current <= p.seeded || p.total <= current {
		return rate
	}
	moved := float64(current - p.seeded)
	left := time.Duration(float64(p.total-current) / moved * float64(elapsed))
	return rate + ", about " + roundDuration(left) + " left"
}

// roundDuration rounds d to the second under a minute (at least 1s), to the minute above, and
// drops a trailing "0s" ("7m", not "7m0s").
func roundDuration(d time.Duration) string {
	if d < time.Minute {
		d = d.Round(time.Second)
		if d < time.Second {
			d = time.Second
		}
		return d.String()
	}
	s := d.Round(time.Minute).String()
	return strings.TrimSuffix(s, "0s")
}

// Complete marks the set uploaded: it prints 100 once, with the average rate and the time taken.
func (p *Progress) Complete() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.complete = true
	p.done = p.total
	p.inflight = make(map[string]uint64)
	p.report()
}

// uploadProgress counts the bytes a backend reads from each volume it uploads. It does not
// implement Sizer, Archiver or LegacyVolumeFinder, so a type assertion on it fails: hand it to
// the data path only (retryUploadChainer), never to code that asks for those.
type uploadProgress struct {
	Backend
	p *Progress
}

// WithUploadProgress wraps b so that its uploads report to p.
func WithUploadProgress(b Backend, p *Progress) Backend {
	return &uploadProgress{Backend: b, p: p}
}

// Upload uploads vol through the wrapped backend. A volume in flight counts read / Size of its
// ZFSStreamBytes; a pipe volume (Size 0 until it ends) counts nothing until its upload returns.
// The final manifest's upload completes the set; manifests are never counted.
func (u *uploadProgress) Upload(ctx context.Context, vol *files.VolumeInfo) error {
	p := u.p
	if vol.IsManifest {
		err := u.Backend.Upload(ctx, vol)
		if err == nil && vol.IsFinalManifest {
			p.Complete()
		}
		return err
	}

	name := vol.ObjectName
	size, raw := vol.Size, vol.ZFSStreamBytes
	var read uint64
	p.mu.Lock()
	p.inflight[name] = 0 // a retry starts over
	p.mu.Unlock()
	vol.SetReadHook(func(n int) {
		p.mu.Lock()
		defer p.mu.Unlock()
		p.wire += uint64(n)
		if size == 0 {
			return
		}
		read += uint64(n)
		// read * raw overflows only for a volume over 16 EiB.
		p.inflight[name] = min(read, size) * raw / size
		p.report()
	})
	err := u.Backend.Upload(ctx, vol)
	vol.SetReadHook(nil)

	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.inflight, name)
	if err != nil {
		return err
	}
	// Read only now: in pipe mode the splitter sets ZFSStreamBytes before the Close that ends
	// the backend's read.
	p.done += vol.ZFSStreamBytes
	p.report()
	return nil
}

// downloadProgress counts the bytes read from each volume a backend downloads. Like
// uploadProgress it hides Sizer, Archiver and LegacyVolumeFinder: hand it to the download
// workers (downloadSequenceVolume) only.
type downloadProgress struct {
	Backend
	p *Progress
}

// WithDownloadProgress wraps b so that its downloads of p's volumes report to p.
func WithDownloadProgress(b Backend, p *Progress) Backend {
	return &downloadProgress{Backend: b, p: p}
}

// Download returns the wrapped backend's reader, counting it when filename is one of p's volumes.
func (d *downloadProgress) Download(ctx context.Context, filename string) (io.ReadCloser, error) {
	p := d.p
	p.mu.Lock()
	_, counted := p.weight[filename]
	p.mu.Unlock()
	if !counted {
		return d.Backend.Download(ctx, filename)
	}
	rc, err := d.Backend.Download(ctx, filename)
	if err != nil {
		return nil, err
	}
	p.mu.Lock()
	p.inflight[filename] = 0 // a retry starts over
	p.mu.Unlock()
	return &countingReader{rc: rc, p: p, name: filename}, nil
}

// countingReader counts what is read from one downloaded volume, capped at its expected size.
type countingReader struct {
	rc   io.ReadCloser
	p    *Progress
	name string
}

func (c *countingReader) Read(b []byte) (int, error) {
	n, err := c.rc.Read(b)
	if n > 0 {
		p := c.p
		p.mu.Lock()
		p.wire += uint64(n)
		p.inflight[c.name] = min(p.inflight[c.name]+uint64(n), p.weight[c.name])
		p.report()
		p.mu.Unlock()
	}
	return n, err
}

// Close closes the download. A volume read whole moves to done; a short one stays in flight at
// its last count until a retry resets it.
func (c *countingReader) Close() error {
	err := c.rc.Close()
	p := c.p
	p.mu.Lock()
	defer p.mu.Unlock()
	if n, ok := p.inflight[c.name]; ok && n == p.weight[c.name] {
		p.done += n
		delete(p.inflight, c.name)
	}
	return err
}
