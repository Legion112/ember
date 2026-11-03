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
//     into trust-on-first-use;
//   - a CPU pod refuses a container disk above 20 GiB with a 400, a limit the
//     published schema gives no maximum for -- so the clamp in
//     Provider.disk is load-bearing and a fake that accepted any size would
//     let its removal pass;
//   - the catalogue endpoints refuse `include=AVAILABILITY` without
//     `product`, and report no availability at all when it is not asked for,
//     because `include` is the difference between a catalogue with capacity
//     in it and one where every offer is filtered out as having none;
//   - a network volume is refused for the v1 field name and for a size below
//     the measured minimum of ten, both of which are what the live v2 API
//     answered 422 to -- see volumes.go, and s3.go for the endpoint over the
//     directory each volume is backed by.
//
// The catalogue bodies are the provider's own committed fixtures rather than
// copies kept here, and that is a decision worth stating because the rest of
// this package argues the other way. What a fixture cannot express is time,
// and a catalogue read has none: ember makes one GET and the answer does not
// depend on how long anything has been alive. What is left is a field shape,
// and a second copy of a field shape is exactly how RunPod's `dataCenter`
// against `dataCenterId` disagreement between its two APIs stayed invisible
// -- the unit tests would have gone on decoding a shape the fake invented.
// Where the catalogue does need behaviour a file cannot hold -- the query
// refusals above -- that is code in this package operating on the one body,
// not a second body. Options.Catalogue stays for a lab that wants to serve a
// different catalogue, which is a lab choosing an input rather than a second
// account of what RunPod's fields are called.
//
// The control plane is fake; the data plane is not. Create starts a real
// container through the Nodes seam, so a lab above this does a real SSH
// handshake against a real sshd using the key ember minted, forwards a real
// port and pulls real bytes back. Only the JSON is pretend. A store is part
// of that data plane now: `mounts.network` is honoured by bind-mounting the
// directory the volume route created, and the S3 endpoint reads that same
// directory -- so "the sync filled the store" and "the pre-flight can see it"
// are one fact rather than two fakes agreeing.
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

	"github.com/Legion112/ember/internal/awssig"
)

// WebTerminalPort is the mapping RunPod adds to every pod with no way to
// disable it. Named here as well as in the provider so a lab can assert that
// ember tolerates exactly this and nothing else.
const WebTerminalPort = 19123

// cpuMaxDiskGiB is the ceiling a CPU pod's container disk is refused above.
//
// Spelled here rather than imported from the provider on purpose. This is the
// fake's account of what the service does, and the provider's CPUMaxDiskGiB is
// ember's account of what it must therefore ask for; sharing one constant
// would mean a lab about the clamp could never fail, because raising ember's
// number would raise the limit it is tested against at the same time. The
// number was measured -- the schema gives the field no maximum -- and the
// message is the one the API answers with.
const cpuMaxDiskGiB = 20

// cpuDiskRefusal is the API's own wording, so an operator who sees it in a
// lab failure can search for the same string they would see live.
const cpuDiskRefusal = "Container Disk must be less than or equal to 20"

// cpuHourlyUSD is what the fake bills a CPU pod.
//
// Two cores of the captured catalogue's cheapest family, $0.03 per vCPU per
// hour. A single hardcoded GPU price for every pod would have had ember read
// an RTX 4090's rate back off a machine with no card -- and the rate is read
// back from the pod precisely because it is the bill rather than a list
// price, so a fake that quotes the wrong one makes `ember ls` and the spend
// ledger agree with a number nothing charged.
const cpuHourlyUSD = 0.06

// gpuHourlyUSD is the rate a GPU pod reports, matching the captured
// catalogue's RTX 4090.
const gpuHourlyUSD = 0.89

// Nodes is the data plane: something that can really start a machine.
//
// A seam rather than a direct Docker dependency, so the fake can be driven
// without one when a lab only cares about the control plane.
type Nodes interface {
	// Start brings up a node carrying the operator's key, the host key ember
	// minted and whatever storage the create asked for, and reports where
	// sshd can be reached.
	//
	// A struct rather than four positional arguments, and the mounts are why:
	// a fake that accepted `mounts.network` and started a container with
	// nothing at that path would be tidier than the service it stands in for.
	// The directory really is there on RunPod, and a workload that found it
	// empty is the bug this exists to catch.
	Start(m Machine) (id, host string, port uint16)

	// Stop destroys it. Called for a pod ember deletes.
	Stop(id string)
}

