// Package runpodfake is a stateful stand-in for RunPod's v2 API.
//
// It exists because the unit tests around the RunPod provider use one-off HTTP
// handlers that return a canned blob, and a handler like that cannot express
// the thing that actually breaks: time. A pod is PROVISIONING and then it is
// RUNNING; `ssh.direct` is null and then it is not; `runtime` is null on a
// list and populated on a fetch by id. Every defect ember's first real
// provisions turned up lived in one of those transitions.
//
// So this fake has state, and it reproduces each behaviour that cost money to
// discover:
//
//   - a public 19123/http mapping appears on every pod, unbidden, which is
//     what made ember refuse and destroy its first node;
//   - `ssh.direct` stays null for a while after the status reads RUNNING,
//     because RunPod assigns the public 22/tcp mapping later;
//   - `runtime` is null until the pod is running;
//   - `ssh` and `runtime` are always null on `GET /v2/pods`, which is why
//     Inspect fetches one pod by id;
//   - nothing ever reports a host key, which is what silently dropped ember
//     into trust-on-first-use.
//
// The control plane is fake; the data plane is not. Create starts a real
// container through the Nodes seam, so a lab above this does a real SSH
// handshake against a real sshd using the key ember minted, forwards a real
// port and pulls real bytes back. Only the JSON is pretend.
package runpodfake

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// WebTerminalPort is the mapping RunPod adds to every pod with no way to
// disable it. Named here as well as in the provider so a lab can assert that
// ember tolerates exactly this and nothing else.
const WebTerminalPort = 19123

// Nodes is the data plane: something that can really start a machine.
//
// A seam rather than a direct Docker dependency, so the fake can be driven
// without one when a lab only cares about the control plane.
type Nodes interface {
	// Start brings up a node carrying the operator's key and the host key
	// ember minted, and reports where sshd can be reached.
	Start(name, authorizedKey, hostKeyPEM string) (id, host string, port uint16)

	// Stop destroys it. Called for a pod ember deletes.
	Stop(id string)
}

// Options tune the behaviours worth varying between labs.
type Options struct {
	// ReadyAfter is how long a pod stays PROVISIONING.
	ReadyAfter time.Duration

	// SSHAfter is how long after RUNNING before ssh.direct appears. RunPod
	// assigns the public 22/tcp mapping some time after the status flips,
	// and a node that is "running" with no endpoint is a state ember has to
	// sit through rather than fail on.
	SSHAfter time.Duration

	// NoPlatformPort suppresses the 19123/http mapping, for a lab that wants
	// to see ember's behaviour without it.
	NoPlatformPort bool

	// ExtraPublicPort, when set, is published as an http port on top of
	// everything else -- a provider doing something ember never asked for
	// and does not tolerate.
	ExtraPublicPort uint16

	// ReportHostKey makes pods report a host key. RunPod does not, and the
	// default of false is what dropped ember into TOFU.
	ReportHostKey string

	// Catalogue is the GPU catalogue to serve. Empty serves the captured one
	// from the provider's testdata.
	Catalogue []byte
}

// Fake is a running stand-in.
type Fake struct {
	// URL is the base to point a provider at.
	URL string

	t     *testing.T
	nodes Nodes
	opts  Options
	srv   *httptest.Server

	mu       sync.Mutex
	pods     map[string]*pod
	nextID   int
	requests []Request
}

// Request is one call ember made, for labs that assert on what was sent.
type Request struct {
	Method string
	Path   string
	Body   map[string]any
}

type pod struct {
	ID        string
	Name      string
	Image     string
	GpuID     string
	GpuCount  int
	Cloud     string
	Env       map[string]string
	Ports     []string
	Created   time.Time
	Destroyed bool

	// Data plane.
	nodeID  string
	sshHost string
	sshPort uint16
	started bool
}

// New starts a fake and registers its shutdown.
func New(t *testing.T, nodes Nodes, opts Options) *Fake {
	t.Helper()

	f := &Fake{t: t, nodes: nodes, opts: opts, pods: map[string]*pod{}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.route))
	t.Cleanup(f.srv.Close)
	f.URL = f.srv.URL
	return f
}

// Requests returns what ember sent, in order.
func (f *Fake) Requests() []Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Request(nil), f.requests...)
}

