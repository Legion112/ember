// Package keys holds the operator's SSH identity.
//
// One long-lived key, not one per node. Both providers register authorized
// keys per account rather than per machine, so a per-node key means an extra
// registration call on create and a cleanup call on destroy — and destroy
// fails sometimes (API timeout, exhausted credit, provider outage). Every
// failure would leak a registered key into the account permanently, in
// exchange for revocation granularity that is worthless on a machine which
// lives twenty minutes.
package keys

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/crypto/ssh"

	"github.com/Legion112/ember/internal/node"
)

// DefaultName is the file ember generates inside its config directory.
const DefaultName = "id_ed25519"

// Identity is the operator's key: the signer ember authenticates with, and the
// public half in the form a provider's API wants.
type Identity struct {
	Signer ssh.Signer

	// AuthorizedKey is the "ssh-ed25519 AAAA... ember" line. Public material,
	// safe in an API body, a label or a log.
	AuthorizedKey string

	// Path is where the private half lives, or "" when the identity came from
	// an agent and ember never saw the key.
	Path string
}

// Fingerprint is the SHA256 fingerprint of the public key.
func (i Identity) Fingerprint() string { return ssh.FingerprintSHA256(i.Signer.PublicKey()) }

// EnsureIdentity loads the key at dir/DefaultName, generating it on first use.
//
// dir is created with mode 0700 and the key with 0600. Both are checked rather
// than assumed on every load: see LoadSigner.
func EnsureIdentity(dir string) (Identity, error) {
	path := filepath.Join(dir, DefaultName)

	switch _, err := os.Stat(path); {
	case err == nil:
		return LoadIdentity(path)
	case !errors.Is(err, os.ErrNotExist):
		return Identity{}, fmt.Errorf("stat %s: %w", path, err)
	}

	if err := os.MkdirAll(dir, 0o700); err != nil { //nolint:gosec // 0700 is the tightest mode a directory can have and still be usable
		return Identity{}, fmt.Errorf("create %s: %w", dir, err)
	}
	// MkdirAll leaves an existing directory's mode alone, so a config
	// directory created earlier by something more permissive would silently
	// keep that mode and the key inside it would be readable.
	if err := os.Chmod(dir, 0o700); err != nil { //nolint:gosec // 0700 is the tightening, not a loosening
		return Identity{}, fmt.Errorf("tighten %s: %w", dir, err)
	}

	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return Identity{}, fmt.Errorf("generate key: %w", err)
	}
	block, err := ssh.MarshalPrivateKey(priv, "ember")
	if err != nil {
		return Identity{}, fmt.Errorf("encode key: %w", err)
	}

	// O_EXCL rather than a plain create: two embers starting at once must not
	// both decide the key is missing and have the loser overwrite the
	// winner's key after the winner has already registered it with a
	// provider.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // path is ember's own config location or one the operator named
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return LoadIdentity(path)
		}
		return Identity{}, fmt.Errorf("create %s: %w", path, err)
	}
	if _, err := f.Write(pem.EncodeToMemory(block)); err != nil {
		_ = f.Close()
		return Identity{}, fmt.Errorf("write %s: %w", path, err)
	}
	if err := f.Close(); err != nil {
		return Identity{}, fmt.Errorf("close %s: %w", path, err)
	}
	return LoadIdentity(path)
}

// LoadIdentity reads an existing private key and builds an Identity.
func LoadIdentity(path string) (Identity, error) {
	signer, err := LoadSigner(path)
	if err != nil {
		return Identity{}, err
	}
	return Identity{
		Signer:        signer,
		AuthorizedKey: AuthorizedKey(signer.PublicKey()),
		Path:          path,
	}, nil
}

// AuthorizedKey renders pub as a single authorized_keys line, comment included.
func AuthorizedKey(pub ssh.PublicKey) string {
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(pub))) + " ember"
}

// LoadSigner reads an SSH private key and refuses it unless only the owner can
// read it.
//
// Refusing rather than warning, because ssh(1) and sshd both refuse these and
// an ember that accepted one would leave the operator believing a key readable
// by every process on a shared box was protected.
//
// The parent directory is checked too: when a directory is world-writable the
// mode on the file inside it proves nothing, since anyone can replace the file
// with their own and ember would authenticate to the operator's nodes with it.
func LoadSigner(path string) (ssh.Signer, error) {
	// The directory is checked first. Checking the file first made the
	// directory branch unreachable — a missing directory already fails
	// stat(path) — which is a branch that can never run and therefore one
	// nobody has read.
	dir := filepath.Dir(path)
	di, err := os.Stat(dir)
	if err != nil {
		return nil, fmt.Errorf("stat %s: %w", dir, err)
	}
	if m := di.Mode().Perm(); m&0o002 != 0 {
		return nil, fmt.Errorf("directory %s is world-writable (mode %04o), so the mode on the key inside it proves nothing: anyone can replace it", dir, m)
	}

	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("stat %s: %w", path, err)
	}
	if m := info.Mode().Perm(); m&0o077 != 0 {
		return nil, fmt.Errorf("private key %s has mode %04o; it must not be readable by group or others (chmod 600 %s)", path, m, path)
	}

	raw, err := os.ReadFile(path) //nolint:gosec // the path is ember's own config location or one the operator named
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	// raw is key material from here on; node.Secret exists so that anything
	// logging it prints a fingerprint.
	secret := node.NewSecret(raw)

	signer, err := ssh.ParsePrivateKey(secret.Bytes())
	if err != nil {
		var missing *ssh.PassphraseMissingError
		if errors.As(err, &missing) {
			// ember never prompts for or decrypts a passphrase. The
			// passphrase and the decrypted key then never enter this
			// program's address space, so there is no handling code here to
			// get wrong; ssh-agent already solves it properly.
			return nil, fmt.Errorf("private key %s is passphrase-protected; ember does not decrypt keys — add it to your agent (ssh-add %s) and ember will use SSH_AUTH_SOCK", path, path)
		}
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return signer, nil
}
