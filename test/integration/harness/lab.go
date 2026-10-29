// Package harness drives ember's real binary against a real Docker daemon.
//
// The labs here run the compiled `ember` as a subprocess rather than calling
// into internal packages. That costs a build and some milliseconds, and it
// buys three things no unit test reaches: the real Docker client in
// internal/dockerengine/dockerclient, which is excluded from the coverage gate
// as a branchless adapter and would otherwise never run at all; the real exit
// codes, which are the contract with cron; and the ability to SIGKILL ember
// mid-provision, which is the one way to produce the orphan that `ember ls`
// and `ember reap` exist to find.
//
// Nothing here skips. A missing daemon or a missing image is a hard failure
// naming the command that fixes it. A lab that skips itself reports success
// for something that never ran, which is worse than a red build: go-tun's
// AGENTS.md records the same rule and the reason it was written down.
package harness

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"hash/crc64"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"
	"github.com/docker/go-connections/nat"

	"github.com/Legion112/ember/internal/dockerengine"
	"github.com/Legion112/ember/internal/loopback"
	"github.com/Legion112/ember/internal/node"
	"github.com/Legion112/ember/internal/provider"
	"github.com/Legion112/ember/internal/provider/local"
	"github.com/Legion112/ember/internal/statestore"
)

// LabImage is what ember's local provider starts as a node.
const LabImage = "ember:lab"

// EnvGate is the second of the two gates on these labs. The build tag keeps
// them out of an ordinary `go test ./...`; this keeps them out of a tagged run
// by someone who did not mean to start containers.
const EnvGate = "EMBER_INTEGRATION"

// Lab is one isolated ember installation plus the assertions about it.
type Lab struct {
	// Dir is the lab's whole world: ember.yaml, the config dir holding the
	// operator's key, and the state dir holding the journal.
	Dir string

	// ConfigPath is the ember.yaml every command is pointed at.
	ConfigPath string

	// Bin is the compiled ember under test.
	Bin string

	cli *client.Client
	t   *testing.T
}

// Require fails the test unless this machine can actually run a lab.
//
// Each failure names the command that fixes it, because the alternative is a
// test that fails with a Docker API error forty lines deep in a provider.
func Require(t *testing.T) {
	t.Helper()
	if os.Getenv(EnvGate) != "1" {
		t.Fatalf("%s is not set to 1.\n"+
			"These labs start real containers, so they are gated twice: the integration build tag and this variable.\n"+
			"Run them with:\n  make test-integration", EnvGate)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		t.Fatalf("cannot construct a Docker client: %v", err)
	}
	defer func() { _ = cli.Close() }()

	if _, err := cli.Ping(ctx); err != nil {
		t.Fatalf("no Docker daemon reachable: %v\nThese labs need one; there is no fake at this layer.", err)
	}
	if _, err := cli.ImageInspect(ctx, LabImage); err != nil {
		t.Fatalf("the lab image %s is missing: %v\nBuild it with:\n  make docker-build", LabImage, err)
	}
}

// New returns a lab with its own config, state and operator key.
//
// The cleanup asserts that nothing wearing ember's label survived. That
// assertion is the point of every lab in this package: a passing lifecycle
// test that leaves a container behind has demonstrated the opposite of what it
// claims, and on a rented GPU it would still be billing.
func New(t *testing.T) *Lab {
	t.Helper()
	Require(t)

	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		t.Fatalf("docker client: %v", err)
	}

	dir := t.TempDir()
	l := &Lab{
		Dir:        dir,
		ConfigPath: filepath.Join(dir, "ember.yaml"),
		Bin:        buildEmber(t),
		cli:        cli,
		t:          t,
	}

	l.WriteConfig(map[string]string{})

	t.Cleanup(func() {
		l.Reap()
		l.AssertNoStray()
		_ = cli.Close()
	})
	return l
}

