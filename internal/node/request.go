package node

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"time"
)

// RunID names one ember run.
//
// It is the thread tying a local state record to a node at a provider: it goes
// into the provider's label, and reconcile parses it back out. That is what
// lets ember find a node it created even when the local record was lost, so
// the id has to survive being put into a container label, a RunPod tag and a
// Vast instance name — hence hex and nothing else.
type RunID string

// NewRunID returns a fresh random RunID.
//
// It returns no error because crypto/rand.Read is documented never to return
// one: it always fills the buffer entirely or the program is already in a
// state where nothing else matters. An error return that cannot fire is worse
// than none — every call site grows a branch that can never be exercised, and
// an unexercised branch is one nobody has read.
func NewRunID() RunID {
	var b [8]byte
	rand.Read(b[:])
	return RunID(hex.EncodeToString(b[:]))
}

// ParseRunID validates a RunID read back from a label or a state file.
//
// Strict rather than permissive: a label ember did not write must not parse as
// one it did, or reconcile will report somebody else's container as an ember
// orphan and `ember reap` will destroy it.
func ParseRunID(s string) (RunID, error) {
	if len(s) != 16 {
		return "", fmt.Errorf("run id %q is not 16 hex characters", s)
	}
	if _, err := hex.DecodeString(s); err != nil {
		return "", fmt.Errorf("run id %q is not hex: %w", s, err)
	}
	return RunID(strings.ToLower(s)), nil
}

func (r RunID) String() string { return string(r) }

// CreateRequest is what a provider is asked to build.
//
// NewCreateRequest is the only constructor, so Deadline is always TTL after
// the instant the request was made. Deriving it in one place is the point: the
// deadline is written into the provider's label, into the local state record
// and into the node's own self-destruct timer, and three copies computed at
// three call sites are three copies that can disagree.
type CreateRequest struct {
	Spec     Spec
	Offer    Offer
	RunID    RunID
	Deadline time.Time

	// AuthorizedKey is the operator's SSH public key in authorized_keys form.
	// Public material: it is safe in a label, a log line or an API body.
	AuthorizedKey string

	// HostKey is the private half of the host key ember minted for this node,
	// injected so that the first connection is already authenticated rather
	// than trusted on sight. Wrapped in Secret so it cannot be logged.
	HostKey Secret
}

// NewCreateRequest validates the pieces and derives the deadline.
func NewCreateRequest(s Spec, o Offer, runID RunID, now time.Time, authorizedKey string, hostKey Secret) (CreateRequest, error) {
	if s.TTL <= 0 || s.TTL > MaxTTL {
		return CreateRequest{}, fmt.Errorf("spec ttl %v is outside (0, %v]", s.TTL, MaxTTL)
	}
	if err := o.Validate(); err != nil {
		return CreateRequest{}, err
	}
	if _, err := ParseRunID(string(runID)); err != nil {
		return CreateRequest{}, err
	}
	if now.IsZero() {
		return CreateRequest{}, fmt.Errorf("creation time is zero: the deadline would be meaningless")
	}
	if strings.TrimSpace(authorizedKey) == "" {
		return CreateRequest{}, fmt.Errorf("no authorized key: a node nobody can log into still bills")
	}
	if hostKey.IsZero() {
		return CreateRequest{}, fmt.Errorf("no host key to inject: without one the first connection cannot be authenticated")
	}
	return CreateRequest{
		Spec:          s,
		Offer:         o,
		RunID:         runID,
		Deadline:      now.Add(s.TTL),
		AuthorizedKey: authorizedKey,
		HostKey:       hostKey,
	}, nil
}
