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
	"context"
	"io"
	"os/exec"
	"testing"

	"github.com/someone1/zfsbackup-go/files"
)

// TestReceiveStreamReportsExtractError makes sure a volume that cannot be
// extracted fails the restore. The extract step decompresses and decrypts, so a
// dropped error there hands `zfs receive` a truncated stream while the command
// reports success. That silently loses the tail of the restored dataset.
func TestReceiveStreamReportsExtractError(t *testing.T) {
	ctx := context.Background()

	// The manifest claims the volumes are gzip compressed, but the volume below
	// holds plain bytes, so files.VolumeInfo.Extract fails on the gzip header.
	manifest := &files.JobInfo{Compressor: files.InternalCompressor}

	vol, err := files.CreateSimpleVolume(ctx, false)
	if err != nil {
		t.Fatalf("could not create volume - %v", err)
	}
	vol.ObjectName = "tank/data|snap1.zstream.gz.vol1"
	if _, err = io.WriteString(vol, "this is not a gzip stream"); err != nil {
		t.Fatalf("could not write volume - %v", err)
	}
	if err = vol.Close(); err != nil {
		t.Fatalf("could not close volume - %v", err)
	}
	t.Cleanup(func() { _ = vol.DeleteVolume() })

	volumes := make(chan *files.VolumeInfo, 1)
	volumes <- vol
	close(volumes)

	buffer := make(chan interface{}, 1)
	buffer <- nil

	// Stand in for `zfs receive`. It drains stdin and exits 0, so the only way
	// receiveStream can report a problem is by propagating the extract error.
	cmd := exec.CommandContext(ctx, "cat")

	if err = receiveStream(ctx, cmd, manifest, volumes, buffer); err == nil {
		t.Fatal("receiveStream reported success for an unextractable volume, want an error")
	}
}
