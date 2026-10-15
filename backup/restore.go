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
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/cenkalti/backoff"
	"golang.org/x/sync/errgroup"

	"github.com/bosgood/zfsbackup-go/backends"
	"github.com/bosgood/zfsbackup-go/files"
	"github.com/bosgood/zfsbackup-go/log"
	"github.com/bosgood/zfsbackup-go/zfs"
)

type downloadSequence struct {
	volume *files.VolumeInfo
	c      chan<- *files.VolumeInfo
}

// AutoRestore will compute which snapshots need to be restored to get to the snapshot provided,
// or to the latest snapshot of the volume provided
// nolint:funlen,gocyclo // Difficult to break this up
func AutoRestore(pctx context.Context, jobInfo *files.JobInfo) error {
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
	localCachePath, cerr := getCacheDir(jobInfo, jobInfo.Destinations[0])
	if cerr != nil {
		log.AppLogger.Errorf("Could not get cache dir for target %s due to error - %v.", backends.RedactURI(target), cerr)
		return cerr
	}

	// Sync the local cache
	manifests, _, serr := syncCache(ctx, jobInfo, localCachePath, backend)
	if serr != nil {
		log.AppLogger.Errorf("Could not sync cache dir for target %s due to error - %v.", backends.RedactURI(target), serr)
		return serr
	}

	// Only this volume's manifests: another dataset's cannot stop this restore.
	ours := manifests[:0]
	for _, manifest := range manifests {
		if manifestMayBeFor(manifest, jobInfo.ManifestPrefix, jobInfo.VolumeName) {
			ours = append(ours, manifest)
		}
	}
	decodedManifests, derr := readAndSortManifests(ctx, jobInfo, localCachePath, backend, ours)
	if derr != nil {
		return derr
	}
	manifestTree := linkManifests(decodedManifests)
	var ok bool
	var volumeSnaps []*files.JobInfo
	if volumeSnaps, ok = manifestTree[jobInfo.VolumeName]; !ok {
		log.AppLogger.Errorf("Could not find any snapshots for volume %s, none found on target.", jobInfo.VolumeName)
		return errors.New("could not determine any snapshots for provided volume")
	}

	// Restore to the latest snapshot available for the volume provided if no snapshot was provided
	if jobInfo.BaseSnapshot.Name == "" {
		log.AppLogger.Infof("Trying to determine latest snapshot for volume %s.", jobInfo.VolumeName)
		jobInfo.BaseSnapshot = volumeSnaps[len(volumeSnaps)-1].BaseSnapshot
		log.AppLogger.Infof("Restoring to snapshot %s.", jobInfo.BaseSnapshot.Name)
	}

	// The snapshots the local dataset already has: restore only what is missing
	log.AppLogger.Infof("Calculating how to restore to %s.", jobInfo.BaseSnapshot.Name)
	volume := jobInfo.LocalVolume
	parts := strings.Split(jobInfo.VolumeName, "/")
	if jobInfo.FullPath {
		parts[0] = volume
		volume = strings.Join(parts, "/")
	}

	if jobInfo.LastPath {
		volume = fmt.Sprintf("%s/%s", volume, parts[len(parts)-1])
	}

	local, listErr := zfs.GetSnapshotsAndBookmarks(ctx, volume)
	exists := listErr == nil
	if listErr != nil {
		// A target that does not exist yet. Other errors: refuseUnlistedTarget, for -F.
		local = []files.SnapshotInfo{}
	}

	// sources are the snapshots an incremental can start from: the target's, and the origin
	// snapshot. Only the target's own tell whether it holds a snapshot already.
	sources := local
	if jobInfo.Origin != "" {
		origin, oerr := originSnapshot(ctx, jobInfo.Origin)
		if oerr != nil {
			return oerr
		}
		sources = append(append([]files.SnapshotInfo(nil), local...), origin)
	}

	// Find the backup of the snapshot we want to restore to: the one that applies to what is here
	var matches []*files.JobInfo
	for _, job := range volumeSnaps {
		if job.BaseSnapshot.Name == jobInfo.BaseSnapshot.Name {
			matches = append(matches, job)
		}
	}
	jobToRestore := backupThatApplies(matches, sources)
	if jobToRestore == nil {
		log.AppLogger.Errorf("Could not find the snapshot %v for volume %s on backend.", jobInfo.BaseSnapshot.Name, jobInfo.VolumeName)
		return errors.New("could not find snapshot provided")
	}

	jobsToRestore := make([]*files.JobInfo, 0, 10)
	inChain := make(map[*files.JobInfo]bool)
	for {
		// See if the snapshots we want to restore already exist
		if ok := validateSnapShotExistsFromSnaps(&jobToRestore.BaseSnapshot, local, false); ok {
			if gerr := refuseOtherGUID(ctx, volume, &jobToRestore.BaseSnapshot); gerr != nil {
				return gerr
			}
			break
		}
		noteLocalSnapshotMismatch(volume, &jobToRestore.BaseSnapshot, local)
		if inChain[jobToRestore] {
			log.AppLogger.Errorf(
				"The backups at the destination that lead to %s form a loop: the backup of %s is its own ancestor.",
				jobInfo.BaseSnapshot.Name, jobToRestore.BaseSnapshot.Name,
			)
			return errors.New("the manifests at the destination form a loop")
		}
		inChain[jobToRestore] = true

		log.AppLogger.Infof("Adding backup job for %s to the restore list.", jobToRestore.BaseSnapshot.Name)
		jobsToRestore = append(jobsToRestore, jobToRestore)
		if jobToRestore.IncrementalSnapshot.Name == "" {
			// This is a full backup, no need to go further back
			break
		}
		if sourceIsLocal(jobToRestore, sources) {
			// Its source is here, whether or not a backup of the source is still at the destination
			break
		}
		if jobToRestore.ParentSnap == nil {
			// With a parent, the next round notes a mismatch of its base snapshot, the same one.
			noteLocalSnapshotMismatch(volume, &jobToRestore.IncrementalSnapshot, local)
			log.AppLogger.Errorf(
				"Want to restore parent snap %s but it is not found in the backend, aborting.",
				jobToRestore.IncrementalSnapshot.Name,
			)
			return errors.New("could not find parent snapshot")
		}
		// Nothing is received before the walk ends, so a refusal here leaves no partial state.
		if jobToRestore.Raw != jobToRestore.ParentSnap.Raw {
			err := fmt.Errorf(
				"cannot restore the chain of %s: the backup of %s was sent %s, the backup of %s it continues from %s; "+
					"restoring them would mix raw and plaintext streams",
				jobInfo.VolumeName, jobToRestore.BaseSnapshot.Name, withRaw(jobToRestore.Raw),
				jobToRestore.ParentSnap.BaseSnapshot.Name, withRaw(jobToRestore.ParentSnap.Raw),
			)
			log.AppLogger.Errorf("%v.", err)
			return err
		}
		jobToRestore = jobToRestore.ParentSnap
	}

	log.AppLogger.Infof("Need to restore %d snapshots.", len(jobsToRestore))
	if len(jobsToRestore) == 0 {
		log.AppLogger.Noticef("Selected base snapshot %s@%s already exists, nothing to do!", volume, jobInfo.BaseSnapshot.Name)
	}

	// -F: check what the whole chain destroys before the first receive, so a refusal never
	// leaves a part of the chain restored. The steps do not check again.
	chain := make([]*files.JobInfo, 0, len(jobsToRestore))
	for i := len(jobsToRestore) - 1; i >= 0; i-- {
		chain = append(chain, jobsToRestore[i])
	}
	if err := refuseUnlistedTarget(jobInfo, volume, listErr); err != nil {
		return err
	}
	if err := refuseDestroy(ctx, jobInfo, chain, volume, local, exists); err != nil {
		return err
	}

	// We have a list of snapshots we need to restore, start at the end and work our way down.
	// Only the first step clones the origin: zfs refuses -o origin= for a dataset that exists.
	origin := jobInfo.Origin
	defer func() { jobInfo.Origin = origin }()
	for i := len(jobsToRestore) - 1; i >= 0; i-- {
		if i != len(jobsToRestore)-1 {
			jobInfo.Origin = ""
		}
		jobInfo.BaseSnapshot = jobsToRestore[i].BaseSnapshot
		jobInfo.IncrementalSnapshot = jobsToRestore[i].IncrementalSnapshot
		jobInfo.Volumes = jobsToRestore[i].Volumes
		jobInfo.Compressor = jobsToRestore[i].Compressor
		jobInfo.Separator = jobsToRestore[i].Separator
		log.AppLogger.Infof("Restoring snapshot %s (%d/%d)", jobInfo.BaseSnapshot.Name, len(jobsToRestore)-i, len(jobsToRestore))
		if err := receive(ctx, jobInfo, false); err != nil {
			log.AppLogger.Errorf("Failed to restore snapshot.")
			return err
		}
	}

	log.AppLogger.Noticef("Done.")

	return nil
}