// Machine is one node for the data plane to bring up.
type Machine struct {
	// Name is the pod id, so a container can be traced back to the call that
	// created it.
	Name string

	// AuthorizedKey is the operator's public key, in authorized_keys form.
	AuthorizedKey string

	// HostKeyPEM is the private host key ember minted. It travels in the
	// create body and never in argv, as in production.
	HostKeyPEM string

	// Mounts is the storage `mounts.network` asked for.
	Mounts []Mount
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

	// CPUCatalogue is the same for the CPU endpoint, which is a different
	// catalogue on a different path with a price quoted per core rather than
	// per machine. Empty serves the captured cpus.json.
	CPUCatalogue []byte

	// S3 is the key pair the store endpoint accepts. Zero accepts the pair
	// S3Credentials returns, so a lab and the endpoint cannot disagree about
	// them by each writing a literal.
	//
	// It is a field because the wrong-credential case is one a lab has to be
	// able to produce: a fake that accepted any signature could not catch
	// ember signing with the wrong key, the wrong region or the wrong
	// header set, and a 403 with no detail is SigV4's only other feedback.
	S3 awssig.Credentials
}

// Fake is a running stand-in.
type Fake struct {
	// URL is the base to point a provider at.
	URL string

	// S3URL is the base to point app.Deps.S3BaseURL at.
	//
	// A second server rather than a second path prefix on the first, because
	// they are separate services on separate hosts -- which is also why
	// app.Deps carries two fields. One server would let a store call reach
	// the pods API by writing the wrong path, which is a mistake the real
	// deployment cannot make.
	S3URL string

	t     *testing.T
	nodes Nodes
	opts  Options
	srv   *httptest.Server
	s3srv *httptest.Server

	// volRoot is the parent of every volume's directory, owned by the test
	// process so what a container wrote as root still comes away.
	volRoot string

	mu         sync.Mutex
	pods       map[string]*pod
	volumes    map[string]*volume
	nextID     int
	nextVolume int
	requests   []Request
}

// Request is one call ember made, for labs that assert on what was sent.
type Request struct {
	Method string
	Path   string

	// Query is the raw query string. Recorded because the catalogue paths
	// carry `include=AVAILABILITY&product=POD` and the parameters are the
	// difference between a catalogue with capacity in it and one without:
	// a lab asserting ember asked for availability has to be able to see
	// that it did, not only that the answer happened to contain some.
	Query string

	Body map[string]any
}

type pod struct {
	ID        string
	Name      string
	Image     string
	GpuID     string
	GpuCount  int
	CpuID     string
	VcpuCount int
	Disk      int
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

	f := &Fake{
		t: t, nodes: nodes, opts: opts,
		pods:    map[string]*pod{},
		volumes: map[string]*volume{},
		// t.TempDir and not os.MkdirTemp, so a directory this suite cannot
		// clean up fails the test rather than accumulating in /tmp. What a
		// container wrote as root is unlinkable because the directory above
		// it belongs to this process -- see volumes.go on the one shape that
		// is not.
		volRoot: t.TempDir(),
	}
	f.srv = httptest.NewServer(http.HandlerFunc(f.route))
	t.Cleanup(f.srv.Close)
	f.URL = f.srv.URL

	f.s3srv = httptest.NewServer(http.HandlerFunc(f.routeS3))
	t.Cleanup(f.s3srv.Close)
	f.S3URL = f.s3srv.URL
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
	f.requests = append(f.requests, Request{
		Method: r.Method, Path: r.URL.Path, Query: r.URL.RawQuery, Body: body,
	})
	f.mu.Unlock()

	// Authorization is checked, because a fake that accepts an unauthenticated
	// call cannot catch ember forgetting to send the header.
	if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
		f.fail(w, http.StatusUnauthorized, "missing bearer token")
		return
	}

	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/v2/catalog/gpus":
		f.catalogue(w, r, gpuFixture, f.opts.Catalogue)
	case r.Method == http.MethodGet && r.URL.Path == "/v2/catalog/cpus":
		// The endpoint `ember store sync` rents its cheap downloaders from.
		// It had no route here at all, so cpuOffers -- and with it the
		// CPUOnly exclusivity and the container-disk clamp -- had never run
		// against the fake in any lab, and a request for it 404'd.
		f.catalogue(w, r, cpuFixture, f.opts.CPUCatalogue)
	case r.Method == http.MethodPost && r.URL.Path == "/v2/pods":
		f.create(w, body)
	case r.Method == http.MethodGet && r.URL.Path == "/v2/pods":
		f.list(w)
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v2/pods/"):
		f.inspect(w, strings.TrimPrefix(r.URL.Path, "/v2/pods/"))
	case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/v2/pods/"):
		f.destroy(w, strings.TrimPrefix(r.URL.Path, "/v2/pods/"))
	case r.Method == http.MethodGet && r.URL.Path == volumePath:
		// The route `ember store` lives at, which this package did not serve
		// at all -- so seven subcommands and the store pre-flight were paths
		// no lab could reach.
		f.listVolumes(w)
	case r.Method == http.MethodPost && r.URL.Path == volumePath:
		f.createVolume(w, body)
	case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, volumePath+"/"):
		f.destroyVolume(w, strings.TrimPrefix(r.URL.Path, volumePath+"/"))
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

