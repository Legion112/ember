package sdserver

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path"
	"slices"
	"strings"
	"time"

	"github.com/Legion112/ember/internal/httpx"
)

// Port is where sd-server listens on the node, and it is a constant rather
// than a setting.
//
// Nothing binds locally: ember reaches it by opening a direct-tcpip channel
// to the node's own loopback, so there is no port on the operator's machine
// to collide with and nothing to configure. A knob here could only ever point
// ember at a different port on the same node, which is a way to get it wrong
// and not a capability.
const Port = 1234

// DefaultModelTimeout is how long the weights have to become loadable.
//
// Longer than gpucheck's two minutes because the thing being waited for is
// bigger: FLUX.2 is 56 GiB read off a network volume. Measured at about a
// gigabyte a second, that is a minute for the read alone before anything is
// resident.
const DefaultModelTimeout = 10 * time.Minute

// ModelPoll is how often the model status is asked for while it loads.
//
// Fixed, not backoff, for gpucheck's reason: the window is tens of seconds to
// a couple of minutes, and backoff spends most of a budget that size asleep.
const ModelPoll = 2 * time.Second

// Client talks to one sd-server.
//
// It holds an httpx.Client because that is the house seam -- BaseURL plus an
// *http.Client is what the provider tests point at an httptest.Server. The
// base URL is plain HTTP to loopback, which httpx permits precisely because
// the alternative is TLS to a port on the far end of an SSH channel, where
// the channel is already the authenticated transport.
type Client struct {
	HTTP httpx.Client
}

// New builds a client that reaches sd-server through dial.
//
// dial is sshdial.Client.DialContext in production: a direct-tcpip channel
// straight to the node's loopback. No local listener is opened, which is why
// this needs no `ports:` entry in a config and cannot trip the ban on
// net.Listen outside internal/loopback.
func New(dial func(ctx context.Context, network, addr string) (net.Conn, error)) Client {
	return Client{HTTP: httpx.Client{
		BaseURL: fmt.Sprintf("http://127.0.0.1:%d", Port),
		HTTP: &http.Client{
			// No timeout on the client itself: a generation is minutes and
			// the per-request deadline belongs to the caller's context, which
			// is the only thing that knows whether this is a status poll or a
			// result that took four minutes to produce.
			Transport: &http.Transport{DialContext: dial},
		},
	}}
}

// Capabilities reports what this server build can do.
func (c Client) Capabilities(ctx context.Context) (Capabilities, error) {
	var w wireCapabilities
	if err := c.HTTP.Get(ctx, pathCapabilities, &w); err != nil {
		return Capabilities{}, fmt.Errorf("read sd-server capabilities: %w", err)
	}
	return mapCapabilities(w), nil
}

// ModelStatus reports whether the weights are loaded.
func (c Client) ModelStatus(ctx context.Context) (ModelStatus, error) {
	var w wireModelStatus
	if err := c.HTTP.Get(ctx, pathModelStatus, &w); err != nil {
		return ModelStatus{}, fmt.Errorf("read sd-server model status: %w", err)
	}
	st, err := mapModelStatus(w)
	if err != nil {
		return ModelStatus{}, fmt.Errorf("read sd-server model status: %w", err)
	}
	return st, nil
}

// Submit queues one job and returns its id.
//
// NOT RETRYABLE, and nothing in this package or httpx will retry it. A
// transport failure here is ambiguous: the request may have reached the server
// and created a job whose id ember never learned. Sending it again would
// generate the image twice and bill for both, and the second one would be
// invisible -- ember would be polling the first id while the second ran. The
// same reasoning as running destroy on a context that survives Ctrl-C: the
// expensive mistake is the one you cannot see.
func (c Client) Submit(ctx context.Context, j Job, preview string, interval int) (string, error) {
	body, err := j.Body(preview, interval)
	if err != nil {
		return "", err
	}
	var w wireAccepted
	if err := c.HTTP.Post(ctx, pathImageGen, body, &w); err != nil {
		return "", fmt.Errorf("submit job to sd-server: %w", err)
	}
	if w.ID == "" {
		// A job that was accepted and whose id did not come back is a job
		// ember cannot poll, cancel or collect -- it will run on a card ember
		// is paying for and the result will be purged. Same shape as a store
		// created without its label coming back.
		return "", errors.New("sd-server accepted the job without returning an id, " +
			"so it cannot be polled or collected")
	}
	return w.ID, nil
}

