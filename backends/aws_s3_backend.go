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

package backends

import (
	"context"
	"crypto/md5" //nolint:gosec // MD5 not used cryptographically here
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/aws/request"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/aws/aws-sdk-go/service/s3/s3iface"
	"github.com/aws/aws-sdk-go/service/s3/s3manager"
	"github.com/aws/aws-sdk-go/service/s3/s3manager/s3manageriface"

	"github.com/someone1/zfsbackup-go/files"
	"github.com/someone1/zfsbackup-go/log"
)

const (
	// AWSS3BackendPrefix is the URI prefix used for the AWSS3Backend.
	AWSS3BackendPrefix = "s3"
	// AWSS3PageSize is the maximum page size for listing objects in S3.
	AWSS3PageSize = 1000
)

// AWSS3Backend integrates with Amazon Web Services' S3.
type AWSS3Backend struct {
	conf       *BackendConfig
	mutex      sync.Mutex
	client     s3iface.S3API
	uploader   s3manageriface.UploaderAPI
	prefix     string
	legacy     []string
	bucketName string
}

// Authenticate https://godoc.org/github.com/aws/aws-sdk-go/aws/session#hdr-Environment_Variables

type logger struct{}

func (l logger) Log(args ...interface{}) {
	log.AppLogger.Debugf("s3 backend:", args...)
}

type withS3Client struct{ client s3iface.S3API }

func (w withS3Client) Apply(b Backend) {
	if v, ok := b.(*AWSS3Backend); ok {
		v.client = w.client
	}
}

// WithS3Client will override an S3 backend's underlying API client with the one provided.
// Primarily used to inject mock clients for testing.
func WithS3Client(c s3iface.S3API) Option {
	return withS3Client{c}
}

type withS3Uploader struct{ uploader s3manageriface.UploaderAPI }

func (w withS3Uploader) Apply(b Backend) {
	if v, ok := b.(*AWSS3Backend); ok {
		v.uploader = w.uploader
	}
}

// WithS3Uploader will override an S3 backend's underlying uploader client with the one provided.
// Primarily used to inject mock clients for testing.
func WithS3Uploader(c s3manageriface.UploaderAPI) Option {
	return withS3Uploader{c}
}

// Init will initialize the AWSS3Backend and verify the provided URI is valid/exists.
func (a *AWSS3Backend) Init(ctx context.Context, conf *BackendConfig, opts ...Option) error {
	a.conf = conf

	bucket, _, prefix, err := parseObjectURI(a.conf.TargetURI, AWSS3BackendPrefix)
	if err != nil {
		return err
	}
	a.bucketName = bucket
	a.prefix = prefix
	a.legacy = legacyPrefixes(conf.TargetURI, conf.TypedURI, AWSS3BackendPrefix)

	for _, opt := range opts {
		opt.Apply(a)
	}

	if a.client == nil {
		awsconf := aws.NewConfig().
			WithS3ForcePathStyle(true).
			WithEndpoint(os.Getenv("AWS_S3_CUSTOM_ENDPOINT"))
		if enableDebug, _ := strconv.ParseBool(os.Getenv("AWS_S3_ENABLE_DEBUG")); enableDebug {
			awsconf = awsconf.WithLogger(logger{}).
				WithLogLevel(aws.LogDebugWithRequestRetries | aws.LogDebugWithRequestErrors)
		}

		sess, err := session.NewSession(awsconf)
		if err != nil {
			return err
		}

		a.client = s3.New(sess)
	}

	if a.uploader == nil {
		a.uploader = s3manager.NewUploaderWithClient(a.client, func(u *s3manager.Uploader) {
			u.Concurrency = conf.MaxParallelUploads
		}, func(u *s3manager.Uploader) {
			u.PartSize = int64(conf.UploadChunkSize)
		})
	}

	listReq := &s3.ListObjectsV2Input{
		Bucket:  aws.String(a.bucketName),
		MaxKeys: aws.Int64(0),
	}

	if _, err = a.client.ListObjectsV2WithContext(ctx, listReq); err != nil {
		return err
	}

	return checkLegacyLayout(conf, a.legacy, prefix, func(p string) (string, error) { return a.firstKey(ctx, p) })
}

