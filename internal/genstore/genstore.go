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
// MONEY IS AN INTEGER COLUMN. ParseFloat("2.01")*1e6 truncates to 2009999,
// which is why node.ParseUSD is hand-written, and a budget that survives a
// round trip
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

// Statuses is every status this package writes, so a command's flag help and
// Valid cannot disagree about the set. The mirror of GroupBys.
func Statuses() []Status {
	return []Status{
		StatusQueued, StatusRunning, StatusDone,
		StatusFailed, StatusCancelled, StatusInterrupted,
	}
}

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

// FromSD is the inverse of SD, for a batch that came from a config file
// rather than over a wire.
//
// IT DROPS Extra, AND THAT IS THE SEAL RATHER THAN AN OMISSION. There is no
// field here to put it in, deliberately -- see this type's own comment -- so
// recording a config's `extra:` map would mean adding one, and the field that
// reaches every untyped part of the generation API would then exist on the
// type a browser posts. The seal is worth more than the fidelity, so what is
// recorded of such a job is its typed ask and the event stream's own
// description of it, and the honest statement is that the recorded ask of a
// job using `extra:` is INCOMPLETE rather than wrong. Nothing else is lost:
// `ember up` is the only caller, a request over HTTP cannot carry Extra in
// the first place, and the costs and timings -- which are what #47 is about
// -- come from the events either way.
func FromSD(j sdserver.Job) Job {
	return Job{
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

	// Rate is what the offer cost per hour, and Elapsed and Billable are how
	// long the rental ran and how long it was charged for.
	//
	// Billable is kept beside Elapsed rather than instead of it, because they
	// differ wherever a provider has a minimum: a node destroyed after four
	// seconds on a one-minute minimum costs a minute. An overhead figure
	// computed against Elapsed would credit ember with a saving the invoice
	// does not show.
	Rate     node.Rate     `json:"rate_micro_usd_per_hour,omitempty"`
	Elapsed  time.Duration `json:"elapsed_ns,omitempty"`
	Billable time.Duration `json:"billable_ns,omitempty"`

	// LoadMeasured is the model-load span ember timed, and LoadReported what
	// the server claimed it was.
	//
	// Both, because the only measurement anybody has made of these had them
	// disagreeing by two orders of magnitude: sd-server reported 551ms for
	// weights that are 56 GiB. Keeping one would mean choosing which figure
	// to be wrong with.
	LoadMeasured time.Duration `json:"load_measured_ns,omitempty"`
	LoadReported time.Duration `json:"load_reported_ns,omitempty"`

	// MemLimit and MemCurrent are the node's own cgroup memory limit and
	// usage; MemTotal and MemCached are the host's, as /proc/meminfo reports
	// them inside a container; AssetBytes is the weights this run was about
	// to read.
	//
	// ALL OF THEM, BECAUSE THE TWO VIEWS DISAGREE AND ONLY ONE BOUNDS THE
	// PAGE CACHE. Every node in the FLUX.2 measurement reported 620-708 GiB
	// free and two of them differed by 81 seconds and $0.056 per image
	// depending on whether the weights stayed cached, which is what `free`
	// inside a container reporting the host's memory looks like from the
	// outside.
	//
	// MemLimit zero means there is NO cgroup limit rather than a limit of
	// nothing, so MemCgroup is what says whether anything was read at all.
	// Written through from the same event that goes into `events`, on the
	// precedent Cost and the load spans set; nothing backfills, because the
	// facts do not exist for rentals that already happened.
	MemCgroup  int   `json:"mem_cgroup,omitempty"`
	MemLimit   int64 `json:"mem_limit_bytes,omitempty"`
	MemCurrent int64 `json:"mem_current_bytes,omitempty"`
	MemTotal   int64 `json:"mem_total_bytes,omitempty"`
	MemCached  int64 `json:"mem_cached_bytes,omitempty"`
	AssetBytes int64 `json:"asset_bytes,omitempty"`

	// Model is which model served this request, as the generation server
	// reported it rather than as the config names it.
	//
	// NOT PART OF THE ASK, AND STRUCTURALLY SO. Enqueue's INSERT does not
	// name the column, exactly as it does not name cost or node: a request
	// does not choose a model -- `sd.serve` does, and a request may not touch
	// it -- so a caller that set this before enqueueing would have it
	// dropped. It is written by the queue, from the same /capabilities
	// response PutCaps caches, at the moment a rental claims the request.
	//
	// THE CONFIG IS NOT THE ANSWER even though it is right in front of
	// whoever asks. An operator can edit `sd.serve` between two rentals, so a
	// reading of the config attributes last week's spend to this week's
	// weights -- the `allowedCudaVersions` mistake in another costume, a
	// value read from the wrong place and believed.
	//
	// Empty means nobody read it, which is a request that never got a node or
	// one from before schema version 5. Both readers print a dash for it;
	// nothing backfills, because the only stored copy of a past model is the
	// cached caps row the next rental overwrites.
	Model string `json:"model,omitempty"`

	Err string `json:"error,omitempty"`
}

// JobRun is what one job did, as opposed to what it asked for.
//
// SEPARATE FROM Job, AND THAT IS THE POINT. Job is the wire type a browser
// posts: every field on it is something a request may set. Putting Cost on it
// would make "what this cost" a field a request could send, which is the same
// structural refusal that keeps Extra off Job -- a value the operator is
// billed against must not be settable by the thing being billed. These are
// written by the queue from the events the run emitted, and only read back.
//
// It shares the jobs table with Job because they are two halves of one job:
// the ask and the outcome. A second table would need the same composite key
// and a join to answer any question worth asking.
type JobRun struct {
	Req ReqID `json:"req"`

	// Idx is 1-based, matching how a job is labelled in output and in the
	// events that report it.
	Idx int `json:"idx"`

	Started time.Time `json:"started,omitempty"`
	Ended   time.Time `json:"ended,omitempty"`

	// Cost is this job's own card time, which is not its share of the bill.
	// The rental also paid for provisioning, the image pull and the asset
	// verify once for everything it served, and that difference is the entire
	// saving the served shape exists for.
	Cost node.MicroUSD `json:"cost_micro_usd,omitempty"`

	Images int   `json:"images,omitempty"`
	Bytes  int64 `json:"bytes,omitempty"`

	// Passes is how many sampling passes ran and Steps the steps across all
	// of them.
	//
	// Steps is summed from each pass's own declared total, never counted from
	// the frames ember saw: ember deduplicates progress on the step, so at a
	// poll slower than the card samples it observes fewer frames than there
	// were steps, and a count of frames would measure the poll interval
	// instead of the work. Passes is observed, so a pass shorter than one
	// poll could in principle be missed.
	//
	// Steps and not Job.Steps is the right denominator for it/s: Job.Steps is
	// one pass, and a pass restarts for each batch item, each high- and
	// low-noise stage and each highres pass.
	Passes int   `json:"passes,omitempty"`
	Steps  int64 `json:"steps,omitempty"`
}

// Sampling is how long this job took.
//
// Zero when either end is missing rather than a duration measured against the
// zero time, which would be two thousand years and would poison every average
// it reached.
func (j JobRun) Sampling() time.Duration {
	if j.Started.IsZero() || j.Ended.IsZero() || j.Ended.Before(j.Started) {
		return 0
	}
	return j.Ended.Sub(j.Started)
}

// Validate refuses a request this store should not hold.
//
// On the way in, because a row nothing can run is worse than a refusal: it
// sits in the queue, the operator sees it pending, and nothing ever picks it
// up.
func (r Request) Validate() error { return r.validate(true) }

// ValidateRun is Validate for a run that is ALREADY HAPPENING -- what Adopt
// records.
//
// THE JOBS ARE THE ONLY DIFFERENCE, and they are the whole reason there are
// two of these. Validate refuses a batch with none because such a row would
// sit in the queue being skipped forever: it "would rent a card, load the
// weights and generate nothing", which is a statement about work nobody has
// done yet. Two of the three shapes `ember up` has have never had a job at
// all -- a forwarded session has ports and no batch, and a `cmds` run has
// argv ember deliberately does not read -- so applying that refusal to a
// record of what already ran would refuse to record the cheapest thing ember
// does and the easiest to leak. Zero jobs there is a fact, not a request
// nothing can satisfy.
func (r Request) ValidateRun() error { return r.validate(false) }

func (r Request) validate(needJobs bool) error {
	if r.Provider == "" {
		return errors.New("a request names no provider")
	}
	if needJobs && len(r.Jobs) == 0 {
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

// GroupBy is what a spend aggregate is grouped by.
//
// A closed set, and checked, because it reaches a SQL GROUP BY. A free-form
// column name there would be the one place in this package where a caller's
// string becomes part of a statement.
type GroupBy string

const (
	// ByTotal is one row for everything matched.
	ByTotal GroupBy = ""

	ByProvider GroupBy = "provider"
	ByGPU      GroupBy = "gpu"
	ByDay      GroupBy = "day"

	// ByModel groups by the model the generation server reported, which is
	// the grouping that makes the others interpretable.
	//
	// Every measurement this project has made is a statement about a model on
	// a card: FLUX.2 at 2048x2048 took 140.8s a job on an RTX PRO 6000, sd15
	// at 256x256 took 0.43s marginal on an RTX 2080 Ti. Those differ by a
	// factor of about three hundred in sampling time on hardware a factor of
	// four apart, so `--by gpu` alone groups the dimension that cannot be
	// read without this one. It was absent until requests.model existed,
	// because the only stored copy of a model was the cached caps row every
	// rental overwrites.
	ByModel GroupBy = "model"
)

// Valid reports whether g is a grouping this package implements.
func (g GroupBy) Valid() bool {
	switch g {
	case ByTotal, ByProvider, ByGPU, ByDay, ByModel:
		return true
	}
	return false
}

// GroupBys is every grouping, so a command's flag help and this list cannot
// disagree.
func GroupBys() []GroupBy { return []GroupBy{ByProvider, ByGPU, ByDay, ByModel} }

// SpendQuery narrows an aggregate.
//
// A zero Since or Until means unbounded at that end, which is what "what has
// ember ever cost" asks for.
type SpendQuery struct {
	Since time.Time
	Until time.Time
	By    GroupBy
}

// Bucket is one group of an aggregate.
//
// Measured facts only. Every ratio an operator wants -- cost per image, it/s,
// the sampling share of the bill -- is a method below rather than a field,
// because a stored quotient is a number that can be wrong while the terms it
// came from are right.
type Bucket struct {
	// Key names the group: the provider, the card, or the day as
	// YYYY-MM-DD. Empty for ByTotal.
	Key string `json:"key,omitempty"`

	Requests int `json:"requests"`
	Jobs     int `json:"jobs"`
	Images   int `json:"images"`

	Bytes int64 `json:"bytes,omitempty"`

	// RunCost is what the rentals came to and JobCost the card time inside
	// them. They are different questions and the gap between them is the
	// per-rental overhead -- provisioning, the image pull, the asset verify,
	// the model load -- which a batch pays once for everything it serves.
	//
	// RunCost sums requests.cost, which is written only on the request that
	// OPENED each rental, so a rental is counted once however many requests
	// rode along on it. Summing a per-request share instead would understate
	// it; attributing the whole rental to each would multiply it.
	RunCost node.MicroUSD `json:"run_cost_micro_usd"`
	JobCost node.MicroUSD `json:"job_cost_micro_usd"`

	// Steps is the sampling steps across every pass, and Sampling the time
	// the jobs themselves took. Billable is what the rentals were charged
	// for, which bounds Sampling from above.
	Steps    int64         `json:"steps"`
	Sampling time.Duration `json:"sampling_ns"`
	Billable time.Duration `json:"billable_ns"`

	// Unrecorded is how many matched requests FINISHED with jobs that never
	// reported, and so contribute nothing to the figures above.
	//
	// Counted and reported rather than quietly excluded. A total that is
	// missing a month of history and does not say so is the clean-bill-of-
	// health failure `ember ls --all` exits non-zero to avoid, in a column of
	// money.
	//
	// Finished is load-bearing and was missing from the first version: a
	// queued request contributes no cost and no images for the same
	// arithmetic reason, so it was counted here and `ember spend` told the
	// operator a request that had not run was missing figures. Nothing is
	// missing from a request that has not happened yet.
	//
	// Two different things are counted together on purpose -- a request from
	// before schema version 3, and one that failed before a job completed --
	// because the warning is about the arithmetic and both divide a rental
	// cost into no images. See unrecorded, which both stores go through.
	Unrecorded int `json:"unrecorded,omitempty"`
}

// PerImage is what one image cost of rental.
//
// Of rental and not of card time, because the rental is the invoice. Zero
// images gives zero rather than a division by nothing.
func (b Bucket) PerImage() node.MicroUSD {
	if b.Images <= 0 {
		return 0
	}
	return node.MicroUSD(int64(b.RunCost) / int64(b.Images))
}

// Overhead is what the rentals cost beyond the jobs themselves.
//
// Never negative. JobCost is card time measured inside the rental and RunCost
// is the rental, so RunCost should bound it -- but a rental whose cost event
// never arrived has RunCost 0 with jobs that did run, and reporting that as a
// negative overhead would read as a refund.
func (b Bucket) Overhead() node.MicroUSD {
	if b.RunCost <= b.JobCost {
		return 0
	}
	return b.RunCost - b.JobCost
}

// StepsPerSecond is the sampling rate across this bucket's jobs.
//
// Computed from Steps, which counts every pass, and not from what the jobs
// asked for: a pass restarts for each batch item and each highres pass, so an
// it/s built from the requested step count understates every batched job.
func (b Bucket) StepsPerSecond() float64 {
	if b.Sampling <= 0 || b.Steps <= 0 {
		return 0
	}
	return float64(b.Steps) / b.Sampling.Seconds()
}

// SamplingShare is the fraction of billable rental time spent generating.
//
// The rest is the overhead a batch exists to pay once: provisioning, the
// image pull, the store read, the model load.
func (b Bucket) SamplingShare() float64 {
	if b.Billable <= 0 || b.Sampling <= 0 {
		return 0
	}
	return b.Sampling.Seconds() / b.Billable.Seconds()
}

// Filter narrows a listing.
//
// One Filter for both readers, so `ember history --status` and the API's queue
// listing cannot come to mean different things. The queue API passes Status
// and Limit and nothing else; the ledger passes all of it.
type Filter struct {
	// Status, empty for every status.
	Status Status

	// Since and Until bound when a request was created, half-open: Since
	// includes and Until excludes, so consecutive periods tile without a
	// request landing in both. The same rule and the same spelling as
	// SpendQuery, because an operator who reads `ember spend --since` and then
	// `ember history --since` must not have to wonder which end is open.
	Since time.Time
	Until time.Time

	// Provider and GPU match exactly, empty for any. Exactly and not by
	// prefix: node.NormalizeGPU has already decided what a card is called by
	// the time a request is stored, and a filter that guessed at prefixes
	// here would be a second spelling of the matching rule that
	// node.Filter.Matches already owns.
	Provider string
	GPU      string

	// Limit caps how many come back, newest first. Zero means a default the
	// implementation chooses: an unbounded listing of every request ever made
	// is not an answer to "what is in the queue".
	Limit int
}

// Entry is one row of the ledger: a request, joined to what it produced.
//
// FLAT, AND NOT A Request WITH COUNTS BESIDE IT, which was the first shape and
// was wrong twice over. Request carries Jobs []Job -- the prompts and the
// geometry -- and reading those for a listing is a query per row, which is the
// N+1 this type exists to replace; so the embedded Request would have arrived
// with Jobs nil, and `"jobs": null` beside `"jobs": 6` is a wire shape where
// the honest answer ("not asked for") is spelled the same way as a request
// with no jobs at all. Validate refuses those, so the listing would have been
// shipping an unrepresentable request. `Get` is where a request's jobs come
// from.
//
// The duplication is bounded by what a ledger row prints. A field added to
// Request that nothing in this table shows does not belong here.
type Entry struct {
	ID      ReqID     `json:"id"`
	Created time.Time `json:"created"`
	Status  Status    `json:"status"`

	Provider string `json:"provider"`
	GPU      string `json:"gpu,omitempty"`

	// Model is what the generation server said it had loaded, which is the
	// other half of every figure in this row: a cost per image is a fact
	// about a model on a card and not about the card.
	//
	// Empty for a request that never reached a node and for every request
	// from before schema version 5. `ember history` prints a dash there,
	// because a blank cell in a column of names reads as a value that failed
	// to print rather than as a question nobody can answer.
	Model string `json:"model,omitempty"`

	// RunID and Node name the rental that served it, which is what joins a
	// row here to what `ember ls` reported at the time.
	RunID string `json:"run_id,omitempty"`
	Node  string `json:"node,omitempty"`

	Started time.Time `json:"started,omitempty"`
	Ended   time.Time `json:"ended,omitempty"`

	// Cost is the rental and JobCost the card time inside it, the same two
	// figures Bucket carries and for the same reason: the gap between them is
	// the per-rental overhead a batch pays once.
	Cost     node.MicroUSD `json:"cost_micro_usd,omitempty"`
	Rate     node.Rate     `json:"rate_micro_usd_per_hour,omitempty"`
	Elapsed  time.Duration `json:"elapsed_ns,omitempty"`
	Billable time.Duration `json:"billable_ns,omitempty"`

	Jobs   int   `json:"jobs"`
	Images int   `json:"images"`
	Bytes  int64 `json:"bytes,omitempty"`

	JobCost  node.MicroUSD `json:"job_cost_micro_usd,omitempty"`
	Steps    int64         `json:"steps,omitempty"`
	Sampling time.Duration `json:"sampling_ns,omitempty"`

	// The node's memory, both views, and the weights this run read. See
	// Request's own copy of these for why all of them; `ember history
	// --memory` is what prints them, because the cost table has no room and
	// a verdict column would be a decision nothing has the measurements to
	// make yet.
	MemCgroup  int   `json:"mem_cgroup,omitempty"`
	MemLimit   int64 `json:"mem_limit_bytes,omitempty"`
	MemCurrent int64 `json:"mem_current_bytes,omitempty"`
	MemTotal   int64 `json:"mem_total_bytes,omitempty"`
	MemCached  int64 `json:"mem_cached_bytes,omitempty"`
	AssetBytes int64 `json:"asset_bytes,omitempty"`

	// Rode means this request generated on a rental another request opened,
	// so its zero rental cost is by design and not a figure that went
	// missing.
	//
	// IT IS HERE BECAUSE A COLUMN OF ZEROS IN A MONEY LEDGER READS AS FREE.
	// The queue writes the rental's cost to the request that opened it and to
	// no other -- "which request gets the rental's cost is a real question
	// with no tidy answer", and that is the answer -- so every request that
	// joined a rental already up has Cost 0 with real card time beside it. A
	// reader who does not know that sums the column and gets a bill several
	// times too small, which is the direction a money figure must never be
	// wrong in.
	Rode bool `json:"rode_along,omitempty"`

	// Unrecorded means no job of this request ever reported finishing, so the
	// per-job columns are zero because nothing wrote them.
	//
	// True for every request that ran before schema version 3, and nothing
	// backfills: a backfill would have to re-derive step counts from progress
	// frames whose poll interval is unknown for those runs. Said out loud for
	// the reason Bucket.Unrecorded is, one row at a time.
	Unrecorded bool `json:"unrecorded,omitempty"`

	Err string `json:"error,omitempty"`
}

// Wall is how long the request itself took.
//
// Its own span and not the rental's Elapsed, which only the opener carries: a
// request that rode along on somebody else's rental has a wall time of its own
// and no Elapsed at all. Zero when either end is missing, as JobRun.Sampling
// is, rather than a duration measured against the zero time -- which would be
// two thousand years and would poison every average it reached.
// MemHeadroom is the bound this node's page cache lived under, less the
// weights that had to fit in it, and whether that is knowable at all.
//
// THE RULE IS NOT REIMPLEMENTED HERE, IT IS CALLED, and it used to be both.
// These four lines existed twice -- once over these columns and once over
// nodemem.Report's fields, written in the same commit -- and the copy on the
// live-node side was never called by anything, so the two stayed in step only
// because nobody exercised one of them. That is the exposure decision's rule
// ("a second copy of the rule is a second place for it to drift") met over a
// subtraction, and the drift would have been silent in the expensive
// direction: AGENTS.md named the dead copy as the home of the dash-versus-zero
// decision, so an edit there changed nothing an operator sees.
//
// node.Memory is the one implementation. It lives in the one package both
// sides already import, which is what keeps this a delegation rather than an
// import of internal/nodemem -- that would put sshx, workload and
// x/crypto/ssh in the dependency graph of a SQLite store.
//
// THERE IS NO EXPORTED MemCeiling, AND THE DETECTOR IS WHAT SETTLED THAT.
// There was one, and its only production caller was the sentence of this
// method that the delegation above replaced -- so the moment the subtraction
// moved, `TestNoProductionDeadCode` reported it, which is the ninth instance
// of the defect this guard was written for and the first it caught as it was
// being created. It is right that it is gone: `ember history --memory` prints
// the cgroup's limit and the host's total as their own columns and the
// headroom as the one derived figure, so the ceiling is a step in an
// arithmetic nobody reads out. The tests ask e.memory().Ceiling() instead,
// which is the same code without an exported name that claims a reader.
//
// The name stays MemHeadroom because that is what the ledger calls it, and
// the columns stay flat for the reason Entry itself is flat.
func (e Entry) MemHeadroom() (int64, bool) { return e.memory().Headroom() }

// memory is this row's memory facts as the rule's own type.
//
// By field name, never positionally: node.Memory's fields are two counts of
// bytes whose order IS the rule, and a ceiling read off the host's total
// instead of the cgroup's limit is the answer that makes a slow node look
// fine.
func (e Entry) memory() node.Memory {
	return node.Memory{
		CgroupLimit: e.MemLimit,
		HostTotal:   e.MemTotal,
		Assets:      e.AssetBytes,
	}
}

func (e Entry) Wall() time.Duration {
	if e.Started.IsZero() || e.Ended.IsZero() || e.Ended.Before(e.Started) {
		return 0
	}
	return e.Ended.Sub(e.Started)
}

// Store is the record of what was asked for and what came back.
//
// Narrow on purpose. Everything here is either a write the queue makes or a
// read the API serves; there is no general query surface, because a general
// query surface is what makes an engine hard to swap.
type Store interface {
	// Enqueue records a request as queued and returns its id.
	Enqueue(ctx context.Context, r Request) (ReqID, error)

	// Adopt records a run that is already happening and returns its id. It is
	// how `ember up` enters the history.
	//
	// IT IS NOT Enqueue, AND THE STATUS IS THE REASON. Enqueue writes
	// StatusQueued whatever the caller said, because that is what entering a
	// queue means -- and a queued row is exactly what ClaimNext takes. An
	// `ember up` row written that way would be claimable: an `ember serve`
	// running in another terminal, which is the ordinary setup for somebody
	// who has both, would pick it up, rent a SECOND card and generate the
	// batch a second time from the row describing the run already in flight.
	// One mistaken status, two GPUs. So this writes StatusRunning, which no
	// claim will take, and the row is terminal from the queue's point of view
	// the moment it exists.
	//
	// IT IS WRITTEN BEFORE THE CREATE CALL, on the rule internal/provision
	// already follows for the journal: "the intent record is written before
	// the create call", because the window between asking for a machine and
	// reading the answer is where a rental nothing recorded is born. So a run
	// whose create call failed, and a run whose node never became ready but
	// billed anyway, both have a row -- and both are runs that can cost money.
	//
	// THE JOBS ARE OPTIONAL HERE, unlike Enqueue: see ValidateRun. Two of
	// `ember up`'s three shapes have none.
	//
	// It names no more columns than Enqueue does. Everything a run produces --
	// the node, the cost, the rate, the memory figures, the model -- is
	// written afterwards by whatever watched the run, for the reason JobRun is
	// separate from Job: a value the operator is billed against must not be
	// settable by the thing being billed.
	//
	// Its one unhappy interaction is with Interrupt, whose premise -- anything
	// running belonged to a process that is gone -- an `ember up` in another
	// terminal makes false. A serve started beside a live `up` marks that
	// run interrupted, and the run's own ending writes over it a moment
	// later. Said out loud rather than fixed, because the alternative is a
	// per-row holder and a liveness check, and the direction this gets wrong
	// is transient and self-correcting; the direction it gets RIGHT is an
	// `ember up` killed with SIGKILL, whose row would otherwise read as
	// running forever.
	Adopt(ctx context.Context, r Request) (ReqID, error)

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

	// UpdateJob applies fn to what one job did and writes it back.
	//
	// Read-modify-write rather than a setter per fact, because the facts
	// arrive separately: unit_started carries the start, job_images the files
	// and bytes, job_progress the passes, unit_cost the end and the money.
	// A setter per event would be four statements that each have to remember
	// not to clobber the others.
	//
	// It writes only the execution columns. The job's prompt and geometry are
	// what was asked for and are not writable here, for the reason Update
	// cannot touch Jobs: a job whose ask changed after it ran is a record of
	// something that did not happen.
	UpdateJob(ctx context.Context, id ReqID, idx int, fn func(*JobRun)) error

	// JobRuns returns what each of a request's jobs did, by index.
	//
	// A job nothing has reported on is still returned, zeroed, so a
	// half-finished request does not read as a shorter one. An absent request
	// is no rows and no error, as Images and Events are: this is a "list the
	// children of" read, not a lookup.
	JobRuns(ctx context.Context, id ReqID) ([]JobRun, error)

	// Spend aggregates what was spent, in the store rather than in Go.
	//
	// IN SQL BECAUSE THE ALTERNATIVE IS WHAT THIS REPLACED. The only
	// aggregate ember had was a Go loop over one request's entire event
	// stream, and the facts it summed were inside a JSON blob per event -- so
	// "what did a week cost, by card" meant parsing every row of the table
	// that grows per step per job per rental. The columns exist so this can
	// be a SUM.
	//
	// Derived figures are computed on the way out and never stored: it/s is a
	// division, and a stored quotient is a number that can disagree with the
	// terms it came from.
	Spend(ctx context.Context, q SpendQuery) ([]Bucket, error)

	// History returns the ledger: one row per request, newest first, each
	// joined to what its jobs did.
	//
	// SEPARATE FROM List BECAUSE THE QUESTIONS ARE. List answers "what is in
	// the queue" and hands back the prompts, which means a second query per
	// row for the jobs; this answers "what has ember run and what did it
	// cost" in one statement, over a period, and never reads a prompt. A
	// single method doing both would do the expensive half always.
	//
	// It is a read and nothing else. There is no write anywhere on the path
	// from `ember history` to here, which is what makes "the ledger cannot
	// advance the queue" a property of the shape rather than a promise.
	History(ctx context.Context, f Filter) ([]Entry, error)

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

	// PruneEvents drops the events of finished requests that ended before the
	// horizon, keeping the request, its jobs, its costs and its images.
	// dryRun counts them and removes nothing.
	//
	// THE ONE THING HERE THAT MAY GO BY AGE. The execution columns are a
	// derived index of these events, so what a prune costs is the replay and
	// not a figure -- see internal/genstore/prune.go for why the other three
	// tables have no equivalent, and why the schema's ON DELETE CASCADE
	// clauses are not the mechanism this looks like it should have used.
	PruneEvents(ctx context.Context, before time.Time, dryRun bool) (int, error)

	// DropImages forgets the image rows of one request and reports how many
	// went. The blobs they named are the caller's to remove, first: a row
	// removed ahead of its file leaves bytes nothing in ember can name.
	DropImages(ctx context.Context, id ReqID) (int, error)

	// Usage counts what the history holds, for `ember history du`. An operator
	// cannot decide to prune something they cannot measure.
	Usage(ctx context.Context) (Usage, error)

	Close() error
}
