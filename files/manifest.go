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
	"context"
	"encoding/json"
	"fmt"
	"io"
)

// MaxManifestBytes is the most of a manifest ReadManifest reads, decompressed. The largest
// manifest the e2e suite writes is 2.6 KB; anyone who can write the destination can make one
// inflate without bound.
const MaxManifestBytes = 64 << 20

// ReadManifest extracts the manifest at path with the keys of j, checks its signature and
// decodes it. A manifest longer than MaxManifestBytes is an error.
func ReadManifest(ctx context.Context, j *JobInfo, path string) (*JobInfo, error) {
	manifestVol, err := ExtractLocal(ctx, j, path, true)
	if err != nil {
		return nil, err
	}
	defer manifestVol.Close()
	// Read checks the signature at EOF, so nothing a forger wrote is decoded.
	data, err := io.ReadAll(io.LimitReader(manifestVol, MaxManifestBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > MaxManifestBytes {
		return nil, fmt.Errorf("manifest %s is longer than %d MiB", path, MaxManifestBytes>>20)
	}
	decodedManifest := new(JobInfo)
	if err = json.Unmarshal(data, decodedManifest); err != nil {
		return nil, err
	}
	return decodedManifest, nil
}