// backupThatApplies returns, among backups of one snapshot, an incremental whose chain of
// parents reaches a snapshot in local (zfs refuses a full stream into a dataset that has
// snapshots); else the full (it needs no parent, and still works once an old chain is gone);
// else the first of them; else nil. A chain reaches local when a backup in it is of a snapshot in
// local, or is an incremental from one: once the backup of that source is retired, the
// incremental still applies.
func backupThatApplies(backups []*files.JobInfo, local []files.SnapshotInfo) *files.JobInfo {
	for _, b := range backups {
		seen := make(map[*files.JobInfo]bool)
		for p := b; p != nil && !seen[p]; p = p.ParentSnap {
			seen[p] = true
			if validateSnapShotExistsFromSnaps(&p.BaseSnapshot, local, false) {
				return b
			}
			if p.IncrementalSnapshot.Name == "" {
				break
			}
			if sourceIsLocal(p, local) {
				return b
			}
		}
	}
	for _, b := range backups {
		if b.IncrementalSnapshot.Name == "" {
			return b
		}
	}
	if len(backups) == 0 {
		return nil
	}
	return backups[0]
}

// noteLocalSnapshotMismatch says so when local holds a snapshot named as snap but created at
// another time: the name is the same, the snapshot is not (destroyed and taken again, say). A
// chain walk that does not stop at it goes on to a full, which zfs refuses to receive into a
// dataset that has snapshots, with a message that does not name this cause.
func noteLocalSnapshotMismatch(volume string, snap *files.SnapshotInfo, local []files.SnapshotInfo) {
	if msg := localSnapshotMismatch(volume, snap, local); msg != "" {
		log.AppLogger.Noticef("%s The restore does not build on it.", msg)
	}
}

// localSnapshotMismatch returns a sentence that names both creation times when local holds a
// snapshot (not a bookmark) named as snap but created at another time, and "" otherwise.
func localSnapshotMismatch(volume string, snap *files.SnapshotInfo, local []files.SnapshotInfo) string {
	const layout = "2006-01-02 15:04:05 MST"
	for i := range local {
		if local[i].Name != snap.Name || local[i].Bookmark || local[i].Equal(snap) {
			continue
		}
		return fmt.Sprintf(
			"The local snapshot %s@%s is not the one backed up: it was created %s, the backup's on %s.",
			volume, snap.Name, local[i].CreationTime.UTC().Format(layout), snap.CreationTime.UTC().Format(layout),
		)
	}
	return ""
}

// originSnapshot lists the snapshot --origin names (dataset@snapshot), for the checks that need
// the creation time of what zfs receive -o origin= will clone.
func originSnapshot(ctx context.Context, origin string) (files.SnapshotInfo, error) {
	listed, err := zfs.GetSnapshotsAndBookmarks(ctx, origin)
	if err != nil {
		log.AppLogger.Errorf("Could not get origin snapshot %s info due to error: %v", origin, err)
		return files.SnapshotInfo{}, err
	}
	if len(listed) != 1 {
		log.AppLogger.Errorf("Could not find origin snapshot %s", origin)
		return files.SnapshotInfo{}, fmt.Errorf("could not find origin snapshot %s", origin)
	}
	return listed[0], nil
}