// Poll reads one job's current state.
func (c Client) Poll(ctx context.Context, id string) (State, error) {
	if id == "" {
		return State{}, errors.New("no job id to poll")
	}
	var w wireJob
	if err := c.HTTP.Get(ctx, pathJobs+id, &w); err != nil {
		return State{}, fmt.Errorf("poll sd-server job %s: %w", id, err)
	}
	st, err := mapJob(w)
	if err != nil {
		return State{}, fmt.Errorf("poll sd-server job %s: %w", id, err)
	}
	return st, nil
}

// Cancel asks the server to stop a job.
func (c Client) Cancel(ctx context.Context, id string) error {
	if id == "" {
		return errors.New("no job id to cancel")
	}
	if err := c.HTTP.Post(ctx, pathJobs+id+"/cancel", struct{}{}, nil); err != nil {
		return fmt.Errorf("cancel sd-server job %s: %w", id, err)
	}
	return nil
}

// NotReadyError is the model never becoming loadable.
//
// A type rather than a formatted string, for gpucheck.NotReadyError's reason:
// a caller can tell this apart from everything else that goes wrong between
// renting and generating. This one means the weights are the problem, and the
// node is about to be destroyed with nothing produced.
type NotReadyError struct {
	Last    ModelState
	Waited  time.Duration
	Polls   int
	Message string
}

func (e *NotReadyError) Error() string {
	s := fmt.Sprintf("sd-server never loaded the weights: last state %q after %d poll(s) over %v.\n",
		e.Last, e.Polls, e.Waited)
	s += "Nothing has been generated, so this run has cost the wait and no more.\n"
	if e.Message != "" {
		s += "  the server said: " + e.Message + "\n"
	}
	s += "Check the paths in sd.serve against what is actually on the store: " +
		"a model file that is not there reads exactly like this."
	return s
}

// ModelStatuser is the one method WaitModel needs.
//
// A narrow interface rather than the concrete Client, for the reason
// gpucheck's scriptedRunner exists: the behaviour worth testing here is time
// -- a load that says "loading" four times and then "ready" -- and the only
// way to assert that without waiting for it in real seconds is
// testing/synctest. A synctest bubble advances its clock when every goroutine
// in it is durably blocked, and a goroutine waiting on a real socket never
// is, so an httptest.Server inside a bubble deadlocks rather than passing.
// Found by writing it that way first and watching the test time out after ten
// minutes.
type ModelStatuser interface {
	ModelStatus(ctx context.Context) (ModelStatus, error)
}

