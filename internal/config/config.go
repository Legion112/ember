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
