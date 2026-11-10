package genstore

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Legion112/ember/internal/node"
	"github.com/Legion112/ember/internal/report"
	"github.com/Legion112/ember/internal/sdserver"
)

// RecordingStore is an in-memory Store good enough to drive the queue's logic.
//
// Field names follow dockerengine.RecordingEngine, which follows go-tun's
// linux.RecordingRunner -- Calls, Errors, FailOn -- so anyone who has read one
// fake in this repository reads this one without learning anything.
//
// IT IS NOT A SECOND IMPLEMENTATION OF THE QUEUE'S RULES. ClaimNext here does
// the same thing the SQL one does, and TestRecordingAgreesWithSQLite drives
// both through the same sequence and compares, because a fake that is kinder
// than the thing it stands in for tests a store that does not exist -- which
// is what runpodfake exists to say about RunPod.
type RecordingStore struct {
	mu sync.Mutex

	// Calls records every operation as "Enqueue abc123".
	Calls []string

	// Errors fails a specific call, keyed by the same string Calls records.
	Errors map[string]error

	// FailOn fails any call whose record contains this substring.
	FailOn string

	// Now is the clock, injected so a recorded timestamp is comparable.
	Now func() time.Time

	reqs   map[ReqID]*Request
	order  []ReqID // insertion order, so "oldest queued" is decidable
	events map[ReqID][]report.Event
	images map[ReqID][]Image
	runs   map[ReqID]map[int]*JobRun
	caps   map[Shape]sdserver.Capabilities

	// leases is when each row's claim stops being held.
	//
	// BESIDE THE ROWS AND NOT ON Request, because the real store keeps it in
	// a column that genstore.Request deliberately does not carry: a lease a
	// caller could read back through Get is a lease a caller could write
	// through Update, and an expiry anybody can move is not an expiry. A
	// missing entry is zero, which is expired -- the same reading the column
	// has.
	leases map[ReqID]time.Time

	nextID int
}

// NewRecordingStore returns an empty store.
func NewRecordingStore() *RecordingStore {
	return &RecordingStore{
		Errors: map[string]error{},
		reqs:   map[ReqID]*Request{},
		events: map[ReqID][]report.Event{},
		images: map[ReqID][]Image{},
	}
}

// record notes a call and reports whether it was configured to fail. The
// caller holds s.mu.
func (s *RecordingStore) record(op, arg string) error {
	call := strings.TrimSpace(op + " " + arg)
	s.Calls = append(s.Calls, call)
	if err, ok := s.Errors[call]; ok {
		return err
	}
	if s.FailOn != "" && strings.Contains(call, s.FailOn) {
		return fmt.Errorf("injected failure on %q", call)
	}
	return nil
}

// lease records when a row's claim runs out. The caller holds s.mu.
func (s *RecordingStore) lease(id ReqID, until time.Time) {
	if s.leases == nil {
		s.leases = map[ReqID]time.Time{}
	}
	s.leases[id] = until
}

func (s *RecordingStore) at() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// Enqueue records a request as queued.
func (s *RecordingStore) Enqueue(_ context.Context, r Request) (ReqID, error) {
	if err := r.Validate(); err != nil {
		return "", err
	}
	return s.put("Enqueue", r, StatusQueued, false)
}

// Adopt records a run that is already happening, as running.
//
// StatusRunning and the started stamp, for the reasons the Store interface
// gives: a queued row is claimable, and a claimable `ember up` row is a second
// card rented for a run already in flight.
func (s *RecordingStore) Adopt(_ context.Context, r Request) (ReqID, error) {
	if err := r.ValidateRun(); err != nil {
		return "", err
	}
	return s.put("Adopt", r, StatusRunning, true)
}