// Live returns the ids of pods that have not been deleted.
func (f *Fake) Live() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for id, p := range f.pods {
		if !p.Destroyed {
			out = append(out, id)
		}
	}
	return out
}

// Pod exposes one pod's recorded create request.
func (f *Fake) Pod(id string) (image string, env map[string]string, ports []string, ok bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p, found := f.pods[id]
	if !found {
		return "", nil, nil, false
	}
	return p.Image, p.Env, append([]string(nil), p.Ports...), true
}

func (f *Fake) route(w http.ResponseWriter, r *http.Request) {
	body := map[string]any{}
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&body)
	}
	f.mu.Lock()
	f.requests = append(f.requests, Request{Method: r.Method, Path: r.URL.Path, Body: body})
	f.mu.Unlock()

	// Authorization is checked, because a fake that accepts an unauthenticated
	// call cannot catch ember forgetting to send the header.
	if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
		f.fail(w, http.StatusUnauthorized, "missing bearer token")
		return
	}

	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/v2/catalog/gpus":
		f.catalogue(w)
	case r.Method == http.MethodPost && r.URL.Path == "/v2/pods":
		f.create(w, body)
	case r.Method == http.MethodGet && r.URL.Path == "/v2/pods":
		f.list(w)
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v2/pods/"):
		f.inspect(w, strings.TrimPrefix(r.URL.Path, "/v2/pods/"))
	case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/v2/pods/"):
		f.destroy(w, strings.TrimPrefix(r.URL.Path, "/v2/pods/"))
	default:
		// The real API's shape for an unknown route, which is what caught
		// ember's doubled /v2 prefix on the very first live call.
		f.fail(w, http.StatusNotFound, "The requested path was not found.")
	}
}

func (f *Fake) fail(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"detail": msg, "status": code, "title": http.StatusText(code),
	})
}

