// Package config resolves where ember keeps things and what the defaults are.
//
// The file is committed-safe: it holds image names, GPU filters, ports and
// budgets, and nothing else. Credentials come from the environment or from a
// 0600 file beside it, never from here and never from a command line, because
// /proc/<pid>/cmdline is world-readable.
package config

import (
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/Legion112/ember/internal/node"
	"github.com/Legion112/ember/internal/sdserver"
	"github.com/Legion112/ember/internal/tts"
)

// FileName is the config ember looks for in the working directory.
const FileName = "ember.yaml"

// Config is the whole of ember's settings.
type Config struct {
	// Provider is the default provider name.
	Provider string `yaml:"provider"`

	// Image is the workload's OCI reference. It must be pinned.
	Image string `yaml:"image"`

	// Images maps a CUDA compute capability to the image built for it, keyed
	// the way every other place writes an architecture down: sm_75, sm_90.
	//
	// A map rather than a list of {arch, ref} pairs -- which is the shape
	// Assets uses -- because here the architecture *is* the lookup key, and a
	// map cannot represent two entries both claiming sm_80 while a list can.
	//
	// The values are pinned references, and repo:tag@sha256:... is the
	// intended form: the digest is what actually runs and the tag is there to
	// be read, which is why a tag naming a different architecture than its
	// key is refused rather than ignored. One image is a wrong-architecture
	// image for every card but one, and running it aborts -- "no kernel image
	// is available for execution on the device" -- after the node has been
	// rented and the weights fetched.
	//
	// image: and images: are both honoured and they are not alternatives.
	// ParseImages has the precedence, and the rule that matters: a run whose
	// card has no entry here refuses, and never falls back to image:.
	Images map[string]string `yaml:"images"`

	// TTL bounds a node's life.
	TTL time.Duration `yaml:"ttl"`

	// Ports are "name:remote" or "name:remote:local" entries.
	Ports []string `yaml:"ports"`

	// GPU and MinVRAMGiB narrow the offers considered.
	GPU        string `yaml:"gpu"`
	MinVRAMGiB int    `yaml:"min_vram_gib"`

	// AllowInterruptible bids on hardware the provider can take back
	// mid-run. False, the default, is the safe answer and now the effective
	// one: it reaches node.Filter.AllowInterruptible, which is read against
	// an offer's node.Reclaim.
	//
	// It was spelled AllowSpot here while the key is allow_interruptible and
	// the filter field is AllowInterruptible -- a third name for one switch,
	// in a feature whose defect was two fields sharing one name.
	AllowInterruptible bool   `yaml:"allow_interruptible"`
	Region             string `yaml:"region"`

	// MaxHourlyUSD and MaxRunUSD are decimal strings, parsed into integer
	// microdollars. They are strings rather than floats on purpose: a YAML
	// float for 2.01 is 2.0099999999999998, which times a million truncates
	// to 2009999, and a budget that is sometimes
	// wrong by a rounding error is a budget nobody trusts.
	MaxHourlyUSD string `yaml:"max_hourly_usd"`
	MaxRunUSD    string `yaml:"max_run_usd"`

	// MaxDayUSD and MaxMonthUSD bound what every rental in a UTC calendar
	// day or month may come to together. Decimal strings for the reason the
	// two above are.
	//
	// THEY BOUND A SEQUENCE OF RENTALS, which nothing did. max_hourly_usd
	// bounds a card and max_run_usd bounds one rental, and `ember serve` is
	// the first thing here that can produce a sequence without the operator
	// being at the keyboard for each: thirty queued requests at $1.79/hour
	// on a one-hour TTL are inside every other ceiling and are $54. They are
	// read by `ember serve` only, where a queue drains unattended; `ember up`
	// is one rental the operator started themselves.
	//
	// A request cannot narrow either, let alone widen one -- there is no
	// field for them on the wire type, so a browser is never consulted about
	// the limit that stops it. See internal/queue's ceiling.go for the
	// calendar-rather-than-rolling decision and what it costs.
	MaxDayUSD   string `yaml:"max_day_usd"`
	MaxMonthUSD string `yaml:"max_month_usd"`

	// Dirs, resolved from the environment when empty.
	ConfigDir string `yaml:"config_dir"`
	StateDir  string `yaml:"state_dir"`

	// RunPodCloud is SECURE or COMMUNITY. Empty means RunPod's own default,
	// SECURE. COMMUNITY is cheaper and runs on hardware somebody else owns
	// and can see, which is a choice to make deliberately.
	RunPodCloud string `yaml:"runpod_cloud"`

	// DataCenters restricts where a node may be placed. Empty lets the
	// provider choose.
	DataCenters []string `yaml:"data_centers"`

	// DiskGiB is the node's container disk. Model weights are the reason
	// this is tunable: one video checkpoint is tens of gigabytes, and a pod
	// that runs out of disk mid-pull fails after it has started billing.
	DiskGiB int `yaml:"disk_gib"`

	// OutputDir is where fetched outputs are written, one directory per run.
	// Empty puts them under the state directory, so a run's results sit
	// beside the record of the run that made them.
	OutputDir string `yaml:"output_dir"`

	// Outputs are the paths to retrieve from the node before it dies.
	Outputs []string `yaml:"outputs"`

	// Cmd is the workload: what the node was rented to run. Empty holds the
	// node open with its ports forwarded, which is the interactive shape.
	// Set, ember runs it, brings the outputs home and destroys the node.
	Cmd []string `yaml:"cmd"`

	// Cmds are several workloads run in sequence on one rental.
	//
	// Every per-rental cost is paid once per `ember up`, not once per
	// result: waiting for the machine, pulling the image, reading and
	// verifying the weights, loading them. On a rented A100 that is about
	// three minutes against roughly 100 seconds of generation, so ten images
	// rented ten times spend more than half the bill on setup.
	//
	// Still opaque argv, deliberately. ember does not know what sd-cli's
	// flags mean and should not learn them -- that knowledge belongs in this
	// file, beside the image it describes, for the same reason gpu_probe is
	// a command rather than a built-in check.
	//
	// Outputs are fetched after each entry rather than once at the end, so a
	// batch that dies on its deadline has already delivered what it finished.
	//
	// Use cmd or cmds, never both.
	Cmds [][]string `yaml:"cmds"`

	// SD is the resident-server shape: one sd-server started on the node,
	// holding the weights, driven over the SSH connection for a list of jobs.
	//
	// The difference from Cmds is one cost. Each entry in Cmds is a fresh
	// sd-cli that reads the weights into VRAM again; a server loads them
	// once. Measured on a rented RTX PRO 6000 running FLUX.2 at 2048x2048:
	// 4.5s per image when the node caches the weights, 86s when it re-reads
	// them off the store, and nothing tells ember which node it has.
	//
	// It is also the one place ember knows what a sampler is. Every other
	// workload here is opaque argv on purpose, and that position does not
	// survive progress streaming: a program that will not understand its own
	// payload cannot report that job two is fourteen steps into its first
	// pass. See internal/sdserver.
	//
	// Use cmd, cmds or sd.serve -- never two of them.
	SD SDConfig `yaml:"sd"`

	// TTS is the `tts:` block: text to speech through piper.
	//
	// THE SECOND TYPED WORKLOAD, and the reason it is typed is not the reason
	// `sd:` is. `sd:` exists for the resident model and the progress stream;
	// speech has no stream to read -- a piper utterance emits nothing at all
	// until it is finished -- and its resident win needs a held-open stdin
	// ember's remote-command seam does not have. What it is typed FOR is the
	// other half of the same argument: a browser cannot be handed argv.
	// `extra` is structurally refused over HTTP, so an untyped speech path is
	// not inconvenient from a web UI but unreachable -- and an operator who
	// has to write `cmds: [["sh","-c","printf %s ... | piper ..."]]` is
	// assembling a shell pipeline in YAML, which is the curl-in-a-config this
	// repository already refuses once.
	//
	// internal/tts has the measurements: what piper emits, what one-shot
	// costs, and why there is no server.
	//
	// Use cmd, cmds, sd.jobs or tts.jobs -- never two of them.
	TTS TTSConfig `yaml:"tts"`

	// Store names the persistent store this configuration uses, if any.
	Store StoreConfig `yaml:"store"`

	// History bounds how long the generation history keeps its event rows.
	History HistoryConfig `yaml:"history"`

	// Assets are the files that must be on the store, verified, before the
	// workload runs.
	Assets []AssetConfig `yaml:"assets"`

	// Verify is what to do about an asset that is already there: "always"
	// re-reads and re-hashes it, "present" accepts it. Empty is "always".
	Verify string `yaml:"verify"`

	// GPUProbe is a command that must exit zero on the node before ember
	// fetches an asset or starts the workload.
	//
	// It exists because a GPU that cannot initialize does not fail -- it
	// silently becomes a slow CPU at the GPU's price. One run lost $0.37
	// that way: CUDA reported "system not yet initialized", sd-cli fell back
	// to the CPU backend, and ember happily streamed a 32B model running on
	// 128 threads. Nothing was broken enough to notice.
	//
	// A command and not a built-in check, because what proves a card usable
	// belongs to the image. For this repository's CUDA image:
	//
	//	gpu_probe: ["sh", "-c", "sd-cli --list-devices | grep -qi cuda"]
	//
	// That probe catches the failure above and *not* a wrong-architecture
	// image. Measured: with an sm_75 image on an sm_89 card, --list-devices
	// exits zero and names the card, because listing devices asks the driver
	// what hardware exists and launches no kernel. The probe passed, the
	// workload started, and it aborted a second into the first generation
	// with "no kernel image is available for execution on the device" --
	// after the pull and a 1.6 GB weight fetch.
	//
	// The published images carry /etc/ember-cuda-arch for that, so a probe
	// can compare the image to the card:
	//
	//	gpu_probe: ["sh", "-c", "arch=$(nvidia-smi --query-gpu=compute_cap \
	//	  --format=csv,noheader | head -1 | tr -d ' .'); \
	//	  img=$(cat /etc/ember-cuda-arch); test \"$arch\" = \"$img\" && \
	//	  sd-cli --list-devices | grep -qi cuda"]
	//
	// Belt and braces: `images:` already refuses a card it has no entry for,
	// before renting. This catches what a map cannot -- the right digest
	// filed under the wrong key, or a scalar image: pinned by hand.
	//
	// Empty runs no probe.
	GPUProbe []string `yaml:"gpu_probe"`

	// GPUProbeTimeout is how long the card has to become usable.
	//
	// Retried rather than checked once, because the driver's own words were
	// "not yet": an SXM board sits on an NVSwitch fabric that the host brings
	// up, and CUDA refuses until it has. Probing once at the earliest
	// possible moment turns a card that needed forty seconds into a refusal.
	// Empty is gpucheck.DefaultTimeout.
	GPUProbeTimeout time.Duration `yaml:"gpu_probe_timeout"`
}