// put is the fake's half of SQLite.insert, and it forgets exactly what that
// statement does not name.
//
// NOT A COPY OF THE REQUEST, which is the point of the list below. The real
// INSERT names the ask and nothing a run produces, so a caller that set the
// cost, the node, the memory figures or the model on the way in has them
// dropped against a file on disk -- and a fake that remembered them would be
// kinder than the thing it stands in for in the one direction that matters:
// the caller sees the value come back, builds on it, and the behaviour changes
// the day a real store is underneath. The model was already forgotten here for
// that reason; the rest were not, which is the same divergence one field at a
// time.
func (s *RecordingStore) put(op string, r Request, status Status, started bool) (ReqID, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if r.ID == "" {
		s.nextID++
		r.ID = ReqID(fmt.Sprintf("req-%d", s.nextID))
	}
	if err := s.record(op, string(r.ID)); err != nil {
		return "", err
	}
	if _, dup := s.reqs[r.ID]; dup {
		return "", fmt.Errorf("request %s is already recorded", r.ID)
	}
	if r.Created.IsZero() {
		r.Created = s.at()
	}
	r.Status = status

	copied := r
	copied.Jobs = append([]Job(nil), r.Jobs...)
	copied.RunID, copied.Node = "", ""
	copied.Cost, copied.Rate = 0, 0
	copied.Elapsed, copied.Billable = 0, 0
	copied.LoadMeasured, copied.LoadReported = 0, 0
	copied.MemCgroup = 0
	copied.MemLimit, copied.MemCurrent = 0, 0
	copied.MemTotal, copied.MemCached = 0, 0
	copied.AssetBytes = 0
	copied.Model = ""
	copied.Err = ""
	copied.Ended = time.Time{}
	copied.Started = time.Time{}
	if started {
		// The run has started, because Adopt is called before the create
		// call. Taken from Created when the caller named no other moment, as
		// the real store does it.
		copied.Started = r.Started
		if copied.Started.IsZero() {
			copied.Started = r.Created
		}
	}
	if started {
		// AND THE LEASE, which is the real store's `r.Created.Add(r.TTL)`:
		// Adopt is the only entry point that writes a running row, and a
		// running row with no lease is one the next Reclaim takes. A fake
		// that left it at zero would report an `ember up` as interrupted the
		// instant anything looked.
		s.lease(r.ID, copied.Created.Add(r.TTL))
	}
	s.reqs[r.ID] = &copied
	s.order = append(s.order, r.ID)
	return r.ID, nil
}

// Get returns one request.
func (s *RecordingStore) Get(_ context.Context, id ReqID) (Request, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.record("Get", string(id)); err != nil {
		return Request{}, err
	}
	r, ok := s.reqs[id]
	if !ok {
		return Request{}, fmt.Errorf("%s: %w", id, ErrNotFound)
	}
	return clone(*r), nil
}

