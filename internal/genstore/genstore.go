// Package genstore records what was asked for and what came back.
//
// WHY EMBER NEEDS A DATABASE AT ALL. internal/statestore is a journal of live
// nodes: it exists so that a GPU billing by the hour cannot be invisible, and
// a clean run leaves nothing in it. That is the right shape for its job and
// the wrong shape for this one. A queue outlives the process that filled it, a
// generation request has a history worth keeping after its node is gone, and a
// browser reconnecting has to be told what it missed.
//
// IT IS NOT A SECOND AUTHORITY OVER LIVE NODES. The provider's own label is
// that, and internal/statestore is its cache -- "the label is the source of
// truth; the journal is a cache", and `ember ls` keeps reading the journal.
// Nothing here is consulted to decide whether a node exists or should be
// destroyed. A row saying "running" over a node that is gone is a stale row,
// not a leak.
//
// BEHIND AN INTERFACE, with a real implementation and a Recording fake, which
// is what AGENTS.md asks of every external system here. The engine is
// swappable because somebody will want Postgres, and because a seam is the
// only honest way to test the layers above without a file on disk.
//
// MONEY IS AN INTEGER COLUMN. ParseFloat("0.29")*1e6 is 289999, which is why
// node.ParseUSD is hand-written, and a budget that survives a round trip
// through a REAL column is a budget that is sometimes wrong by a rounding
// error. cfg and guidance *are* REAL, and that is not the same decision: they
// are generation parameters the API takes as float64, SQLite's REAL is the
// same IEEE-754 double, and the round trip is exact.
package genstore

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/Legion112/ember/internal/node"
	"github.com/Legion112/ember/internal/report"
	"github.com/Legion112/ember/internal/sdserver"
)

// ErrNotFound means no such request. Distinguished from a read failure
// because one is a 404 and the other is a 500.
var ErrNotFound = errors.New("no such request")

// ReqID identifies one queued batch.
type ReqID string

// Status is where a request has got to.
type Status string

const (
	// StatusQueued is waiting for a rental. This is the only status
	// ClaimNext will take.
	StatusQueued Status = "queued"

	// StatusRunning means a rental is generating it.
	StatusRunning Status = "running"

	StatusDone      Status = "done"
	StatusFailed    Status = "failed"
	StatusCancelled Status = "cancelled"

	// StatusInterrupted is what a restart writes over anything it found
	// running.
	//
	// It is a separate status and not StatusFailed, because the two are
	// different facts and only one of them is about the generation: a failed
	// request was refused or broke, and an interrupted one was fine until the
	// process holding its rental went away. The rental cannot be resumed --
	// the node is gone -- so the request is not re-queued either, because
	// silently renting a second card for work the operator may no longer
	// want is the kind of help nobody asked for. Whatever images it had
	// already produced stay listed: they were paid for.
	StatusInterrupted Status = "interrupted"
)

// Terminal reports whether nothing more will happen to a request in this
// status.
func (s Status) Terminal() bool {
	switch s {
	case StatusDone, StatusFailed, StatusCancelled, StatusInterrupted:
		return true
	}
	return false
}

// Valid reports whether s is a status this package writes.
//
// Checked on the way in, because a status nothing recognises is a request the
// queue will neither run nor report -- it would simply sit there.
func (s Status) Valid() bool {
	switch s {
	case StatusQueued, StatusRunning, StatusDone, StatusFailed, StatusCancelled, StatusInterrupted:
		return true
	}
	return false
}