// AssetConfig is one file to put on the store.
//
// The digest is not optional and has no default. A store is writable by every
// node that mounts it, so an unverified asset is a file any previous run
// could have replaced -- and the first thing that would notice is the model
// producing nonsense on a GPU somebody is paying for.
type AssetConfig struct {
	// URL is where to fetch it from if it is not already on the store.
	URL string `yaml:"url"`

	// SHA256 is the digest the file must have, in hex.
	SHA256 string `yaml:"sha256"`

	// As is the path on the store, relative to the mount.
	As string `yaml:"as"`
}

// StoreConfig names a persistent store and where it is mounted.
//
// Called StoreConfig and not Store because this package already refers to
// node.Store, and two types a line apart with the same name and different
// meanings is how the wrong one gets passed.
//
// Naming a store here does not create it. A store is a standing monthly
// charge, so bringing one into existence takes `ember store create` and an
// operator who typed it; a config that silently created one would be a config
// that bills for a typo.
type StoreConfig struct {
	// Name is ember's name for the store, without the label prefix.
	Name string `yaml:"name"`

	// SizeGiB is what `ember store create` asks for. It is not enforced
	// against an existing store: providers let a volume grow and never
	// shrink, so a number here that disagrees with reality is reported
	// rather than acted on.
	SizeGiB int `yaml:"size_gib"`

	// Region is the datacentre the store lives in. A store pins every node
	// that mounts it to its own datacentre, so this is read when choosing an
	// offer and not only when creating.
	Region string `yaml:"region"`

	// Mount is where the store is attached inside the node. Empty means
	// DefaultMount.
	Mount string `yaml:"mount"`
}

