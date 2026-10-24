package node

import (
	"testing"
	"time"
)

func TestNewRunID_ProducesSomethingParseIDAccepts(t *testing.T) {
	seen := map[RunID]bool{}
	for range 64 {
		id := NewRunID()
		if _, err := ParseRunID(string(id)); err != nil {
			t.Fatalf("NewRunID produced %q, which ParseRunID rejects: %v", id, err)
		}
		if seen[id] {
			t.Fatalf("NewRunID repeated %q", id)
		}
		seen[id] = true
	}
}

// TestParseRunID_IsStrict: a label ember did not write must not parse as one it
// did. reconcile reports unparseable-but-ember-looking things as findings, but
// a loose parser would let somebody else's container be classified as an ember
// orphan — and `ember reap` destroys orphans.
func TestParseRunID_IsStrict(t *testing.T) {
	for _, in := range []string{
		"", "abc", "0123456789abcde", "0123456789abcdef0", "0123456789abcdeg", "zzzzzzzzzzzzzzzz",
	} {
		if got, err := ParseRunID(in); err == nil {
			t.Errorf("ParseRunID(%q) = %q, want an error", in, got)
		}
	}
}

func TestParseRunID_NormalisesCase(t *testing.T) {
	got, err := ParseRunID("0123456789ABCDEF")
	if err != nil {
		t.Fatal(err)
	}
	if got != "0123456789abcdef" {
		t.Errorf("ParseRunID did not lowercase: %q", got)
	}
	if got.String() != string(got) {
		t.Error("String() must round-trip")
	}
}

func newRequestInputs(t *testing.T) (Spec, Offer, RunID, time.Time, string, Secret, HostKey) {
	t.Helper()
	s, err := NewSpec(validSpecOptions())
	if err != nil {
		t.Fatal(err)
	}
	return s, validOffer(), RunID("0123456789abcdef"),
		time.Date(2026, 10, 24, 11, 0, 0, 0, time.UTC),
		"ssh-ed25519 AAAAC3Nz operator", NewSecret([]byte(fakeKey)),
		HostKey{Source: HostKeyMinted, Algo: "ssh-ed25519", Base64: "AAAAC3Nz"}
}

// TestNewCreateRequest_DerivesTheDeadlineInOnePlace. The deadline is written
// into the provider's label, the local state record and the node's own
// self-destruct timer. Three copies computed at three call sites are three
// copies that can disagree, and the one that disagrees is the one that does
// not stop the bill.
func TestNewCreateRequest_DerivesTheDeadlineInOnePlace(t *testing.T) {
	s, o, id, now, key, hk, pubk := newRequestInputs(t)
	req, err := NewCreateRequest(s, o, id, now, key, hk, pubk)
	if err != nil {
		t.Fatal(err)
	}
	if want := now.Add(s.TTL); !req.Deadline.Equal(want) {
		t.Errorf("Deadline = %v, want %v", req.Deadline, want)
	}
	if req.RunID != id {
		t.Errorf("RunID = %q", req.RunID)
	}
	if req.HostKey.IsZero() {
		t.Error("the host key did not survive")
	}
}

func TestNewCreateRequest_Refuses(t *testing.T) {
	type mut func(*Spec, *Offer, *RunID, *time.Time, *string, *Secret, *HostKey)
	for name, m := range map[string]mut{
		"zero ttl":           func(s *Spec, _ *Offer, _ *RunID, _ *time.Time, _ *string, _ *Secret, _ *HostKey) { s.TTL = 0 },
		"over-max ttl":       func(s *Spec, _ *Offer, _ *RunID, _ *time.Time, _ *string, _ *Secret, _ *HostKey) { s.TTL = MaxTTL + 1 },
		"bad offer":          func(_ *Spec, o *Offer, _ *RunID, _ *time.Time, _ *string, _ *Secret, _ *HostKey) { o.Token = "" },
		"bad run id":         func(_ *Spec, _ *Offer, r *RunID, _ *time.Time, _ *string, _ *Secret, _ *HostKey) { *r = "nope" },
		"zero now":           func(_ *Spec, _ *Offer, _ *RunID, n *time.Time, _ *string, _ *Secret, _ *HostKey) { *n = time.Time{} },
		"no authorized key":  func(_ *Spec, _ *Offer, _ *RunID, _ *time.Time, k *string, _ *Secret, _ *HostKey) { *k = "  " },
		"no host key":        func(_ *Spec, _ *Offer, _ *RunID, _ *time.Time, _ *string, h *Secret, _ *HostKey) { *h = Secret{} },
		"no host public key": func(_ *Spec, _ *Offer, _ *RunID, _ *time.Time, _ *string, _ *Secret, k *HostKey) { *k = HostKey{} },
	} {
		s, o, id, now, key, hk, pubk := newRequestInputs(t)
		m(&s, &o, &id, &now, &key, &hk, &pubk)
		if _, err := NewCreateRequest(s, o, id, now, key, hk, pubk); err == nil {
			t.Errorf("%s: NewCreateRequest accepted it", name)
		}
	}
}