// List returns requests newest first.
func (s *RecordingStore) List(_ context.Context, f Filter) ([]Request, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.record("List", string(f.Status)); err != nil {
		return nil, err
	}

	var out []Request
	for _, id := range s.order {
		r := s.reqs[id]
		if !f.matches(*r) {
			continue
		}
		out = append(out, clone(*r))
	}
	// Newest first, as the SQL one orders.
	sort.SliceStable(out, func(i, j int) bool { return out[i].Created.After(out[j].Created) })

	limit := f.Limit
	if limit <= 0 {
		limit = defaultLimit
	}
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// ClaimNext takes the oldest queued request c can serve.
func (s *RecordingStore) ClaimNext(_ context.Context, runID string, c Compat) (Request, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.record("ClaimNext", runID); err != nil {
		return Request{}, false, err
	}

	// Insertion order, which is oldest first -- the same thing ORDER BY
	// created_unix, id answers.
	for _, id := range s.order {
		r := s.reqs[id]
		if r.Status != StatusQueued {
			continue
		}
		// Through WorkloadOf on BOTH sides, which is what the sqlite store's
		// nested CASE does in SQL: a row written before either column holds
		// '', and a compat built from one holds 'sd', so comparing the raw
		// strings would make an upgraded queue unclaimable. A fake that got
		// this right by comparing raw strings while the real store normalised
		// -- or the other way round -- is the divergence the conformance
		// table exists to catch, which is why both spell the same rule.
		if !c.IsZero() && !serves(c, *r) {
			continue
		}
		// AND THE CREATE SIDE: a request some live rental can serve is not
		// taken by a claim looking for work none of them can. Through
		// WorkloadOf on both sides for the reason the ride check is -- see the
		// SQL store, where a raw comparison here hands a migrated row a second
		// card.
		if servedByLive(*r, c.Live) {
			continue
		}
		r.Status = StatusRunning
		r.RunID = runID
		r.Started = s.at()
		// THE LEASE, IN THE SAME BREATH AS THE CLAIM, because the real store
		// writes both in one statement: a fake that left the lease at zero
		// would hand every claimed row straight to the next Reclaim, which is
		// a fake LESS capable than the thing it stands in for in the
		// direction that loses a live rental's row.
		s.lease(id, s.at().Add(LeaseTerm))
		return clone(*r), true, nil
	}
	return Request{}, false, nil
}

// serves is the fake's spelling of the claim relation: could the rental c
// describes run request r.
//
// A SECOND IMPLEMENTATION OF THE PREDICATE, as Spend and Filter.matches here
// already are, and for the same reason: servesClause is where this is most
// likely to be subtly wrong, and a fake that delegated to it could not
// disagree. The conformance table drives both.
//
// THREE ADMISSIONS FOR THE CARD, and only the third needs the advertised fact:
//
//   - the request named no card, so any card this provider has satisfies it;
//   - the request named exactly what this rental asked for, which is the whole
//     of the rule before Compat.Card existed;
//   - the request named something this rental's actual card Accepts, which is
//     the capacity-suffix rule node.GPUModel owns -- "a100" is satisfied by
//     "a100-80g" -- and is also how a rental that asked for nothing serves a
//     request that asked for the card it happens to have got.
//
// THROUGH KindOf ON BOTH SIDES, which is what servesClause's CASE does in SQL:
// a row written before the kind column holds ”, and a compat built from one
// holds 'sd', so comparing the raw strings would make an upgraded queue
// unclaimable. A fake that got this right by comparing raw strings while the
// real store normalised -- or the other way round -- is the divergence the
// conformance table exists to catch.
//
// AND Accepts RATHER THAN A PREFIX WRITTEN OUT HERE. servesClause enumerates
// the spellings a card admits because SQL cannot call Accepts; this one can,
// so it does. Writing the prefix rule a second time is how two copies of one
// rule drift, and the copy in SQL is already as far as that had to go.
func serves(c Compat, r Request) bool {
	// THROUGH WorkloadOf AND NOT KindOf, which is this branch's narrowing
	// applied where master moved the comparison. A declared file may hold two
	// `sd` entries pointing at different images, and a match on the driver
	// alone admits both: same provider, same card, the claim succeeds, and the
	// drain loop hands a request for one model to a resident server holding
	// the other -- after a card has been paid for. Strictly narrower and never
	// wider, because two entries with different drivers necessarily have
	// different ids, so every refusal the driver made is still made.
	if WorkloadOf(r.Workload, r.Kind) != WorkloadOf(c.Workload, c.Kind) ||
		r.Provider != c.Provider {
		return false
	}
	return r.GPU == "" || r.GPU == c.GPU ||
		(c.Card != "" && c.Card.Accepts(node.GPUModel(r.GPU)))
}

// servesPending is serves widened by what is not known yet: the create side's
// question. See servesPendingClause, which is the half with the measurement
// in it.
func servesPending(c Compat, r Request) bool {
	if !c.CardKnown {
		return WorkloadOf(r.Workload, r.Kind) == WorkloadOf(c.Workload, c.Kind) &&
			r.Provider == c.Provider
	}
	return serves(c, r)
}

// servedByLive reports whether any rental already up could take this request.
//
// A FUNCTION RATHER THAN A CONDITION INSIDE THE LOOP, so the fake's spelling of
// the exclusion is one expression that can be read against the SQL store's one
// `AND NOT (...)` -- two rules written in two languages is how `dataCenter`
// against `dataCenterId` stayed invisible, and the conformance table is what
// holds these two to each other.
func servedByLive(r Request, live []Compat) bool {
	for _, l := range live {
		if servesPending(l, r) {
			return true
		}
	}
	return false
}

// began is the real store's first act, reproduced because its absence here is
// what hid #159 from every test in the tree.
//
// A CANCELLED CONTEXT IS A WRITE THAT DID NOT LAND. The sqlite store is
// `database/sql`: the context is checked when a connection is taken, so a
// write on a cancelled one answers `begin: context canceled` BEFORE SQLite is
// reached and leaves the row exactly as it was -- no partial write, and no
// version of it that depends on timing. This fake used to take one happily, so
// a row looked recorded in precisely the case where nothing had been written:
// a fake tidier than the thing it stands in for, which this repository has now
// paid for seven times.
//
// ON THE TWO WRITES A TEARDOWN REACHES, and not on every method. Update is
// called by Queue.finish, Queue.endIfUnfinished, Queue.fail and Recorder.set,
// and AppendEvents by the flush that follows a generation -- all of them while
// the context the rental used may already be cancelled, and all of them
// recording a fact nothing else carries. The one other call that takes a
// rental's context is Queue.once's PutCaps, and a caps cache that did not land
// is logged and harmless: the next request is checked against a card instead
// of against a cache. So a check on the rest would be a guard with no defect
// available to catch.
func began(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	return nil
}

// Update applies fn to a request and writes it back.
func (s *RecordingStore) Update(ctx context.Context, id ReqID, fn func(*Request)) error {
	if err := began(ctx); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.record("Update", string(id)); err != nil {
		return err
	}
	r, ok := s.reqs[id]
	if !ok {
		return fmt.Errorf("%s: %w", id, ErrNotFound)
	}

	next := clone(*r)
	fn(&next)
	if !next.Status.Valid() {
		return fmt.Errorf("request %s: status %q is not one ember writes", id, next.Status)
	}
	// The jobs are not writable, as the interface says: a request whose jobs
	// changed after it ran is a record of something that did not happen.
	next.Jobs = r.Jobs
	next.ID = r.ID
	next.Created = r.Created
	*r = next
	return nil
}

// AppendEvents records progress.
func (s *RecordingStore) AppendEvents(ctx context.Context, id ReqID, es []report.Event) error {
	if len(es) == 0 {
		return nil
	}
	if err := began(ctx); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.record("AppendEvents", string(id)); err != nil {
		return err
	}
	for _, e := range es {
		// Replace by sequence number, as INSERT OR REPLACE does: a
		// re-delivered event is not an error.
		replaced := false
		for i := range s.events[id] {
			if s.events[id][i].Seq == e.Seq {
				s.events[id][i] = e
				replaced = true
				break
			}
		}
		if !replaced {
			s.events[id] = append(s.events[id], e)
		}
	}
	sort.SliceStable(s.events[id], func(i, j int) bool { return s.events[id][i].Seq < s.events[id][j].Seq })
	return nil
}

// Events returns this request's events after a sequence number.
func (s *RecordingStore) Events(_ context.Context, id ReqID, after int64) ([]report.Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.record("Events", string(id)); err != nil {
		return nil, err
	}
	var out []report.Event
	for _, e := range s.events[id] {
		if e.Seq > after {
			out = append(out, e)
		}
	}
	return out, nil
}

// AddImage records a file a request produced.
func (s *RecordingStore) AddImage(_ context.Context, im Image) error {
	if im.Key == "" {
		return fmt.Errorf("an image of %s has no blob key, so nothing could find its bytes", im.Req)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.record("AddImage", im.Key); err != nil {
		return err
	}
	if im.At.IsZero() {
		im.At = s.at()
	}
	for i, have := range s.images[im.Req] {
		if have.Job == im.Job && have.Index == im.Index && have.Preview == im.Preview {
			s.images[im.Req][i] = im
			return nil
		}
	}
	s.images[im.Req] = append(s.images[im.Req], im)
	return nil
}

// Images returns what a request produced, results before previews.
func (s *RecordingStore) Images(_ context.Context, id ReqID) ([]Image, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.record("Images", string(id)); err != nil {
		return nil, err
	}
	out := append([]Image(nil), s.images[id]...)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Preview != out[j].Preview {
			return !out[i].Preview
		}
		if out[i].Job != out[j].Job {
			return out[i].Job < out[j].Job
		}
		return out[i].Index < out[j].Index
	})
	return out, nil
}

