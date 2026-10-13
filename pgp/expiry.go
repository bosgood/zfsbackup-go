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
	"time"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/packet"
)

// KeyExpiry is a key, or a self-signature on it, that stops being valid at a set time.
type KeyExpiry struct {
	// Fingerprint is the primary key's or the subkey's, in upper-case hex.
	Fingerprint string
	// Subkey is true when Fingerprint is a subkey's.
	Subkey bool
	// At is when the key or its self-signature expires; it can be in the past.
	At time.Time
}

func (k KeyExpiry) String() string {
	kind := "key"
	if k.Subkey {
		kind = "subkey"
	}
	return fmt.Sprintf("%s %s expires %s", kind, k.Fingerprint, k.At.UTC().Format(time.RFC3339))
}

// Expiries lists the parts of e that expire: the primary key and each subkey, by their key
// lifetime or the lifetime of the self-signature that binds them. go-crypto checks both
// against the current time when it verifies a signature, so a volume signed by a key that
// has since expired fails to verify.
func Expiries(e *openpgp.Entity) []KeyExpiry {
	var out []KeyExpiry
	add := func(pk *packet.PublicKey, sig *packet.Signature, subkey bool) {
		if sig == nil {
			return
		}
		var at time.Time
		if sig.KeyLifetimeSecs != nil && *sig.KeyLifetimeSecs != 0 {
			at = pk.CreationTime.Add(time.Duration(*sig.KeyLifetimeSecs) * time.Second)
		}
		if sig.SigLifetimeSecs != nil && *sig.SigLifetimeSecs != 0 {
			sigAt := sig.CreationTime.Add(time.Duration(*sig.SigLifetimeSecs) * time.Second)
			if at.IsZero() || sigAt.Before(at) {
				at = sigAt
			}
		}
		if !at.IsZero() {
			out = append(out, KeyExpiry{Fingerprint: fmt.Sprintf("%X", pk.Fingerprint), Subkey: subkey, At: at})
		}
	}
	selfSig, _ := e.PrimarySelfSignature()
	add(e.PrimaryKey, selfSig, false)
	for _, sk := range e.Subkeys {
		add(sk.PublicKey, sk.Sig, true)
		// A signing subkey's binding carries the subkey's own back-signature.
		if sk.Sig != nil {
			add(sk.PublicKey, sk.Sig.EmbeddedSignature, true)
		}
	}
	return out
}
