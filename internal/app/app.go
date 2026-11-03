// Package app is everything ember does once the flags have been read.
//
// WHY IT IS NOT IN internal/cli. The pipeline that turns a config into a
// destroyed node lived in cmd_up.go, as 1300 lines of unexported functions,
// and `ember store sync` ran the same sequence a second time from
// cmd_store.go. A second caller cannot reach any of it: `ember serve` is a
// subcommand, so internal/cli importing a web server that imports internal/cli
// is a cycle. Moving the wiring and the pipeline here is what breaks it.
//
// WHY IT IS CALLED app AND NOT run. Most of this is not about running a
// workload. `ember ls`, `ember reap`, `ember offers`, `ember down` and all
// four `store` subcommands need the registry, the paths and the journal, and a
// package called `run` that `ember ls` imports to list nodes would be a name
// that lies about what it holds.
//
// WHAT STAYS IN internal/cli: flag parsing, usage text, exit codes, and the
// one renderer that is pure text -- the dry run.
package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"time"

	"github.com/Legion112/ember/internal/blob"
	"github.com/Legion112/ember/internal/config"
	"github.com/Legion112/ember/internal/dockerengine"
	"github.com/Legion112/ember/internal/dockerengine/dockerclient"
	"github.com/Legion112/ember/internal/keys"
	"github.com/Legion112/ember/internal/logx"
	"github.com/Legion112/ember/internal/provider"
	"github.com/Legion112/ember/internal/provider/local"
	"github.com/Legion112/ember/internal/provider/runpod"
	"github.com/Legion112/ember/internal/provision"
	"github.com/Legion112/ember/internal/statestore"
)

// ErrUsage marks an error the operator fixes by changing a flag or the config
// file, rather than one ember or a provider caused.
//
// It is a sentinel because exit status 3 is a documented contract with cron
// and CI, and this package cannot construct internal/cli's own unexported
// usage type any more. Without it every refusal moved out of cmd_up.go --
// "no image", "that is not a port", "a serve command chose its own listen
// address" -- would silently become exit 1, and the five tests that assert
// only `err != nil` would not have noticed.
var ErrUsage = errors.New("usage")

// Usage marks err as the operator's to fix.
//
// The message is err's own, unchanged: wrapping with a prefix would rewrite
// every refusal this program has ever printed, and the tests assert on those
// strings. Unwrap returns both, so errors.Is finds ErrUsage and errors.As
// still finds whatever err wrapped.
func Usage(err error) error { return usageError{err} }

type usageError struct{ err error }

func (e usageError) Error() string   { return e.err.Error() }
func (e usageError) Unwrap() []error { return []error{e.err, ErrUsage} }

// Deps are the collaborators ember needs, injected so a command is testable
// without a Docker daemon or a real home directory.
//
// Exported now, which is a widening worth naming: these are injection points
// and not seals, so unlike hostkey.Trust or exposure.PortRequest there is
// nothing here a test could use to reach a state production cannot. The two
// fields that had no reader at all -- ConfigIn and Signals, which nothing in
// the module referenced -- are gone rather than exported.
type Deps struct {
	Engine  dockerengine.API
	HomeDir string

	// Creds overrides what is read from the environment and from disk, so a
	// test can register a cloud provider without a real token and without
	// touching the operator's credentials file.
	Creds *config.Credentials

	// DetectGPU overrides what the local provider finds in this machine.
	//
	// The same hazard Creds exists for. Left to resolve itself it reads the
	// developer's own graphics card, and a test asserting "nothing matches
	// --gpu h100" would pass on CI and fail on a workstation -- which is
	// exactly what happened when local GPU support was added.
	//
	// The second result is local.DetectGPU's: whether the probe answered at
	// all. Carried rather than filled in here, because a seam that turned
	// "I could not ask" into "there is no card" at this boundary would put the
	// collapse local.gpuNow was fixed for one layer up.
	DetectGPU func(ctx context.Context) (local.GPU, bool)

	// CloudBaseURL redirects every cloud provider's API.
	//
	// Without it a test that registers RunPod with a fake token talks to
	// api.runpod.io for real: slow, dependent on somebody's network, and in
	// CI a stream of authentication failures against a live service. The
	// first version of the test below did exactly that, and the comment
	// claiming otherwise was the only thing standing in for this field.
	CloudBaseURL string

	// Now overrides the clock the availability collector stamps samples with.
	//
	// The one injected value that is not a collaborator, and it is here
	// rather than inside the collector because observed_at is the only field
	// no provider supplies -- so what the file records has to be the instant
	// the caller recorded, and a test that could not pin it could not assert
	// which day file a sample lands in. The day boundary is exactly where a
	// sample goes into the wrong file.
	Now func() time.Time

	// S3BaseURL redirects the store upload endpoint.
	//
	// Separate from CloudBaseURL because they are separate services on
	// separate hosts, and for the same reason that one exists: a test that
	// pushed to a store must not be able to reach RunPod. Without it the
	// endpoint is derived from the store's datacentre and is a real
	// hostname.
	S3BaseURL string
}

// Credentials resolves this run's provider tokens.
func (d Deps) Credentials(configDir string) (config.Credentials, error) {
	if d.Creds != nil {
		return *d.Creds, nil
	}
	return config.LoadCredentials(configDir)
}

// Clock is the time this run should call now.
//
// UTC, always. Every timestamp ember keeps is UTC -- the spend period, the
// label's deadline, the catalogue's day files -- and a local-time sample would
// land in a different day file depending on where the operator was sitting.
func (d Deps) Clock() time.Time {
	if d.Now != nil {
		return d.Now().UTC()
	}
	return time.Now().UTC()
}

