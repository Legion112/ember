package genstore

import (
	"testing"
	"time"

	"github.com/Legion112/ember/internal/node"
	"github.com/Legion112/ember/internal/sdserver"
	"github.com/Legion112/ember/internal/tts"
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
		Card: node.GPUModel(card), CardKnown: true,
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
			Card: node.GPUModel("rtx2080ti"), CardKnown: true}}
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

// TestClaimNext_ARequestThatNamedNoCardRidesAnything.
//
// THE FIRST CARD ADMISSION, and the one that needs no advertised fact. An
// empty `gpu` means "whatever this provider has" -- Narrow writes it straight
// over the config's own selection -- so every live rental of the right kind
// and provider can serve it. Matching ask against ask made it ride only a
// rental that ALSO named nothing, which is a second node for a request that
// said it did not care.
func TestClaimNext_ARequestThatNamedNoCardRidesAnything(t *testing.T) {
	each(t, func(t *testing.T, s Store) {
		anyCard := req("whatever you have")
		anyCard.GPU = ""
		id, err := s.Enqueue(t.Context(), anyCard)
		if err != nil {
			t.Fatal(err)
		}

		// The ride side: a rental that asked for an rtx4090 and got one takes
		// it on its own drain loop.
		got, ok, err := s.ClaimNext(t.Context(), "run-1", up("rtx4090", "rtx4090"))
		if err != nil || !ok {
			t.Fatalf("a rental could not take a request that named no card: ok=%v err=%v", ok, err)
		}
		if got.ID != id {
			t.Fatalf("claimed %s, want %s", got.ID, id)
		}

		// AND THE CREATE SIDE AGREES, which is the half that spends money: a
		// request a live rental will absorb must not be given a node of its
		// own. Put back first, because the ride claim above took it.
		if err := s.Update(t.Context(), id, func(r *Request) {
			r.Status, r.RunID = StatusQueued, ""
		}); err != nil {
			t.Fatal(err)
		}
		live := []Compat{up("rtx4090", "rtx4090")}
		if _, ok, err := s.ClaimNext(t.Context(), "run-2", Compat{Live: live}); err != nil || ok {
			t.Errorf("a request that named no card was given a node of its own while a rental "+
				"it could ride was up: ok=%v err=%v", ok, err)
		}
	})
}

// TestClaimNext_ACardTheRentalAskedForNothingAboutStillPacks.
//
// THE THIRD CARD ADMISSION, and the only one that reads Compat.Card. A rental
// opened by a request that named no card lands on whatever the provider had;
// a later request naming exactly that card can ride it, and before the card
// was advertised it could not -- the two asks differ, so ember rented a second
// node for a card it already had up.
//
// THE NEGATIVE IS IN THE SAME BODY, because an admission that fired for every
// card would be satisfied by the first assertion alone: a request for a
// DIFFERENT card is still given its own node.
func TestClaimNext_ACardTheRentalAskedForNothingAboutStillPacks(t *testing.T) {
	each(t, func(t *testing.T, s Store) {
		wants := req("specifically an rtx4090")
		wants.GPU = "rtx4090"
		id, err := s.Enqueue(t.Context(), wants)
		if err != nil {
			t.Fatal(err)
		}
		elsewhere := req("specifically an h100")
		elsewhere.GPU = "h100-80g"
		other, err := s.Enqueue(t.Context(), elsewhere)
		if err != nil {
			t.Fatal(err)
		}

		// A rental that asked for nothing and got an rtx4090.
		live := []Compat{up("", "rtx4090")}
		got, ok, err := s.ClaimNext(t.Context(), "run-1", Compat{Live: live})
		if err != nil || !ok {
			t.Fatalf("the create-side claim found nothing at all: ok=%v err=%v", ok, err)
		}
		if got.ID == id {
			t.Fatalf("the rtx4090 request was given a node of its own while a rental holding " +
				"that exact card was up; its ask and the rental's differ, the silicon does not")
		}
		if got.ID != other {
			t.Fatalf("claimed %s, want %s", got.ID, other)
		}

		// And the ride side takes it, which is where it has to end up or the
		// exclusion above has starved it.
		rode, ok, err := s.ClaimNext(t.Context(), "run-1", up("", "rtx4090"))
		if err != nil || !ok {
			t.Fatalf("the rental holding an rtx4090 would not take the request asking for one: "+
				"ok=%v err=%v", ok, err)
		}
		if rode.ID != id {
			t.Errorf("rode %s, want %s", rode.ID, id)
		}
	})
}

