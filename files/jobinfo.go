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

package files

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	humanize "github.com/dustin/go-humanize"
	"golang.org/x/crypto/openpgp"

	"github.com/someone1/zfsbackup-go/log"
)

// disallowedSeps matches a separator that is built only from characters ZFS
// permits in a dataset path. Object names are formed by joining the volume name
// and the snapshot names with the separator, so such a separator makes the
// result ambiguous. With an empty separator, an incremental "a" -> "b" of
// tank/data and a full backup of tank/data@atob both give "tank/dataatob"; the
// second backup then silently overwrites the first at the destination. The
// empty string matches because * permits zero characters.
//
// A separator is safe when at least one of its characters cannot occur in a ZFS
// dataset path, because no snapshot name can then absorb it.
var disallowedSeps = regexp.MustCompile(`^[\w\-:./]*$`)

// JobInfo represents the relevant information for a job that can be used to read
// in details of that job at a later time.
type JobInfo struct {
	StartTime           time.Time
	EndTime             time.Time
	VolumeName          string
	BaseSnapshot        SnapshotInfo
	IncrementalSnapshot SnapshotInfo
	SnapshotPrefix      string
	Raw                 bool
	Compressor          string
	CompressionLevel    int
	Separator           string
	ZFSCommandLine      string
	ZFSStreamBytes      uint64
	Volumes             []*VolumeInfo
	Version             float64
	EncryptTo           string
	SignFrom            string
	// The keys EncryptTo and SignFrom resolved to: an address can name another key later.
	EncryptKeyFingerprint   string `json:",omitempty"`
	SignKeyFingerprint      string `json:",omitempty"`
	Replication             bool
	SkipMissing             bool
	Deduplication           bool
	Properties              bool
	IntermediaryIncremental bool
	Resume                  bool `json:"-"`
	// CompletePartial is set when smart options planned a set that some destinations already
	// have: Backup copies its manifest to the rest instead of sending it (as --resume would).
	CompletePartial bool `json:"-"`
	// "Smart" Options
	Full            bool          `json:"-"`
	Incremental     bool          `json:"-"`
	FullIfOlderThan time.Duration `json:"-"`
	// When set, full backups are anchored on the newest snapshot whose name ends
	// with FullSnapshotSuffix (e.g. "_monthly") and incremental backups target the
	// newest snapshot whose name ends with IncrementalSnapshotSuffix (e.g. "_daily").
	// This lets fullIfOlderThan chain off snapshots that survive long enough to
	// outlive the full window, instead of ephemeral snapshots that get pruned.
	FullSnapshotSuffix        string `json:"-"`
	IncrementalSnapshotSuffix string `json:"-"`

	// ZFS Receive options
	Force       bool   `json:"-"`
	FullPath    bool   `json:"-"`
	LastPath    bool   `json:"-"`
	NotMounted  bool   `json:"-"`
	Origin      string `json:"-"`
	LocalVolume string `json:"-"`
	AutoRestore bool   `json:"-"`
	// TrustedCompressor is receive's --compressor: a decompressor a manifest may name although
	// it is not one of the known ones.
	TrustedCompressor string `json:"-"`

	Destinations []string `json:"-"`
	// DestinationsAsTyped maps each of Destinations (canonical URIs) to the spelling given on
	// the command line. Older versions keyed the local cache and the object key prefix by it.
	DestinationsAsTyped map[string]string `json:"-"`
	VolumeSize          uint64            `json:"-"`
	ManifestPrefix      string            `json:"-"`
	MaxBackoffTime      time.Duration     `json:"-"`
	MaxRetryTime        time.Duration     `json:"-"`
	MaxParallelUploads  int               `json:"-"`
	MaxFileBuffer       int               `json:"-"`
	EncryptKey          *openpgp.Entity   `json:"-"`
	SignKey             *openpgp.Entity   `json:"-"`
	ParentSnap          *JobInfo          `json:"-"`
	UploadChunkSize     int               `json:"-"`
}

// SnapshotInfo represents a snapshot with relevant information.
type SnapshotInfo struct {
	CreationTime time.Time
	Name         string
	Bookmark     bool
	// GUID is zfs's guid property: unlike the name, it changes when a snapshot is recreated.
	// Only recorded in the manifests of sends.
	GUID string `json:",omitempty"`
}

// Equal will test two SnapshotInfo objects for equality. This is based on the snapshot name and the time of creation
func (s *SnapshotInfo) Equal(t *SnapshotInfo) bool {
	if s == nil || t == nil {
		return s == t
	}
	return strings.Compare(s.Name, t.Name) == 0 && s.CreationTime.Equal(t.CreationTime)
}

