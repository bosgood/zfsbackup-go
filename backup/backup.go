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
	"crypto/md5" //nolint:gosec // Not used for cryptography
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cenkalti/backoff"
	"github.com/dustin/go-humanize"
	"github.com/miolini/datacounter"
	"github.com/nightlyone/lockfile"
	"golang.org/x/sync/errgroup"

	"github.com/someone1/zfsbackup-go/backends"
	"github.com/someone1/zfsbackup-go/config"
	"github.com/someone1/zfsbackup-go/files"
	"github.com/someone1/zfsbackup-go/log"
	"github.com/someone1/zfsbackup-go/zfs"
)

var (
	ErrNoOp       = errors.New("nothing new to sync")
	manifestmutex sync.Mutex
)

// ProcessSmartOptions will compute the snapshots to use
func ProcessSmartOptions(ctx context.Context, jobInfo *files.JobInfo) error {
	snapshots, err := zfs.GetSnapshotsAndBookmarks(context.Background(), jobInfo.VolumeName)
	if err != nil {
		return err
	}

	// An explicit full backup never consults the destinations, so skip the manifest
	// sync entirely rather than downloading and decoding manifests we would discard.
	destBackups := make([][]*files.JobInfo, len(jobInfo.Destinations))
	if !jobInfo.Full {
		for idx := range jobInfo.Destinations {
			b, derr := getBackupsForTarget(ctx, jobInfo.VolumeName, jobInfo.Destinations[idx], jobInfo)
			if derr != nil {
				return derr
			}
			destBackups[idx] = b
		}
	}

	return selectSmartSnapshots(jobInfo, snapshots, destBackups)
}

// snapshotMatches reports whether a snapshot is eligible as a backup base given
// the configured prefix/suffix. Base snapshots can never be bookmarks.
func snapshotMatches(s files.SnapshotInfo, prefix, suffix string) bool {
	if s.Bookmark {
		return false
	}
	if prefix != "" && !strings.HasPrefix(s.Name, prefix) {
		return false
	}
	if suffix != "" && !strings.HasSuffix(s.Name, suffix) {
		return false
	}
	return true
}

// newestMatchingSnapshot returns the newest snapshot matching prefix/suffix.
// snapshots must be sorted newest-first (as `zfs list -S creation` returns).
func newestMatchingSnapshot(snapshots []files.SnapshotInfo, prefix, suffix string) *files.SnapshotInfo {
	for i := range snapshots {
		if snapshotMatches(snapshots[i], prefix, suffix) {
			return &snapshots[i]
		}
	}
	return nil
}

// selectSmartSnapshots applies the smart backup plan (see planSmartSnapshots)
// to jobInfo: it sets BaseSnapshot, and IncrementalSnapshot for an incremental
// backup. It returns ErrNoOp when there is nothing new to send.
func selectSmartSnapshots(jobInfo *files.JobInfo, snapshots []files.SnapshotInfo, destBackups [][]*files.JobInfo) error {
	p, err := planSmartSnapshots(jobInfo, snapshots, destBackups)
	if err != nil {
		return err
	}
	log.AppLogger.Infof("Smart backup plan: %s.", p)
	if p.FullDue {
		log.AppLogger.Noticef(
			"The last full backup is older than %v; the next full waits for a full backup candidate newer than the last backup.",
			jobInfo.FullIfOlderThan,
		)
	}
	if p.Action == PlanNoop {
		if p.Reason == reasonSourcePruned {
			log.AppLogger.Warningf(
				"The last backup (%s) is no longer on the pool, so no incremental can be sent, and no full backup candidate "+
					"newer than it exists yet. Nothing will be backed up until one does.",
				p.Source.Name,
			)
		}
		if p.Reason == reasonAlreadyBackedUp {
			log.AppLogger.Noticef("The full backup candidate is already backed up as a full at every destination; not sending it again.")
		}
		return ErrNoOp
	}
	jobInfo.BaseSnapshot = p.Base
	if p.Action == PlanIncremental {
		jobInfo.IncrementalSnapshot = p.Source
	}
	return nil
}

// BackupsAtTarget lists the backups of volume at the target destination,
// newest-first, as the smart options see them. It syncs the local manifest
// cache for the target and only reads from the target.
func BackupsAtTarget(ctx context.Context, volume, target string, jobInfo *files.JobInfo) ([]*files.JobInfo, error) {
	return getBackupsForTarget(ctx, volume, target, jobInfo)
}

