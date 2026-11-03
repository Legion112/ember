package node

import (
	"strings"
	"testing"
	"time"
)

func validSpecOptions() SpecOptions {
	return SpecOptions{
		Image:   "ghcr.io/example/comfyui:v0.3.1",
		Ports:   []PortMap{{Name: "comfyui", Remote: 8188, Local: 8188}},
		Outputs: []OutputPath{{Remote: "/workspace/output", Local: "output"}},
		TTL:     time.Hour,
	}
}

// TestNewSpec_RefusesAnUnpinnedImage: the point of ember is to measure what a
// model costs and how long it takes. Two runs a week apart that both say
// ":latest" may not have run the same software, and a result nobody can
// reproduce is not a measurement.
func TestNewSpec_RefusesAnUnpinnedImage(t *testing.T) {
	for _, img := range []string{"", "   ", "comfyui", "comfyui:latest", "ghcr.io/e/comfyui:latest", "comfyui:", ":v1"} {
		o := validSpecOptions()
		o.Image = img
		if _, err := NewSpec(o); err == nil {
			t.Errorf("NewSpec accepted image %q", img)
		}
	}
}

func TestNewSpec_AcceptsTagsAndDigests(t *testing.T) {
	for _, img := range []string{
		"comfyui:v1",
		"ghcr.io/example/comfyui:v0.3.1",
		"example/comfyui@sha256:0123456789abcdef",
		// A registry host may carry a port, and that colon is not a tag.
		"localhost:5000/comfyui:v2",
	} {
		o := validSpecOptions()
		o.Image = img
		if _, err := NewSpec(o); err != nil {
			t.Errorf("NewSpec refused image %q: %v", img, err)
		}
	}
}

// TestNewSpec_TTLIsMandatory: there is no exported path to a Spec with no
// deadline, because a node with no deadline is a node nothing will stop.
func TestNewSpec_TTLIsMandatory(t *testing.T) {
	for _, ttl := range []time.Duration{0, -time.Second, MaxTTL + time.Second, 240 * time.Hour} {
		o := validSpecOptions()
		o.TTL = ttl
		if _, err := NewSpec(o); err == nil {
			t.Errorf("NewSpec accepted ttl %v", ttl)
		}
	}
	o := validSpecOptions()
	o.TTL = MaxTTL
	if _, err := NewSpec(o); err != nil {
		t.Errorf("NewSpec refused the ceiling itself: %v", err)
	}
}

func TestNewSpec_RefusesBadPorts(t *testing.T) {
	for name, ports := range map[string][]PortMap{
		"no remote":  {{Name: "a", Remote: 0, Local: 1}},
		"no local":   {{Name: "a", Remote: 1, Local: 0}},
		"no name":    {{Name: " ", Remote: 1, Local: 1}},
		"dup remote": {{Name: "a", Remote: 1, Local: 1}, {Name: "b", Remote: 1, Local: 2}},
		"dup local":  {{Name: "a", Remote: 1, Local: 1}, {Name: "b", Remote: 2, Local: 1}},
		"dup name":   {{Name: "a", Remote: 1, Local: 1}, {Name: "a", Remote: 2, Local: 2}},
	} {
		o := validSpecOptions()
		o.Ports = ports
		if _, err := NewSpec(o); err == nil {
			t.Errorf("%s: NewSpec accepted it", name)
		}
	}
}

func TestNewSpec_RefusesBadOutputs(t *testing.T) {
	for name, out := range map[string]OutputPath{
		"relative remote": {Remote: "workspace/out", Local: "o"},
		"unclean remote":  {Remote: "/workspace/../out", Local: "o"},
		"trailing slash":  {Remote: "/workspace/out/", Local: "o"},
		"no local":        {Remote: "/workspace/out", Local: ""},
		"absolute local":  {Remote: "/workspace/out", Local: "/etc"},
		"escaping local":  {Remote: "/workspace/out", Local: "../elsewhere"},
	} {
		o := validSpecOptions()
		o.Outputs = []OutputPath{out}
		if _, err := NewSpec(o); err == nil {
			t.Errorf("%s: NewSpec accepted it", name)
		}
	}
}