// WriteConfig writes ember.yaml with the lab's directories plus extra lines.
//
// config_dir and state_dir point inside the lab, so a lab never reads or
// writes the operator's real ~/.config/ember -- including their real journal,
// which a reap lab would otherwise be entitled to act on.
func (l *Lab) WriteConfig(extra map[string]string) {
	l.t.Helper()

	lines := []string{
		"provider: local",
		"image: " + LabImage,
		"ttl: 10m",
		"config_dir: " + filepath.Join(l.Dir, "config"),
		"state_dir: " + filepath.Join(l.Dir, "state"),
		"output_dir: " + l.OutputDir(),
	}
	for k, v := range extra {
		lines = append(lines, k+": "+v)
	}
	body := strings.Join(lines, "\n") + "\n"
	if err := os.WriteFile(l.ConfigPath, []byte(body), 0o600); err != nil {
		l.t.Fatalf("write %s: %v", l.ConfigPath, err)
	}
}

// buildEmber compiles the binary under test once per package run.
func buildEmber(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(binDir(t), "ember")
	if _, err := os.Stat(bin); err == nil {
		return bin
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	cmd := exec.CommandContext(ctx, goTool(), "build", "-o", bin, "./cmd/ember")
	cmd.Dir = repoRoot(t)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build ember: %v\n%s", err, out)
	}
	return bin
}

// binDir is shared across the package's tests so the binary is built once.
var sharedBin string

func binDir(t *testing.T) string {
	t.Helper()
	if sharedBin == "" {
		d, err := os.MkdirTemp("", "ember-lab-bin")
		if err != nil {
			t.Fatalf("temp dir: %v", err)
		}
		sharedBin = d
	}
	return sharedBin
}

// goTool prefers the same toolchain the Makefile selects, so the lab cannot
// compile ember with a different compiler than the one go.mod declares.
func goTool() string {
	sdk := filepath.Join(os.Getenv("HOME"), "sdk", "go1.27.0", "bin", "go")
	if _, err := os.Stat(sdk); err == nil {
		return sdk
	}
	return "go"
}

func repoRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	// This package lives at test/integration, so the root is two up.
	return filepath.Join(wd, "..", "..")
}

// Result is one ember invocation.
type Result struct {
	Stdout   string
	ExitCode int
	Err      error
}

// Combined is for assertions that do not care which stream a line came from.
func (r Result) Combined() string { return r.Stdout }

// Run invokes ember with the lab's config and waits for it to finish.
func (l *Lab) Run(ctx context.Context, args ...string) Result {
	l.t.Helper()
	return l.run(ctx, nil, args...)
}

func (l *Lab) run(ctx context.Context, before func(*exec.Cmd), args ...string) Result {
	l.t.Helper()

	full := append([]string{args[0], "--config", l.ConfigPath}, args[1:]...)
	cmd := exec.CommandContext(ctx, l.Bin, full...)
	cmd.Dir = l.Dir
	// A deliberately small environment. HOME is inside the lab so that a bug
	// in path resolution writes to a temp dir and fails the lab, instead of
	// quietly touching the operator's real ~/.config/ember.
	cmd.Env = append(os.Environ(), "HOME="+l.Dir)
	if before != nil {
		before(cmd)
	}

	out, err := cmd.CombinedOutput()
	res := Result{Stdout: string(out), Err: err}

	var ee *exec.ExitError
	switch {
	case err == nil:
		res.ExitCode = 0
	case errors.As(err, &ee):
		res.ExitCode = ee.ExitCode()
	default:
		res.ExitCode = -1
	}
	return res
}

// Start launches ember without waiting, for the labs that need to interrupt
// it. The returned stop function kills the process and collects its output.
func (l *Lab) Start(ctx context.Context, args ...string) (*exec.Cmd, func() string) {
	l.t.Helper()

	full := append([]string{args[0], "--config", l.ConfigPath}, args[1:]...)
	cmd := exec.CommandContext(ctx, l.Bin, full...)
	cmd.Dir = l.Dir
	cmd.Env = append(os.Environ(), "HOME="+l.Dir)

	var buf syncBuffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf

	if err := cmd.Start(); err != nil {
		l.t.Fatalf("start ember %v: %v", args, err)
	}
	return cmd, func() string {
		_ = cmd.Wait()
		return buf.String()
	}
}

