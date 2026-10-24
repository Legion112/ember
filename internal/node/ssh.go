package node

import (
	"fmt"
	"strings"
)

// Endpoint is where sshd answers on a node.
//
// sshd is the only thing ember ever dials, and on a correctly created node it
// is the only thing the provider exposes publicly at all. Everything else
// reaches the node through a forward inside this one connection.
type Endpoint struct {
	Host string
	Port uint16
	User string
}

// Validate rejects an Endpoint that cannot be dialled.
//
// Port 0 is the specific case worth naming: it is the zero value, it means
// "any port" to a listener, and it means "connection refused" to a dialler.
// A provider that has not yet published its SSH mapping returns 0 here, which
// is why Ready refuses to build a ReadyNode around it rather than letting the
// tunnel fail later with a less obvious message.
func (e Endpoint) Validate() error {
	if strings.TrimSpace(e.Host) == "" {
		return fmt.Errorf("ssh endpoint has no host")
	}
	if e.Port == 0 {
		return fmt.Errorf("ssh endpoint has no port: the provider has not published a mapping for 22/tcp yet")
	}
	if strings.TrimSpace(e.User) == "" {
		return fmt.Errorf("ssh endpoint has no user")
	}
	return nil
}

func (e Endpoint) String() string { return fmt.Sprintf("%s@%s:%d", e.User, e.Host, e.Port) }

// HostKeySource records how much ember knows about a node's host key.
//
// The zero value is HostKeyUnset, which means "nothing is pinned" and is the
// value that makes the strict trust policy refuse. Forgetting to populate this
// field therefore fails closed.
type HostKeySource uint8

const (
	// HostKeyUnset means ember has no key for this node yet.
	HostKeyUnset HostKeySource = iota

	// HostKeyMinted means ember generated the key and injected it at create
	// time, so the first connection is already authenticated. This is the
	// normal case and the reason ember does not rely on trust-on-first-use.
	HostKeyMinted

	// HostKeyFromAPI means the provider published the fingerprint. No
	// provider ember speaks to does this today; the case exists because it
	// costs one constant and it is the only path needing no injection.
	HostKeyFromAPI

	// HostKeyPinned means ember accepted a key it had never seen and recorded
	// it. A node in this state was never authenticated on its first
	// connection; ember reports it so the operator can see that without
	// reading logs.
	HostKeyPinned
)

var hostKeySourceNames = map[HostKeySource]string{
	HostKeyUnset:   "unset",
	HostKeyMinted:  "minted",
	HostKeyFromAPI: "from-api",
	HostKeyPinned:  "pinned",
}

func (s HostKeySource) String() string {
	if n, ok := hostKeySourceNames[s]; ok {
		return n
	}
	return fmt.Sprintf("hostkeysource(%d)", uint8(s))
}

// Authenticated reports whether the first connection to this node was proved
// rather than assumed.
func (s HostKeySource) Authenticated() bool {
	return s == HostKeyMinted || s == HostKeyFromAPI
}

// HostKey is a node's SSH host public key in the form ember stores it.
//
// Algo and Base64 are kept separate and never concatenated into an
// "ssh-ed25519 AAAA..." line in this package: a known_hosts line also carries
// a hostname field, and a type that is sometimes a key and sometimes a line is
// a type whose comparisons are sometimes wrong.
type HostKey struct {
	Source HostKeySource
	Algo   string
	Base64 string
}

// IsZero reports whether this HostKey carries no key material.
func (h HostKey) IsZero() bool { return h.Algo == "" && h.Base64 == "" }
