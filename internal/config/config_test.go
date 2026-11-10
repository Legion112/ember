package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Legion112/ember/internal/node"
	"github.com/Legion112/ember/internal/sdserver"
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
image: ghcr.io/example/sd:v1
ttl: 30m
gpu: "NVIDIA GeForce RTX 4090"
min_vram_gib: 20
allow_interruptible: true
region: eu
max_hourly_usd: "0.50"
max_run_usd: "1.00"
outputs:
  - /workspace/out:renders
`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Provider != "runpod" || cfg.TTL != 30*time.Minute || len(cfg.Outputs) != 1 {
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

// TestBudgets_AreParsedAsDecimalStringsNotFloats. A YAML float for 2.01 is
// 2.0099999999999998, which times a million truncates to 2009999, and a
// budget wrong by a rounding error is one nobody
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

// TestBudgets_NameTheKeyAndWhatToWrite. ParseUSD's refusal cannot name the key
// -- it is shared by four -- so each caller wraps it with its own, and an
// operator reading `ember: config: max_month_usd: malformed price ...` needs
// both halves. One case per key, because a wrapper that forgot its prefix
// would be right for the other three.
func TestBudgets_NameTheKeyAndWhatToWrite(t *testing.T) {
	for _, tc := range []struct {
		key string
		cfg Config
		run func(Config) error
	}{
		{"max_hourly_usd", Config{MaxHourlyUSD: "$1,000"}, func(c Config) error { _, _, err := c.Budgets(); return err }},
		{"max_hourly_usd", Config{MaxHourlyUSD: "$1,000"}, func(c Config) error { _, err := c.Filter(); return err }},
		{"max_run_usd", Config{MaxRunUSD: "$1,000"}, func(c Config) error { _, _, err := c.Budgets(); return err }},
		{"max_day_usd", Config{MaxDayUSD: "$1,000"}, func(c Config) error { _, _, err := c.PeriodCeilings(); return err }},
		{"max_month_usd", Config{MaxMonthUSD: "$1,000"}, func(c Config) error { _, _, err := c.PeriodCeilings(); return err }},
	} {
		err := tc.run(tc.cfg)
		if err == nil {
			t.Errorf("%s: accepted", tc.key)
			continue
		}
		for _, want := range []string{tc.key + ": malformed price \"$1,000\"", "plain decimal number of dollars", "thousands separator"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("%s: error %q does not carry %q", tc.key, err, want)
			}
		}
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
// relative path would scatter node records and the queue database into
// whatever directory ember happened to be started from.
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

func TestParseSD_EmptyIsNotARun(t *testing.T) {
	run, err := Config{}.ParseSD()
	if err != nil {
		t.Fatal(err)
	}
	if len(run.Jobs) != 0 || run.Preview != "" {
		t.Errorf("run = %+v, want the zero value", run)
	}
}

// Each half is useless without the other, and both failures look like a run
// that worked: jobs with no server have nothing to send them to, and a server
// with no jobs rents a card, loads the weights and produces an empty output
// directory for the whole TTL.
func TestParseSD_RefusesHalfAServedRun(t *testing.T) {
	jobsOnly := Config{SD: SDConfig{Jobs: []SDJob{{Prompt: "a cat"}}}}
	err := mustFailSD(t, jobsOnly)
	if !strings.Contains(err.Error(), "no server to send them to") {
		t.Errorf("err = %v", err)
	}

	serveOnly := Config{SD: SDConfig{Serve: []string{"sd-server"}}}
	err = mustFailSD(t, serveOnly)
	if !strings.Contains(err.Error(), "generate nothing") {
		t.Errorf("err = %v", err)
	}
}

func TestParseSD_MapsEveryTypedField(t *testing.T) {
	cfg := Config{SD: SDConfig{
		Serve: []string{"sd-server", "--diffusion-model", "/models/x.gguf"},
		Jobs: []SDJob{{
			Prompt: "a cat", NegativePrompt: "blurry",
			Width: 1536, Height: 1024, Steps: 28, BatchCount: 2, Seed: 20261101,
			Sampler: "euler", Scheduler: "karras", CFG: 1, Guidance: 4,
			Output: "station", Extra: map[string]any{"output_format": "png"},
		}},
	}}
	run, err := cfg.ParseSD()
	if err != nil {
		t.Fatal(err)
	}
	if len(run.Jobs) != 1 {
		t.Fatalf("jobs = %d, want 1", len(run.Jobs))
	}
	j := run.Jobs[0]
	if j.Prompt != "a cat" || j.NegativePrompt != "blurry" {
		t.Errorf("prompts = %q / %q", j.Prompt, j.NegativePrompt)
	}
	if j.Width != 1536 || j.Height != 1024 || j.Steps != 28 || j.BatchCount != 2 {
		t.Errorf("geometry = %+v", j)
	}
	if j.Seed != 20261101 {
		t.Errorf("seed = %d", j.Seed)
	}
	if j.Sampler != "euler" || j.Scheduler != "karras" {
		t.Errorf("sampler/scheduler = %q / %q", j.Sampler, j.Scheduler)
	}
	// Two different numbers that both get called "cfg": conflating them
	// generates at the wrong one.
	if j.CFG != 1 || j.Guidance != 4 {
		t.Errorf("cfg = %v, guidance = %v", j.CFG, j.Guidance)
	}
	if j.Output != "station" {
		t.Errorf("output = %q", j.Output)
	}
	if j.Extra["output_format"] != "png" {
		t.Errorf("extra = %v", j.Extra)
	}
	// ember's defaults, not upstream's frame-every-step.
	if run.Preview != sdserver.DefaultPreview || run.PreviewInterval != sdserver.DefaultPreviewInterval {
		t.Errorf("preview = %q every %d, want the ember defaults", run.Preview, run.PreviewInterval)
	}
}

// Every job, not just the first, and the key path says which. A config with
// nine good jobs and a bad tenth would otherwise sit committed until somebody
// waits an hour for the tenth.
func TestParseSD_ValidatesEveryJobAndNamesTheIndex(t *testing.T) {
	for _, tc := range []struct {
		name string
		job  SDJob
		want string
	}{
		{"extra shadows a typed field", SDJob{Prompt: "a", Seed: 1,
			Extra: map[string]any{"seed": 2}}, "sd.jobs[2]: extra sets seed"},
		{"extra shadows the preview ember owns", SDJob{Prompt: "a",
			Extra: map[string]any{"preview": "vae"}}, "sd.jobs[2]: extra sets preview"},
		{"an unusable output name", SDJob{Prompt: "a", Output: ".."}, `sd.jobs[2]: output ".."`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Config{SD: SDConfig{
				Serve: []string{"sd-server"},
				Jobs:  []SDJob{{Prompt: "ok"}, {Prompt: "also ok"}, tc.job},
			}}
			err := mustFailSD(t, cfg)
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want it to contain %q", err, tc.want)
			}
		})
	}
}

func TestParseSD_RefusesABadPreviewSetting(t *testing.T) {
	cfg := Config{SD: SDConfig{
		Serve: []string{"sd-server"}, Jobs: []SDJob{{Prompt: "a"}},
		Preview: "full",
	}}
	if err := mustFailSD(t, cfg); !strings.Contains(err.Error(), "sd.preview:") {
		t.Errorf("err = %v, want the key path", err)
	}

	cfg2 := Config{SD: SDConfig{
		Serve: []string{"sd-server"}, Jobs: []SDJob{{Prompt: "a"}},
		PreviewInterval: -1,
	}}
	if err := mustFailSD(t, cfg2); !strings.Contains(err.Error(), "sd.preview_interval:") {
		t.Errorf("err = %v, want the key path", err)
	}
}

// The `sd:` block is subject to KnownFields like everything else: a misspelled
// key inside it is a typo, not a comment.
func TestLoad_RefusesAMisspelledKeyInsideTheSDBlock(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ember.yaml")
	body := "sd:\n  serve: [sd-server]\n  jobs:\n    - prompt: a cat\n      promt: a dog\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("a misspelled key inside sd.jobs was accepted as if it were a comment")
	}
}

func mustFailSD(t *testing.T, cfg Config) error {
	t.Helper()
	_, err := cfg.ParseSD()
	if err == nil {
		t.Fatal("want a refusal")
	}
	return err
}

// TestExampleConfigs_LoadAndParse reads every configuration this repository
// commits.
//
// KnownFields(true) means a misspelled key is a hard failure, so a committed
// example with a typo in it is an example that cannot be run -- and the
// person who finds out is whoever copies it. These are hand-written and long,
// which is exactly the size where a key gets misremembered.
//
// WHAT THIS RULE NARROWS, written beside it rather than left to be
// discovered: "every ember configuration this repository commits" is the
// sentence, and "every ember*.yaml at the repository root" is the code. The
// difference is a configuration written inside a document -- README.md holds
// yaml fences of its own, and so may docs/ -- and none of those is read here.
// README's are partial by design, a two-line illustration is not a
// configuration, and deciding which of them are would be the hand-maintained
// list this guard exists not to be. The glob is the whole of the pattern, so
// there is nothing left in it to narrow by accident except the ember prefix,
// which the floor below reports.
func TestExampleConfigs_LoadAndParse(t *testing.T) {
	for _, p := range committedConfigs(t, filepath.Join("..", "..")) {
		t.Run(filepath.Base(p), func(t *testing.T) {
			cfg, err := Load(p)
			if err != nil {
				t.Fatalf("%s does not load: %v", filepath.Base(p), err)
			}
			if _, err := cfg.ParseSD(); err != nil {
				t.Errorf("sd: block does not parse: %v", err)
			}
			if _, err := cfg.ParseTTS(); err != nil {
				t.Errorf("tts: block does not parse: %v", err)
			}
			if _, err := cfg.ParseOutputs(); err != nil {
				t.Errorf("outputs: does not parse: %v", err)
			}
			if _, err := cfg.ParseImages(); err != nil {
				t.Errorf("images: does not parse: %v", err)
			}
			if _, err := cfg.Filter(); err != nil {
				t.Errorf("the offer filter does not build: %v", err)
			}
			if _, _, err := cfg.Budgets(); err != nil {
				t.Errorf("the budgets do not parse: %v", err)
			}
		})
	}
}

// committedConfigs is every configuration this repository commits: the
// ember*.yaml files at the root.
//
// THE FLOOR IS OVER THE ANSWER AND NOT OVER AN INTERMEDIATE, which is the
// whole care in this function. Load() answers Default() and a NIL error for a
// file that is not there, so a source that contributed nothing would pass
// every assertion in the caller -- and a floor on a local slice that is then
// not returned is the same hole one line further down. So the count is taken
// over the list that is handed back.
func committedConfigs(t *testing.T, root string) []string {
	t.Helper()

	all, err := filepath.Glob(filepath.Join(root, "ember*.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	// THE FLOOR IS ONE AND IT WAS THREE, and the number is not the point: this
	// guard exists so a glob that matched NOTHING cannot make the test vacuous,
	// not to mandate how many files there are. There is one committed
	// configuration now -- the four collapsed into one file with a `workloads:`
	// entry each, because they differed in what a workload is rather than in
	// what an operator wants -- so three would fail over the thing that was
	// done on purpose while still not catching a broken glob any better than
	// one does.
	if len(all) < 1 {
		t.Fatalf("%d configuration(s) matched ember*.yaml at the repository root, "+
			"want at least 1; this test is checking nothing if that glob is wrong "+
			"or its result is not read", len(all))
	}
	return all
}

// TestLoad_RefusesADuplicateKey pins a behaviour of the YAML library rather
// than of ember.
//
// KnownFields(true) covers a key that is not a field. It says nothing about
// the same field twice, which is a different mistake and an easy one to make
// in a long `sd.jobs` list -- two `steps:` lines in one job, where the second
// silently wins and the run measures something the config does not say.
//
// gopkg.in/yaml.v3 refuses it, naming both lines. That is relied on here and
// so it is asserted here: a dependency behaviour nothing checks is a
// dependency behaviour that can change in a patch release. Found by making
// the mistake in a real config, not by reading the library.
func TestLoad_RefusesADuplicateKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ember.yaml")
	body := "sd:\n  serve: [sd-server]\n  jobs:\n    - prompt: a cat\n      steps: 4\n      steps: 8\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	if err == nil {
		t.Fatal("the same key twice was accepted; the second value would silently win")
	}
	if !strings.Contains(err.Error(), "already defined") {
		t.Errorf("err = %v, want it to name the duplicate", err)
	}
}

// The optional parameters parse from a config and reach the client's type.
//
// Both the flat keys and the three nested blocks, because the nesting is where
// a yaml tag typo hides: a misspelled key on a nested struct is not an error,
// it is a block that silently stays at its zero value -- and the generation
// then runs at the server's defaults while the config says otherwise.
func TestParseSD_ReadsTheOptionalParameters(t *testing.T) {
	cfg, err := Load(write(t, `
