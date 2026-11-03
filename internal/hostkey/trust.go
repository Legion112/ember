package hostkey

import (
	"fmt"
	"net"

	"golang.org/x/crypto/ssh"
)

// Trust decides whether a presented host key may be used.
//
// check is unexported, so no type outside this package can satisfy Trust.
// That is the entire point: ember exposes no way to supply an
// ssh.HostKeyCallback, and this package contains no permissive
// implementation, so there is nowhere to write the insecure path.
//
// See TestTrustIsSealed, which fails if the method is ever exported.
type Trust interface {
	check(nodeID string, remote net.Addr, key ssh.PublicKey) error
}

// Pin verifies against a fingerprint ember already holds, and refuses every
// other key from the very first connection.
//
// It is **exported and concrete**, which is the point. Trust being an
// interface is what keeps ssh.InsecureIgnoreHostKey untypeable, and it is
// also what let the only other implementation in this package -- Store's
// trust-on-first-use -- be returned from provision.trustFor by a one-line
// change. That was ember's quietest bug: RunPod reports no host key, so
// reading the provider's answer first dropped the first real cloud provision
// into TOFU against a brand-new IP, which is the one case TOFU cannot defend.
// The regression tests written for it asserted that the log did not say
// "first use", and nothing in ember logs that string, so reinstating the
// defect kept a hundred labs and the whole unit suite green (#111).
//
// So trustFor and provision.Session.Trust are typed Pin rather than Trust:
// a trust that is not a pin is now a compile error on the path the bug was
// on, rather than a thing a test has to notice. Trust stays an interface
// because its own seal -- one unexported method, no permissive
// implementation -- is what makes the insecure callback unwritable, and
// because the dial legitimately takes either.
//
// The zero Pin refuses every key. That is deliberate: Pinned's error return
// used to hand back a nil Trust, which panics in Callback, and a caller who
// drops the error now gets the strictest possible answer instead of a crash
// -- the direction Caps.PublicSSH defaults in.
type Pin struct{ fp Fingerprint }

// Pinned verifies against a fingerprint ember already knows.
//
// This is the normal case, because ember mints the host key itself and injects
// it at create time (see Mint): the first connection is already authenticated
// and there is no trust-on-first-use window at all.
func Pinned(fp Fingerprint) (Pin, error) {
	if fp.IsZero() {
		// An unset fingerprint means the provider field was empty or the
		// caller forgot. Either way there is nothing to verify against, and
		// returning a Trust that accepts anything is how "pinned" becomes a
		// synonym for "ignored".
		return Pin{}, fmt.Errorf("refusing to build a pinned trust from an unset fingerprint")
	}
	return Pin{fp: fp}, nil
}

func (p Pin) check(nodeID string, remote net.Addr, key ssh.PublicKey) error {
	got := Of(key)
	if p.fp.Equal(got) {
		return nil
	}
	return &MismatchError{NodeID: nodeID, Remote: remote, Want: p.fp, Got: got}
}

// MismatchError is returned when a server presents the wrong host key.
//
// There is no --force and no prompt. A prompt is a bypass with extra steps,
// and for a node that lives twenty minutes the operator's correct response is
// always to destroy it rather than to reason about why its identity changed.
type MismatchError struct {
	NodeID string
	Remote net.Addr
	Want   Fingerprint
	Got    Fingerprint
}

func (e *MismatchError) Error() string {
	where := "unknown address"
	if e.Remote != nil {
		where = e.Remote.String()
	}
	return fmt.Sprintf(
		"host key mismatch for node %s at %s: expected %s, got %s — destroy the node (ember down %s) rather than connecting",
		e.NodeID, where, e.Want, e.Got, e.NodeID)
}

// Callback adapts a Trust to the shape x/crypto/ssh wants.
//
// It lives here because check is unexported, which is what keeps internal/sshdial
// — the only place a HostKeyCallback is ever assigned — unable to construct a
// permissive one even by accident.
func Callback(t Trust, nodeID string) ssh.HostKeyCallback {
	return func(_ string, remote net.Addr, key ssh.PublicKey) error {
		return t.check(nodeID, remote, key)
	}
}