func TestNewSpec_RefusesNegativeVolumeAndBadFilter(t *testing.T) {
	o := validSpecOptions()
	o.Volume = -1
	if _, err := NewSpec(o); err == nil {
		t.Error("NewSpec accepted a negative volume")
	}
	o = validSpecOptions()
	o.Filter = Filter{MaxPrice: -1}
	if _, err := NewSpec(o); err == nil {
		t.Error("NewSpec accepted an invalid filter")
	}
}

// TestNewSpec_CopiesItsInputs: a Spec that aliased the caller's slices would
// let a later mutation change what a running node was asked to do.
func TestNewSpec_CopiesItsInputs(t *testing.T) {
	o := validSpecOptions()
	o.Cmd = []string{"python", "main.py"}
	o.Env = map[string]string{"A": "1"}

	s, err := NewSpec(o)
	if err != nil {
		t.Fatal(err)
	}
	o.Cmd[0] = "rm"
	o.Env["A"] = "2"
	o.Ports[0].Local = 9999

	if s.Cmds[0][0] != "python" {
		t.Error("Cmd aliased the caller's slice")
	}
	if s.Env["A"] != "1" {
		t.Error("Env aliased the caller's map")
	}
	if s.Ports[0].Local != 8188 {
		t.Error("Ports aliased the caller's slice")
	}
}

func TestNewSpec_NilEnvStaysNil(t *testing.T) {
	s, err := NewSpec(validSpecOptions())
	if err != nil {
		t.Fatal(err)
	}
	if s.Env != nil {
		t.Error("a nil Env should not become an empty map")
	}
}

func TestNewSpec_ErrorsNameTheField(t *testing.T) {
	o := validSpecOptions()
	o.Image = "comfyui"
	_, err := NewSpec(o)
	if err == nil || !strings.Contains(err.Error(), "no tag") {
		t.Errorf("error should explain the pin requirement, got %v", err)
	}
}

func TestNewSpec_RefusesHalfAMount(t *testing.T) {
	// Half a mount is not a smaller mount, it is a silent one. A store with
	// nowhere to land would be attached and unreachable; a mount with no
	// store would be a path the workload reads and finds empty. Both look
	// like a run that succeeded and produced nothing.
	mount, err := MountAt("/models")
	if err != nil {
		t.Fatal(err)
	}
	handle := StoreHandle{Provider: "local", ID: "ember-store-models"}

	base := SpecOptions{Image: "ghcr.io/x/y:v1", TTL: time.Hour}

	storeOnly := base
	storeOnly.Store = handle
	if _, err := NewSpec(storeOnly); err == nil {
		t.Error("a store with no mount path was accepted")
	}

	mountOnly := base
	mountOnly.Mount = mount
	if _, err := NewSpec(mountOnly); err == nil {
		t.Error("a mount path with no store was accepted")
	}

	both := base
	both.Store, both.Mount = handle, mount
	s, err := NewSpec(both)
	if err != nil {
		t.Fatalf("a complete mount was refused: %v", err)
	}
	if s.Store != handle || s.Mount != mount {
		t.Errorf("the mount did not survive NewSpec: %+v / %+v", s.Store, s.Mount)
	}

	// Neither is the shape every run had before stores existed.
	if _, err := NewSpec(base); err != nil {
		t.Errorf("a spec with no store was refused: %v", err)
	}
}