// TestClaimNext_ACapacitySuffixIsTheRuleNodeAlreadyOwns.
//
// node.GPUModel.Accepts admits a card whose name is the ask plus a capacity
// suffix: an operator types `a100` and every offer carries `a100-80g`, which is
// the defect NormalizeGPU's own comment records costing a filter every offer
// there was. The claim predicate has to admit the same thing or a request
// typed the way the filter documents rides nothing.
//
// AND THE WRONG DIRECTION IS REFUSED: a request naming `a100-80g` does not
// ride a rental holding a plain `a100`, because the suffix makes the ask MORE
// specific and the rental has not got it.
func TestClaimNext_ACapacitySuffixIsTheRuleNodeAlreadyOwns(t *testing.T) {
	each(t, func(t *testing.T, s Store) {
		loose := req("an a100 of any size")
		loose.GPU = "a100"
		id, err := s.Enqueue(t.Context(), loose)
		if err != nil {
			t.Fatal(err)
		}

		got, ok, err := s.ClaimNext(t.Context(), "run-1", up("", "a100-80g"))
		if err != nil || !ok {
			t.Fatalf("a rental on an a100-80g would not serve a request asking for an a100: "+
				"ok=%v err=%v", ok, err)
		}
		if got.ID != id {
			t.Fatalf("claimed %s, want %s", got.ID, id)
		}

		strict := req("an a100 with 80 gigabytes")
		strict.GPU = "a100-80g"
		if _, err := s.Enqueue(t.Context(), strict); err != nil {
			t.Fatal(err)
		}
		if _, ok, err := s.ClaimNext(t.Context(), "run-2", up("", "a100")); err != nil || ok {
			t.Errorf("a rental on a plain a100 took a request that asked for 80 gigabytes: "+
				"ok=%v err=%v", ok, err)
		}
	})
}

// TestClaimNext_TheRideRelationAndTheExclusionAreOneRelation.
//
// THE PROPERTY, NOT A CASE. Every assertion above is about one pairing; this
// one is about the two sides agreeing for all of them, which is what makes
// fit-or-create a partition rather than two conditions that happen to line up.
// Drift either way is expensive and neither direction fails loudly: an
// exclusion WIDER than the ride relation leaves a request waiting out a whole
// rental that will never claim it, and one NARROWER hands it a card of its
// own while a node that could serve it is idle.
//
// READ OFF THE STORE AND NOT OFF THE PREDICATE, so it holds the SQL and the
// Go spelling to each other rather than holding one of them to itself: for
// each pairing it asks the rental to ride, puts the row back, and asks the
// supervisor to exclude. The two answers must be opposite, every time.
func TestClaimNext_TheRideRelationAndTheExclusionAreOneRelation(t *testing.T) {
	// A matrix that covers all three card admissions, both kinds, a second
	// provider and the capacity-suffix rule in both directions.
	asks := []string{"", "rtx4090", "a100", "a100-80g", "h100-80g"}
	rentals := []Compat{
		up("", ""),
		up("", "rtx4090"),
		up("rtx4090", "rtx4090"),
		up("a100", "a100-80g"),
		up("a100-80g", "a100-80g"),
		{Kind: KindTTS, Provider: "local", GPU: "", Card: node.GPUModel("rtx4090"), CardKnown: true},
		{Kind: KindSD, Provider: "runpod", GPU: "", Card: node.GPUModel("rtx4090"), CardKnown: true},
	}

	each(t, func(t *testing.T, s Store) {
		for _, kind := range []string{KindSD, KindTTS, ""} {
			for _, ask := range asks {
				r := req("one")
				r.Kind = kind
				r.GPU = ask
				if kind == KindTTS {
					r.Jobs = []Job{{TTS: &tts.Ask{Text: "hello", VoiceID: "v1"}}}
				}
				id, err := s.Enqueue(t.Context(), r)
				if err != nil {
					t.Fatal(err)
				}
				for _, l := range rentals {
					_, rode, err := s.ClaimNext(t.Context(), "ride", l)
					if err != nil {
						t.Fatal(err)
					}
					if rode {
						requeue(t, s, id)
					}
					// claimable is whether the SUPERVISOR would still create a
					// node for this row with that rental up, so it has to be
					// the complement of rode.
					_, claimable, err := s.ClaimNext(t.Context(), "create", Compat{Live: []Compat{l}})
					if err != nil {
						t.Fatal(err)
					}
					if claimable {
						requeue(t, s, id)
					}
					if rode == claimable {
						t.Errorf("kind=%q ask=%q against %+v: the rental rides it=%v and the "+
							"supervisor would still create for it=%v -- the two sides of "+
							"fit-or-create disagree, which either starves this request or buys "+
							"it a second card", kind, ask, l, rode, claimable)
					}
				}
				// Done with this row: take it out of the queue so the next
				// pairing starts from one row again.
				if err := s.Update(t.Context(), id, func(q *Request) { q.Status = StatusDone }); err != nil {
					t.Fatal(err)
				}
			}
		}
	})
}

