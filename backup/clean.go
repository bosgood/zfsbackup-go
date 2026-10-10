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

// cleanManifest is a manifest clean read, and where it read it.
type cleanManifest struct {
	object   string         // its name at the destination; "" for a manifest only in the cache
	path     string         // its copy in the cache
	manifest *files.JobInfo // nil when it could not be read
	// foreign marks a local-only manifest cached under another name than it has with this
	// --manifestPrefix: the state of another job that shares the destination, not of this one.
	foreign bool
}

// cleanView is what clean reads from a destination and its manifest cache.
type cleanView struct {
	manifests  []cleanManifest // of the manifests at the destination
	localOnly  []cleanManifest // of the manifests only in the cache
	allObjects []string
	datasets   map[string]bool // of every decoded manifest, local-only included, foreign excluded
}

// readForClean syncs the cache of the destination at backend, lists it and decodes its manifests.
func readForClean(
	ctx context.Context, jobInfo *files.JobInfo, target, localCachePath string, backend backends.Backend, cleanLocal bool,
) (*cleanView, error) {
	manifests, localOnlyFiles, serr := syncCache(ctx, jobInfo, localCachePath, backend)
	if serr != nil {
		log.AppLogger.Errorf("Could not sync cache dir for target %s due to error - %v.", backends.RedactURI(target), serr)
		return nil, serr
	}

	// TODO: The following can be done in a much more efficient way (probably)
	allObjects, err := backend.List(ctx, "")
	if err != nil {
		log.AppLogger.Errorf("Could not list objects in backend %s due to error - %v", backends.RedactURI(target), err)
		return nil, err
	}

	view := &cleanView{allObjects: allObjects, datasets: make(map[string]bool)}
	view.manifests = make([]cleanManifest, 0, len(manifests))
	for _, manifest := range manifests {
		decodedManifest, oerr := readCachedManifest(ctx, jobInfo, localCachePath, backend, manifest)
		if oerr != nil {
			return nil, oerr
		}
		view.manifests = append(view.manifests, cleanManifest{
			object:   manifest,
			path:     filepath.Join(localCachePath, cachedManifestName(manifest)),
			manifest: decodedManifest,
		})
		view.datasets[decodedManifest.VolumeName] = true
	}

	// Local-only manifests are the cached state of sends that never finished (or whose
	// destination copy is gone). Decode them: their datasets are locked, and without
	// --cleanLocal their volumes are protected as live. Under --cleanLocal an undecodable one
	// cannot be matched to a lock, so it is deleted as before. One cached under another
	// --manifestPrefix belongs to another job: its volumes are protected, and it is never deleted.
	view.localOnly = make([]cleanManifest, 0, len(localOnlyFiles))
	for _, manifest := range localOnlyFiles {
		manifestPath := filepath.Join(localCachePath, manifest)
		decodedManifest, oerr := readManifest(ctx, manifestPath, jobInfo)
		if oerr != nil && !cleanLocal {
			// Its volumes are unknown, so none can be told apart from orphans.
			log.AppLogger.Errorf(
				"Could not read local manifest %s due to error - %v. It is not at the destination, so it cannot be "+
					"downloaded again: if it is the state of an interrupted send (cut short by a kill), delete it, "+
					"or run clean with --cleanLocal.",
				manifestPath, oerr,
			)
			return nil, oerr
		}
		if oerr != nil {
			log.AppLogger.Warningf("Could not read local manifest %s due to error - %v; --cleanLocal deletes it.", manifestPath, oerr)
			decodedManifest = nil
		}
		foreign := decodedManifest != nil &&
			cachedManifestName(decodedManifest.StoredManifestObjectName(jobInfo.ManifestPrefix)) != manifest
		if decodedManifest != nil && !foreign {
			view.datasets[decodedManifest.VolumeName] = true
		}
		view.localOnly = append(view.localOnly, cleanManifest{path: manifestPath, manifest: decodedManifest, foreign: foreign})
	}
	return view, nil
}

// cleanReadRounds is how many times clean reads the destination before giving up on it settling.
const cleanReadRounds = 3

