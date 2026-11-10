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
	"github.com/Legion112/ember/internal/tts"
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

	// TTS is the utterance, when this request's Kind is KindTTS, and nil for
	// every image generation.
	//
	// A POINTER TO tts.Ask AND NEVER TO tts.Job, which is the seal and not a
	// convenience. `Ask` has no field for a voice path, for espeak's data
	// directory, for a binary or for argv -- it carries a voice **id** that
	// names an entry the operator published -- so a request body cannot
	// express one and neither can a row read back out of this store. The
	// resolution happens in the planner, from the config, which is the only
	// place a path enters. Same asymmetry AGENTS.md records for an init
	// image: the bytes are on the thing serialised nowhere and the reference
	// is on the thing stored as JSON.
	//
	// ITS OWN COLUMN AND NOT A SECOND KEY IN `params`. That column is
	// `sdserver.Params` -- a typed struct with twenty-three fields of one
	// driver's -- so folding a second driver's payload into it would make
	// `params` mean "whichever kind's parameters these are", and the first
	// reader to unmarshal it into the wrong one gets zeros rather than an
	// error. One JSON column per kind is still "one JSON column, not
	// twenty-three": what that rule refuses is a column per FIELD.
	TTS *tts.Ask `json:"tts,omitempty"`

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

	// Kind is which workload this request is for: empty or KindSD for stable
	// diffusion, KindTTS for speech.
	//
	// ON THE REQUEST AND NOT ON A JOB, which makes a mixed batch
	// UNREPRESENTABLE rather than refused. One rental runs one driver -- a
	// resident sd-server, or a piper per utterance -- so an sd job and a
	// speech job in one request would need two of them on one node, and a
	// per-job discriminator would make that a thing a validator has to catch
	// instead of a thing nobody can say. It is also what Compat reads, so a
	// speech request cannot ride a rental whose sd-server it has nothing to
	// say to.
	//
	// EMPTY IS SD, and that is compatibility rather than a default worth
	// defending: every row written before this column existed is an image
	// generation, and a migration that guessed otherwise would mislabel an
	// operator's whole history. KindOf is the one place that reading lives.
	Kind string `json:"kind,omitempty"`

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
// done yet. An `ember up` whose configuration describes no jobs has never had
// one, so applying that refusal to a record of what already ran would refuse
// to record the cheapest thing ember does and the easiest to leak. Zero jobs
// there is a fact, not a request nothing can satisfy.
func (r Request) ValidateRun() error { return r.validate(false) }

