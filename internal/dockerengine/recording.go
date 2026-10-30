package dockerengine

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
)

// RecordingEngine is an in-memory Docker daemon good enough to drive the local
// provider's logic.
//
// Field names follow go-tun's linux.RecordingRunner — Calls, Errors, FailOn —
// so anyone who has read one fake reads this one without learning anything.
type RecordingEngine struct {
	mu sync.Mutex

	// Calls records every operation as "ContainerCreate ember-node-abc".
	Calls []string

	// Errors fails a specific call, keyed by the same string Calls records.
	Errors map[string]error

	// FailOn fails any call whose record contains this substring.
	FailOn string

	// GPURuntime is what HasGPURuntime answers. False by default, which is
	// what a machine with no nvidia-container-toolkit has -- and what every
	// test written before GPU passthrough existed assumes.
	GPURuntime bool

	// ExecOutput is what Exec returns, keyed by the joined command. A command
	// with no entry returns the empty string, which is what makes "the host
	// key was not generated" a case the provider has to handle.
	ExecOutput map[string]string

	// PortMap is the host port the fake daemon "publishes" a container port
	// on. Unset ports get 30000+n, so a test that never looks at the number
	// still gets a plausible one.
	PortMap map[uint16]uint16

	// BindIP is the host address the fake daemon binds published ports to.
	// Empty means 127.0.0.1, which is what the real client asks for. A test
	// sets it to "0.0.0.0" to reproduce Docker's own default, where a
	// published port is reachable from the whole LAN.
	BindIP string

	containers map[string]*Container
	volumes    map[string]*Volume
	networks   map[string]*Network
	images     map[string]bool
	nextID     int
	nextPort   uint16
}

// NewRecordingEngine returns an empty fake daemon.
func NewRecordingEngine() *RecordingEngine {
	return &RecordingEngine{
		Errors:     map[string]error{},
		ExecOutput: map[string]string{},
		PortMap:    map[uint16]uint16{},
		containers: map[string]*Container{},
		networks:   map[string]*Network{},
		images:     map[string]bool{},
		nextPort:   30000,
	}
}

// AddImage makes ref appear present to ImageExists.
func (e *RecordingEngine) AddImage(ref string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.images[ref] = true
}

// Containers returns the live containers, for assertions.
func (e *RecordingEngine) Containers() []Container {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]Container, 0, len(e.containers))
	for _, c := range e.containers {
		out = append(out, *c)
	}
	return out
}

// Networks returns the live networks, for assertions.
func (e *RecordingEngine) Networks() []Network {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]Network, 0, len(e.networks))
	for _, n := range e.networks {
		out = append(out, *n)
	}
	return out
}

// Recorded returns every call made so far.
func (e *RecordingEngine) Recorded() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.Calls...)
}

// record notes a call and reports whether it was configured to fail. The
// caller holds e.mu.
func (e *RecordingEngine) record(op, arg string) error {
	call := strings.TrimSpace(op + " " + arg)
	e.Calls = append(e.Calls, call)
	if err, ok := e.Errors[call]; ok {
		return err
	}
	if e.FailOn != "" && strings.Contains(call, e.FailOn) {
		return fmt.Errorf("recording engine refusing %q (FailOn %q)", call, e.FailOn)
	}
	return nil
}

func (e *RecordingEngine) Ping(context.Context) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.record("Ping", "")
}

func (e *RecordingEngine) ImageExists(_ context.Context, ref string) (bool, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.record("ImageExists", ref); err != nil {
		return false, err
	}
	return e.images[ref], nil
}

func (e *RecordingEngine) NetworkCreate(_ context.Context, name string, labels map[string]string) (string, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.record("NetworkCreate", name); err != nil {
		return "", err
	}
	e.nextID++
	id := fmt.Sprintf("net%d", e.nextID)
	e.networks[id] = &Network{ID: id, Name: name, Labels: copyMap(labels)}
	return id, nil
}

func (e *RecordingEngine) NetworkRemove(_ context.Context, id string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.record("NetworkRemove", id); err != nil {
		return err
	}
	if _, ok := e.networks[id]; !ok {
		return ErrNotFound
	}
	delete(e.networks, id)
	return nil
}

func (e *RecordingEngine) NetworkList(_ context.Context, label string) ([]Network, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.record("NetworkList", label); err != nil {
		return nil, err
	}
	var out []Network
	for _, n := range e.networks {
		if hasLabel(n.Labels, label) {
			out = append(out, *n)
		}
	}
	return out, nil
}

// bindIP mirrors what the real client asks the daemon for. A test sets BindIP
// to 0.0.0.0 to reproduce Docker's own default -- the dangerous one -- without
// needing a daemon.
func (e *RecordingEngine) bindIP() string {
	if e.BindIP != "" {
		return e.BindIP
	}
	return "127.0.0.1"
}