// PutCaps caches what the generation server said it can do, for one shape.
func (s *RecordingStore) PutCaps(_ context.Context, sh Shape, c sdserver.Capabilities) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.record("PutCaps", c.Model); err != nil {
		return err
	}
	if s.caps == nil {
		s.caps = map[Shape]sdserver.Capabilities{}
	}
	// KEYED THROUGH KindOf FOR THE REAL STORE'S REASON: the caps table stores
	// the normalised kind, so '' and 'sd' are one row there and must be one
	// entry here, or the fake answers a cached document where SQLite answers
	// "nothing has learned them yet".
	s.caps[normShape(sh)] = c
	return nil
}

// normShape is the key both stores agree on.
func normShape(sh Shape) Shape {
	sh.Kind = KindOf(sh.Kind)
	return sh
}

// Caps reads the cached capabilities for one shape back.
func (s *RecordingStore) Caps(_ context.Context, sh Shape) (sdserver.Capabilities, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.record("Caps", ""); err != nil {
		return sdserver.Capabilities{}, false, err
	}
	c, ok := s.caps[normShape(sh)]
	return c, ok, nil
}

// Renew extends the lease on the rows the caller is still responsible for.
func (s *RecordingStore) Renew(_ context.Context, ids []ReqID) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.record("Renew", ""); err != nil {
		return 0, err
	}
	n := 0
	for _, id := range ids {
		r, ok := s.reqs[id]
		// Running only, for the real store's reason: a renewal races a
		// request's own ending, and a terminal row carrying a live lease is a
		// state nothing reads and nothing would notice.
		if !ok || r.Status != StatusRunning {
			continue
		}
		s.lease(id, s.at().Add(LeaseTerm))
		n++
	}
	return n, nil
}

