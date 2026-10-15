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
	"slices"
	"testing"
	"time"

	"github.com/bosgood/zfsbackup-go/cmd"
)

// TestIntegrationS3KeyExpiry proves the reason for --allowKeyExpiry on real
// objects: a key that expires in 45 s is refused without the flag, sends with
// it, restores before the expiry, and is refused by go-crypto after it (the
// volumes' signatures no longer verify). Takes about a minute.
func TestIntegrationS3KeyExpiry(t *testing.T) {
	pool := testPool(t)
	client, bucket, prefix := itBucket(t, "zfsbackup-it-expiry")
	target := "s3://" + bucket + "/" + prefix
	src := pool + "/expiry"
	restored := pool + "/expiry-restored"
	restored2 := pool + "/expiry-restored2"
	destroyAfter(t, restored, restored2)
	work := t.TempDir()
	newSource(t, src, 1<<20)
	s1 := snapshot(t, src, "daily")

	const email = "expiring@example.com"
	const lifetime = 45 * time.Second
	made := time.Now()
	keys := append(expiringRingsFor(t, email, lifetime), "--encryptTo", email, "--signFrom", email)
	allow := append([]string{"--allowKeyExpiry"}, keys...)

	logs, err := itSend(t, work, append(append(slices.Clone(keys), plainSend...), src+"@"+s1, target)...)
	if err == nil {
		t.Fatalf("send with an expiring key succeeded without --allowKeyExpiry\n%s", logs)
	}
	wantInLog(t, logs, "Refusing PGP keys with an expiry date", "--allowKeyExpiry")
	if volumes, manifests, list := countObjects(t, client, bucket, prefix); volumes+manifests != 0 {
		t.Fatalf("the refused send uploaded %d volumes and %d manifests:\n%s", volumes, manifests, list)
	}

	mustSend(t, work, append(append(slices.Clone(allow), plainSend...), src+"@"+s1, target)...)
	wantPGPObjects(t, client, bucket, prefix, wantPGPVolume)

	// Before the expiry the restore works; the manifest comes from S3.
	itReceive(t, t.TempDir(), append(slices.Clone(allow), "-F", src+"@"+s1, target, restored)...)
	wantGUIDs(t, src, restored, s1)
	compareDirs(t, "/"+src, "/"+restored)
	if left := time.Until(made.Add(lifetime)); left < 0 {
		t.Fatalf("the key expired %v before the first restore ended; the lifetime is too short for this host", -left)
	}

	// After it the same restore fails: the key that signed the volumes has expired.
	time.Sleep(time.Until(made.Add(lifetime + 2*time.Second)))
	logs, err = itRun(cmd.ResetReceiveJobInfo, "receive", t.TempDir(), append(slices.Clone(allow), "-F", src+"@"+s1, target, restored2)...)
	if err == nil {
		t.Fatalf("receive after the key expired succeeded\n%s", logs)
	}
	wantInLog(t, logs, "key expired")
	wantNoDataset(t, restored2)
}
