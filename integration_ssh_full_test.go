//go:build integration

package main

import "testing"

// TestIntegrationSSHFull is the refquota part of TestIntegrationFileFull over
// sftp: the remote fills up in the middle of the send. A volume cut by the
// full disk must not stay at the remote. The remote is chrooted, so its
// paths are not the test's local paths.
func TestIntegrationSSHFull(t *testing.T) {
	pool := testPool(t)
	itSSHClearEnv(t)
	testFullResume(t, pool, "sfull", newSSHJailDest)
}
