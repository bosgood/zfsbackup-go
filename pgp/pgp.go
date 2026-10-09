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
	"errors"
	"fmt"
	"os"
	"strings"

	"golang.org/x/crypto/openpgp"

	"github.com/someone1/zfsbackup-go/log"
)

var (
	pubRing openpgp.EntityList
	secRing openpgp.EntityList
)

// GetPublicKeyByEmail will return the key from the pubpoic PGP ring (if available) matching
// the provided email address.
func GetPublicKeyByEmail(email string) *openpgp.Entity {
	return getKeyByEmail(pubRing, email)
}

// GetPrivateKeyByEmail will return the key from the secret PGP ring (if available) matching
// the provided email address.
func GetPrivateKeyByEmail(email string) *openpgp.Entity {
	return getKeyByEmail(secRing, email)
}

// GetPublicKeys returns every key in the public ring that has an identity for email, or whose
// primary key's fingerprint is fingerprint: 40 hex digits, in any case, spaces and a 0x prefix
// allowed.
func GetPublicKeys(emailOrFingerprint string) []*openpgp.Entity {
	fingerprint := strings.ToUpper(strings.ReplaceAll(emailOrFingerprint, " ", ""))
	fingerprint = strings.TrimPrefix(fingerprint, "0X")
	var keys []*openpgp.Entity
	for _, entity := range pubRing {
		if fmt.Sprintf("%X", entity.PrimaryKey.Fingerprint) == fingerprint {
			keys = append(keys, entity)
			continue
		}
		for _, ident := range entity.Identities {
			if ident.UserId.Email == emailOrFingerprint {
				keys = append(keys, entity)
				break
			}
		}
	}
	return keys
}

// GetCombinedKeyRing will return both the public and secret key rings combined
func GetCombinedKeyRing() openpgp.KeyRing {
	return append(pubRing, secRing...)
}

// ErrNeedsPassphrase is what PromptFunc returns: a message that no already-decrypted secret key
// opens needs a passphrase, and zfsbackup never asks for one.
var ErrNeedsPassphrase = errors.New("needs a passphrase")

// PromptFunc is used to satisfy the openpgp package's requirements. openpgp calls it for a
// message no decrypted secret key opens: one sealed with a passphrase, or encrypted to a secret
// key that is still locked. It never decrypts anything.
func PromptFunc(keys []openpgp.Key, symmetric bool) ([]byte, error) {
	if len(keys) == 0 && symmetric {
		return nil, fmt.Errorf("is encrypted with a passphrase, not to a key: %w", ErrNeedsPassphrase)
	}
	ids := make([]string, 0, len(keys))
	for _, k := range keys {
		ids = append(ids, k.PublicKey.KeyIdString())
	}
	return nil, fmt.Errorf("is encrypted to locked secret key %s: %w", strings.Join(ids, ", "), ErrNeedsPassphrase)
}

func getKeyByEmail(keyring openpgp.EntityList, email string) *openpgp.Entity {
	for _, entity := range keyring {
		for _, ident := range entity.Identities {
			if ident.UserId.Email == email {
				return entity
			}
		}
	}

	return nil
}

// LoadPublicRing will open and parse the PGP keyring from the file path provided.
func LoadPublicRing(path string) error {
	pubringFile, err := os.Open(path)
	if err != nil {
		return err
	}
	pubRing, err = openpgp.ReadArmoredKeyRing(pubringFile)
	return err
}

// LoadPrivateRing will open and parse the PGP keyring from the file path provided.
func LoadPrivateRing(path string) error {
	privringFile, err := os.Open(path)
	if err != nil {
		return err
	}
	secRing, err = openpgp.ReadArmoredKeyRing(privringFile)

	return err
}

// PrintPGPDebugInformation will output a debug log entry listing the keys it has read in from each keyring.
func PrintPGPDebugInformation() {
	debugStr := make([]string, 0, 4)
	debugStr = append(debugStr, "PGP Debug Info:\nLoaded Private Keys:")
	for _, key := range secRing {
		debugStr = append(debugStr, fmt.Sprintf("\t%v\n\t%v", key.PrimaryKey.KeyIdString(), key.Identities))
	}

	debugStr = append(debugStr, "\nLoaded Public Keys:")
	for _, key := range pubRing {
		debugStr = append(debugStr, fmt.Sprintf("\t%v\n\t%v", key.PrimaryKey.KeyIdString(), key.Identities))
	}

	log.AppLogger.Debugf("%s", strings.Join(debugStr, "\n"))
}
