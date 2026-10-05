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
	"crypto/md5" // nolint:gosec // MD5 not used for cryptographic purposes here
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/someone1/zfsbackup-go/backends"
	"github.com/someone1/zfsbackup-go/config"
	"github.com/someone1/zfsbackup-go/files"
	"github.com/someone1/zfsbackup-go/log"
	"github.com/someone1/zfsbackup-go/zfs"
)

// redactURI removes an embedded password from a destination URI so it is safe to
// log. Destinations may carry credentials (e.g. ssh://user:password@host/path) and
// some output is emitted at Notice level, which is visible by default. The URI is
// returned unchanged unless it actually contains a password.
func redactURI(uri string) string {
	u, err := url.Parse(uri)
	if err != nil || u.User == nil {
		return uri
	}
	if _, hasPassword := u.User.Password(); !hasPassword {
		return uri
	}
	return u.Redacted()
}

// joinURI appends an object name to a destination URI for display purposes, and
// redacts any credentials. filepath.Join must not be used here: it runs
// filepath.Clean, which collapses the "//" in a scheme, turning "s3://bucket"
// into "s3:/bucket".
func joinURI(target, obj string) string {
	return strings.TrimSuffix(redactURI(target), "/") + "/" + obj
}

func prepareBackend(ctx context.Context, j *files.JobInfo, backendURI string, uploadBuffer chan bool) (backends.Backend, error) {
	log.AppLogger.Debugf("Initializing Backend %s", backends.RedactURI(backendURI))
	conf := &backends.BackendConfig{
		MaxParallelUploadBuffer: uploadBuffer,
		TargetURI:               backendURI,
		TypedURI:                j.DestinationsAsTyped[backendURI],
		MaxParallelUploads:      j.MaxParallelUploads,
		MaxBackoffTime:          j.MaxBackoffTime,
		MaxRetryTime:            j.MaxRetryTime,
		UploadChunkSize:         j.UploadChunkSize * 1024 * 1024,
		ManifestPrefix:          j.ManifestPrefix,
	}

	backend, err := backends.GetBackendForURI(backendURI)
	if err != nil {
		return nil, err
	}

	err = backend.Init(ctx, conf)

	return backend, err
}

// cacheDirFor is where the manifests of the destination backendURI are cached.
func cacheDirFor(backendURI string) string {
	// nolint:gosec // MD5 not used for cryptographic purposes here
	return filepath.Join(config.WorkingDir, "cache", fmt.Sprintf("%x", md5.Sum([]byte(backendURI))))
}

// getCacheDir creates the cache directory of backendURI (a canonical URI) and first moves into
// it whatever an older version cached under another spelling of the same destination.
func getCacheDir(j *files.JobInfo, backendURI string) (string, error) {
	dest := cacheDirFor(backendURI)
	oerr := os.MkdirAll(dest, os.ModePerm)
	if oerr != nil {
		return "", fmt.Errorf("could not create cache directory %s due to an error: %v", dest, oerr)
	}

	for _, spelling := range backends.LegacySpellings(backendURI, j.DestinationsAsTyped[backendURI]) {
		if err := adoptCacheDir(cacheDirFor(spelling), dest); err != nil {
			return "", fmt.Errorf("could not move the cache of %s to %s due to an error: %v", backends.RedactURI(spelling), dest, err)
		}
	}

	return dest, nil
}

// adoptCacheDir moves the cached manifests in from to to and removes from. A manifest cached in
// both keeps to's copy: it was synced from the destination or written since the upgrade.
func adoptCacheDir(from, to string) error {
	entries, err := os.ReadDir(from)
	if os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return err
	}
	moved := 0
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		src, dst := filepath.Join(from, entry.Name()), filepath.Join(to, entry.Name())
		if _, serr := os.Stat(dst); serr == nil {
			if err = os.Remove(src); err != nil {
				return err
			}
			continue
		}
		if err = os.Rename(src, dst); err != nil {
			return err
		}
		moved++
	}
	log.AppLogger.Noticef("Moved %d cached manifests from %s to %s: the cache is now keyed by the canonical URI.", moved, from, to)
	if err = os.Remove(from); err != nil {
		log.AppLogger.Debugf("Could not remove the old cache directory %s: %v", from, err)
	}
	return nil
}

// Returns local manifest paths that exist in the backend and those that do not
// nolint:gocritic // Don't need to name the results
func syncCache(ctx context.Context, j *files.JobInfo, localCache string, backend backends.Backend) ([]string, []string, error) {
	// List all manifests at the destination
	manifests, merr := backend.List(ctx, j.ManifestPrefix)
	if merr != nil {
		return nil, nil, fmt.Errorf("could not list manifest files from the backed due to error - %v", merr)
	}

	// Make it safe for local file system storage
	safeManifests := make([]string, len(manifests))
	for idx := range manifests {
		// nolint:gosec // MD5 not used for cryptographic purposes here
		safeManifests[idx] = fmt.Sprintf("%x", md5.Sum([]byte(manifests[idx])))
	}

	// Check what manifests we have locally, and if we are missing any, download them
	manifestFiles, ferr := os.ReadDir(localCache)
	if ferr != nil {
		return nil, nil, fmt.Errorf("could not list files from the local cache dir due to error - %v", ferr)
	}

	var localOnlyFiles []string
	var foundFiles []string
	for _, file := range manifestFiles {
		if file.IsDir() {
			continue
		}
		found := false
		for idx := range manifests {
			if strings.Compare(file.Name(), safeManifests[idx]) != 0 {
				continue
			}

			found = true
			foundFiles = append(foundFiles, safeManifests[idx])
			manifests = append(manifests[:idx], manifests[idx+1:]...)
			safeManifests = append(safeManifests[:idx], safeManifests[idx+1:]...)
			break
		}
		if !found {
			localOnlyFiles = append(localOnlyFiles, file.Name())
		}
	}

	pderr := backend.PreDownload(ctx, manifests)
	if pderr != nil {
		return nil, nil, fmt.Errorf("could not prepare manifests for download due to error - %v", pderr)
	}

	if len(manifests) > 0 {
		log.AppLogger.Debugf("Syncing %d manifests to local cache.", len(manifests))

		// manifests should only contain what we don't have locally
		for idx, manifest := range manifests {
			if err := downloadTo(ctx, backend, manifest, filepath.Join(localCache, safeManifests[idx])); err != nil {
				return nil, nil, err
			}
		}
	}

	safeManifests = append(safeManifests, foundFiles...)

	return safeManifests, localOnlyFiles, nil
}

// nolint:unparam // Some errors are not ok to ignore
func validateSnapShotExists(ctx context.Context, snapshot *files.SnapshotInfo, target string, includeBookmarks bool) (bool, error) {
	snapshots, err := zfs.GetSnapshotsAndBookmarks(ctx, target)
	if err != nil {
		log.AppLogger.Debugf("Could not list snapshots for %s: %v", backends.RedactURI(target), err)
		// TODO: There are some error cases that are ok to ignore!
		return false, nil
	}
	return validateSnapShotExistsFromSnaps(snapshot, snapshots, includeBookmarks), nil
}

func validateSnapShotExistsFromSnaps(snapshot *files.SnapshotInfo, snapshots []files.SnapshotInfo, includeBookmarks bool) bool {
	for _, snap := range snapshots {
		if !includeBookmarks && snap.Bookmark {
			continue
		}
		if snap.Equal(snapshot) {
			// Flag the snapshot as a bookmark if it is one
			snapshot.Bookmark = snap.Bookmark
			return true
		}
	}

	return false
}
