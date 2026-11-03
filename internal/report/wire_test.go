package report

import (
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

// optionalFields reads Event's own tags.
//
// FROM THE TREE AND NOT A LIST BESIDE IT, which is the rule
// TestEveryBuildTagReachesTheGates follows one layer up: a hand-written list
// of the fields that are supposed to be optional cannot catch the field
// somebody adds, and the field somebody adds is exactly how this defect
// arrived. reflect walks whatever Event has today.
func optionalFields(t *testing.T) map[string]string {
	t.Helper()
	out := map[string]string{}
	ty := reflect.TypeFor[Event]()
	for i := range ty.NumField() {
		f := ty.Field(i)
		parts := strings.Split(f.Tag.Get("json"), ",")
		if parts[0] == "" || parts[0] == "-" {
			t.Fatalf("%s has no json name; every field on the wire needs one", f.Name)
		}
		if slices.Contains(parts[1:], "omitempty") || slices.Contains(parts[1:], "omitzero") {
			out[parts[0]] = f.Name
		}
	}
	if len(out) == 0 {
		t.Fatal("no optional fields found; the tag reader is broken, not the struct")
	}
	return out
}

// TestEvent_AnOptionalFieldIsAbsentWhenNobodySetIt.
//
// THE TAG IS THE MECHANISM THIS STRUCT USES TO SAY WHICH FIELDS A KIND MEANS.
// Event is flat with omitempty rather than a payload interface per kind --
// one serde type on the other side instead of fifteen -- and the doc comment
// leans on the tag for the cost of that: "fields that are meaningless for
// most kinds, which the Kind documents and the renderer enforces". For
// `deadline` the mechanism was not running at all, because omitempty has no
// effect on a struct, so every event ember emits carried
// "deadline":"0001-01-01T00:00:00Z" -- including the 224 job_progress rows of
// a FLUX.2 batch of eight, in the table this repository calls the source of
// truth.
//
// Nothing could tell the two tags apart before this. internal/report's tests
// assert on Text's output, which never reads Deadline; web/ has no deadline
// field and serde ignores unknown keys; genstore stores the body opaquely. So
// no test in either language had an opinion about the marshalled shape of
// this struct, and Rate's tag working and Deadline's not working looked the
// same.
//
// Over Kinds() as well as over the fields, because a kind list derived from
// the tree is what TestTextRendersEveryKind already does and the same list
// settles the other half of the claim: that the only event which names a
// deadline is the one the field was written for.
func TestEvent_AnOptionalFieldIsAbsentWhenNobodySetIt(t *testing.T) {
	optional := optionalFields(t)
	if _, ok := optional["deadline"]; !ok {
		t.Fatal("deadline is no longer tagged optional; this test is about that tag")
	}

	for _, k := range Kinds() {
		t.Run(string(k), func(t *testing.T) {
			keys := marshalKeys(t, Event{Kind: k})
			for name, field := range optional {
				if _, present := keys[name]; present {
					t.Errorf("%s (%s) is tagged optional and nobody set it, "+
						"yet %q is on the wire as %s -- omitempty does nothing for a struct",
						field, name, name, keys[name])
				}
			}
			// Seq, At and Kind are NOT optional and must be there: a test
			// that only looked for absences would pass against a marshaller
			// that dropped everything.
			for _, name := range []string{"seq", "at", "kind"} {
				if _, present := keys[name]; !present {
					t.Errorf("%q is absent; it carries no omitempty on purpose", name)
				}
			}
		})
	}
}

// TestEvent_TheOneKindThatMeansADeadlineStillSendsIt.
//
// The other direction, and the reason the fix is a tag rather than a deletion:
// a node_ready carries the deadline and the rate because they are what an
// operator watching a rental needs -- how long they have, and what it is
// costing while they have it. A tag that omitted a set value would be the
// same defect with the sign flipped.
func TestEvent_TheOneKindThatMeansADeadlineStillSendsIt(t *testing.T) {
	deadline := time.Date(2026, 11, 1, 13, 0, 0, 0, time.UTC)
	keys := marshalKeys(t, Event{
		Kind: KindNodeReady, Node: "runpod/abc", Endpoint: "root@1.2.3.4:22",
		Deadline: deadline, Rate: 1790000,
	})
	for _, name := range []string{"deadline", "rate_micro_usd_per_hour"} {
		if _, present := keys[name]; !present {
			t.Errorf("%q is absent from a node_ready that set it", name)
		}
	}
	if got := string(keys["deadline"]); got != `"2026-11-01T13:00:00Z"` {
		t.Errorf("deadline = %s", got)
	}
}

func marshalKeys(t *testing.T, e Event) map[string]json.RawMessage {
	t.Helper()
	b, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(b, &keys); err != nil {
		t.Fatalf("decode %s: %v", b, err)
	}
	return keys
}
