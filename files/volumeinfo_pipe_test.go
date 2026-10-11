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

package files

import (
	"bytes"
	"context"
	"crypto/rand"
	"io"
	"testing"
	"time"

	gzip "github.com/klauspost/pgzip"
)

// receive --maxFileBuffer 0: the download writes a pipe volume and closes it
// while the restore still reads it through Extract's decompressor. The
// writer's Close must leave the decompressor to the reader. It closed it before
// it flushed its write buffer, and pgzip's read-ahead waited for the unflushed
// end of the volume: a deadlock (found by TestIntegrationS3/Pipe).
func TestPipeVolumeWriterCloseLeavesReaderDecompressor(t *testing.T) {
	data := make([]byte, 3<<20)
	if _, err := rand.Read(data); err != nil {
		t.Fatal(err)
	}
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	if _, err := zw.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	vol, err := CreateSimpleVolume(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	j := &JobInfo{Compressor: InternalCompressor}

	var out bytes.Buffer
	readErr := make(chan error, 1)
	go func() {
		if err := vol.Extract(ctx, j, false); err != nil {
			readErr <- err
			return
		}
		if _, err := io.Copy(&out, vol); err != nil {
			readErr <- err
			return
		}
		readErr <- vol.Close()
	}()

	writeErr := make(chan error, 1)
	go func() {
		// As processSequence copies: 32 KiB writes, so the end of the volume
		// waits in the volume's write buffer until Close flushes it.
		if _, err := io.Copy(vol, io.LimitReader(&gz, int64(gz.Len())+1)); err != nil {
			writeErr <- err
			return
		}
		writeErr <- vol.Close()
	}()

	timeout := time.After(10 * time.Second)
	for _, c := range []chan error{writeErr, readErr} {
		select {
		case err := <-c:
			if err != nil {
				t.Fatal(err)
			}
		case <-timeout:
			t.Fatal("the pipe volume deadlocked: the writer's Close waits on the reader's decompressor")
		}
	}
	if !bytes.Equal(out.Bytes(), data) {
		t.Fatalf("read %d bytes back, want the %d written", out.Len(), len(data))
	}
}