// catalogue serves one of the two catalogue endpoints, as unhelpfully as the
// live one does.
//
// override is the lab's own body, if it supplied one; otherwise the provider's
// committed fixture is served verbatim, so the fake's field shapes and the
// unit tests' cannot drift apart. The captured gpus.json carries entries with
// no `dataCenters` key at all, which is a real omission and the reason
// hasCapacity has to read the summary as well as the sites -- so that arrives
// here for free, and a fake with its own tidier body would have lost it.
func (f *Fake) catalogue(w http.ResponseWriter, r *http.Request, fixture string, override []byte) {
	q := r.URL.Query()
	wantAvailability := false
	for _, inc := range q["include"] {
		if strings.EqualFold(inc, "AVAILABILITY") {
			wantAvailability = true
		}
	}

	// `include=AVAILABILITY` is refused without `product`: stock differs
	// between pods and serverless, so the endpoint will not answer the
	// question without being told which it is about. Observed as a 400 on the
	// CPU endpoint and stated in both endpoints' descriptions in the live v2
	// OpenAPI document. Reproduced because the query lives in one constant
	// per endpoint feeding three call sites -- two offer paths and the
	// availability snapshot -- and a dropped parameter there is a catalogue
	// with no capacity in it rather than an error anybody sees.
	if wantAvailability && q.Get("product") == "" {
		f.fail(w, http.StatusBadRequest,
			"include=AVAILABILITY requires product; availability differs by product context")
		return
	}

	raw := override
	if raw == nil {
		raw = f.captured(fixture)
	}
	if !wantAvailability {
		// Not asked for, not reported. The levels exist only because
		// `include` asks for them, and ember drops an entry nothing anywhere
		// says has capacity -- so a request that forgot the parameter gets a
		// priced catalogue that produces no offers at all, which presents as
		// "that card is sold out everywhere". A fake that answered the full
		// body regardless would make the parameter look optional.
		raw = withoutAvailability(f.t, raw)
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(raw)
}

// captured reads one of the provider's committed catalogue fixtures.
//
// A literal path per file rather than one assembled from the parameter,
// because the set is closed at two and a read whose path came from somewhere
// else is worth the second look gosec's G304 asks for. The relative path is
// from test/integration, which is where every lab that drives this fake runs.
func (f *Fake) captured(fixture string) []byte {
	f.t.Helper()
	var raw []byte
	var err error
	switch fixture {
	case gpuFixture:
		raw, err = os.ReadFile(filepath.Join("..", "..", "internal", "provider", "runpod", "testdata", "gpus.json"))
	case cpuFixture:
		raw, err = os.ReadFile(filepath.Join("..", "..", "internal", "provider", "runpod", "testdata", "cpus.json"))
	default:
		f.t.Fatalf("no captured catalogue named %q", fixture)
	}
	if err != nil {
		f.t.Fatalf("read the captured catalogue %s: %v", fixture, err)
	}
	return raw
}

// The two catalogue fixtures, named so the router and captured cannot disagree
// about which file an endpoint serves.
const (
	gpuFixture = "gpus.json"
	cpuFixture = "cpus.json"
)

// withoutAvailability strips the levels from every entry of a catalogue body.
//
// Generic over the top-level array's name because the two endpoints spell it
// differently ("gpus", "cpus") and the point is the shape of an entry, not
// which catalogue it came from.
func withoutAvailability(t *testing.T, raw []byte) []byte {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("decode the catalogue to strip availability: %v", err)
	}
	for _, v := range doc {
		entries, ok := v.([]any)
		if !ok {
			continue
		}
		for _, e := range entries {
			entry, ok := e.(map[string]any)
			if !ok {
				continue
			}
			delete(entry, "availability")
			delete(entry, "dataCenters")
		}
	}
	out, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("re-encode the stripped catalogue: %v", err)
	}
	return out
}

