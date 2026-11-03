// Package hostkey decides whether a node's SSH host key may be used.
//
// The decision is sealed. Trust has a single unexported method and this
// package contains no permissive implementation, so ssh.InsecureIgnoreHostKey
// cannot be adapted into it — not behind a flag, not behind a config key, not
// "temporarily" in a test. Shipping an SSH client with no server
// authentication is the most common way this goes wrong, and "we grep for it
// in review" has a hundred per cent failure rate eventually.
//
// "No permissive implementation" is checked rather than asserted here, by
// TestTrustHasOneImplementationAndItIsPin -- because for the life of this
// package the sentence was not literally true: the deleted Store's
// trust-on-first-use accepted a key it had never seen, and the only thing
// keeping it off the path that rents machines was provision.trustFor's return
// type. See the note where it was, in trust.go.
package hostkey

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"

	"golang.org/x/crypto/ssh"
)

// Fingerprint identifies a host key.
//
// It is a struct and never a string. A string fingerprint has a zero value
// that compares equal to nothing and is easy to wave through on an
// `if fp != ""` somebody later inverts; a zero Fingerprint makes Pinned return
// an error instead, so an unpopulated provider field becomes a refusal rather
// than a bypass.
type Fingerprint struct {
	algo   string
	sha256 [32]byte
	set    bool
}

// Of computes the fingerprint of key.
func Of(key ssh.PublicKey) Fingerprint {
	return Fingerprint{algo: key.Type(), sha256: sha256.Sum256(key.Marshal()), set: true}
}

// There is deliberately no parser for the form String writes.
// ParseFingerprint was one, and its only non-test caller was the deleted
// Store's OpenStore, reading the pin file back: a fingerprint reaches ember
// as a key to hash (Of) or as a provider's base64 blob (FromNodeKey), and
// String exists for messages an operator reads rather than for a round trip.
// A reviver of the pin file needs it again, and the note in trust.go is where
// to read first.

// IsZero reports whether f identifies nothing.
func (f Fingerprint) IsZero() bool { return !f.set }

// Algo is the key type, for example "ssh-ed25519".
func (f Fingerprint) Algo() string { return f.algo }

func (f Fingerprint) String() string {
	if !f.set {
		return "<unset>"
	}
	return f.algo + " SHA256:" + base64.RawStdEncoding.EncodeToString(f.sha256[:])
}

// Equal reports whether f and g identify the same key.
//
// The comparison is constant-time. The timing of a host-key comparison is not
// a realistic oracle, but a variable-time compare in the one function whose
// whole job is deciding whether to trust a server is the kind of detail that
// is cheap now and awkward to argue about later.
func (f Fingerprint) Equal(g Fingerprint) bool {
	if !f.set || !g.set || f.algo != g.algo {
		return false
	}
	return subtle.ConstantTimeCompare(f.sha256[:], g.sha256[:]) == 1
}