// WaitModel blocks until the weights are loaded, and reports how long the
// server says that took.
//
// The shape is gpucheck.Wait's, because the situation is the same: a thing
// that is legitimately "not yet" for a while and must not be refused early.
// The deadline is checked after an attempt rather than before, so there is
// always at least one; the final sleep is clamped to what is left of the
// budget rather than overshooting it; and the operator gets one line, not one
// per poll.
//
// ModelError is a refusal and not a retry. A load that failed has already read
// what it was going to read -- waiting changes nothing and costs the whole
// timeout on a card that will never generate anything.
func WaitModel(ctx context.Context, c ModelStatuser, timeout time.Duration, out io.Writer, log *slog.Logger) (time.Duration, error) {
	if timeout <= 0 {
		timeout = DefaultModelTimeout
	}
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}

	start := time.Now()
	deadline := start.Add(timeout)

	var (
		polls int
		last  ModelState
		msg   string
	)

	for {
		polls++
		st, err := c.ModelStatus(ctx)
		switch {
		case err != nil:
			// A status ember could not read is not a model that failed to
			// load: sd-server comes up and starts answering before it has
			// finished reading the weights, and the window in between looks
			// exactly like this. Retried.
			msg = err.Error()
		case st.State == ModelReady:
			if polls > 1 {
				say(out, "the weights loaded in %v (sd-server reports %v)\n",
					time.Since(start).Round(time.Second), st.LoadElapsed)
			}
			log.Debug("sd-server model ready", "polls", polls, "load_elapsed", st.LoadElapsed)
			return st.LoadElapsed, nil
		case st.State == ModelError:
			return 0, &NotReadyError{Last: st.State, Waited: time.Since(start).Round(time.Second), Polls: polls, Message: st.Err}
		default:
			last, msg = st.State, st.Err
		}

		log.Debug("sd-server model not ready", "poll", polls, "state", last, "err", msg)

		remaining := time.Until(deadline)
		if remaining <= 0 {
			return 0, &NotReadyError{Last: last, Waited: time.Since(start).Round(time.Second), Polls: polls, Message: msg}
		}
		wait := ModelPoll
		if remaining < wait {
			wait = remaining
		}
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-time.After(wait):
		}
	}
}

// Validate checks a job against what the server says it can do.
//
// The cheapest refusal in the feature, and the only reason Capabilities is in
// this client at all. A mistyped sampler discovered at submission time is
// discovered after the card was rented, the image pulled, the weights
// verified and loaded -- and in a batch, after however many jobs ran before
// the bad one. Checked for every job before the first is submitted.
//
// An empty list from the server is not a licence to accept anything: it means
// this build did not report that capability, and ember says so rather than
// guessing. The bounds are only applied when the server published them,
// because a zero maximum is "not reported" and not "nothing is allowed".
func Validate(j Job, caps Capabilities) error {
	if strings.TrimSpace(j.Prompt) == "" && j.Extra["prompt"] == nil {
		return errors.New("no prompt")
	}
	if j.Sampler != "" {
		if len(caps.Samplers) == 0 {
			return fmt.Errorf("sampler %q cannot be checked: the server reported no sampler list", j.Sampler)
		}
		if !slices.Contains(caps.Samplers, j.Sampler) {
			return fmt.Errorf("sampler %q is not one this server has (%s)", j.Sampler, strings.Join(caps.Samplers, ", "))
		}
	}
	if j.Scheduler != "" {
		if len(caps.Schedulers) == 0 {
			return fmt.Errorf("scheduler %q cannot be checked: the server reported no scheduler list", j.Scheduler)
		}
		if !slices.Contains(caps.Schedulers, j.Scheduler) {
			return fmt.Errorf("scheduler %q is not one this server has (%s)", j.Scheduler, strings.Join(caps.Schedulers, ", "))
		}
	}
	if err := within("width", j.Width, caps.Limits.MinWidth, caps.Limits.MaxWidth); err != nil {
		return err
	}
	if err := within("height", j.Height, caps.Limits.MinHeight, caps.Limits.MaxHeight); err != nil {
		return err
	}
	if j.BatchCount != 0 && caps.Limits.MaxBatchCount != 0 && j.BatchCount > caps.Limits.MaxBatchCount {
		return fmt.Errorf("batch_count %d is above this server's maximum of %d", j.BatchCount, caps.Limits.MaxBatchCount)
	}
	if j.Steps < 0 {
		return fmt.Errorf("steps %d is negative", j.Steps)
	}
	if err := validateParams(j.Params, caps); err != nil {
		return err
	}
	return nil
}

