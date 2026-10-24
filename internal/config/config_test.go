package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Legion112/ember/internal/node"
)

func write(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), FileName)
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestLoad_MissingFileIsTheDefaults: every setting has a flag, so running
// without a config is normal.
func TestLoad_MissingFileIsTheDefaults(t *testing.T) {
	cfg, err := Load(filepath.Join(t.TempDir(), "absent.yaml"))
	if err != nil {
		t.Fatalf("a missing config should not be an error: %v", err)
	}
	if cfg.Provider != "local" || cfg.TTL != time.Hour {
		t.Errorf("defaults not applied: %+v", cfg)
	}
}

// TestLoad_RefusesAMisspelledKey. Silently ignoring one means a budget line
// with a typo stops applying and nothing says so.
func TestLoad_RefusesAMisspelledKey(t *testing.T) {
	p := write(t, "max_hourly_used: \"1.00\"\n")
	if _, err := Load(p); err == nil {
		t.Fatal("a misspelled key was accepted as if it were a comment")
	}
}

func TestLoad_RefusesMalformedYAML(t *testing.T) {
	if _, err := Load(write(t, "ports: [unclosed\n")); err == nil {
		t.Fatal("malformed YAML was accepted")
	}
}

// TestLoad_RefusesSomethingThatLooksLikeACredential. This file is meant to be
// committed, and a key pasted into it is not revocable by deleting the line.
func TestLoad_RefusesSomethingThatLooksLikeACredential(t *testing.T) {
	for name, body := range map[string]string{
		"api key in image": "image: \"registry/x:v1?api_key=abc\"\n",
		"a pasted key":     "gpu: \"-----BEGIN OPENSSH PRIVATE KEY-----\"\n",
		"a token":          "region: \"token-abc123\"\n",
	} {
		_, err := Load(write(t, body))
		if err == nil {
			t.Errorf("%s: accepted", name)
			continue
		}
		if !strings.Contains(err.Error(), "credential") {
			t.Errorf("%s: the error should explain why, got %v", name, err)
		}
	}
}