// sourceIsLocal returns whether the incremental backup b is from a snapshot in local.
func sourceIsLocal(b *files.JobInfo, local []files.SnapshotInfo) bool {
	source := b.IncrementalSnapshot // a copy: the check records whether it matched a bookmark
	return validateSnapShotExistsFromSnaps(&source, local, false)
}

// Receive will download and restore the backup job described to the Volume target provided.
func Receive(pctx context.Context, jobInfo *files.JobInfo) error {
	return receive(pctx, jobInfo, true)
}

// receive is Receive. checkDestroy is false when the caller ran refuseDestroy already, for the
// whole chain this backup is a step of (AutoRestore).
// nolint:funlen,gocyclo // Difficult to break this up
func receive(pctx context.Context, jobInfo *files.JobInfo, checkDestroy bool) error {
	ctx, cancel := context.WithCancel(pctx)
	defer cancel()

	target := jobInfo.Destinations[0]

	// Prepare the backend client
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

	// The dataset zfs receive writes to
	volume := jobInfo.LocalVolume
	parts := strings.Split(jobInfo.VolumeName, "/")
	if jobInfo.FullPath {
		parts[0] = volume
		volume = strings.Join(parts, "/")
	}

	if jobInfo.LastPath {
		volume = fmt.Sprintf("%s/%s", volume, parts[len(parts)-1])
	}

	manifest, err := readCachedManifest(ctx, jobInfo, localCachePath, backend, jobInfo.ManifestObjectName())
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			explainMissingManifest(ctx, jobInfo, localCachePath, backend)
		}
		log.AppLogger.Errorf("Error trying to retrieve manifest volume - %v", err)
		return err
	}

	// Compare with the manifest's snapshots: they carry the creation times of what was backed up.
	local, lerr := zfs.GetSnapshotsAndBookmarks(ctx, volume)
	if lerr != nil {
		// The dataset is not there yet
		log.AppLogger.Debugf("Could not list snapshots for %s: %v", volume, lerr)
		local = nil
	}
	if validateSnapShotExistsFromSnaps(&manifest.BaseSnapshot, local, false) {
		if gerr := refuseOtherGUID(ctx, volume, &manifest.BaseSnapshot); gerr != nil {
			return gerr
		}
		log.AppLogger.Noticef("Selected base snapshot %s@%s already exists, nothing to do!", volume, manifest.BaseSnapshot.Name)
		return nil
	}
	// zfs would refuse it, and with --force a full stream destroys the snapshots there: a name
	// collision is never what -F is for.
	if msg := localSnapshotMismatch(volume, &manifest.BaseSnapshot, local); msg != "" {
		log.AppLogger.Errorf(
			"%s Refusing to receive over it, with or without --force. It holds other data: to restore the backup, "+
				"rename it (zfs rename %s@%s %s@<another name>) or receive into another dataset, then run again.",
			msg, volume, manifest.BaseSnapshot.Name, volume,
		)
		return fmt.Errorf("the local snapshot %s@%s is not the one backed up", volume, manifest.BaseSnapshot.Name)
	}

	// Check that we have the parent snapshot this wants to restore from. In --auto, an earlier
	// step of the chain received it, and the listing above shows it. With --origin, zfs receive
	// clones that snapshot of another dataset and applies the stream to the clone: it is the
	// source then, and the target need not exist.
	if manifest.IncrementalSnapshot.Name != "" {
		sources := local
		if jobInfo.Origin != "" {
			origin, oerr := originSnapshot(ctx, jobInfo.Origin)
			if oerr != nil {
				return oerr
			}
			sources = append(append([]files.SnapshotInfo(nil), local...), origin)
		}
		source := manifest.IncrementalSnapshot // a copy: the check records whether it matched a bookmark
		if !validateSnapShotExistsFromSnaps(&source, sources, false) {
			noteLocalSnapshotMismatch(volume, &manifest.IncrementalSnapshot, sources)
			log.AppLogger.Errorf("Selected incremental snapshot does not exist!")
			return fmt.Errorf("selected incremental snapshot does not exist")
		}
	}

	if checkDestroy {
		if err = refuseUnlistedTarget(jobInfo, volume, lerr); err != nil {
			return err
		}
		if err = refuseDestroy(ctx, jobInfo, []*files.JobInfo{manifest}, volume, local, lerr == nil); err != nil {
			return err
		}
	}
	if err = refuseMountOverSource(ctx, jobInfo, manifest, volume); err != nil {
		return err
	}

	manifest.ManifestPrefix = jobInfo.ManifestPrefix
	manifest.SignKey = jobInfo.SignKey
	manifest.TrustedSignKeys = jobInfo.TrustedSignKeys
	manifest.EncryptKey = jobInfo.EncryptKey
	manifest.TrustedCompressor = jobInfo.TrustedCompressor
	if err = files.CheckDecompressor(manifest.Compressor, manifest.TrustedCompressor); err != nil {
		log.AppLogger.Errorf("Refusing to restore %s: %v", jobInfo.ManifestObjectName(), err)
		return err
	}

	// Get list of Objects
	toDownload := make([]string, len(manifest.Volumes))
	for idx := range manifest.Volumes {
		toDownload[idx] = manifest.Volumes[idx].ObjectName
	}

	// PreDownload step
	err = backend.PreDownload(ctx, toDownload)
	if err != nil {
		log.AppLogger.Errorf("Error trying to pre download backup set volumes - %v", err)
		return err
	}
	toDownload = nil

	// The download workers read through a progress wrapper; everything else keeps the raw
	// backend, as the wrapper hides backends.Sizer and the other optional interfaces. The label
	// names the backup, not the -d/-e mapped target.
	label := fmt.Sprintf("Downloading %s@%s from %s", manifest.VolumeName, manifest.BaseSnapshot.Name, backends.RedactURI(target))
	dl := backends.WithDownloadProgress(backend, backends.NewDownloadProgress(label, manifest.Volumes))

	// Prepare Download Pipeline
	usePipe := false
	fileBufferSize := jobInfo.MaxFileBuffer
	if fileBufferSize == 0 {
		fileBufferSize = 1
		usePipe = true
	}

	downloadChannel := make(chan downloadSequence, len(manifest.Volumes))
	bufferChannel := make(chan interface{}, fileBufferSize)
	orderedChannels := make([]chan *files.VolumeInfo, len(manifest.Volumes))
	defer close(bufferChannel)

	// Queue up files to download
	for idx := range manifest.Volumes {
		c := make(chan *files.VolumeInfo, 1)
		orderedChannels[idx] = c
		downloadChannel <- downloadSequence{manifest.Volumes[idx], c}
	}
	close(downloadChannel)

	var wg *errgroup.Group
	wg, ctx = errgroup.WithContext(ctx)

	// Kick off go routines to download
	for i := 0; i < fileBufferSize; i++ {
		wg.Go(func() error {
			for {
				select {
				case <-ctx.Done():
					return ctx.Err()
				case sequence, ok := <-downloadChannel:
					if !ok {
						return nil
					}
					if derr := downloadSequenceVolume(ctx, jobInfo, sequence, dl, bufferChannel, usePipe); derr != nil {
						return derr
					}
				}
			}
		})
	}

	// Order the downloaded Volumes
	orderedVolumes := make(chan *files.VolumeInfo, len(toDownload))
	wg.Go(func() error {
		defer close(orderedVolumes)
		for _, c := range orderedChannels {
			var vol *files.VolumeInfo
			select {
			case <-ctx.Done():
				return ctx.Err()
			case v, ok := <-c:
				if !ok {
					// Its download failed, and that error cancels ctx: let it be the one reported.
					<-ctx.Done()
					return ctx.Err()
				}
				vol = v
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case orderedVolumes <- vol:
			}
		}
		return nil
	})

	// Prepare ZFS Receive command
	cmd := zfs.GetZFSReceiveCommand(ctx, jobInfo)
	wg.Go(func() error {
		return receiveStream(ctx, cmd, manifest, orderedVolumes, bufferChannel)
	})

	// Wait for processes to finish
	err = wg.Wait()
	if err != nil {
		log.AppLogger.Errorf("There was an error during the restore process, aborting: %v", err)
		return err
	}

	// pctx: the errgroup canceled ctx when Wait returned.
	noteReceivedMountpoint(pctx, jobInfo, manifest, volume)
	log.AppLogger.Noticef("Done. Elapsed Time: %v", time.Since(jobInfo.StartTime))
	return nil
}