// Job is one image to generate.
//
// IT IS DELIBERATELY NOT sdserver.Job, and the missing field is the point.
// That type carries Extra, which is merged into the generation request and
// therefore reaches every field of the API ember does not type -- including
// the ones naming file paths on the node. A request that could set it could
// point a running sd-server at weights the config never mentioned, which is
// the hazard sdserver.ServeCommand is sealed against. There is no Extra here,
// so a request that arrives over a wire cannot carry one and a round trip
// through this store cannot invent one.
//
// A config can still use Extra: internal/config's own SDJob has it, and `ember
// up` reads that file. The difference is who typed it.
type Job struct {
	Prompt         string `json:"prompt"`
	NegativePrompt string `json:"negative_prompt,omitempty"`

	Width      int   `json:"width,omitempty"`
	Height     int   `json:"height,omitempty"`
	Steps      int   `json:"steps,omitempty"`
	BatchCount int   `json:"batch_count,omitempty"`
	Seed       int64 `json:"seed,omitempty"`

	Sampler   string `json:"sampler,omitempty"`
	Scheduler string `json:"scheduler,omitempty"`

	// CFG is the prompt guidance and Guidance is the distilled guidance. Two
	// numbers that both get called "cfg" in conversation -- the flux2-dev
	// recipe sets cfg-scale 1 and guidance 4 -- so conflating them would
	// quietly generate at the wrong one.
	CFG      float64 `json:"cfg,omitempty"`
	Guidance float64 `json:"guidance,omitempty"`

	// Output is the local filename stem and never reaches the server.
	Output string `json:"output,omitempty"`

	// Params are the optional generation parameters.
	//
	// sdserver.Params itself rather than a mirror of it, because this package
	// already imports sdserver for Job.SD and Caps, and a second copy of
	// twenty-three fields is twenty-three chances for the two to disagree
	// about which one is a pointer. It is stored as the JSON of this struct
	// in one column; see sqlite.go's migrations for why one.
	Params sdserver.Params `json:"params,omitzero"`
}

// SD converts to the type the generation client takes.
//
// Extra is left nil rather than set to an empty map, which is the same thing
// to sdserver.Job.Body and says more plainly that there is nothing to merge.
func (j Job) SD() sdserver.Job {
	return sdserver.Job{
		Prompt:         j.Prompt,
		NegativePrompt: j.NegativePrompt,
		Width:          j.Width,
		Height:         j.Height,
		Steps:          j.Steps,
		BatchCount:     j.BatchCount,
		Seed:           j.Seed,
		Sampler:        j.Sampler,
		Scheduler:      j.Scheduler,
		CFG:            j.CFG,
		Guidance:       j.Guidance,
		Output:         j.Output,
		Params:         j.Params,
	}
}

// Request is a batch of jobs queued as one unit.
//
// A BATCH AND NOT A JOB, because the whole argument for the served shape is
// that one rental pays the per-rental costs once. A request is the unit an
// operator pushes, cancels and is billed for.
type Request struct {
	ID      ReqID     `json:"id"`
	Created time.Time `json:"created"`
	Status  Status    `json:"status"`

	// Provider and GPU are the selection. They are also what decides whether
	// a queued request can join a rental that is already up -- see Compat.
	Provider string `json:"provider"`
	GPU      string `json:"gpu,omitempty"`

	// Offer is the token actually rented, empty until a rental picks one.
	Offer string `json:"offer,omitempty"`

	// TTL, MaxHourly and MaxRun are this request's ceilings, already narrowed
	// against the config's by whoever enqueued it. Nothing here enforces
	// them: this is a record of what was asked for.
	TTL       time.Duration `json:"ttl_ns"`
	MaxHourly node.Rate     `json:"max_hourly_micro_usd,omitempty"`
	MaxRun    node.MicroUSD `json:"max_run_micro_usd,omitempty"`

	Preview         string `json:"preview,omitempty"`
	PreviewInterval int    `json:"preview_interval,omitempty"`

	Jobs []Job `json:"jobs"`

	// RunID and Node name the rental that served it, for joining a request to
	// what `ember ls` reports.
	RunID string `json:"run_id,omitempty"`
	Node  string `json:"node,omitempty"`

	Started time.Time `json:"started,omitempty"`
	Ended   time.Time `json:"ended,omitempty"`

	// Cost is what the rental came to, from provision's own estimate.
	Cost node.MicroUSD `json:"cost_micro_usd,omitempty"`

	Err string `json:"error,omitempty"`
}

// Validate refuses a request this store should not hold.
//
// On the way in, because a row nothing can run is worse than a refusal: it
// sits in the queue, the operator sees it pending, and nothing ever picks it
// up.
func (r Request) Validate() error {
	if r.Provider == "" {
		return errors.New("a request names no provider")
	}
	if len(r.Jobs) == 0 {
		return errors.New("a request with no jobs would rent a card, load the weights and generate nothing")
	}
	if r.TTL <= 0 {
		return errors.New("a request with no TTL would rent a node nothing destroys on a deadline")
	}
	if r.Status != "" && !r.Status.Valid() {
		return fmt.Errorf("status %q is not one ember writes", r.Status)
	}
	return nil
}

