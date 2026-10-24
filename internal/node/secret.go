package node

import (
	"crypto/sha256"
	"encoding/base64"
	"log/slog"
)

// Secret carries key material so that logging it yields a fingerprint.
//
// It implements slog.LogValuer, which is what makes this structural rather
// than a convention. One slog.String("hostkey", string(pem)) at debug level
// writes a private key into the journal permanently, and nothing about that
// line looks wrong in review — not to the author, not to the reviewer, and not
// in a diff. A type that cannot print itself removes the possibility instead
// of relying on everyone noticing.
//
// String() is deliberately not the raw value either, because fmt.Sprintf("%v")
// and a stray %s in an error are the other two ways key material escapes.
type Secret struct {
	b []byte
}

// NewSecret wraps b. The caller must not retain or mutate b afterwards.
func NewSecret(b []byte) Secret { return Secret{b: b} }

// Bytes returns the wrapped material.
//
// Named Bytes rather than String so that every unwrapping is visible at the
// call site: an audit for places key material is handled greps for ".Bytes()"
// and finds all of them.
func (s Secret) Bytes() []byte { return s.b }

// IsZero reports whether s carries nothing.
func (s Secret) IsZero() bool { return len(s.b) == 0 }

// Fingerprint is the SHA-256 of the material, in the form OpenSSH prints.
func (s Secret) Fingerprint() string {
	if len(s.b) == 0 {
		return "SHA256:<empty>"
	}
	sum := sha256.Sum256(s.b)
	return "SHA256:" + base64.RawStdEncoding.EncodeToString(sum[:])
}

func (s Secret) String() string       { return s.Fingerprint() }
func (s Secret) LogValue() slog.Value { return slog.StringValue(s.Fingerprint()) }