// Reclaim marks interrupted every running request whose lease has expired.
// Retry is the fake's half of the one-statement rule, spelled as the same
// condition the SQL store's WHERE is.
//
// THE THREE TERMINAL STATUSES AND NOT "NOT QUEUED OR RUNNING", because the
// two readings differ on `done`: a finished request has nothing to retry, and
// a fake that let one through would disagree with the SQL store on the row an
// operator is most likely to press by accident. The conformance table is what
// holds the two to each other.
func (s *RecordingStore) Retry(_ context.Context, id ReqID) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.record("Retry", string(id)); err != nil {
		return false, err
	}
	r, ok := s.reqs[id]
	if !ok {
		return false, nil
	}
	switch r.Status {
	case StatusFailed, StatusCancelled, StatusInterrupted:
	default:
		return false, nil
	}
	r.Status = StatusQueued
	r.Err = ""
	r.Node = ""
	r.Offer = ""
	r.Started = time.Time{}
	r.Ended = time.Time{}
	// THE COST SURVIVES. A request that failed after renting has already spent
	// money, and the row is the total across every attempt.
	delete(s.leases, id)
	return true, nil
}

func (s *RecordingStore) Reclaim(_ context.Context) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.record("Reclaim", ""); err != nil {
		return 0, err
	}
	now := s.at()
	n := 0
	for _, id := range s.order {
		if s.reqs[id].Status != StatusRunning {
			continue
		}
		// NOT After, SO THAT A LEASE EXPIRING EXACTLY NOW IS EXPIRED: the SQL
		// half is `lease_until_unix <= ?`, and a fake using the strict
		// comparison would disagree with it on the one instant a test with an
		// injected clock is most likely to pick.
		if s.leases[id].After(now) {
			continue
		}
		s.reqs[id].Status = StatusInterrupted
		s.reqs[id].Ended = now
		n++
	}
	return n, nil
}

