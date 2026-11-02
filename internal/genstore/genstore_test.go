package genstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Legion112/ember/internal/node"
	"github.com/Legion112/ember/internal/report"
	"github.com/Legion112/ember/internal/sdserver"
)

// stores returns both implementations, so every behavioural test below runs
// against each of them.
//
// THIS IS THE POINT OF THE TABLE. A fake that is kinder than the thing it
// stands in for tests a store that does not exist -- which is what
// runpodfake's header says about RunPod, and the reason it reports no host key
// and nulls fields the documentation promises. Running one body against both
// is cheaper than keeping two sets of assertions honest by hand.
func stores(t *testing.T) map[string]Store {
	t.Helper()
	sq, err := Open(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sq.Close() })
	return map[string]Store{"sqlite": sq, "recording": NewRecordingStore()}
}

func req(prompts ...string) Request {
	r := Request{
		Provider: "local",
		GPU:      "rtx2080ti",
		TTL:      time.Hour,
		Preview:  "proj", PreviewInterval: 5,
		MaxHourly: node.Rate(340000),
	}
	for _, p := range prompts {
		r.Jobs = append(r.Jobs, Job{Prompt: p, Width: 512, Height: 512, Steps: 4, Seed: 7, CFG: 1, Guidance: 4})
	}
	return r
}

func each(t *testing.T, fn func(t *testing.T, s Store)) {
	t.Helper()
	for name, s := range stores(t) {
		t.Run(name, func(t *testing.T) { fn(t, s) })
	}
}

func TestEnqueue_RoundTripsEverythingAsked(t *testing.T) {
	each(t, func(t *testing.T, s Store) {
		in := req("a cat", "a dog")
		id, err := s.Enqueue(t.Context(), in)
		if err != nil {
			t.Fatal(err)
		}
		if id == "" {
			t.Fatal("Enqueue returned no id")
		}

		got, err := s.Get(t.Context(), id)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status != StatusQueued {
			t.Errorf("status = %q, want queued", got.Status)
		}
		if got.Provider != "local" || got.GPU != "rtx2080ti" {
			t.Errorf("selection = %q/%q, want local/rtx2080ti", got.Provider, got.GPU)
		}
		if got.TTL != time.Hour {
			t.Errorf("ttl = %v, want 1h", got.TTL)
		}
		// Money through an integer column, because ParseFloat("0.29")*1e6 is
		// 289999 and a ceiling that changes in the round trip is a ceiling
		// nobody trusts.
		if got.MaxHourly != node.Rate(340000) {
			t.Errorf("max_hourly = %v, want $0.34/hr exactly", got.MaxHourly)
		}
		if len(got.Jobs) != 2 {
			t.Fatalf("got %d job(s), want 2", len(got.Jobs))
		}
		if got.Jobs[0].Prompt != "a cat" || got.Jobs[1].Prompt != "a dog" {
			t.Errorf("jobs came back as %q, %q", got.Jobs[0].Prompt, got.Jobs[1].Prompt)
		}
		// Order matters: job 2 is "job 2/2" in every line of output and in
		// every filename, so a listing that reordered them would rename the
		// operator's results.
		if got.Jobs[0].CFG != 1 || got.Jobs[0].Guidance != 4 {
			t.Errorf("cfg/guidance = %v/%v, want 1/4 -- two numbers that both get called cfg",
				got.Jobs[0].CFG, got.Jobs[0].Guidance)
		}
	})
}

func TestEnqueue_RefusesWhatNothingCouldRun(t *testing.T) {
	each(t, func(t *testing.T, s Store) {
		for _, c := range []struct {
			name string
			r    Request
			want string
		}{
			{"no provider", Request{TTL: time.Hour, Jobs: []Job{{Prompt: "a"}}}, "no provider"},
			{"no jobs", Request{Provider: "local", TTL: time.Hour}, "generate nothing"},
			{"no ttl", Request{Provider: "local", Jobs: []Job{{Prompt: "a"}}}, "no TTL"},
		} {
			t.Run(c.name, func(t *testing.T) {
				_, err := s.Enqueue(t.Context(), c.r)
				if err == nil {
					t.Fatal("accepted a request nothing could run")
				}
				if !strings.Contains(err.Error(), c.want) {
					t.Errorf("err = %v, want it to mention %q", err, c.want)
				}
			})
		}
	})
}