// firstKey returns the first key in the bucket that starts with keyPrefix, or "".
func (a *AWSS3Backend) firstKey(ctx context.Context, keyPrefix string) (string, error) {
	resp, lerr := a.client.ListObjectsV2WithContext(ctx, &s3.ListObjectsV2Input{
		Bucket:  aws.String(a.bucketName),
		MaxKeys: aws.Int64(1),
		Prefix:  aws.String(keyPrefix),
	})
	if lerr != nil || len(resp.Contents) == 0 {
		return "", lerr
	}
	return aws.StringValue(resp.Contents[0].Key), nil
}

// FindLegacyVolume implements LegacyVolumeFinder.
func (a *AWSS3Backend) FindLegacyVolume(ctx context.Context, datasets, separators []string) (string, error) {
	return findLegacyVolume(a.legacy, datasets, separators, func(p string) (string, error) { return a.firstKey(ctx, p) })
}

func withContentMD5Header(md5sum string) request.Option {
	return func(ro *request.Request) {
		if md5sum != "" {
			ro.Handlers.Build.PushBack(func(r *request.Request) {
				r.HTTPRequest.Header.Set("Content-MD5", md5sum)
			})
		}
	}
}

func withRequestLimiter(buffer chan bool) request.Option {
	return func(ro *request.Request) {
		ro.Handlers.Send.PushFront(func(r *request.Request) {
			buffer <- true
		})

		ro.Handlers.Send.PushBack(func(r *request.Request) {
			<-buffer
		})
	}
}

func withComputeMD5HashHandler(ro *request.Request) {
	ro.Handlers.Build.PushBack(func(r *request.Request) {
		reader := r.GetBody()
		if reader == nil {
			return
		}

		//nolint:gosec // MD5 not used cryptographically here
		md5Raw := md5.New()
		_, err := io.Copy(md5Raw, reader)
		if err != nil {
			r.Error = err
			return
		}
		_, r.Error = reader.Seek(0, io.SeekStart)
		b64md5 := base64.StdEncoding.EncodeToString(md5Raw.Sum(nil))
		r.HTTPRequest.Header.Set("Content-MD5", b64md5)
	})
}

type reader struct {
	r io.Reader
}

func (r *reader) Read(p []byte) (int, error) {
	return r.r.Read(p)
}

// Upload will upload the provided volume to this AWSS3Backend's configured bucket+prefix
// It utilizes multipart uploads to upload a single file in chunks concurrently. Impartial
// uploads are cleaned up by the s3manager provided by the AWS Go SDK, but users can also
// implement lifecycle rules, see:
// https://docs.aws.amazon.com/AmazonS3/latest/dev/mpuoverview.html#mpu-abort-incomplete-mpu-lifecycle-config
func (a *AWSS3Backend) Upload(ctx context.Context, vol *files.VolumeInfo) error {
	// We will achieve parallel upload by splitting a single upload into chunks
	// so don't let multiple calls to this function run in parallel.
	a.mutex.Lock()
	defer a.mutex.Unlock()

	key := a.prefix + vol.ObjectName
	var options []request.Option
	options = append(options, withRequestLimiter(a.conf.MaxParallelUploadBuffer))
	var r io.Reader

	if !vol.IsUsingPipe() {
		r = vol
		if vol.Size < uint64(s3manager.MinUploadPartSize) {
			// It will not chunk the upload so we already know the md5 of the content
			md5Raw, merr := hex.DecodeString(vol.MD5Sum)
			if merr != nil {
				return merr
			}
			b64md5 := base64.StdEncoding.EncodeToString(md5Raw)
			options = append(options, withContentMD5Header(b64md5))
		} else {
			options = append(options, withComputeMD5HashHandler)
		}
	} else {
		r = &reader{vol} // Remove the Seek interface since we are using a Pipe
	}

	// Do a MultiPart Upload - force the s3manager to compute each chunks md5 hash
	_, err := a.uploader.UploadWithContext(ctx, &s3manager.UploadInput{
		Bucket:       aws.String(a.bucketName),
		Key:          aws.String(key),
		Body:         r,
		StorageClass: getS3EnvironmentOverride("AWS_S3_STORAGE_CLASS"),
	}, s3manager.WithUploaderRequestOptions(options...))

	if err != nil {
		log.AppLogger.Debugf("s3 backend: Error while uploading volume %s - %v", vol.ObjectName, err)
	}
	return err
}