// PruneEvents drops the events of finished requests that ended before the
// horizon.
//
// A SECOND IMPLEMENTATION OF THE PREDICATE, as Spend and Filter.matches here
// already are, and for the same reason: the SQL half is where this is most
// likely to be subtly wrong, and a fake that delegated to it could not
// disagree. The conformance table drives both and compares what each one
// removed AND what each one kept, because a prune that takes a row it should
// have left is the failure mode, and a prune that takes nothing passes any
// assertion that only counts what went.
func (s *RecordingStore) PruneEvents(_ context.Context, before time.Time, dryRun bool) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.record("PruneEvents", before.UTC().Format(time.RFC3339)); err != nil {
		return 0, err
	}

	n := 0
	for _, id := range s.order {
		r, ok := s.reqs[id]
		if !ok {
			continue
		}
		// Ended and before the horizon, which is the whole of the SQL
		// predicate. A queued or running request has no end, so its events
		// are unreachable from here -- the live-stream case is excluded by
		// the shape of the test rather than by a separate filter.
		if r.Ended.IsZero() || !r.Ended.Before(before) {
			continue
		}
		n += len(s.events[id])
		if !dryRun {
			delete(s.events, id)
		}
	}
	return n, nil
}

// DropImages forgets the image rows of one request.
func (s *RecordingStore) DropImages(_ context.Context, id ReqID) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.record("DropImages", string(id)); err != nil {
		return 0, err
	}
	n := len(s.images[id])
	delete(s.images, id)
	return n, nil
}

// Usage counts what the history holds.
func (s *RecordingStore) Usage(_ context.Context) (Usage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.record("Usage", ""); err != nil {
		return Usage{}, err
	}

	var u Usage
	for _, id := range s.order {
		r, ok := s.reqs[id]
		if !ok {
			continue
		}
		u.Requests++
		u.Jobs += len(r.Jobs)
		u.Events += len(s.events[id])
		u.Images += len(s.images[id])
		for _, im := range s.images[id] {
			u.ImageBytes += im.Bytes
		}

		// Finished, reported images, and has none left -- which is what
		// `ember history rm` leaves behind. Read off the job runs rather than
		// off the image rows, because that is where the count survives a
		// prune, exactly as the SQL reads jobs.images.
		if r.Ended.IsZero() || len(s.images[id]) > 0 {
			continue
		}
		for _, j := range s.runsOf(id) {
			if j.Images > 0 {
				u.Reclaimed++
				break
			}
		}
	}
	return u, nil
}

// Close is a no-op. It exists so a caller can hold the interface and not care
// which implementation it has.
func (s *RecordingStore) Close() error { return nil }

// Err returns the first injected error, for a test that wants to assert it was
// the one that came back rather than something else.
func (s *RecordingStore) Err(call string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err, ok := s.Errors[call]; ok {
		return err
	}
	return errors.New("no error was injected for " + call)
}

// clone copies a request so a caller cannot mutate the store's own copy.
//
// The slice matters: without it, a caller appending to the Jobs it was handed
// writes into the store, and the store then holds jobs nobody enqueued.
func clone(r Request) Request {
	r.Jobs = append([]Job(nil), r.Jobs...)
	return r
}

// Both implementations satisfy the interface, or the seam is not a seam.
var (
	_ Store = (*RecordingStore)(nil)
	_ Store = (*SQLite)(nil)
)

// runs is what each request's jobs did, by request and 1-based job index.
//
// A second map rather than fields on the stored Request, because the fake
// has to be able to disagree with itself the way the real store can: a job
// index nothing wrote is absent here and reads as ErrNotFound, which is how
// the off-by-one that writes every figure to nowhere shows up in a test
// rather than in a database.
func (s *RecordingStore) runsOf(id ReqID) map[int]*JobRun {
	if s.runs == nil {
		s.runs = map[ReqID]map[int]*JobRun{}
	}
	if s.runs[id] == nil {
		s.runs[id] = map[int]*JobRun{}
	}
	return s.runs[id]
}