// Compat is what a live rental can serve.
//
// THE RULE, and it is deliberately narrow: a queued request joins a rental
// that is already up when the provider and the GPU selection are identical.
// Everything else about the rental was settled when its offer was chosen, and
// the model is fixed by `sd.serve` in the config -- which a request may not
// touch -- so every request in one serve process targets the same weights and
// compatibility is mostly automatic.
//
// What a request cannot do is ask for a longer life than the node it joins has
// left. That is not checked here: the node's own deadline ends the rental, and
// a batch that needs longer needs a larger ttl. Checking it would mean
// predicting how long a generation takes, which nothing here can do.
//
// The zero Compat means no rental is up, and ClaimNext will then take the
// oldest queued request whatever it asked for.
type Compat struct {
	Provider string
	GPU      string
}

// IsZero reports whether no rental is up.
func (c Compat) IsZero() bool { return c.Provider == "" }

// Image is one file a request produced.
type Image struct {
	Req ReqID `json:"req"`

	// Job is 1-based, matching how a job is labelled in output. Index is the
	// image within that job, which batch_count can make more than one.
	Job   int `json:"job"`
	Index int `json:"index"`

	// Key is where the bytes are, in blob.Store terms. The row is the join
	// between a request and its results; swapping the blob backend changes
	// nothing here.
	Key   string `json:"key"`
	Bytes int64  `json:"bytes"`

	// Preview marks a progress frame rather than a result. They are in one
	// table because they are both images of the same job, and separated by
	// this column because a preview is overwritten as sampling proceeds and a
	// result is not.
	Preview bool      `json:"preview,omitempty"`
	At      time.Time `json:"at"`
}

// Filter narrows a listing.
type Filter struct {
	// Status, empty for every status.
	Status Status

	// Limit caps how many come back, newest first. Zero means a default the
	// implementation chooses: an unbounded listing of every request ever made
	// is not an answer to "what is in the queue".
	Limit int
}

// Store is the record of what was asked for and what came back.
//
// Narrow on purpose. Everything here is either a write the queue makes or a
// read the API serves; there is no general query surface, because a general
// query surface is what makes an engine hard to swap.
type Store interface {
	// Enqueue records a request as queued and returns its id.
	Enqueue(ctx context.Context, r Request) (ReqID, error)

	// Get returns one request with its jobs, or ErrNotFound.
	Get(ctx context.Context, id ReqID) (Request, error)

	// List returns requests newest first.
	List(ctx context.Context, f Filter) ([]Request, error)

	// ClaimNext atomically takes the oldest queued request that c can serve
	// and marks it running, or reports that there is none.
	//
	// ONE STATEMENT, and that is the whole point of the method. The drain
	// loop's decision is "serve another or destroy the node", and a
	// read-then-write would leave a window where a request pushed between the
	// two is neither claimed nor counted -- so the node is destroyed with
	// work outstanding and the operator's request sits in a queue nothing is
	// draining.
	ClaimNext(ctx context.Context, runID string, c Compat) (Request, bool, error)

	// Update applies fn to a request and writes it back.
	//
	// The jobs are not writable: they are what was asked for, and a request
	// whose jobs changed after it ran is a record of something that did not
	// happen.
	Update(ctx context.Context, id ReqID, fn func(*Request)) error

	// AppendEvents records progress. Events are keyed by sequence number so a
	// client can ask for everything after what it has seen.
	AppendEvents(ctx context.Context, id ReqID, es []report.Event) error

	// Events returns this request's events with Seq greater than after, in
	// order.
	Events(ctx context.Context, id ReqID, after int64) ([]report.Event, error)

	// AddImage records a file a request produced.
	AddImage(ctx context.Context, im Image) error

	// Images returns what a request produced, results and previews.
	Images(ctx context.Context, id ReqID) ([]Image, error)

	// PutCaps caches what the generation server said it can do, and Caps
	// reads it back.
	//
	// CACHED BECAUSE A SAMPLER TYPO SHOULD NOT COST A RENTAL. sdserver.Validate
	// needs the server's own capability list, which is only available once a
	// node is up and the weights are loaded -- so without this, a mistyped
	// sampler is refused about a minute into a rented card instead of at the
	// moment it was typed. `sd.serve` is fixed by the config, so the
	// capabilities are the same for every rental in one process: the first one
	// learns them and every later enqueue is checked against them.
	PutCaps(ctx context.Context, c sdserver.Capabilities) error
	Caps(ctx context.Context) (sdserver.Capabilities, bool, error)

	// Interrupt marks everything it finds running as interrupted, and returns
	// how many. Called once at startup: a request that was in flight when the
	// process died cannot be resumed, because its rental is gone.
	Interrupt(ctx context.Context) (int, error)

	Close() error
}