// HistoryConfig is the `history:` block.
type HistoryConfig struct {
	// KeepEvents is how long after a request finishes its per-step event rows
	// are worth keeping. Empty, or zero, keeps them forever.
	//
	// OFF BY DEFAULT, AND IT IS THE HORIZON RATHER THAN THE SCHEDULE. Nothing
	// in ember deletes an operator's data as a side effect of running a
	// generation -- the inverse of the rule that stops a rental creating a
	// standing monthly charge -- so this sets the default for
	// `ember history prune --older-than` and runs nothing itself. The verb is
	// what deletes; this is what makes typing the verb not also mean choosing
	// a number every time.
	//
	// A duration and not a row count, because what a count means depends on
	// the shape of the runs behind it: one FLUX.2 batch at 28 steps with
	// previews every 5 emits rows per step per job per pass, and passes
	// restart for each batch item and each highres pass. "Thirty days" is a
	// retention policy; "ten thousand rows" is a number nobody can reason
	// about.
	KeepEvents time.Duration `yaml:"keep_events"`
}

// KeepEvents is the configured retention horizon for event rows.
//
// The shape of ParseAssets and ParseImages: a value receiver, no arguments,
// empty is not an error. A NEGATIVE VALUE IS REFUSED RATHER THAN CLAMPED, for
// the reason `ember store abort` refuses a negative --older-than: it reads as
// "no bound", and a horizon of "ended before now plus a month" matches every
// request that has ever finished. Clamping it to zero would be the other
// defensible guess and would silently mean "keep everything" to somebody who
// wrote -720h meaning "drop everything".
func (c Config) KeepEvents() (time.Duration, error) {
	if c.History.KeepEvents < 0 {
		return 0, fmt.Errorf("history.keep_events is %s; it is how long to KEEP events after a "+
			"request finishes, so a negative horizon would select every request that has ever "+
			"ended -- leave it out to keep them forever", c.History.KeepEvents)
	}
	return c.History.KeepEvents, nil
}

// SDConfig is the `sd:` block.
type SDConfig struct {
	// Serve is the sd-server argv. ember appends the listen address itself
	// and refuses a command that names it -- see sdserver.ServeCommand.
	Serve []string `yaml:"serve"`

	// ReadyTimeout is how long the weights have to load. Empty is
	// sdserver.DefaultModelTimeout, which is longer than the GPU probe's
	// because the thing being waited for is bigger: FLUX.2 is 56 GiB read off
	// a network volume.
	ReadyTimeout time.Duration `yaml:"ready_timeout"`

	// Preview and PreviewInterval are the progress stream: none, proj, tae or
	// vae, and how many sampling steps between frames.
	//
	// Empty is proj every 5 steps, which is deliberately not upstream's
	// default of a frame every single step. tae and vae run a decode to
	// produce each frame, so upstream's default is the expensive one -- on a
	// card billing by the second, for frames that in an unattended batch
	// nobody is watching.
	Preview         string `yaml:"preview"`
	PreviewInterval int    `yaml:"preview_interval"`

	// Jobs are the images to generate, in order, on one rental.
	Jobs []SDJob `yaml:"jobs"`
}

// SDJob is one image.
//
// A typed subset of sd-server's request plus Extra, rather than a mirror of
// it. /sdcpp/v1/img_gen has around forty top-level fields and four nested
// objects: typing all of them would be a second copy of somebody else's
// schema, which every upstream addition makes wrong until ember is released,
// and typing none would mean ember could not check a sampler name or report
// which seed produced which file.
type SDJob struct {
	Prompt         string `yaml:"prompt"`
	NegativePrompt string `yaml:"negative_prompt"`

	Width      int   `yaml:"width"`
	Height     int   `yaml:"height"`
	Steps      int   `yaml:"steps"`
	BatchCount int   `yaml:"batch_count"`
	Seed       int64 `yaml:"seed"`

	Sampler   string `yaml:"sampler"`
	Scheduler string `yaml:"scheduler"`

	// CFG is the prompt guidance and Guidance is the distilled guidance.
	// Two different numbers that both get called "cfg" in conversation -- the
	// flux2-dev recipe sets cfg-scale 1 and guidance 4, so a config that
	// conflated them would quietly generate at the wrong one.
	CFG      float64 `yaml:"cfg"`
	Guidance float64 `yaml:"guidance"`

	// Output is the local filename stem. It never reaches the server.
	Output string `yaml:"output"`

	// ClipSkip, Eta and FlowShift are pointers so that an absent key and a
	// zero are different. The server's own defaults for them are -1, null
	// and null, so zero cannot stand for unset the way it does above --
	// `clip_skip: 0` and `eta: 0` are both things a model is told.
	ClipSkip  *int     `yaml:"clip_skip"`
	Eta       *float64 `yaml:"eta"`
	FlowShift *float64 `yaml:"flow_shift"`

	ShiftedTimestep int `yaml:"shifted_timestep"`

	// OutputFormat is checked against the list the server publishes, so a
	// typo here is refused before anything is rented. Compression applies to
	// jpeg and webp; png ignores it.
	OutputFormat      string `yaml:"output_format"`
	OutputCompression int    `yaml:"output_compression"`

	// Nested where the API nests, because these are read as units: hires
	// with `enabled` and nothing else is a request for upstream's own
	// defaults, and slg with layers and no scale is off.
	SLG       SDSLG       `yaml:"slg"`
	Hires     SDHires     `yaml:"hires"`
	VAETiling SDVAETiling `yaml:"vae_tiling"`

	// Extra reaches every field the typed ones above do not set, so nothing
	// in the API is unreachable from a config. A key that collides with one
	// ember set is refused rather than resolved: a seed or a step count
	// resolved the wrong way makes two runs that claim to be the same
	// measurement produce different images.
	Extra map[string]any `yaml:"extra"`
}

