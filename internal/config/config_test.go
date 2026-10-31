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

func TestParseOutputs(t *testing.T) {
	c := Config{Outputs: []string{
		"/workspace/out",
		"/workspace/run.log:logs",
		"  /workspace/frames  ",
		"",
	}}
	got, err := c.ParseOutputs()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("got %+v", got)
	}
	// A bare remote path derives its local name from the base, so
	// `outputs: [/workspace/out]` lands in "out" rather than a directory
	// named after the whole remote path.
	if got[0] != (node.OutputPath{Remote: "/workspace/out", Local: "out"}) {
		t.Errorf("got %+v", got[0])
	}
	if got[1] != (node.OutputPath{Remote: "/workspace/run.log", Local: "logs"}) {
		t.Errorf("got %+v", got[1])
	}
	if got[2].Remote != "/workspace/frames" {
		t.Errorf("surrounding space was not trimmed: %+v", got[2])
	}
}

func TestParseOutputs_RefusesAnEmptyRemote(t *testing.T) {
	if _, err := (Config{Outputs: []string{":logs"}}).ParseOutputs(); err == nil {
		t.Fatal("an output with no remote path was accepted")
	}
}

func TestParseOutputs_NoneIsNotAnError(t *testing.T) {
	got, err := (Config{}).ParseOutputs()
	if err != nil || len(got) != 0 {
		t.Fatalf("got %+v, %v", got, err)
	}
}

// TestOutputDirOrDefault_SitsUnderTheStateDir, so a run's results are beside
// the journal record of the run that produced them and one XDG_STATE_HOME move
// takes both.
func TestOutputDirOrDefault_SitsUnderTheStateDir(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", "/tmp/xdgstate")
	got, err := Default().OutputDirOrDefault()
	if err != nil {
		t.Fatal(err)
	}
	if got != filepath.Join("/tmp/xdgstate", "ember", "outputs") {
		t.Errorf("got %q", got)
	}
}

func TestOutputDirOrDefault_HonoursTheOverride(t *testing.T) {
	got, err := Config{OutputDir: "/mnt/renders"}.OutputDirOrDefault()
	if err != nil {
		t.Fatal(err)
	}
	if got != "/mnt/renders" {
		t.Errorf("got %q", got)
	}
}

func TestOutputDirOrDefault_ReportsAnUnresolvableStateDir(t *testing.T) {
	t.Setenv("HOME", "")
	t.Setenv("XDG_STATE_HOME", "")
	if _, err := Default().OutputDirOrDefault(); err == nil {
		t.Fatal("a state dir that cannot be resolved was accepted")
	}
}

func TestConfig_HasStore(t *testing.T) {
	if (Config{}).HasStore() {
		t.Error("an empty config claims a store")
	}
	if !(Config{Store: StoreConfig{Name: "models"}}).HasStore() {
		t.Error("a config naming a store does not claim one")
	}
}

func TestConfig_StoreMount(t *testing.T) {
	// The default is /models and deliberately not /workspace, which is the
	// provider default and also where ember writes outputs: a store there
	// would bill for every render for as long as the store exists.
	m, err := (Config{}).StoreMount()
	if err != nil {
		t.Fatalf("StoreMount() = %v", err)
	}
	if m.Path() != DefaultMount {
		t.Errorf("default mount = %q, want %q", m.Path(), DefaultMount)
	}
	if m.Path() == "/workspace" {
		t.Error("the default mount is /workspace, which bills every render")
	}

	m, err = (Config{Store: StoreConfig{Mount: "/weights"}}).StoreMount()
	if err != nil {
		t.Fatalf("StoreMount() = %v", err)
	}
	if m.Path() != "/weights" {
		t.Errorf("mount = %q, want /weights", m.Path())
	}
}

