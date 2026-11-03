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
// also what let the only other implementation this package ever had -- the
// deleted Store's trust-on-first-use, see the note below -- be returned from
// provision.trustFor by a one-line change. That was ember's quietest bug:
// RunPod reports no host key, so reading the provider's answer first dropped
// the first real cloud provision into TOFU against a brand-new IP, which is
// the one case TOFU cannot defend. The regression tests written for it
// asserted that the log did not say "first use", and nothing in ember logs
// that string, so reinstating the defect kept a hundred labs and the whole
// unit suite green (#111).
//
// So trustFor and provision.Session.Trust are typed Pin rather than Trust:
// a trust that is not a pin is now a compile error on the path the bug was
// on, rather than a thing a test has to notice. Trust stays an interface
// because its own seal -- one unexported method, no permissive
// implementation -- is what makes the insecure callback unwritable, and
// because Callback has to take something.
//
// Pin is now the ONLY implementation, which is what
// TestTrustHasOneImplementationAndItIsPin keeps true. Narrowing sshdial.Dial
// and session.Dial to Pin as well was considered and refused: it would move
// #117's compile error off the provisioning path, where the defect was, and
// onto the dial, where a provider that publishes its fingerprint out of band
// would one day need the signature widened again -- and widening a signature
// is how #111 arrived in the first place.
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

// There is deliberately no persisted pin store. `hostkey.Store` was one --
// OpenStore, Get, Forget, TrustOnFirstUse, a versioned JSON file at
// `<state_dir>/known_hosts.json` and an atomic saveLocked behind it -- and it
// is deleted having never written a file in production (#120).
//
// WHY, because "it was unused" is not the test. The test is whether a second
// live mechanism exists for the thing it would do, and the thing it would do
// is carry a pin for one node from one process to the next:
//
//   - THERE IS NO CONSUMER. The only production dial in the tree is
//     internal/app's dialNode, reached only from inside provision.WithNode's
//     closure, and it is handed a Pin that trustFor built in memory from the
//     key WithNode minted seconds earlier in the same process. Nothing read a
//     pin by id: Store.Get had no caller outside the store's own tests, and
//     its waiver in cmd/check-coverage said so in writing -- "for a reader
//     that does not exist yet".
//   - NO FLOW RE-DIALS A NODE ACROSS PROCESSES, INCLUDING THE NEW ONE. #146
//     made `ember up` a client of `ember serve` over a unix socket, which
//     looks like the case that would need this and is not: internal/cli's
//     upclient names neither hostkey nor sshdial nor ssh, and the rental --
//     and therefore the dial -- happens in the server process, inside
//     queue.Direct.Run calling WithNode. The process that mints is the
//     process that dials, in every shape ember has.
//   - THE SECOND MECHANISM IS Pinned, AND IT IS THE LIVE ONE. An attach-to-a-
//     running-node verb, if one is ever written, rebuilds its pin with
//     FromNodeKey off what the provider reports and checks it against the
//     minted key -- which trustFor already does. A file could not have helped
//     it, because a Store pin could only ever be a TOFU pin: tofu.check was
//     the only writer. So the file could hold only keys ember never
//     authenticated, and making one of those authoritative for every later
//     process is worse than having no file at all. A pin that dies with the
//     node has nothing to gain from outliving the process.
//
// Measured rather than read, so that this note is not a reading of the code:
// with saveLocked made to panic, exactly two packages in the whole unit suite
// reached it -- internal/hostkey and internal/tunnel/sshdial, both from their
// own test files -- and internal/provision, internal/app and every other
// package stayed green. And TrustOnFirstUse's waiver kept it "for a provider
// that genuinely cannot accept an injected key", which is a shape ember
// cannot represent: node.NewCreateRequest refuses a zero HostKey, so there is
// no create request to build for such a provider. Dropping that refusal turns
// node.TestNewCreateRequest_Refuses red at "no host key: NewCreateRequest
// accepted it".
//
// The deletion is a security strengthening and not a loosening. tofu was the
// one implementation of Trust in this package that accepts a key it has never
// seen, so fingerprint.go's "this package contains no permissive
// implementation" was not literally true for the life of the package; #117's
// return type kept it off the renting path, and this makes the sentence true.
//
// THE THREE JUDGEMENTS THE DELETED CODE CARRIED are the reason this is a note
// rather than nothing, because whoever revives persistence has to make them
// again:
//
//   - KEYED BY NODE ID, NEVER BY host:port. A pin keyed by address matches
//     whoever is given that address next, which on a provider recycling them
//     between customers is a stranger -- and the churn that produces, a
//     warning on every new node, is exactly what talks people into turning
//     host key checking off.
//   - WRITE-THEN-RENAME, AND REFUSE A FILE FROM A NEWER EMBER. Both are the
//     same failure: a crash halfway through an in-place write leaves a
//     truncated file, and reading a newer layout as an older one drops pins
//     silently. A pin file that will not parse, or that quietly lost a pin,
//     re-arms trust-on-first-use for every node in it without anybody being
//     told -- which is the one outcome a pin file must not have.
//   - THE FILE'S MODE IS A CONTROL AND NOT HYGIENE. It was 0600 inside a
//     0700 directory. A fingerprint is public material, so the mode is not
//     about secrecy: anyone who can WRITE that file makes their own key
//     authoritative for a node ember will dial, which is the MITM the pin
//     exists to refuse, arriving through the filesystem instead of the
//     network. loopback's socket directory carries the same argument.
//
// And the question a reviver has to answer before any of it: where does the
// pin come from. If the answer is "the first key we saw", this is TOFU again
// and the node-id key is the only thing keeping it honest. If the answer is
// "the key ember minted", the private half never outlives the process that
// minted it and the public half is already on the provider's own record, so
// FromNodeKey is the mechanism and no file is needed.

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