// SDSLG is skip-layer guidance, as a config writes it.
//
// Scale is the switch: upstream's default is 0, which is off, so layers with
// no scale configure something that never runs.
type SDSLG struct {
	Layers     []int   `yaml:"layers"`
	LayerStart float64 `yaml:"layer_start"`
	LayerEnd   float64 `yaml:"layer_end"`
	Scale      float64 `yaml:"scale"`
}

// SDHires is the second, higher-resolution pass, as a config writes it.
type SDHires struct {
	Enabled           bool    `yaml:"enabled"`
	Upscaler          string  `yaml:"upscaler"`
	Scale             float64 `yaml:"scale"`
	TargetWidth       int     `yaml:"target_width"`
	TargetHeight      int     `yaml:"target_height"`
	Steps             int     `yaml:"steps"`
	DenoisingStrength float64 `yaml:"denoising_strength"`
	UpscaleTileSize   int     `yaml:"upscale_tile_size"`
}

// SDVAETiling decodes the latent in tiles, as a config writes it.
type SDVAETiling struct {
	Enabled       bool    `yaml:"enabled"`
	TileSizeW     int     `yaml:"tile_size_w"`
	TileSizeH     int     `yaml:"tile_size_h"`
	TargetOverlap float64 `yaml:"target_overlap"`
	RelSizeW      float64 `yaml:"rel_size_w"`
	RelSizeH      float64 `yaml:"rel_size_h"`
}

// params is the config's spelling of these turned into the client's.
//
// A method and not an inline literal at the call site, because the queue maps
// the same parameters from its own wire type: two literals in two packages
// are two places to forget a field, and a forgotten field here is a
// generation that silently ignored what the operator asked for.
func (j SDJob) params() sdserver.Params {
	return sdserver.Params{
		ClipSkip:          j.ClipSkip,
		Eta:               j.Eta,
		FlowShift:         j.FlowShift,
		ShiftedTimestep:   j.ShiftedTimestep,
		OutputFormat:      j.OutputFormat,
		OutputCompression: j.OutputCompression,
		SLG: sdserver.SLG{
			Layers:     j.SLG.Layers,
			LayerStart: j.SLG.LayerStart,
			LayerEnd:   j.SLG.LayerEnd,
			Scale:      j.SLG.Scale,
		},
		Hires: sdserver.Hires{
			Enabled:           j.Hires.Enabled,
			Upscaler:          j.Hires.Upscaler,
			Scale:             j.Hires.Scale,
			TargetWidth:       j.Hires.TargetWidth,
			TargetHeight:      j.Hires.TargetHeight,
			Steps:             j.Hires.Steps,
			DenoisingStrength: j.Hires.DenoisingStrength,
			UpscaleTileSize:   j.Hires.UpscaleTileSize,
		},
		VAETiling: sdserver.VAETiling{
			Enabled:       j.VAETiling.Enabled,
			TileSizeW:     j.VAETiling.TileSizeW,
			TileSizeH:     j.VAETiling.TileSizeH,
			TargetOverlap: j.VAETiling.TargetOverlap,
			RelSizeW:      j.VAETiling.RelSizeW,
			RelSizeH:      j.VAETiling.RelSizeH,
		},
	}
}

// DefaultMount is where a store is attached when the config does not say.
//
// Not /workspace, which is the provider default and also where ember writes
// outputs: a store mounted there would bill for every render for as long as
// the store exists. node.MountAt refuses that path outright; this is the
// value that keeps anyone from having to find out.
const DefaultMount = "/models"

// StoreMount is where this configuration attaches its store.
func (c Config) StoreMount() (node.Mount, error) {
	p := c.Store.Mount
	if p == "" {
		p = DefaultMount
	}
	return node.MountAt(p)
}

// HasStore reports whether this configuration names a store.
func (c Config) HasStore() bool { return c.Store.Name != "" }

// ParseAssets validates the asset list and returns it in domain form.
//
// Refused as a set rather than one at a time, so two entries that land on the
// same name with different digests are caught here: whichever fetch ran
// second would overwrite a verified file or fail against a digest that was
// never meant for it, and the error would blame the store for what the config
// asked for.
func (c Config) ParseAssets() ([]node.Asset, error) {
	if len(c.Assets) == 0 {
		return nil, nil
	}
	if !c.HasStore() {
		return nil, fmt.Errorf("assets are listed but no store is named; they would be written to the "+
			"node's own disk and thrown away with it. Add a store block, or remove the %d asset(s)", len(c.Assets))
	}
	out := make([]node.Asset, 0, len(c.Assets))
	for _, a := range c.Assets {
		out = append(out, node.Asset{URL: a.URL, SHA256: a.SHA256, As: a.As})
	}
	return node.ValidateAssets(out)
}

// ParseImages validates the per-architecture image map, keyed by capability.
//
// Every entry is checked, not just the one a given run will use. A config with
// six images and one bad digest would otherwise sit committed until the day
// the sixth card is the cheapest offer, and the refusal would arrive in the
// one place it is useless -- after the offer was chosen, with the operator
// looking at a card they did not ask for.
//
// Empty is not an error: a single pinned image: is the supported shape and
// always will be, because the CPU variant and the node image have no
// architecture to key on.
func (c Config) ParseImages() (map[node.ComputeCap]string, error) {
	if len(c.Images) == 0 {
		return nil, nil
	}
	out := make(map[node.ComputeCap]string, len(c.Images))
	for k, ref := range c.Images {
		cap, err := node.ParseImageKey(k)
		if err != nil {
			return nil, err
		}
		if err := node.ValidateImage(ref); err != nil {
			return nil, fmt.Errorf("images[%s]: %w", k, err)
		}
		if err := node.ValidateImageForArch(ref, cap); err != nil {
			return nil, fmt.Errorf("images[%s]: %w", k, err)
		}
		out[cap] = ref
	}
	return out, nil
}

// Default is what ember uses with no file and no flags.
func Default() Config {
	return Config{
		Provider: "local",
		TTL:      time.Hour,
	}
}

