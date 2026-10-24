package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/Legion112/ember/internal/config"
	"github.com/Legion112/ember/internal/dockerengine"
	"github.com/Legion112/ember/internal/dockerengine/dockerclient"
	"github.com/Legion112/ember/internal/hostkey"
	"github.com/Legion112/ember/internal/keys"
	"github.com/Legion112/ember/internal/node"
	"github.com/Legion112/ember/internal/provider"
	"github.com/Legion112/ember/internal/provider/local"
	"github.com/Legion112/ember/internal/provider/runpod"
	"github.com/Legion112/ember/internal/provision"
	"github.com/Legion112/ember/internal/session"
	"github.com/Legion112/ember/internal/sshx"
	"github.com/Legion112/ember/internal/sshx/sshexec"
	"github.com/Legion112/ember/internal/statestore"
	"github.com/Legion112/ember/internal/transfer"
	"github.com/Legion112/ember/internal/tunnel/sshdial"
)

// deps are the collaborators cmdUp needs, injected so the command is testable
// without a Docker daemon or a real home directory.
type deps struct {
	Engine   dockerengine.API
	ConfigIn string
	HomeDir  string
	Signals  []int

	// Creds overrides what is read from the environment and from disk, so a
	// test can register a cloud provider without a real token and without
	// touching the operator's credentials file.
	Creds *config.Credentials

	// CloudBaseURL redirects every cloud provider's API.
	//
	// Without it a test that registers RunPod with a fake token talks to
	// api.runpod.io for real: slow, dependent on somebody's network, and in
	// CI a stream of authentication failures against a live service. The
	// first version of the test below did exactly that, and the comment
	// claiming otherwise was the only thing standing in for this field.
	CloudBaseURL string
}

// credentials resolves this run's provider tokens.
func (d deps) credentials(configDir string) (config.Credentials, error) {
	if d.Creds != nil {
		return *d.Creds, nil
	}
	return config.LoadCredentials(configDir)
}

func cmdUp(args []string, out io.Writer) error { return runUp(args, out, deps{}) }

func runUp(args []string, out io.Writer, d deps) error {
	fs := flag.NewFlagSet("up", flag.ContinueOnError)
	fs.SetOutput(out)

	var (
		providerName = fs.String("provider", "", "which provider to rent from")
		image        = fs.String("image", "", "workload image, pinned to a tag or digest")
		ttl          = fs.Duration("ttl", 0, "how long the node may live before it destroys itself")
		gpu          = fs.String("gpu", "", "GPU model to require, for example rtx4090")
		maxHourly    = fs.String("max-hourly-usd", "", "refuse any offer dearer than this per hour")
		cfgPath      = fs.String("config", config.FileName, "path to ember.yaml")
		dryRun       = fs.Bool("dry-run", false, "say what would be rented and create nothing")
		holdFor      = fs.Duration("hold", 0, "stop after this long instead of waiting for Ctrl-C")
	)
	var ports multiFlag
	fs.Var(&ports, "port", "forward a port, as name:remote or name:remote:local (repeatable)")

	fs.Usage = func() {
		outf(out, "ember up rents a node, forwards its ports to loopback, and destroys it.\n\n")
		outf(out, "Every forwarded port is bound to 127.0.0.1 and reached over SSH.\nNothing ember creates is reachable from the internet.\n\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return usageError{err}
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	applyFlags(&cfg, *providerName, *image, *gpu, *maxHourly, *ttl, ports)

	spec, err := buildSpec(cfg)
	if err != nil {
		return usageError{err}
	}

	paths, err := resolvePaths(cfg, d)
	if err != nil {
		return err
	}

	creds, err := d.credentials(paths.config)
	if err != nil {
		return err
	}
	reg, err := buildRegistry(d, creds, cfg)
	if err != nil {
		return err
	}
	p, err := reg.Get(node.ProviderID(cfg.Provider))
	if err != nil {
		return usageError{err}
	}

	opts, err := buildOptions(paths, cfg, out)
	if err != nil {
		return err
	}

	if *dryRun {
		return dryRunUp(fs.Output(), p, spec, opts)
	}

	// The first Ctrl-C unwinds through WithNode's deferred destroy, which
	// runs on a context that survives this cancellation. A second one is the
	// operator's choice, and the state record is already on disk by then.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	return provision.WithNode(ctx, p, spec, opts, func(ctx context.Context, s provision.Session) error {
		return serveSession(ctx, out, s, opts, spec, *holdFor, paths.outputs)
	})
}