func (e *RecordingEngine) ContainerCreate(_ context.Context, spec ContainerSpec) (string, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.record("ContainerCreate", spec.Name); err != nil {
		return "", err
	}
	if err := RefuseHostNetwork(spec.NetworkMode); err != nil {
		return "", err
	}
	e.nextID++
	id := fmt.Sprintf("c%d", e.nextID)

	ports := map[uint16]Binding{}
	for _, p := range spec.PublishTCP {
		host, ok := e.PortMap[p]
		if !ok {
			e.nextPort++
			host = e.nextPort
		}
		ports[p] = Binding{IP: e.bindIP(), Port: host}
	}
	e.containers[id] = &Container{
		ID: id, Name: spec.Name, Image: spec.Image, State: "created",
		Labels: copyMap(spec.Labels), Ports: ports,
		IPs: map[string]string{spec.Network: "10.88.0.2"},
		// Recorded rather than dropped. Binds reached the real client and
		// nothing else, so a unit test could not tell a container created
		// with a store mounted from one created without -- which is exactly
		// the assertion the local store needs to be able to make.
		Binds: append([]string(nil), spec.Binds...),
		// And the same for the GPU count, for the same reason and with a
		// worse failure if it were dropped: a node created without a card
		// does not fail, it runs the workload on the CPU and bills for the
		// card it did not get.
		GPUs: spec.GPUs,
	}
	return id, nil
}

func (e *RecordingEngine) ContainerStart(_ context.Context, id string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.record("ContainerStart", id); err != nil {
		return err
	}
	c, ok := e.containers[id]
	if !ok {
		return ErrNotFound
	}
	c.State = "running"
	return nil
}

func (e *RecordingEngine) ContainerInspect(_ context.Context, id string) (Container, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.record("ContainerInspect", id); err != nil {
		return Container{}, err
	}
	c, ok := e.containers[id]
	if !ok {
		return Container{}, ErrNotFound
	}
	return *c, nil
}

func (e *RecordingEngine) ContainerRemove(_ context.Context, id string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.record("ContainerRemove", id); err != nil {
		return err
	}
	if _, ok := e.containers[id]; !ok {
		return ErrNotFound
	}
	delete(e.containers, id)
	return nil
}

func (e *RecordingEngine) ContainerList(_ context.Context, label string) ([]Container, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.record("ContainerList", label); err != nil {
		return nil, err
	}
	var out []Container
	for _, c := range e.containers {
		if hasLabel(c.Labels, label) {
			out = append(out, *c)
		}
	}
	return out, nil
}

func (e *RecordingEngine) Exec(_ context.Context, id string, cmd []string) (string, error) {
	joined := strings.Join(cmd, " ")
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.record("Exec", id+" "+joined); err != nil {
		return "", err
	}
	if _, ok := e.containers[id]; !ok {
		return "", ErrNotFound
	}
	return e.ExecOutput[joined], nil
}

// SetState forces a container's state, so a test can stage "the workload
// exited" without waiting for anything.
func (e *RecordingEngine) SetState(id, state string, exitCode int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if c, ok := e.containers[id]; ok {
		c.State = state
		c.ExitCode = exitCode
	}
}

func copyMap(m map[string]string) map[string]string {
	if m == nil {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// hasLabel matches the "key=value" or bare "key" filter Docker accepts.
func hasLabel(labels map[string]string, filter string) bool {
	if filter == "" {
		return true
	}
	k, v, hasValue := strings.Cut(filter, "=")
	got, ok := labels[k]
	if !ok {
		return false
	}
	return !hasValue || got == v
}

func (e *RecordingEngine) VolumeCreate(_ context.Context, name string, labels map[string]string) (Volume, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.record("VolumeCreate", name); err != nil {
		return Volume{}, err
	}
	if e.volumes == nil {
		e.volumes = map[string]*Volume{}
	}
	// Idempotent, as the daemon is: creating a volume that exists returns it.
	if v, ok := e.volumes[name]; ok {
		return *v, nil
	}
	v := &Volume{Name: name, Labels: copyMap(labels)}
	e.volumes[name] = v
	return *v, nil
}

func (e *RecordingEngine) VolumeRemove(_ context.Context, name string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.record("VolumeRemove", name); err != nil {
		return err
	}
	if _, ok := e.volumes[name]; !ok {
		return ErrNotFound
	}
	delete(e.volumes, name)
	return nil
}

func (e *RecordingEngine) VolumeList(_ context.Context, label string) ([]Volume, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.record("VolumeList", label); err != nil {
		return nil, err
	}
	var out []Volume
	for _, v := range e.volumes {
		if label != "" {
			if _, ok := v.Labels[label]; !ok {
				continue
			}
		}
		out = append(out, *v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// HasGPURuntime answers GPURuntime, and records the call.
func (e *RecordingEngine) HasGPURuntime(_ context.Context) (bool, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.record("HasGPURuntime", ""); err != nil {
		return false, err
	}
	return e.GPURuntime, nil
}