provider: local
image: ember-sd:1
ttl: 10m
sd:
  serve: [sd-server, --model, /models/sd15.gguf]
  jobs:
    - prompt: a cat
      output: cat
      clip_skip: 2
      eta: 0
      flow_shift: 3.5
      shifted_timestep: 4
      output_format: webp
      output_compression: 90
      slg:
        layers: [7, 8, 9]
        layer_start: 0.01
        layer_end: 0.2
        scale: 2.5
      hires:
        enabled: true
        upscaler: Latent
        scale: 2
        steps: 10
        denoising_strength: 0.7
        target_width: 1024
        target_height: 768
        upscale_tile_size: 128
      vae_tiling:
        enabled: true
        tile_size_w: 512
        tile_size_h: 256
        target_overlap: 0.5
        rel_size_w: 0.25
        rel_size_h: 0.125
`))
	if err != nil {
		t.Fatal(err)
	}
	run, err := cfg.ParseSD()
	if err != nil {
		t.Fatal(err)
	}
	p := run.Jobs[0].Params
	if p.ClipSkip == nil || *p.ClipSkip != 2 {
		t.Errorf("clip_skip = %v, want 2", p.ClipSkip)
	}
	// `eta: 0` is a value and not an absent key, which is the whole reason
	// these three are pointers.
	if p.Eta == nil || *p.Eta != 0 {
		t.Errorf("eta = %v, want a pointer to 0", p.Eta)
	}
	if p.FlowShift == nil || *p.FlowShift != 3.5 {
		t.Errorf("flow_shift = %v, want 3.5", p.FlowShift)
	}
	if p.ShiftedTimestep != 4 || p.OutputFormat != "webp" || p.OutputCompression != 90 {
		t.Errorf("scalars = %+v", p)
	}
	if p.SLG.Scale != 2.5 || p.SLG.LayerStart != 0.01 || p.SLG.LayerEnd != 0.2 ||
		len(p.SLG.Layers) != 3 || p.SLG.Layers[0] != 7 {
		t.Errorf("slg = %+v", p.SLG)
	}
	if !p.Hires.Enabled || p.Hires.Upscaler != "Latent" || p.Hires.Scale != 2 ||
		p.Hires.Steps != 10 || p.Hires.DenoisingStrength != 0.7 ||
		p.Hires.TargetWidth != 1024 || p.Hires.TargetHeight != 768 ||
		p.Hires.UpscaleTileSize != 128 {
		t.Errorf("hires = %+v", p.Hires)
	}
	if !p.VAETiling.Enabled || p.VAETiling.TileSizeW != 512 || p.VAETiling.TileSizeH != 256 ||
		p.VAETiling.TargetOverlap != 0.5 || p.VAETiling.RelSizeW != 0.25 ||
		p.VAETiling.RelSizeH != 0.125 {
		t.Errorf("vae tiling = %+v", p.VAETiling)
	}

	// An absent key is absent, not zero: a config that says nothing about
	// clip_skip must not tell the model to use 0.
	plain, err := Load(write(t, `