func (r Request) validate(needJobs bool) error {
	if r.Provider == "" {
		return errors.New("a request names no provider")
	}
	if !KindValid(r.Kind) {
		return fmt.Errorf("kind %q is not one ember runs; it is %q or %q",
			r.Kind, KindSD, KindTTS)
	}
	// ONE KIND, AND THE JOBS HAVE TO AGREE WITH IT. A speech request whose
	// jobs carry no utterance would rent a card and say nothing; an image
	// request carrying one would drop it silently, which is the "built end to
	// end and never connected" shape in a single row. Checked here rather
	// than at the wire alone, because Adopt and a row read back out of the
	// store reach this and not that.
	for i, j := range r.Jobs {
		switch KindOf(r.Kind) {
		case KindTTS:
			if j.TTS == nil {
				return fmt.Errorf("jobs[%d]: this is a %s request and the job carries no utterance", i, KindTTS)
			}
			if err := j.TTS.Validate(); err != nil {
				return fmt.Errorf("jobs[%d]: %w", i, err)
			}
		default:
			if j.TTS != nil {
				return fmt.Errorf("jobs[%d]: this is a %s request and the job carries an utterance", i, KindSD)
			}
		}
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

// Shape is the rental a request names: a kind, a provider and a card.
//
// IT IS EXACTLY WHAT A REQUEST MAY CHOOSE, which is why it is a type. A
// request names a provider, a card, a kind and a batch of jobs and nothing
// else -- no image, no store, no mount, no argv -- so these three fields are
// the whole of what distinguishes two rentals one process can be asked for.
// Everything else about a rental comes from the config this process loaded
// once, which no request can touch.
//
// THE GPU HERE IS THE ASK AND NOT THE CARD, and that distinction is the whole
// of Compat.Card: an empty GPU is "whatever the provider has", and two
// requests that both asked for nothing can land on different silicon.
// Anything keyed on a Shape is therefore keyed on what the request said, which
// is all a caller has before a node exists -- see the caps cache, whose
// lookup happens at enqueue time.
type Shape struct {
	// Kind is read through KindOf, so '' and "sd" are one shape.
	Kind string

	Provider string

	// GPU is the request's own selection, empty for "any card this provider
	// has". It is NOT normalised here: node.NormalizeGPU is applied where a
	// request is accepted, so by the time a Shape is built the spelling is
	// already the one spelling.
	GPU string
}

// Shape is which rental this request asked for.
func (r Request) Shape() Shape {
	return NewShape(r.Kind, r.Provider, r.GPU)
}

// Shape is the rental this compat describes, without its exclusions.
func (c Compat) Shape() Shape {
	return NewShape(c.Kind, c.Provider, c.GPU)
}

// NewShape normalises a kind and a card name into the one spelling anything
// keyed on a shape is keyed on.
//
// BOTH FOLDS IN ONE PLACE, because a key built two ways is two keys. The kind
// goes through KindOf, so a row written before that column and a compat
// holding "sd" are one shape. The card goes through node.NormalizeGPU, which
// is idempotent and is already what config.Filter runs an operator's typed
// string through -- without it "RTX 4090" from a browser and "rtx4090" from a
// config would cache two capability documents for one card, and the enqueue
// that read the wrong one would refuse a sampler the card has.
func NewShape(kind, provider, gpu string) Shape {
	return Shape{
		Kind:     KindOf(kind),
		Provider: provider,
		GPU:      string(node.NormalizeGPU(gpu)),
	}
}

// Compat is what a live rental can serve.
//
// THE RULE: a queued request joins a rental that is already up when the kind
// and the provider agree and the rental's card satisfies what the request
// asked for. Everything else about the rental was settled when its offer was
// chosen, and the image, the store and the mount are resolved from the config
// this process loaded -- which a request may not touch -- so they are
// functions of the three fields here and need no field of their own. See
// Card for the one of those that is a FACT rather than an ask, and
// servesClause for the relation itself.
//
// WHAT IS DELIBERATELY NOT HERE, because it is not a dimension of the match:
// the worker's queue depth. It is not a column of `requests`, so it cannot be
// compared against anything in the one statement ClaimNext is; as a per-rental
// constant it would be a gate and not a match, and a gate belongs in the
// caller. It is also already known: ember pushes one request's batch and
// pushBatch does not return until every job of it has ended, so the depth is
// `len(req.Jobs)` of the request this rental is serving and ember wrote that
// number itself. Asking a rented machine for a number ember derives from its
// own bookkeeping would be a new thing to believe for no decision it could
// change -- and using it to exclude on the create side is exactly the
// scale-out strict packing refuses: ~27s of per-rental overhead and 80-96s
// per image of model reload are what riding a busy rental buys.
//
// What a request cannot do is ask for a longer life than the node it joins has
// left. That is not checked here: the node's own deadline ends the rental, and
// a batch that needs longer needs a larger ttl. Checking it would mean
// predicting how long a generation takes, which nothing here can do.
//
// The zero Compat means no rental is up, and ClaimNext will then take the
// oldest queued request whatever it asked for.
type Compat struct {
	// Kind is read through KindOf, so a row written before the column
	// existed and a row that says "sd" are the same rental to this.
	//
	// WITHOUT IT A SPEECH REQUEST COULD RIDE AN sd RENTAL. The provider and
	// the GPU would match, the claim would succeed, and the drain loop would
	// hand an utterance to a resident sd-server that has no route for it --
	// after a card had been paid for. The voice does NOT appear here, and
	// that is the measurement rather than an omission: a one-shot piper names
	// its own model per invocation, so two requests wanting different voices
	// are compatible and a resident driver is the only shape that would
	// change it.
	Kind     string
	Provider string
	GPU      string

	// Card is the card this rental actually got, empty until its offer has
	// been chosen.
	//
	// THE ONE ADVERTISED FACT, and it is ember's own rather than the node's.
	// The three fields above are what a request ASKED for, and matching ask
	// against ask under-packs in the direction that costs money: a request
	// that named no card was refused a ride on a rental that asked for an
	// rtx4090 and got one, and a request that named rtx4090 was refused a
	// ride on a rental that asked for nothing and landed on exactly that --
	// so ember rented a second node for work the first one could serve. The
	// fact closes both: an ask of "" is satisfied by any card, and a named
	// ask is satisfied by a card node.GPUModel.Accepts admits.
	//
	// IT IS NOT ADVERTISED BY THE WORKER, which is the shape this field
	// deliberately is not. ember chose the offer, so ember holds the card;
	// asking the rented machine to name its own silicon would make a packing
	// decision -- and therefore a decision about money -- rest on a rented
	// machine's self-report, which is the trust edge "a tar stream from a
	// rented machine is untrusted input" generalises. The same argument
	// disposes of the image and the mounted store: both are app.ResolveImage
	// and app.Filter over this process's own config and this card, so the
	// node's copy of them is derived from ember's and worth nothing extra.
	// What the node genuinely knows and ember does not is the generation
	// server's capability document, and that already travels verbatim on
	// control.KindPrepared.
	//
	// EMPTY IS "NOT ADVERTISED YET", AND IT WIDENS NOTHING. The supervisor
	// registers a rental before its goroutine exists -- that is what stops
	// runaway scale-out -- and the offer is not chosen until Plan returns, so
	// for the width of one offer lookup a live rental has no card and the two
	// card admissions that do not need one are all that apply. Both sides of
	// fit-or-create read the same relation, so that window narrows the ride
	// side and the exclusion side together and cannot starve a request.
	//
	// WHAT THAT WINDOW COSTS, AND THE WIDER EXCLUSION THAT WAS REFUSED. A
	// request naming a card that a still-provisioning rental turns out to
	// have can be given a node of its own, because nothing yet knows the two
	// match. Excluding on "might" instead -- kind and provider alone until
	// the card arrives -- would save that rental and was measured against
	// what it costs: Plan is an offer lookup of seconds in the ordinary case,
	// and up to 14m47s when the store is empty and a second machine fetches
	// the weights. The wide exclusion would stall EVERY request of that kind
	// and provider, including requests for cards the rental provably cannot
	// serve, for the whole of that -- a minutes-long stall on every request
	// to save one rental in the narrow case where two asks differ and the
	// silicon agrees. Queue.advertise kicks the supervisor the moment the
	// card is known, so the exposed window is the lookup and not a poll.
	Card node.GPUModel

	// Live is every rental that is already up, for a claim whose job is to
	// find work that NONE of them can take.
	//
	// THE SELECTION EXPRESSED AS ITS COMPLEMENT, which is how fit-or-create
	// is built without a second window. #204 describes the N>1 scheduler as
	// `Compat` becoming "a selection -- which of these N can take it" -- and
	// a central selection would mean ember deciding which node gets a request
	// and then writing that decision, which reintroduces exactly the
	// read-then-write gap ClaimNext's ONE STATEMENT argument exists to close.
	// The rentals pull instead: each one's own drain loop claims what it can
	// serve with the fields above. So the only question left for the
	// supervisor is the create side -- "the oldest queued request no live
	// rental can serve" -- and that is this field, as one `AND NOT (...)` per
	// entry in the same statement.
	//
	// AND IT IS WHY THE CLAUSE IS A BUILDER. Both sides of fit-or-create are
	// built from the same one, so the relation a rental rides by and the
	// relation the supervisor excludes by cannot drift: a request excluded
	// here that no rental will then claim is a request that waits out a whole
	// rental for nothing.
	//
	// IT IS WHAT MAKES PACKING STRICT. A request a busy rental could serve is
	// excluded here, so it waits for that rental rather than getting a node of
	// its own: ~27s of per-rental overhead and up to 80-96s per image of model
	// reload are what that buys, and the cost is that a homogeneous queue sees
	// no parallelism at all. See config.MaxNodes.
	//
	// NEVER SET WITH THE THREE FIELDS ABOVE. A ride claim does not care what
	// other nodes are doing, and a rider claim carrying Live would exclude
	// every request it could ride -- including itself. A nested Live on an
	// entry here is ignored for the same reason: an entry describes one live
	// rental and nothing about its neighbours.
	Live []Compat
}

// IsZero reports whether no rental is up to ride.
//
// IT IS ABOUT THE RIDE FIELDS AND NOT ABOUT Live, which is what lets a
// create-side claim carry exclusions and still take the oldest queued request
// whatever it asked for: the take-anything branch is chosen by this and the
// exclusions are applied either way.
func (c Compat) IsZero() bool { return c.Provider == "" }

// LeaseTerm is how long a claim holds a request before something may take it
// back. See Store.Renew for why it is a heartbeat term and not a work bound.
//
// TWO MINUTES AGAINST A FIFTEEN-SECOND SUPERVISOR POLL, which is the ratio
// that matters rather than the number: Queue.Run renews every row it holds at
// the top of each iteration, and the loop's longest park is idlePoll. Eight
// missed renewals is a process that is not running. Shorter would reclaim a
// live rental's row over one slow statement, and the cost of longer is only
// how stale a dead process's row looks -- which nothing spends money on,
// because what destroys a leaked node is internal/reconcile reading the
// provider's label and not this column.
const LeaseTerm = 2 * time.Minute

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
	// health failure `ember ls` exits non-zero to avoid, in a column of
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
	// IT TAKES A LEASE OF ITS OWN, and that is what its unhappy interaction
	// with the startup sweep became. The sweep marked anything running as
	// interrupted on the premise that it belonged to a process that is gone,
	// which an `ember up` in another terminal makes false; Reclaim keys on
	// the lease instead, so a row needs one or it is reclaimable the moment
	// anything looks. This writes `now + TTL`: the run's node is destroyed by
	// its own deadline, so nothing it does can outlive that, and `ember up`
	// is one process with no renewal loop to keep a heartbeat going.
	//
	// IT IS SHORT BY THE WIDTH OF THE CREATE CALL, said rather than fudged.
	// The row is written BEFORE the create, so the node's deadline is
	// `created + TTL` and this is `adopted + TTL` -- a few seconds earlier.
	// A run that uses its whole TTL therefore has its row reclaimed just
	// before it ends, and the run's own ending writes over it a moment later.
	// That is the same transient the sweep had and the same self-correcting
	// direction; what it gets RIGHT is an `ember up` killed with SIGKILL,
	// whose row would otherwise read as running for ever.
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
	//
	// IT TAKES A LEASE AND NOT JUST A LABEL. runID is written to
	// requests.run_id and the row is stamped with an expiry LeaseTerm from
	// now; Renew extends it and Reclaim takes back what expired. Without the
	// expiry a claimed row read `running` for ever if the process that
	// claimed it died, and the only thing that cleared one was a blanket
	// sweep at the next startup -- see Reclaim.
	ClaimNext(ctx context.Context, runID string, c Compat) (Request, bool, error)

	// Renew extends the lease on the requests this process is still
	// responsible for, and returns how many rows it touched.
	//
	// WHY A HEARTBEAT AND NOT A DEADLINE COMPUTED AT CLAIM TIME. The obvious
	// expiry is "the claim plus the request's TTL", because a node cannot
	// outlive its own deadline -- and it is wrong by however long planning
	// takes. The node's deadline is derived from the moment it was CREATED,
	// and between the claim and the create sits an offer lookup and, when the
	// store is empty, a second rental that fetches fifty-six gigabytes of
	// weights: measured at 14m47s. A lease that expired during that would be
	// reclaimed out from under a request whose card was still being got
	// ready.
	//
	// ids ARE THE ROWS THE CALLER IS RESPONSIBLE FOR and not a run id,
	// because that set is the one the caller actually tracks: Queue.held is
	// "anywhere between the claim and the end of its batch", which is exactly
	// the set of rows reading `running` because this process is alive. A
	// renewal keyed on run_id would renew a rental's rows, and a submitted
	// run's row has no claim lease at all.
	//
	// It touches only rows that are still running, so a renewal racing a
	// request's own ending cannot resurrect a finished row's lease.
	Renew(ctx context.Context, ids []ReqID) (int, error)

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
	// moment it was typed.
	//
	// ONE ROW PER SHAPE, AND IT USED TO BE ONE ROW PER PROCESS. That single
	// row was defended with "`sd.serve` is fixed by the config, so every
	// rental in this process reports the same capabilities", and the premise
	// does not survive the request seal it rests on: a request names a
	// provider AND a card, so two live rentals of one process can differ in
	// both -- and a generation server's reported capabilities legitimately
	// differ with the card, which is why `images:` exists at all. With one
	// row the last rental to come up overwrote the cache, and the next
	// enqueue was validated against the WRONG node's capability list: a
	// sampler that card A's build has and card B's does not was accepted for
	// B and refused for A, whichever had answered most recently.
	//
	// THE KEY IS THE ASK AND NOT THE CARD, which bounds what this can do. A
	// lookup happens at enqueue, where no node exists and the card is
	// therefore unknowable; all the request has is its own Shape. So a
	// request that named no card is checked against whatever card the last
	// "any card" rental landed on -- which is the same uncertainty as before
	// for that one shape, and exact for every shape that names its card.
	// Keying on the card would make the cache unreadable by the only caller
	// that needs it.
	PutCaps(ctx context.Context, s Shape, c sdserver.Capabilities) error
	Caps(ctx context.Context, s Shape) (sdserver.Capabilities, bool, error)

	// Reclaim takes back the leases that expired and marks those requests
	// interrupted, returning how many.
	//
	// IT REPLACED A BLANKET SWEEP WHOSE PREMISE WAS FALSE. That sweep marked
	// EVERY running row interrupted at startup, and the argument for it was
	// "a request that was in flight when the process died cannot be resumed,
	// because its rental is gone". The first half still holds and is why a
	// reclaimed row is not re-queued: renting a second card for work the
	// operator may no longer want is the kind of help nobody asked for, and
	// whatever images it produced stay listed because they were paid for.
	//
	// The second half -- "its rental is gone" -- is not true. A node carries
	// the run id and the deadline in the provider's own label, the label is
	// the source of truth, and internal/reconcile reads it: a node can be
	// alive and billing when ember restarts, and an ember that has just
	// written `interrupted` over that node's row has orphaned the one record
	// that said what the machine was for. Two ember processes against one
	// store made it worse in the direction that costs money, because the
	// second one's startup swept the first one's live rentals.
	//
	// So the sweep is keyed on the lease instead: a row whose lease has run
	// out belonged to a process that is not renewing it, and a row being
	// renewed is left alone. Called at startup AND on the supervisor's own
	// loop, because "the process that held this is gone" becomes true while
	// ember is running and not only before it started.
	//
	// A ZERO LEASE IS AN EXPIRED ONE, never "no expiry". Every row written
	// before the column existed holds zero, so an upgraded file behaves
	// exactly as the blanket sweep did for its old rows -- and the dangerous
	// default, a row nothing will ever reclaim, is unrepresentable.
	Reclaim(ctx context.Context) (int, error)

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
