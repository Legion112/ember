package genstore

import (
	"testing"
	"time"

	"github.com/Legion112/ember/internal/node"
)

// The create side of fit-or-create (#204), against both stores.
//
// `Compat`'s three fields answer "may this request ride that rental", which is
// the claim each rental's own loop makes. `Compat.Live` answers the
// complement -- "is there work NONE of the rentals already up can serve" --
// which is the only question a supervisor deciding whether to CREATE has to
// ask. Both are one statement, because a read followed by a write is the
// window ClaimNext exists to close.
//
// EVERY BODY RUNS AGAINST BOTH IMPLEMENTATIONS, which is this file's whole
// reason: the SQL spells the exclusion as one `AND NOT (...)` per live rental
// and `RecordingStore` spells it in Go, so two rules in two languages is
// exactly how `dataCenter` against `dataCenterId` stayed invisible.

// up is a rental that is already up and knows what card it got.
//
// THE CARD IS SET IN EVERY FIXTURE HERE, because that is the production state:
// Queue.advertise writes it one statement after Plan returns and before
// anything is rented, so a live rental with no card is a rental in the middle
// of an offer lookup. A fixture that left it unset would be asking the WIDER
// create-side question in every test below -- see
// TestClaimNext_ARentalStillChoosingItsOfferExcludesItsWholeKind, which is the
// one test that asks it on purpose.
func up(gpu, card string) Compat {
	return Compat{
		Kind: KindSD, Provider: "local", GPU: gpu,
		Card: node.GPUModel(card),
	}
}

// TestClaimNext_SkipsWorkALiveRentalCouldServe is strict packing.
//
// A request a rental already up can take is NOT given a node of its own, even
// while that rental is mid-batch: a fitting rental skips the ~27s per-rental
// overhead and, for a resident model, a reload measured at 80-96s per image.
// So this is the measurement turned into a claim predicate, and the
// counter-intuitive consequence is the assertion -- a homogeneous queue gets
// no second node however many slots are free.
func TestClaimNext_SkipsWorkALiveRentalCouldServe(t *testing.T) {
	each(t, func(t *testing.T, s Store) {
		rideable := req("the live rental's own card")
		rideable.Created = time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC) // oldest

		other := req("a card nothing up has")
		other.GPU = "h100-80g"
		other.Created = time.Date(2026, 11, 1, 12, 1, 0, 0, time.UTC)

		if _, err := s.Enqueue(t.Context(), rideable); err != nil {
			t.Fatal(err)
		}
		idOther, err := s.Enqueue(t.Context(), other)
		if err != nil {
			t.Fatal(err)
		}

		// One rental is up on rtx2080ti. The create-side claim must skip the
		// OLDER request -- which that rental will absorb through its own ride
		// claim -- and take the one nothing up can serve.
		live := []Compat{up("rtx2080ti", "rtx2080ti")}
		got, ok, err := s.ClaimNext(t.Context(), "run-2", Compat{Live: live})
		if err != nil || !ok {
			t.Fatalf("a create-side claim found nothing for a card nothing up has: ok=%v err=%v",
				ok, err)
		}
		if got.ID != idOther {
			t.Errorf("claimed %s, want %s: a request the live rental can serve was given a "+
				"node of its own, which pays the per-rental overhead twice for work one node "+
				"was about to do", got.ID, idOther)
		}

		// AND NOTHING IS LEFT FOR A THIRD NODE, which is the half that makes
		// packing strict rather than merely preferred: with both rentals up,
		// the rideable request still waits for the one that can serve it.
		live = append(live, up("h100-80g", "h100-80g"))
		if _, ok, err := s.ClaimNext(t.Context(), "run-3", Compat{Live: live}); err != nil || ok {
			t.Errorf("a third node was claimed for work a live rental can serve: ok=%v err=%v",
				ok, err)
		}
	})
}

// TestClaimNext_IsExcludedByOneLiveRentalAndNotAnother, because an exclusion
// that fired for every live rental would refuse every create there is.
//
// This is the assertion that stops the clause above being satisfied by "take
// nothing whenever anything is up": with a rental on another card up, the
// request for THIS card is still claimable.
func TestClaimNext_IsExcludedByOneLiveRentalAndNotAnother(t *testing.T) {
	each(t, func(t *testing.T, s Store) {
		mine := req("rtx2080ti")
		id, err := s.Enqueue(t.Context(), mine)
		if err != nil {
			t.Fatal(err)
		}

		live := []Compat{up("h100-80g", "h100-80g")}
		got, ok, err := s.ClaimNext(t.Context(), "run-1", Compat{Live: live})
		if err != nil || !ok {
			t.Fatalf("a rental on a different card excluded this request: ok=%v err=%v", ok, err)
		}
		if got.ID != id {
			t.Errorf("claimed %s, want %s", got.ID, id)
		}
	})
}