func (s *RecordingStore) UpdateJob(_ context.Context, id ReqID, idx int, fn func(*JobRun)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.record("UpdateJob", fmt.Sprintf("%s/%d", id, idx)); err != nil {
		return err
	}
	r, ok := s.reqs[id]
	if !ok {
		return fmt.Errorf("%s: %w", id, ErrNotFound)
	}
	// The index is checked against the jobs that exist, exactly as the real
	// store's WHERE clause does. A fake that accepted any index would accept
	// the off-by-one this is most likely to be given.
	if idx < 1 || idx > len(r.Jobs) {
		return fmt.Errorf("job %d of %s: %w", idx, id, ErrNotFound)
	}

	runs := s.runsOf(id)
	j := runs[idx]
	if j == nil {
		j = &JobRun{Req: id, Idx: idx}
		runs[idx] = j
	}
	fn(j)
	j.Req, j.Idx = id, idx
	return nil
}

func (s *RecordingStore) JobRuns(_ context.Context, id ReqID) ([]JobRun, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.record("JobRuns", string(id)); err != nil {
		return nil, err
	}
	r, ok := s.reqs[id]
	if !ok {
		// Nothing, and no error, because that is what the real store does
		// and what Images and Events already do for an absent request: these
		// are "list the children of" reads, and SQLite answers one with zero
		// rows. A fake that returned ErrNotFound here would be STRICTER than
		// the thing it stands in for, which is the usual danger inverted and
		// just as bad -- a caller would grow a branch for an error the real
		// store never returns. Found by the conformance test, which is what
		// it is for.
		return nil, nil
	}
	runs := s.runsOf(id)
	out := make([]JobRun, 0, len(r.Jobs))
	for i := 1; i <= len(r.Jobs); i++ {
		if j := runs[i]; j != nil {
			out = append(out, *j)
			continue
		}
		// A job nothing has reported on is still a job. Omitting it would
		// make a half-finished request look like a shorter one.
		out = append(out, JobRun{Req: id, Idx: i})
	}
	return out, nil
}

// Spend aggregates in Go what SQLite aggregates in SQL.
//
// A second implementation of the arithmetic, which is normally the thing to
// avoid -- and here it is the point: the SQL is the part most likely to be
// subtly wrong (the job-side subquery exists because a plain join multiplies
// each rental's cost by its job count), and a fake that delegated to it could
// not disagree. The two are checked against each other by the store's own
// conformance test, so a change to one that the other does not make is a
// failure rather than a divergence nobody notices.
func (s *RecordingStore) Spend(_ context.Context, q SpendQuery) ([]Bucket, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.record("Spend", string(q.By)); err != nil {
		return nil, err
	}
	if !q.By.Valid() {
		return nil, fmt.Errorf("cannot group spend by %q; ember groups by %v or not at all",
			q.By, GroupBys())
	}

	byKey := map[string]*Bucket{}
	for _, id := range s.order {
		r, ok := s.reqs[id]
		if !ok {
			continue
		}
		// Half-open, as spendWhere is: Since inclusive, Until exclusive, so
		// consecutive periods tile without a request landing in both.
		if !q.Since.IsZero() && r.Created.Before(q.Since) {
			continue
		}
		if !q.Until.IsZero() && !r.Created.Before(q.Until) {
			continue
		}

		k := ""
		switch q.By {
		case ByProvider:
			k = r.Provider
		case ByGPU:
			k = r.GPU
		case ByDay:
			k = r.Created.UTC().Format("2006-01-02")
		case ByModel:
			// The stored value and not a fallback, so a request whose model
			// nobody read groups under the empty key here exactly as it does
			// under `r.model` in SQL. A fake that substituted "unknown" would
			// make the conformance test disagree about the one row whose
			// answer is "ember does not know".
			k = r.Model
		}
		b := byKey[k]
		if b == nil {
			b = &Bucket{Key: k}
			byKey[k] = b
		}

		b.Requests++
		b.RunCost += r.Cost
		b.Billable += r.Billable

		runs := s.runsOf(id)
		recorded := 0
		for i := 1; i <= len(r.Jobs); i++ {
			b.Jobs++
			j := runs[i]
			if j == nil {
				continue
			}
			b.Images += j.Images
			b.Bytes += j.Bytes
			b.JobCost += j.Cost
			b.Steps += j.Steps
			b.Sampling += j.Sampling()
			if !j.Ended.IsZero() {
				recorded++
			}
		}
		// The same conditions unrecorded applies, spelled against the values
		// this store has: a request that has not finished is not missing
		// anything, and one that finished with no job reporting is -- whether
		// it had jobs that never reported or, after Adopt, no jobs at all.
		if unrecorded(unixOrZero(r.Ended), int64(recorded)) {
			b.Unrecorded++
		}
	}

	keys := make([]string, 0, len(byKey))
	for k := range byKey {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]Bucket, 0, len(keys))
	for _, k := range keys {
		out = append(out, *byKey[k])
	}
	return out, nil
}