// Paths are the directories a run reads and writes.
//
// Inputs IS UNDER THE STATE DIRECTORY AND NOT UNDER THE OUTPUTS, which is the
// one thing about it worth reading. An uploaded image is not something a run
// produced: it is an argument to one, it exists before the request that uses
// it, and it is what `ember history prune` must not treat as a paid-for
// result. internal/blob says the rest.
//
// Resolved here rather than joined at each call site, for statestore.DirName's
// reason: the journal's "nodes" subdirectory was a literal in the CLI and a
// second literal in the lab harness, the two drifted, and a lab asserting
// that a clean run leaves no records passed while reading an empty directory.
type Paths struct{ Config, State, Outputs, Inputs string }

// ResolvePaths decides where keys, records, outputs and staged inputs live.
func ResolvePaths(cfg config.Config, d Deps) (Paths, error) {
	if d.HomeDir != "" {
		state := filepath.Join(d.HomeDir, "state")
		return Paths{
			Config:  filepath.Join(d.HomeDir, "config"),
			State:   state,
			Outputs: filepath.Join(d.HomeDir, "outputs"),
			Inputs:  filepath.Join(state, blob.InputsDirName),
		}, nil
	}
	c, err := cfg.ConfigDirOrDefault()
	if err != nil {
		return Paths{}, err
	}
	s, err := cfg.StateDirOrDefault()
	if err != nil {
		return Paths{}, err
	}
	o, err := cfg.OutputDirOrDefault()
	if err != nil {
		return Paths{}, err
	}
	return Paths{
		Config: c, State: s, Outputs: o,
		Inputs: filepath.Join(s, blob.InputsDirName),
	}, nil
}

// ConfigDir resolves the credentials directory, falling back to the config's
// own so that a failure here is reported by the loader rather than by a nil
// path somewhere lower down.
func ConfigDir(cfg config.Config, d Deps) string {
	if ps, err := ResolvePaths(cfg, d); err == nil {
		return ps.Config
	}
	return cfg.ConfigDir
}

// OpenStore opens the journal of nodes ember created.
func OpenStore(dir string) (*statestore.Store, error) {
	return statestore.Open(filepath.Join(dir, statestore.DirName))
}

// BuildRegistry registers every provider this run can reach.
//
// A provider with no credential is left out rather than registered and left
// to fail: `ember ls` reports an unreachable provider as a finding and exits
// 2, so registering RunPod on a machine that has never had a token would make
// every clean run look like a problem. The absence is reported by `ember
// offers` and by a named-provider lookup instead, where it is actionable.
func BuildRegistry(d Deps, creds config.Credentials, cfg config.Config) (*provider.Registry, error) {
	engine := d.Engine
	if engine == nil {
		c, err := dockerclient.NewClient()
		if err != nil {
			return nil, err
		}
		engine = c
	}
	reg := provider.NewRegistry()
	lp := local.New(engine)
	// Injected the same way credentials are, and for the same reason: left
	// to resolve itself, this reads the developer's own graphics card, and
	// the CLI tests would behave differently on a machine that has one.
	if d.DetectGPU != nil {
		lp.Detect = d.DetectGPU
	}
	if err := reg.Add(lp); err != nil {
		return nil, err
	}

	if !creds.RunPod.IsZero() {
		rp := runpod.New(creds.RunPod)
		if d.CloudBaseURL != "" {
			rp.HTTP.BaseURL = d.CloudBaseURL
		}
		rp.Cloud = cfg.RunPodCloud
		rp.DataCenters = cfg.DataCenters
		rp.DiskGiB = cfg.DiskGiB
		if err := reg.Add(rp); err != nil {
			return nil, err
		}
	}
	return reg, nil
}

// BuildOptions assembles the collaborators one provisioning needs.
//
// EVERY RETURNED VALUE IS PER PROCESS, NOT PER RUN, and a caller that runs
// more than one node has to call this once. The sharpest instance of that was
// the host key pin store, which loaded its file at open and rewrote the whole
// thing on every change -- two of them being two snapshots and
// last-writer-wins, with provision.destroy's Pins.Forget able to drop a pin
// another run had just written. It is deleted (#120, and the note in
// internal/hostkey's trust.go), so what is left is statestore.Store and
// keys.Identity: the journal is keyed per run and holds no in-memory
// snapshot, so the warning is about the shape rather than about a live hazard
// -- which is exactly why it stays written down. It mattered nowhere for
// `ember up`, which provisions once per process, and a great deal for a
// server that provisions on request.
//
// lo is the level and the format, decided by whoever read the flags. Its zero
// value is info-level text, which is what the literal that used to sit here
// hard-coded -- so a caller that has no opinion gets exactly the previous
// behaviour rather than a new default nobody chose.
func BuildOptions(ps Paths, cfg config.Config, out io.Writer, lo logx.Options) (provision.Options, error) {
	store, err := OpenStore(ps.State)
	if err != nil {
		return provision.Options{}, err
	}
	id, err := keys.EnsureIdentity(ps.Config)
	if err != nil {
		return provision.Options{}, err
	}
	hourly, run, err := cfg.Budgets()
	if err != nil {
		return provision.Options{}, Usage(err)
	}
	return provision.Options{
		Store:    store,
		Identity: id,
		Budget:   provision.Budget{MaxRate: hourly, MaxRun: run},
		Log:      logx.New(out, lo),
	}, nil
}

// outf writes to the operator's stream.
//
// The error is discarded deliberately, for the reason internal/cli's own outf
// gives: a failed write to stdout means the pipe is gone -- `ember up | head`
// is the ordinary way that happens -- and the exit status is about to be the
// only thing anyone sees. Returning it would turn a closed pager into "there
// are findings".
func outf(w io.Writer, format string, a ...any) { _, _ = fmt.Fprintf(w, format, a...) }