// Will list all backups found in the target destination
func getBackupsForTarget(ctx context.Context, volume, target string, jobInfo *files.JobInfo) ([]*files.JobInfo, error) {
	// Prepare the backend client
	backend, berr := prepareBackend(ctx, jobInfo, target, nil)
	if berr != nil {
		log.AppLogger.Errorf("Could not initialize backend due to error - %v.", berr)
		return nil, berr
	}

	// Get the local cache dir
	localCachePath, cerr := getCacheDir(target)
	if cerr != nil {
		log.AppLogger.Errorf("Could not get cache dir for target %s due to error - %v.", backends.RedactURI(target), cerr)
		return nil, cerr
	}

	// Sync the local cache
	safeManifests, _, serr := syncCache(ctx, jobInfo, localCachePath, backend)
	if serr != nil {
		log.AppLogger.Errorf("Could not sync cache dir for target %s due to error - %v.", backends.RedactURI(target), serr)
		return nil, serr
	}

	// Read in Manifests and display
	decodedManifests := make([]*files.JobInfo, 0, len(safeManifests))
	for _, manifest := range safeManifests {
		manifestPath := filepath.Join(localCachePath, manifest)
		decodedManifest, oerr := readManifest(ctx, manifestPath, jobInfo)
		if oerr != nil {
			return nil, oerr
		}
		if strings.Compare(decodedManifest.VolumeName, volume) == 0 {
			decodedManifests = append(decodedManifests, decodedManifest)
		}
	}

	sort.SliceStable(decodedManifests, func(i, j int) bool {
		return decodedManifests[i].BaseSnapshot.CreationTime.After(decodedManifests[j].BaseSnapshot.CreationTime)
	})
	return decodedManifests, nil
}

// reportDryRun validates the selected snapshots exist and logs what a real
// backup would do, without acquiring the lock or uploading anything.
func reportDryRun(ctx context.Context, jobInfo *files.JobInfo) error {
	if ok, verr := validateSnapShotExists(ctx, &jobInfo.BaseSnapshot, jobInfo.VolumeName, false); verr != nil {
		log.AppLogger.Errorf("Cannot validate if selected base snapshot exists due to error - %v", verr)
		return verr
	} else if !ok {
		log.AppLogger.Errorf("Selected base snapshot does not exist!")
		return fmt.Errorf("selected base snapshot does not exist")
	}

	if jobInfo.IncrementalSnapshot.Name != "" {
		if ok, verr := validateSnapShotExists(ctx, &jobInfo.IncrementalSnapshot, jobInfo.VolumeName, true); verr != nil {
			log.AppLogger.Errorf("Cannot validate if selected incremental snapshot exists due to error - %v", verr)
			return verr
		} else if !ok {
			log.AppLogger.Errorf("Selected incremental snapshot does not exist!")
			return fmt.Errorf("selected incremental snapshot does not exist")
		}
	}

	backupType := "full"
	if jobInfo.IncrementalSnapshot.Name != "" {
		backupType = "incremental"
	}

	log.AppLogger.Noticef(
		"Dry-run: would perform a %s backup of %s@%s (snapshot created %v).",
		backupType, jobInfo.VolumeName, jobInfo.BaseSnapshot.Name, jobInfo.BaseSnapshot.CreationTime,
	)
	if jobInfo.IncrementalSnapshot.Name != "" {
		log.AppLogger.Noticef(
			"Dry-run: incremental from %s (created %v).",
			jobInfo.IncrementalSnapshot.Name, jobInfo.IncrementalSnapshot.CreationTime,
		)
	}
	log.AppLogger.Noticef(
		"Dry-run: would upload to %d destination(s): %s",
		len(jobInfo.Destinations), strings.Join(jobInfo.Destinations, ", "),
	)
	log.AppLogger.Noticef("Dry-run: ZFS send command: %s", strings.Join(zfs.GetZFSSendCommand(ctx, jobInfo).Args, " "))

	if size, err := zfs.GetZFSSendDryRun(ctx, jobInfo); err != nil {
		log.AppLogger.Debugf("Dry-run: could not estimate ZFS send size - %v", err)
	} else {
		log.AppLogger.Noticef("Dry-run: estimated ZFS stream size: %d (%s)", size, humanize.IBytes(size))
	}

	return nil
}

// Backup will initiate a backup with the provided configuration.
// nolint:funlen,gocyclo // Difficult to break this up
// destination is one place a backup set is sent to: its URI and the backend initialized for it.
// Keeping them in one value means a backend can never be reported, or cached, under another's URI.
type destination struct {
	uri     string
	backend backends.Backend
}

// String is the URI as it may appear in logs.
func (d destination) String() string {
	return backends.RedactURI(d.uri)
}