func TestNewSpec_RefusesAssetsWithNowhereToPutThem(t *testing.T) {
	// A fetch with no store writes gigabytes to the node's own disk and
	// throws them away with it: a run that downloads a model, discards it,
	// and reports success.
	a := Asset{URL: "https://h/x", SHA256: strings.Repeat("a", 64), As: "sd15.gguf"}
	base := SpecOptions{Image: "ghcr.io/x/y:v1", TTL: time.Hour}

	noStore := base
	noStore.Assets = []Asset{a}
	if _, err := NewSpec(noStore); err == nil {
		t.Error("assets with no store were accepted")
	}

	// And the asset list itself is validated here, so a bad one fails before
	// anything is rented.
	mount, err := MountAt("/models")
	if err != nil {
		t.Fatal(err)
	}
	withStore := base
	withStore.Store = StoreHandle{Provider: "local", ID: "v1"}
	withStore.Mount = mount
	withStore.Assets = []Asset{{URL: "https://h/x", SHA256: "nope", As: "a.gguf"}}
	if _, err := NewSpec(withStore); err == nil {
		t.Error("an asset with a bad digest was accepted")
	}

	ok := withStore
	ok.Assets = []Asset{a}
	s, err := NewSpec(ok)
	if err != nil {
		t.Fatalf("a valid asset was refused: %v", err)
	}
	if len(s.Assets) != 1 {
		t.Errorf("assets = %+v", s.Assets)
	}
}

func TestNewSpec_RefusesANegativeProbeTimeout(t *testing.T) {
	o := validSpecOptions()
	o.GPUProbeTimeout = -time.Second
	if _, err := NewSpec(o); err == nil {
		t.Error("a negative gpu probe timeout was accepted")
	}
}

// TestNewSpec_RefusesAProbeOnACPURun: a CPU-only run has no card, so a GPU
// probe against one can only ever fail -- and it would fail after the machine
// was rented, which is the exact shape of late discovery the probe exists to
// prevent.
func TestNewSpec_RefusesAProbeOnACPURun(t *testing.T) {
	o := validSpecOptions()
	o.Filter.CPUOnly = true
	o.GPUProbe = []string{"sh", "-c", "nvidia-smi"}
	if _, err := NewSpec(o); err == nil {
		t.Error("a gpu probe was accepted on a run that asks for no GPU")
	}
}

func TestNewSpec_CopiesTheProbe(t *testing.T) {
	o := validSpecOptions()
	o.GPUProbe = []string{"sh", "-c", "true"}
	o.GPUProbeTimeout = 90 * time.Second

	s, err := NewSpec(o)
	if err != nil {
		t.Fatal(err)
	}
	if s.GPUProbeTimeout != 90*time.Second {
		t.Errorf("GPUProbeTimeout = %v", s.GPUProbeTimeout)
	}
	// Copied, not aliased: a caller mutating its slice afterwards must not
	// change what the spec says will be run on the node.
	o.GPUProbe[2] = "false"
	if s.GPUProbe[2] != "true" {
		t.Error("the spec aliases the caller's probe slice")
	}
}

func TestParseCUDAVersion(t *testing.T) {
	for _, tc := range []struct {
		in           string
		major, minor int
		wantErr      bool
	}{
		{in: "12.6", major: 12, minor: 6},
		{in: "13.0", major: 13, minor: 0},
		// Not a float: 12.10 is newer than 12.9, and a float would say the
		// opposite. This is the case the string parsing exists for.
		{in: "12.10", major: 12, minor: 10},
		{in: " 12.6 ", major: 12, minor: 6},
		{in: "12", wantErr: true},
		{in: "", wantErr: true},
		{in: "twelve.six", wantErr: true},
		{in: "12.x", wantErr: true},
		{in: "-1.0", wantErr: true},
		{in: "12.-1", wantErr: true},
	} {
		maj, min, err := ParseCUDAVersion(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("ParseCUDAVersion(%q) accepted a bad version", tc.in)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseCUDAVersion(%q) = %v", tc.in, err)
			continue
		}
		if maj != tc.major || min != tc.minor {
			t.Errorf("ParseCUDAVersion(%q) = %d.%d, want %d.%d", tc.in, maj, min, tc.major, tc.minor)
		}
	}
}

