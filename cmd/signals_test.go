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

package cmd

import (
	"bytes"
	"context"
	"os"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/op/go-logging"

	"github.com/bosgood/zfsbackup-go/log"
)

// These tests send real signals to the test process. It survives only because SignalContext
// registers the handler before the test sends. Do not run them in parallel.

// syncBuffer is a log backend's writer that the handler goroutine and the test share.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// signalTest captures the log and replaces exitFunc with one that records the code.
func signalTest(t *testing.T) (logs *syncBuffer, exits chan int) {
	t.Helper()
	logs = new(syncBuffer)
	log.AppLogger.SetBackend(logging.AddModuleLevel(logging.NewLogBackend(logs, "", 0)))
	exits = make(chan int, 2)
	exitFunc = func(code int) { exits <- code }
	t.Cleanup(func() {
		exitFunc = os.Exit
		log.AppLogger.SetBackend(logging.AddModuleLevel(logging.NewLogBackend(os.Stderr, "", 0)))
	})
	return logs, exits
}

func kill(t *testing.T, sig syscall.Signal) {
	t.Helper()
	if err := syscall.Kill(os.Getpid(), sig); err != nil {
		t.Fatal(err)
	}
}

func TestSignalContextFirstSignalCancels(t *testing.T) {
	logs, exits := signalTest(t)
	ctx, stop := SignalContext(context.Background())
	defer stop()

	kill(t, syscall.SIGTERM)
	select {
	case <-ctx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("the context was not canceled within 2 s of SIGTERM")
	}
	if got := interrupted(); got != syscall.SIGTERM {
		t.Errorf("interrupted() = %v, want SIGTERM", got)
	}
	select {
	case code := <-exits:
		t.Errorf("the first signal exited with %d", code)
	case <-time.After(100 * time.Millisecond):
	}
	if want := "Received SIGTERM; stopping."; !strings.Contains(logs.String(), want) {
		t.Errorf("log has no %q:\n%s", want, logs)
	}
	stop()
	if got := interrupted(); got != 0 {
		t.Errorf("after stop, interrupted() = %v, want none", got)
	}
}

func TestSignalContextSecondSignalExits(t *testing.T) {
	logs, exits := signalTest(t)
	ctx, stop := SignalContext(context.Background())
	defer stop()

	kill(t, syscall.SIGTERM)
	<-ctx.Done()
	kill(t, syscall.SIGTERM)
	select {
	case code := <-exits:
		if code != 143 {
			t.Errorf("the second SIGTERM exited with %d, want 143", code)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the second SIGTERM did not exit within 2 s")
	}
	if want := "Received SIGTERM again; exiting now."; !strings.Contains(logs.String(), want) {
		t.Errorf("log has no %q:\n%s", want, logs)
	}
}

func TestSignalHintNamesTheCommand(t *testing.T) {
	for _, tc := range []struct{ cmd, want string }{
		{"send", "A send continues with --resume."},
		{"receive", "Run the receive again from the start."},
		{"clean", ""},
	} {
		if got := signalHint(tc.cmd); (tc.want == "") != (got == "") || !strings.Contains(got, tc.want) {
			t.Errorf("signalHint(%q) = %q, want %q", tc.cmd, got, tc.want)
		}
	}
}
