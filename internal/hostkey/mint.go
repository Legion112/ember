package hostkey

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"fmt"

	"golang.org/x/crypto/ssh"

	"github.com/Legion112/ember/internal/node"
)

// Pair is a host key ember generated for one node.
type Pair struct {
	// Private is the OpenSSH-format private key, injected into the node at
	// create time. Wrapped in node.Secret so logging it yields a fingerprint.
	Private node.Secret

	// Public is the matching public key.
	Public ssh.PublicKey
}

// Fingerprint identifies the pair.
func (p Pair) Fingerprint() Fingerprint { return Of(p.Public) }

// Mint generates a host key for a node ember is about to create.
//
// The private half travels to the node in the provider's API body and the
// entrypoint writes it to /etc/ssh/ssh_host_ed25519_key, so the very first
// connection is verified against a key ember already holds.
//
// Handing a host private key to the provider sounds worse than it is, and the
// trade is worth stating plainly: the provider supplies the kernel, the
// hypervisor and the container runtime, so it has root on that node already
// and could read /etc/ssh a second after sshd generated its own key. It learns
// nothing. What this buys is everyone *else* — anyone on the path to a
// brand-new IP that has never had a key before, which is exactly the position
// trust-on-first-use cannot defend.
func Mint() (Pair, error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return Pair{}, fmt.Errorf("generate host key: %w", err)
	}
	block, err := ssh.MarshalPrivateKey(priv, "ember-node")
	if err != nil {
		return Pair{}, fmt.Errorf("encode host key: %w", err)
	}
	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		return Pair{}, fmt.Errorf("derive host public key: %w", err)
	}
	return Pair{Private: node.NewSecret(pem.EncodeToMemory(block)), Public: sshPub}, nil
}
