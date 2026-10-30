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
)

// FileName is the config ember looks for in the working directory.
const FileName = "ember.yaml"

// Config is the whole of ember's settings.
type Config struct {
	// Provider is the default provider name.
	Provider string `yaml:"provider"`

	// Image is the workload's OCI reference. It must be pinned.
	Image string `yaml:"image"`

	// TTL bounds a node's life.
	TTL time.Duration `yaml:"ttl"`

	// Ports are "name:remote" or "name:remote:local" entries.
	Ports []string `yaml:"ports"`

	// GPU and MinVRAMGiB narrow the offers considered.
	GPU        string `yaml:"gpu"`
	MinVRAMGiB int    `yaml:"min_vram_gib"`
	AllowSpot  bool   `yaml:"allow_interruptible"`
	Region     string `yaml:"region"`

	// MaxHourlyUSD and MaxRunUSD are decimal strings, parsed into integer
	// microdollars. They are strings rather than floats on purpose: a YAML
	// float for 0.29 is 0.28999999999999998, and a budget that is sometimes
	// wrong by a rounding error is a budget nobody trusts.
	MaxHourlyUSD string `yaml:"max_hourly_usd"`
	MaxRunUSD    string `yaml:"max_run_usd"`

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

	// Store names the persistent store this configuration uses, if any.
	Store StoreConfig `yaml:"store"`

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

	// MinCUDA is the oldest CUDA runtime the image can run against, as
	// "major.minor" -- for example "12.6", which is what this repository's
	// workload-sd-cuda.Dockerfile builds against.
	//
	// It restricts placement to hosts new enough to run the image. Empty
	// asks for no constraint, which is what ember did for its whole life
	// before this key existed: every pod accepted any CUDA version,
	// including hosts too old for the image about to run on them -- a
	// failure that appears only after the pod is created and billing.
	//
	// Empty by default because the field is unmeasured. RunPod publishes it
	// and its enum, but nothing here has confirmed that it actually filters
	// placement, and a constraint that silently narrows capacity without
	// doing anything would be worse than none.
	MinCUDA string `yaml:"min_cuda"`
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

	dec := yaml.NewDecoder(strings.NewReader(string(raw)))
	dec.KnownFields(true) // a misspelled key is a typo, not a comment
	if err := dec.Decode(&cfg); err != nil {
		return Config{}, fmt.Errorf("parse %s: %w", path, err)
	}
	if err := cfg.rejectSecrets(); err != nil {
		return Config{}, fmt.Errorf("%s: %w", path, err)
	}
	return cfg, nil
}

// rejectSecrets fails when the file looks like it holds a credential.
//
// This file is meant to be committed. A key pasted into it is not revocable by
// deleting the line later, so the first run after the mistake is the right
// time to say so.
func (c Config) rejectSecrets() error {
	for name, v := range map[string]string{
		"image": c.Image, "provider": c.Provider, "gpu": c.GPU, "region": c.Region,
	} {
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
		AllowInterruptible: c.AllowSpot,
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

// StateDirOrDefault resolves where node records and host key pins live.
//
// Separate from the config directory because these are machine-local facts
// about running nodes, not settings: a config directory synced between
// machines would carry pins for nodes the other machine cannot see.
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
