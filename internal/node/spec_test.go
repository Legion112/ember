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

	if s.Cmd[0] != "python" {
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
