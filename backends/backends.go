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
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/someone1/zfsbackup-go/files"
	"github.com/someone1/zfsbackup-go/log"
)

// Backend is an interface type that defines the functions and functionality required for different backend implementations.
// nolint:lll // It's neater this way
type Backend interface {
	Init(ctx context.Context, conf *BackendConfig, opts ...Option) error  // Verifies settings required for backend are present and valid, does basic initialization of backend
	Upload(ctx context.Context, vol *files.VolumeInfo) error              // Upload the volume provided
	List(ctx context.Context, prefix string) ([]string, error)            // Lists all files in the backend, filtering by the provided prefix.
	Close() error                                                         // Release any resources in use
	PreDownload(ctx context.Context, objects []string) error              // PreDownload will prepare the provided files for download (think restoring from Glacier to S3)
	Download(ctx context.Context, filename string) (io.ReadCloser, error) // Download the requested file that can be read from the returned io.ReaderCloser
	Delete(ctx context.Context, filename string) error                    // Delete the file specified on the configured backend
}

// Sizer is implemented by backends that write a volume under its final name as it uploads
// (file, ssh), so a killed upload can leave a truncated object behind. Object stores only
// expose an object once its upload completes, so they need not implement it.
type Sizer interface {
	Size(ctx context.Context, filename string) (uint64, error) // Size of the stored object in bytes
}

// Option lets users inject functionality to specific backends
type Option interface {
	Apply(Backend)
}

// BackendConfig holds values that relate to backend configurations
type BackendConfig struct {
	MaxParallelUploadBuffer chan bool
	MaxParallelUploads      int
	MaxBackoffTime          time.Duration
	MaxRetryTime            time.Duration
	TargetURI               string
	UploadChunkSize         int
	// ManifestPrefix lets object-store backends detect the pre-normalization
	// key layout (see checkLegacyLayout). Empty disables the check.
	ManifestPrefix string
}

var (
	// ErrInvalidURI is returned when a backend determines that the provided URI is malformed/invalid.
	ErrInvalidURI = errors.New("backends: invalid URI provided to backend")
	// ErrInvalidPrefix is returned when a backend destination is provided with a URI prefix that isn't registered.
	ErrInvalidPrefix = errors.New("backends: the provided prefix does not exist")
)

// GetBackendForURI will try and parse the URI for a matching backend to use.
func GetBackendForURI(uri string) (Backend, error) {
	prefix := strings.Split(uri, "://")
	if len(prefix) < 2 {
		return nil, ErrInvalidURI
	}

	switch prefix[0] {
	case DeleteBackendPrefix:
		return &DeleteBackend{}, nil
	case GoogleCloudStorageBackendPrefix:
		return &GoogleCloudStorageBackend{}, nil
	case AWSS3BackendPrefix:
		return &AWSS3Backend{}, nil
	case FileBackendPrefix:
		return &FileBackend{}, nil
	case AzureBackendPrefix:
		return &AzureBackend{}, nil
	case B2BackendPrefix:
		return &B2Backend{}, nil
	case SSHBackendPrefix:
		return &SSHBackend{}, nil
	default:
		return nil, ErrInvalidPrefix
	}
}

// objectStoreSchemes are the backends whose URI path is a key prefix inside a bucket/container.
var objectStoreSchemes = map[string]bool{
	AWSS3BackendPrefix:              true,
	GoogleCloudStorageBackendPrefix: true,
	AzureBackendPrefix:              true,
	B2BackendPrefix:                 true,
}

// objectPrefix turns the path part of an object-store URI into a key prefix that names a
// "directory": "", "/" -> ""; "p", "p/", "/p/", "p//" -> "p/". Without the trailing slash,
// s3://b/p would also match keys under p-other/ and concatenate to keys like "pmanifests|...".
func objectPrefix(uriPath string) string {
	prefix := strings.Trim(uriPath, "/")
	if prefix != "" {
		prefix += "/"
	}
	return prefix
}

// parseObjectURI splits an object-store URI into its bucket/container and normalized key prefix.
// rawPrefix is the path with surrounding slashes trimmed, as older versions used it (minus the slashes).
func parseObjectURI(uri, scheme string) (bucket, rawPrefix, prefix string, err error) {
	clean := strings.TrimPrefix(uri, scheme+"://")
	if clean == uri {
		return "", "", "", ErrInvalidURI
	}
	bucket, path, _ := strings.Cut(clean, "/")
	return bucket, strings.Trim(path, "/"), objectPrefix(path), nil
}

// stripPrefix returns key relative to prefix, and false if key is not under prefix.
func stripPrefix(prefix, key string) (string, bool) {
	if !strings.HasPrefix(key, prefix) {
		return "", false
	}
	return key[len(prefix):], true
}

// relativeKey is stripPrefix for List implementations: keys outside the prefix are logged and dropped.
func relativeKey(backend, prefix, key string) (string, bool) {
	name, ok := stripPrefix(prefix, key)
	if !ok {
		log.AppLogger.Debugf("%s backend: ignoring listed key %s outside of prefix %q", backend, key, prefix)
	}
	return name, ok
}

// checkLegacyLayout fails when the destination holds manifests written before object-store
// prefixes were normalized, i.e. at "<rawPrefix><ManifestPrefix>..." with no "/" in between.
// firstKey must return the first key in the bucket starting with the given (bucket-absolute)
// prefix, or "" if there is none.
func checkLegacyLayout(conf *BackendConfig, rawPrefix string, firstKey func(prefix string) (string, error)) error {
	if rawPrefix == "" || conf.ManifestPrefix == "" {
		return nil
	}
	key, err := firstKey(rawPrefix + conf.ManifestPrefix)
	if err != nil {
		return err
	}
	if key == "" {
		return nil
	}
	return fmt.Errorf(
		"found %s, a backup written by an older version that used the URI path %q as a raw key prefix; "+
			"move every object starting with %q to %q (i.e. under %s/) and retry",
		key, rawPrefix, rawPrefix, rawPrefix+"/", rawPrefix,
	)
}

// CanonicalURI normalizes object-store URIs so that spellings with and without a trailing
// slash name the same destination (and therefore share one local manifest cache):
// s3://b/p -> s3://b/p/, s3://b/ -> s3://b. Other schemes are returned unchanged.
func CanonicalURI(uri string) string {
	scheme, rest, ok := strings.Cut(uri, "://")
	if !ok || !objectStoreSchemes[scheme] {
		return uri
	}
	bucket, path, _ := strings.Cut(rest, "/")
	if prefix := objectPrefix(path); prefix != "" {
		return scheme + "://" + bucket + "/" + prefix
	}
	return scheme + "://" + bucket
}