// RecordKeyFingerprints records the fingerprints of EncryptKey and SignKey, so that a manifest
// names the keys it was written with, not just their addresses.
func (j *JobInfo) RecordKeyFingerprints() {
	j.EncryptKeyFingerprint, j.SignKeyFingerprint = "", ""
	if j.EncryptKey != nil {
		j.EncryptKeyFingerprint = fmt.Sprintf("%X", j.EncryptKey.PrimaryKey.Fingerprint)
	}
	if j.SignKey != nil {
		j.SignKeyFingerprint = fmt.Sprintf("%X", j.SignKey.PrimaryKey.Fingerprint)
	}
}

// TotalBytesWritten will sum up the size of all underlying Volumes to give a total
// that represents how many bytes have been written.
func (j *JobInfo) TotalBytesWritten() uint64 {
	var total uint64

	for _, vol := range j.Volumes {
		total += vol.Size
	}

	return total
}

// String will return a string representation of this JobInfo.
func (j *JobInfo) String() string {
	var output []string
	output = append(
		output,
		fmt.Sprintf("Volume: %s", j.VolumeName),
		fmt.Sprintf("Snapshot: %s (%v)", j.BaseSnapshot.Name, j.BaseSnapshot.CreationTime),
	)
	if j.IncrementalSnapshot.Name != "" {
		output = append(
			output,
			fmt.Sprintf("Incremental From Snapshot: %s (%v)", j.IncrementalSnapshot.Name, j.IncrementalSnapshot.CreationTime),
			fmt.Sprintf("Intermediary: %v", j.IntermediaryIncremental),
		)
	}

	totalWrittenBytes := j.TotalBytesWritten()

	output = append(
		output,
		fmt.Sprintf("Replication: %v", j.Replication),
		fmt.Sprintf("SkipMissing: %v", j.SkipMissing),
		fmt.Sprintf("Raw: %v", j.Raw),
		fmt.Sprintf("Archives: %d - %d bytes (%s)", len(j.Volumes), totalWrittenBytes, humanize.IBytes(totalWrittenBytes)),
		fmt.Sprintf("Volume Size (Raw): %d bytes (%s)", j.ZFSStreamBytes, humanize.IBytes(j.ZFSStreamBytes)),
		fmt.Sprintf("Uploaded: %v (took %v)\n\n", j.StartTime, j.EndTime.Sub(j.StartTime)),
	)
	return strings.Join(output, "\n\t")
}

// TotalBytesStreamedAndVols will sum up the streamed bytes of all underlying Volumes to give a total
// that represents how many bytes have been streamed. It will stop at any out of order volume number.
func (j *JobInfo) TotalBytesStreamedAndVols() (total uint64, volnum int64) {
	volnum = 1
	if len(j.Volumes) > 0 {
		for _, vol := range j.Volumes {
			if vol.VolumeNumber != volnum {
				break
			}
			total += vol.ZFSStreamBytes
			volnum++
		}
		j.Volumes = j.Volumes[:volnum-1]
	}
	return
}

// ValidateSendFlags will check if the options assigned to this JobInfo object is
// properly within the bounds for a send backup operation.
// nolint:golint,stylecheck // Error strings used as log messages for user
func (j *JobInfo) ValidateSendFlags() error {
	if j.MaxFileBuffer < 0 {
		return fmt.Errorf("The number of active files must be set to a value greater than or equal to 0. Was given %d", j.MaxFileBuffer)
	}

	if j.MaxParallelUploads <= 0 {
		return fmt.Errorf("The number of parallel uploads must be set to a value greater than 0. Was given %d", j.MaxParallelUploads)
	}

	if j.MaxFileBuffer < j.MaxParallelUploads {
		log.AppLogger.Warningf(
			"The number of parallel uploads (%d) is greater than the number of active files allowed (%d), this may result in an unachievable max parallel upload target.", //nolint:lll // Long log output
			j.MaxParallelUploads,
			j.MaxFileBuffer,
		)
	}

	if j.MaxRetryTime < 0 {
		return fmt.Errorf("The max retry time must be set to a value greater than or equal to 0. Was given %d", j.MaxRetryTime)
	}

	if j.MaxBackoffTime <= 0 {
		return fmt.Errorf("The max backoff time must be set to a value greater than 0. Was given %d", j.MaxBackoffTime)
	}

	if j.CompressionLevel < 1 || j.CompressionLevel > 9 {
		return fmt.Errorf("The compression level specified must be between 1 and 9. Was given %d", j.CompressionLevel)
	}

	if disallowedSeps.MatchString(j.Separator) {
		return fmt.Errorf(
			"The separator provided (%q) should not be used as it can conflict with allowed characters in zfs components",
			j.Separator,
		)
	}

	if j.UploadChunkSize < 5 || j.UploadChunkSize > 100 {
		return fmt.Errorf("The uploadChunkSize provided (%d) is not between 5 and 100", j.UploadChunkSize)
	}

	return nil
}