provider: local
image: ember-sd:1
ttl: 10m
sd:
  serve: [sd-server, --model, /models/sd15.gguf]
  jobs:
    - prompt: a cat
`))
	if err != nil {
		t.Fatal(err)
	}
	run, err = plain.ParseSD()
	if err != nil {
		t.Fatal(err)
	}
	if run.Jobs[0].Params.ClipSkip != nil || run.Jobs[0].Params.Eta != nil {
		t.Errorf("params = %+v, want the pointers nil", run.Jobs[0].Params)
	}
}

// A parameter outside its range is refused when the config is read, which is
// before anything is rented.
func TestParseSD_RefusesAParameterOutsideItsRange(t *testing.T) {
	_, err := func() (any, error) {
		cfg, err := Load(write(t, `
provider: local
image: ember-sd:1
ttl: 10m
sd:
  serve: [sd-server, --model, /models/sd15.gguf]
  jobs:
    - prompt: a cat
      hires:
        enabled: true
        denoising_strength: 2.0
`))
		if err != nil {
			return nil, err
		}
		return cfg.ParseSD()
	}()
	if err == nil {
		t.Fatal("a denoising strength of 2 was accepted")
	}
	if !strings.Contains(err.Error(), "denoising_strength") || !strings.Contains(err.Error(), "sd.jobs[0]") {
		t.Errorf("err = %v, want it to name the field and the job", err)
	}
}

// TestKeepEvents_OffByDefaultAndRefusesANegativeHorizon.
//
// Off by default is the retention decision: nothing in ember deletes an
// operator's data as a side effect, so the absent key means keep every event
// forever rather than keep them for some built-in number of days.
//
// A negative value is refused rather than clamped, as `ember store abort`
// refuses a negative --older-than. It reads as "no bound", and a horizon of
// "finished before now PLUS a month" selects every request that has ever
// ended -- while clamping it to zero is the other defensible guess and would
// silently mean "keep everything" to somebody who wrote -720h meaning the
// opposite.
func TestKeepEvents_OffByDefaultAndRefusesANegativeHorizon(t *testing.T) {
	bare, err := Load(write(t, "provider: local\nimage: ember-sd:1\nttl: 10m\n"))
	if err != nil {
		t.Fatal(err)
	}
	keep, err := bare.KeepEvents()
	if err != nil || keep != 0 {
		t.Errorf("KeepEvents() = %v, %v; an absent history block keeps everything", keep, err)
	}

	set, err := Load(write(t, "provider: local\nimage: ember-sd:1\nttl: 10m\nhistory:\n  keep_events: 720h\n"))
	if err != nil {
		t.Fatal(err)
	}
	if keep, err = set.KeepEvents(); err != nil || keep != 720*time.Hour {
		t.Errorf("KeepEvents() = %v, %v; want 720h", keep, err)
	}

	bad, err := Load(write(t, "provider: local\nimage: ember-sd:1\nttl: 10m\nhistory:\n  keep_events: -720h\n"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bad.KeepEvents(); err == nil {
		t.Fatal("a negative horizon was accepted")
	} else if !strings.Contains(err.Error(), "keep_events") {
		t.Errorf("err = %v, want it to name the key", err)
	}
}

// TestLoad_RefusesMinCUDAByNameRatherThanAsAnUnknownField is #91's
// operator-facing half.
//
// `min_cuda` was a documented config key whose only effect was a refusal on
// RunPod and SILENCE on the local provider, which is the defect the issue is
// about. Removing the field answers the silence by itself, because
// KnownFields(true) refuses a key that is not a field -- but what yaml.v3 then
// says is "field min_cuda not found in type config.Config", which names a Go
// type an operator should never see and offers no remedy. The key was in
// README for its whole life, so the person who hits this is the one who
// followed the documentation, and `gpu_probe` is what they wanted.
//
// So the assertions are the two halves an unknown-field error cannot give: the
// key by name, and the remedy by name. The negative control beside them is a
// config with no such line, because a refusal that fired on every file would
// satisfy both.
func TestLoad_RefusesMinCUDAByNameRatherThanAsAnUnknownField(t *testing.T) {
	_, err := Load(write(t, "provider: local\nimage: ember-sd:1\nttl: 10m\nmin_cuda: \"12.6\"\n"))
	if err == nil {
		t.Fatal("min_cuda was accepted; no provider here can honour it")
	}
	for _, want := range []string{"min_cuda", "gpu_probe"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not name %q: %v", want, err)
		}
	}
	// The thing that distinguishes ember's answer from yaml's. A guard that
	// only checked for "min_cuda" would pass against the generic message,
	// which contains the key too.
	if strings.Contains(err.Error(), "config.Config") {
		t.Errorf("the refusal is yaml's unknown-field message rather than ember's: %v", err)
	}

	if _, err := Load(write(t, "provider: local\nimage: ember-sd:1\nttl: 10m\n")); err != nil {
		t.Fatalf("a config that never named min_cuda was refused: %v", err)
	}
}

// TestLoad_RefusesAMinCUDAWithNoValue is the spelling a struct of pointers
// cannot see.
//
// `min_cuda:` with nothing after it decodes as null, so a check written as
// `*string != nil` reads it as absent -- and it is exactly what an operator
// reaches for when they want the key off without deleting the line. The
// question rejectRemovedKeys asks is whether the key was WRITTEN, which is why
// it decodes into a map and looks for the key rather than for a value.
//
// IT ASSERTS THE MESSAGE AND NOT MERELY AN ERROR, and the first version of
// this test did the second thing and could not fail. KnownFields(true) refuses
// a key that is not a field whatever rejectRemovedKeys concluded, so `err !=
// nil` holds against the pointer defect this test exists for as happily as
// against the fix -- "what would this say if the bug were present?", answered
// with "the same thing". What tells the two apart is whose sentence it is:
// ember's names gpu_probe, yaml's names the Go type.
func TestLoad_RefusesAMinCUDAWithNoValue(t *testing.T) {
	for _, body := range []string{
		"provider: local\nimage: ember-sd:1\nttl: 10m\nmin_cuda:\n",
		"provider: local\nimage: ember-sd:1\nttl: 10m\nmin_cuda: \"\"\n",
	} {
		_, err := Load(write(t, body))
		if err == nil {
			t.Errorf("a min_cuda line with no value was accepted:\n%s", body)
			continue
		}
		if !strings.Contains(err.Error(), "gpu_probe") {
			t.Errorf("a min_cuda line with no value got yaml's unknown-field error "+
				"rather than ember's remedy: %v\n%s", err, body)
		}
	}
}