// requeue puts a claimed row back, so one fixture can be asked several
// questions.
func requeue(t *testing.T, s Store, id ReqID) {
	t.Helper()
	if err := s.Update(t.Context(), id, func(r *Request) {
		r.Status, r.RunID = StatusQueued, ""
	}); err != nil {
		t.Fatal(err)
	}
}

// TestCaps_AreKeyedByShapeAndOneShapeDoesNotAnswerForAnother.
//
// THE DEFECT WAS ONE ROW PER PROCESS. It was written by whichever rental came
// up last and read to refuse a job for whatever shape asked next, so the
// sampler list of card A was used to validate a request naming card B --
// accepted for one and refused for the other depending on which had answered
// most recently, with nothing anywhere reporting it. A capability list is
// allowed to differ with the card, which is the whole reason `images:` maps an
// architecture to a build.
//
// THE MISS IS ASSERTED AS WELL AS THE HIT, because a cache that answered
// everything with the last document is exactly what this replaced, and a
// test that only checked the hit would pass against it.
func TestCaps_AreKeyedByShapeAndOneShapeDoesNotAnswerForAnother(t *testing.T) {
	each(t, func(t *testing.T, s Store) {
		cheap := NewShape(KindSD, "runpod", "rtx2080ti")
		dear := NewShape(KindSD, "runpod", "rtx-pro-6000")

		if err := s.PutCaps(t.Context(), cheap, sdserver.Capabilities{
			Model: "sd15.gguf", Samplers: []string{"euler"},
		}); err != nil {
			t.Fatal(err)
		}
		if err := s.PutCaps(t.Context(), dear, sdserver.Capabilities{
			Model: "flux2-dev-Q8_0.gguf", Samplers: []string{"euler", "dpm++2m"},
		}); err != nil {
			t.Fatal(err)
		}

		// Each shape gets its own answer: the second write must not have
		// overwritten the first, which is what the single row did.
		got, ok, err := s.Caps(t.Context(), cheap)
		if err != nil || !ok {
			t.Fatalf("Caps for the first shape = %v, %v", ok, err)
		}
		if got.Model != "sd15.gguf" || len(got.Samplers) != 1 {
			t.Errorf("the second rental's capabilities answered the first shape: %+v", got)
		}
		got, ok, err = s.Caps(t.Context(), dear)
		if err != nil || !ok {
			t.Fatalf("Caps for the second shape = %v, %v", ok, err)
		}
		if got.Model != "flux2-dev-Q8_0.gguf" {
			t.Errorf("capabilities for the second shape = %+v", got)
		}

		// AND A SHAPE NOTHING HAS RENTED IS A MISS, not the nearest answer.
		// "nothing has asked a server yet" and "the server has no samplers"
		// are different answers, and treating a neighbouring shape's document
		// as this one's is the mistake with a card's worth of money in it.
		for _, sh := range []Shape{
			NewShape(KindSD, "runpod", "h100-80g"),
			NewShape(KindSD, "local", "rtx2080ti"),
			NewShape(KindTTS, "runpod", "rtx2080ti"),
			NewShape(KindSD, "runpod", ""),
		} {
			if _, ok, err := s.Caps(t.Context(), sh); err != nil {
				t.Fatal(err)
			} else if ok {
				t.Errorf("%+v was answered from another shape's cache", sh)
			}
		}
	})
}

// TestCaps_AMigratedKindAndACardSpellingAreOneShape.
//
// NewShape folds both, and a key built two ways is two keys. A row written
// before requests.kind holds ” and a compat built from one holds 'sd'; a card
// a browser spelled "RTX 4090" and one a config spelled "rtx4090" are the same
// silicon. Either split would cache two documents for one rental shape and
// leave the enqueue reading whichever it was not.
func TestCaps_AMigratedKindAndACardSpellingAreOneShape(t *testing.T) {
	each(t, func(t *testing.T, s Store) {
		if err := s.PutCaps(t.Context(), NewShape(KindSD, "runpod", "rtx4090"),
			sdserver.Capabilities{Model: "sd15.gguf"}); err != nil {
			t.Fatal(err)
		}
		for _, sh := range []Shape{
			NewShape("", "runpod", "rtx4090"),
			NewShape(KindSD, "runpod", "NVIDIA GeForce RTX 4090"),
			NewShape("", "runpod", "rtx_4090"),
		} {
			got, ok, err := s.Caps(t.Context(), sh)
			if err != nil {
				t.Fatal(err)
			}
			if !ok || got.Model != "sd15.gguf" {
				t.Errorf("%+v is a second key for one shape: ok=%v model=%q", sh, ok, got.Model)
			}
		}
	})
}