// explainMissingManifest says, when the destination has no manifest of the backup the flags
// name, which backups of that snapshot it does have and the flags that name each: the usual
// cause is an incremental received without -i, or a full with it. Its errors are warnings:
// the receive fails on the missing manifest whatever this finds.
func explainMissingManifest(ctx context.Context, j *files.JobInfo, localCache string, backend backends.Backend) {
	target := backends.RedactURI(j.Destinations[0])
	asked := "full backup"
	if j.IncrementalSnapshot.Name != "" {
		asked = "incremental backup from " + j.IncrementalSnapshot.Name
	}
	names, _, err := syncCache(ctx, j, localCache, backend)
	if err != nil {
		log.AppLogger.Warningf("Could not list the backups at %s to say what it holds: %v", target, err)
		return
	}
	ours := names[:0]
	for _, name := range names {
		if manifestMayBeFor(name, j.ManifestPrefix, j.VolumeName) {
			ours = append(ours, name)
		}
	}
	manifests, err := readAndSortManifests(ctx, j, localCache, backend, ours)
	if err != nil {
		log.AppLogger.Warningf("Could not read the backups at %s to say what it holds: %v", target, err)
		return
	}
	var holds []string
	for _, m := range manifests {
		if m.VolumeName != j.VolumeName || m.BaseSnapshot.Name != j.BaseSnapshot.Name {
			continue
		}
		if m.IncrementalSnapshot.Name == "" {
			holds = append(holds, "a full backup (run again without -i)")
		} else {
			holds = append(holds, fmt.Sprintf("an incremental backup from %s (run again with -i %s)",
				m.IncrementalSnapshot.Name, m.IncrementalSnapshot.Name))
		}
	}
	snapshot := j.VolumeName + "@" + j.BaseSnapshot.Name
	if len(holds) == 0 {
		log.AppLogger.Errorf(
			"No %s of %s at %s, and no other backup of it: zfsbackup list %s shows what is there.",
			asked, snapshot, target, target,
		)
		return
	}
	log.AppLogger.Errorf(
		"No %s of %s at %s. It has %s. Or receive --auto picks the chain itself.",
		asked, snapshot, target, strings.Join(holds, " and "),
	)
}

// streamHasMountpoint returns whether the stream of manifest may carry a mountpoint: it was
// sent with --properties or --replication.
func streamHasMountpoint(manifest *files.JobInfo) bool {
	return manifest.Properties || manifest.Replication
}

// mounts returns whether a dataset with this mountpoint property is mounted by zfs mount -a or
// at import: "none" and "legacy" are not, nor is a zvol's "-".
func mounts(mountpoint string) bool {
	return mountpoint != "none" && mountpoint != "legacy" && mountpoint != "-"
}

// refuseMountOverSource refuses a receive that would mount the restored dataset over a live
// one, unless --unmounted is set. The stream carries the source's mountpoint when it was sent
// with --properties or --replication. Two cases:
//   - The source is on this host with a mountpoint of its own (local or received: an inherited
//     one is not in the stream), and the target is another dataset. With --replication, each
//     dataset below the source counts too: the stream carries their mountpoints as well.
//   - The manifest records the mountpoints the stream sets (SourceMountpoints), and another
//     dataset is mounted at one of them on this host: a restore on another host, whose own
//     dataset uses that path.
//
// Only --unmounted lifts it: -F means a rollback, and a user who asked for that did not ask for
// a mount over live data.
func refuseMountOverSource(ctx context.Context, jobInfo, manifest *files.JobInfo, volume string) error {
	if !streamHasMountpoint(manifest) || jobInfo.NotMounted {
		return nil
	}
	if volume != manifest.VolumeName {
		rows, err := zfs.GetMountpoints(ctx, manifest.VolumeName, manifest.Replication)
		if err != nil {
			log.AppLogger.Debugf("%s is not on this host (%v): the received mountpoint cannot hide it.", manifest.VolumeName, err)
			rows = nil
		}
		for _, row := range rows {
			if !mounts(row.Value) || (row.Source != "local" && row.Source != "received") {
				continue
			}
			restored := volume + strings.TrimPrefix(row.Dataset, manifest.VolumeName)
			log.AppLogger.Errorf(
				"The backup of %s was sent with its properties, so its stream sets the mountpoint of %s to the source's "+
					"(now %s), and %s is on this host. Receiving it without --unmounted would mount %s over %s and hide "+
					"the live data. Run again with --unmounted, then move the restored dataset (zfs set mountpoint=<another "+
					"path> %s) before a reboot or zfs mount -a mounts it there.",
				row.Dataset, restored, row.Value, row.Dataset, restored, row.Value, restored,
			)
			return fmt.Errorf("refusing to mount %s over %s", restored, row.Dataset)
		}
	}
	return refuseMountOverRecordedPath(ctx, manifest, volume)
}

