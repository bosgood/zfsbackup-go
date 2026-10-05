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
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/cenkalti/backoff"
	"golang.org/x/sync/errgroup"

	"github.com/someone1/zfsbackup-go/backends"
	"github.com/someone1/zfsbackup-go/files"
	"github.com/someone1/zfsbackup-go/log"
)

// parseBackupVolume returns the dataset of a backup volume object name written with any of separators.
func parseBackupVolume(name string, separators []string) (string, bool) {
	for _, sep := range separators {
		if dataset, _, _, _, ok := files.ParseBackupVolumeObjectName(name, sep); ok {
			return dataset, true
		}
	}
	return "", false
}

// nestedDestinations returns "<dir>/" for every listed object that is the manifest of another
// destination nested below this one ("<dir>/<manifestPrefix>...manifest..."). Objects under such
// a directory belong to that destination, whatever their names parse as from here.
func nestedDestinations(objects []string, manifestPrefix string) []string {
	var roots []string
	for _, obj := range objects {
		if strings.HasPrefix(obj, manifestPrefix) {
			continue
		}
		idx := strings.Index(obj, "/"+manifestPrefix)
		if idx < 0 || !strings.Contains(obj[idx:], ".manifest") {
			continue
		}
		roots = append(roots, obj[:idx+1])
	}
	return roots
}

// nestedUnder returns the root of roots that obj is under, or "".
func nestedUnder(obj string, roots []string) string {
	for _, root := range roots {
		if strings.HasPrefix(obj, root) {
			return root
		}
	}
	return ""
}