// Load reads path, or returns the defaults when it does not exist.
//
// A missing config is normal: every setting has a flag. A malformed one is
// not, and is an error rather than a fallback — silently running with
// defaults because a budget line had a typo is how a ceiling stops applying.
func Load(path string) (Config, error) {
	cfg := Default()

	raw, err := os.ReadFile(path) //nolint:gosec // a path the operator named
	if errors.Is(err, os.ErrNotExist) {
		return cfg, nil
	}
	if err != nil {
		return Config{}, fmt.Errorf("read %s: %w", path, err)
	}

	// Before the strict decode, so that a key ember used to accept gets
	// ember's sentence rather than yaml's. See rejectRemovedKeys.
	if err := rejectRemovedKeys(raw); err != nil {
		return Config{}, fmt.Errorf("%s: %w", path, err)
	}

	dec := yaml.NewDecoder(strings.NewReader(string(raw)))
	dec.KnownFields(true) // a misspelled key is a typo, not a comment
	if err := dec.Decode(&cfg); err != nil {
		return Config{}, fmt.Errorf("parse %s: %w", path, err)
	}
	if err := cfg.rejectSecrets(); err != nil {
		return Config{}, fmt.Errorf("%s: %w", path, err)
	}
	// AT LOAD AND NOT AT THE FIRST REQUEST. A duplicate or malformed voice id
	// is a mistake in a file the operator just edited, and its symptom at
	// lookup time is a UI offering a choice that cannot be made -- read as
	// ember being broken rather than as their own typo. The same argument
	// ParseSD makes for refusing a mistyped sampler before a card is rented,
	// one layer earlier because a catalogue is published rather than
	// submitted.
	if _, err := cfg.TTSCatalogue(); err != nil {
		return Config{}, fmt.Errorf("%s: %w", path, err)
	}
	return cfg, nil
}

// rejectRemovedKeys answers for a top-level key ember used to accept.
//
// KnownFields(true) already refuses an unknown key, so a removal is not
// silence -- but what it says is "field min_cuda not found in type
// config.Config", which names a Go type an operator should never see and no
// remedy at all. `min_cuda` was in README for the life of the key, so the
// person who hits this is precisely the one who followed the documentation,
// and the remedy is specific: gpu_probe.
//
// (This comment cited a flag that has never existed as its precedent, and
// TestComments_CiteNoFlagThatNoFlagSetDeclares reported it on the first run of
// the suite -- the eighteen-comment sweep that guard was built for, walked
// into by the change that was removing a key for the same reason. The rule it
// was reaching for needs no citation: a documented thing nobody can use is
// worse than no documented thing.)
//
// Presence and not emptiness, which is why this is a map rather than a struct
// of pointers: `min_cuda:` with no value decodes as null and leaves a pointer
// nil, so the one spelling that would be waved through is the one an operator
// reaches for when they want to turn the key off without deleting the line.
//
// One entry, and the list may only shrink. A table of removed keys maintained
// by hand is the thing this repository refuses -- what makes it earn its place
// is that it is bounded by what was once documented, so it cannot grow except
// by a removal, and an entry nobody can still be carrying is a paragraph
// nobody needs.
func rejectRemovedKeys(raw []byte) error {
	var top map[string]yaml.Node
	if err := yaml.Unmarshal(raw, &top); err != nil {
		// A file this cannot read is one the strict decode cannot either, and
		// that is the error worth printing because it names the line. Saying
		// anything here would describe the wrong problem.
		return nil //nolint:nilerr // the strict decode below reports it with a line number
	}
	if _, ok := top["min_cuda"]; !ok {
		return nil
	}
	return errors.New("min_cuda is no longer a config key, because ember could not honour it " +
		"anywhere: api.runpod.io/v2/pods, where ember creates, has no CUDA field and answered " +
		"422 to the allowedCudaVersions ember used to send, and the per-GPU cudaVersions its " +
		"catalogue does report tracks whether the card has capacity rather than what a host can " +
		"run -- a card that is rentable in one datacentre reports every CUDA version " +
		"unavailable -- so narrowing offers by it would refuse a card for every value of " +
		"min_cuda, including values the card satisfies.\n" +
		"Delete the line. To refuse a host that cannot run the image, use gpu_probe: it runs on " +
		"the node and fails before any workload does, costing the probe rather than the run")
}

// rejectSecrets fails when the file looks like it holds a credential.
//
// This file is meant to be committed. A key pasted into it is not revocable by
// deleting the line later, so the first run after the mistake is the right
// time to say so.
// ParseSD turns the `sd:` block into the run a served batch needs.
//
// The shape of ParseAssets and ParseImages: a value receiver, no arguments,
// empty is not an error, the cross-field precondition first, and every entry
// validated rather than only the one this run reaches. A config with nine good
// jobs and a tenth whose extra shadows its seed would otherwise sit committed
// until the day somebody waits an hour for the tenth.
//
// Every refusal below lives in internal/sdserver. This function is the loop
// and the key paths, which is what keeps the meaning in one place.
func (c Config) ParseSD() (sdserver.Run, error) {
	if len(c.SD.Jobs) == 0 && len(c.SD.Serve) == 0 {
		return sdserver.Run{}, nil
	}

	// Each half is useless without the other, and both failures look like a
	// run that worked. Jobs with no server have nothing to send them to;
	// a server with no jobs rents a card, loads the weights and generates
	// nothing -- which bills for the whole TTL and produces an empty output
	// directory.
	if len(c.SD.Serve) == 0 {
		return sdserver.Run{}, fmt.Errorf("sd.jobs has %d job(s) but sd.serve is empty, "+
			"so there is no server to send them to", len(c.SD.Jobs))
	}
	if len(c.SD.Jobs) == 0 {
		return sdserver.Run{}, errors.New("sd.serve is set but sd.jobs is empty, " +
			"so the node would rent a card, load the weights and generate nothing")
	}

	preview, err := sdserver.ParsePreview(c.SD.Preview)
	if err != nil {
		return sdserver.Run{}, fmt.Errorf("sd.preview: %w", err)
	}
	interval, err := sdserver.ParsePreviewInterval(c.SD.PreviewInterval)
	if err != nil {
		return sdserver.Run{}, fmt.Errorf("sd.preview_interval: %w", err)
	}

	run := sdserver.Run{Preview: preview, PreviewInterval: interval}
	for i, j := range c.SD.Jobs {
		job := sdserver.Job{
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
			Params:         j.params(),
			Extra:          j.Extra,
		}
		// Everything checkable without a server, which is where a key in
		// extra that shadows a typed field is refused -- and doing it here
		// means that happens before anything is rented rather than at
		// submission. One function, shared with the path a queued request
		// takes, so the refusals do not exist twice.
		if err := sdserver.ValidateStatic(job, preview, interval); err != nil {
			return sdserver.Run{}, fmt.Errorf("sd.jobs[%d]: %w", i, err)
		}
		run.Jobs = append(run.Jobs, job)
	}
	return run, nil
}