// TestClaimNext_AMigratedRowIsExcludedByAnSDRental, which is the expensive
// direction of the empty-means-sd rule.
//
// A row written before schema version 6 holds `kind = ”` and a compat built
// from one holds `'sd'`. On the RIDE side, comparing the raw column refuses a
// migrated row to the rental serving its siblings -- a permanently
// unclaimable queue, which TestClaimNext_AMigratedRowHasNoKindAndIsStillClaimable
// covers. On the EXCLUSION side it fails to exclude one, so the supervisor
// gives a migrated row a SECOND CARD for work the node already up was about to
// do. Same rule, same KindOf, and this is the half with the money in it.
func TestClaimNext_AMigratedRowIsExcludedByAnSDRental(t *testing.T) {
	each(t, func(t *testing.T, s Store) {
		migrated := req("written before the kind column existed")
		migrated.Kind = ""
		id, err := s.Enqueue(t.Context(), migrated)
		if err != nil {
			t.Fatal(err)
		}

		// ASSERTED IN ITS OWN BODY, because what this test turns on is that
		// the row really has no kind: a fixture that quietly set one would
		// make every assertion below pass against a raw comparison, which is
		// the shape a tidy-up of `req()` already disarmed once in this
		// package.
		stored, err := s.Get(t.Context(), id)
		if err != nil {
			t.Fatal(err)
		}
		if stored.Kind != "" {
			t.Fatalf("the seeded row has kind %q, so this test cannot see the defect it is "+
				"for: a raw comparison would agree with KindOf on it", stored.Kind)
		}

		live := []Compat{up("rtx2080ti", "rtx2080ti")}
		if _, ok, err := s.ClaimNext(t.Context(), "run-2", Compat{Live: live}); err != nil || ok {
			t.Errorf("an sd rental did not exclude a migrated row, so it gets a card of its "+
				"own while the node already up was about to serve it: ok=%v err=%v", ok, err)
		}

		// And a tts rental does not exclude it, which is what stops the
		// normalisation above being "exclude everything".
		live = []Compat{{Kind: KindTTS, Provider: "local", GPU: "rtx2080ti",
			Card: node.GPUModel("rtx2080ti")}}
		got, ok, err := s.ClaimNext(t.Context(), "run-3", Compat{Live: live})
		if err != nil || !ok {
			t.Fatalf("a speech rental excluded an image request: ok=%v err=%v", ok, err)
		}
		if got.ID != id {
			t.Errorf("claimed %s, want %s", got.ID, id)
		}
	})
}

// TestClaimNext_NoLiveRentalsIsTheOldTakeAnythingClaim, so that every existing
// caller of `Compat{}` keeps its meaning.
//
// `Live: nil` has to be exactly what `Compat{}` was before this field existed:
// the oldest queued request whatever it asked for. Otherwise the field would
// be a behaviour change for every claim in the tree rather than an addition.
func TestClaimNext_NoLiveRentalsIsTheOldTakeAnythingClaim(t *testing.T) {
	each(t, func(t *testing.T, s Store) {
		oldest := req("oldest")
		oldest.GPU = "h100-80g"
		oldest.Created = time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
		id, err := s.Enqueue(t.Context(), oldest)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.Enqueue(t.Context(), req("newer")); err != nil {
			t.Fatal(err)
		}

		for _, c := range []Compat{{}, {Live: nil}, {Live: []Compat{}}} {
			got, ok, err := s.ClaimNext(t.Context(), "run-1", c)
			if err != nil || !ok {
				t.Fatalf("%+v claimed nothing: ok=%v err=%v", c, ok, err)
			}
			if got.ID != id {
				t.Errorf("%+v claimed %s, want the oldest %s", c, got.ID, id)
			}
			// Put it back, so each spelling is asked the same question.
			if err := s.Update(t.Context(), id, func(r *Request) {
				r.Status = StatusQueued
				r.RunID = ""
			}); err != nil {
				t.Fatal(err)
			}
		}
	})
}
