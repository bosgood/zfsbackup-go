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
	"context"
	"os"
	"os/signal"
	"sync"
	"syscall"

	"github.com/someone1/zfsbackup-go/log"
)

var (
	// exitFunc ends the process on a second signal. Tests replace it.
	exitFunc = os.Exit

	signalMu sync.Mutex
	// interruptedBy is the first SIGINT or SIGTERM received, 0 if none. exitCode reads it.
	interruptedBy syscall.Signal
	// runningCmd is the name of the command processFlags started, for the signal notice.
	runningCmd string
)

// SignalContext returns a context that is canceled by the first SIGINT or SIGTERM.
// A second signal exits the process at once. stop releases the handler.
func SignalContext(parent context.Context) (ctx context.Context, stop func()) {
	ctx, cancel := context.WithCancel(parent)
	// Room for both signals: the second must not be dropped while the first is handled.
	ch := make(chan os.Signal, 2)
	signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM)
	done := make(chan struct{})

	go func() {
		first := true
		for {
			select {
			case <-done:
				return
			case s := <-ch:
				sig, _ := s.(syscall.Signal)
				if !first {
					log.AppLogger.Noticef("Received %s again; exiting now.", signalName(sig))
					exitFunc(128 + int(sig))
					continue
				}
				first = false
				signalMu.Lock()
				interruptedBy = sig
				name := runningCmd
				signalMu.Unlock()
				log.AppLogger.Noticef("Received %s; stopping.%s", signalName(sig), signalHint(name))
				cancel()
			}
		}
	}()

	var once sync.Once
	return ctx, func() {
		once.Do(func() {
			signal.Stop(ch)
			close(done)
			cancel()
			resetInterrupted()
		})
	}
}

// signalHint tells the user how to continue after an interrupted command.
func signalHint(cmdName string) string {
	switch cmdName {
	case sendCmd.Name():
		return " Nothing more is uploaded. A send continues with --resume."
	case receiveCmd.Name():
		return " Run the receive again from the start."
	default:
		return ""
	}
}

func signalName(sig syscall.Signal) string {
	switch sig {
	case syscall.SIGINT:
		return "SIGINT"
	case syscall.SIGTERM:
		return "SIGTERM"
	default:
		return sig.String()
	}
}

// interrupted returns the first signal received, 0 if none.
func interrupted() syscall.Signal {
	signalMu.Lock()
	defer signalMu.Unlock()
	return interruptedBy
}

func setRunningCmd(name string) {
	signalMu.Lock()
	defer signalMu.Unlock()
	runningCmd = name
}

// resetInterrupted forgets a received signal, so the next in-process run does not exit 128+signal.
func resetInterrupted() {
	signalMu.Lock()
	defer signalMu.Unlock()
	interruptedBy = 0
	runningCmd = ""
}