// matches is the fake's half of filterWhere.
//
// A second implementation of the same predicate, which is normally the thing
// to avoid and here is the point: TestRecordingAgreesWithSQLite drives both
// stores through the same sequence and compares, and a fake that delegated to
// the SQL could not disagree with it. The danger it guards against is the one
// the conformance table already caught once in the other direction -- a fake
// stricter than the store it stands in for grows callers a branch for an error
// nothing returns.
func (f Filter) matches(r Request) bool {
	if f.Status != "" && r.Status != f.Status {
		return false
	}
	// Half-open, as clauses.period is: Since includes, Until excludes.
	if !f.Since.IsZero() && r.Created.Before(f.Since) {
		return false
	}
	if !f.Until.IsZero() && !r.Created.Before(f.Until) {
		return false
	}
	if f.Provider != "" && r.Provider != f.Provider {
		return false
	}
	if f.GPU != "" && r.GPU != f.GPU {
		return false
	}
	return true
}

// History returns the ledger, newest first.
func (s *RecordingStore) History(_ context.Context, f Filter) ([]Entry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.record("History", string(f.Status)); err != nil {
		return nil, err
	}
	if f.Status != "" && !f.Status.Valid() {
		return nil, fmt.Errorf("status %q is not one ember writes", f.Status)
	}

	// Which rentals somebody else paid for, so a request that rode along can
	// be told from one whose cost never arrived. Billable as well as cost
	// because a free rental's cost is legitimately zero.
	paid := map[string]ReqID{}
	for _, id := range s.order {
		r := s.reqs[id]
		if r.RunID != "" && (r.Cost > 0 || r.Billable > 0) {
			paid[r.RunID] = id
		}
	}

	var out []Entry
	for _, id := range s.order {
		r, ok := s.reqs[id]
		if !ok || !f.matches(*r) {
			continue
		}

		e := Entry{
			ID: r.ID, Created: r.Created, Status: r.Status,
			Provider: r.Provider, GPU: r.GPU, Model: r.Model,
			RunID: r.RunID, Node: r.Node,
			Started: r.Started, Ended: r.Ended,
			Cost: r.Cost, Rate: r.Rate,
			Elapsed: r.Elapsed, Billable: r.Billable,
			MemCgroup: r.MemCgroup,
			MemLimit:  r.MemLimit, MemCurrent: r.MemCurrent,
			MemTotal: r.MemTotal, MemCached: r.MemCached,
			AssetBytes: r.AssetBytes,
			Jobs:       len(r.Jobs),
			Err:        r.Err,
		}
		if opener, ok := paid[r.RunID]; ok && opener != r.ID {
			e.Rode = true
		}

		runs := s.runsOf(id)
		recorded := 0
		for i := 1; i <= len(r.Jobs); i++ {
			j := runs[i]
			if j == nil {
				continue
			}
			e.Images += j.Images
			e.Bytes += j.Bytes
			e.JobCost += j.Cost
			e.Steps += j.Steps
			e.Sampling += j.Sampling()
			if !j.Ended.IsZero() {
				recorded++
			}
		}
		e.Unrecorded = unrecorded(unixOrZero(r.Ended), int64(recorded))
		out = append(out, e)
	}

	// Newest first, as the SQL one orders.
	sort.SliceStable(out, func(i, j int) bool { return out[i].Created.After(out[j].Created) })

	limit := f.Limit
	if limit <= 0 {
		limit = defaultLimit
	}
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}