func TestConfig_StoreMountRefusesADangerousPath(t *testing.T) {
	// The refusal lives in node.MountAt; this checks the config path reaches
	// it rather than defaulting around it. A yaml key that could spell
	// /workspace or /root would undo a sealed type with one line of text.
	for _, bad := range []string{"/workspace", "/workspace/out", "/root", "/etc", "models", "/"} {
		t.Run(bad, func(t *testing.T) {
			if _, err := (Config{Store: StoreConfig{Mount: bad}}).StoreMount(); err == nil {
				t.Errorf("mount %q was accepted", bad)
			}
		})
	}
}

func TestLoad_ReadsTheStoreBlock(t *testing.T) {
	p := write(t, "store:\n  name: models\n  size_gib: 40\n  region: EU-RO-1\n  mount: /weights\n")
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load = %v", err)
	}
	if !cfg.HasStore() {
		t.Fatal("the store block was not read")
	}
	if cfg.Store.Name != "models" || cfg.Store.SizeGiB != 40 || cfg.Store.Region != "EU-RO-1" {
		t.Errorf("store = %+v", cfg.Store)
	}
	m, err := cfg.StoreMount()
	if err != nil || m.Path() != "/weights" {
		t.Errorf("StoreMount = %q, %v", m.Path(), err)
	}
}

func TestConfig_ParseAssets(t *testing.T) {
	const good = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

	if got, err := (Config{}).ParseAssets(); err != nil || got != nil {
		t.Errorf("no assets = %v, %v", got, err)
	}

	// Assets with no store would be written to the node's own disk and
	// thrown away with it -- a run that downloads forty gigabytes and
	// discards them, reported as success.
	withoutStore := Config{Assets: []AssetConfig{{URL: "https://h/x", SHA256: good, As: "a.gguf"}}}
	if _, err := withoutStore.ParseAssets(); err == nil {
		t.Error("assets with no store were accepted")
	} else if !strings.Contains(err.Error(), "thrown away") {
		t.Errorf("err = %q", err)
	}

	withStore := withoutStore
	withStore.Store = StoreConfig{Name: "models"}
	got, err := withStore.ParseAssets()
	if err != nil {
		t.Fatalf("ParseAssets = %v", err)
	}
	if len(got) != 1 || got[0].As != "a.gguf" || got[0].SHA256 != good {
		t.Errorf("got %+v", got)
	}

	// Validation is the domain's, so a bad digest fails here too rather than
	// on the node after a pod has been created.
	bad := withStore
	bad.Assets = []AssetConfig{{URL: "https://h/x", SHA256: "nope", As: "a.gguf"}}
	if _, err := bad.ParseAssets(); err == nil {
		t.Error("an asset with a bad digest was accepted")
	}
}

func TestLoad_ReadsAssetsAndVerify(t *testing.T) {
	const good = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	p := write(t, "store:\n  name: models\n"+
		"verify: present\n"+
		"assets:\n  - url: https://h/x.gguf\n    sha256: "+good+"\n    as: sd15.gguf\n")
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load = %v", err)
	}
	if cfg.Verify != "present" {
		t.Errorf("verify = %q", cfg.Verify)
	}
	as, err := cfg.ParseAssets()
	if err != nil || len(as) != 1 || as[0].As != "sd15.gguf" {
		t.Fatalf("assets = %+v, %v", as, err)
	}
}

// pinned is a digest-pinned reference, which is what every images: value has
// to be.
func pinned(tag string) string {
	ref := "ghcr.io/legion112/ember-sd"
	if tag != "" {
		ref += ":" + tag
	}
	return ref + "@sha256:" + strings.Repeat("a", 64)
}