// TestEnqueue_IsHowARequestEntersTheQueue.
//
// Whatever status the caller set, the row is queued. Letting a caller enqueue
// something already "done" would put a row in the history that never
// happened -- and one already "running" would be a row the drain loop never
// claims and never finishes.
func TestEnqueue_IsHowARequestEntersTheQueue(t *testing.T) {
	each(t, func(t *testing.T, s Store) {
		in := req("a")
		in.Status = StatusDone
		id, err := s.Enqueue(t.Context(), in)
		if err != nil {
			t.Fatal(err)
		}
		got, err := s.Get(t.Context(), id)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status != StatusQueued {
			t.Errorf("status = %q, want queued whatever the caller asked for", got.Status)
		}
	})
}

func TestGet_MissingIsNotFound(t *testing.T) {
	each(t, func(t *testing.T, s Store) {
		if _, err := s.Get(t.Context(), "nothing"); !errors.Is(err, ErrNotFound) {
			t.Errorf("err = %v, want ErrNotFound", err)
		}
	})
}

// TestClaimNext_TakesTheOldestQueued. First in, first served: an operator who
// pushed three prompts expects them in that order, and a queue that reordered
// them would make the output directory unreadable.
func TestClaimNext_TakesTheOldestQueued(t *testing.T) {
	each(t, func(t *testing.T, s Store) {
		var ids []ReqID
		for i, p := range []string{"first", "second", "third"} {
			r := req(p)
			// Distinct timestamps, because the ordering is by creation time
			// and a test where all three are simultaneous would pass on a
			// store that sorted at random.
			r.Created = time.Date(2026, 11, 1, 12, i, 0, 0, time.UTC)
			id, err := s.Enqueue(t.Context(), r)
			if err != nil {
				t.Fatal(err)
			}
			ids = append(ids, id)
		}

		for i, want := range ids {
			got, ok, err := s.ClaimNext(t.Context(), fmt.Sprintf("run-%d", i), Compat{})
			if err != nil {
				t.Fatal(err)
			}
			if !ok {
				t.Fatalf("claim %d found nothing", i)
			}
			if got.ID != want {
				t.Errorf("claim %d took %s, want %s", i, got.ID, want)
			}
			if got.Status != StatusRunning {
				t.Errorf("a claimed request has status %q, want running", got.Status)
			}
			if got.RunID != fmt.Sprintf("run-%d", i) {
				t.Errorf("run id = %q, want run-%d", got.RunID, i)
			}
			if got.Started.IsZero() {
				t.Error("a claimed request has no start time")
			}
			// The jobs come with it, or the caller would have to go back for
			// them and could be handed a request that changed in between.
			if len(got.Jobs) != 1 {
				t.Errorf("got %d job(s) with the claim, want 1", len(got.Jobs))
			}
		}

		if _, ok, err := s.ClaimNext(t.Context(), "run-x", Compat{}); err != nil || ok {
			t.Errorf("claimed something from an empty queue: ok=%v err=%v", ok, err)
		}
	})
}

// TestClaimNext_OnlyJoinsACompatibleRental is the rule the one-rental drain
// loop turns on.
//
// A request asking for a different card cannot be served by the node that is
// up, so it waits -- and the loop then has nothing to claim, finishes, and
// destroys. Matching it anyway would generate on hardware the operator did not
// ask for and bill them for it.
func TestClaimNext_OnlyJoinsACompatibleRental(t *testing.T) {
	each(t, func(t *testing.T, s Store) {
		same := req("same card")
		same.Created = time.Date(2026, 11, 1, 12, 1, 0, 0, time.UTC)

		other := req("another card")
		other.GPU = "h100-80g"
		other.Created = time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC) // older

		idOther, err := s.Enqueue(t.Context(), other)
		if err != nil {
			t.Fatal(err)
		}
		idSame, err := s.Enqueue(t.Context(), same)
		if err != nil {
			t.Fatal(err)
		}

		// A rental on rtx2080ti is up. The older request wants an h100, so
		// the newer one is served even though it is not next in line.
		live := Compat{Provider: "local", GPU: "rtx2080ti"}
		got, ok, err := s.ClaimNext(t.Context(), "run-1", live)
		if err != nil || !ok {
			t.Fatalf("the live rental could not claim its own card's request: ok=%v err=%v", ok, err)
		}
		if got.ID != idSame {
			t.Errorf("claimed %s, want the compatible %s", got.ID, idSame)
		}

		// And nothing else for that rental, so the loop knows to stop.
		if _, ok, err := s.ClaimNext(t.Context(), "run-1", live); err != nil || ok {
			t.Errorf("the live rental claimed an incompatible request: ok=%v err=%v", ok, err)
		}

		// With no rental up, the h100 request is next.
		got, ok, err = s.ClaimNext(t.Context(), "run-2", Compat{})
		if err != nil || !ok {
			t.Fatalf("a fresh rental found nothing to start: ok=%v err=%v", ok, err)
		}
		if got.ID != idOther {
			t.Errorf("a fresh rental claimed %s, want %s", got.ID, idOther)
		}
	})
}