func TestCUDAAtLeast(t *testing.T) {
	for _, tc := range []struct {
		have, want string
		ok         bool
	}{
		{"12.6", "12.6", true},
		{"12.9", "12.6", true},
		{"13.0", "12.9", true},
		{"12.5", "12.6", false},
		{"11.8", "12.0", false},
		// The comparison that a float would get wrong.
		{"12.10", "12.9", true},
		{"12.9", "12.10", false},
	} {
		got, err := CUDAAtLeast(tc.have, tc.want)
		if err != nil {
			t.Errorf("CUDAAtLeast(%q, %q) = %v", tc.have, tc.want, err)
			continue
		}
		if got != tc.ok {
			t.Errorf("CUDAAtLeast(%q, %q) = %v, want %v", tc.have, tc.want, got, tc.ok)
		}
	}

	if _, err := CUDAAtLeast("nonsense", "12.6"); err == nil {
		t.Error("an unreadable 'have' was accepted")
	}
	if _, err := CUDAAtLeast("12.6", "nonsense"); err == nil {
		t.Error("an unreadable 'want' was accepted")
	}
}

// TestNewSpec_CmdAndCmdsAreTheSameThing: one command is a batch of one, so
// everything downstream reads Cmds and nothing has to ask which spelling the
// operator used.
func TestNewSpec_CmdAndCmdsAreTheSameThing(t *testing.T) {
	o := validSpecOptions()
	o.Cmd = []string{"sd-cli", "-p", "a cat"}
	s, err := NewSpec(o)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Cmds) != 1 || s.Cmds[0][2] != "a cat" {
		t.Fatalf("Cmds = %v", s.Cmds)
	}

	o2 := validSpecOptions()
	o2.Cmd = nil
	o2.Cmds = [][]string{{"sd-cli", "-p", "one"}, {"sd-cli", "-p", "two"}}
	s2, err := NewSpec(o2)
	if err != nil {
		t.Fatal(err)
	}
	if len(s2.Cmds) != 2 || s2.Cmds[1][2] != "two" {
		t.Fatalf("Cmds = %v", s2.Cmds)
	}

	// Copied, not aliased: a caller mutating its slice afterwards must not
	// change what the node will be asked to run.
	o2.Cmds[0][2] = "mutated"
	if s2.Cmds[0][2] != "one" {
		t.Error("the spec aliases the caller's command slices")
	}
}

// TestNewSpec_RefusesBothSpellings. A config answering "what does this node
// run" twice is one where an operator watches the wrong thing execute.
func TestNewSpec_RefusesBothSpellings(t *testing.T) {
	o := validSpecOptions()
	o.Cmd = []string{"a"}
	o.Cmds = [][]string{{"b"}}
	if _, err := NewSpec(o); err == nil {
		t.Error("a spec with both cmd and cmds was accepted")
	}
}

func TestNewSpec_RefusesAnEmptyStep(t *testing.T) {
	o := validSpecOptions()
	o.Cmd = nil
	o.Cmds = [][]string{{"sd-cli"}, {}}
	if _, err := NewSpec(o); err == nil {
		t.Error("a batch containing an empty command was accepted")
	}
}