// Clean will remove files found in the desination that are not found in any of the manifests found locally or in the destination.
// If cleanLocal is true, then local manifests not found in the destination are ignored and deleted. This function will optionally
// delete broken backup sets in the destination if the --force flag is provided.
// nolint:funlen,gocyclo // Difficult to break this up
func Clean(pctx context.Context, jobInfo *files.JobInfo, cleanLocal, dryRun bool) error {
	ctx, cancel := context.WithCancel(pctx)
	defer cancel()

	// Prepare the backend client
	target := jobInfo.Destinations[0]
	backend, berr := prepareBackend(ctx, jobInfo, target, nil)
	if berr != nil {
		log.AppLogger.Errorf("Could not initialize backend for target %s due to error - %v.", backends.RedactURI(target), berr)
		return berr
	}
	defer backend.Close()

	// Get the local cache dir
	localCachePath, cerr := getCacheDir(jobInfo, target)
	if cerr != nil {
		log.AppLogger.Errorf("Could not get cache dir for target %s due to error - %v.", backends.RedactURI(target), cerr)
		return cerr
	}

	// Sync the local cache
	safeManifests, localOnlyFiles, serr := syncCache(ctx, jobInfo, localCachePath, backend)
	if serr != nil {
		log.AppLogger.Errorf("Could not sync cache dir for target %s due to error - %v.", backends.RedactURI(target), serr)
		return serr
	}

	// TODO: The following can be done in a much more efficient way (probably)
	allObjects, err := backend.List(ctx, "")
	if err != nil {
		log.AppLogger.Errorf("Could not list objects in backend %s due to error - %v", backends.RedactURI(target), err)
		return err
	}

	// With no manifests at all there is nothing to protect: the URI is almost certainly wrong,
	// or the manifests are already gone. Either way, a bulk delete must not be one command away.
	if len(safeManifests) == 0 && len(localOnlyFiles) == 0 && len(allObjects) > 0 {
		err = fmt.Errorf(
			"destination %s holds %d objects but no manifests; refusing to clean. If this is intended, delete the objects manually",
			target, len(allObjects),
		)
		log.AppLogger.Errorf("%v.", err)
		return err
	}

	// Read in Manifests
	decodedManifests := make([]*files.JobInfo, 0, len(safeManifests))
	for _, manifest := range safeManifests {
		manifestPath := filepath.Join(localCachePath, manifest)
		decodedManifest, oerr := readManifest(ctx, manifestPath, jobInfo)
		if oerr != nil {
			log.AppLogger.Errorf("Could not read manifest %s due to error - %v", manifestPath, oerr)
			return oerr
		}
		decodedManifests = append(decodedManifests, decodedManifest)
	}

	if !cleanLocal {
		if len(localOnlyFiles) > 0 {
			// nolint:lll // Long log message
			log.AppLogger.Noticef(
				"There are %d local manifests not found in the destination, use --cleanLocal to delete these locally and any of their volumes found in the destination.",
				len(localOnlyFiles),
			)
			for _, manifest := range localOnlyFiles {
				manifestPath := filepath.Join(localCachePath, manifest)
				decodedManifest, oerr := readManifest(ctx, manifestPath, jobInfo)
				if oerr != nil {
					log.AppLogger.Errorf("Could not read manifest %s due to error - %v", manifestPath, oerr)
					return oerr
				}
				decodedManifests = append(decodedManifests, decodedManifest)
			}
		}
	} else {
		for _, manifest := range localOnlyFiles {
			manifestPath := filepath.Join(localCachePath, manifest)
			if dryRun {
				log.AppLogger.Noticef("Would delete local manifest %s.", manifestPath)
				continue
			}
			err = os.Remove(manifestPath)
			if err != nil {
				log.AppLogger.Errorf("Could not delete local manifest %s due to error - %v", manifestPath, err)
				return err
			}
			log.AppLogger.Debugf("Deleted %s.", manifestPath)
		}
	}

	// Only delete what we can name: objects that parse as a backup volume written by this tool,
	// with any separator in use here. Manifests and everything else are left alone.
	separators := []string{files.DefaultSeparator}
	addSeparator := func(sep string) {
		for _, have := range separators {
			if have == sep {
				return
			}
		}
		separators = append(separators, sep)
	}
	addSeparator(jobInfo.Separator)
	datasets := make(map[string]bool)
	for _, manifest := range decodedManifests {
		addSeparator(manifest.Separator)
		datasets[manifest.VolumeName] = true
	}

	// Volumes an older version wrote under a raw key prefix are outside this destination: say so,
	// since nothing here will ever list or delete them.
	if finder, ok := backend.(backends.LegacyVolumeFinder); ok {
		names := make([]string, 0, len(datasets))
		for dataset := range datasets {
			names = append(names, dataset)
		}
		sort.Strings(names)
		key, ferr := finder.FindLegacyVolume(ctx, names, separators)
		if ferr != nil {
			log.AppLogger.Errorf("Could not check %s for volumes in the old key layout due to error - %v", backends.RedactURI(target), ferr)
			return ferr
		}
		if key != "" {
			log.AppLogger.Warningf(
				"Found %s, which looks like a backup volume written by an older version outside of %s. "+
					"clean cannot delete it. If no manifest lists it (an interrupted send), delete it and the volumes next to it by hand.",
				key, backends.RedactURI(target),
			)
		}
	}
	manifestPrefix := jobInfo.ManifestPrefix + jobInfo.Separator
	if jobInfo.Separator == "" {
		manifestPrefix = jobInfo.ManifestPrefix + files.DefaultSeparator
	}
	// A running send has volumes at the destination that its cached manifest does not list yet.
	// Hold each dataset's send lock for the rest of the run; where a send holds it, leave that
	// dataset alone. This only sees sends on this host with the same --workingDirectory: the lock
	// is a file in it.
	busy := make(map[string]bool)
	for dataset := range datasets {
		lock, lockPath, lerr := volumeLock(dataset)
		if lerr != nil {
			log.AppLogger.Errorf("Cannot init lock for %s. reason: %v", dataset, lerr)
			return lerr
		}
		if lerr = lock.TryLock(); lerr != nil {
			log.AppLogger.Noticef(
				"A send of %s appears to be running (%s: %v); leaving its volumes alone. Run clean again when it is done.",
				dataset, lockPath, lerr,
			)
			busy[dataset] = true
			continue
		}
		if p, held := legacyLockHolder(dataset); held {
			if uerr := lock.Unlock(); uerr != nil {
				log.AppLogger.Warningf("Could not release lock %s: %v", lockPath, uerr)
			}
			log.AppLogger.Noticef(
				"A send of %s by an older version appears to be running (pid %d holds %s); leaving its volumes alone. Run clean again when it is done.",
				dataset, p.Pid, legacyVolumeLockPath(dataset),
			)
			busy[dataset] = true
			continue
		}
		defer func() {
			if uerr := lock.Unlock(); uerr != nil {
				log.AppLogger.Warningf("Could not release lock %s: %v", lockPath, uerr)
			}
		}()
	}

	nested := nestedDestinations(allObjects, manifestPrefix)
	candidates := make([]string, 0, len(allObjects))
	skipped := 0
	for _, obj := range allObjects {
		if strings.HasPrefix(obj, manifestPrefix) {
			continue
		}
		if root := nestedUnder(obj, nested); root != "" {
			log.AppLogger.Noticef("Skipping %s: %s holds its own manifests, so it is another destination.", obj, root)
			skipped++
			continue
		}
		dataset, ok := parseBackupVolume(obj, separators)
		if !ok {
			log.AppLogger.Noticef("Skipping unrecognized object %s (not a backup volume written by this tool).", obj)
			skipped++
			continue
		}
		// A volume of a dataset no manifest here mentions belongs to some other destination
		// (e.g. s3://bucket/p/ seen from s3://bucket) - it is not ours to delete.
		if !datasets[dataset] {
			log.AppLogger.Noticef("Skipping %s: no manifest at this destination is for dataset %s.", obj, dataset)
			skipped++
			continue
		}
		if busy[dataset] {
			continue
		}
		candidates = append(candidates, obj)
	}
	if skipped > 0 {
		log.AppLogger.Noticef("Found %d objects in destination that clean does not recognize; they will not be deleted.", skipped)
	}
	allObjects = candidates

	// Go through all manifests and remove from the allObjects list what we know should exist
	for _, manifest := range decodedManifests {
		// Not judged at all, so --force cannot remove a set a running send is still completing.
		if busy[manifest.VolumeName] {
			continue
		}
		for vidx, vol := range manifest.Volumes {
			found := false
			for idx := range allObjects {
				if strings.Compare(vol.ObjectName, allObjects[idx]) == 0 {
					allObjects = append(allObjects[:idx], allObjects[idx+1:]...)
					found = true
					break
				}
			}

			if !found {
				// Broken backup set! inform the user!
				if jobInfo.Force {
					log.AppLogger.Warningf(
						"The following backup set is missing volume %s. Removing entire backupset:\n\n%s",
						vol.ObjectName, manifest.String(),
					)

					// Compute the manifest object name and cache name to delete
					manifest.ManifestPrefix = jobInfo.ManifestPrefix
					manifest.SignKey = jobInfo.SignKey
					manifest.EncryptKey = jobInfo.EncryptKey
					tempManifest, terr := files.CreateManifestVolume(ctx, manifest)
					if terr != nil {
						log.AppLogger.Errorf("Could not compute manifest path due to error - %v.", terr)
						return terr
					}
					allObjects = append(allObjects, tempManifest.ObjectName)
					if err = tempManifest.Close(); err != nil {
						log.AppLogger.Warningf("Could not close temporary manifest %v", err)
					}
					if err = tempManifest.DeleteVolume(); err != nil {
						log.AppLogger.Warningf("Could not delete temporary manifest %v", err)
					}
					// nolint:gosec // MD5 not used for cryptographic purposes here
					manifestPath := filepath.Join(localCachePath, fmt.Sprintf("%x", md5.Sum([]byte(tempManifest.ObjectName))))
					if dryRun {
						log.AppLogger.Noticef("Would delete local cached manifest %s.", manifestPath)
					} else {
						err = os.Remove(manifestPath)
						if err != nil {
							log.AppLogger.Errorf("Could not delete local manifest %s due to error - %v. Continuing.", manifestPath, err)
						}
					}

					// Delete all volumes already processed in the manifest
					for i := range vidx {
						allObjects = append(allObjects, manifest.Volumes[i].ObjectName)
					}
					break
				} else {
					log.AppLogger.Warningf(
						"The following backup set is missing volume %s:\n\n%s\n\nPass the --force flag to delete this backup set.",
						vol.ObjectName, manifest.String(),
					)
				}
			}
		}
	}

	if dryRun {
		log.AppLogger.Noticef("Dry-run: would delete %d objects in destination.", len(allObjects))
		for _, obj := range allObjects {
			log.AppLogger.Noticef("Would delete %s.", joinURI(target, obj))
		}
		log.AppLogger.Noticef("Done.")
		return nil
	}

	log.AppLogger.Noticef("Starting to delete %d objects in destination.", len(allObjects))

	// Whatever is left in allObjects was not found in any manifest, delete 'em
	var group *errgroup.Group
	group, ctx = errgroup.WithContext(ctx)

	deleteChan := make(chan string, len(allObjects))
	for _, obj := range allObjects {
		deleteChan <- obj
	}
	close(deleteChan)

	// Let's not slam the endpoint with a lot of concurrent requests, pick a sensible default and stick to it
	for range 5 {
		group.Go(func() error {
			for {
				select {
				case <-ctx.Done():
					return ctx.Err()
				case objectPath, ok := <-deleteChan:
					if !ok {
						return nil
					}

					be := backoff.NewExponentialBackOff()
					be.MaxInterval = time.Minute
					be.MaxElapsedTime = 10 * time.Minute
					retryconf := backoff.WithContext(be, ctx)

					operation := func() error {
						return backend.Delete(ctx, objectPath)
					}

					if berr := backoff.Retry(operation, retryconf); berr != nil {
						log.AppLogger.Errorf("Could not delete object %s in due to error - %v", objectPath, berr)
						return berr
					}

					log.AppLogger.Debugf("Deleted %s.", joinURI(target, objectPath))
				}
			}
		})
	}

	log.AppLogger.Debugf("Waiting to delete %d objects in destination.", len(allObjects))
	err = group.Wait()
	if err != nil {
		log.AppLogger.Errorf("Could not finish clean operation due to error, aborting: %v", err)
		return err
	}

	log.AppLogger.Noticef("Done.")
	return nil
}
