package node

import "testing"

// TestState_BillableIsTrueForEverythingButDestroyed is the money-safety claim
// of this type. Each row names why the state is billable, because each one is
// a way an operator has been surprised by a charge.
func TestState_BillableIsTrueForEverythingButDestroyed(t *testing.T) {
	for _, tc := range []struct {
		state State
		want  bool
		why   string
	}{
		{StateInvalid, true, "nobody set it, so nobody knows"},
		{StatePending, true, "the provider may already have reserved hardware"},
		{StateProvisioning, true, "billing usually starts at allocation, not at ready"},
		{StateReady, true, "obviously"},
		{StateStopped, true, "a stopped pod still bills for its volume"},
		{StateInterrupted, true, "the instance exists until it is destroyed"},
		{StateFailed, true, "a failed start may still have been charged"},
		{StateDestroyed, false, "the only state that is provably free"},
	} {
		if got := tc.state.Billable(); got != tc.want {
			t.Errorf("%v.Billable() = %v, want %v (%s)", tc.state, got, tc.want, tc.why)
		}
	}
}

// TestState_UnknownStateIsBillable guards the default for a state added later:
// the author has to think about it to make it free, not to make it cost money.
func TestState_UnknownStateIsBillable(t *testing.T) {
	future := State(200)
	if !future.Billable() {
		t.Fatal("a State this package does not define must be assumed to cost money")
	}
	if future.Valid() {
		t.Fatal("an undefined State must not report Valid")
	}
	if got, want := future.String(), "state(200)"; got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}
}

func TestState_Terminal(t *testing.T) {
	terminal := map[State]bool{
		StateDestroyed: true, StateFailed: true, StateInterrupted: true,
	}
	for s := StateInvalid; s <= StateDestroyed; s++ {
		if got, want := s.Terminal(), terminal[s]; got != want {
			t.Errorf("%v.Terminal() = %v, want %v", s, got, want)
		}
	}
}

func TestState_ValidRejectsTheZeroValue(t *testing.T) {
	if StateInvalid.Valid() {
		t.Fatal("the zero value must never be Valid, or a State nobody set reads as a State somebody chose")
	}
	for s := StatePending; s <= StateDestroyed; s++ {
		if !s.Valid() {
			t.Errorf("%v should be Valid", s)
		}
	}
}

func TestParseState_RoundTripsEveryDefinedState(t *testing.T) {
	for s := StatePending; s <= StateDestroyed; s++ {
		got, err := ParseState(s.String())
		if err != nil {
			t.Fatalf("ParseState(%q): %v", s.String(), err)
		}
		if got != s {
			t.Errorf("ParseState(%q) = %v, want %v", s.String(), got, s)
		}
	}
}

func TestParseState_RefusesUnknownAndInvalid(t *testing.T) {
	for _, in := range []string{"", "invalid", "RUNNING", "running", "destroyed "} {
		if got, err := ParseState(in); err == nil {
			t.Errorf("ParseState(%q) = %v, want an error", in, got)
		}
	}
}
