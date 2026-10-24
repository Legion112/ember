// Package hostkey decides whether a node's SSH host key may be used.
//
// The decision is sealed. Trust has a single unexported method and this
// package contains no permissive implementation, so ssh.InsecureIgnoreHostKey
// cannot be adapted into it — not behind a flag, not behind a config key, not
// "temporarily" in a test. Shipping an SSH client with no server
// authentication is the most common way this goes wrong, and "we grep for it
// in review" has a hundred per cent failure rate eventually.
package hostkey

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"strings"

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

// ParseFingerprint reads the form String writes: "ssh-ed25519 SHA256:base64".
func ParseFingerprint(s string) (Fingerprint, error) {
	algo, rest, ok := strings.Cut(strings.TrimSpace(s), " ")
	if !ok {
		return Fingerprint{}, fmt.Errorf("fingerprint %q has no algorithm; write it as \"ssh-ed25519 SHA256:...\"", s)
	}
	b64, ok := strings.CutPrefix(rest, "SHA256:")
	if !ok {
		return Fingerprint{}, fmt.Errorf("fingerprint %q is not a SHA256 fingerprint", s)
	}
	raw, err := base64.RawStdEncoding.DecodeString(b64)
	if err != nil {
		return Fingerprint{}, fmt.Errorf("fingerprint %q: %w", s, err)
	}
	if len(raw) != sha256.Size {
		return Fingerprint{}, fmt.Errorf("fingerprint %q is %d bytes, want %d", s, len(raw), sha256.Size)
	}
	var f Fingerprint
	f.algo = algo
	copy(f.sha256[:], raw)
	f.set = true
	return f, nil
}

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