func (f *Fake) ok(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func (f *Fake) catalogue(w http.ResponseWriter) {
	raw := f.opts.Catalogue
	if raw == nil {
		// The captured response from the provider's own testdata, so the
		// fake and the unit tests cannot drift apart.
		var err error
		raw, err = os.ReadFile(filepath.Join("..", "..", "internal", "provider", "runpod", "testdata", "gpus.json"))
		if err != nil {
			f.t.Fatalf("read the captured catalogue: %v", err)
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(raw)
}

func (f *Fake) create(w http.ResponseWriter, body map[string]any) {
	f.mu.Lock()
	f.nextID++
	id := fmt.Sprintf("fakepod%06d", f.nextID)

	p := &pod{
		ID:      id,
		Name:    str(body["name"]),
		Image:   str(body["image"]),
		Cloud:   str(body["cloud"]),
		Created: time.Now(),
		Env:     strMap(body["env"]),
		Ports:   strSlice(body["ports"]),
	}
	if g, ok := body["gpu"].(map[string]any); ok {
		p.GpuID = str(g["id"])
		p.GpuCount = int(num(g["count"]))
	}
	f.pods[id] = p
	nodes := f.nodes
	f.mu.Unlock()

	// The data plane is real. Starting it here rather than lazily means a
	// create that the fake accepted is a machine that genuinely exists, so a
	// lab cannot pass while the container failed to come up.
	if nodes != nil {
		nodeID, host, port := nodes.Start(id, p.Env["EMBER_AUTHORIZED_KEY"], p.Env["EMBER_HOST_KEY"])
		f.mu.Lock()
		p.nodeID, p.sshHost, p.sshPort, p.started = nodeID, host, port, true
		f.mu.Unlock()
	}

	// RunPod returns the pod, and ember reads the id off it.
	f.ok(w, map[string]any{"id": id, "name": p.Name, "status": "PROVISIONING"})
}

func (f *Fake) inspect(w http.ResponseWriter, id string) {
	f.mu.Lock()
	p, ok := f.pods[id]
	f.mu.Unlock()
	if !ok || p.Destroyed {
		f.fail(w, http.StatusNotFound, "pod not found")
		return
	}
	f.ok(w, f.render(p, true))
}

func (f *Fake) list(w http.ResponseWriter) {
	f.mu.Lock()
	var live []*pod
	for _, p := range f.pods {
		if !p.Destroyed {
			live = append(live, p)
		}
	}
	f.mu.Unlock()

	pods := make([]map[string]any, 0, len(live))
	for _, p := range live {
		// withDetail false: RunPod documents ssh and runtime as null on every
		// entry of a list response, which is why Inspect fetches one pod by
		// id. A fake that populated them here would let a list-based Inspect
		// pass in testing and report every ready pod as provisioning forever
		// in production.
		pods = append(pods, f.render(p, false))
	}
	f.ok(w, map[string]any{"pods": pods, "pagination": map[string]any{"nextCursor": nil}})
}

func (f *Fake) destroy(w http.ResponseWriter, id string) {
	f.mu.Lock()
	p, ok := f.pods[id]
	if ok {
		p.Destroyed = true
	}
	nodes, nodeID := f.nodes, ""
	if ok {
		nodeID = p.nodeID
	}
	f.mu.Unlock()

	if !ok {
		// Not-found on delete is the case ember treats as success, so the
		// fake has to be able to produce it.
		f.fail(w, http.StatusNotFound, "pod not found")
		return
	}
	if nodes != nil && nodeID != "" {
		nodes.Stop(nodeID)
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{}`))
}

// render builds the JSON for one pod, in the shape the live API returns.
//
// withDetail is false for a list entry, where ssh and runtime are always null.
func (f *Fake) render(p *pod, withDetail bool) map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()

	age := time.Since(p.Created)
	running := age >= f.opts.ReadyAfter
	sshUp := running && age >= f.opts.ReadyAfter+f.opts.SSHAfter && p.started

	status := "PROVISIONING"
	if running {
		status = "RUNNING"
	}

	out := map[string]any{
		"id":           p.ID,
		"name":         p.Name,
		"status":       status,
		"image":        p.Image,
		"cloud":        p.Cloud,
		"dataCenterId": "US-IL-1",
		"cost":         0.89,
		"createdAt":    p.Created.UTC().Format(time.RFC3339),
		"gpu":          map[string]any{"id": p.GpuID, "count": p.GpuCount},
		"ssh":          nil,
		"runtime":      nil,
	}
	// RunPod reports no host key at all. That is the default here because it
	// is what dropped ember into trust-on-first-use against a brand-new IP,
	// and a fake that helpfully supplied one would hide the whole class.
	if f.opts.ReportHostKey != "" {
		parts := strings.SplitN(f.opts.ReportHostKey, " ", 2)
		if len(parts) == 2 {
			out["hostKey"] = map[string]any{"algo": parts[0], "base64": parts[1]}
		}
	}
	if !withDetail || !running {
		return out
	}

	ports := []map[string]any{}
	if sshUp {
		ports = append(ports, map[string]any{
			"private": 22, "public": int(p.sshPort), "type": "tcp", "ip": p.sshHost,
		})
		out["ssh"] = map[string]any{
			"proxy": map[string]any{
				"host": "ssh.runpod.io", "port": 22, "username": p.ID + "-fake",
			},
			"direct": map[string]any{
				"host": p.sshHost, "port": int(p.sshPort), "username": "root",
			},
		}
	}
	if !f.opts.NoPlatformPort {
		// The mapping RunPod adds to every pod, which ember never asked for.
		ports = append(ports, map[string]any{
			"private": WebTerminalPort, "public": 60549, "type": "http",
			"ip": "100.65.24.164",
		})
	}
	if f.opts.ExtraPublicPort != 0 {
		ports = append(ports, map[string]any{
			"private": int(f.opts.ExtraPublicPort), "public": 8188, "type": "http",
			"ip": "100.65.24.164",
		})
	}
	out["runtime"] = map[string]any{"uptime": int(age.Seconds()), "ports": ports}
	return out
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

func num(v any) float64 {
	n, _ := v.(float64)
	return n
}

func strMap(v any) map[string]string {
	m, _ := v.(map[string]any)
	out := map[string]string{}
	for k, val := range m {
		out[k] = str(val)
	}
	return out
}

func strSlice(v any) []string {
	l, _ := v.([]any)
	out := make([]string, 0, len(l))
	for _, e := range l {
		out = append(out, str(e))
	}
	return out
}

// SplitAddr is a convenience for a Nodes implementation returning "host:port".
func SplitAddr(t *testing.T, addr string) (string, uint16) {
	t.Helper()
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("split %q: %v", addr, err)
	}
	port, err := strconv.ParseUint(portStr, 10, 16)
	if err != nil {
		t.Fatalf("parse port in %q: %v", addr, err)
	}
	return host, uint16(port)
}
