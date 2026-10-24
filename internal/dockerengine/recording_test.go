package dockerengine

import (
	"errors"
	"strings"
	"testing"
)

// The fake daemon is the foundation every provider test stands on. A fake
// nobody tested is a fake that lies, and it lies in the direction of making
// the code under test look correct.

func TestRecordingEngine_LifecycleMirrorsARealDaemon(t *testing.T) {
	e := NewRecordingEngine()
	ctx := t.Context()

	if err := e.Ping(ctx); err != nil {
		t.Fatal(err)
	}

	ok, err := e.ImageExists(ctx, "ember:lab")
	if err != nil || ok {
		t.Fatalf("an unknown image should not exist: %v, %v", ok, err)
	}
	e.AddImage("ember:lab")
	if ok, _ := e.ImageExists(ctx, "ember:lab"); !ok {
		t.Fatal("AddImage did not take effect")
	}

	netID, err := e.NetworkCreate(ctx, "n", map[string]string{LabelRunID: "r1"})
	if err != nil {
		t.Fatal(err)
	}

	id, err := e.ContainerCreate(ctx, ContainerSpec{
		Name: "c", Image: "ember:lab", Network: "n",
		Labels:     map[string]string{LabelRunID: "r1"},
		PublishTCP: []uint16{22},
	})
	if err != nil {
		t.Fatal(err)
	}

	c, err := e.ContainerInspect(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if c.State != "created" {
		t.Errorf("a created container should not be running, got %q", c.State)
	}
	if c.Ports[22].Port == 0 {
		t.Error("the fake must publish a plausible host port, or nothing downstream can read one back")
	}

	if err := e.ContainerStart(ctx, id); err != nil {
		t.Fatal(err)
	}
	if c, _ := e.ContainerInspect(ctx, id); c.State != "running" {
		t.Errorf("state after start = %q", c.State)
	}

	if err := e.ContainerRemove(ctx, id); err != nil {
		t.Fatal(err)
	}
	if err := e.NetworkRemove(ctx, netID); err != nil {
		t.Fatal(err)
	}
	if len(e.Containers()) != 0 || len(e.Networks()) != 0 {
		t.Error("removal did not take effect")
	}
}

// TestRecordingEngine_MissingObjectsAreErrNotFound. The providers rely on this
// to make destroy idempotent, so a fake that returned a generic error would
// make every idempotency test pass for the wrong reason.
func TestRecordingEngine_MissingObjectsAreErrNotFound(t *testing.T) {
	e := NewRecordingEngine()
	ctx := t.Context()

	if _, err := e.ContainerInspect(ctx, "ghost"); !errors.Is(err, ErrNotFound) {
		t.Errorf("ContainerInspect = %v", err)
	}
	if err := e.ContainerRemove(ctx, "ghost"); !errors.Is(err, ErrNotFound) {
		t.Errorf("ContainerRemove = %v", err)
	}
	if err := e.ContainerStart(ctx, "ghost"); !errors.Is(err, ErrNotFound) {
		t.Errorf("ContainerStart = %v", err)
	}
	if err := e.NetworkRemove(ctx, "ghost"); !errors.Is(err, ErrNotFound) {
		t.Errorf("NetworkRemove = %v", err)
	}
	if _, err := e.Exec(ctx, "ghost", []string{"true"}); !errors.Is(err, ErrNotFound) {
		t.Errorf("Exec = %v", err)
	}
}

func TestRecordingEngine_LabelFiltersMatchDockerSemantics(t *testing.T) {
	e := NewRecordingEngine()
	ctx := t.Context()

	if _, err := e.NetworkCreate(ctx, "mine", map[string]string{LabelRunID: "r1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.NetworkCreate(ctx, "theirs", map[string]string{"other": "x"}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.ContainerCreate(ctx, ContainerSpec{Name: "c", Labels: map[string]string{LabelRunID: "r1"}}); err != nil {
		t.Fatal(err)
	}

	for name, tc := range map[string]struct {
		filter string
		want   int
	}{
		"key=value matches":  {LabelRunID + "=r1", 1},
		"key=value misses":   {LabelRunID + "=r2", 0},
		"bare key matches":   {LabelRunID, 1},
		"unknown key misses": {"nope", 0},
		"empty matches all":  {"", 2},
	} {
		nets, err := e.NetworkList(ctx, tc.filter)
		if err != nil {
			t.Fatal(err)
		}
		if len(nets) != tc.want {
			t.Errorf("%s: NetworkList(%q) returned %d, want %d", name, tc.filter, len(nets), tc.want)
		}
	}

	cs, err := e.ContainerList(ctx, LabelRunID+"=r1")
	if err != nil {
		t.Fatal(err)
	}
	if len(cs) != 1 {
		t.Errorf("ContainerList returned %d, want 1", len(cs))
	}
}

// TestRecordingEngine_FailOnAndErrors is the fault injection every teardown
// test depends on.
func TestRecordingEngine_FailOnAndErrors(t *testing.T) {
	ctx := t.Context()

	e := NewRecordingEngine()
	e.FailOn = "NetworkCreate"
	if _, err := e.NetworkCreate(ctx, "n", nil); err == nil {
		t.Error("FailOn did not fire")
	}

	e = NewRecordingEngine()
	want := errors.New("daemon exploded")
	e.Errors["Ping"] = want
	if err := e.Ping(ctx); !errors.Is(err, want) {
		t.Errorf("Errors did not fire: %v", err)
	}

	// Every operation must be refusable, or a teardown path goes untested.
	for _, op := range []string{
		"Ping", "ImageExists", "NetworkCreate", "NetworkRemove", "NetworkList",
		"ContainerCreate", "ContainerStart", "ContainerInspect", "ContainerRemove",
		"ContainerList", "Exec",
	} {
		e := NewRecordingEngine()
		e.FailOn = op
		if err := callOp(t, e, op); err == nil {
			t.Errorf("%s could not be made to fail", op)
		}
	}
}

func callOp(t *testing.T, e *RecordingEngine, op string) error {
	t.Helper()
	ctx := t.Context()
	switch op {
	case "Ping":
		return e.Ping(ctx)
	case "ImageExists":
		_, err := e.ImageExists(ctx, "x")
		return err
	case "NetworkCreate":
		_, err := e.NetworkCreate(ctx, "n", nil)
		return err
	case "NetworkRemove":
		return e.NetworkRemove(ctx, "id")
	case "NetworkList":
		_, err := e.NetworkList(ctx, "")
		return err
	case "ContainerCreate":
		_, err := e.ContainerCreate(ctx, ContainerSpec{Name: "c"})
		return err
	case "ContainerStart":
		return e.ContainerStart(ctx, "id")
	case "ContainerInspect":
		_, err := e.ContainerInspect(ctx, "id")
		return err
	case "ContainerRemove":
		return e.ContainerRemove(ctx, "id")
	case "ContainerList":
		_, err := e.ContainerList(ctx, "")
		return err
	case "Exec":
		_, err := e.Exec(ctx, "id", []string{"true"})
		return err
	}
	t.Fatalf("unknown op %q", op)
	return nil
}

func TestRecordingEngine_RefusesHostNetworkLikeTheRealOne(t *testing.T) {
	e := NewRecordingEngine()
	_, err := e.ContainerCreate(t.Context(), ContainerSpec{Name: "c", NetworkMode: "host"})
	if err == nil {
		t.Fatal("the fake allowed host networking; every guard test above it would pass for the wrong reason")
	}
}

func TestRecordingEngine_ExecAndState(t *testing.T) {
	e := NewRecordingEngine()
	ctx := t.Context()
	id, err := e.ContainerCreate(ctx, ContainerSpec{Name: "c"})
	if err != nil {
		t.Fatal(err)
	}

	// A command with no configured output returns empty, which is what makes
	// "the host key was not generated" a case the caller must handle.
	out, err := e.Exec(ctx, id, []string{"cat", "/etc/ssh/key"})
	if err != nil || out != "" {
		t.Errorf("Exec = %q, %v", out, err)
	}
	e.ExecOutput["cat /etc/ssh/key"] = "ssh-ed25519 AAAA"
	if out, _ := e.Exec(ctx, id, []string{"cat", "/etc/ssh/key"}); out != "ssh-ed25519 AAAA" {
		t.Errorf("Exec = %q", out)
	}

	e.SetState(id, "exited", 7)
	c, err := e.ContainerInspect(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if c.State != "exited" || c.ExitCode != 7 {
		t.Errorf("SetState did not take: %+v", c)
	}
	// SetState on something absent must not panic.
	e.SetState("ghost", "running", 0)
}

func TestRecordingEngine_RecordsEveryCall(t *testing.T) {
	e := NewRecordingEngine()
	if err := e.Ping(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := e.NetworkCreate(t.Context(), "n", nil); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(e.Recorded(), "|")
	if !strings.Contains(got, "Ping") || !strings.Contains(got, "NetworkCreate n") {
		t.Errorf("Recorded() = %q", got)
	}
}

func TestRecordingEngine_PortMapOverridesTheGeneratedPort(t *testing.T) {
	e := NewRecordingEngine()
	e.PortMap[22] = 49999
	id, err := e.ContainerCreate(t.Context(), ContainerSpec{Name: "c", PublishTCP: []uint16{22, 8188}})
	if err != nil {
		t.Fatal(err)
	}
	c, err := e.ContainerInspect(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if c.Ports[22].Port != 49999 {
		t.Errorf("PortMap ignored: %v", c.Ports)
	}
	if c.Ports[8188].Port == 0 {
		t.Error("an unmapped port should still get a plausible number")
	}
}

func TestBinding_IsLoopback(t *testing.T) {
	for _, tc := range []struct {
		ip   string
		want bool
	}{
		{"127.0.0.1", true},
		{"127.0.0.53", true}, // loopback without the obvious spelling
		{"::1", true},
		{"0.0.0.0", false}, // Docker's own default: every interface
		{"::", false},
		{"192.168.1.10", false},
		{"", false},        // cannot tell, so assume public
		{"garbage", false}, // same
	} {
		if got := (Binding{IP: tc.ip, Port: 22}).IsLoopback(); got != tc.want {
			t.Errorf("Binding{%q}.IsLoopback() = %v, want %v", tc.ip, got, tc.want)
		}
	}
}

func TestBinding_String(t *testing.T) {
	if got := (Binding{IP: "127.0.0.1", Port: 40022}).String(); got != "127.0.0.1:40022" {
		t.Errorf("got %q", got)
	}
	if got := (Binding{IP: "::1", Port: 22}).String(); got != "[::1]:22" {
		t.Errorf("got %q", got)
	}
}

// TestRecordingEngine_BindsToLoopbackByDefault, matching what client.go asks
// the real daemon for. A fake that binds somewhere safer than production is a
// fake that hides the bug it exists to catch.
func TestRecordingEngine_BindsToLoopbackByDefault(t *testing.T) {
	e := NewRecordingEngine()
	id, err := e.ContainerCreate(t.Context(), ContainerSpec{Name: "c", PublishTCP: []uint16{22}})
	if err != nil {
		t.Fatal(err)
	}
	c, err := e.ContainerInspect(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if !c.Ports[22].IsLoopback() {
		t.Fatalf("the fake published 22 on %s", c.Ports[22])
	}
}

func TestRecordingEngine_BindIPCanReproduceDockersDefault(t *testing.T) {
	e := NewRecordingEngine()
	e.BindIP = "0.0.0.0"
	id, err := e.ContainerCreate(t.Context(), ContainerSpec{Name: "c", PublishTCP: []uint16{22}})
	if err != nil {
		t.Fatal(err)
	}
	c, err := e.ContainerInspect(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if c.Ports[22].IsLoopback() {
		t.Fatal("BindIP was ignored, so no test can reproduce a LAN-reachable node")
	}
}