// serveSession opens the tunnel and holds it until the operator stops or the
// node's deadline arrives. The decisions it used to make inline -- when to
// retry a dial, when to refuse one -- live in internal/session, where they
// can be tested against an in-process SSH server instead of a rented machine.
func serveSession(ctx context.Context, out io.Writer, s provision.Session, opts provision.Options, spec node.Spec, hold time.Duration, outDir string) error {
	signers := []ssh.Signer{opts.Identity.Signer}
	if agentSigners, err := keys.AgentSigners(); err == nil {
		signers = append(signers, agentSigners...)
	}

	so := session.Options{Log: opts.Log, Hold: hold}
	client, err := session.Dial(ctx, sshdial.Target{
		NodeID:   s.Node.Handle().ID,
		Endpoint: s.Node.Endpoint(),
	}, s.Trust, signers, so)
	if err != nil {
		return err
	}
	defer func() { _ = client.Close() }()

	serveErr := session.Serve(ctx, out, client, session.Forwards(spec.Ports), so)

	// Outputs are fetched even when the session ended badly, and on a context
	// that survives the operator's Ctrl-C. The node is about to be destroyed
	// either way: whatever it produced is about to stop existing, and a run
	// that failed halfway is often exactly the one whose partial output and
	// logs are worth having.
	fetchCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), outputTimeout)
	defer cancel()

	if err := fetchOutputs(fetchCtx, out, sshexec.New(client.SSH()), s.RunID, spec.Outputs, outDir); err != nil {
		if serveErr != nil {
			return errors.Join(serveErr, err)
		}
		return err
	}
	return serveErr
}

// outputTimeout bounds the output fetch.
//
// Generous, because a run of generated video is gigabytes over a home upload
// link, and ungenerous enough that a wedged transfer cannot hold the node --
// and the bill -- open indefinitely.
const outputTimeout = 30 * time.Minute

// fetchOutputs brings the run's outputs home.
//
// It takes the sshx.Runner seam rather than the SSH client, so the reporting
// and the error handling here are testable against a recording runner instead
// of needing a node.
func fetchOutputs(ctx context.Context, out io.Writer, r sshx.Runner, runID node.RunID, outputs []node.OutputPath, base string) error {
	if len(outputs) == 0 {
		return nil
	}

	dir := transfer.RunDir(base, runID, time.Now())
	outf(out, "fetching %d output path(s) into %s\n", len(outputs), dir)

	res, err := transfer.Fetch(ctx, r, outputs, dir)

	// What arrived is reported even when something failed, because "3 of 4
	// paths, 1.2 GiB" is what tells the operator whether the run is worth
	// keeping.
	outf(out, "fetched %d file(s), %s\n", res.Files, humanBytes(res.Bytes))
	if err != nil {
		return fmt.Errorf("fetch outputs into %s: %w", dir, err)
	}
	return nil
}

// humanBytes is for one line of operator-facing output.
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	v, exp := float64(n), 0
	for v >= unit && exp < 4 {
		v /= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", v, "KMGT"[exp-1])
}

func dryRunUp(out io.Writer, p provider.Provider, spec node.Spec, opts provision.Options) error {
	offer, err := provision.PickOffer(context.Background(), p, spec, opts)
	if err != nil {
		return err
	}
	outf(out, "dry-run: would rent %s from %s at %v for %v\n", offer.Token, p.Name(), offer.Price, spec.TTL)
	outf(out, "dry-run: would run %s and forward %d port(s)\n", spec.Image, len(spec.Ports))
	outf(out, "dry-run: nothing was created\n")
	return nil
}

type paths struct{ config, state, outputs string }