func Backup(pctx context.Context, jobInfo *files.JobInfo, dryRun bool) error {
	ctx, cancel := context.WithCancel(pctx)
	defer cancel()

	if dryRun {
		return reportDryRun(ctx, jobInfo)
	}

	uploadBuffer := make(chan bool, jobInfo.MaxParallelUploads)
	defer close(uploadBuffer)

	// Prepare the destinations first: refusing to overwrite a backup set and verifying a resume
	// both need them, and neither may touch the cache when it fails.
	var dests []destination
	defer func() {
		log.AppLogger.Debugf("Cleaning up resources...")
		for _, d := range dests {
			if cerr := d.backend.Close(); cerr != nil {
				log.AppLogger.Warningf("Could not properly close backend due to error - %v", cerr)
			}
		}
	}()
	for _, uri := range jobInfo.Destinations {
		backend, berr := prepareBackend(ctx, jobInfo, uri, uploadBuffer)
		if berr != nil {
			log.AppLogger.Errorf("Could not initialize backend due to error - %v.", berr)
			return berr
		}
		dests = append(dests, destination{uri: uri, backend: backend})
		if _, cerr := getCacheDir(uri); cerr != nil {
			log.AppLogger.Errorf("Could not create cache for destination %s due to error - %v.", backends.RedactURI(uri), cerr)
			return cerr
		}
	}

	// Make sure nobody else is working on the same volume/dataset we are!
	// nolint:gosec // MD5 not used for cryptographic purposes
	lockFilePath := filepath.Join(os.TempDir(), fmt.Sprintf("zfsbackup.%x.lck", md5.Sum([]byte(jobInfo.VolumeName))))
	lock, lferr := lockfile.New(lockFilePath)
	if lferr != nil {
		log.AppLogger.Errorf("Cannot init lock. reason: %v", lferr)
		return lferr
	}
	lferr = lock.TryLock()

	if lferr != nil {
		log.AppLogger.Errorf(
			"Cannot lock %q, reason: %v. If no other execution of %s is working on %s, you may forcefully remove the lock file located %s.",
			lock, lferr, config.ProgramName, jobInfo.VolumeName, lockFilePath,
		)
		return lferr
	}
	defer func() {
		if err := lock.Unlock(); err != nil {
			log.AppLogger.Warningf("Could not release lock %s: %v", lockFilePath, err)
		}
	}()

	// Only now, holding the lock: checked any earlier, an overlapping send of the same set
	// could finish between the check and the lock, and this one would then overwrite it.
	if done, err := refuseExistingSet(ctx, jobInfo, dests); err != nil || done {
		return err
	}

	if jobInfo.Resume {
		if err := tryResume(ctx, jobInfo, dests); err != nil {
			return err
		}
	}

	fileBufferSize := jobInfo.MaxFileBuffer
	if fileBufferSize == 0 {
		fileBufferSize = 1
	}

	// Validate the snapshots we want to use exist
	if ok, verr := validateSnapShotExists(ctx, &jobInfo.BaseSnapshot, jobInfo.VolumeName, false); verr != nil {
		log.AppLogger.Errorf("Cannot validate if selected base snapshot exists due to error - %v", verr)
		return verr
	} else if !ok {
		log.AppLogger.Errorf("Selected base snapshot does not exist!")
		return fmt.Errorf("selected base snapshot does not exist")
	}

	if jobInfo.IncrementalSnapshot.Name != "" {
		if ok, verr := validateSnapShotExists(ctx, &jobInfo.IncrementalSnapshot, jobInfo.VolumeName, true); verr != nil {
			log.AppLogger.Errorf("Cannot validate if selected incremental snapshot exists due to error - %v", verr)
			return verr
		} else if !ok {
			log.AppLogger.Errorf("Selected incremental snapshot does not exist!")
			return fmt.Errorf("selected incremental snapshot does not exist")
		}
	}

	startCh := make(chan *files.VolumeInfo, fileBufferSize) // Sent to ZFS command and meant to be closed when done
	stepCh := make(chan *files.VolumeInfo, fileBufferSize)  // Used as input to first backend, closed when final manifest is sent through

	var maniwg sync.WaitGroup
	maniwg.Add(1)

	fileBuffer := make(chan bool, fileBufferSize)
	for i := 0; i < fileBufferSize; i++ {
		fileBuffer <- true
	}

	var group *errgroup.Group
	group, ctx = errgroup.WithContext(ctx)

	// Used to prevent closing the upload pipeline after the ZFS command is done
	// so we can send the manifest file up after all volumes have made it to the backends.
	go func() {
		defer maniwg.Done()
		for {
			select {
			case vol, ok := <-startCh:
				if !ok {
					return
				}
				maniwg.Add(1)
				select {
				// Might take a while to pass along the volume so be sure to listen to context cancellations
				case stepCh <- vol:
					continue
				case <-ctx.Done():
					return
				}
			case <-ctx.Done():
				return
			}
		}
	}()

	// Start the ZFS send stream
	group.Go(func() error {
		return sendStream(ctx, jobInfo, startCh, fileBuffer)
	})

	var channels []<-chan *files.VolumeInfo
	channels = append(channels, stepCh)

	if jobInfo.MaxFileBuffer != 0 {
		uri := backends.DeleteBackendPrefix + "://"
		backend, berr := prepareBackend(ctx, jobInfo, uri, uploadBuffer)
		if berr != nil {
			log.AppLogger.Errorf("Could not initialize backend due to error - %v.", berr)
			return berr
		}
		jobInfo.Destinations = append(jobInfo.Destinations, uri)
		dests = append(dests, destination{uri: uri, backend: backend})
	}

	// Setup plumbing
	for _, d := range dests {
		out, waitgroup := retryUploadChainer(ctx, channels[len(channels)-1], d.backend, jobInfo, d.uri)
		channels = append(channels, out)
		group.Go(waitgroup.Wait)
	}

	// Create and copy a copy of the manifest during the backup procedure for future retry requests
	group.Go(func() error {
		defer close(fileBuffer)
		lastChan := channels[len(channels)-1]
		for {
			select {
			case vol, ok := <-lastChan:
				if !ok {
					return nil
				}
				if !vol.IsManifest {
					log.AppLogger.Debugf("Volume %s has finished the entire pipeline.", vol.ObjectName)
					log.AppLogger.Debugf("Adding %s to the manifest volume list.", vol.ObjectName)
					manifestmutex.Lock()
					jobInfo.Volumes = append(jobInfo.Volumes, vol)
					manifestmutex.Unlock()
					// Write a manifest file and save it locally in order to resume later
					manifestVol, err := saveManifest(ctx, jobInfo, false)
					if err != nil {
						return err
					}
					if err = manifestVol.DeleteVolume(); err != nil {
						log.AppLogger.Warningf("Error deleting temporary manifest file  - %v", err)
					}
					maniwg.Done()
				} else {
					// Manifest has been processed, we're done!
					return nil
				}
				select {
				// May take a while to add to buffer channel so listen for context cancellations.
				case <-ctx.Done():
					return ctx.Err()

				case fileBuffer <- true:
				}
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	})

	// Final Manifest Creation
	group.Go(func() error {
		// Wait until the ZFS send command has completed and all volumes have been uploaded to all backends,
		// unless the pipeline fails first: then no manifest may be written, and maniwg may never reach zero.
		allUploaded := make(chan struct{})
		go func() {
			maniwg.Wait()
			close(allUploaded)
		}()
		select {
		case <-allUploaded:
		case <-ctx.Done():
			return ctx.Err()
		}
		log.AppLogger.Infof("All volumes dispatched in pipeline, finalizing manifest file.")
		manifestmutex.Lock()
		jobInfo.EndTime = time.Now()
		manifestmutex.Unlock()
		manifestVol, err := saveManifest(ctx, jobInfo, true)
		if err != nil {
			return err
		}
		select {
		case stepCh <- manifestVol:
		case <-ctx.Done():
			return ctx.Err()
		}
		close(stepCh)
		return nil
	})

	err := group.Wait() // Wait for ZFS Send to finish, Backends to finish, and Manifest files to be copied/uploaded
	if err != nil {
		return err
	}

	totalWrittenBytes := jobInfo.TotalBytesWritten()
	if config.JSONOutput {
		var doneOutput = struct {
			TotalZFSBytes    uint64
			TotalBackupBytes uint64
			ElapsedTime      time.Duration
			FilesUploaded    int
		}{jobInfo.ZFSStreamBytes, totalWrittenBytes, time.Since(jobInfo.StartTime), len(jobInfo.Volumes) + 1}
		if j, jerr := json.Marshal(doneOutput); jerr != nil {
			log.AppLogger.Errorf("could not output json due to error - %v", jerr)
		} else {
			fmt.Fprintf(config.Stdout, "%s", string(j))
		}
	} else {
		fmt.Fprintf(
			config.Stdout,
			"Done.\n\tTotal ZFS Stream Bytes: %d (%s)\n\tTotal Bytes Written: %d (%s)\n\tElapsed Time: %v\n\tTotal Files Uploaded: %d\n",
			jobInfo.ZFSStreamBytes,
			humanize.IBytes(jobInfo.ZFSStreamBytes),
			totalWrittenBytes,
			humanize.IBytes(totalWrittenBytes),
			time.Since(jobInfo.StartTime),
			len(jobInfo.Volumes)+1,
		)
	}

	return nil
}

// refuseExistingSet fails when the manifest of the backup set about to be sent already exists at
// any destination. Volume boundaries are not reproducible, so a re-send would overwrite the set's
// volumes in place and leave its manifest pointing at changed objects. A set whose manifest is at
// only some destinations (its upload failed at the others) is completed instead under --resume, by
// copying that manifest to the rest; done reports that nothing is left to send.
func refuseExistingSet(ctx context.Context, jobInfo *files.JobInfo, destinations []destination) (done bool, err error) {
	name := jobInfo.ManifestObjectName()
	var have, missing []int
	for idx, d := range destinations {
		existing, lerr := d.backend.List(ctx, name)
		if lerr != nil {
			log.AppLogger.Errorf("Could not list %s at %s due to error - %v", name, d, lerr)
			return false, lerr
		}
		found := false
		for _, obj := range existing {
			found = found || obj == name
		}
		if found {
			have = append(have, idx)
		} else {
			missing = append(missing, idx)
		}
	}

	switch {
	case len(have) == 0:
		return false, nil
	case len(missing) == 0:
		err = fmt.Errorf(
			"backup set %s already exists at %s; refusing to overwrite it. Delete it at the destination to send it again",
			name, destinations[have[0]],
		)
	case !jobInfo.Resume:
		err = fmt.Errorf(
			"backup set %s already exists at %s but not at %s; refusing to overwrite it. Run again with --resume to copy its manifest to %s",
			name, destinations[have[0]], destinations[missing[0]], destinations[missing[0]],
		)
	default:
		return true, copyManifest(ctx, jobInfo, destinations, have[0], missing)
	}
	log.AppLogger.Errorf("%v.", err)
	return false, err
}

// copyManifest uploads the manifest of jobInfo's backup set from destinations[from] to each of
// destinations[missing], once every volume it lists is present there. A manifest is only uploaded
// after its volumes have passed every destination, so present volumes are complete ones.
func copyManifest(ctx context.Context, jobInfo *files.JobInfo, destinations []destination, from int, missing []int) error {
	name := jobInfo.ManifestObjectName()
	tmp, err := ioutil.TempFile(config.BackupTempdir, config.ProgramName)
	if err != nil {
		return err
	}
	tmp.Close()
	defer os.Remove(tmp.Name())
	if err = downloadTo(ctx, destinations[from].backend, name, tmp.Name()); err != nil {
		return err
	}
	manifest, err := readManifest(ctx, tmp.Name(), jobInfo)
	if err != nil {
		log.AppLogger.Errorf("Could not read manifest %s from %s due to error - %v", name, destinations[from], err)
		return err
	}

	prefix := jobInfo.BackupVolumeObjectPrefix()
	for _, idx := range missing {
		listed, lerr := destinations[idx].backend.List(ctx, prefix)
		if lerr != nil {
			log.AppLogger.Errorf("Could not list the volumes at %s due to error - %v", destinations[idx], lerr)
			return lerr
		}
		present := make(map[string]bool, len(listed))
		for _, obj := range listed {
			present[obj] = true
		}
		for _, vol := range manifest.Volumes {
			if !present[vol.ObjectName] {
				err = fmt.Errorf(
					"cannot complete backup set %s at %s: its volume %s is missing there. Delete the set at %s to send it again",
					name, destinations[idx], vol.ObjectName, destinations[from],
				)
				log.AppLogger.Errorf("%v.", err)
				return err
			}
		}
	}

	vol, err := files.LoadVolume(ctx, tmp.Name(), name, true)
	if err != nil {
		return err
	}
	defer func() {
		if derr := vol.DeleteVolume(); derr != nil {
			log.AppLogger.Warningf("Error deleting temporary manifest file  - %v", derr)
		}
	}()
	for _, idx := range missing {
		dest := destinations[idx]
		if err = volUploadWrapper(ctx, dest.backend, vol, strings.Split(dest.uri, "://")[0])(); err != nil {
			log.AppLogger.Errorf("Could not upload manifest %s to %s due to error - %v", name, dest, err)
			return err
		}
		log.AppLogger.Noticef("Completed backup set %s at %s with the manifest from %s.", name, dest, destinations[from])
	}
	return nil
}

func saveManifest(ctx context.Context, j *files.JobInfo, final bool) (*files.VolumeInfo, error) {
	manifestmutex.Lock()
	defer manifestmutex.Unlock()
	sort.Sort(files.ByVolumeNumber(j.Volumes))

	// Setup Manifest File
	manifest, err := files.CreateManifestVolume(ctx, j)
	if err != nil {
		log.AppLogger.Errorf("Error trying to create manifest volume - %v", err)
		return nil, err
	}
	// nolint:gosec // MD5 not used for cryptographic purposes here
	safeManifestFile := fmt.Sprintf("%x", md5.Sum([]byte(manifest.ObjectName)))
	manifest.IsFinalManifest = final
	jsonEnc := json.NewEncoder(manifest)
	err = jsonEnc.Encode(j)
	if err != nil {
		log.AppLogger.Errorf("Could not JSON Encode job information due to error - %v", err)
		return nil, err
	}
	if err = manifest.Close(); err != nil {
		log.AppLogger.Errorf("Could not close manifest volume due to error - %v", err)
		return nil, err
	}
	for _, destination := range j.Destinations {
		if destination == backends.DeleteBackendPrefix+"://" {
			continue
		}
		// nolint:gosec // MD5 not used for cryptographic purposes here
		safeFolder := fmt.Sprintf("%x", md5.Sum([]byte(destination)))
		dest := filepath.Join(config.WorkingDir, "cache", safeFolder, safeManifestFile)
		if err = manifest.CopyTo(dest); err != nil {
			log.AppLogger.Warningf("Could not write manifest volume due to error - %v", err)
			return nil, err
		}
		log.AppLogger.Debugf("Copied manifest to local cache for destination %s.", backends.RedactURI(destination))
	}
	return manifest, nil
}

// nolint:funlen,gocyclo // Difficult to break this apart
func sendStream(pctx context.Context, j *files.JobInfo, c chan<- *files.VolumeInfo, buffer <-chan bool) error {
	group, ctx := errgroup.WithContext(pctx)

	buf := bytes.NewBuffer(nil)
	cmd := zfs.GetZFSSendCommand(ctx, j)
	cin, cout := io.Pipe()
	cmd.Stdout = cout
	cmd.Stderr = buf
	counter := datacounter.NewReaderCounter(cin)
	usingPipe := false
	if j.MaxFileBuffer == 0 {
		usingPipe = true
	}

	// With pipes, a volume's writer (below) and its uploader block on each other: once the
	// pipeline fails, abort the volume in flight so whichever side is still waiting gets an error.
	// Watch pctx: group's ctx is also cancelled by a successful Wait, while the last volume uploads.
	var current atomic.Pointer[files.VolumeInfo]
	if usingPipe {
		stop := context.AfterFunc(pctx, func() {
			if volume := current.Load(); volume != nil {
				volume.Abort(pctx.Err())
			}
		})
		defer stop()
	}

	group.Go(func() error {
		var lastTotalBytes uint64
		defer close(c)
		// If we stop reading early, make zfs send's output copy fail instead of blocking forever.
		defer cin.Close()
		var err error
		var volume *files.VolumeInfo
		skipBytes, volNum := j.TotalBytesStreamedAndVols()
		lastTotalBytes = skipBytes
		for {
			// Skip bytes if we are resuming
			if skipBytes > 0 {
				log.AppLogger.Debugf("Want to skip %d bytes.", skipBytes)
				written, serr := io.CopyN(ioutil.Discard, counter, int64(skipBytes))
				if serr == io.EOF {
					serr = fmt.Errorf(
						"zfs stream ended before the %d bytes recorded in the cached manifest were skipped; run without --resume",
						lastTotalBytes,
					)
				}
				if serr != nil {
					log.AppLogger.Errorf("Error while trying to read from the zfs stream to skip %d bytes - %v", skipBytes, serr)
					return serr
				}
				skipBytes -= uint64(written)
				log.AppLogger.Debugf("Skipped %d bytes of the ZFS send stream.", written)
				continue
			}

			// Setup next Volume
			if volume == nil || volume.Counter() >= (j.VolumeSize*humanize.MiByte)-50*humanize.KiByte {
				if volume != nil {
					log.AppLogger.Debugf("Finished creating volume %s", volume.ObjectName)
					volume.ZFSStreamBytes = counter.Count() - lastTotalBytes
					lastTotalBytes = counter.Count()
					if err = volume.Close(); err != nil {
						log.AppLogger.Errorf("Error while trying to close volume %s - %v", volume.ObjectName, err)
						return err
					}
					if !usingPipe {
						select {
						case c <- volume:
						case <-ctx.Done():
							return ctx.Err()
						}
					}
				}
				<-buffer
				volume, err = files.CreateBackupVolume(ctx, j, volNum)
				if err != nil {
					log.AppLogger.Errorf("Error while creating volume %d - %v", volNum, err)
					return err
				}
				log.AppLogger.Debugf("Starting volume %s", volume.ObjectName)
				volNum++
				if usingPipe {
					current.Store(volume)
					if pctx.Err() != nil { // the AfterFunc may have run before the Store
						volume.Abort(pctx.Err())
					}
					select {
					case c <- volume:
					case <-ctx.Done():
						return ctx.Err()
					}
				}
			}

			// Write a little at a time and break the output between volumes as needed
			_, ierr := io.CopyN(volume, counter, files.BufferSize*2)
			if ierr == io.EOF {
				// We are done!
				log.AppLogger.Debugf("Finished creating volume %s", volume.ObjectName)
				volume.ZFSStreamBytes = counter.Count() - lastTotalBytes
				if err = volume.Close(); err != nil {
					log.AppLogger.Errorf("Error while trying to close volume %s - %v", volume.ObjectName, err)
					return err
				}
				if !usingPipe {
					select {
					case c <- volume:
					case <-ctx.Done():
						return ctx.Err()
					}
				}
				return nil
			} else if ierr != nil {
				log.AppLogger.Errorf("Error while trying to read from the zfs stream for volume %s - %v", volume.ObjectName, ierr)
				return ierr
			}
		}
	})

	// Start the zfs send command
	log.AppLogger.Infof("Starting zfs send command: %s", strings.Join(cmd.Args, " "))
	err := cmd.Start()
	if err != nil {
		log.AppLogger.Errorf("Error starting zfs command - %v", err)
		return err
	}

	group.Go(func() error {
		// A zfs send that dies mid-stream must reach the splitter as a read error, not as an
		// io.EOF it would take for a clean finish (and ship a truncated last volume).
		werr := cmd.Wait()
		if werr != nil {
			werr = fmt.Errorf("zfs send failed: %w: %s", werr, strings.TrimSpace(buf.String()))
		}
		cout.CloseWithError(werr)
		return werr
	})

	defer func() {
		if cmd.ProcessState == nil || !cmd.ProcessState.Exited() {
			err = cmd.Process.Kill()
			if err != nil {
				log.AppLogger.Errorf("Could not kill zfs send command due to error - %v", err)
				return
			}
			err = cmd.Process.Release()
			if err != nil {
				log.AppLogger.Errorf("Could not release resources from zfs send command due to error - %v", err)
				return
			}
		}
	}()

	manifestmutex.Lock()
	j.ZFSCommandLine = strings.Join(cmd.Args, " ")
	manifestmutex.Unlock()
	// Wait for the command to finish

	err = group.Wait()
	if err != nil {
		log.AppLogger.Errorf("Error waiting for zfs command to finish - %v", err)
		// The deferred stop runs before the caller's group cancels pctx, so abort here too.
		if volume := current.Load(); volume != nil {
			volume.Abort(err)
		}
		return err
	}
	log.AppLogger.Infof("zfs send completed without error")
	manifestmutex.Lock()
	j.ZFSStreamBytes = counter.Count()
	manifestmutex.Unlock()
	return nil
}

// tryResume continues a previous attempt from its cached partial manifest. destinations are the
// prepared destinations of j.Destinations, in order: every volume it skips must still exist at each.
// nolint:funlen,gocyclo // Difficult to break this apart
func tryResume(ctx context.Context, j *files.JobInfo, destinations []destination) error {
	// Temproary Final Manifest File
	manifest, merr := files.CreateManifestVolume(ctx, j)
	if merr != nil {
		log.AppLogger.Errorf("Error trying to create manifest volume - %v", merr)
		return merr
	}
	defer func() {
		if err := manifest.Close(); err != nil {
			log.AppLogger.Warningf("Could not close the temporary manifest volume - %v", err)
		}
		if err := manifest.DeleteVolume(); err != nil {
			log.AppLogger.Warningf("Could not delete the temporary manifest volume - %v", err)
		}
	}()

	// nolint:gosec // MD5 not used for cryptographic purposes here
	safeManifestFile := fmt.Sprintf("%x", md5.Sum([]byte(manifest.ObjectName)))

	safeFolder := fmt.Sprintf("%x", md5.Sum([]byte(destinations[0].uri))) // nolint:gosec // MD5 not used for cryptographic purposes here
	origManiPath := filepath.Join(config.WorkingDir, "cache", safeFolder, safeManifestFile)

	switch originalManifest, oerr := readManifest(ctx, origManiPath, j); {
	case os.IsNotExist(oerr):
		log.AppLogger.Info("No previous manifest file exists, nothing to resume")
	case oerr != nil:
		log.AppLogger.Errorf("Could not open previous manifest file %s due to error: %v", origManiPath, oerr)
		return oerr
	default:
		if originalManifest.Compressor != j.Compressor {
			log.AppLogger.Errorf(
				"Cannot resume backup, original compressor %s != compressor specified %s",
				originalManifest.Compressor, j.Compressor,
			)
			return fmt.Errorf("option mismatch")
		}

		if originalManifest.EncryptTo != j.EncryptTo {
			log.AppLogger.Errorf(
				"Cannot resume backup, different encryptTo flags specified (original %v != current %v)",
				originalManifest.EncryptTo, j.EncryptTo,
			)
			return fmt.Errorf("option mismatch")
		}

		if originalManifest.SignFrom != j.SignFrom {
			log.AppLogger.Errorf(
				"Cannot resume backup, different signFrom flags specified (original %v != current %v)",
				originalManifest.SignFrom, j.SignFrom,
			)
			return fmt.Errorf("option mismatch")
		}

		currentCMD := zfs.GetZFSSendCommand(ctx, j)
		oldCMD := zfs.GetZFSSendCommand(ctx, originalManifest)
		oldCMDLine := strings.Join(oldCMD.Args, " ")
		currentCMDLine := strings.Join(currentCMD.Args, " ")
		if strings.Compare(oldCMDLine, currentCMDLine) != 0 {
			log.AppLogger.Errorf(
				"Cannot resume backup, different options given for zfs send command: original `%s` != current `%s`",
				oldCMDLine, currentCMDLine,
			)
			return fmt.Errorf("option mismatch")
		}

		// Manifests do not record their destinations, so a destination added since the first
		// attempt shows up here as one that is missing every volume: the resume starts over.
		volumes, err := verifiedVolumes(ctx, j, originalManifest.Volumes, destinations)
		if err != nil {
			return err
		}
		if len(volumes) == 0 {
			log.AppLogger.Noticef("Nothing verifiable to resume; starting over.")
			return nil
		}

		manifestmutex.Lock()
		j.Volumes = volumes
		j.StartTime = originalManifest.StartTime
		manifestmutex.Unlock()
		log.AppLogger.Infof("Will be resuming previous backup attempt.")
	}
	return nil
}

// verifiedVolumes returns the longest run of cached volumes, numbered contiguously from 1, that
// exist at every destination. Volumes past that point are sent again; their old copies at the
// destinations are overwritten by the new attempt's same-numbered objects or, being referenced
// by no final manifest, removed by clean once the manifest lands.
func verifiedVolumes(
	ctx context.Context, j *files.JobInfo, cached []*files.VolumeInfo, destinations []destination,
) ([]*files.VolumeInfo, error) {
	cached = append([]*files.VolumeInfo(nil), cached...)
	sort.Sort(files.ByVolumeNumber(cached))

	// "<volume>|<snap>[...].zstream[.ext].vol": every volume of this set, and nothing else.
	prefix := j.BackupVolumeObjectPrefix()
	keep := len(cached)
	for _, d := range destinations {
		listed, err := d.backend.List(ctx, prefix)
		if err != nil {
			log.AppLogger.Errorf("Could not list the volumes at %s due to error - %v", d, err)
			return nil, err
		}
		present := make(map[string]bool, len(listed))
		for _, name := range listed {
			present[name] = true
		}
		sizer, _ := d.backend.(backends.Sizer)
		for n := 0; n < keep; n++ {
			vol := cached[n]
			if vol.VolumeNumber != int64(n+1) || !present[vol.ObjectName] {
				log.AppLogger.Noticef(
					"Volume %s missing at %s; it and later volumes will be re-sent.", vol.ObjectName, d,
				)
				keep = n
				break
			}
			if sizer == nil {
				continue
			}
			// A send killed mid-upload leaves a truncated volume under its final name.
			size, err := sizer.Size(ctx, vol.ObjectName)
			if err != nil {
				log.AppLogger.Errorf("Could not stat %s at %s due to error - %v", vol.ObjectName, d, err)
				return nil, err
			}
			if size != vol.Size {
				log.AppLogger.Noticef(
					"Volume %s at %s is %d bytes, want %d; it and later volumes will be re-sent.",
					vol.ObjectName, d, size, vol.Size,
				)
				keep = n
				break
			}
		}
	}
	log.AppLogger.Noticef("Resuming from volume %d: %d of %d cached volumes verified at all destinations.", keep+1, keep, len(cached))
	return cached[:keep], nil
}

func retryUploadChainer(
	ctx context.Context,
	in <-chan *files.VolumeInfo,
	b backends.Backend,
	j *files.JobInfo,
	dest string,
) (<-chan *files.VolumeInfo, *errgroup.Group) {
	out := make(chan *files.VolumeInfo)
	parts := strings.Split(dest, "://")
	prefix := parts[0]
	var gwg *errgroup.Group
	if j.MaxParallelUploads > 1 {
		gwg, ctx = errgroup.WithContext(ctx)
	} else {
		gwg = new(errgroup.Group)
	}

	var wg sync.WaitGroup
	wg.Add(j.MaxParallelUploads)
	for i := 0; i < j.MaxParallelUploads; i++ {
		gwg.Go(func() error {
			defer wg.Done()
			for {
				// Waiting on in must also watch ctx: after a failure upstream, in is never closed.
				var vol *files.VolumeInfo
				select {
				case <-ctx.Done():
					return ctx.Err()
				case v, ok := <-in:
					if !ok {
						return nil
					}
					vol = v
				}
				log.AppLogger.Debugf("%s backend: Processing volume %s", prefix, vol.ObjectName)
				// Prepare the backoff retryer (forces the user configured retry options across all backends)
				be := backoff.NewExponentialBackOff()
				be.MaxInterval = j.MaxBackoffTime
				be.MaxElapsedTime = j.MaxRetryTime
				retryconf := backoff.WithContext(be, ctx)

				operation := volUploadWrapper(ctx, b, vol, prefix)
				if err := backoff.Retry(operation, retryconf); err != nil {
					log.AppLogger.Errorf("%s backend: Failed to upload volume %s due to error: %v", prefix, vol.ObjectName, err)
					return err
				}
				log.AppLogger.Debugf("%s backend: Processed volume %s", prefix, vol.ObjectName)
				select {
				case out <- vol:
				case <-ctx.Done():
					return ctx.Err()
				}
			}
		})
	}

	gwg.Go(func() error {
		wg.Wait()
		log.AppLogger.Debugf("%s backend: closing out channel.", prefix)
		close(out)
		return nil
	})

	return out, gwg
}

func volUploadWrapper(ctx context.Context, b backends.Backend, vol *files.VolumeInfo, prefix string) func() error {
	return func() error {
		if err := vol.OpenVolume(); err != nil {
			log.AppLogger.Debugf("%s: Error while opening volume %s - %v", prefix, vol.ObjectName, err)
			return err
		}

		err := b.Upload(ctx, vol)
		if err != nil {
			log.AppLogger.Debugf("%s: Error while uploading volume %s - %v", prefix, vol.ObjectName, err)
			if vol.IsUsingPipe() {
				// The splitter may still be writing this volume: fail its writes rather than Close,
				// which would flush into a pipe nobody reads. A retry would read on from wherever
				// this attempt stopped consuming the pipe, so there is none.
				vol.Abort(err)
				return backoff.Permanent(err)
			}
		}
		vol.Close()
		return err
	}
}
