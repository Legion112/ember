// Package statestore records every node ember created, on disk, before it
// exists.
//
// It is a cache and not the authority — the provider's own label is that, so
// an orphan stays findable after this directory is lost — but it is the only
// thing that can see the window between asking a provider to create a machine
// and learning its id. A crash in that window otherwise leaves an H100
// billing by the hour with no trace on this machine at all.
package statestore

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Legion112/ember/internal/node"
)

// recordVersion is the on-disk shape. A file written by a newer ember is
// refused rather than misparsed: reading a newer layout as an older one drops
// fields, and the field it would drop is the one that says a node still exists.
const recordVersion = 1

// Phase is how far a node got.
type Phase string

const (
	// PhaseIntent is written *before* the create call. A record stuck here
	// means ember asked for a machine and never learned whether it got one,
	// which is the one state that must be checked against the provider.
	PhaseIntent Phase = "intent"

	PhaseCreated Phase = "created"
	PhaseReady   Phase = "ready"

	// PhaseDestroyed is terminal and the only phase that is free.
	PhaseDestroyed Phase = "destroyed"

	// PhaseDestroyFailed is sticky. It is never cleared automatically,
	// because the node may still be running and the operator is the only one
	// who can confirm otherwise.
	PhaseDestroyFailed Phase = "destroy_failed"
)

// Billable reports whether a record in this phase may still cost money.
func (p Phase) Billable() bool { return p != PhaseDestroyed }

// Record is what ember knows locally about one run.
type Record struct {
	Version  int             `json:"version"`
	RunID    node.RunID      `json:"run_id"`
	Provider node.ProviderID `json:"provider"`
	NodeID   string          `json:"node_id,omitempty"` // empty until Create returns
	Phase    Phase           `json:"phase"`
	Label    string          `json:"label"`
	Created  time.Time       `json:"created"`
	Deadline time.Time       `json:"deadline"`
	Offer    string          `json:"offer,omitempty"`
	Price    node.Rate       `json:"price_micro_usd_per_hour,omitempty"`
	LastErr  string          `json:"last_error,omitempty"`
}

// Handle reports the provider handle this record names, if it has one yet.
func (r Record) Handle() (node.Handle, bool) {
	if r.NodeID == "" {
		return node.Handle{}, false
	}
	return node.Handle{Provider: r.Provider, ID: r.NodeID}, true
}

// Store is a directory of records, one JSON file per run.
type Store struct {
	dir string
	mu  sync.Mutex
}

// Open returns a store rooted at dir. The directory is created lazily.
func Open(dir string) (*Store, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, errors.New("state directory is empty")
	}
	return &Store{dir: dir}, nil
}

// Dir reports where records are kept.
func (s *Store) Dir() string { return s.dir }

func (s *Store) path(id node.RunID) string {
	return filepath.Join(s.dir, string(id)+".json")
}

// Create writes a new record.
//
// O_EXCL, so two embers starting at once cannot both claim one run id and have
// the loser overwrite the winner's node id after the winner already created a
// machine with it.
func (s *Store) Create(r Record) error {
	if _, err := node.ParseRunID(string(r.RunID)); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := os.MkdirAll(s.dir, 0o700); err != nil { //nolint:gosec // 0700 is the tightest usable mode
		return fmt.Errorf("create %s: %w", s.dir, err)
	}
	r.Version = recordVersion

	raw, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return fmt.Errorf("encode record %s: %w", r.RunID, err)
	}
	f, err := os.OpenFile(s.path(r.RunID), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // ember's own state dir
	if err != nil {
		return fmt.Errorf("create record %s: %w", r.RunID, err)
	}
	if _, err := f.Write(raw); err != nil {
		_ = f.Close()
		return fmt.Errorf("write record %s: %w", r.RunID, err)
	}
	return f.Close()
}

// Update applies mutate to the record and rewrites it atomically.
func (s *Store) Update(id node.RunID, mutate func(*Record)) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	r, err := s.readLocked(id)
	if err != nil {
		return err
	}
	mutate(&r)
	r.Version = recordVersion
	return s.writeLocked(r)
}

// Get returns one record.
func (s *Store) Get(id node.RunID) (Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.readLocked(id)
}

// List returns every record, oldest first.
func (s *Store) List() ([]Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	entries, err := os.ReadDir(s.dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", s.dir, err)
	}

	var out []Record
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".json") {
			continue
		}
		id, err := node.ParseRunID(strings.TrimSuffix(name, ".json"))
		if err != nil {
			// A file ember did not write. Skipping it is right — this
			// directory is ember's, but an editor backup or a stray file must
			// not stop `ember ls` from reporting the real records.
			continue
		}
		r, err := s.readLocked(id)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Created.Before(out[j].Created) })
	return out, nil
}

// Forget removes a record.
//
// Only ever called after a confirmed destroy. A record in PhaseDestroyFailed
// is not forgotten automatically, because the machine it names may still be
// running and only the operator can say otherwise.
func (s *Store) Forget(id node.RunID) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	err := os.Remove(s.path(id))
	if errors.Is(err, os.ErrNotExist) {
		return nil // idempotent, like destroy
	}
	if err != nil {
		return fmt.Errorf("forget %s: %w", id, err)
	}
	return nil
}

func (s *Store) readLocked(id node.RunID) (Record, error) {
	raw, err := os.ReadFile(s.path(id)) //nolint:gosec // ember's own state dir
	if err != nil {
		return Record{}, fmt.Errorf("read record %s: %w", id, err)
	}
	var r Record
	if err := json.Unmarshal(raw, &r); err != nil {
		return Record{}, fmt.Errorf("parse record %s: %w", id, err)
	}
	if r.Version > recordVersion {
		return Record{}, fmt.Errorf("record %s was written by a newer ember (version %d, this build understands %d); upgrade rather than let it drop fields", id, r.Version, recordVersion)
	}
	return r, nil
}

func (s *Store) writeLocked(r Record) error {
	raw, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return fmt.Errorf("encode record %s: %w", r.RunID, err)
	}
	if err := os.MkdirAll(s.dir, 0o700); err != nil { //nolint:gosec // 0700 is the tightest usable mode
		return fmt.Errorf("create %s: %w", s.dir, err)
	}

	// Write-then-rename: a crash midway through an in-place write leaves a
	// truncated record, and a record that fails to parse is a node ember can
	// no longer account for.
	tmp, err := os.CreateTemp(s.dir, ".rec-*")
	if err != nil {
		return fmt.Errorf("create a temporary file in %s: %w", s.dir, err)
	}
	name := tmp.Name()
	defer func() { _ = os.Remove(name) }()

	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("tighten %s: %w", name, err)
	}
	if _, err := tmp.Write(raw); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write %s: %w", name, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close %s: %w", name, err)
	}
	if err := os.Rename(name, s.path(r.RunID)); err != nil {
		return fmt.Errorf("install record %s: %w", r.RunID, err)
	}
	return nil
}