// refuseMountOverRecordedPath refuses the receive when the manifest records a mountpoint that the
// stream gives a dataset (SourceMountpoints), and a dataset other than the restored one is mounted
// at that path on this host.
func refuseMountOverRecordedPath(ctx context.Context, manifest *files.JobInfo, volume string) error {
	if len(manifest.SourceMountpoints) == 0 {
		return nil // an older manifest, or a stream with no mountpoint of its own
	}
	sources := make([]string, 0, len(manifest.SourceMountpoints))
	for source := range manifest.SourceMountpoints {
		sources = append(sources, source)
	}
	sort.Strings(sources)
	mounted, err := zfs.MountedFilesystems(ctx)
	if err != nil {
		log.AppLogger.Errorf(
			"Could not list the mounted filesystems to see whether %s is in use (%v). The stream mounts %s there. "+
				"Run again with --unmounted to receive it without a mount.",
			manifest.SourceMountpoints[sources[0]], err, restoredName(volume, manifest.VolumeName, sources[0]),
		)
		return err
	}
	for _, source := range sources {
		path := manifest.SourceMountpoints[source]
		restored := restoredName(volume, manifest.VolumeName, source)
		dataset, ok := mounted[path]
		if !ok || dataset == restored {
			continue
		}
		log.AppLogger.Errorf(
			"The backup of %s was sent with its properties, so its stream sets the mountpoint of %s to %s, and %s is "+
				"mounted there on this host. Receiving it without --unmounted would mount %s over %s and hide its data. "+
				"Run again with --unmounted, then move the restored dataset (zfs set mountpoint=<another path> %s) before "+
				"a reboot or zfs mount -a mounts it there.",
			manifest.VolumeName, restored, path, dataset, restored, dataset, restored,
		)
		return fmt.Errorf("refusing to mount %s over %s at %s", restored, dataset, path)
	}
	return nil
}

// restoredName is the dataset that a receive into volume makes of source, the stream's top
// dataset top or a dataset below it.
func restoredName(volume, top, source string) string {
	return volume + strings.TrimPrefix(source, top)
}

// recordSourceMountpoints records in j.SourceMountpoints the mountpoints that j's stream gives
// the datasets it creates, when the send has --properties or --replication: the source's own
// (local or received). With --replication, the stream holds each filesystem below the source
// too: record each whose mountpoint is its own, or inherited from a dataset in the stream (the
// restored child inherits it from the restored parent). An inherited mountpoint of the top
// dataset, and a default one, are not in the stream. Receive refuses to mount a restored dataset
// over another dataset mounted at that path.
func recordSourceMountpoints(ctx context.Context, j *files.JobInfo) {
	j.SourceMountpoints = nil
	if !streamHasMountpoint(j) {
		return
	}
	rows, err := zfs.GetMountpoints(ctx, j.VolumeName, j.Replication)
	if err != nil {
		log.AppLogger.Debugf("Could not get the mountpoints of %s: %v", j.VolumeName, err)
		return
	}
	for _, row := range rows {
		if !mounts(row.Value) || !mountpointInStream(j, row) {
			continue
		}
		if j.SourceMountpoints == nil {
			j.SourceMountpoints = make(map[string]string)
		}
		j.SourceMountpoints[row.Dataset] = row.Value
	}
}

// mountpointInStream returns whether the stream of j sets the mountpoint of row's dataset to
// row's value.
func mountpointInStream(j *files.JobInfo, row zfs.Mountpoint) bool {
	if row.Source == "local" || row.Source == "received" {
		return true
	}
	from, ok := strings.CutPrefix(row.Source, "inherited from ")
	return ok && row.Dataset != j.VolumeName && j.Replication &&
		(from == j.VolumeName || strings.HasPrefix(from, j.VolumeName+"/"))
}

// refuseOtherGUID refuses when the local snapshot of volume named as snap (and created in the
// same second) has another guid than the one the manifest records: it is another snapshot, for
// example of another host that took a snapshot of the same name in the same second. Without
// this, the restore would say "already exists" and receive nothing. A manifest without a guid
// (of an older version) is not checked.
func refuseOtherGUID(ctx context.Context, volume string, snap *files.SnapshotInfo) error {
	if snap.GUID == "" {
		return nil
	}
	guid, err := zfs.GetSnapshotGUID(ctx, volume, &files.SnapshotInfo{Name: snap.Name})
	if err != nil {
		log.AppLogger.Errorf("Could not get the guid of %s@%s to compare it with the backup's: %v", volume, snap.Name, err)
		return err
	}
	if guid == snap.GUID {
		return nil
	}
	log.AppLogger.Errorf(
		"The local snapshot %s@%s has the name and creation time of the one backed up, but another guid (%s, the "+
			"backup's is %s): it is another snapshot. Refusing to receive over it, with or without --force. It holds "+
			"other data: to restore the backup, rename it (zfs rename %s@%s %s@<another name>) or receive into another "+
			"dataset, then run again.",
		volume, snap.Name, guid, snap.GUID, volume, snap.Name, volume,
	)
	return fmt.Errorf("the local snapshot %s@%s is not the one backed up: its guid is %s, the backup's %s", volume, snap.Name, guid, snap.GUID)
}