func (f *Fake) create(w http.ResponseWriter, body map[string]any) {
	cpu, isCPU := body["cpu"].(map[string]any)
	disk := int(num(body["disk"]))

	// A CPU pod refuses a container disk above 20 GiB, and the published
	// schema gives the field no maximum -- the limit is enforced at the
	// handler, which is why it was found by a 400 and not by reading
	// anything. GPU pods have no such ceiling, so this is a property of the
	// product rather than a cap that could be lowered for both.
	//
	// Reproduced here because ember clamps rather than refuses, and a clamp
	// is only load-bearing against a service that would have said no: with
	// the fake accepting any size, deleting the clamp passed every test. The
	// status and the message are the live ones; the envelope around them is
	// this package's own, because only the wording was captured.
	if isCPU && disk > cpuMaxDiskGiB {
		f.fail(w, http.StatusBadRequest, cpuDiskRefusal)
		return
	}

	// The storage, read before the pod exists: a create naming a volume this
	// account does not have cannot succeed, and answering 200 and then
	// starting a container with nothing at the mount path would be the
	// tidier-than-the-service failure -- a workload reading an empty
	// directory and a lab that could not tell it from a full store.
	//
	// `disk` is NOT refused beside a mount. The schema says `persistent` and
	// `network` are mutually exclusive and ember never sends `persistent`:
	// the first live store run created a pod with both `disk` and
	// `mounts.network` and RunPod honoured it, so a refusal here would make
	// every store rental a 400 that the service does not answer.
	mounts, err := f.mountsOf(body)
	if err != nil {
		f.fail(w, http.StatusBadRequest, err.Error())
		return
	}

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
		Disk:    disk,
	}
	if g, ok := body["gpu"].(map[string]any); ok {
		p.GpuID = str(g["id"])
		p.GpuCount = int(num(g["count"]))
	}
	if isCPU {
		p.CpuID = str(cpu["id"])
		p.VcpuCount = int(num(cpu["vcpuCount"]))
	}
	f.pods[id] = p
	nodes := f.nodes
	f.mu.Unlock()

	// The data plane is real. Starting it here rather than lazily means a
	// create that the fake accepted is a machine that genuinely exists, so a
	// lab cannot pass while the container failed to come up.
	if nodes != nil {
		nodeID, host, port := nodes.Start(Machine{
			Name:          id,
			AuthorizedKey: p.Env["EMBER_AUTHORIZED_KEY"],
			HostKeyPEM:    p.Env["EMBER_HOST_KEY"],
			Mounts:        mounts,
		})
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
	// A pod that has already been deleted is gone, so the second DELETE is a
	// 404 exactly as the first DELETE of a pod that never existed is. This
	// used to check only `ok`, which left a destroyed pod in the map and
	// answered 200 to every later delete -- so a destroy ember repeated
	// looked successful here for a reason the live API does not have, and
	// providertest's destroy-is-idempotent check passed against this fake
	// without the not-found path ever being taken. A fake tidier than the
	// service it stands in for tests a service that does not exist.
	if ok && p.Destroyed {
		ok = false
	}
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
		"cost":         gpuHourlyUSD,
		"createdAt":    p.Created.UTC().Format(time.RFC3339),
		"gpu":          map[string]any{"id": p.GpuID, "count": p.GpuCount},
		"ssh":          nil,
		"runtime":      nil,
	}
	// A CPU pod reports the machine it is, which is one with no card. The
	// previous shape echoed a `gpu` object onto every pod and quoted an RTX
	// 4090's hourly rate for all of them, so an assertion about whether a
	// CPU-only run got a card would have passed against either answer, and
	// the rate ember reads back off the pod -- the bill, deliberately not the
	// catalogue's list price -- was fifteen times what a CPU box costs.
	if p.CpuID != "" {
		delete(out, "gpu")
		out["cpu"] = map[string]any{"id": p.CpuID, "count": p.VcpuCount}
		out["cost"] = cpuHourlyUSD
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
