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
	"strings"
	"sync"
	"time"

	"github.com/cenkalti/backoff"
	"golang.org/x/sync/errgroup"

	"github.com/someone1/zfsbackup-go/backends"
	"github.com/someone1/zfsbackup-go/files"
	"github.com/someone1/zfsbackup-go/log"
	"github.com/someone1/zfsbackup-go/zfs"
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

	snapshots, err := zfs.GetSnapshotsAndBookmarks(ctx, volume)
	if err != nil {
		// TODO: There are some error cases that are ok to ignore!
		snapshots = []files.SnapshotInfo{}
	}

	if jobInfo.Origin != "" {
		originSnapshot, oerr := zfs.GetSnapshotsAndBookmarks(ctx, jobInfo.Origin)
		if oerr != nil {
			log.AppLogger.Errorf("Could not get origin snapshot %s info due to error: %v", jobInfo.Origin, oerr)
			return oerr
		}

		if len(originSnapshot) == 1 {
			// The origin snapshot can be added as an existing snapshot we can start the restore from
			snapshots = append(snapshots, originSnapshot[0])
		} else {
			log.AppLogger.Errorf("Could not find origin snapshot %s", jobInfo.Origin)
			return fmt.Errorf("could not find origin snapshot %s", jobInfo.Origin)
		}
	}

	// Find the backup of the snapshot we want to restore to: the one that applies to what is here
	var matches []*files.JobInfo
	for _, job := range volumeSnaps {
		if job.BaseSnapshot.Name == jobInfo.BaseSnapshot.Name {
			matches = append(matches, job)
		}
	}
	jobToRestore := backupThatApplies(matches, snapshots)
	if jobToRestore == nil {
		log.AppLogger.Errorf("Could not find the snapshot %v for volume %s on backend.", jobInfo.BaseSnapshot.Name, jobInfo.VolumeName)
		return errors.New("could not find snapshot provided")
	}

	jobsToRestore := make([]*files.JobInfo, 0, 10)
	inChain := make(map[*files.JobInfo]bool)
	for {
		// See if the snapshots we want to restore already exist
		if ok := validateSnapShotExistsFromSnaps(&jobToRestore.BaseSnapshot, snapshots, false); ok {
			break
		}
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
		if sourceIsLocal(jobToRestore, snapshots) {
			// Its source is here, whether or not a backup of the source is still at the destination
			break
		}
		if jobToRestore.ParentSnap == nil {
			log.AppLogger.Errorf(
				"Want to restore parent snap %s but it is not found in the backend, aborting.",
				jobToRestore.IncrementalSnapshot.Name,
			)
			return errors.New("could not find parent snapshot")
		}
		jobToRestore = jobToRestore.ParentSnap
	}

	log.AppLogger.Infof("Need to restore %d snapshots.", len(jobsToRestore))

	// We have a list of snapshots we need to restore, start at the end and work our way down
	for i := len(jobsToRestore) - 1; i >= 0; i-- {
		jobInfo.BaseSnapshot = jobsToRestore[i].BaseSnapshot
		jobInfo.IncrementalSnapshot = jobsToRestore[i].IncrementalSnapshot
		jobInfo.Volumes = jobsToRestore[i].Volumes
		jobInfo.Compressor = jobsToRestore[i].Compressor
		jobInfo.Separator = jobsToRestore[i].Separator
		log.AppLogger.Infof("Restoring snapshot %s (%d/%d)", jobInfo.BaseSnapshot.Name, len(jobsToRestore)-i, len(jobsToRestore))
		if err := Receive(ctx, jobInfo); err != nil {
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

// sourceIsLocal returns whether the incremental backup b is from a snapshot in local.
func sourceIsLocal(b *files.JobInfo, local []files.SnapshotInfo) bool {
	source := b.IncrementalSnapshot // a copy: the check records whether it matched a bookmark
	return validateSnapShotExistsFromSnaps(&source, local, false)
}

// Receive will download and restore the backup job described to the Volume target provided.
// nolint:funlen,gocyclo // Difficult to break this up
func Receive(pctx context.Context, jobInfo *files.JobInfo) error {
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

	// See if the snapshots we want to restore already exist
	volume := jobInfo.LocalVolume
	parts := strings.Split(jobInfo.VolumeName, "/")
	if jobInfo.FullPath {
		parts[0] = volume
		volume = strings.Join(parts, "/")
	}

	if jobInfo.LastPath {
		volume = fmt.Sprintf("%s/%s", volume, parts[len(parts)-1])
	}

	if jobInfo.BaseSnapshot.CreationTime.IsZero() {
		if ok, verr := validateSnapShotExists(ctx, &jobInfo.BaseSnapshot, volume, false); verr != nil {
			log.AppLogger.Errorf("Cannot validate if selected base snapshot exists due to error - %v", verr)
			return verr
		} else if ok {
			log.AppLogger.Noticef("Selected base snapshot already exists, nothing to do!")
			return nil
		}
	}

	// Check that we have the parent snap shot this wants to restore from
	if jobInfo.IncrementalSnapshot.Name != "" && jobInfo.IncrementalSnapshot.CreationTime.IsZero() {
		if ok, verr := validateSnapShotExists(ctx, &jobInfo.IncrementalSnapshot, volume, false); verr != nil {
			log.AppLogger.Errorf("Cannot validate if selected incremental snapshot exists due to error - %v", verr)
			return verr
		} else if !ok {
			log.AppLogger.Errorf("Selected incremental snapshot does not exist!")
			return fmt.Errorf("selected incremental snapshot does not exist")
		}
	}

	manifest, err := readCachedManifest(ctx, jobInfo, localCachePath, backend, jobInfo.ManifestObjectName())
	if err != nil {
		log.AppLogger.Errorf("Error trying to retrieve manifest volume - %v", err)
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
					if derr := downloadSequenceVolume(ctx, jobInfo, sequence, backend, bufferChannel, usePipe); derr != nil {
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

	log.AppLogger.Noticef("Done. Elapsed Time: %v", time.Since(jobInfo.StartTime))
	return nil
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
		log.AppLogger.Noticef("Could not create temporary file to download %s due to error - %v.", sequence.volume.ObjectName, err)
		return err
	}

	vol.ObjectName = sequence.volume.ObjectName
	if usePipe {
		sequence.c <- vol
	}

	// Read at most one byte more than the manifest records: a larger object is not the volume, and
	// need not be downloaded whole to tell.
	limit := int64(math.MaxInt64)
	if sequence.volume.Size < math.MaxInt64 {
		limit = int64(sequence.volume.Size) + 1
	}
	_, err = io.Copy(vol, io.LimitReader(r, limit))
	if err != nil {
		log.AppLogger.Noticef("Could not download file %s to the local cache dir due to error - %v.", sequence.volume.ObjectName, err)
		if err = vol.Close(); err != nil {
			log.AppLogger.Warningf("Could not close volume %s due to error - %v", sequence.volume.ObjectName, err)
		}
		if err = vol.DeleteVolume(); err != nil {
			log.AppLogger.Warningf("Could not delete volume %s due to error - %v", sequence.volume.ObjectName, err)
		}
		if usePipe {
			return backoff.Permanent(fmt.Errorf("cannot retry when using no file buffer, aborting"))
		}
		return err
	}
	if cerr := vol.Close(); cerr != nil {
		log.AppLogger.Noticef("Could not close temporary file to download %s due to error - %v.", sequence.volume.ObjectName, cerr)
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
					// Must be eerr, not err: err is the outer cmd.Start() error and is
					// nil here, so returning it would report a failed decompress or
					// decrypt as a successful restore of a truncated stream.
					return eerr
				}
				_, eerr = io.Copy(cout, vol)
				if eerr != nil {
					log.AppLogger.Errorf("Error while trying to read from volume %s - %v", vol.ObjectName, eerr)
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
	return downloadAtMost(ctx, backend, objectName, toPath, files.MaxManifestBytes)
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