// refuseDestroy refuses a receive with -F (--force) that makes zfs destroy data, unless
// --allowDestroy is set too. chain holds the backups the run receives, oldest first: one for an
// explicit receive, the whole chain for receive --auto, which checks before its first receive, so
// that it never stops after a part of the chain. local and exists describe the target before the
// first of them. With -F, zfs:
//   - replaces a target that exists with no snapshots (bookmarks hold no data), and all its data,
//     with a full stream;
//   - destroys the local snapshots newer than the source of an incremental. A snapshot of another
//     name taken in the same second as the source counts: creation times have one-second
//     steps, and it may be the newer one;
//   - with an incremental --replication stream, destroys each snapshot and dataset of the target
//     tree that the sending side did not have. The manifest does not say which ones the sender
//     had, so each local snapshot other than the source, and each dataset below the target, may
//     be one. What the earlier steps of the chain received came from the sender and does not
//     count: if the sender destroyed it later, the restore does the same.
//
// The rollback of changes made since the source snapshot is what -F is for, and stays allowed.
func refuseDestroy(
	ctx context.Context, jobInfo *files.JobInfo, chain []*files.JobInfo, volume string, local []files.SnapshotInfo, exists bool,
) error {
	if !jobInfo.Force || jobInfo.AllowDestroy || len(chain) == 0 {
		return nil
	}
	first := chain[0]
	var snaps []files.SnapshotInfo
	for _, snap := range local {
		if !snap.Bookmark {
			snaps = append(snaps, snap)
		}
	}
	var loss []string
	source := first.IncrementalSnapshot
	if source.Name == "" {
		if exists && len(snaps) == 0 {
			loss = append(loss, fmt.Sprintf("%s exists and has no snapshots: zfs replaces it, and all its data, with the full stream", volume))
		}
	} else if newer, tied := snapshotsNotOlder(snaps, &source); len(newer)+len(tied) > 0 {
		msg := fmt.Sprintf("zfs destroys the local snapshots newer than %s: %s", source.Name,
			strings.Join(prefixNames(volume, append(newer, tied...)), ", "))
		if len(tied) > 0 {
			msg += fmt.Sprintf(" (%s: taken in the same second as %s, so it may be newer)",
				strings.Join(prefixNames(volume, tied), ", "), source.Name)
		}
		loss = append(loss, msg)
	}
	if replicationIncremental(chain) {
		loss = append(loss, replicationLoss(ctx, volume, snaps, &source, exists)...)
	}
	if len(loss) == 0 {
		return nil
	}
	last := chain[len(chain)-1]
	log.AppLogger.Errorf(
		"Refusing to receive %s@%s with --force: %s. Add --allowDestroy to accept that loss, or receive into another "+
			"dataset.",
		volume, last.BaseSnapshot.Name, strings.Join(loss, "; "),
	)
	return fmt.Errorf("--force would destroy data in %s; --allowDestroy accepts it", volume)
}

// refuseUnlistedTarget refuses a receive with --force when the snapshots of volume could not be
// listed (listErr) for a reason other than that volume does not exist: refuseDestroy cannot tell
// then what -F destroys, and a target that is there with data but no snapshots would be replaced.
func refuseUnlistedTarget(jobInfo *files.JobInfo, volume string, listErr error) error {
	if !jobInfo.Force || jobInfo.AllowDestroy || listErr == nil || zfs.IsNotExist(listErr) {
		return nil
	}
	log.AppLogger.Errorf(
		"Refusing to receive into %s with --force: its snapshots could not be listed (%v), so what zfs would destroy "+
			"is not known. Fix the cause, add --allowDestroy to accept any loss, or receive into another dataset.",
		volume, listErr,
	)
	return fmt.Errorf("could not list the snapshots of %s to check what --force destroys: %w", volume, listErr)
}

// snapshotsNotOlder returns the names of the snapshots in snaps, other than source, that are newer
// than source, and those created in the same second (tied): zfs -F destroys the ones newer than the
// source of an incremental, and of a tie in creation time (one-second steps) either one may be the
// newer.
func snapshotsNotOlder(snaps []files.SnapshotInfo, source *files.SnapshotInfo) (newer, tied []string) {
	for _, snap := range snaps {
		switch {
		case snap.Name == source.Name:
		case snap.CreationTime.After(source.CreationTime):
			newer = append(newer, snap.Name)
		case snap.CreationTime.Equal(source.CreationTime):
			tied = append(tied, snap.Name)
		}
	}
	return newer, tied
}

// replicationIncremental returns whether chain holds an incremental sent with --replication.
func replicationIncremental(chain []*files.JobInfo) bool {
	for _, b := range chain {
		if b.Replication && b.IncrementalSnapshot.Name != "" {
			return true
		}
	}
	return false
}

// replicationLoss returns what an incremental --replication stream received with -F may destroy
// in the target volume, as it is before the first receive (snaps, exists): the snapshots older
// than the chain's source (source; none for a full) and each dataset below volume. The newer
// snapshots are refuseDestroy's own case.
func replicationLoss(ctx context.Context, volume string, snaps []files.SnapshotInfo, source *files.SnapshotInfo, exists bool) []string {
	var loss []string
	var older []string
	for _, snap := range snaps {
		if snap.Name != source.Name && snap.CreationTime.Before(source.CreationTime) {
			older = append(older, snap.Name)
		}
	}
	if len(older) > 0 {
		loss = append(loss, fmt.Sprintf("the stream was sent with --replication: zfs destroys each snapshot that the "+
			"sending side did not have, and the backup does not say which ones it had: %s",
			strings.Join(prefixNames(volume, older), ", ")))
	}
	if !exists {
		return loss
	}
	below, err := zfs.Descendants(ctx, volume)
	switch {
	case err != nil:
		loss = append(loss, fmt.Sprintf("the stream was sent with --replication: zfs destroys each dataset below %s "+
			"that the sending side did not have, and they could not be listed (%v)", volume, err))
	case len(below) > 0:
		loss = append(loss, fmt.Sprintf("the stream was sent with --replication: zfs destroys each dataset below %s "+
			"that the sending side did not have, and the backup does not say which ones it had: %s",
			volume, strings.Join(below, ", ")))
	}
	return loss
}