// TestClaimNext_ARentalStillChoosingItsOfferExcludesItsWholeKind.
//
// THE WINDOW THAT IS HIT EVERY TIME, not occasionally. The supervisor
// registers a rental before its goroutine exists and then goes straight round
// its loop, so the next create-side decision is taken before the slot has run
// Plan at all -- which means a live rental with no card yet is the ORDINARY
// state in which the second decision is made, and the exact relation there
// provokes two nodes for two asks whose silicon agrees. So an unadvertised
// rental excludes everything of its kind and provider: the money-safe reading
// of "might serve it" on the side that spends money.
//
// AND IT IS NOT "EXCLUDE EVERYTHING", which is the half that keeps a speech
// request out of an image rental's offer lookup.
func TestClaimNext_ARentalStillChoosingItsOfferExcludesItsWholeKind(t *testing.T) {
	each(t, func(t *testing.T, s Store) {
		elsewhere := req("a card nothing up has")
		elsewhere.GPU = "h100-80g"
		if _, err := s.Enqueue(t.Context(), elsewhere); err != nil {
			t.Fatal(err)
		}

		// A rental the supervisor has registered and whose Plan has not
		// returned: it asked for nothing and does not yet know what it got.
		pending := Compat{Kind: KindSD, Provider: "local", GPU: ""}
		if _, ok, err := s.ClaimNext(t.Context(), "run-2", Compat{Live: []Compat{pending}}); err != nil || ok {
			t.Errorf("a request was given a node of its own while a rental of its kind and "+
				"provider was still choosing an offer: that rental may be about to hold "+
				"exactly the card it asked for (ok=%v err=%v)", ok, err)
		}

		// A speech request is NOT excluded by an image rental's lookup: the
		// kind is the one axis that cannot be two values on one node, so no
		// offer it chooses could serve this.
		spoken := req()
		spoken.Kind = KindTTS
		spoken.Jobs = []Job{{TTS: &tts.Ask{Text: "hello", VoiceID: "v1"}}}
		id, err := s.Enqueue(t.Context(), spoken)
		if err != nil {
			t.Fatal(err)
		}
		got, ok, err := s.ClaimNext(t.Context(), "run-3", Compat{Live: []Compat{pending}})
		if err != nil || !ok {
			t.Fatalf("an image rental's offer lookup excluded a speech request: ok=%v err=%v", ok, err)
		}
		if got.ID != id {
			t.Errorf("claimed %s, want %s", got.ID, id)
		}
	})
}

// TestClaimNext_TheWideExclusionEndsWhenTheCardArrives.
//
// WITHOUT THIS THE WIDENING IS A STALL WITH NO END. A request excluded because
// a rental MIGHT serve it has to become claimable again the moment that rental
// says it cannot -- Queue.advertise writes the card and kicks, so the wait is
// one offer lookup. A widening that did not narrow would make `max_nodes: 2`
// mean one node for every queue with a rental coming up in it.
func TestClaimNext_TheWideExclusionEndsWhenTheCardArrives(t *testing.T) {
	each(t, func(t *testing.T, s Store) {
		elsewhere := req("a card nothing up has")
		elsewhere.GPU = "h100-80g"
		id, err := s.Enqueue(t.Context(), elsewhere)
		if err != nil {
			t.Fatal(err)
		}

		pending := Compat{Kind: KindSD, Provider: "local", GPU: ""}
		if _, ok, _ := s.ClaimNext(t.Context(), "run-2", Compat{Live: []Compat{pending}}); ok {
			t.Fatal("the fixture is not excluded, so this test cannot see the stall it is for")
		}

		// The same rental, now holding an rtx4090: it provably cannot serve a
		// request for an h100, so the request gets its node.
		got, ok, err := s.ClaimNext(t.Context(), "run-2", Compat{Live: []Compat{up("", "rtx4090")}})
		if err != nil || !ok {
			t.Fatalf("a request stayed excluded after the rental advertised a card that cannot "+
				"serve it: ok=%v err=%v", ok, err)
		}
		if got.ID != id {
			t.Errorf("claimed %s, want %s", got.ID, id)
		}
	})
}