// validateParams checks the optional parameters against the server's lists.
//
// Both names here are checked against a list the server published rather than
// against a pattern, which is the allow-list rule and not merely tidiness for
// the upscaler: api.md resolves a model-backed upscaler as
// `--hires-upscalers-dir / (name + ext)`, so the name reaches a path join on
// the node. "Not in the list the server gave us" is a refusal ember can make
// without knowing how sd-server handles a hostile name -- which nothing here
// has evidence about either way.
//
// An empty list is "this build did not report that capability" and refuses,
// as it does for samplers. Guessing in either direction is how a name nobody
// checked reaches a path join.
func validateParams(p Params, caps Capabilities) error {
	if p.OutputFormat != "" {
		if len(caps.OutputFormats) == 0 {
			return fmt.Errorf("output_format %q cannot be checked: the server reported no format list", p.OutputFormat)
		}
		if !slices.Contains(caps.OutputFormats, p.OutputFormat) {
			return fmt.Errorf("output_format %q is not one this server has (%s)", p.OutputFormat, strings.Join(caps.OutputFormats, ", "))
		}
	}
	if p.Hires.Upscaler != "" {
		if len(caps.Upscalers) == 0 {
			return fmt.Errorf("hires upscaler %q cannot be checked: the server reported no upscaler list", p.Hires.Upscaler)
		}
		if !slices.Contains(caps.Upscalers, p.Hires.Upscaler) {
			return fmt.Errorf("hires upscaler %q is not one this server has (%s)", p.Hires.Upscaler, strings.Join(caps.Upscalers, ", "))
		}
	}
	if err := within("hires target_width", p.Hires.TargetWidth, caps.Limits.MinWidth, caps.Limits.MaxWidth); err != nil {
		return err
	}
	if err := within("hires target_height", p.Hires.TargetHeight, caps.Limits.MinHeight, caps.Limits.MaxHeight); err != nil {
		return err
	}
	return nil
}

// ValidateStatic is every refusal that needs no server.
//
// IT EXISTS BECAUSE THERE ARE TWO MOMENTS to refuse a job and only one of them
// is cheap. Validate above needs the server's own capability list, which only
// exists once a node is up and the weights are loaded -- about a minute of a
// rented card. Everything checkable without that is checkable the instant the
// job is written: a key in extra that shadows a seed, an output name that
// cleans away to nothing, a negative preview interval.
//
// One function so the refusals do not exist twice. config.ParseSD calls it for
// a file and the queue calls it for a request, and a config error that was
// caught for one spelling and not the other would be the worse half of having
// both.
func ValidateStatic(j Job, preview string, interval int) error {
	// Building the body IS the check: it is where a key in extra that
	// shadows a typed field is refused.
	if _, err := j.Body(preview, interval); err != nil {
		return err
	}
	if err := staticParams(j.Params); err != nil {
		return err
	}
	return ValidateOutput(j.Output)
}

