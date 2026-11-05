package genstore

import (
	"testing"
	"time"

	"github.com/Legion112/ember/internal/tts"
)

// TestWorkloadOf_ResolvesBothAbsences, which is the one place that knows what
// an empty workload and an empty kind mean.
//
// THREE SPELLINGS FOR ONE THING, and the way that cost is paid once is that
// nothing else compares against `""`. A row written before the kind column
// holds ” for both; a row written between schema 6 and 7 holds a kind and no
// workload; a row written since holds the entry id the request named. All
// three name the same entry when the operator's file declares no `workloads:`,
// because the implicit entries are named after their own drivers.
func TestWorkloadOf_ResolvesBothAbsences(t *testing.T) {
	cases := []struct {
		workload, kind, want string
	}{
		{"", "", KindSD},
		{"", KindSD, KindSD},
		{"", KindTTS, KindTTS},
		{"image", "", "image"},
		{"speech", KindTTS, "speech"},
		// THE WORKLOAD WINS OVER THE KIND, which is what makes two entries
		// sharing a driver two rentals: the id identifies an image and the
		// driver does not.
		{"flux", KindSD, "flux"},
		{"sd15", KindSD, "sd15"},
	}
	for _, c := range cases {
		if got := WorkloadOf(c.workload, c.kind); got != c.want {
			t.Errorf("WorkloadOf(%q, %q) = %q, want %q", c.workload, c.kind, got, c.want)
		}
	}
}

// TestClaimNext_TwoWorkloadsSharingADriverAreTwoRentals.
//
// THE DEFECT A PER-ENTRY IMAGE OPENS, and the reason `Compat` is keyed on the
// entry rather than on the driver. A declared file may hold two `sd` entries
// pointing at different images -- one FLUX, one SD1.5 -- and a Compat keyed on
// the driver matches them: the provider and the GPU agree, the claim succeeds,
// and the drain loop hands a request for one model to a resident server
// holding the other, after a card has been paid for.
//
// BOTH DIRECTIONS, because a refusal alone is consistent with a Compat that
// refuses every rider there is -- which would make the resident server worth
// nothing.
func TestClaimNext_TwoWorkloadsSharingADriverAreTwoRentals(t *testing.T) {
	each(t, func(t *testing.T, s Store) {
		one := Request{
			Kind: KindSD, Workload: "flux", Provider: "local", GPU: "rtx4090",
			TTL: time.Hour, Jobs: []Job{{Prompt: "a"}},
		}
		two := Request{
			Kind: KindSD, Workload: "sd15", Provider: "local", GPU: "rtx4090",
			TTL: time.Hour, Jobs: []Job{{Prompt: "b"}},
		}
		three := Request{
			Kind: KindSD, Workload: "flux", Provider: "local", GPU: "rtx4090",
			TTL: time.Hour, Jobs: []Job{{Prompt: "c"}},
		}
		for _, r := range []Request{one, two, three} {
			if _, err := s.Enqueue(t.Context(), r); err != nil {
				t.Fatal(err)
			}
		}

		got, ok, err := s.ClaimNext(t.Context(), "lease", Compat{})
		if err != nil || !ok {
			t.Fatalf("the first claim: %v, %v", ok, err)
		}
		if got.Workload != "flux" {
			t.Fatalf("the first claim took %q", got.Workload)
		}
		compat := Compat{
			Workload: WorkloadOf(got.Workload, got.Kind),
			Kind:     KindOf(got.Kind), Provider: got.Provider, GPU: got.GPU,
		}

		// THE OTHER flux ROW RIDES, which is the saving the resident server
		// exists for: the same entry means the same image and the same model.
		rider, ok, err := s.ClaimNext(t.Context(), "lease", compat)
		if err != nil || !ok {
			t.Fatalf("a second request for the same workload did not ride: %v, %v", ok, err)
		}
		if rider.Jobs[0].Prompt != "c" {
			t.Errorf("the rider is %q, want the other flux row", rider.Jobs[0].Prompt)
		}

		// AND THE sd15 ROW DOES NOT, although its driver, provider and card
		// all match.
		if _, ok, err := s.ClaimNext(t.Context(), "lease", compat); err != nil {
			t.Fatal(err)
		} else if ok {
			t.Error("a request for another workload with the same driver rode this rental; " +
				"it would reach a resident server holding another model")
		}
	})
}

// TestClaimNext_AMigratedRowHasNoWorkloadAndIsStillClaimable.
//
// THE UPGRADE, which is the direction a column added to a `WHERE` breaks. A
// plain `AND workload = ?` would refuse every row written before schema
// version 7 to any rental at all -- they hold ” and a compat built from one
// holds an entry id -- so a restart after an upgrade would leave an operator's
// queue permanently unclaimable.
//
// IT BUILDS THE ABSENCE ITSELF AND ASSERTS IT, rather than resting on a
// fixture that happens not to set the field: that is the accident
// `TestClaimNext_AMigratedRowHasNoKindAndIsStillClaimable` was written to
// remove one column over, and a shared fixture gaining a `Workload:` would
// otherwise switch this off silently.
//
// THE REFUSAL IS ASSERTED FIRST, because an acceptance alone is consistent
// with a claim that takes everything -- which is exactly what a compat
// ignoring the workload is.
func TestClaimNext_AMigratedRowHasNoWorkloadAndIsStillClaimable(t *testing.T) {
	each(t, func(t *testing.T, s Store) {
		legacy := Request{
			// A row an older ember wrote: a driver, and no entry id.
			Kind: KindTTS, Provider: "local", TTL: time.Hour,
			Jobs: []Job{{TTS: &tts.Ask{Text: "hello", VoiceID: "lessac", Output: "g"}}},
		}
		id, err := s.Enqueue(t.Context(), legacy)
		if err != nil {
			t.Fatal(err)
		}
		back, err := s.Get(t.Context(), id)
		if err != nil {
			t.Fatal(err)
		}
		if back.Workload != "" {
			t.Fatalf("the row carries workload %q; this test is about the absence", back.Workload)
		}

		// An image rental does not take it, which is the invariant
		// `Compat.Kind` was added for and which the entry id has to keep.
		if _, ok, err := s.ClaimNext(t.Context(), "l", Compat{
			Workload: KindSD, Kind: KindSD, Provider: "local",
		}); err != nil {
			t.Fatal(err)
		} else if ok {
			t.Fatal("a migrated speech row was claimed by an image rental")
		}

		// AND A SPEECH RENTAL DOES, because WorkloadOf resolves the absence to
		// the entry named after its own driver.
		got, ok, err := s.ClaimNext(t.Context(), "l", Compat{
			Workload: KindTTS, Kind: KindTTS, Provider: "local",
		})
		if err != nil {
			t.Fatal(err)
		}
		if !ok {
			t.Fatal("a migrated speech row was claimable by nothing; an upgraded queue would " +
				"sit there for ever")
		}
		if got.ID != id {
			t.Errorf("claimed %q, want %q", got.ID, id)
		}
	})
}