func (j *JobInfo) ManifestObjectName() string {
	extensions := []string{"manifest"}
	nameParts := []string{j.ManifestPrefix}

	baseParts, ext := j.volumeNameParts(true)
	extensions = append(extensions, ext...)
	nameParts = append(nameParts, baseParts...)

	return fmt.Sprintf("%s.%s", strings.Join(nameParts, j.Separator), strings.Join(extensions, "."))
}

// DefaultSeparator is the default for --separator.
const DefaultSeparator = "|"

// BackupVolumeObjectPrefix is the object name of every volume of this backup set up to,
// but not including, the volume number. Listing by it returns the set's volumes and nothing else.
func (j *JobInfo) BackupVolumeObjectPrefix() string {
	extensions := []string{"zstream"}

	nameParts, ext := j.volumeNameParts(false)
	extensions = append(extensions, ext...)
	extensions = append(extensions, "vol")

	return fmt.Sprintf("%s.%s", strings.Join(nameParts, j.Separator), strings.Join(extensions, "."))
}

func (j *JobInfo) BackupVolumeObjectName(volumeNumber int64) string {
	return j.BackupVolumeObjectPrefix() + strconv.FormatInt(volumeNumber, 10)
}

// ParseBackupVolumeObjectName is the inverse of BackupVolumeObjectName: it recognizes
// "<volume>|<snap>.zstream[.<compressor>][.pgp].vol<N>" and the incremental form
// "<volume>|<incr>|to|<snap>...". Anything else (manifests, foreign objects, names with a
// leading "/") is reported as not ok.
// nolint:gocritic // Named results document the parts
func ParseBackupVolumeObjectName(name, separator string) (volume, base, incr string, volNum int64, ok bool) {
	if separator == "" {
		return "", "", "", 0, false
	}
	idx := strings.LastIndex(name, ".zstream.")
	if idx < 0 {
		return "", "", "", 0, false
	}
	extensions := strings.Split(name[idx+len(".zstream."):], ".")
	last := extensions[len(extensions)-1]
	if !strings.HasPrefix(last, "vol") {
		return "", "", "", 0, false
	}
	volNum, err := strconv.ParseInt(strings.TrimPrefix(last, "vol"), 10, 64)
	if err != nil || volNum < 1 || strings.TrimPrefix(last, "vol") != strconv.FormatInt(volNum, 10) {
		return "", "", "", 0, false
	}
	switch extensions = extensions[:len(extensions)-1]; len(extensions) {
	case 0:
	case 1:
		if extensions[0] == "" {
			return "", "", "", 0, false
		}
	case 2:
		if extensions[0] == "" || extensions[0] == "pgp" || extensions[1] != "pgp" {
			return "", "", "", 0, false
		}
	default:
		return "", "", "", 0, false
	}

	parts := strings.Split(name[:idx], separator)
	switch {
	case len(parts) == 2:
		volume, base = parts[0], parts[1]
	case len(parts) == 4 && parts[2] == "to":
		volume, incr, base = parts[0], parts[1], parts[3]
	default:
		return "", "", "", 0, false
	}
	if volume == "" || base == "" || strings.HasPrefix(volume, "/") || (len(parts) == 4 && incr == "") {
		return "", "", "", 0, false
	}
	return volume, base, incr, volNum, true
}

func (j *JobInfo) volumeNameParts(isManifest bool) (nameParts, extensions []string) {
	extensions = make([]string, 0, 2)

	if j.EncryptKey != nil || j.SignKey != nil {
		extensions = append(extensions, "pgp")
	}

	compressorName := j.Compressor
	if isManifest {
		compressorName = InternalCompressor
	}

	switch compressorName {
	case InternalCompressor:
		extensions = append([]string{"gz"}, extensions...)
	case "", ZfsCompressor:
	default:
		extensions = append([]string{compressorName}, extensions...)
	}

	nameParts = []string{j.VolumeName}
	if j.IncrementalSnapshot.Name != "" {
		nameParts = append(nameParts, j.IncrementalSnapshot.Name, "to", j.BaseSnapshot.Name)
	} else {
		nameParts = append(nameParts, j.BaseSnapshot.Name)
	}

	return nameParts, extensions
}
