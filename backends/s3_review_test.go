package backends

import (
	"bytes"
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/someone1/zfsbackup-go/files"
)

// multipartFake is a path-style S3 that starts a multipart upload and counts the aborts.
// With cancel set, the first part PUT cancels the caller's context (as SIGTERM does) and then
// waits for the client to go away. Without it, every part PUT fails with a 400.
type multipartFake struct {
	mu     sync.Mutex
	cancel context.CancelFunc
	aborts int
	parts  int
}

func (f *multipartFake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	switch {
	case q.Has("list-type"):
		w.Header().Set("Content-Type", "application/xml")
		fmt.Fprint(w, `<?xml version="1.0" encoding="UTF-8"?><ListBucketResult><Name>bucket</Name><KeyCount>0</KeyCount><IsTruncated>false</IsTruncated></ListBucketResult>`)
	case r.Method == http.MethodPost && q.Has("uploads"):
		w.Header().Set("Content-Type", "application/xml")
		fmt.Fprint(w, `<?xml version="1.0" encoding="UTF-8"?><InitiateMultipartUploadResult><Bucket>bucket</Bucket><Key>k</Key><UploadId>upload-1</UploadId></InitiateMultipartUploadResult>`)
	case r.Method == http.MethodPut && q.Has("partNumber"):
		f.mu.Lock()
		f.parts++
		cancel := f.cancel
		f.mu.Unlock()
		_, _ = io.Copy(io.Discard, r.Body)
		if cancel == nil {
			w.Header().Set("Content-Type", "application/xml")
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `<?xml version="1.0" encoding="UTF-8"?><Error><Code>InvalidRequest</Code><Message>no</Message></Error>`)
			return
		}
		cancel() // the operator's SIGTERM lands mid-part
		<-r.Context().Done()
	case r.Method == http.MethodDelete && q.Has("uploadId"):
		f.mu.Lock()
		if q.Get("uploadId") == "upload-1" {
			f.aborts++
		}
		f.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	default:
		w.WriteHeader(http.StatusInternalServerError)
	}
}

func (f *multipartFake) counts() (parts, aborts int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.parts, f.aborts
}

// uploadToMultipartFake uploads an 11 MiB volume (three 5 MiB parts) to f with ctx.
func uploadToMultipartFake(t *testing.T, f *multipartFake, ctx context.Context) error {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	t.Setenv("AWS_S3_CUSTOM_ENDPOINT", srv.URL)
	t.Setenv("AWS_REGION", "us-east-1")
	t.Setenv("AWS_ACCESS_KEY_ID", "x")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "y")

	conf := &BackendConfig{
		TargetURI:               "s3://bucket/p/",
		MaxParallelUploads:      1,
		MaxParallelUploadBuffer: make(chan bool, 1),
		UploadChunkSize:         5 * 1024 * 1024,
	}
	b := &AWSS3Backend{}
	if err := b.Init(context.Background(), conf); err != nil {
		t.Fatal(err)
	}
	payload := make([]byte, 11*1024*1024)
	_, _ = rand.Read(payload)
	vol, err := files.CreateSimpleVolume(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = io.Copy(vol, bytes.NewReader(payload)); err != nil {
		t.Fatal(err)
	}
	if err = vol.Close(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = vol.DeleteVolume() })
	vol.ObjectName = "tank/data|a.zstream.vol1"
	if err = vol.OpenVolume(); err != nil {
		t.Fatal(err)
	}
	return b.Upload(ctx, vol)
}

// Finding 12: a multipart upload cut by a context cancel (SIGTERM) must still be aborted, or its
// parts stay in the bucket, billed, and invisible to List and so to clean.
func TestS3MultipartUploadAbortedAfterCancel(t *testing.T) {
	f := &multipartFake{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.cancel = cancel

	err := uploadToMultipartFake(t, f, ctx)
	if err == nil || !strings.Contains(err.Error(), "anceled") {
		t.Fatalf("want a canceled upload, got %v", err)
	}
	// Upload sends the abort before it returns: no wait.
	parts, aborts := f.counts()
	t.Logf("parts seen %d, aborts received %d", parts, aborts)
	if aborts != 1 {
		t.Errorf("got %d AbortMultipartUpload requests after the cancel, want 1: the parts stay in the bucket", aborts)
	}
}

// Finding 12: on a failure that is not a cancel, s3manager aborts the upload itself. Upload must
// not send a second abort.
func TestS3MultipartUploadFailureAbortedOnce(t *testing.T) {
	f := &multipartFake{}
	err := uploadToMultipartFake(t, f, context.Background())
	if err == nil {
		t.Fatal("want a failed upload")
	}
	time.Sleep(100 * time.Millisecond) // a stray abort has time to arrive
	parts, aborts := f.counts()
	t.Logf("error %v; parts seen %d, aborts received %d", err, parts, aborts)
	if aborts != 1 {
		t.Errorf("got %d AbortMultipartUpload requests after a failed part, want 1", aborts)
	}
}

// Finding 13: an s3:// URI with user:password@ (not a supported form, but the ssh:// one has it)
// put the password in the bucket name; a network error from the SDK quotes the request URL,
// password included, and the callers log that error.
func TestS3InitRejectsUserInfoInBucket(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	endpoint := srv.URL
	srv.Close() // connection refused from now on
	t.Setenv("AWS_S3_CUSTOM_ENDPOINT", endpoint)
	t.Setenv("AWS_REGION", "us-east-1")
	t.Setenv("AWS_ACCESS_KEY_ID", "x")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "y")
	b := &AWSS3Backend{}
	err := b.Init(context.Background(), &BackendConfig{TargetURI: "s3://AKIAEXAMPLE:hunter2secret@bucket/prefix/", ManifestPrefix: "manifests"})
	if err == nil {
		t.Fatal("want an error")
	}
	t.Logf("error: %v", err)
	if strings.Contains(err.Error(), "hunter2secret") || strings.Contains(err.Error(), "AKIAEXAMPLE") {
		t.Errorf("Init error text holds the URI user or password")
	}
	if !strings.Contains(err.Error(), "':' or '@'") {
		t.Errorf("want the error to say why the URI is refused, got %v", err)
	}
}

// Finding 13, the other object stores: gs, azure and b2 parse their URI with parseObjectURI too.
func TestParseObjectURIRejectsUserInfo(t *testing.T) {
	for _, scheme := range []string{AWSS3BackendPrefix, GoogleCloudStorageBackendPrefix, AzureBackendPrefix, B2BackendPrefix} {
		for _, rest := range []string{"user:hunter2secret@b/p/", "user@b", "b:1/p"} {
			_, _, _, err := parseObjectURI(scheme+"://"+rest, scheme)
			if err == nil {
				t.Errorf("%s://%s: want an error", scheme, rest)
				continue
			}
			if strings.Contains(err.Error(), "hunter2secret") || strings.Contains(err.Error(), "b:1") {
				t.Errorf("%s://%s: the error echoes the URI: %v", scheme, rest, err)
			}
		}
		// An '@' or ':' in the key prefix is not the bucket's.
		if bucket, _, prefix, err := parseObjectURI(scheme+"://b/p@q:r/", scheme); err != nil || bucket != "b" || prefix != "p@q:r/" {
			t.Errorf("%s://b/p@q:r/: got %q %q %v", scheme, bucket, prefix, err)
		}
	}
}