// staticParams refuses the optional parameters that are wrong on their face.
//
// None of these needs a server, so none of them should cost a rental to
// discover. The bounds are the ones upstream's own defaults sit inside --
// denoising 0.7, slg layers 0.01 to 0.2, compression 100 -- and a value
// outside them is a typo rather than a preference. A negative value is the
// case that matters: upstream clamps a non-positive preview_interval to 1,
// so somebody writing a negative number to mean "off" buys the most
// expensive setting there is, and the same habit applied to a scale or a
// strength is worth refusing before it is applied to something that bills.
func staticParams(p Params) error {
	// -1 is the model's own default and the only negative with a meaning.
	if p.ClipSkip != nil && *p.ClipSkip < -1 {
		return fmt.Errorf("clip_skip %d is below -1, which is the model's own default", *p.ClipSkip)
	}
	if err := fraction("slg layer_start", p.SLG.LayerStart); err != nil {
		return err
	}
	if err := fraction("slg layer_end", p.SLG.LayerEnd); err != nil {
		return err
	}
	if p.SLG.LayerEnd != 0 && p.SLG.LayerStart > p.SLG.LayerEnd {
		return fmt.Errorf("slg layer_start %g is after layer_end %g, so the range is empty",
			p.SLG.LayerStart, p.SLG.LayerEnd)
	}
	for _, l := range p.SLG.Layers {
		if l < 0 {
			return fmt.Errorf("slg layer %d is negative", l)
		}
	}
	if p.Hires.Scale < 0 {
		return fmt.Errorf("hires scale %g is negative", p.Hires.Scale)
	}
	if p.Hires.Steps < 0 {
		return fmt.Errorf("hires steps %d is negative", p.Hires.Steps)
	}
	if err := fraction("hires denoising_strength", p.Hires.DenoisingStrength); err != nil {
		return err
	}
	if p.Hires.UpscaleTileSize < 0 {
		return fmt.Errorf("hires upscale_tile_size %d is negative", p.Hires.UpscaleTileSize)
	}
	if p.VAETiling.TileSizeW < 0 || p.VAETiling.TileSizeH < 0 {
		return fmt.Errorf("vae tiling tile size %dx%d is negative",
			p.VAETiling.TileSizeW, p.VAETiling.TileSizeH)
	}
	if err := fraction("vae tiling target_overlap", p.VAETiling.TargetOverlap); err != nil {
		return err
	}
	if err := fraction("vae tiling rel_size_w", p.VAETiling.RelSizeW); err != nil {
		return err
	}
	if err := fraction("vae tiling rel_size_h", p.VAETiling.RelSizeH); err != nil {
		return err
	}
	if p.OutputCompression < 0 || p.OutputCompression > 100 {
		return fmt.Errorf("output_compression %d is outside 0 to 100", p.OutputCompression)
	}
	return nil
}

// fraction refuses a value outside 0 to 1, where 0 means unset.
func fraction(name string, v float64) error {
	if v < 0 || v > 1 {
		return fmt.Errorf("%s %g is outside 0 to 1", name, v)
	}
	return nil
}

func within(name string, v, minimum, maximum int) error {
	if v == 0 {
		return nil
	}
	if v < 0 {
		return fmt.Errorf("%s %d is negative", name, v)
	}
	if minimum != 0 && v < minimum {
		return fmt.Errorf("%s %d is below this server's minimum of %d", name, v, minimum)
	}
	if maximum != 0 && v > maximum {
		return fmt.Errorf("%s %d is above this server's maximum of %d", name, v, maximum)
	}
	return nil
}

// Written is what WriteImages put on disk.
type Written struct {
	Files int
	Bytes int64

	// Each is what was written, in order.
	//
	// It exists so a caller recording where the bytes went does not have to
	// re-derive the naming. Deriving it a second time would be a second
	// spelling of safeStem, and the two would drift the first time a stem is
	// sanitised differently -- leaving a database pointing at filenames that
	// are not the ones on disk.
	Each []WrittenFile
}

// WrittenFile is one file and its length.
type WrittenFile struct {
	Name  string
	Bytes int64
}