// Clean deletes the backup volumes at the destination that no manifest lists, for the datasets
// that have manifests there or in the cache. It deletes only objects that parse as backup volumes,
// never a manifest, and refuses a destination that lists no manifests under jobInfo.ManifestPrefix
// but holds objects. It holds the send lock of every dataset it judges; a dataset whose lock a
// send holds is left alone, cached manifest included.
//
// Cached manifests not at the destination (local-only) are the state of interrupted sends: they
// keep their volumes, unless cleanLocal is true, which deletes them and their volumes. One cached
// under another manifest prefix is never deleted. jobInfo.Force also deletes broken backup sets
// at the destination (manifest included), but never a local-only manifest. With dryRun, Clean
// only logs what it would delete.
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

	// A running send has volumes at the destination that its cached manifest does not list yet,
	// and that cached manifest is what --resume continues from. Hold each dataset's send lock for
	// the rest of the run; where a send holds it, leave that dataset alone, cached manifest
	// included. This only sees sends on this host with the same --workingDirectory: the lock is a
	// file in it.
	//
	// The datasets come from the manifests, so read once to learn them, lock them, and read
	// again: a send that finished in between has uploaded volumes and a manifest the first read
	// did not see, and every decision below must come from a read made under the locks. A
	// dataset that first appears in a later read is locked and read again.
	busy := make(map[string]bool)
	locked := make(map[string]bool)
	var view *cleanView
	var err error
	for round := 1; ; round++ {
		if view, err = readForClean(ctx, jobInfo, target, localCachePath, backend, cleanLocal); err != nil {
			return err
		}
		names := make([]string, 0, len(view.datasets))
		for dataset := range view.datasets {
			if !locked[dataset] && !busy[dataset] {
				names = append(names, dataset)
			}
		}
		if len(names) == 0 {
			break // every dataset this read names was locked (or found busy) before it
		}
		if round == cleanReadRounds {
			err = fmt.Errorf("the manifests at %s keep changing; run clean again", backends.RedactURI(target))
			log.AppLogger.Errorf("%v.", err)
			return err
		}
		sort.Strings(names)
		for _, dataset := range names {
			lock, lockPath, lerr := volumeLock(dataset)
			if lerr != nil {
				log.AppLogger.Errorf("Cannot init lock for %s. reason: %v", dataset, lerr)
				return lerr
			}
			if lerr = lock.TryLock(); lerr != nil {
				log.AppLogger.Noticef(
					"A send of %s appears to be running (%s: %v); leaving its volumes and its cached manifest alone. "+
						"Run clean again when it is done.",
					dataset, lockPath, lerr,
				)
				busy[dataset] = true
				continue
			}
			locked[dataset] = true
			defer func() {
				if uerr := lock.Unlock(); uerr != nil {
					log.AppLogger.Warningf("Could not release lock %s: %v", lockPath, uerr)
				}
			}()
		}
	}
	decodedManifests, localOnlyManifests, allObjects, datasets := view.manifests, view.localOnly, view.allObjects, view.datasets

	manifestPrefix := jobInfo.ManifestPrefix + jobInfo.Separator
	if jobInfo.Separator == "" {
		manifestPrefix = jobInfo.ManifestPrefix + files.DefaultSeparator
	}

	// With no manifests at the destination there is nothing to protect: the URI or
	// --manifestPrefix is almost certainly wrong, or the manifests are already gone. Either way,
	// a bulk delete must not be one command away. Cached manifests do not count: with a wrong
	// prefix every one of them is local-only, and --cleanLocal would delete all their volumes.
	if len(decodedManifests) == 0 && len(allObjects) > 0 {
		err = fmt.Errorf(
			"destination %s holds %d objects but no manifests; refusing to clean. No manifest there starts with %q: "+
				"check --manifestPrefix and --separator. If the manifests are gone on purpose, delete the objects manually",
			backends.RedactURI(target), len(allObjects), manifestPrefix,
		)
		if len(localOnlyManifests) > 0 {
			err = fmt.Errorf("%w. The %d cached manifests not at the destination are kept, and so are their volumes",
				err, len(localOnlyManifests))
		}
		log.AppLogger.Errorf("%v.", err)
		return err
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
	for _, manifest := range decodedManifests {
		addSeparator(manifest.manifest.Separator)
	}
	for _, local := range localOnlyManifests {
		if local.manifest != nil {
			addSeparator(local.manifest.Separator)
		}
	}

	foreign := 0
	for _, local := range localOnlyManifests {
		if local.foreign {
			foreign++
		}
	}
	if foreign > 0 {
		log.AppLogger.Noticef(
			"There are %d cached manifests of another --manifestPrefix at this destination; leaving them and their volumes alone.",
			foreign,
		)
	}
	if !cleanLocal && len(localOnlyManifests) > foreign {
		// nolint:lll // Long log message
		log.AppLogger.Noticef(
			"There are %d local manifests not found in the destination, use --cleanLocal to delete these locally and any of their volumes found in the destination.",
			len(localOnlyManifests)-foreign,
		)
	}
	// Removed only once their volumes are gone: a local manifest is how a later clean finds
	// volumes a failed delete left behind.
	var localDeletes []string
	for _, local := range localOnlyManifests {
		if !cleanLocal || local.foreign || (local.manifest != nil && busy[local.manifest.VolumeName]) {
			decodedManifests = append(decodedManifests, local) // live: its volumes stay
			continue
		}
		if dryRun {
			log.AppLogger.Noticef("Would delete local manifest %s.", local.path)
			continue
		}
		localDeletes = append(localDeletes, local.path)
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
	nested := nestedDestinations(allObjects, manifestPrefix)
	// Every volume a manifest here names is ours, whatever directory it seems to be in: from
	// s3://bucket, the volumes of tank/data are under tank/, which may also be a nested
	// destination's directory.
	named := make(map[string]bool)
	for _, m := range decodedManifests {
		for _, vol := range m.manifest.Volumes {
			named[vol.ObjectName] = true
		}
	}
	present := make(map[string]bool, len(allObjects))
	candidates := make([]string, 0, len(allObjects))
	skipped := 0
	for _, obj := range allObjects {
		present[obj] = true
		if strings.HasPrefix(obj, manifestPrefix) {
			continue
		}
		if root := nestedUnder(obj, nested); root != "" && !named[obj] {
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

	// Go through all manifests and keep what we know should exist. Whether a set is complete is
	// judged against everything listed, not just the candidates.
	keep := make(map[string]bool)
	var forcedManifests []string
	for _, m := range decodedManifests {
		manifest := m.manifest
		// Not judged at all, so --force cannot remove a set a running send is still completing.
		if busy[manifest.VolumeName] {
			continue
		}
		missing := ""
		for _, vol := range manifest.Volumes {
			if !present[vol.ObjectName] {
				missing = vol.ObjectName
				break
			}
		}
		// A local-only manifest kept here is the state of an interrupted send (or of another
		// job): --force is about broken sets at the destination, and only --cleanLocal deletes it.
		localOnly := m.object == ""
		if missing == "" || !jobInfo.Force || localOnly {
			switch {
			case missing != "" && localOnly:
				log.AppLogger.Noticef(
					"The local manifest %s is missing volume %s (an interrupted send lists only what it uploaded); "+
						"--force leaves it alone, --cleanLocal deletes it.",
					m.path, missing,
				)
			case missing != "":
				// Broken backup set! inform the user!
				log.AppLogger.Warningf(
					"The following backup set is missing volume %s:\n\n%s\n\nPass the --force flag to delete this backup set.",
					missing, manifest.String(),
				)
			}
			for _, vol := range manifest.Volumes {
				keep[vol.ObjectName] = true
			}
			continue
		}
		// Its volumes are not kept, so the ones that are there are deleted with the
		// orphans. The manifest deleted is the object read, not a name computed from its
		// content, which nothing has checked.
		log.AppLogger.Warningf(
			"The following backup set is missing volume %s. Removing entire backupset:\n\n%s",
			missing, manifest.String(),
		)
		forcedManifests = append(forcedManifests, m.object)
		if dryRun {
			log.AppLogger.Noticef("Would delete local cached manifest %s.", m.path)
		} else {
			localDeletes = append(localDeletes, m.path)
		}
	}
	deletes := make([]string, 0, len(candidates)+len(forcedManifests))
	for _, obj := range candidates {
		if !keep[obj] {
			deletes = append(deletes, obj)
		}
	}
	deletes = append(deletes, forcedManifests...)

	if dryRun {
		log.AppLogger.Noticef("Dry-run: would delete %d objects in destination.", len(deletes))
		for _, obj := range deletes {
			log.AppLogger.Noticef("Would delete %s.", joinURI(target, obj))
		}
		log.AppLogger.Noticef("Done.")
		return nil
	}

	log.AppLogger.Noticef("Starting to delete %d objects in destination.", len(deletes))

	// Whatever is left in deletes was not found in any manifest, delete 'em
	var group *errgroup.Group
	group, ctx = errgroup.WithContext(ctx)

	deleteChan := make(chan string, len(deletes))
	for _, obj := range deletes {
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

	log.AppLogger.Debugf("Waiting to delete %d objects in destination.", len(deletes))
	err = group.Wait()
	if err != nil {
		log.AppLogger.Errorf("Could not finish clean operation due to error, aborting: %v", err)
		return err
	}

	for _, path := range localDeletes {
		if err = os.Remove(path); err != nil {
			log.AppLogger.Errorf("Could not delete local manifest %s due to error - %v", path, err)
			return err
		}
		log.AppLogger.Debugf("Deleted %s.", path)
	}

	log.AppLogger.Noticef("Done.")
	return nil
}