// A serve command is not a step: it is the thing the steps are sent to. A run
// with both would hold the weights in VRAM for a resident server and then run
// a separate sd-cli that loads them a second time -- paying for the resident
// copy and not using it.
func TestNewSpec_OneWorkloadSpellingNeverTwo(t *testing.T) {
	serve := []string{"sd-server", "--diffusion-model", "/models/x.gguf"}
	cmd := []string{"sd-cli", "-p", "a cat"}
	cmds := [][]string{{"sd-cli", "-p", "one"}}

	for _, tc := range []struct {
		name  string
		cmd   []string
		cmds  [][]string
		serve []string
		want  string
	}{
		{"cmd and cmds", cmd, cmds, nil, "cmd and cmds are set"},
		{"cmd and serve", cmd, nil, serve, "cmd and sd.serve are set"},
		{"cmds and serve", nil, cmds, serve, "cmds and sd.serve are set"},
		{"all three", cmd, cmds, serve, "cmd, cmds and sd.serve are set"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := validSpecOptions()
			o.Cmd, o.Cmds, o.Serve = tc.cmd, tc.cmds, tc.serve
			_, err := NewSpec(o)
			if err == nil {
				t.Fatal("want a refusal")
			}
			if got := err.Error(); !strings.Contains(got, tc.want) {
				t.Errorf("err = %q, want it to contain %q", got, tc.want)
			}
		})
	}
}

func TestNewSpec_ServeAloneIsCarriedThrough(t *testing.T) {
	o := validSpecOptions()
	o.Cmd = nil
	o.Serve = []string{"sd-server", "--diffusion-model", "/models/x.gguf"}
	o.ServeTimeout = 11 * time.Minute

	s, err := NewSpec(o)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Serve) != 3 || s.Serve[2] != "/models/x.gguf" {
		t.Fatalf("Serve = %v", s.Serve)
	}
	if s.ServeTimeout != 11*time.Minute {
		t.Errorf("ServeTimeout = %v", s.ServeTimeout)
	}
	// No Cmds: the jobs do not reach the node as argv, so there is nothing
	// for a step loop to find and run.
	if len(s.Cmds) != 0 {
		t.Errorf("Cmds = %v, want none for a served run", s.Cmds)
	}

	// Copied, not aliased, for the reason the command slices are.
	o.Serve[2] = "mutated"
	if s.Serve[2] != "/models/x.gguf" {
		t.Error("the spec aliases the caller's serve slice")
	}
}

// Zero is legal and means the package default, the same contract
// GPUProbeTimeout has. Negative is a config error.
func TestNewSpec_RefusesANegativeServeTimeout(t *testing.T) {
	o := validSpecOptions()
	o.ServeTimeout = -time.Second
	if _, err := NewSpec(o); err == nil {
		t.Fatal("want a refusal")
	}

	o2 := validSpecOptions()
	o2.ServeTimeout = 0
	if _, err := NewSpec(o2); err != nil {
		t.Errorf("a zero serve timeout must mean the default, got %v", err)
	}
}

func TestNewSpec_RefusesAnEmptyServeArgument(t *testing.T) {
	for _, argv := range [][]string{
		{"sd-server", ""},
		{"sd-server", "   "},
		{""},
	} {
		o := validSpecOptions()
		o.Cmd = nil
		o.Serve = argv
		if _, err := NewSpec(o); err == nil {
			t.Errorf("NewSpec accepted serve %q", argv)
		}
	}
}

// The same reasoning as the gpu probe on a CPU-only run: the only reason to
// hold weights resident is to generate with them, and this would fail after
// the machine was rented.
func TestNewSpec_RefusesAServerOnACPUOnlyRun(t *testing.T) {
	o := validSpecOptions()
	o.Cmd = nil
	o.GPUProbe = nil
	o.Serve = []string{"sd-server"}
	o.Filter = Filter{CPUOnly: true}

	_, err := NewSpec(o)
	if err == nil {
		t.Fatal("want a refusal")
	}
	if !strings.Contains(err.Error(), "no GPU") {
		t.Errorf("err = %v, want it to name the contradiction", err)
	}
}

func TestHumanList(t *testing.T) {
	for _, tc := range []struct {
		in   []string
		want string
	}{
		{nil, ""},
		{[]string{"a"}, "a"},
		{[]string{"a", "b"}, "a and b"},
		{[]string{"a", "b", "c"}, "a, b and c"},
	} {
		if got := humanList(tc.in); got != tc.want {
			t.Errorf("humanList(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