func TestConfig_ParseImages(t *testing.T) {
	// Empty is not an error. One pinned image: is the supported shape and
	// always will be, because the CPU variant has no architecture to key on.
	if got, err := (Config{}).ParseImages(); err != nil || got != nil {
		t.Errorf("no images: = %v, %v; want nil, nil", got, err)
	}

	good := Config{Images: map[string]string{
		"sm_75":  pinned("sd-cuda-sm75"),
		"sm_90":  pinned("sd-cuda-sm90"),
		"sm_120": pinned(""),
	}}
	got, err := good.ParseImages()
	if err != nil {
		t.Fatalf("ParseImages = %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d entries, want 3: %v", len(got), got)
	}
	if got[75] != pinned("sd-cuda-sm75") {
		t.Errorf("images[sm_75] = %q", got[75])
	}
	if got[120] != pinned("") {
		t.Errorf("images[sm_120] = %q", got[120])
	}
}

// TestConfig_ParseImages_RefusesEveryBadEntry.
//
// Checked as a set rather than one at a time: a config with six images and one
// bad digest would otherwise sit committed until the day the sixth card is
// the cheapest offer, and the refusal would arrive after the offer was chosen.
func TestConfig_ParseImages_RefusesEveryBadEntry(t *testing.T) {
	for name, c := range map[string]Config{
		"a key that is not an architecture": {Images: map[string]string{"sm75": pinned("")}},
		"an uppercase key":                  {Images: map[string]string{"SM_75": pinned("")}},
		"a dotted key":                      {Images: map[string]string{"sm_7.5": pinned("")}},
		"no architecture at all":            {Images: map[string]string{"default": pinned("")}},
		"an unpinned value":                 {Images: map[string]string{"sm_75": "ghcr.io/x/ember-sd"}},
		"a latest value":                    {Images: map[string]string{"sm_75": "ghcr.io/x/ember-sd:latest"}},
		"an empty value":                    {Images: map[string]string{"sm_75": ""}},
		"a tag naming another architecture": {Images: map[string]string{"sm_75": pinned("sd-cuda-sm90")}},
		// One good entry and one bad: the bad one still has to be found.
		"one bad entry among good ones": {Images: map[string]string{
			"sm_75": pinned("sd-cuda-sm75"),
			"sm_90": pinned("sd-cuda-sm89"),
		}},
	} {
		if got, err := c.ParseImages(); err == nil {
			t.Errorf("%s: accepted as %v", name, got)
		}
	}
}

// TestLoad_RefusesACredentialInAnImagesEntry.
//
// rejectSecrets used to iterate a literal map of the free-text scalars, so a
// token pasted into a per-architecture reference would have been the one value
// in a committed file that nothing scanned.
func TestLoad_RefusesACredentialInAnImagesEntry(t *testing.T) {
	_, err := Load(write(t, "images:\n  sm_90: \"registry/x:v1?api_key=abc\"\n"))
	if err == nil {
		t.Fatal("a credential in an images: entry was accepted")
	}
	if !strings.Contains(err.Error(), "credential") {
		t.Errorf("the error should explain why, got %v", err)
	}
	// And it names the entry, not just "images".
	if !strings.Contains(err.Error(), "images[sm_90]") {
		t.Errorf("the error should name the entry, got %v", err)
	}
}

// TestLoad_RefusesADuplicateArchitecture.
//
// A map cannot hold two entries for one architecture, but a YAML file can
// spell one twice -- and if the decoder took the last writer, half the config
// would be a comment. Verified rather than assumed: yaml.v3 refuses it.
func TestLoad_RefusesADuplicateArchitecture(t *testing.T) {
	_, err := Load(write(t, "images:\n  sm_80: a:v1\n  sm_80: b:v1\n"))
	if err == nil {
		t.Fatal("a duplicate architecture key was accepted; one of the two entries was silently dropped")
	}
}

func TestLoad_ReadsTheImagesMap(t *testing.T) {
	cfg, err := Load(write(t, "images:\n  sm_89: ember:lab-sm89\n  sm_90: ember:lab-sm90\n"))
	if err != nil {
		t.Fatalf("Load = %v", err)
	}
	if len(cfg.Images) != 2 || cfg.Images["sm_89"] != "ember:lab-sm89" {
		t.Errorf("images = %v", cfg.Images)
	}
}
