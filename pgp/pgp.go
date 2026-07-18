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

package pgp

import (
	"fmt"
	"os"
	"strings"

	openpgp "github.com/ProtonMail/go-crypto/openpgp/v2"
	"github.com/ProtonMail/gopenpgp/v3/crypto"

	"github.com/someone1/zfsbackup-go/log"
)

var (
	pubKeys  []*crypto.Key
	privKeys []*crypto.Key
)

// GetPublicKeyByEmail returns the key from the public PGP ring (if available)
// matching the provided email address.
func GetPublicKeyByEmail(email string) *crypto.Key {
	return findKeyByEmail(pubKeys, email)
}

// GetPrivateKeyByEmail returns the key from the secret PGP ring (if available)
// matching the provided email address. The returned key may be locked.
func GetPrivateKeyByEmail(email string) *crypto.Key {
	return findKeyByEmail(privKeys, email)
}

// ReplacePrivateKey swaps a previously-returned locked key with its unlocked
// counterpart in the in-memory secret ring so subsequent lookups receive the
// unlocked key.
func ReplacePrivateKey(old, unlocked *crypto.Key) {
	for i, k := range privKeys {
		if k == old {
			privKeys[i] = unlocked
			return
		}
	}
}

func findKeyByEmail(keys []*crypto.Key, email string) *crypto.Key {
	for _, k := range keys {
		entity := k.GetEntity()
		if entity == nil {
			continue
		}
		for _, ident := range entity.Identities {
			if ident.UserId != nil && ident.UserId.Email == email {
				return k
			}
		}
	}
	return nil
}

// LoadPublicRing opens and parses the PGP public keyring from the given path.
func LoadPublicRing(path string) error {
	keys, err := readKeysFromFile(path)
	if err != nil {
		return err
	}
	pubKeys = keys
	return nil
}

// LoadPrivateRing opens and parses the PGP private keyring from the given path.
// Keys may be locked; they are not unlocked here.
func LoadPrivateRing(path string) error {
	keys, err := readKeysFromFile(path)
	if err != nil {
		return err
	}
	privKeys = keys
	return nil
}

func readKeysFromFile(path string) ([]*crypto.Key, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	entities, err := openpgp.ReadArmoredKeyRing(f)
	if err != nil {
		return nil, err
	}

	keys := make([]*crypto.Key, 0, len(entities))
	for _, e := range entities {
		k, err := crypto.NewKeyFromEntity(e)
		if err != nil {
			return nil, err
		}
		keys = append(keys, k)
	}
	return keys, nil
}

// PrintPGPDebugInformation logs the keys it has read in from each keyring.
func PrintPGPDebugInformation() {
	debugStr := make([]string, 0, 4)
	debugStr = append(debugStr, "PGP Debug Info:\nLoaded Private Keys:")
	for _, k := range privKeys {
		debugStr = append(debugStr, fmt.Sprintf("\t%s\n\t%v", k.GetHexKeyID(), identitiesOf(k)))
	}

	debugStr = append(debugStr, "\nLoaded Public Keys:")
	for _, k := range pubKeys {
		debugStr = append(debugStr, fmt.Sprintf("\t%s\n\t%v", k.GetHexKeyID(), identitiesOf(k)))
	}

	log.AppLogger.Debugf("%s", strings.Join(debugStr, "\n"))
}

func identitiesOf(k *crypto.Key) []string {
	entity := k.GetEntity()
	if entity == nil {
		return nil
	}
	ids := make([]string, 0, len(entity.Identities))
	for name := range entity.Identities {
		ids = append(ids, name)
	}
	return ids
}