// Delete will delete the given object from the configured bucket
func (a *AWSS3Backend) Delete(ctx context.Context, key string) error {
	_, err := a.client.DeleteObjectWithContext(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(a.bucketName),
		Key:    aws.String(a.prefix + key),
	})

	return err
}

// PreDownload restores objects in the GLACIER and DEEP_ARCHIVE storage classes, and
// INTELLIGENT_TIERING objects in its ARCHIVE_ACCESS and DEEP_ARCHIVE_ACCESS tiers, which a GET
// cannot read until a restore has run (GLACIER_IR and the other classes read directly), and
// waits for the restores to finish. AWS_S3_GLACIER_RESTORE_TIER picks the restore tier
// (default Bulk); AWS_S3_RESTORE_POLL_INTERVAL sets how often a pending restore is polled
// (a duration, default 1m; the interval grows up to ten times that).
func (a *AWSS3Backend) PreDownload(ctx context.Context, keys []string) error {
	toRestore := make([]string, 0, len(keys))
	restoreTier := os.Getenv("AWS_S3_GLACIER_RESTORE_TIER")
	if restoreTier == "" {
		restoreTier = s3.TierBulk
	}
	var bytesToRestore int64
	log.AppLogger.Debugf("s3 backend: will use the %s restore tier when trying to restore from Glacier.", restoreTier)
	for _, key := range keys {
		key = a.prefix + key
		resp, err := a.client.HeadObjectWithContext(ctx, &s3.HeadObjectInput{
			Bucket: aws.String(a.bucketName),
			Key:    aws.String(key),
		})
		if err != nil {
			return err
		}
		if !needsRestore(resp) {
			continue
		}
		if restored(resp) {
			log.AppLogger.Debugf("s3 backend: key %s is in the %s storage class and already restored.", key, *resp.StorageClass)
			continue
		}
		bytesToRestore += aws.Int64Value(resp.ContentLength)
		toRestore = append(toRestore, key)
		if restoreInProgress(resp.Restore) {
			log.AppLogger.Debugf("s3 backend: key %s is already being restored from the %s storage class.", key, *resp.StorageClass)
			continue
		}
		log.AppLogger.Debugf("s3 backend: key %s will be restored from the %s storage class.", key, *resp.StorageClass)
		request := &s3.RestoreRequest{
			GlacierJobParameters: &s3.GlacierJobParameters{
				Tier: aws.String(restoreTier),
			},
		}
		// A restored Intelligent-Tiering object moves back to a frequent-access tier for good;
		// S3 rejects a lifetime for it.
		if resp.ArchiveStatus == nil {
			request.Days = aws.Int64(3)
		}
		_, rerr := a.client.RestoreObjectWithContext(ctx, &s3.RestoreObjectInput{
			Bucket:         aws.String(a.bucketName),
			Key:            aws.String(key),
			RestoreRequest: request,
		})
		if rerr != nil {
			if aerr, ok := rerr.(awserr.Error); ok && aerr.Code() == "RestoreAlreadyInProgress" {
				continue
			}
			log.AppLogger.Debugf("s3 backend: error trying to restore key %s - %v", key, rerr)
			return rerr
		}
	}
	if len(toRestore) == 0 {
		return nil
	}
	log.AppLogger.Infof(
		"s3 backend: waiting for %d objects to restore from an archive storage class totaling %d bytes (this could take several hours)",
		len(toRestore), bytesToRestore,
	)
	// Now wait for the objects to be restored
	poll := restorePollInterval()
	backoffCount := 1
	for idx := 0; idx < len(toRestore); idx++ {
		key := toRestore[idx]
		resp, err := a.client.HeadObjectWithContext(ctx, &s3.HeadObjectInput{
			Bucket: aws.String(a.bucketName),
			Key:    aws.String(key),
		})
		if err != nil {
			return err
		}
		if restored(resp) {
			backoffCount = 1
			log.AppLogger.Debugf("s3 backend: key %s restored.", key)
			continue
		}
		// Still ongoing, or no x-amz-restore header yet (the request was only just accepted).
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Duration(backoffCount) * poll):
		}
		idx--
		if backoffCount < 10 {
			backoffCount++
		}
	}
	return nil
}