// TTSConfig is the `tts:` block.
//
// SHAPED LIKE `sd:` DOWN TO THE FIELD ORDER, because the two are the same kind
// of thing from a config file's point of view: a per-rental setting or two and
// then a list of jobs. What it has no equivalent of is `serve` and
// `ready_timeout` -- there is no server to start and nothing to wait for --
// and that absence is the whole difference between the two drivers.
type TTSConfig struct {
	// Voice is the .onnx voice on the node.
	//
	// ONE VOICE PER RENTAL, for the reason `sd.serve` is one server per
	// rental: it is a file on the node, so it is the operator's to choose and
	// a request's to know nothing about -- and the day this becomes resident
	// there is exactly one process to hold open.
	//
	// Empty is the voice ember's own TTS image bakes in, so a configuration
	// using that image needs no path at all. Named rather than discovered,
	// because a directory scan on the node would make the voice a thing that
	// changes when somebody writes to the store -- which is the channel
	// between runs that asset digests exist to close.
	Voice string `yaml:"voice"`

	// EspeakData is piper's espeak-ng data directory on the node.
	//
	// It has a default for the same reason Voice does, and it is a key rather
	// than a constant because an image that put it elsewhere would otherwise
	// be unusable with no way to say so. AGENTS.md's rule that an image's ENV
	// does not reach anything sshd starts is why ember names it on every
	// invocation rather than trusting ESPEAK_DATA_PATH.
	EspeakData string `yaml:"espeak_data"`

	// Voices is the published catalogue: the model choices a REQUEST may name.
	//
	// WHY A LIST EXISTS AT ALL AND WHY IT IS NOT INFERRED. A browser cannot be
	// handed a path -- a request names a provider, a card and a batch of jobs
	// and nothing else -- so the only way it can choose a voice is to pick an
	// **id** out of a list ember published, which is #190's shape and this
	// repository's allow-list rule for anything dangerous. See
	// internal/tts/catalogue.go.
	//
	// It is DELIBERATELY NOT derived from `voice` above. An implicit
	// single-entry catalogue would be ember publishing a choice the operator
	// did not make -- and the sharp version is that `voice` has a default
	// pointing at ember's own speech image, so an operator running `ember
	// serve` with `image: ember:sd` and no `tts:` block at all would be shown
	// a voice that cannot work. A UI offering a button that fails after a card
	// is rented is the "reads as working" defect with a rental attached.
	//
	// So an empty list means **this process serves no speech to a browser**,
	// and `POST /api/requests` with kind `tts` is refused naming this key.
	// `ember up` is unaffected: a config file names one voice because it names
	// one run, and `voice` above is still that.
	Voices []TTSVoice `yaml:"voices"`

	// Jobs are the utterances, in order, on one rental.
	Jobs []TTSJob `yaml:"jobs"`
}

// TTSVoice is one catalogue entry.
//
// The paths are the operator's and are unreachable from any request, which is
// the whole reason this type is in the config and not on the wire.
type TTSVoice struct {
	// ID is the opaque string a request names. Lowercase letters, digits,
	// dot, underscore and dash; see tts.ValidateVoiceID for why an
	// allow-list on a field that reaches no path today.
	ID string `yaml:"id"`

	// Name is what a UI shows. Free text, read by nobody but a human.
	Name string `yaml:"name"`

	// Voice and EspeakData are the files on the node. EspeakData is optional
	// and falls back to the image's, because every entry of a list repeating
	// one path is a list with one typo waiting in it.
	Voice      string `yaml:"voice"`
	EspeakData string `yaml:"espeak_data"`
}

// DefaultVoice and DefaultEspeakData are where ember's own TTS image puts them.
//
// CONSTANTS HERE AND NOT IN internal/tts, because they are a fact about an
// image this repository builds rather than about piper -- the same split that
// keeps sd-server's flag spellings in internal/sdserver and the model path in
// a config. A different image names its own paths and neither constant is
// consulted.
const (
	DefaultVoice      = "/opt/voices/en_US-lessac-medium.onnx"
	DefaultEspeakData = "/opt/piper/espeak-ng-data"
)

// TTSJob is one utterance.
//
// A TYPED SUBSET OF NOTHING, which is the opposite of SDJob and is worth
// saying because the asymmetry looks like an omission. SDJob types eleven of
// sd-server's forty-odd fields and carries the rest through `extra`, because
// the server publishes more than ember should mirror. piper publishes six
// options in total -- `--speaker`, `--length_scale`, `--noise_scale`,
// `--noise_w`, `--sentence_silence` and the two paths -- so this block is the
// WHOLE surface and there is nothing for an `extra` to carry. There is
// therefore no `extra` here, which also means nothing has to be refused over
// HTTP later: the thing a request could abuse does not exist.
type TTSJob struct {
	// Text is what to say.
	Text string `yaml:"text"`

	// Output is the filename stem, without an extension. ember numbers it and
	// appends .wav.
	Output string `yaml:"output"`

	// Speaker picks a voice of a multi-speaker model.
	//
	// POINTERS, ALL OF THEM, because piper's defaults are 0, 1.0, 0.667, 0.8
	// and 0.2 -- so for Speaker zero is a real speaker id and for the rest
	// zero is a value an operator might mean. A zero sentinel would make the
	// first speaker and the fastest speech this model will do unrequestable
	// while looking exactly like a field that works, which is clip_skip's
	// rule and the fifth time it has been reached from a different default.
	Speaker         *int     `yaml:"speaker"`
	LengthScale     *float64 `yaml:"length_scale"`
	NoiseScale      *float64 `yaml:"noise_scale"`
	NoiseW          *float64 `yaml:"noise_w"`
	SentenceSilence *float64 `yaml:"sentence_silence"`
}

