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
	"crypto/sha256"
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

// Test hooks, nil outside tests. They widen race windows in sendStream that are otherwise
// too narrow to hit reliably.
var (
	// TestHookStreamFailed runs when sendStream is about to return an error.
	TestHookStreamFailed func()
	// TestHookBeforeStreamBytes runs when sendStream is about to record the stream's length.
	TestHookBeforeStreamBytes func()
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

	completable := jobInfo.Resume
	if partial := partialSet(destBackups); partial != nil && !completable {
		if completable, err = partialVolumesPresent(ctx, jobInfo, partial, destBackups); err != nil {
			return err
		}
	}
	return selectSmartSnapshots(jobInfo, snapshots, destBackups, completable)
}

// partialVolumesPresent reports whether every destination that lacks the backup set partial has
// an object named like each of its volumes: the trace of a send whose manifest upload failed
// there, rather than of destinations that diverged. Backup checks the volumes' contents.
func partialVolumesPresent(ctx context.Context, jobInfo *files.JobInfo, partial *files.JobInfo, destBackups [][]*files.JobInfo) (bool, error) {
	if len(partial.Volumes) == 0 {
		return false, nil
	}
	prefix := partial.Volumes[0].ObjectName
	for _, vol := range partial.Volumes[1:] {
		for !strings.HasPrefix(vol.ObjectName, prefix) {
			prefix = prefix[:len(prefix)-1]
		}
	}
	for idx, backups := range destBackups {
		if len(backups) > 0 && sameSets(backups[:1], []*files.JobInfo{partial}) {
			continue
		}
		backend, err := prepareBackend(ctx, jobInfo, jobInfo.Destinations[idx], nil)
		if err != nil {
			return false, err
		}
		listed, err := backend.List(ctx, prefix)
		if cerr := backend.Close(); cerr != nil {
			log.AppLogger.Warningf("Could not properly close backend due to error - %v", cerr)
		}
		if err != nil {
			return false, err
		}
		present := make(map[string]bool, len(listed))
		for _, name := range listed {
			present[name] = true
		}
		for _, vol := range partial.Volumes {
			if !present[vol.ObjectName] {
				log.AppLogger.Infof(
					"Destination #%d lacks %s, a volume of the set it is behind on; not completing that set there.",
					idx+1, vol.ObjectName,
				)
				return false, nil
			}
		}
	}
	return true, nil
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
func selectSmartSnapshots(
	jobInfo *files.JobInfo, snapshots []files.SnapshotInfo, destBackups [][]*files.JobInfo, completable bool,
) error {
	p, err := planSmartSnapshots(jobInfo, snapshots, destBackups, completable)
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
	jobInfo.CompletePartial = p.Reason == reasonCompletePartial
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
	localCachePath, cerr := getCacheDir(jobInfo, target)
	if cerr != nil {
		log.AppLogger.Errorf("Could not get cache dir for target %s due to error - %v.", backends.RedactURI(target), cerr)
		return nil, cerr
	}

	// Sync the local cache
	manifests, _, serr := syncCache(ctx, jobInfo, localCachePath, backend)
	if serr != nil {
		log.AppLogger.Errorf("Could not sync cache dir for target %s due to error - %v.", backends.RedactURI(target), serr)
		return nil, serr
	}

	// Read in Manifests and display
	decodedManifests := make([]*files.JobInfo, 0, len(manifests))
	for _, manifest := range manifests {
		decodedManifest, oerr := readCachedManifest(ctx, jobInfo, localCachePath, backend, manifest)
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
// pendingVolumes counts the volumes still in the pipeline. zero is closed when the count first
// reaches 0. Unlike a sync.WaitGroup it can be waited on in a select, so nothing stays parked
// on it when the pipeline fails and the count never gets there.
type pendingVolumes struct {
	mu   sync.Mutex
	n    int
	zero chan struct{}
	once sync.Once
}

func newPendingVolumes(n int) *pendingVolumes {
	return &pendingVolumes{n: n, zero: make(chan struct{})}
}

func (p *pendingVolumes) add() {
	p.mu.Lock()
	p.n++
	p.mu.Unlock()
}

func (p *pendingVolumes) done() {
	p.mu.Lock()
	p.n--
	n := p.n
	p.mu.Unlock()
	if n == 0 {
		p.once.Do(func() { close(p.zero) })
	}
}

// volumeLock is the lock a send holds while it works on volume. clean takes it too, so that it
// never judges the volumes of a set that is still being uploaded. It lives in the working
// directory, beside the cache it protects: unlike os.TempDir(), that does not change with TMPDIR
// (systemd's PrivateTmp, cron vs a shell). So a send and a clean only see each other on the same
// host, with the same --workingDirectory.
func volumeLock(volume string) (lock lockfile.Lockfile, path string, err error) {
	dir, err := filepath.Abs(filepath.Join(config.WorkingDir, "locks"))
	if err != nil {
		return "", "", err
	}
	if err = os.MkdirAll(dir, 0700); err != nil {
		return "", "", err
	}
	// nolint:gosec // MD5 not used for cryptographic purposes
	path = filepath.Join(dir, fmt.Sprintf("%x.lck", md5.Sum([]byte(volume))))
	lock, err = lockfile.New(path)
	return lock, path, err
}

// legacyVolumeLockPath is where versions before the lock moved to the working directory locked
// volume. A send of such a version may still be running while this one is installed.
func legacyVolumeLockPath(volume string) string {
	// nolint:gosec // MD5 not used for cryptographic purposes
	return filepath.Join(os.TempDir(), fmt.Sprintf("zfsbackup.%x.lck", md5.Sum([]byte(volume))))
}

// legacyLockHolder reports the live process that holds volume's legacy lock, if any.
// TODO: drop it one release after the lock moved to the working directory.
func legacyLockHolder(volume string) (*os.Process, bool) {
	lock, err := lockfile.New(legacyVolumeLockPath(volume))
	if err != nil {
		return nil, false
	}
	p, err := lock.GetOwner()
	if err != nil || p.Pid == os.Getpid() {
		return nil, false
	}
	return p, true
}

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
		if _, cerr := getCacheDir(jobInfo, uri); cerr != nil {
			log.AppLogger.Errorf("Could not create cache for destination %s due to error - %v.", backends.RedactURI(uri), cerr)
			return cerr
		}
	}

	// Make sure nobody else is working on the same volume/dataset we are!
	lock, lockFilePath, lferr := volumeLock(jobInfo.VolumeName)
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
	if p, held := legacyLockHolder(jobInfo.VolumeName); held {
		err := fmt.Errorf(
			"an older version of %s (pid %d) is sending %s; its lock is %s",
			config.ProgramName, p.Pid, jobInfo.VolumeName, legacyVolumeLockPath(jobInfo.VolumeName),
		)
		log.AppLogger.Errorf("%v.", err)
		return err
	}

	// What the manifest records about the stream's identity, for a later --resume to compare.
	jobInfo.RecordKeyFingerprints()
	recordSnapshotGUIDs(ctx, jobInfo)

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

	pending := newPendingVolumes(1)

	fileBuffer := make(chan bool, fileBufferSize)
	for i := 0; i < fileBufferSize; i++ {
		fileBuffer <- true
	}

	var group *errgroup.Group
	group, ctx = errgroup.WithContext(ctx)

	// Used to prevent closing the upload pipeline after the ZFS command is done
	// so we can send the manifest file up after all volumes have made it to the backends.
	go func() {
		defer pending.done()
		for {
			select {
			case vol, ok := <-startCh:
				if !ok {
					return
				}
				pending.add()
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
					pending.done()
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
		// unless the pipeline fails first: then no manifest may be written, and the count may never reach zero.
		select {
		case <-pending.zero:
		case <-ctx.Done():
			return ctx.Err()
		}
		// The forwarder also counts down when it stops on a cancelled ctx, so both cases can be
		// ready at once; a failed pipeline must never get as far as a final manifest.
		if err := ctx.Err(); err != nil {
			return err
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

// recordSnapshotGUIDs records the guids of j's snapshots. A snapshot that is gone gets none: a
// set can still be completed by copying its manifest, while a send fails on it later anyway.
func recordSnapshotGUIDs(ctx context.Context, j *files.JobInfo) {
	snaps := []*files.SnapshotInfo{&j.BaseSnapshot}
	if j.IncrementalSnapshot.Name != "" {
		snaps = append(snaps, &j.IncrementalSnapshot)
	}
	for _, snap := range snaps {
		guid, err := zfs.GetSnapshotGUID(ctx, j.VolumeName, snap)
		if err != nil {
			log.AppLogger.Debugf("Could not get the guid of %s: %v", snap.Name, err)
			guid = ""
		}
		snap.GUID = guid
	}
}

// sameStream fails, with the reason logged, unless a send of current would produce the stream
// that original (a manifest written by an earlier attempt or another destination) describes:
// the same zfs send arguments, snapshots (by guid), compressor and keys.
func sameStream(ctx context.Context, original, current *files.JobInfo) error {
	mismatch := func(what string, was, is interface{}) error {
		log.AppLogger.Errorf("Cannot resume backup: %s differs (original %v, current %v)", what, was, is)
		return fmt.Errorf("option mismatch: %s differs (original %v, current %v)", what, was, is)
	}
	if original.Compressor != current.Compressor {
		return mismatch("compressor", original.Compressor, current.Compressor)
	}
	if original.EncryptTo != current.EncryptTo {
		return mismatch("encryptTo", original.EncryptTo, current.EncryptTo)
	}
	if original.SignFrom != current.SignFrom {
		return mismatch("signFrom", original.SignFrom, current.SignFrom)
	}
	if original.EncryptKeyFingerprint != current.EncryptKeyFingerprint {
		return mismatch("encryption key", original.EncryptKeyFingerprint, current.EncryptKeyFingerprint)
	}
	if original.SignKeyFingerprint != current.SignKeyFingerprint {
		return mismatch("signing key", original.SignKeyFingerprint, current.SignKeyFingerprint)
	}
	if original.BaseSnapshot.GUID != current.BaseSnapshot.GUID {
		return mismatch("guid of "+current.BaseSnapshot.Name, original.BaseSnapshot.GUID, current.BaseSnapshot.GUID)
	}
	if original.IncrementalSnapshot.GUID != current.IncrementalSnapshot.GUID {
		return mismatch("guid of "+current.IncrementalSnapshot.Name, original.IncrementalSnapshot.GUID, current.IncrementalSnapshot.GUID)
	}
	oldCMDLine := strings.Join(zfs.GetZFSSendCommand(ctx, original).Args, " ")
	currentCMDLine := strings.Join(zfs.GetZFSSendCommand(ctx, current).Args, " ")
	if oldCMDLine != currentCMDLine {
		return mismatch("zfs send command", "`"+oldCMDLine+"`", "`"+currentCMDLine+"`")
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
	case !jobInfo.Resume && !jobInfo.CompletePartial:
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
// destinations[missing], once it has checked that the manifest describes what this send would send,
// and that every volume it lists is at each of those destinations with the size and SHA-256 it
// records: same-named volumes may be left over from another attempt (another --volsize, another
// stream).
// nolint:funlen // Difficult to break this apart
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
	if err = sameStream(ctx, manifest, comparableJob(jobInfo, manifest)); err != nil {
		return err
	}

	for _, idx := range missing {
		if err = verifyVolumesAt(ctx, jobInfo, manifest, destinations[idx], destinations[from]); err != nil {
			return err
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
	// nolint:gosec // MD5 not used for cryptographic purposes here
	safeManifestFile := fmt.Sprintf("%x", md5.Sum([]byte(name)))
	for _, idx := range missing {
		dest := destinations[idx]
		if err = volUploadWrapper(ctx, dest.backend, vol, strings.Split(dest.uri, "://")[0])(); err != nil {
			log.AppLogger.Errorf("Could not upload manifest %s to %s due to error - %v", name, dest, err)
			return err
		}
		// Whatever an earlier attempt cached under this name is not what dest now has.
		if err = cacheFile(cacheDirFor(dest.uri), safeManifestFile, tmp.Name()); err != nil {
			log.AppLogger.Errorf("Could not cache manifest %s for %s due to error - %v", name, dest, err)
			return err
		}
		log.AppLogger.Noticef("Completed backup set %s at %s with the manifest from %s.", name, dest, destinations[from])
	}
	return nil
}

// comparableJob is current, as sameStream should compare it with manifest: what manifest does not
// record (written by an older version), and the guid of a snapshot that is gone locally (nothing
// is sent from it), are not compared.
func comparableJob(current, manifest *files.JobInfo) *files.JobInfo {
	c := *current
	if manifest.EncryptKeyFingerprint == "" {
		c.EncryptKeyFingerprint = ""
	}
	if manifest.SignKeyFingerprint == "" {
		c.SignKeyFingerprint = ""
	}
	if manifest.BaseSnapshot.GUID == "" || c.BaseSnapshot.GUID == "" {
		c.BaseSnapshot.GUID = manifest.BaseSnapshot.GUID
	}
	if manifest.IncrementalSnapshot.GUID == "" || c.IncrementalSnapshot.GUID == "" {
		c.IncrementalSnapshot.GUID = manifest.IncrementalSnapshot.GUID
	}
	return &c
}

// verifyVolumesAt fails unless every volume manifest lists is at d with the size and SHA-256 the
// manifest records. from is where the complete set is, for the advice in the error.
func verifyVolumesAt(ctx context.Context, jobInfo, manifest *files.JobInfo, d, from destination) error {
	name := jobInfo.ManifestObjectName()
	prefix := jobInfo.BackupVolumeObjectPrefix()
	listed, err := d.backend.List(ctx, prefix)
	if err != nil {
		log.AppLogger.Errorf("Could not list the volumes at %s due to error - %v", d, err)
		return err
	}
	present := make(map[string]bool, len(listed))
	for _, obj := range listed {
		present[obj] = true
	}
	for _, vol := range manifest.Volumes {
		if !present[vol.ObjectName] {
			err = fmt.Errorf(
				"cannot complete backup set %s at %s: its volume %s is missing there. Delete the set at %s to send it again",
				name, d, vol.ObjectName, from,
			)
			log.AppLogger.Errorf("%v.", err)
			return err
		}
	}
	sizer, _ := d.backend.(backends.Sizer)
	for _, vol := range manifest.Volumes {
		var size uint64
		var sum string
		if sizer != nil {
			if size, err = sizer.Size(ctx, vol.ObjectName); err != nil {
				log.AppLogger.Errorf("Could not stat %s at %s due to error - %v", vol.ObjectName, d, err)
				return err
			}
		}
		if size == vol.Size || sizer == nil {
			if size, sum, err = hashObject(ctx, d.backend, vol.ObjectName); err != nil {
				log.AppLogger.Errorf("Could not read %s at %s due to error - %v", vol.ObjectName, d, err)
				return err
			}
		}
		if size != vol.Size || sum != vol.SHA256Sum {
			if sum == "" {
				sum = "not read"
			}
			err = fmt.Errorf(
				"cannot complete backup set %s at %s: its volume %s there is not the one the manifest at %s describes "+
					"(%d bytes, SHA256 %s; want %d bytes, SHA256 %s), probably left over from another attempt. "+
					"Delete the objects %s* at %s, then send this set to %s alone",
				name, d, vol.ObjectName, from, size, sum, vol.Size, vol.SHA256Sum, prefix, d, d,
			)
			log.AppLogger.Errorf("%v.", err)
			return err
		}
	}
	log.AppLogger.Infof("Verified the %d volumes of %s at %s.", len(manifest.Volumes), name, d)
	return nil
}

// hashObject downloads objectName from backend and returns its size and SHA-256.
func hashObject(ctx context.Context, backend backends.Backend, objectName string) (uint64, string, error) {
	r, err := backend.Download(ctx, objectName)
	if err != nil {
		return 0, "", err
	}
	defer r.Close()
	h := sha256.New()
	n, err := io.Copy(h, r)
	if err != nil {
		return 0, "", err
	}
	return uint64(n), fmt.Sprintf("%x", h.Sum(nil)), nil
}

// cacheFile copies the file at path to dir/name, atomically.
func cacheFile(dir, name, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return files.WriteFileAtomic(dir, name, f)
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
		dest := filepath.Join(cacheDirFor(destination), safeManifestFile)
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
	// Everything read from the stream, skipped or not, is hashed: each volume records the hash so
	// far, and a resume checks the bytes it skips against the last volume it keeps.
	streamHash := sha256.New()
	counter := datacounter.NewReaderCounter(io.TeeReader(cin, streamHash))
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
		// c is closed by sendStream, once the whole stream is known good: closing it here, on a
		// failure, would let the caller finalize the volumes sent so far as a complete set.
		// If we stop reading early, make zfs send's output copy fail instead of blocking forever.
		defer cin.Close()
		var err error
		var volume *files.VolumeInfo
		manifestmutex.Lock()
		skipBytes, volNum := j.TotalBytesStreamedAndVols()
		var keptStreamSHA256 string
		if len(j.Volumes) > 0 {
			keptStreamSHA256 = j.Volumes[len(j.Volumes)-1].StreamSHA256
		}
		manifestmutex.Unlock()
		lastTotalBytes = skipBytes
		for {
			// Skip bytes if we are resuming, and check they are the bytes the kept volumes hold
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
				log.AppLogger.Debugf("Skipped %d bytes of the ZFS send stream.", written)
				skipBytes = 0
				if got := fmt.Sprintf("%x", streamHash.Sum(nil)); got != keptStreamSHA256 {
					err = fmt.Errorf("zfs stream differs from the interrupted attempt's; run without --resume")
					log.AppLogger.Errorf("%v (SHA256 of the first %d bytes: %s, recorded %s).", err, written, got, keptStreamSHA256)
					return err
				}
				continue
			}

			// Setup next Volume
			if volume == nil || volume.Counter() >= (j.VolumeSize*humanize.MiByte)-50*humanize.KiByte {
				if volume != nil {
					log.AppLogger.Debugf("Finished creating volume %s", volume.ObjectName)
					volume.ZFSStreamBytes = counter.Count() - lastTotalBytes
					volume.StreamSHA256 = fmt.Sprintf("%x", streamHash.Sum(nil))
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
				volume.StreamSHA256 = fmt.Sprintf("%x", streamHash.Sum(nil))
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
		if TestHookStreamFailed != nil {
			TestHookStreamFailed()
		}
		return err
	}
	log.AppLogger.Infof("zfs send completed without error")
	if TestHookBeforeStreamBytes != nil {
		TestHookBeforeStreamBytes()
	}
	manifestmutex.Lock()
	j.ZFSStreamBytes = counter.Count()
	manifestmutex.Unlock()
	close(c)
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

	origManiPath := filepath.Join(cacheDirFor(destinations[0].uri), safeManifestFile)

	switch originalManifest, oerr := readManifest(ctx, origManiPath, j); {
	case os.IsNotExist(oerr):
		log.AppLogger.Info("No previous manifest file exists, nothing to resume")
	case oerr != nil:
		// Cut short by a kill or a full disk: there is nothing to resume from.
		log.AppLogger.Warningf("Could not read previous manifest file %s (%v); starting over.", origManiPath, oerr)
	default:
		if j.BaseSnapshot.GUID == "" {
			return fmt.Errorf("cannot resume: could not read the guid of %s@%s", j.VolumeName, j.BaseSnapshot.Name)
		}
		if err := sameStream(ctx, originalManifest, j); err != nil {
			return err
		}
		for _, vol := range originalManifest.Volumes {
			if vol.StreamSHA256 == "" {
				err := fmt.Errorf(
					"cannot resume: the interrupted attempt was made by an older version, which recorded nothing to " +
						"check the stream against; start over without --resume",
				)
				log.AppLogger.Errorf("%v.", err)
				return err
			}
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
// by no final manifest, removed by clean once the manifest lands. clean skips this dataset while
// a send on this host holds its lock.
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
				// The manifest makes the set final: never upload it for a pipeline that has failed.
				if vol.IsManifest && ctx.Err() != nil {
					return ctx.Err()
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
