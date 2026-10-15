//go:build integration

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

package main

import (
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/bosgood/zfsbackup-go/cmd"
)

// zvolDevice returns a block device node for the zvol made by create: the
// one /sys/block/zd* entry that create added. The container runs no udev, so
// there is no /dev/zvol; the node is made with mknod from the entry's
// major:minor when it is missing.
func zvolDevice(t *testing.T, create func()) string {
	t.Helper()
	before, _ := filepath.Glob("/sys/block/zd*")
	create()
	after, _ := filepath.Glob("/sys/block/zd*")
	var added []string
	for _, p := range after {
		if !slices.Contains(before, p) {
			added = append(added, p)
		}
	}
	if len(added) != 1 {
		t.Fatalf("zfs create -V added %d entries under /sys/block (%v), want 1; cannot find the zvol's device", len(added), added)
	}
	dev := "/dev/" + filepath.Base(added[0])
	if _, err := os.Stat(dev); err == nil {
		return dev
	}
	numbers, err := os.ReadFile(filepath.Join(added[0], "dev"))
	if err != nil {
		t.Fatal(err)
	}
	major, minor, ok := strings.Cut(strings.TrimSpace(string(numbers)), ":")
	if !ok {
		t.Fatalf("%s/dev holds %q, want major:minor", added[0], numbers)
	}
	majN, _ := strconv.Atoi(major)
	minN, _ := strconv.Atoi(minor)
	if err := syscall.Mknod(dev, syscall.S_IFBLK|0o600, int(unixMkdev(uint32(majN), uint32(minN)))); err != nil {
		t.Fatalf("mknod %s b %d %d: %v", dev, majN, minN, err)
	}
	t.Cleanup(func() { os.Remove(dev) })
	return dev
}

// unixMkdev is unix.Mkdev for Linux: the 32-bit device number of major:minor.
func unixMkdev(major, minor uint32) uint64 {
	dev := uint64(major&0x00000fff) << 8
	dev |= uint64(major&0xfffff000) << 32
	dev |= uint64(minor&0x000000ff) << 0
	dev |= uint64(minor&0xffffff00) << 12
	return dev
}

// writeDevice writes size random bytes at offset into dev and syncs them.
func writeDevice(t *testing.T, dev string, offset, size int64) {
	t.Helper()
	f, err := os.OpenFile(dev, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.Seek(offset, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	if _, err = io.CopyN(f, rand.Reader, size); err != nil {
		t.Fatal(err)
	}
	if err = f.Sync(); err != nil {
		t.Fatal(err)
	}
	if err = f.Close(); err != nil {
		t.Fatal(err)
	}
}

// deviceSum returns the SHA-256 of the first size bytes of dev.
func deviceSum(t *testing.T, dev string, size int64) string {
	t.Helper()
	f, err := os.Open(dev)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err = io.CopyN(h, f, size); err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf("%x", h.Sum(nil))
}

// TestIntegrationS3Zvol backs up a zvol (a volume, mountpoint "-") with data
// in it: a full, an incremental, the explicit and the --auto restore. The
// restored volume has the same bytes and volsize. The -F guards of the
// 2026-10-12 review work on a volume too.
func TestIntegrationS3Zvol(t *testing.T) {
	pool := testPool(t)
	_, bucket, prefix := itBucket(t, "zfsbackup-it-zvol")
	target := "s3://" + bucket + "/" + prefix
	vol := pool + "/zvol"
	restored := pool + "/zvol-restored"
	restored2 := pool + "/zvol-restored2"
	destroyAfter(t, vol, restored, restored2)
	work := t.TempDir()
	const volsize = 16 << 20

	dev := zvolDevice(t, func() { zfsRun(t, "create", "-V", strconv.Itoa(volsize), vol) })
	writeDevice(t, dev, 0, 3<<20)
	s1 := snapshot(t, vol, "daily")
	writeDevice(t, dev, 8<<20, 2<<20)
	s2 := snapshot(t, vol, "daily")
	sum := deviceSum(t, dev, volsize)

	mustSend(t, work, append(slices.Clone(plainSend), vol+"@"+s1, target)...)
	mustSend(t, work, append(slices.Clone(plainSend), "-i", vol+"@"+s1, vol+"@"+s2, target)...)

	// Explicit full, then --auto for the incremental; an empty working directory each time.
	restoredDev := zvolDevice(t, func() { itReceive(t, t.TempDir(), "-F", vol+"@"+s1, target, restored) })
	wantSnapshots(t, restored, s1)
	wantGUIDs(t, vol, restored, s1)
	if got := zfsGet(t, restored, "type"); got != "volume" {
		t.Fatalf("%s is a %s, want a volume", restored, got)
	}
	if got := zfsGet(t, restored, "volsize"); got != strconv.Itoa(volsize) {
		t.Fatalf("%s has volsize %s, want %d", restored, got, volsize)
	}
	itReceive(t, t.TempDir(), "--auto", "-F", vol, target, restored)
	wantSnapshots(t, restored, s1, s2)
	wantGUIDs(t, vol, restored, s1, s2)
	if got := deviceSum(t, restoredDev, volsize); got != sum {
		t.Fatalf("%s holds other bytes than %s after the restore", restored, vol)
	}

	// The whole chain at once into a new volume.
	zvolDevice(t, func() { itReceive(t, t.TempDir(), "--auto", "-F", vol, target, restored2) })
	wantSnapshots(t, restored2, s1, s2)
	wantGUIDs(t, vol, restored2, s1, s2)

	// -F over a volume without snapshots destroys it: refused without --allowDestroy.
	zfsRun(t, "destroy", "-r", restored2)
	zvolDevice(t, func() { zfsRun(t, "create", "-V", strconv.Itoa(volsize), restored2) })
	logs, err := itRun(cmd.ResetReceiveJobInfo, "receive", t.TempDir(), "-F", vol+"@"+s1, target, restored2)
	if err == nil {
		t.Fatalf("receive -F over a volume with no snapshots succeeded without --allowDestroy\n%s", logs)
	}
	wantInLog(t, logs, "--allowDestroy")
	itReceive(t, t.TempDir(), "-F", "--allowDestroy", vol+"@"+s1, target, restored2)
	wantSnapshots(t, restored2, s1)
}