// ParseTTS turns the `tts:` block into the run a spoken batch needs.
//
// The shape of ParseSD and ParseAssets: a value receiver, no arguments, empty
// is not an error, and every entry validated rather than only the one this run
// reaches -- a config with nine good utterances and a tenth whose output is a
// path would otherwise sit committed until the day somebody pays for the
// tenth.
//
// Every refusal lives in internal/tts. This function is the loop and the two
// defaults, which is what keeps the meaning in one place.
func (c Config) ParseTTS() (tts.Run, error) {
	if len(c.TTS.Jobs) == 0 {
		// NOT an error when voice or espeak_data is set with no jobs, unlike
		// `sd.serve` with no `sd.jobs`. That one rents a card, loads the
		// weights and generates nothing -- it costs the whole TTL. A voice
		// path with no utterances starts no process and costs nothing, so it
		// is a line an operator left behind rather than a run that burns
		// money, and refusing it would break `ember up --cmd` against a
		// config that also happens to describe a voice.
		return tts.Run{}, nil
	}

	// THE SAME TWO PATHS FOR EVERY JOB OF AN `ember up`, and the asymmetry
	// with a request is stated rather than left to be noticed: a config file
	// names ONE voice because it names one run, and a request picks from the
	// catalogue because a browser has a list in front of it. One function
	// resolves both, so the two cannot come to disagree about what a voice
	// is -- it is just that this one has nothing to look up.
	voice := firstNonEmpty(c.TTS.Voice, DefaultVoice)
	espeak := firstNonEmpty(c.TTS.EspeakData, DefaultEspeakData)

	var run tts.Run
	for _, j := range c.TTS.Jobs {
		run.Jobs = append(run.Jobs, tts.Job{
			Text:            j.Text,
			Output:          j.Output,
			Voice:           voice,
			EspeakData:      espeak,
			Speaker:         j.Speaker,
			LengthScale:     j.LengthScale,
			NoiseScale:      j.NoiseScale,
			NoiseW:          j.NoiseW,
			SentenceSilence: j.SentenceSilence,
		})
	}
	// One call and not a check per field here, so the refusals exist once --
	// and so the path a request would one day take gets the identical set.
	if err := run.Validate(); err != nil {
		return tts.Run{}, err
	}
	return run, nil
}

// TTSCatalogue is the voices a REQUEST may name.
//
// EMPTY IS NOT AN ERROR AND IS NOT A DEFAULT EITHER. A configuration that
// publishes none serves no speech to a browser, which `POST /api/requests`
// says in one sentence naming `tts.voices`; see the field's own comment for
// why an implicit entry would be ember publishing a choice nobody made.
//
// The espeak fallback is here rather than in internal/tts, because it is a
// fact about the image this repository builds and not about piper -- the same
// split that keeps sd-server's flag spellings in internal/sdserver and a model
// path in a config.
func (c Config) TTSCatalogue() (tts.Catalogue, error) {
	if len(c.TTS.Voices) == 0 {
		return nil, nil
	}
	cat := make(tts.Catalogue, 0, len(c.TTS.Voices))
	for _, v := range c.TTS.Voices {
		cat = append(cat, tts.Voice{
			ID:         v.ID,
			Name:       firstNonEmpty(v.Name, v.ID),
			Voice:      v.Voice,
			EspeakData: firstNonEmpty(v.EspeakData, firstNonEmpty(c.TTS.EspeakData, DefaultEspeakData)),
		})
	}
	// One call, so every refusal about an id or a path exists once -- and at
	// LOAD rather than at lookup, because a duplicate id's symptom is that one
	// of two voices can never be chosen, which an operator reads as the UI
	// being broken rather than as their own file.
	if err := cat.Validate(); err != nil {
		return nil, err
	}
	return cat, nil
}

// firstNonEmpty is the default-if-blank rule, named once.
func firstNonEmpty(v, def string) string {
	if strings.TrimSpace(v) == "" {
		return def
	}
	return v
}

func (c Config) rejectSecrets() error {
	// Built rather than written as a literal, because the literal was the
	// whole set of free-text scalars and `images:` adds a map of them. A
	// reference pasted there would otherwise be the one place in a committed
	// file that is not scanned, and the keys name the entry so the error can
	// point at it.
	fields := map[string]string{
		"image": c.Image, "provider": c.Provider, "gpu": c.GPU, "region": c.Region,
	}
	for k, v := range c.Images {
		fields["images["+k+"]"] = v
	}

	for name, v := range fields {
		l := strings.ToLower(v)
		for _, marker := range []string{"api_key", "apikey", "secret", "token", "password", "-----begin"} {
			if strings.Contains(l, marker) {
				return fmt.Errorf("%s looks like a credential; ember reads those from the environment or ~/.config/ember/credentials.yaml (0600), never from a committed file", name)
			}
		}
	}
	return nil
}

// Filter builds the offer filter this config describes.
func (c Config) Filter() (node.Filter, error) {
	f := node.Filter{
		GPU:                node.NormalizeGPU(c.GPU),
		MinVRAM:            node.MiB(c.MinVRAMGiB) * 1024,
		AllowInterruptible: c.AllowInterruptible,
		Region:             c.Region,
	}
	if c.MinVRAMGiB < 0 {
		return node.Filter{}, errors.New("min_vram_gib is negative")
	}
	if c.MaxHourlyUSD != "" {
		rate, err := node.ParseUSD(c.MaxHourlyUSD)
		if err != nil {
			return node.Filter{}, fmt.Errorf("max_hourly_usd: %w", err)
		}
		f.MaxPrice = node.Rate(rate)
	}
	return f, f.Validate()
}

