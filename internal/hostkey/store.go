package hostkey

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"

	"golang.org/x/crypto/ssh"
)

// storeVersion is the on-disk shape. A file written by a future ember is
// refused rather than misparsed: silently reading a newer layout as an older
// one would drop pins, and a dropped pin re-arms trust-on-first-use without
// anybody being told.
const storeVersion = 1

type storeFile struct {
	Version int               `json:"version"`
	Pins    map[string]string `json:"pins"` // node id -> "algo SHA256:..."
}

// Store remembers the host key ember first saw for a node.
//
// Pins are keyed by **node id, not host:port**. An IP-keyed pin matches
// whoever is given that IP next, which on a provider recycling addresses
// between customers is a stranger; and the churn that produces — a warning on
// every new node — is exactly what talks people into turning host key checking
// off. A node-id pin is created with the node and deleted with it.
type Store struct {
	path string

	mu   sync.Mutex
	pins map[string]Fingerprint
}

// OpenStore reads the pin file at path, creating nothing until a pin is made.
func OpenStore(path string) (*Store, error) {
	s := &Store{path: path, pins: map[string]Fingerprint{}}

	raw, err := os.ReadFile(path) //nolint:gosec // ember's own state path
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}

	var f storeFile
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if f.Version > storeVersion {
		return nil, fmt.Errorf("%s was written by a newer ember (version %d, this build understands %d); upgrade rather than let it drop pins", path, f.Version, storeVersion)
	}
	for id, line := range f.Pins {
		fp, err := ParseFingerprint(line)
		if err != nil {
			return nil, fmt.Errorf("%s: pin for node %s: %w", path, id, err)
		}
		s.pins[id] = fp
	}
	return s, nil
}

// Get returns the pin recorded for nodeID.
func (s *Store) Get(nodeID string) (Fingerprint, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fp, ok := s.pins[nodeID]
	return fp, ok
}

// Forget removes the pin for nodeID. Called when the node is destroyed, so the
// pin dies with the thing it identifies.
func (s *Store) Forget(nodeID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.pins[nodeID]; !ok {
		return nil
	}
	delete(s.pins, nodeID)
	return s.saveLocked()
}

// TrustOnFirstUse returns a Trust that records the first key it sees for
// nodeID and refuses any later change.
//
// This is the fallback for a provider offering no way to inject a host key.
// It cannot authenticate the first connection — nothing can, against an
// address that has never had a key — so callers record node.HostKeyPinned and
// ember reports those nodes differently.
func (s *Store) TrustOnFirstUse(nodeID string) Trust { return tofu{store: s, nodeID: nodeID} }

type tofu struct {
	store  *Store
	nodeID string
}

func (t tofu) check(nodeID string, remote net.Addr, key ssh.PublicKey) error {
	got := Of(key)

	t.store.mu.Lock()
	defer t.store.mu.Unlock()

	if have, ok := t.store.pins[t.nodeID]; ok {
		if have.Equal(got) {
			return nil
		}
		return &MismatchError{NodeID: nodeID, Remote: remote, Want: have, Got: got}
	}
	t.store.pins[t.nodeID] = got
	return t.store.saveLocked()
}

// saveLocked writes the pin file atomically. The caller holds s.mu.
func (s *Store) saveLocked() error {
	f := storeFile{Version: storeVersion, Pins: make(map[string]string, len(s.pins))}
	for id, fp := range s.pins {
		f.Pins[id] = fp.String()
	}
	raw, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return fmt.Errorf("encode pins: %w", err)
	}

	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}

	// Write-then-rename rather than write-in-place: a crash midway through an
	// in-place write leaves a truncated file, and a pin file that fails to
	// parse on the next run is a pin file that silently re-arms
	// trust-on-first-use for every node in it.
	tmp, err := os.CreateTemp(dir, ".pins-*")
	if err != nil {
		return fmt.Errorf("create a temporary file in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()

	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("tighten %s: %w", tmpName, err)
	}
	if _, err := tmp.Write(raw); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write %s: %w", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close %s: %w", tmpName, err)
	}
	if err := os.Rename(tmpName, s.path); err != nil {
		return fmt.Errorf("install %s: %w", s.path, err)
	}
	return nil
}