// TestClaimNext_ClaimsEachRequestExactlyOnce is the race the single statement
// exists to prevent.
//
// Two drain loops -- or one loop and a restart that overlapped -- must not
// both serve the same request: the operator would be billed twice for one
// batch and the second rental would overwrite the first's images.
func TestClaimNext_ClaimsEachRequestExactlyOnce(t *testing.T) {
	each(t, func(t *testing.T, s Store) {
		const n = 20
		for i := 0; i < n; i++ {
			r := req(fmt.Sprintf("job %d", i))
			r.Created = time.Date(2026, 11, 1, 12, 0, i, 0, time.UTC)
			if _, err := s.Enqueue(t.Context(), r); err != nil {
				t.Fatal(err)
			}
		}

		var mu sync.Mutex
		seen := map[ReqID]int{}
		var wg sync.WaitGroup
		for w := 0; w < 4; w++ {
			wg.Add(1)
			go func(w int) {
				defer wg.Done()
				for {
					r, ok, err := s.ClaimNext(context.Background(), fmt.Sprintf("run-%d", w), Compat{})
					if err != nil {
						t.Errorf("claim: %v", err)
						return
					}
					if !ok {
						return
					}
					mu.Lock()
					seen[r.ID]++
					mu.Unlock()
				}
			}(w)
		}
		wg.Wait()

		if len(seen) != n {
			t.Errorf("claimed %d distinct request(s), want %d", len(seen), n)
		}
		for id, times := range seen {
			if times != 1 {
				t.Errorf("request %s was claimed %d times", id, times)
			}
		}
	})
}

func TestUpdate_WritesWhatTheRunLearned(t *testing.T) {
	each(t, func(t *testing.T, s Store) {
		id, err := s.Enqueue(t.Context(), req("a"))
		if err != nil {
			t.Fatal(err)
		}
		ended := time.Date(2026, 11, 1, 13, 0, 0, 0, time.UTC)

		if err := s.Update(t.Context(), id, func(r *Request) {
			r.Status = StatusDone
			r.Offer = "offer-1"
			r.Node = "local/n1"
			r.Cost = node.MicroUSD(11333)
			r.Ended = ended
		}); err != nil {
			t.Fatal(err)
		}

		got, err := s.Get(t.Context(), id)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status != StatusDone || got.Offer != "offer-1" || got.Node != "local/n1" {
			t.Errorf("got %+v", got)
		}
		if got.Cost != node.MicroUSD(11333) {
			t.Errorf("cost = %v, want $0.011333 exactly", got.Cost)
		}
		if !got.Ended.Equal(ended) {
			t.Errorf("ended = %v, want %v", got.Ended, ended)
		}
	})
}

// TestUpdate_RefusesAStatusNothingWrites. A status nothing recognises is a
// request the queue will neither run nor report: it would simply sit there.
func TestUpdate_RefusesAStatusNothingWrites(t *testing.T) {
	each(t, func(t *testing.T, s Store) {
		id, err := s.Enqueue(t.Context(), req("a"))
		if err != nil {
			t.Fatal(err)
		}
		err = s.Update(t.Context(), id, func(r *Request) { r.Status = "nearly" })
		if err == nil {
			t.Fatal("accepted a status ember does not write")
		}
		got, err := s.Get(t.Context(), id)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status != StatusQueued {
			t.Errorf("the refused update was applied anyway: status = %q", got.Status)
		}
	})
}

// TestUpdate_CannotRewriteWhatWasAskedFor. A request whose jobs changed after
// it ran is a record of something that did not happen -- and the images on
// disk would be named after prompts nobody submitted.
func TestUpdate_CannotRewriteWhatWasAskedFor(t *testing.T) {
	each(t, func(t *testing.T, s Store) {
		id, err := s.Enqueue(t.Context(), req("the prompt that was submitted"))
		if err != nil {
			t.Fatal(err)
		}
		if err := s.Update(t.Context(), id, func(r *Request) {
			r.Jobs = []Job{{Prompt: "something else entirely"}}
		}); err != nil {
			t.Fatal(err)
		}
		got, err := s.Get(t.Context(), id)
		if err != nil {
			t.Fatal(err)
		}
		if len(got.Jobs) != 1 || got.Jobs[0].Prompt != "the prompt that was submitted" {
			t.Errorf("the jobs were rewritten: %+v", got.Jobs)
		}
	})
}

