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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// MaxManifestBytes is the most of a manifest ReadManifest reads, decompressed, and the most a
// send writes: anyone who can write the destination can make a manifest inflate without bound,
// and a reader allocates about three times this much for one at the limit. A send records some
// 520 bytes of JSON per volume, so the limit is about 2 million volumes: one send of 400 TiB at
// the default --volsize of 200 MiB, or of 100 TiB at --volsize 50. (The old limit of 64 MiB was
// 128,558 volumes, 24.5 TiB at the default --volsize.) A variable so tests can lower it.
var MaxManifestBytes = 1 << 30

// MaxManifestVolumes is the most volumes a manifest may list. Each volume a send records takes
// more than 256 bytes of JSON (its keys and hex sums alone), so no manifest under
// MaxManifestBytes lists more; a forged one of `{},` would, and decode to a VolumeInfo of some
// 480 bytes per 3 bytes of JSON.
func MaxManifestVolumes() int { return MaxManifestBytes / 256 }

// ErrManifestTooLong marks a manifest longer than MaxManifestBytes. It is the one read error
// that may be a real backup's (one of more volumes than fit), so readers must not treat such a
// manifest as a forgery or a damaged file, nor advise deleting it.
var ErrManifestTooLong = errors.New("too long")

// ReadManifest extracts the manifest at path with the keys of j, checks its signature and
// decodes it. A manifest longer than MaxManifestBytes is an ErrManifestTooLong.
func ReadManifest(ctx context.Context, j *JobInfo, path string) (*JobInfo, error) {
	manifestVol, err := ExtractLocal(ctx, j, path, true)
	if err != nil {
		return nil, err
	}
	defer manifestVol.Close()
	// Read checks the signature at EOF, so nothing a forger wrote is decoded.
	data, err := readAtMost(manifestVol, MaxManifestBytes)
	if errors.Is(err, errTooLong) {
		return nil, fmt.Errorf("manifest %s is longer than %d bytes (%d MiB), the most this version reads: %w",
			path, MaxManifestBytes, MaxManifestBytes>>20, ErrManifestTooLong)
	} else if err != nil {
		return nil, err
	}
	// Check the volumes before decoding them: their number bounds what decoding allocates.
	var check struct{ Volumes manifestVolumes }
	if err = json.Unmarshal(data, &check); err != nil {
		return nil, fmt.Errorf("manifest %s: %w", path, err)
	}
	decodedManifest := new(JobInfo)
	if err = json.Unmarshal(data, decodedManifest); err != nil {
		return nil, err
	}
	return decodedManifest, nil
}

// manifestVolumes checks the Volumes array of a manifest without decoding its volumes: there are
// at most MaxManifestVolumes, and none is null (it would decode to a nil *VolumeInfo, which every
// reader of Volumes dereferences).
type manifestVolumes struct{}

func (manifestVolumes) UnmarshalJSON(data []byte) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('[') {
		return nil // null, or not an array: decoding the manifest reports the latter
	}
	for n := 1; dec.More(); n++ {
		if n > MaxManifestVolumes() {
			return fmt.Errorf("it lists more than %d volumes", MaxManifestVolumes())
		}
		var vol json.RawMessage
		if err := dec.Decode(&vol); err != nil {
			return err
		}
		if bytes.Equal(vol, []byte("null")) {
			return fmt.Errorf("its volume %d is null", n)
		}
	}
	return nil
}

var errTooLong = errors.New("too long")

// readAtMost reads all of r, which must be at most limit bytes long; else it returns errTooLong.
// Unlike io.ReadAll, whose slice grows by a quarter at a time when large, it allocates about twice
// what it reads (chunks, then one slice of the right size), not five times.
func readAtMost(r io.Reader, limit int) ([]byte, error) {
	var chunks [][]byte
	total, size := 0, 64<<10
	for {
		chunk := make([]byte, min(size, limit+1-total))
		n, err := io.ReadFull(r, chunk)
		chunks = append(chunks, chunk[:n])
		total += n
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			break
		} else if err != nil {
			return nil, err
		}
		if total > limit {
			return nil, errTooLong
		}
		size = min(2*size, 4<<20)
	}
	data := make([]byte, 0, total)
	for _, chunk := range chunks {
		data = append(data, chunk...)
	}
	return data, nil
}