// TestLoad_RefusesTheWorkloadKeysByNameRatherThanAsUnknownFields is the
// operator-facing half of #203.
//
// ember runs AI model jobs, so a config describes them with `sd:` or `tts:`
// and a node publishes nothing but sshd. Deleting the three keys that said
// otherwise answers the behaviour by itself, because KnownFields(true) refuses
// a key that is not a field -- but what yaml.v3 then says is "field cmds not
// found in type config.Config", which names a Go type an operator should never
// see and offers no remedy. All three were in README for their whole life, so
// the person who hits this is the one who followed the documentation.
//
// THE REMEDY IS ASSERTED AND NOT ONLY THE KEY, which is what distinguishes
// ember's sentence from yaml's: the generic message contains the key too, so a
// check for the key alone passes against the state this exists to replace.
// Each key's own remedy is a separate want, because one wording for all three
// would send an operator whose `ports:` line is the problem looking for a
// block to write.
func TestLoad_RefusesTheWorkloadKeysByNameRatherThanAsUnknownFields(t *testing.T) {
	base := "provider: local\nimage: ember-sd:1\nttl: 10m\n"
	for _, tc := range []struct {
		key  string
		body string
		want []string
	}{
		{"cmd", "cmd: [sd-cli, -p, a cat]\n", []string{"cmd", "sd:", "tts:"}},
		{"cmds", "cmds:\n  - [sd-cli, -p, one]\n", []string{"cmds", "sd:", "tts:"}},
		{"ports", "ports:\n  - sshd:22:18022\n", []string{"ports", "outputs:"}},
	} {
		t.Run(tc.key, func(t *testing.T) {
			_, err := Load(write(t, base+tc.body))
			if err == nil {
				t.Fatalf("%s was accepted", tc.key)
			}
			for _, want := range tc.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the refusal does not name %q: %v", want, err)
				}
			}
			if strings.Contains(err.Error(), "config.Config") {
				t.Errorf("the refusal is yaml's unknown-field message rather than ember's: %v", err)
			}
		})
	}

	// The negative control: a refusal that fired on every file would satisfy
	// every assertion above.
	if _, err := Load(write(t, base)); err != nil {
		t.Fatalf("a config that named none of them was refused: %v", err)
	}
}