// Budgets returns the hourly and whole-run ceilings.
func (c Config) Budgets() (hourly node.Rate, run node.MicroUSD, err error) {
	if c.MaxHourlyUSD != "" {
		v, perr := node.ParseUSD(c.MaxHourlyUSD)
		if perr != nil {
			return 0, 0, fmt.Errorf("max_hourly_usd: %w", perr)
		}
		hourly = node.Rate(v)
	}
	if c.MaxRunUSD != "" {
		v, perr := node.ParseUSD(c.MaxRunUSD)
		if perr != nil {
			return 0, 0, fmt.Errorf("max_run_usd: %w", perr)
		}
		run = v
	}
	return hourly, run, nil
}

// PeriodCeilings returns the per-day and per-month rental ceilings.
//
// Separate from Budgets because the two are checked against different things:
// Budgets bounds one offer and one rental, and these bound every rental in a
// period together -- which needs the generation history and therefore cannot
// be answered by a pure function of this file.
//
// Zero means no ceiling for that period, as it does everywhere else here, so
// a config that sets neither behaves exactly as it did before they existed.
func (c Config) PeriodCeilings() (day, month node.MicroUSD, err error) {
	if c.MaxDayUSD != "" {
		v, perr := node.ParseUSD(c.MaxDayUSD)
		if perr != nil {
			return 0, 0, fmt.Errorf("max_day_usd: %w", perr)
		}
		day = v
	}
	if c.MaxMonthUSD != "" {
		v, perr := node.ParseUSD(c.MaxMonthUSD)
		if perr != nil {
			return 0, 0, fmt.Errorf("max_month_usd: %w", perr)
		}
		month = v
	}
	return day, month, nil
}

// ParsePorts turns the "name:remote[:local]" entries into port maps.
//
// The local port defaults to the remote one, which is what an operator means
// by "forward 8188" — and both are validated by node.NewSpec afterwards.
func (c Config) ParsePorts() ([]node.PortMap, error) {
	out := make([]node.PortMap, 0, len(c.Ports))
	for _, s := range c.Ports {
		pm, err := ParsePort(s)
		if err != nil {
			return nil, err
		}
		out = append(out, pm)
	}
	return out, nil
}

// ParsePort reads one "name:remote[:local]" entry.
func ParsePort(s string) (node.PortMap, error) {
	parts := strings.Split(strings.TrimSpace(s), ":")
	if len(parts) < 2 || len(parts) > 3 {
		return node.PortMap{}, fmt.Errorf("port %q: write it as name:remote or name:remote:local", s)
	}
	name := strings.TrimSpace(parts[0])
	if name == "" {
		return node.PortMap{}, fmt.Errorf("port %q has no name", s)
	}
	remote, err := parsePortNumber(parts[1])
	if err != nil {
		return node.PortMap{}, fmt.Errorf("port %q: remote %w", s, err)
	}
	local := remote
	if len(parts) == 3 {
		local, err = parsePortNumber(parts[2])
		if err != nil {
			return node.PortMap{}, fmt.Errorf("port %q: local %w", s, err)
		}
	}
	return node.PortMap{Name: name, Remote: remote, Local: local}, nil
}

func parsePortNumber(s string) (uint16, error) {
	s = strings.TrimSpace(s)
	var n int
	if _, err := fmt.Sscanf(s, "%d", &n); err != nil || fmt.Sprint(n) != s {
		return 0, fmt.Errorf("%q is not a port number", s)
	}
	if n < 1 || n > 65535 {
		return 0, fmt.Errorf("%d is outside 1..65535", n)
	}
	return uint16(n), nil
}

// ParseOutputs turns the configured output paths into OutputPaths.
//
// The spelling is "remote" or "remote:local". A bare remote path derives its
// local name from the base, so `outputs: [/workspace/out]` lands in a
// directory called "out" rather than one named after the whole remote path.
func (c Config) ParseOutputs() ([]node.OutputPath, error) {
	out := make([]node.OutputPath, 0, len(c.Outputs))
	for _, raw := range c.Outputs {
		s := strings.TrimSpace(raw)
		if s == "" {
			continue
		}
		remote, local, hasLocal := strings.Cut(s, ":")
		remote = strings.TrimSpace(remote)
		local = strings.TrimSpace(local)
		if !hasLocal || local == "" {
			local = path.Base(remote)
		}
		if remote == "" {
			return nil, fmt.Errorf("output %q has no remote path; write it as /remote/path or /remote/path:local-name", raw)
		}
		out = append(out, node.OutputPath{Remote: remote, Local: local})
	}
	return out, nil
}

// OutputDirOrDefault resolves where fetched outputs are written.
//
// Under the state directory by default, so a run's results sit beside the
// journal record of the run that produced them -- and so a single
// XDG_STATE_HOME move takes both.
func (c Config) OutputDirOrDefault() (string, error) {
	if c.OutputDir != "" {
		return c.OutputDir, nil
	}
	state, err := c.StateDirOrDefault()
	if err != nil {
		return "", err
	}
	return filepath.Join(state, "outputs"), nil
}

// ConfigDirOrDefault resolves where keys and credentials live.
func (c Config) ConfigDirOrDefault() (string, error) {
	if c.ConfigDir != "" {
		return c.ConfigDir, nil
	}
	base, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("find the user config directory: %w", err)
	}
	return filepath.Join(base, "ember"), nil
}

// StateDirOrDefault resolves where node records, the queue and staged inputs
// live.
//
// Separate from the config directory because these are machine-local facts
// about running nodes, not settings: a config directory synced between
// machines would carry records for nodes the other machine cannot see. Host
// key pins used to be the sharpest instance of that and are not here any
// more -- a pin is built in memory from the key ember minted and dies with
// the node, so nothing persists one (#120).
func (c Config) StateDirOrDefault() (string, error) {
	if c.StateDir != "" {
		return c.StateDir, nil
	}
	if dir := os.Getenv("XDG_STATE_HOME"); dir != "" {
		return filepath.Join(dir, "ember"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("find the home directory: %w", err)
	}
	return filepath.Join(home, ".local", "state", "ember"), nil
}
