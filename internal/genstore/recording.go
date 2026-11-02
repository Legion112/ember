package genstore

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

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
	caps   *sdserver.Capabilities
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
	s.mu.Lock()
	defer s.mu.Unlock()

	if r.ID == "" {
		s.nextID++
		r.ID = ReqID(fmt.Sprintf("req-%d", s.nextID))
	}
	if err := s.record("Enqueue", string(r.ID)); err != nil {
		return "", err
	}
	if _, dup := s.reqs[r.ID]; dup {
		return "", fmt.Errorf("request %s is already recorded", r.ID)
	}
	if r.Created.IsZero() {
		r.Created = s.at()
	}
	r.Status = StatusQueued

	copied := r
	copied.Jobs = append([]Job(nil), r.Jobs...)
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
		if !c.IsZero() && (r.Provider != c.Provider || r.GPU != c.GPU) {
			continue
		}
		r.Status = StatusRunning
		r.RunID = runID
		r.Started = s.at()
		return clone(*r), true, nil
	}
	return Request{}, false, nil
}

// Update applies fn to a request and writes it back.
func (s *RecordingStore) Update(_ context.Context, id ReqID, fn func(*Request)) error {
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
func (s *RecordingStore) AppendEvents(_ context.Context, id ReqID, es []report.Event) error {
	if len(es) == 0 {
		return nil
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

// PutCaps caches what the generation server said it can do.
func (s *RecordingStore) PutCaps(_ context.Context, c sdserver.Capabilities) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.record("PutCaps", c.Model); err != nil {
		return err
	}
	s.caps = &c
	return nil
}

// Caps reads the cached capabilities back.
func (s *RecordingStore) Caps(_ context.Context) (sdserver.Capabilities, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.record("Caps", ""); err != nil {
		return sdserver.Capabilities{}, false, err
	}
	if s.caps == nil {
		return sdserver.Capabilities{}, false, nil
	}
	return *s.caps, true, nil
}

// Interrupt marks everything running as interrupted.
func (s *RecordingStore) Interrupt(_ context.Context) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.record("Interrupt", ""); err != nil {
		return 0, err
	}
	n := 0
	for _, id := range s.order {
		if s.reqs[id].Status == StatusRunning {
			s.reqs[id].Status = StatusInterrupted
			s.reqs[id].Ended = s.at()
			n++
		}
	}
	return n, nil
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
		// The same three conditions unrecorded applies, spelled against the
		// values this store has: a request that has not finished is not
		// missing anything.
		if !r.Ended.IsZero() && len(r.Jobs) > 0 && recorded == 0 {
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
			Provider: r.Provider, GPU: r.GPU,
			RunID: r.RunID, Node: r.Node,
			Started: r.Started, Ended: r.Ended,
			Cost: r.Cost, Rate: r.Rate,
			Elapsed: r.Elapsed, Billable: r.Billable,
			Jobs: len(r.Jobs),
			Err:  r.Err,
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
		e.Unrecorded = unrecorded(unixOrZero(r.Ended), e.Jobs, int64(recorded))
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
