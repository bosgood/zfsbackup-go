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
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/someone1/zfsbackup-go/backends"
	"github.com/someone1/zfsbackup-go/config"
	"github.com/someone1/zfsbackup-go/files"
	"github.com/someone1/zfsbackup-go/log"
	"github.com/someone1/zfsbackup-go/zfs"
)

// joinURI appends an object name to a destination URI for display purposes, and
// redacts any credentials (backends.RedactURI). filepath.Join must not be used
// here: it runs filepath.Clean, which collapses the "//" in a scheme, turning
// "s3://bucket" into "s3:/bucket".
func joinURI(target, obj string) string {
	return strings.TrimSuffix(backends.RedactURI(target), "/") + "/" + obj
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

// syncCache downloads the manifests at backend that localCache lacks. It returns the object names
// of the manifests at backend (read them with readCachedManifest) and the cache file names of
// manifests cached but not at backend.
// nolint:gocritic // Don't need to name the results
func syncCache(ctx context.Context, j *files.JobInfo, localCache string, backend backends.Backend) ([]string, []string, error) {
	// List all manifests at the destination
	listed, merr := backend.List(ctx, j.ManifestPrefix)
	if merr != nil {
		return nil, nil, fmt.Errorf("could not list manifest files from the backed due to error - %v", merr)
	}
	manifests := make([]string, 0, len(listed))
	for _, name := range listed {
		if !isManifestObject(name, j.ManifestPrefix, j.Separator) {
			log.AppLogger.Debugf("Ignoring %s: it is not a manifest, it only starts with %q.", name, j.ManifestPrefix)
			continue
		}
		manifests = append(manifests, name)
	}

	atDestination := append([]string(nil), manifests...)

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
	for _, file := range manifestFiles {
		if files.IsAtomicTemp(file.Name()) && !file.IsDir() {
			removeStaleTemp(filepath.Join(localCache, file.Name()), file)
			continue
		}
		if file.IsDir() {
			continue
		}
		found := false
		for idx := range manifests {
			if strings.Compare(file.Name(), safeManifests[idx]) != 0 {
				continue
			}

			found = true
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
			err := downloadTo(ctx, backend, manifest, filepath.Join(localCache, safeManifests[idx]))
			if errors.Is(err, errObjectTooLarge) {
				// Not a manifest. Leave it uncached: only a reader that needs it (it names that
				// reader's dataset, or the reader needs every manifest) fails on it.
				continue
			} else if err != nil {
				return nil, nil, err
			}
		}
	}

	return atDestination, localOnlyFiles, nil
}

// staleTempAge is how old a temporary file of WriteFileAtomic in the cache must be before
// syncCache removes it. A write in progress (a send on this host caching its manifest, a download)
// keeps its file's modification time fresh; one left by a kill does not.
const staleTempAge = 24 * time.Hour

// removeStaleTemp removes the temporary file path, entry in the cache, if it is older than
// staleTempAge.
func removeStaleTemp(path string, entry os.DirEntry) {
	info, err := entry.Info()
	if err != nil || time.Since(info.ModTime()) < staleTempAge {
		return
	}
	if err = os.Remove(path); err != nil && !os.IsNotExist(err) {
		log.AppLogger.Warningf("Could not remove %s, left in the cache by an interrupted write: %v", path, err)
		return
	}
	log.AppLogger.Infof("Removed %s, left in the cache by an interrupted write.", path)
}

// manifestMayBeFor returns false when objectName, a manifest under manifestPrefix, is certainly
// not a manifest of volume: its name is not "<prefix><sep><volume><sep>..." for any separator.
// The name of every manifest must be the one its contents give it (manifestNamed), so readers that
// need only volume's manifests skip the others without decoding them, and an object someone else
// put at the destination under another dataset's name cannot stop them.
func manifestMayBeFor(objectName, manifestPrefix, volume string) bool {
	rest, ok := strings.CutPrefix(objectName, manifestPrefix)
	if !ok {
		return true
	}
	for i := 1; i+len(volume) <= len(rest); i++ {
		sep := rest[:i]
		if strings.HasPrefix(rest[i:], volume) && strings.HasPrefix(rest[i+len(volume):], sep) {
			return true
		}
	}
	return false
}

// isManifestObject returns whether name, listed by manifestPrefix, is a manifest rather than an
// object under a sibling prefix such as "manifests-old/". A manifest's prefix is followed by its
// separator, which is separator or (written with another --separator) holds a character no ZFS
// dataset name can, while a sibling's name goes on with one a dataset name can.
func isManifestObject(name, manifestPrefix, separator string) bool {
	rest, ok := strings.CutPrefix(name, manifestPrefix)
	if !ok || rest == "" {
		return false
	}
	if separator != "" && strings.HasPrefix(rest, separator) {
		return true
	}
	c := rest[0]
	inDatasetName := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.IndexByte("_-:./", c) >= 0
	return !inDatasetName
}

// cachedManifestName is the file name objectName is cached under.
func cachedManifestName(objectName string) string {
	// nolint:gosec // MD5 not used for cryptographic purposes here
	return fmt.Sprintf("%x", md5.Sum([]byte(objectName)))
}

// readCachedManifest decodes the manifest objectName at backend from its copy in localCache,
// downloading it first if there is none. A cached copy that does not decode (cut short by a kill or
// a failed download, before cache writes were atomic) is downloaded again, once. The manifest
// must be the one objectName names: a manifest copied over another set's name is an error.
func readCachedManifest(
	ctx context.Context, j *files.JobInfo, localCache string, backend backends.Backend, objectName string,
) (*files.JobInfo, error) {
	path := filepath.Join(localCache, cachedManifestName(objectName))
	manifest, err := readManifest(ctx, path, j)
	if err == nil {
		if err = manifestNamed(j, manifest, objectName); err == nil {
			return manifest, nil
		}
	}
	if errors.Is(err, files.ErrManifestTooLong) {
		// The copy is whole; downloading the object again would only cost the same egress.
		return nil, unreadableManifestError(objectName, path, err)
	}
	if !os.IsNotExist(err) {
		log.AppLogger.Warningf("Cached manifest %s (%s) is unreadable (%v); downloading it again.", path, objectName, err)
		if rerr := os.Remove(path); rerr != nil {
			return nil, rerr
		}
	}
	if err = backend.PreDownload(ctx, []string{objectName}); err != nil {
		log.AppLogger.Errorf("Error trying to pre download manifest %s - %v", objectName, err)
		return nil, err
	}
	if err = downloadTo(ctx, backend, objectName, path); err != nil {
		if errors.Is(err, errObjectTooLarge) {
			return nil, unreadableManifestError(objectName, path, err)
		}
		return nil, err
	}
	if manifest, err = readManifest(ctx, path, j); err == nil {
		err = manifestNamed(j, manifest, objectName)
	}
	if err != nil && !errors.Is(err, files.ErrManifestTooLong) {
		// Keep no copy: the next run reads the destination's object again, which may be fixed by then.
		if rerr := os.Remove(path); rerr != nil {
			log.AppLogger.Warningf("Could not remove %s from the cache: %v", path, rerr)
		}
	}
	if err != nil {
		return nil, unreadableManifestError(objectName, path, err)
	}
	return manifest, nil
}

// unreadableManifestError is the error for the object objectName under the manifest prefix that
// is not a manifest this run can accept. It names the object, and says what to do: a manifest
// over the limit may be a real backup of more volumes than this version reads, so it must not
// be deleted; anything else is not a manifest, and deleting it is the way out. A manifest over
// the limit stays at cachePath, and readCachedManifest reads that copy, not the object, until
// it is deleted: the error says so, or replacing the object would change nothing.
func unreadableManifestError(objectName, cachePath string, err error) error {
	if errors.Is(err, files.ErrManifestTooLong) {
		err = fmt.Errorf(
			"could not read the manifest %s at the destination: %w. If you wrote it, it is the manifest of a backup "+
				"with more volumes than this version reads: do not delete it, its volumes or its copy in the cache, and "+
				"send later backups with a larger --volsize. If you did not write it, it is not a manifest: remove it "+
				"from the destination and delete its copy in the cache, %s. Until that copy is deleted, every run "+
				"reads it, not the object at the destination",
			objectName, err, cachePath,
		)
	} else {
		err = fmt.Errorf(
			"could not read the manifest %s at the destination: %w. If you did not write it (it is not a backup "+
				"of yours, or someone else put it there), delete that object from the destination, then run again",
			objectName, err,
		)
	}
	log.AppLogger.Errorf("%v", err)
	return err
}

// manifestNamed returns an error unless manifest, read from the object objectName, is the
// manifest of the set that name promises.
func manifestNamed(j *files.JobInfo, manifest *files.JobInfo, objectName string) error {
	if name := manifest.StoredManifestObjectName(j.ManifestPrefix); name != objectName {
		return fmt.Errorf("the manifest stored as %s is the manifest of another backup set, %s", objectName, name)
	}
	return nil
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