// TestLoad_RefusesAWorkloadKeyWithNoValue is the spelling a struct of pointers
// cannot see, for the three keys #203 removed.
//
// `cmds:` with nothing after it decodes as null, so a check written as
// `*string != nil` reads it as absent -- and it is exactly what an operator
// reaches for when they want a key off without deleting the line. An empty
// sequence is the other one, and `len(...) > 0` reads that as absent too.
//
// IT ASSERTS THE MESSAGE AND NOT MERELY AN ERROR, because KnownFields(true)
// refuses a key that is not a field whatever rejectRemovedKeys concluded: `err
// != nil` holds against the defect as happily as against the fix. What tells
// the two apart is whose sentence it is.
func TestLoad_RefusesAWorkloadKeyWithNoValue(t *testing.T) {
	base := "provider: local\nimage: ember-sd:1\nttl: 10m\n"
	for _, tc := range []struct {
		body string
		want string
	}{
		{"cmd:\n", "sd:"},
		{"cmd: []\n", "sd:"},
		{"cmds:\n", "sd:"},
		{"cmds: []\n", "sd:"},
		{"ports:\n", "outputs:"},
		{"ports: []\n", "outputs:"},
	} {
		_, err := Load(write(t, base+tc.body))
		if err == nil {
			t.Errorf("a key with no value was accepted:\n%s", tc.body)
			continue
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%q got yaml's unknown-field error rather than ember's remedy: %v",
				tc.body, err)
		}
	}
}