func TestUpdate_MissingIsNotFound(t *testing.T) {
	each(t, func(t *testing.T, s Store) {
		if err := s.Update(t.Context(), "nothing", func(*Request) {}); !errors.Is(err, ErrNotFound) {
			t.Errorf("err = %v, want ErrNotFound", err)
		}
	})
}

// TestEvents_TheCursorIsWhatMakesAReconnectLossless.
//
// A browser says "I have seen up to 41" and gets everything since, whether it
// was away for a second or an hour. A ring buffer in memory could not answer
// that once it had wrapped, which is why these are rows.
func TestEvents_TheCursorIsWhatMakesAReconnectLossless(t *testing.T) {
	each(t, func(t *testing.T, s Store) {
		id, err := s.Enqueue(t.Context(), req("a"))
		if err != nil {
			t.Fatal(err)
		}

		var es []report.Event
		for i := int64(1); i <= 5; i++ {
			es = append(es, report.Event{Seq: i, Kind: report.KindJobProgress, Step: int(i), TotalSteps: 5})
		}
		if err := s.AppendEvents(t.Context(), id, es); err != nil {
			t.Fatal(err)
		}

		all, err := s.Events(t.Context(), id, 0)
		if err != nil {
			t.Fatal(err)
		}
		if len(all) != 5 {
			t.Fatalf("got %d event(s) from the start, want 5", len(all))
		}
		for i, e := range all {
			if e.Seq != int64(i+1) {
				t.Errorf("event %d has Seq %d; the order is the cursor", i, e.Seq)
			}
		}

		rest, err := s.Events(t.Context(), id, 3)
		if err != nil {
			t.Fatal(err)
		}
		if len(rest) != 2 || rest[0].Seq != 4 {
			t.Errorf("after 3 got %d event(s) starting at %d, want 2 starting at 4", len(rest), seqOf(rest))
		}

		// The payload survives, not just the sequence number: the whole point
		// is that a reconnecting client sees what it missed.
		if rest[0].Step != 4 || rest[0].TotalSteps != 5 {
			t.Errorf("the replayed event lost its payload: %+v", rest[0])
		}
	})
}

// TestEvents_ARedeliveredEventIsNotAnError. A writer that died between
// emitting and committing emits the same sequence number again; a unique
// violation there would turn a resumed run into a failed one.
func TestEvents_ARedeliveredEventIsNotAnError(t *testing.T) {
	each(t, func(t *testing.T, s Store) {
		id, err := s.Enqueue(t.Context(), req("a"))
		if err != nil {
			t.Fatal(err)
		}
		first := report.Event{Seq: 1, Kind: report.KindLog, Text: "first\n"}
		again := report.Event{Seq: 1, Kind: report.KindLog, Text: "again\n"}

		if err := s.AppendEvents(t.Context(), id, []report.Event{first}); err != nil {
			t.Fatal(err)
		}
		if err := s.AppendEvents(t.Context(), id, []report.Event{again}); err != nil {
			t.Fatalf("a re-delivered event was an error: %v", err)
		}
		got, err := s.Events(t.Context(), id, 0)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 {
			t.Fatalf("got %d event(s) for one sequence number, want 1", len(got))
		}
		if got[0].Text != "again\n" {
			t.Errorf("text = %q, want the later delivery to win", got[0].Text)
		}
	})
}

func TestAppendEvents_NothingIsNotAWrite(t *testing.T) {
	each(t, func(t *testing.T, s Store) {
		id, err := s.Enqueue(t.Context(), req("a"))
		if err != nil {
			t.Fatal(err)
		}
		if err := s.AppendEvents(t.Context(), id, nil); err != nil {
			t.Errorf("appending nothing failed: %v", err)
		}
	})
}

// TestImages_APreviewIsOverwrittenAndAResultIsNot.
//
// The two are in one table because they are both images of the same job, and
// separated by a column because a preview is written over as sampling proceeds
// -- the useful thing is a file an operator can leave open in a viewer, not
// three hundred PNGs of a half-finished cat -- while a result is kept.
func TestImages_APreviewIsOverwrittenAndAResultIsNot(t *testing.T) {
	each(t, func(t *testing.T, s Store) {
		id, err := s.Enqueue(t.Context(), req("a"))
		if err != nil {
			t.Fatal(err)
		}

		for i := 0; i < 3; i++ {
			if err := s.AddImage(t.Context(), Image{
				Req: id, Job: 1, Index: 0, Preview: true,
				Key: "run/a-preview-00.png", Bytes: int64(100 + i),
			}); err != nil {
				t.Fatal(err)
			}
		}
		for i := 0; i < 2; i++ {
			if err := s.AddImage(t.Context(), Image{
				Req: id, Job: 1, Index: i, Key: fmt.Sprintf("run/a-%02d.png", i), Bytes: 2048,
			}); err != nil {
				t.Fatal(err)
			}
		}

		got, err := s.Images(t.Context(), id)
		if err != nil {
			t.Fatal(err)
		}
		var results, previews int
		for _, im := range got {
			if im.Preview {
				previews++
			} else {
				results++
			}
		}
		if previews != 1 {
			t.Errorf("got %d preview row(s) after three frames, want 1 overwritten row", previews)
		}
		if results != 2 {
			t.Errorf("got %d result(s), want 2 -- a batch_count of two keeps both", results)
		}
		// Results before previews, so a gallery does not open on a latent.
		if len(got) > 0 && got[0].Preview {
			t.Error("the listing leads with a preview frame")
		}
	})
}