// needsRestore reports whether an object must be restored before a GET can read it: its
// storage class is an archive one, or Intelligent-Tiering has moved it to an archive tier.
func needsRestore(resp *s3.HeadObjectOutput) bool {
	switch aws.StringValue(resp.StorageClass) {
	case s3.ObjectStorageClassGlacier, s3.ObjectStorageClassDeepArchive:
		return true
	}
	return resp.ArchiveStatus != nil
}

// restored reports whether a GET can read an object needsRestore picked: an archive storage
// class object has a restored copy, an Intelligent-Tiering one has left the archive tier.
func restored(resp *s3.HeadObjectOutput) bool {
	if aws.StringValue(resp.StorageClass) == s3.ObjectStorageClassIntelligentTiering {
		return resp.ArchiveStatus == nil
	}
	return restoreDone(resp.Restore)
}

// restoreInProgress reads the x-amz-restore header of a HEAD response: a restore has been
// requested and is not done.
func restoreInProgress(restore *string) bool {
	return restore != nil && strings.Contains(*restore, `ongoing-request="true"`)
}

// restoreDone reads the x-amz-restore header of a HEAD response: a restored copy is present.
// No header at all (nil) means no restore was requested, or the request is too new to show.
func restoreDone(restore *string) bool {
	return restore != nil && !restoreInProgress(restore)
}

// restorePollInterval is how long PreDownload waits between polls of a pending restore.
func restorePollInterval() time.Duration {
	if v := os.Getenv("AWS_S3_RESTORE_POLL_INTERVAL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
		log.AppLogger.Warningf("s3 backend: ignoring AWS_S3_RESTORE_POLL_INTERVAL=%q: want a duration such as 30s.", v)
	}
	return time.Minute
}

// Download will download the requseted object which can be read from the returned io.ReadCloser
func (a *AWSS3Backend) Download(ctx context.Context, key string) (io.ReadCloser, error) {
	resp, err := a.client.GetObjectWithContext(ctx, &s3.GetObjectInput{
		Bucket: aws.String(a.bucketName),
		Key:    aws.String(a.prefix + key),
	})
	if err != nil {
		return nil, err
	}
	return resp.Body, nil
}

// Close will release any resources used by the AWS S3 backend.
func (a *AWSS3Backend) Close() error {
	a.client = nil
	a.uploader = nil
	return nil
}

// List will iterate through all objects in the configured AWS S3 bucket and return
// a list of keys, filtering by the provided prefix.
func (a *AWSS3Backend) List(ctx context.Context, prefix string) ([]string, error) {
	fetchPage := func(token *string) (*s3.ListObjectsV2Output, error) {
		return a.client.ListObjectsV2WithContext(ctx, &s3.ListObjectsV2Input{
			Bucket:            aws.String(a.bucketName),
			MaxKeys:           aws.Int64(AWSS3PageSize),
			Prefix:            aws.String(a.prefix + prefix),
			ContinuationToken: token,
		})
	}

	// Initial page
	resp, err := fetchPage(nil)
	if err != nil {
		return nil, err
	}

	l := make([]string, 0, AWSS3PageSize)
	for {
		for _, obj := range resp.Contents {
			if name, ok := relativeKey(AWSS3BackendPrefix, a.prefix, *obj.Key); ok {
				l = append(l, name)
			}
		}

		if !*resp.IsTruncated {
			break
		}

		// Next page
		resp, err = fetchPage(resp.NextContinuationToken)
		if err != nil {
			return nil, fmt.Errorf("s3 backend: could not list bucket due to error - %v", err)
		}
	}

	return l, nil
}

func getS3EnvironmentOverride(envVar string) *string {
	storageClass := os.Getenv(envVar)
	if storageClass != "" {
		return aws.String(storageClass)
	}
	return nil
}