// TestLoad_NamesEveryRemovedKeyTheFileCarries.
//
// An operator who wrote two of them and is told about one edits the file, runs
// it again, and is told about the next -- the same work split across as many
// runs as they made mistakes. A loop that returned on the first hit would
// satisfy every other assertion in this file, because each of those writes one
// key.
func TestLoad_NamesEveryRemovedKeyTheFileCarries(t *testing.T) {
	_, err := Load(write(t, "provider: local\nimage: ember-sd:1\nttl: 10m\n"+
		"cmd: [true]\nports:\n  - sshd:22\nmin_cuda: \"12.6\"\n"))
	if err == nil {
		t.Fatal("three removed keys were accepted")
	}
	for _, want := range []string{"cmd ", "ports ", "min_cuda "} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal stops before %q: %v", want, err)
		}
	}
}

// TestLoad_AFileItCannotParseReportsTheParseAndNotARemovedKey.
//
// rejectRemovedKeys runs before the strict decode so that its sentence wins,
// and that puts it in front of every malformed file as well. A file it cannot
// read is one the decode cannot read either, and the decode's message names
// the line -- so this one must stay silent and let it through rather than
// describing the wrong problem.
func TestLoad_AFileItCannotParseReportsTheParseAndNotARemovedKey(t *testing.T) {
	// A sequence at the root: valid YAML, and not a mapping, so unmarshalling
	// it into a map fails where the strict decode into Config also fails.
	_, err := Load(write(t, "- provider: local\n- image: ember-sd:1\n"))
	if err == nil {
		t.Fatal("a root-level sequence was accepted as a config")
	}
	if !strings.Contains(err.Error(), "parse ") {
		t.Errorf("err = %v, want the parse error that names the line", err)
	}
	if strings.Contains(err.Error(), "min_cuda") {
		t.Errorf("a malformed file was reported as a removed key: %v", err)
	}
}