// TestAddImage_RefusesARowNothingCouldRead. A row with no key is a record that
// a file exists and no way to find it.
func TestAddImage_RefusesARowNothingCouldRead(t *testing.T) {
	each(t, func(t *testing.T, s Store) {
		id, err := s.Enqueue(t.Context(), req("a"))
		if err != nil {
			t.Fatal(err)
		}
		if err := s.AddImage(t.Context(), Image{Req: id, Job: 1}); err == nil {
			t.Error("accepted an image with no blob key")
		}
	})
}

// TestCaps_AbsentUntilSomethingHasLearnedThem.
//
// The distinction is load-bearing: "the server said this sampler does not
// exist" and "nothing has asked a server yet" are different answers, and
// treating the second as an empty capability list would refuse every job in
// the process before the first rental.
func TestCaps_AbsentUntilSomethingHasLearnedThem(t *testing.T) {
	each(t, func(t *testing.T, s Store) {
		_, ok, err := s.Caps(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if ok {
			t.Fatal("a fresh store already claims to know what a server can do")
		}

		want := sdserver.Capabilities{
			Model:        "sd15.gguf",
			Samplers:     []string{"euler", "heun"},
			Schedulers:   []string{"karras"},
			PreviewModes: []string{"none", "proj"},
			Limits:       sdserver.Limits{MinWidth: 64, MaxWidth: 2048},
		}
		if err := s.PutCaps(t.Context(), want); err != nil {
			t.Fatal(err)
		}

		got, ok, err := s.Caps(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if !ok {
			t.Fatal("the capabilities did not come back")
		}
		if got.Model != want.Model || len(got.Samplers) != 2 || got.Limits.MaxWidth != 2048 {
			t.Errorf("got %+v, want %+v", got, want)
		}
	})
}

// TestInterrupt_ARentalThatIsGoneIsNotResumed.
//
// A request in flight when the process died cannot be resumed: its node is
// gone. It is marked interrupted rather than re-queued, because renting a
// second card for work the operator may no longer want is the kind of help
// nobody asked for -- and rather than failed, because it was not the
// generation that broke.
func TestInterrupt_ARentalThatIsGoneIsNotResumed(t *testing.T) {
	each(t, func(t *testing.T, s Store) {
		running, err := s.Enqueue(t.Context(), req("in flight"))
		if err != nil {
			t.Fatal(err)
		}
		waiting, err := s.Enqueue(t.Context(), req("still queued"))
		if err != nil {
			t.Fatal(err)
		}
		if _, ok, err := s.ClaimNext(t.Context(), "run-1", Compat{}); err != nil || !ok {
			t.Fatalf("could not set up a running request: ok=%v err=%v", ok, err)
		}

		n, err := s.Interrupt(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if n != 1 {
			t.Errorf("interrupted %d request(s), want 1", n)
		}

		got, err := s.Get(t.Context(), running)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status != StatusInterrupted {
			t.Errorf("status = %q, want interrupted", got.Status)
		}
		if got.Ended.IsZero() {
			t.Error("an interrupted request has no end time, so nothing can say how long it ran")
		}

		// The queue is untouched: a request that never started is still
		// waiting for a rental, and a restart should run it.
		q, err := s.Get(t.Context(), waiting)
		if err != nil {
			t.Fatal(err)
		}
		if q.Status != StatusQueued {
			t.Errorf("a queued request became %q on restart", q.Status)
		}
	})
}

func TestList_NewestFirstAndFiltered(t *testing.T) {
	each(t, func(t *testing.T, s Store) {
		for i, p := range []string{"oldest", "middle", "newest"} {
			r := req(p)
			r.Created = time.Date(2026, 11, 1, 12, i, 0, 0, time.UTC)
			if _, err := s.Enqueue(t.Context(), r); err != nil {
				t.Fatal(err)
			}
		}
		if _, ok, err := s.ClaimNext(t.Context(), "run-1", Compat{}); err != nil || !ok {
			t.Fatal("setup claim failed")
		}

		all, err := s.List(t.Context(), Filter{})
		if err != nil {
			t.Fatal(err)
		}
		if len(all) != 3 {
			t.Fatalf("got %d, want 3", len(all))
		}
		if all[0].Jobs[0].Prompt != "newest" {
			t.Errorf("the listing leads with %q, want newest first", all[0].Jobs[0].Prompt)
		}

		queued, err := s.List(t.Context(), Filter{Status: StatusQueued})
		if err != nil {
			t.Fatal(err)
		}
		if len(queued) != 2 {
			t.Errorf("got %d queued, want 2 (one was claimed)", len(queued))
		}

		one, err := s.List(t.Context(), Filter{Limit: 1})
		if err != nil {
			t.Fatal(err)
		}
		if len(one) != 1 {
			t.Errorf("Limit 1 returned %d", len(one))
		}
	})
}

// TestOpen_RefusesAFileFromTheFuture.
//
// internal/statestore's reason: reading a newer layout as an older one drops
// columns, and the one it would drop is the one that says a request is still
// running -- so a downgrade would report a clean queue over a rental that is
// billing.
func TestOpen_RefusesAFileFromTheFuture(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE meta SET value = ? WHERE key = 'schema_version'`,
		fmt.Sprint(schemaVersion+1)); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	_, err = Open(dir)
	if err == nil {
		t.Fatal("a database written by a newer ember was opened anyway")
	}
	if !strings.Contains(err.Error(), "newer ember") {
		t.Errorf("err = %v, want it to say the file is from the future", err)
	}
}

func TestOpen_ReportsADirectoryItCannotMake(t *testing.T) {
	f := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(f, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(filepath.Join(f, "state")); err == nil {
		t.Error("Open accepted a path under a regular file")
	}
}

func TestOpen_SurvivesBeingReopened(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	id, err := s.Enqueue(t.Context(), req("a"))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	again, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = again.Close() }()
	if _, err := again.Get(t.Context(), id); err != nil {
		t.Errorf("a request did not survive a restart: %v", err)
	}
	if again.Path() == "" {
		t.Error("Path() is empty, so an error message could not name the file")
	}
}

func TestNewReqID_IsNotASequence(t *testing.T) {
	// An id that leaks a count tells anyone who sees one URL how many others
	// exist.
	seen := map[ReqID]bool{}
	for i := 0; i < 100; i++ {
		id := NewReqID()
		if seen[id] {
			t.Fatalf("NewReqID repeated %s", id)
		}
		seen[id] = true
	}
}

func TestStatus_TerminalAndValid(t *testing.T) {
	for _, s := range []Status{StatusDone, StatusFailed, StatusCancelled, StatusInterrupted} {
		if !s.Terminal() {
			t.Errorf("%s is not terminal", s)
		}
		if !s.Valid() {
			t.Errorf("%s is not valid", s)
		}
	}
	for _, s := range []Status{StatusQueued, StatusRunning} {
		if s.Terminal() {
			t.Errorf("%s is terminal", s)
		}
		if !s.Valid() {
			t.Errorf("%s is not valid", s)
		}
	}
	if Status("nearly").Valid() {
		t.Error("an invented status is valid")
	}
}

// TestJob_SDCarriesNoExtra is the seal this type exists for.
//
// sdserver.Job's Extra reaches every field of the generation API ember does
// not type, including the ones naming file paths on the node -- so a request
// that could set it could point a running sd-server at weights the config
// never mentioned. There is no Extra on genstore.Job, so a round trip through
// this store cannot invent one.
func TestJob_SDCarriesNoExtra(t *testing.T) {
	j := Job{Prompt: "a", Width: 512, Height: 512, Steps: 4, Seed: 7, CFG: 1, Guidance: 4, Output: "cat"}
	sd := j.SD()

	if sd.Extra != nil {
		t.Errorf("Extra = %v, want nil: nothing that arrives over a wire may set it", sd.Extra)
	}
	if sd.Prompt != "a" || sd.Width != 512 || sd.Seed != 7 || sd.Output != "cat" {
		t.Errorf("the conversion lost a field: %+v", sd)
	}
	if sd.CFG != 1 || sd.Guidance != 4 {
		t.Errorf("cfg/guidance = %v/%v, want 1/4", sd.CFG, sd.Guidance)
	}
	// And the body it produces is acceptable, so the conversion is not just
	// field-for-field but actually usable.
	if _, err := sd.Body("proj", 5); err != nil {
		t.Errorf("the converted job does not produce a request body: %v", err)
	}
}

func seqOf(es []report.Event) int64 {
	if len(es) == 0 {
		return 0
	}
	return es[0].Seq
}

// A version-1 file is brought forward rather than broken.
//
// THIS IS THE BUG THE MIGRATION LIST EXISTS FOR, and it was latent for the
// life of the schema: migrate refused a file from the future and did nothing
// at all about a file from the past, because the schema is
// `CREATE TABLE IF NOT EXISTS` -- a no-op against a table that already
// exists. So the first column ever added would have been absent from every
// database written before it, Open would have succeeded, the version check
// would have passed, and the failure would have arrived as
// "table jobs has no column named params" the next time an operator queued a
// request, with their whole history in a file ember could no longer write to.
//
// Built by hand as a version-1 file rather than by checking out the old code,
// because what has to be proved is that TODAY'S ember reads YESTERDAY'S file.
func TestOpen_MigratesAVersionOneFile(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, FileName)

	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	// The version-1 schema, as it was: jobs with no params column. Only the
	// tables this test touches, since the rest were unchanged.
	for _, stmt := range []string{
		`CREATE TABLE meta (key TEXT PRIMARY KEY, value TEXT NOT NULL)`,
		`INSERT INTO meta (key, value) VALUES ('schema_version', '1')`,
		`CREATE TABLE requests (
			id TEXT PRIMARY KEY, created_unix INTEGER NOT NULL, status TEXT NOT NULL,
			provider TEXT NOT NULL, gpu TEXT NOT NULL DEFAULT '', offer TEXT NOT NULL DEFAULT '',
			ttl_ns INTEGER NOT NULL, max_hourly INTEGER NOT NULL DEFAULT 0,
			max_run INTEGER NOT NULL DEFAULT 0, preview TEXT NOT NULL DEFAULT '',
			preview_every INTEGER NOT NULL DEFAULT 0, run_id TEXT NOT NULL DEFAULT '',
			node TEXT NOT NULL DEFAULT '', started_unix INTEGER NOT NULL DEFAULT 0,
			ended_unix INTEGER NOT NULL DEFAULT 0, cost INTEGER NOT NULL DEFAULT 0,
			err TEXT NOT NULL DEFAULT '')`,
		`CREATE TABLE jobs (
			req TEXT NOT NULL, idx INTEGER NOT NULL, prompt TEXT NOT NULL DEFAULT '',
			negative TEXT NOT NULL DEFAULT '', width INTEGER NOT NULL DEFAULT 0,
			height INTEGER NOT NULL DEFAULT 0, steps INTEGER NOT NULL DEFAULT 0,
			batch_count INTEGER NOT NULL DEFAULT 0, seed INTEGER NOT NULL DEFAULT 0,
			sampler TEXT NOT NULL DEFAULT '', scheduler TEXT NOT NULL DEFAULT '',
			cfg REAL NOT NULL DEFAULT 0, guidance REAL NOT NULL DEFAULT 0,
			output TEXT NOT NULL DEFAULT '', PRIMARY KEY (req, idx))`,
		`CREATE TABLE events (req TEXT NOT NULL, seq INTEGER NOT NULL, kind TEXT NOT NULL,
			body TEXT NOT NULL, PRIMARY KEY (req, seq))`,
		`CREATE TABLE images (req TEXT NOT NULL, job INTEGER NOT NULL, idx INTEGER NOT NULL,
			preview INTEGER NOT NULL DEFAULT 0, key TEXT NOT NULL,
			bytes INTEGER NOT NULL DEFAULT 0, at_unix INTEGER NOT NULL,
			PRIMARY KEY (req, job, idx, preview))`,
		// A request already in the file, so the migration has to preserve
		// history rather than merely succeed against an empty table.
		`INSERT INTO requests (id, created_unix, status, provider, ttl_ns)
			VALUES ('old1', 1000, 'queued', 'local', 600000000000)`,
		`INSERT INTO jobs (req, idx, prompt, steps) VALUES ('old1', 1, 'an older cat', 12)`,
		// A capability cache written by the old code: complete for what it
		// knew about and silent about the two lists added with this version.
		`INSERT INTO meta (key, value) VALUES ('caps',
			'{"Model":"sd15.gguf","Samplers":["euler"],"Schedulers":["karras"]}')`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("seed a version-1 file: %v", err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	s, err := Open(dir)
	if err != nil {
		t.Fatalf("a version-1 file was refused: %v", err)
	}
	defer func() { _ = s.Close() }()

	// The version moved.
	var v string
	if err := s.db.QueryRow(`SELECT value FROM meta WHERE key = 'schema_version'`).Scan(&v); err != nil {
		t.Fatal(err)
	}
	if v != fmt.Sprint(schemaVersion) {
		t.Errorf("schema version = %q, want %d", v, schemaVersion)
	}

	// The old request is still there, and reads with an empty Params rather
	// than failing to decode the column's default.
	got, err := s.Get(context.Background(), "old1")
	if err != nil {
		t.Fatalf("the migrated file lost its history: %v", err)
	}
	if len(got.Jobs) != 1 || got.Jobs[0].Prompt != "an older cat" {
		t.Fatalf("jobs = %+v, want the seeded one", got.Jobs)
	}
	if !got.Jobs[0].Params.SLG.IsZero() || got.Jobs[0].Params.ClipSkip != nil {
		t.Errorf("params = %+v, want the zero value for a pre-migration job", got.Jobs[0].Params)
	}

	// The stale capability cache is GONE rather than carried forward
	// incomplete. Kept, its empty output_formats would read as "the server
	// does not report formats" and refuse a correct one, blaming the server
	// for a field ember never asked about -- so "nothing has asked yet",
	// which is true, is the better answer. The next rental re-caches it.
	if _, ok, err := s.Caps(context.Background()); err != nil {
		t.Fatal(err)
	} else if ok {
		t.Error("a capability cache from before the new lists survived the migration")
	}

	// And the new column is writable, which is the half that would have
	// failed at runtime rather than at open.
	two := 2
	id, err := s.Enqueue(context.Background(), Request{
		Provider: "local", TTL: time.Minute,
		Jobs: []Job{{Prompt: "a newer cat", Params: sdserver.Params{ClipSkip: &two}}},
	})
	if err != nil {
		t.Fatalf("the migrated file cannot be written to: %v", err)
	}
	back, err := s.Get(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if back.Jobs[0].Params.ClipSkip == nil || *back.Jobs[0].Params.ClipSkip != 2 {
		t.Errorf("clip_skip = %v, want 2", back.Jobs[0].Params.ClipSkip)
	}
}

// Every optional parameter survives the round trip through the JSON column.
//
// One column holds all of them, so a field missing from the struct's tags --
// or a pointer silently flattened to its zero -- would lose a parameter the
// operator set and generate at the server's default instead. Checked by
// comparing the whole struct rather than field by field, so a field added
// later is covered without anybody remembering to add an assertion.
func TestEnqueue_RoundTripsEveryParameter(t *testing.T) {
	skip, eta, shift := 2, 0.0, 3.5
	want := sdserver.Params{
		ClipSkip: &skip, Eta: &eta, FlowShift: &shift,
		ShiftedTimestep: 4, OutputFormat: "webp", OutputCompression: 90,
		SLG: sdserver.SLG{Layers: []int{7, 8, 9}, LayerStart: 0.01, LayerEnd: 0.2, Scale: 2.5},
		Hires: sdserver.Hires{
			Enabled: true, Upscaler: "Latent", Scale: 2, TargetWidth: 1024,
			TargetHeight: 768, Steps: 10, DenoisingStrength: 0.7, UpscaleTileSize: 128,
		},
		VAETiling: sdserver.VAETiling{
			Enabled: true, TileSizeW: 512, TileSizeH: 256,
			TargetOverlap: 0.5, RelSizeW: 0.25, RelSizeH: 0.125,
		},
	}

	for name, s := range stores(t) {
		t.Run(name, func(t *testing.T) {
			roundTripParams(t, s, want)
		})
	}
}

// roundTripParams is the body both stores run, so the fake cannot be kinder
// than the database about a field it forgot to carry.
func roundTripParams(t *testing.T, s Store, want sdserver.Params) {
	t.Helper()
	id, err := s.Enqueue(context.Background(), Request{
		Provider: "local", TTL: time.Minute,
		Jobs: []Job{{Prompt: "a cat", Params: want}},
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.Get(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.Jobs[0].Params, want) {
		t.Errorf("params round-tripped as\n %+v\nwant\n %+v", got.Jobs[0].Params, want)
	}
	// And a pointer holding zero came back as a pointer holding zero rather
	// than as nil: eta 0 is a real instruction to the sampler.
	if got.Jobs[0].Params.Eta == nil || *got.Jobs[0].Params.Eta != 0 {
		t.Errorf("eta = %v, want a pointer to 0", got.Jobs[0].Params.Eta)
	}
	// SD carries them to the client, which is the only reason they are stored.
	if !reflect.DeepEqual(got.Jobs[0].SD().Params, want) {
		t.Errorf("SD() dropped the parameters: %+v", got.Jobs[0].SD().Params)
	}
}
