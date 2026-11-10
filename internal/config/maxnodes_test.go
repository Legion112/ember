package config

import (
	"strings"
	"testing"
)

// The node ceiling that replaced an invariant (#204).
//
// Until #204 two rentals at once were unrepresentable; `max_nodes` is the
// bound that stands in its place, so both arms of its refusal and its default
// are worth exit codes of their own. `internal/config` is gated at 100%, which
// is the other reason every arm here is covered rather than argued.

// TestDefault_HoldsTwoNodes, which is a behaviour change for every existing
// operator and is therefore the arm worth asserting twice.
//
// A file that omits `max_nodes` keeps this, because yaml does not touch an
// absent field -- so the default is what every operator who never heard of the
// key gets, and it is 2 where their whole experience of ember has been one
// rental by construction.
func TestDefault_HoldsTwoNodes(t *testing.T) {
	if n := Default().MaxNodes; n != 2 {
		t.Errorf("Default().MaxNodes = %d, want 2: an operator who never writes the key gets "+
			"this number, and it is the ceiling that replaced the one-rental invariant", n)
	}

	// AND THROUGH Load, which is the half the default alone cannot prove: a
	// config that says nothing about nodes has to come back with 2 rather
	// than with whatever a decode over the default leaves.
	cfg, err := Load(write(t, "provider: local\nimage: ember-sd:1\nttl: 10m\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MaxNodes != 2 {
		t.Errorf("a file that omits max_nodes loaded %d, want 2", cfg.MaxNodes)
	}
}

// TestLoad_RefusesFewerThanOneNode.
//
// `max_nodes: 0` decodes 0 over the default, and 0 would read to an operator
// as "no ceiling at all" while meaning 1 to the queue, which clamps anything
// below 1. A negative is the same mistake with a minus sign. Both are refused
// where every other refusal of a VALUE in a file the operator just edited
// lives, so they find it while the line is still on screen.
//
// THE REMEDY IS NAMED, because an operator writing 0 is most likely reaching
// for the old single-rental guarantee, and `max_nodes: 1` is it.
func TestLoad_RefusesFewerThanOneNode(t *testing.T) {
	for _, body := range []string{
		"provider: local\nimage: ember-sd:1\nttl: 10m\nmax_nodes: 0\n",
		"provider: local\nimage: ember-sd:1\nttl: 10m\nmax_nodes: -1\n",
	} {
		_, err := Load(write(t, body))
		if err == nil {
			t.Errorf("a max_nodes below one was accepted:\n%s", body)
			continue
		}
		if !strings.Contains(err.Error(), "max_nodes: 1") {
			t.Errorf("the refusal does not name the remedy an operator reaching for the old "+
				"single-rental behaviour wants: %v\n%s", err, body)
		}
	}
}

// TestLoad_AMaxNodesWithNoValueKeepsTheDefault.
//
// `max_nodes:` with nothing after it is a null, which yaml leaves the default
// alone for -- so it keeps 2 rather than being refused as a zero. That is the
// honest reading of a key somebody has written and not yet decided about, and
// it is asserted because the refusal above is one `if` away from catching it:
// a check on PRESENCE rather than on the value would refuse the line that
// means nothing, which is the shape `min_cuda`'s own check had to be the
// opposite way round for.
func TestLoad_AMaxNodesWithNoValueKeepsTheDefault(t *testing.T) {
	cfg, err := Load(write(t, "provider: local\nimage: ember-sd:1\nttl: 10m\nmax_nodes:\n"))
	if err != nil {
		t.Fatalf("max_nodes with no value was refused: %v", err)
	}
	if cfg.MaxNodes != 2 {
		t.Errorf("max_nodes with no value loaded %d, want the default of 2", cfg.MaxNodes)
	}
}

// TestLoad_AcceptsOneNode, because a refusal alone is consistent with a check
// that refuses every value there is -- the zero-Pin lesson.
//
// One is the mode a cautious operator sets and the closest thing left to the
// invariant #204 traded away, so it has to be representable rather than merely
// not-refused by accident.
func TestLoad_AcceptsOneNode(t *testing.T) {
	cfg, err := Load(write(t, "provider: local\nimage: ember-sd:1\nttl: 10m\nmax_nodes: 1\n"))
	if err != nil {
		t.Fatalf("max_nodes: 1 was refused, and it is the one mode an operator who wants the "+
			"old guarantee will write: %v", err)
	}
	if cfg.MaxNodes != 1 {
		t.Errorf("max_nodes: 1 loaded %d", cfg.MaxNodes)
	}
}