func TestLoad_ReadsEverySetting(t *testing.T) {
	cfg, err := Load(write(t, `
provider: runpod
image: ghcr.io/example/comfyui:v1
ttl: 30m
ports:
  - comfyui:8188
  - jupyter:8888:18888
gpu: "NVIDIA GeForce RTX 4090"
min_vram_gib: 20
allow_interruptible: true
region: eu
max_hourly_usd: "0.50"
max_run_usd: "1.00"
`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Provider != "runpod" || cfg.TTL != 30*time.Minute || len(cfg.Ports) != 2 {
		t.Fatalf("not read back: %+v", cfg)
	}

	f, err := cfg.Filter()
	if err != nil {
		t.Fatal(err)
	}
	if f.GPU != "rtx4090" {
		t.Errorf("the GPU name was not normalised: %q", f.GPU)
	}
	if f.MinVRAM != 20*1024 {
		t.Errorf("MinVRAM = %d MiB, want GiB converted", f.MinVRAM)
	}
	if !f.AllowInterruptible || f.Region != "eu" {
		t.Errorf("filter = %+v", f)
	}
	if f.MaxPrice != node.Rate(500000) {
		t.Errorf("MaxPrice = %v, want $0.50 in microdollars", f.MaxPrice)
	}

	hourly, run, err := cfg.Budgets()
	if err != nil {
		t.Fatal(err)
	}
	if hourly != 500000 || run != 1000000 {
		t.Errorf("budgets = %v, %v", hourly, run)
	}
}

// TestBudgets_AreParsedAsDecimalStringsNotFloats. A YAML float for 0.29 is
// 0.28999999999999998, and a budget wrong by a rounding error is one nobody
// trusts.
func TestBudgets_AreParsedAsDecimalStringsNotFloats(t *testing.T) {
	cfg := Config{MaxHourlyUSD: "0.29"}
	hourly, _, err := cfg.Budgets()
	if err != nil {
		t.Fatal(err)
	}
	if hourly != 290000 {
		t.Errorf("0.29/hr parsed as %d microdollars, want exactly 290000", hourly)
	}
}

func TestBudgets_RefuseMalformedAmounts(t *testing.T) {
	for name, cfg := range map[string]Config{
		"hourly": {MaxHourlyUSD: "cheap"},
		"run":    {MaxRunUSD: "-1"},
	} {
		if _, _, err := cfg.Budgets(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := (Config{MaxHourlyUSD: "nope"}).Filter(); err == nil {
		t.Error("Filter accepted a malformed ceiling")
	}
	if _, err := (Config{MinVRAMGiB: -1}).Filter(); err == nil {
		t.Error("Filter accepted negative VRAM")
	}
}

func TestParsePort(t *testing.T) {
	got, err := ParsePort("comfyui:8188")
	if err != nil {
		t.Fatal(err)
	}
	// "forward 8188" means the same port both ends.
	if got.Name != "comfyui" || got.Remote != 8188 || got.Local != 8188 {
		t.Errorf("got %+v", got)
	}

	got, err = ParsePort("jupyter:8888:18888")
	if err != nil {
		t.Fatal(err)
	}
	if got.Remote != 8888 || got.Local != 18888 {
		t.Errorf("got %+v", got)
	}
}

func TestParsePort_Refuses(t *testing.T) {
	for name, in := range map[string]string{
		"no port":       "comfyui",
		"too many":      "a:1:2:3",
		"no name":       ":8188",
		"remote zero":   "a:0",
		"remote words":  "a:eight",
		"remote high":   "a:70000",
		"local zero":    "a:8188:0",
		"local words":   "a:8188:eight",
		"trailing junk": "a:8188x",
	} {
		if got, err := ParsePort(in); err == nil {
			t.Errorf("%s: ParsePort(%q) = %+v, want an error", name, in, got)
		}
	}
}

func TestParsePorts_PropagatesFailures(t *testing.T) {
	if _, err := (Config{Ports: []string{"ok:1", "bad"}}).ParsePorts(); err == nil {
		t.Fatal("a malformed entry was skipped")
	}
	got, err := (Config{Ports: []string{"a:1", "b:2"}}).ParsePorts()
	if err != nil || len(got) != 2 {
		t.Fatalf("got %v, %v", got, err)
	}
}

// TestStateDirIsSeparateFromConfigDir: pins and node records are machine-local
// facts about running machines, not settings. A config directory synced
// between machines would carry pins for nodes the other one cannot see.
func TestStateDirIsSeparateFromConfigDir(t *testing.T) {
	cfg := Default()
	c, err := cfg.ConfigDirOrDefault()
	if err != nil {
		t.Fatal(err)
	}
	s, err := cfg.StateDirOrDefault()
	if err != nil {
		t.Fatal(err)
	}
	if c == s {
		t.Fatalf("config and state resolved to the same place: %s", c)
	}
}

func TestDirOverridesAreHonoured(t *testing.T) {
	cfg := Config{ConfigDir: "/tmp/c", StateDir: "/tmp/s"}
	if got, _ := cfg.ConfigDirOrDefault(); got != "/tmp/c" {
		t.Errorf("ConfigDir ignored: %q", got)
	}
	if got, _ := cfg.StateDirOrDefault(); got != "/tmp/s" {
		t.Errorf("StateDir ignored: %q", got)
	}
}

func TestStateDirHonoursXDG(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", "/tmp/xdgstate")
	got, err := Default().StateDirOrDefault()
	if err != nil {
		t.Fatal(err)
	}
	if got != filepath.Join("/tmp/xdgstate", "ember") {
		t.Errorf("got %q", got)
	}
}

func TestLoad_ReportsAnUnreadableFile(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores file permissions")
	}
	p := write(t, "provider: local\n")
	if err := os.Chmod(p, 0o000); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(p); err == nil {
		t.Fatal("an unreadable config was treated as absent")
	}
}

// TestDirDefaults_ReportWhenTheEnvironmentCannotAnswer. Falling back to a
// relative path would scatter host-key pins into whatever directory ember
// happened to be started from.
func TestDirDefaults_ReportWhenTheEnvironmentCannotAnswer(t *testing.T) {
	t.Setenv("HOME", "")
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("XDG_STATE_HOME", "")

	if got, err := Default().ConfigDirOrDefault(); err == nil {
		t.Errorf("ConfigDirOrDefault invented %q with no HOME", got)
	}
	if got, err := Default().StateDirOrDefault(); err == nil {
		t.Errorf("StateDirOrDefault invented %q with no HOME", got)
	}
}