// prefixNames returns volume@name for each name.
func prefixNames(volume string, names []string) []string {
	full := make([]string, len(names))
	for i, name := range names {
		full[i] = volume + "@" + name
	}
	return full
}

// noteReceivedMountpoint says, after a receive with --unmounted of a stream that carries a
// mountpoint, where the restored dataset mounts next time something mounts it: --unmounted
// skips the mount once, it does not change the property.
func noteReceivedMountpoint(ctx context.Context, jobInfo, manifest *files.JobInfo, volume string) {
	if !streamHasMountpoint(manifest) || !jobInfo.NotMounted {
		return
	}
	mountpoint, source, err := zfs.GetMountpoint(ctx, volume)
	if err != nil || source != "received" || !mounts(mountpoint) {
		return
	}
	log.AppLogger.Noticef(
		"%s is not mounted, and its mountpoint from the stream is %s. A reboot or zfs mount -a mounts it there: "+
			"set another one first (zfs set mountpoint=<path> %s) if that path is in use.",
		volume, mountpoint, volume,
	)
}

// downloadSequenceVolume downloads one volume, with retries, and closes its sequence's channel once
// it is done with it, whether or not the download succeeded.
func downloadSequenceVolume(
	ctx context.Context, jobInfo *files.JobInfo, sequence downloadSequence, backend backends.Backend,
	bufferChannel chan interface{}, usePipe bool,
) error {
	defer close(sequence.c)
	select {
	case <-ctx.Done():
		return ctx.Err()
	case bufferChannel <- nil:
	}

	be := backoff.NewExponentialBackOff()
	be.MaxInterval = jobInfo.MaxBackoffTime
	be.MaxElapsedTime = jobInfo.MaxRetryTime
	retryconf := backoff.WithContext(be, ctx)

	operation := func() error {
		oerr := processSequence(ctx, sequence, backend, usePipe)
		if oerr != nil {
			log.AppLogger.Warningf("error trying to download file %s - %v", sequence.volume.ObjectName, oerr)
		}
		return oerr
	}

	log.AppLogger.Debugf("Downloading volume %s.", sequence.volume.ObjectName)

	if berr := backoff.Retry(operation, retryconf); berr != nil {
		log.AppLogger.Errorf("Failed to download volume %s due to error: %v, aborting...", sequence.volume.ObjectName, berr)
		return berr
	}
	return nil
}

func processSequence(ctx context.Context, sequence downloadSequence, backend backends.Backend, usePipe bool) error {
	r, rerr := backend.Download(ctx, sequence.volume.ObjectName)
	if rerr != nil {
		log.AppLogger.Infof("Could not get %s due to error %v.", sequence.volume.ObjectName, rerr)
		if errors.Is(rerr, fs.ErrNotExist) {
			return backoff.Permanent(rerr) // waiting will not bring it back
		}
		return rerr
	}
	defer r.Close()
	vol, err := files.CreateSimpleVolume(ctx, usePipe)
	if err != nil {
		// A local failure (full disk, no temp dir): another download does not change it.
		log.AppLogger.Noticef("Could not create temporary file to download %s due to error - %v.", sequence.volume.ObjectName, err)
		return backoff.Permanent(err)
	}

	vol.ObjectName = sequence.volume.ObjectName
	if usePipe {
		// Before receiveStream can get it: see WillExtract.
		vol.WillExtract()
		// ctx is the errgroup's of Receive. It is canceled when another part of the restore
		// fails (zfs receive exits, a volume does not extract), and then nobody reads the pipe
		// any more: a write to it must fail, not wait forever. A restore that works cancels ctx
		// only after Wait, when this function has returned and stop has run.
		stop := context.AfterFunc(ctx, func() { vol.Abort(context.Cause(ctx)) })
		defer stop()
		sequence.c <- vol
	}

	// Read at most one byte more than the manifest records: a larger object is not the volume, and
	// need not be downloaded whole to tell.
	limit := int64(math.MaxInt64)
	if sequence.volume.Size < math.MaxInt64 {
		limit = int64(sequence.volume.Size) + 1
	}
	src := &readErrReader{r: io.LimitReader(r, limit)}
	_, err = io.Copy(vol, src)
	if err != nil {
		log.AppLogger.Noticef("Could not download file %s to the local cache dir due to error - %v.", sequence.volume.ObjectName, err)
		copyErr := err
		if err = vol.Close(); err != nil {
			log.AppLogger.Warningf("Could not close volume %s due to error - %v", sequence.volume.ObjectName, err)
		}
		if err = vol.DeleteVolume(); err != nil {
			log.AppLogger.Warningf("Could not delete volume %s due to error - %v", sequence.volume.ObjectName, err)
		}
		if usePipe {
			return backoff.Permanent(fmt.Errorf("cannot retry when using no file buffer, aborting"))
		}
		if src.err == nil {
			// The write to the temporary file failed, not the download: a retry fails the same way.
			return backoff.Permanent(copyErr)
		}
		return copyErr
	}
	if cerr := vol.Close(); cerr != nil {
		log.AppLogger.Noticef("Could not close temporary file to download %s due to error - %v.", sequence.volume.ObjectName, cerr)
		if !usePipe {
			// A late write error of the temporary file (ENOSPC, EDQUOT): local, as above.
			_ = vol.DeleteVolume()
			return backoff.Permanent(cerr)
		}
		return cerr
	}

	// Verify the size and SHA256 hash. A mismatch is the stored object, not the transfer: a
	// retry would download the same bytes again, for up to --maxRetryTime.
	if vol.Size != sequence.volume.Size || vol.SHA256Sum != sequence.volume.SHA256Sum {
		if !usePipe {
			if err = vol.DeleteVolume(); err != nil {
				log.AppLogger.Noticef("Could not delete temporary file to download %s due to error - %v.", sequence.volume.ObjectName, err)
			}
		}
		if vol.Size > sequence.volume.Size {
			return backoff.Permanent(fmt.Errorf(
				"%s does not match its manifest: it is larger than the %d bytes the manifest records",
				sequence.volume.ObjectName, sequence.volume.Size,
			))
		}
		return backoff.Permanent(fmt.Errorf(
			"%s does not match its manifest: got %d bytes with SHA256 %s, want %d bytes with SHA256 %s",
			sequence.volume.ObjectName, vol.Size, vol.SHA256Sum, sequence.volume.Size, sequence.volume.SHA256Sum,
		))
	}
	log.AppLogger.Debugf("Downloaded %s.", sequence.volume.ObjectName)

	if !usePipe {
		sequence.c <- vol
	}

	return nil
}