// Nodes lists the containers wearing ember's run-id label.
//
// It asks Docker directly rather than running `ember ls`, because a lab that
// used ember to check ember would pass whenever both sides were wrong in the
// same direction -- which is exactly what a label-matching bug looks like.
func (l *Lab) Nodes(ctx context.Context) []container.Summary {
	l.t.Helper()
	cs, err := l.cli.ContainerList(ctx, container.ListOptions{
		All:     true,
		Filters: filters.NewArgs(filters.Arg("label", dockerengine.LabelRunID)),
	})
	if err != nil {
		l.t.Fatalf("list containers: %v", err)
	}
	return cs
}

// AssertNoStray fails if anything wearing ember's label is still there.
func (l *Lab) AssertNoStray() {
	l.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cs := l.Nodes(ctx)
	if len(cs) == 0 {
		return
	}
	var b strings.Builder
	for _, c := range cs {
		fmt.Fprintf(&b, "\n  %s  %s  %s  labels=%v", c.ID[:12], c.State, strings.Join(c.Names, ","), c.Labels)
	}
	l.t.Errorf("%d container(s) wearing ember's label survived the lab:%s"+
		"\nOn a rented GPU each of these would still be billing.", len(cs), b.String())
}

// Reap removes anything this lab left behind, so one failing lab does not
// cascade into every later one. It runs before AssertNoStray records the
// failure, never instead of it.
func (l *Lab) Reap() {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	for _, c := range l.Nodes(ctx) {
		_ = l.cli.ContainerRemove(ctx, c.ID, container.RemoveOptions{Force: true})
	}
}

// StateDir is where the journal lives.
func (l *Lab) StateDir() string { return filepath.Join(l.Dir, "state") }

// OutputDir is where fetched outputs land, one directory per run.
func (l *Lab) OutputDir() string { return filepath.Join(l.Dir, "outputs") }

// FetchedFiles lists every file under the output directory, relative to it.
//
// Flattened rather than returning the run directory, because the run
// directory's name carries a timestamp a lab should not have to predict.
func (l *Lab) FetchedFiles() []string {
	l.t.Helper()
	var out []string
	err := filepath.WalkDir(l.OutputDir(), func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(l.OutputDir(), p)
		if err != nil {
			return err
		}
		out = append(out, rel)
		return nil
	})
	if err != nil {
		l.t.Fatalf("walk the output directory: %v", err)
	}
	sort.Strings(out)
	return out
}

// Records returns the journal, read through statestore itself.
//
// Going through the real package rather than globbing a hand-spelled path is
// what keeps this honest. Two earlier versions got it wrong: one read a single
// nodes.json, the other read the state directory while records live in a
// "nodes" subdirectory of it. Both returned nothing, both reported an empty
// journal while ember was writing records perfectly well, and a lab asserting
// "a clean run leaves no records" passed without checking anything.
func (l *Lab) Records() []statestore.Record {
	l.t.Helper()
	store, err := statestore.Open(filepath.Join(l.StateDir(), statestore.DirName))
	if err != nil {
		l.t.Fatalf("open the journal: %v", err)
	}
	recs, err := store.List()
	if err != nil {
		l.t.Fatalf("read the journal: %v", err)
	}
	return recs
}

// RecordedNodeIDs are the provider node ids the journal knows about.
func (l *Lab) RecordedNodeIDs() []string {
	l.t.Helper()
	var out []string
	for _, r := range l.Records() {
		if h, ok := r.Handle(); ok {
			out = append(out, h.ID)
		}
	}
	return out
}

