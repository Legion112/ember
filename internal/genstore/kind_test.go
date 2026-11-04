package genstore

import (
	"testing"
	"time"
)

// TestClaimNext_AMigratedRowHasNoKindAndIsStillClaimable is the test whose
// SUBJECT is the empty-means-sd rule, and it exists because the test that
// caught the rule's defect caught it by accident.
//
// WHAT THE ACCIDENT WAS. Replacing the claim's
// `CASE WHEN kind = ” THEN 'sd' ELSE kind END` with a plain `AND kind = ?`
// turned `TestClaimNext_OnlyJoinsACompatibleRental` red -- but only because the
// shared `req()` fixture happened not to set `Kind`, so its rows stored `”`
// and the compat held `'sd'`. Setting `Kind: KindSD` in that fixture is an
// entirely reasonable tidy-up now the column is first-class, and with it the
// wedge is `go test ./...` **exit 0**: no assertion anywhere whose subject is
// the rule, and a future tidy-up silently disarming the one that was. So the
// fixture is tidied AND this is written, which is the only arrangement in
// which neither depends on the other.
//
// WHAT THE RULE IS FOR. Every row written before schema version 6 holds `”`,
// because the migration adds the column with that default rather than
// rewriting a table this schema never rewrites. A claim comparing the raw
// column refuses every one of those rows to an sd rental -- so an operator
// upgrading from schema 5 gets a queue that is **permanently unclaimable**,
// with `ember serve` draining nothing and saying nothing about why.
//
// THE EMPTY KIND IS SET HERE EXPLICITLY, which is what cannot be switched off
// by somebody tidying a fixture: this test says in its own body that the
// absence is the subject.
//
// It is the mirror of `TestOpen_MigratesAVersionFourFile`'s lesson -- that one
// was a hand-built fixture missing the ROW the bug reads, and this was a
// hand-built fixture missing the VALUE.
func TestClaimNext_AMigratedRowHasNoKindAndIsStillClaimable(t *testing.T) {
	each(t, func(t *testing.T, s Store) {
		// THE PRE-SCHEMA-6 SHAPE, deliberately: Kind is left empty, which is
		// what every row written before the column existed holds. Built here
		// rather than through req(), so no edit to that fixture can change
		// what this test is about.
		migrated := Request{
			Provider: "local",
			GPU:      "rtx2080ti",
			TTL:      time.Hour,
			Jobs:     []Job{{Prompt: "a cat", Width: 64, Height: 64, Steps: 1}},
			Created:  time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC),
		}
		if migrated.Kind != "" {
			t.Fatal("this test is about a row with NO kind; something gave it one")
		}
		id, err := s.Enqueue(t.Context(), migrated)
		if err != nil {
			t.Fatal(err)
		}

		// A SPEECH RENTAL MUST NOT TAKE IT, asserted first so that the
		// acceptance below cannot be satisfied by a claim that takes
		// everything -- which is what a compat ignoring the kind is.
		if _, ok, err := s.ClaimNext(t.Context(), "run-tts",
			Compat{Kind: KindTTS, Provider: "local", GPU: "rtx2080ti"}); err != nil || ok {
			t.Errorf("a speech rental claimed a migrated image row: ok=%v err=%v", ok, err)
		}

		// AND AN sd RENTAL MUST. This is the whole rule: the row says `''`
		// and the rental says `sd`, and they are the same kind.
		got, ok, err := s.ClaimNext(t.Context(), "run-sd",
			Compat{Kind: KindSD, Provider: "local", GPU: "rtx2080ti"})
		if err != nil {
			t.Fatal(err)
		}
		if !ok {
			t.Fatalf("an sd rental could not claim a row written before the kind column existed; "+
				"every request in a store upgraded from schema 5 is one of these, so this is a "+
				"queue that drains nothing (request %s)", id)
		}
		if got.ID != id {
			t.Errorf("claimed %s, want %s", got.ID, id)
		}
	})
}

// TestClaimNext_AnEmptyCompatKindStillMeansSD covers the other direction of
// the same normalisation, which is the shape `once` actually produces for a
// pre-v6 ROW: `Compat{Kind: KindOf(first.Kind)}` is `sd`, but a caller that
// built one by hand -- as the drain loop's own tests and `ClaimNext(…,
// Compat{})` do -- leaves it empty.
//
// Both sides go through KindOf, so an empty compat and an `sd` one are one
// rental to this. Without it the queue would have a state in which a request
// is claimable by neither spelling.
func TestClaimNext_AnEmptyCompatKindStillMeansSD(t *testing.T) {
	each(t, func(t *testing.T, s Store) {
		r := req("a cat")
		r.Kind = KindSD
		id, err := s.Enqueue(t.Context(), r)
		if err != nil {
			t.Fatal(err)
		}
		got, ok, err := s.ClaimNext(t.Context(), "run-1",
			Compat{Provider: "local", GPU: "rtx2080ti"})
		if err != nil {
			t.Fatal(err)
		}
		if !ok {
			t.Fatal("a compat with no kind could not claim an sd request")
		}
		if got.ID != id {
			t.Errorf("claimed %s, want %s", got.ID, id)
		}
	})
}
