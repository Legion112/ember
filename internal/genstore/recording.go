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
		if f.Status != "" && r.Status != f.Status {
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