// WaitRecorded blocks until the journal holds a node id for some run.
//
// The labs that interrupt ember need this, or whether the record is complete
// depends on how fast the machine is and the lab asserts a different thing on
// a loaded CI runner than on a laptop. That is exactly what happened: the
// SIGKILL lab passed locally, where the kill landed before the id was
// written, and failed on CI, where it landed after.
func (l *Lab) WaitRecorded(within time.Duration) {
	l.t.Helper()
	deadline := time.Now().Add(within)
	for {
		if len(l.RecordedNodeIDs()) > 0 {
			return
		}
		if time.Now().After(deadline) {
			l.t.Fatalf("no node id reached the journal within %v; it holds %d record(s)", within, len(l.Records()))
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// FreePort asks the kernel for a port nothing is using.
//
// A hardcoded port would collide with whatever else a CI runner is doing, and
// the failure would read as "ember could not bind" rather than "two tests
// wanted the same number". There is a race between closing this listener and
// ember binding it; the alternative is a fixed port, whose collision window is
// the entire run rather than a microsecond.
func FreePort(t *testing.T) int {
	t.Helper()
	// Through loopback.Listen, not net.Listen. lab.go is a non-test file, so
	// TestNoListenOutsideLoopback covers it -- and it caught this exact line.
	// The right answer was to route it through the one sanctioned listener
	// rather than widen the guard: a rule with an exemption for the test
	// harness is a rule with an exemption for whoever needs one next.
	ln, err := loopback.Listen(context.Background(), loopback.Port(0))
	if err != nil {
		t.Fatalf("find a free port: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	if err := ln.Close(); err != nil {
		t.Fatalf("release the probe listener: %v", err)
	}
	return port
}

// WaitNode blocks until ember has created a node, or fails the lab.
func (l *Lab) WaitNode(ctx context.Context, within time.Duration) {
	l.t.Helper()
	deadline := time.Now().Add(within)
	for {
		if len(l.Nodes(ctx)) > 0 {
			return
		}
		if time.Now().After(deadline) {
			l.t.Fatalf("no node wearing ember's label appeared within %v", within)
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// WaitNoNodes blocks until nothing wearing ember's label is left.
//
// Separate from AssertNoStray because destroy is allowed to take time: it runs
// on a context that deliberately outlives an interrupt, with its own two
// minute budget, and asserting immediately after a signal would be a race the
// lab loses at random.
func (l *Lab) WaitNoNodes(ctx context.Context, within time.Duration, detail func() string) {
	l.t.Helper()
	deadline := time.Now().Add(within)
	for {
		if len(l.Nodes(ctx)) == 0 {
			return
		}
		if time.Now().After(deadline) {
			l.t.Fatalf("node(s) survived %v; on a rented GPU each would still be billing\n%s", within, detail())
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// ReadBanner dials a forwarded port and reads what the far end sends first.
//
// Used against a forward pointed at the node's own sshd: the banner is bytes
// that provably crossed the tunnel from inside the container, which a
// successful TCP accept on the local listener does not demonstrate. go-tun's
// lesson applies here -- a tunnel that passes a connect and drops payload
// passes a weaker test and fails in production.
func (l *Lab) ReadBanner(port int, within time.Duration) (string, error) {
	d := net.Dialer{Timeout: within}
	conn, err := d.Dial("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		return "", err
	}
	defer func() { _ = conn.Close() }()

	if err := conn.SetReadDeadline(time.Now().Add(within)); err != nil {
		return "", err
	}
	buf := make([]byte, 256)
	n, err := conn.Read(buf)
	if n > 0 {
		return strings.TrimSpace(string(buf[:n])), nil
	}
	return "", err
}

// NodeSpec describes a node the lab starts itself, bypassing ember.
//
// Used by the labs that need a node to still be there after ember has
// finished with it -- the host-key mismatch lab in particular, where the
// control under test only triggers on a second dial to the same node id, and
// ember's nodes are destroyed by design.
type NodeSpec struct {
	Name string

	// AuthorizedKey is the operator's public key, in authorized_keys form.
	AuthorizedKey string

	// HostKeyPEM is the private host key ember minted. It travels in the
	// Engine API's JSON body and never in argv, the same as in production:
	// /proc/<pid>/cmdline is world-readable.
	HostKeyPEM string

	// TTLSeconds arms the node's own self-destruct. Zero disarms it, which is
	// what a lab wants unless the self-destruct is the thing being tested.
	TTLSeconds int

	// EmberLabels, when set, makes the container look like one of ember's own
	// to the local provider -- so `ember ls` and `ember reap` classify it.
	// Deadline is encoded into the label, which lets a lab produce a node
	// that is already expired without waiting for a clock. The node's own
	// self-destruct stays disarmed unless TTLSeconds says otherwise, so reap
	// is the only thing that can end it and the lab tests reap rather than
	// racing it.
	EmberLabels bool

	// Deadline goes into the ember label when EmberLabels is set.
	Deadline time.Time

	// RunID goes into the ember label. Empty derives one from Name.
	RunID node.RunID
}

// StartNode runs the lab image directly and returns its loopback SSH address.
//
// The port is read back from the daemon rather than requested, because asking
// for host port 0 and then assuming you know which port you got is how a
// forward silently points at somebody else's service.
func (l *Lab) StartNode(ctx context.Context, spec NodeSpec) (id, addr string) {
	l.t.Helper()

	runID := spec.RunID
	if runID == "" {
		// The run id is a fixed-width hex token, so a lab name cannot be used
		// directly; derive one that still traces back to the lab.
		runID = node.RunID(fmt.Sprintf("%016x", crc64.Checksum([]byte(spec.Name), crc64.MakeTable(crc64.ECMA))))
	}

	env := []string{
		"EMBER_AUTHORIZED_KEY=" + spec.AuthorizedKey,
		"EMBER_HOST_KEY=" + spec.HostKeyPEM,
		"EMBER_RUN_ID=" + string(runID),
	}
	if spec.TTLSeconds > 0 {
		env = append(env, fmt.Sprintf("EMBER_TTL_SECONDS=%d", spec.TTLSeconds))
	}

	labels := map[string]string{dockerengine.LabelRunID: "lab-" + spec.Name}
	if spec.EmberLabels {
		deadline := spec.Deadline
		if deadline.IsZero() {
			deadline = time.Now().Add(time.Hour)
		}
		labels = map[string]string{
			dockerengine.LabelRunID: string(runID),
			local.LabelKind:         "node",
			local.LabelLabel:        provider.Label(runID, deadline),
			local.LabelUser:         "root",
		}
	}

	resp, err := l.cli.ContainerCreate(ctx,
		&container.Config{
			Image: LabImage,
			Env:   env,
			// Always labelled with ember's run-id key, so AssertNoStray
			// covers it and a lab that forgets to clean up fails rather
			// than leaking a container onto the developer's machine.
			Labels:       labels,
			ExposedPorts: nat.PortSet{"22/tcp": struct{}{}},
		},
		&container.HostConfig{
			// Loopback, not Docker's 0.0.0.0 default: a lab must not publish
			// a key-only sshd to the whole LAN for the length of a test run.
			PortBindings: nat.PortMap{"22/tcp": []nat.PortBinding{{HostIP: "127.0.0.1"}}},
		},
		nil, nil, "")
	if err != nil {
		l.t.Fatalf("create node container: %v", err)
	}
	id = resp.ID
	l.t.Cleanup(func() {
		_ = l.cli.ContainerRemove(context.Background(), id, container.RemoveOptions{Force: true})
	})

	if err := l.cli.ContainerStart(ctx, id, container.StartOptions{}); err != nil {
		l.t.Fatalf("start node container: %v", err)
	}

	j, err := l.cli.ContainerInspect(ctx, id)
	if err != nil {
		l.t.Fatalf("inspect node container: %v", err)
	}
	bs := j.NetworkSettings.Ports["22/tcp"]
	if len(bs) == 0 {
		l.t.Fatalf("the daemon published nothing for 22/tcp on %s", id[:12])
	}
	return id, net.JoinHostPort("127.0.0.1", bs[0].HostPort)
}

// RotateHostKey replaces the node's host key and restarts sshd.
//
// This is what an attacker substituting a machine looks like from the client's
// side, and it is the only way to make a pin mismatch happen on purpose.
func (l *Lab) RotateHostKey(ctx context.Context, id string) {
	l.t.Helper()
	l.exec(ctx, id, "rm", "-f", "/etc/ssh/ssh_host_ed25519_key", "/etc/ssh/ssh_host_ed25519_key.pub")
	l.exec(ctx, id, "ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", "/etc/ssh/ssh_host_ed25519_key")
	// sshd reads its host keys at startup, so the new key only takes effect
	// once the running daemon is replaced.
	l.exec(ctx, id, "pkill", "-HUP", "sshd")
}

// Exec runs a command inside a node container over the Docker API.
//
// Out-of-band on purpose: a lab seeding files on a node should not go through
// ember's own SSH path, both because that is the thing under test and because
// widening sshx.Runner with stdin support to let a test write a file would be
// a production interface shaped by a test's convenience.
func (l *Lab) Exec(ctx context.Context, id string, args ...string) {
	l.t.Helper()
	l.exec(ctx, id, args...)
}

// ExecOut runs a command in a node container and returns its stdout.
func (l *Lab) ExecOut(ctx context.Context, id string, args ...string) string {
	l.t.Helper()
	return l.execOut(ctx, id, args...)
}

func (l *Lab) exec(ctx context.Context, id string, args ...string) {
	l.t.Helper()
	l.execOut(ctx, id, args...)
}

func (l *Lab) execOut(ctx context.Context, id string, args ...string) string {
	l.t.Helper()
	ex, err := l.cli.ContainerExecCreate(ctx, id, container.ExecOptions{
		Cmd: args, AttachStdout: true, AttachStderr: true,
	})
	if err != nil {
		l.t.Fatalf("exec create %v: %v", args, err)
	}
	_ = ex
	att, err := l.cli.ContainerExecAttach(ctx, ex.ID, container.ExecAttachOptions{})
	if err != nil {
		l.t.Fatalf("exec attach %v: %v", args, err)
	}
	var out, errb bytes.Buffer
	_, _ = stdcopy.StdCopy(&out, &errb, att.Reader)
	att.Close()

	ins, err := l.cli.ContainerExecInspect(ctx, ex.ID)
	if err != nil {
		l.t.Fatalf("exec inspect %v: %v", args, err)
	}
	if ins.ExitCode != 0 {
		l.t.Fatalf("exec %v exit %d: %s%s", args, ins.ExitCode, out.String(), errb.String())
	}
	return out.String()
}

// WaitSSH waits for a completed SSH handshake, not merely a TCP accept.
//
// sshd binds its listener before it is ready to negotiate, so a lab that
// waited on a successful dial would race the daemon and fail at random in the
// code under test rather than here.
func (l *Lab) WaitSSH(addr string, within time.Duration) {
	l.t.Helper()
	deadline := time.Now().Add(within)
	for {
		b, err := l.readBannerAt(addr, 2*time.Second)
		if err == nil && strings.Contains(b, "SSH-2.0") {
			return
		}
		if time.Now().After(deadline) {
			l.t.Fatalf("no sshd banner from %s within %v (last: %q, %v)", addr, within, b, err)
		}
		time.Sleep(250 * time.Millisecond)
	}
}

func (l *Lab) readBannerAt(addr string, within time.Duration) (string, error) {
	d := net.Dialer{Timeout: within}
	conn, err := d.Dial("tcp", addr)
	if err != nil {
		return "", err
	}
	defer func() { _ = conn.Close() }()
	if err := conn.SetReadDeadline(time.Now().Add(within)); err != nil {
		return "", err
	}
	buf := make([]byte, 256)
	n, err := conn.Read(buf)
	if n > 0 {
		return strings.TrimSpace(string(buf[:n])), nil
	}
	return "", err
}

// WaitExited blocks until the container is no longer running.
func (l *Lab) WaitExited(ctx context.Context, id string, within time.Duration) {
	l.t.Helper()
	deadline := time.Now().Add(within)
	for {
		j, err := l.cli.ContainerInspect(ctx, id)
		if err != nil {
			l.t.Fatalf("inspect %s: %v", id[:12], err)
		}
		if !j.State.Running {
			return
		}
		if time.Now().After(deadline) {
			l.t.Fatalf("container %s was still running after %v; its self-destruct did nothing", id[:12], within)
		}
		time.Sleep(500 * time.Millisecond)
	}
}
