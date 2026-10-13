package files

// Regression test for finding 16 of
// docs/specs/2026-10-12--adversarial-review-plan-send-restore/findings.md: --volsize 0, and a
// --volsize that S3 cannot take in 10,000 parts of --uploadChunkSize, were accepted.

import (
	"strings"
	"testing"
)

func TestValidateVolumeSize(t *testing.T) {
	limit := uint64(S3MaxVolumeParts * 10)
	for _, c := range []struct {
		name         string
		volsize      uint64
		destinations []string
		want         string
	}{
		{"zero", 0, nil, "greater than 0"},
		{"zero to file", 0, []string{"file:///tmp/x"}, "greater than 0"},
		{"default", 200, []string{"s3://bucket/prefix"}, ""},
		{"at the S3 limit", limit, []string{"s3://bucket"}, ""},
		{"over the S3 limit", limit + 1, []string{"file:///tmp/x", "s3://bucket"}, "too large for the s3:// destination"},
		{"over the S3 limit, no S3", limit + 1, []string{"file:///tmp/x", "gs://bucket"}, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			j := validSendJob()
			j.VolumeSize = c.volsize
			j.Destinations = c.destinations
			err := j.ValidateSendFlags()
			switch {
			case c.want == "" && err != nil:
				t.Errorf("ValidateSendFlags = %v, want nil", err)
			case c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)):
				t.Errorf("ValidateSendFlags = %v, want an error with %q", err, c.want)
			}
		})
	}
}