func resolvePaths(cfg config.Config, d deps) (paths, error) {
	if d.HomeDir != "" {
		return paths{
			config:  filepath.Join(d.HomeDir, "config"),
			state:   filepath.Join(d.HomeDir, "state"),
			outputs: filepath.Join(d.HomeDir, "outputs"),
		}, nil
	}
	c, err := cfg.ConfigDirOrDefault()
	if err != nil {
		return paths{}, err
	}
	s, err := cfg.StateDirOrDefault()
	if err != nil {
		return paths{}, err
	}
	o, err := cfg.OutputDirOrDefault()
	if err != nil {
		return paths{}, err
	}
	return paths{config: c, state: s, outputs: o}, nil
}

// buildRegistry registers every provider this run can reach.
//
// A provider with no credential is left out rather than registered and left
// to fail: `ember ls` reports an unreachable provider as a finding and exits
// 2, so registering RunPod on a machine that has never had a token would make
// every clean run look like a problem. The absence is reported by `ember
// offers` and by a named-provider lookup instead, where it is actionable.
func buildRegistry(d deps, creds config.Credentials, cfg config.Config) (*provider.Registry, error) {
	engine := d.Engine
	if engine == nil {
		c, err := dockerclient.NewClient()
		if err != nil {
			return nil, err
		}
		engine = c
	}
	reg := provider.NewRegistry()
	if err := reg.Add(local.New(engine)); err != nil {
		return nil, err
	}

	if !creds.RunPod.IsZero() {
		rp := runpod.New(creds.RunPod)
		if d.CloudBaseURL != "" {
			rp.HTTP.BaseURL = d.CloudBaseURL
		}
		rp.Cloud = cfg.RunPodCloud
		rp.DataCenters = cfg.DataCenters
		rp.DiskGiB = cfg.DiskGiB
		if err := reg.Add(rp); err != nil {
			return nil, err
		}
	}
	return reg, nil
}

func buildOptions(ps paths, cfg config.Config, out io.Writer) (provision.Options, error) {
	store, err := openStore(ps.state)
	if err != nil {
		return provision.Options{}, err
	}
	pins, err := hostkey.OpenStore(filepath.Join(ps.state, "known_hosts.json"))
	if err != nil {
		return provision.Options{}, err
	}
	id, err := keys.EnsureIdentity(ps.config)
	if err != nil {
		return provision.Options{}, err
	}
	hourly, run, err := cfg.Budgets()
	if err != nil {
		return provision.Options{}, usageError{err}
	}
	return provision.Options{
		Store:    store,
		Pins:     pins,
		Identity: id,
		Budget:   provision.Budget{MaxRate: hourly, MaxRun: run},
		Log:      slog.New(slog.NewTextHandler(out, &slog.HandlerOptions{Level: slog.LevelInfo})),
	}, nil
}

func applyFlags(cfg *config.Config, providerName, image, gpu, maxHourly string, ttl time.Duration, ports multiFlag) {
	if providerName != "" {
		cfg.Provider = providerName
	}
	if image != "" {
		cfg.Image = image
	}
	if gpu != "" {
		cfg.GPU = gpu
	}
	if maxHourly != "" {
		cfg.MaxHourlyUSD = maxHourly
	}
	if ttl > 0 {
		cfg.TTL = ttl
	}
	if len(ports) > 0 {
		cfg.Ports = ports
	}
}

func buildSpec(cfg config.Config) (node.Spec, error) {
	ports, err := cfg.ParsePorts()
	if err != nil {
		return node.Spec{}, err
	}
	filter, err := cfg.Filter()
	if err != nil {
		return node.Spec{}, err
	}
	outputs, err := cfg.ParseOutputs()
	if err != nil {
		return node.Spec{}, err
	}
	if cfg.Image == "" {
		return node.Spec{}, fmt.Errorf("no image: pass -image or set it in %s", config.FileName)
	}
	return node.NewSpec(node.SpecOptions{
		Image:   cfg.Image,
		Ports:   ports,
		Filter:  filter,
		TTL:     cfg.TTL,
		Outputs: outputs,
	})
}

// multiFlag collects a repeatable string flag.
type multiFlag []string

func (m *multiFlag) String() string     { return fmt.Sprint([]string(*m)) }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }

// openStore is a thin wrapper so cmd_up does not import statestore twice.
func openStore(dir string) (*statestore.Store, error) {
	return statestore.Open(filepath.Join(dir, statestore.DirName))
}