// WriteImages writes a job's finished images under root.
//
// Through an *os.Root even though the names come from ember's own config
// rather than from a tar stream a rented machine produced. It costs nothing
// and it means there is one containment discipline in the program rather than
// one for untrusted names and a different one for names that are currently
// trusted.
//
// The stem is sanitised rather than refused, because it is an operator's
// filename and the useful behaviour is to write the file. A stem that cleans
// away to nothing is refused: that is a config error, not a name.
func WriteImages(root *os.Root, stem string, imgs []Image) (Written, error) {
	if len(imgs) == 0 {
		return Written{}, nil
	}
	name := safeStem(stem)
	if name == "" {
		return Written{}, fmt.Errorf("output name %q is not usable as a filename", stem)
	}

	var w Written
	for _, img := range imgs {
		// Numbered even when there is one, so a batch_count of four and a
		// batch_count of one produce the same shape of filename and a script
		// reading the directory does not need two cases.
		f := fmt.Sprintf("%s-%02d.png", name, img.Index)
		fh, err := root.OpenFile(f, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
		if err != nil {
			return w, fmt.Errorf("create %s: %w", f, err)
		}
		n, err := fh.Write(img.Data)
		cerr := fh.Close()
		if err != nil {
			return w, fmt.Errorf("write %s: %w", f, err)
		}
		if cerr != nil {
			return w, fmt.Errorf("close %s: %w", f, cerr)
		}
		w.Files++
		w.Bytes += int64(n)
		w.Each = append(w.Each, WrittenFile{Name: f, Bytes: int64(n)})
	}
	return w, nil
}

// ValidateOutput refuses an output name that is not usable as a filename.
//
// Checked at config time and not only when the image comes back, because the
// alternative is discovering it after the generation it names has been paid
// for. An empty name is fine -- the caller numbers jobs that do not name
// themselves -- but a name that cleans away to nothing is a typo.
func ValidateOutput(stem string) error {
	if strings.TrimSpace(stem) == "" {
		return nil
	}
	if safeStem(stem) == "" {
		return fmt.Errorf("output %q is not usable as a filename", stem)
	}
	return nil
}

// safeStem reduces an operator's output name to one path component.
//
// A stem is a filename and not a path: a job writing to ../../etc/x would be
// the operator's own config doing it, but the Root would refuse it and the
// error would be about a root rather than about a name. Flattening first means
// the message names the real problem.
func safeStem(s string) string {
	s = strings.TrimSpace(s)
	s = strings.ReplaceAll(s, "\\", "/")
	s = path.Base(path.Clean("/" + s))

	// path.Base answers "/" for a name that cleaned away to nothing -- "",
	// "..", "/" and "../.." all land here, because Clean absorbs a .. that
	// has nothing above it. Returning early is what makes those a refusal:
	// the character map below would otherwise turn "/" into "-" and write a
	// file called "-00.png", which is a silent rename rather than an error.
	if s == "/" || s == "." || s == ".." {
		return ""
	}

	s = strings.TrimLeft(s, ".")
	s = strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			return r
		case r == '-', r == '_', r == '.':
			return r
		}
		return '-'
	}, s)
	return s
}

// ServeCommand finishes the operator's sd-server argv with the address ember
// requires it to listen on.
//
// EMBER OWNS THE LISTEN ADDRESS. A config passing --listen-ip 0.0.0.0 would
// put sd-server on every interface the node has, which on a rented machine is
// a public one -- an unauthenticated image generator with arbitrary file paths
// in its request body, which is the shape of the ComfyUI exposure this
// repository's whole threat model is written against. So the flags are
// appended here and a serve command that names either of them is refused.
//
// The same rule and the same reason as tunnel.RemoteLoopback: the far end of
// something ember opens is built by ember and never parsed from input.
func ServeCommand(argv []string) ([]string, error) {
	if len(argv) == 0 {
		return nil, nil
	}
	for _, a := range argv {
		flag := a
		if i := strings.Index(flag, "="); i >= 0 {
			flag = flag[:i]
		}
		switch flag {
		case "--listen-ip", "--listen-port":
			return nil, fmt.Errorf("sd.serve passes %s, which ember sets itself: "+
				"sd-server is reached over the SSH connection on 127.0.0.1:%d, and a serve command "+
				"that chooses its own address could publish an unauthenticated generator on a rented "+
				"machine's public interface", flag, Port)
		}
	}
	out := append([]string(nil), argv...)
	return append(out, "--listen-ip", "127.0.0.1", "--listen-port", fmt.Sprintf("%d", Port)), nil
}

// say writes an operator-facing line, tolerating a nil writer.
//
// The same helper as gpucheck.say and assets.say, for the same reason: a
// progress line that cannot be written is not a reason to fail a run, and a
// nil out is what every unit test passes.
func say(out io.Writer, format string, a ...any) {
	if out == nil {
		return
	}
	_, _ = fmt.Fprintf(out, format, a...)
}