// readErrReader records the error its reader returns, other than io.EOF: io.Copy does not say
// whether a failure was the read or the write.
type readErrReader struct {
	r   io.Reader
	err error
}

func (e *readErrReader) Read(p []byte) (int, error) {
	n, err := e.r.Read(p)
	if err != nil && !errors.Is(err, io.EOF) {
		e.err = err
	}
	return n, err
}

func receiveStream(ctx context.Context, cmd *exec.Cmd, j *files.JobInfo, c <-chan *files.VolumeInfo, buffer <-chan interface{}) error {
	buf := bytes.NewBuffer(nil)
	cin, cout := io.Pipe()
	cmd.Stdin = cin
	cmd.Stderr = buf
	var group *errgroup.Group
	var once sync.Once
	group, ctx = errgroup.WithContext(ctx)

	// Start the zfs receive command
	log.AppLogger.Infof("Starting zfs receive command: %s", strings.Join(cmd.Args, " "))
	err := cmd.Start()
	if err != nil {
		log.AppLogger.Errorf("Error starting zfs command - %v", err)
		return err
	}

	defer func() {
		if cmd.ProcessState == nil || !cmd.ProcessState.Exited() {
			err = cmd.Process.Kill()
			if err != nil {
				log.AppLogger.Errorf("Could not kill zfs receive command due to error - %v", err)
				return
			}
			err = cmd.Process.Release()
			if err != nil {
				log.AppLogger.Errorf("Could not release resources from zfs send command due to error - %v", err)
				return
			}
		}
	}()

	// Extract ZFS stream from files and send it to the zfs command
	group.Go(func() error {
		defer once.Do(func() { cout.Close() })
		for {
			select {
			case vol, ok := <-c:
				if !ok {
					return nil
				}
				log.AppLogger.Debugf("Processing %s.", vol.ObjectName)
				eerr := vol.Extract(ctx, j, false)
				if eerr != nil {
					log.AppLogger.Errorf("Error while trying to read from volume %s - %v", vol.ObjectName, eerr)
					// In pipe mode its writer waits for this side to read: let it fail too.
					vol.Abort(eerr)
					// Must be eerr, not err: err is the outer cmd.Start() error and is
					// nil here, so returning it would report a failed decompress or
					// decrypt as a successful restore of a truncated stream.
					return eerr
				}
				_, eerr = io.Copy(cout, vol)
				if eerr != nil {
					log.AppLogger.Errorf("Error while trying to read from volume %s - %v", vol.ObjectName, eerr)
					// zfs receive exited, or the volume is bad: nothing reads the rest of the pipe.
					vol.Abort(eerr)
					return eerr
				}
				if err = vol.Close(); err != nil {
					log.AppLogger.Warningf("Could not close volume %s due to error - %v", vol.ObjectName, err)
				}
				if err = vol.DeleteVolume(); err != nil {
					log.AppLogger.Warningf("Could not delete volume %s due to error - %v", vol.ObjectName, err)
				}
				log.AppLogger.Debugf("Processed %s.", vol.ObjectName)
				vol = nil
				<-buffer
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	})

	group.Go(func() error {
		defer once.Do(func() { cout.Close() })
		return cmd.Wait()
	})

	// Wait for the command to finish
	err = group.Wait()
	if err != nil {
		log.AppLogger.Errorf("Error waiting for zfs command to finish - %v: %s", err, buf.String())
		return err
	}
	log.AppLogger.Infof("zfs receive completed without error")

	return nil
}

// downloadTo downloads the manifest objectName to toPath, atomically: toPath is never a partial
// download. A manifest object longer than files.MaxManifestBytes is an errObjectTooLarge, and
// leaves nothing at toPath: ReadManifest would reject it anyway (its gzip and pgp framing never
// make a real manifest longer than the JSON it holds), and whoever can write the destination
// could otherwise fill the cache's disk.
func downloadTo(ctx context.Context, backend backends.Backend, objectName, toPath string) error {
	return downloadAtMost(ctx, backend, objectName, toPath, int64(files.MaxManifestBytes))
}

// errObjectTooLarge is a download that was cut off at its limit.
var errObjectTooLarge = errors.New("the object is larger than expected")

// downloadAtMost is downloadTo with limit bytes as the limit.
func downloadAtMost(ctx context.Context, backend backends.Backend, objectName, toPath string, limit int64) error {
	r, rerr := backend.Download(ctx, objectName)
	if rerr != nil {
		log.AppLogger.Errorf("Could not download file %s to the local cache dir due to error - %v.", objectName, rerr)
		return rerr
	}
	defer r.Close()
	if err := files.WriteFileAtomic(filepath.Dir(toPath), filepath.Base(toPath), &atMostReader{r: r, left: limit}); err != nil {
		if errors.Is(err, errObjectTooLarge) {
			err = fmt.Errorf("%s is larger than %d MiB, more than any manifest: %w", objectName, limit>>20, err)
		}
		log.AppLogger.Errorf("Could not download file %s to the local cache dir due to error - %v.", objectName, err)
		return err
	}
	log.AppLogger.Debugf("Downloaded %s to local cache.", objectName)
	return nil
}

// atMostReader reads r, and fails with errObjectTooLarge once r has more than left bytes.
type atMostReader struct {
	r    io.Reader
	left int64
}

func (a *atMostReader) Read(p []byte) (int, error) {
	if int64(len(p)) > a.left+1 {
		p = p[:a.left+1]
	}
	n, err := a.r.Read(p)
	if a.left -= int64(n); a.left < 0 {
		return 0, errObjectTooLarge
	}
	return n, err
}
