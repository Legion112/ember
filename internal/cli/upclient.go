package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"

	"github.com/Legion112/ember/internal/app"
	"github.com/Legion112/ember/internal/genstore"
	"github.com/Legion112/ember/internal/loopback"
	"github.com/Legion112/ember/internal/report"
	"github.com/Legion112/ember/internal/webapi"
)

// THERE ARE TWO CODE PATHS FOR `ember up` AND THAT IS WRITTEN DOWN RATHER
// THAN DISCOVERED.
//
// With an `ember serve` running on this machine, `up` submits the run to it
// over the unix socket and streams the result: the run then outlives the
// terminal that started it, which is the whole of what #146 asks for. With no
// daemon, `up` does exactly what it has always done -- rents in this process,
// prints, and destroys the node on Ctrl-C.
//
// REFUSING INSTEAD WAS THE OTHER ANSWER and it is wrong here. #146's own
// comment argued for it, correctly, for a NEW verb: "refuse when no daemon is
// running, naming `ember serve`; a direct fallback is two code paths for a
// command that does not exist yet". `up` exists, every measurement in README
// was taken with it, and a version that refused unless a server happened to be
// up would break every one of those workflows for no gain. Auto-starting one
// is the answer that is not available at all: a background process that can
// spend money, started because somebody typed `up`, is not something ember
// does.
//
// WHAT DIFFERS BETWEEN THE TWO, said here because it is the thing a reader
// will want and the thing README has to carry:
//
//   - Ctrl-C. Direct: the node is destroyed, on a context that survives the
//     cancellation. Submitted: the CLIENT detaches and the run carries on.
//     That is the resilience being asked for and it removes what used to
//     guarantee no leak when you walk away, so the TTL becomes the real
//     exposure -- detached() says so, with the deadline in it, rather than
//     leaving an operator to find out from an invoice.
//   - --allow-paid. Direct: not gated, because the operator is at the
//     keyboard. Submitted: gated, because the flag is a property of the
//     long-running process. See queue.DirectRunner.AllowPaid.
//   - --dry-run never goes to a daemon. It rents nothing and prints, so
//     routing it through one would make the cheapest thing in ember depend on
//     a process being up.
//   - The node's own output arrives as events rather than straight onto the
//     terminal, so `ember history` has it. The rendering is the same function
//     either way, which is why the printed output is not.

// upDial bounds the "is a daemon running" probe.
//
// A unix connect(2) does not go near a network, so this is not a round trip:
// it is the bound on a socket whose owner has stopped accepting and whose
// backlog is full, which is the one way the call can block. loopback's own
// staleDial is the same decision from the other side.
const upDial = 2 * time.Second

// upPoll is the first gap between two reads of the event stream, and upPollMax
// the longest.
//
// A FIXED CADENCE IS A GUESS ABOUT HOW LONG THE WORK TAKES, and this
// repository has measured one being wrong by a factor of six: sdserver's flat
// two-second poll noticed a 0.3-second job up to two seconds late and hid the
// entire saving the resident model buys. So this is the same shape -- small,
// doubling, capped -- for the same reason. The floor is not smaller because
// each poll is two reads of a SQLite file and a human is reading the output;
// the cap is not larger because a progress line two seconds stale reads as a
// hang.
const (
	upPoll    = 50 * time.Millisecond
	upPollMax = time.Second
)

// serveSocket is the channel a running `ember serve` answers on, or false.
//
// THE PATH IS loopback's AND NOT THIS FILE'S. StateSocket is the only
// constructor for a Socket and it appends its own subdirectory and basename,
// so a client that spelled <state>/socket/serve.sock for itself would be the
// journal's "nodes" subdirectory again: two literals that drift, and a client
// that looks in the wrong place reports no daemon over a daemon that is
// running.
//
// A PATH TOO LONG TO BIND IS NOT REPORTED, which is the one silence here and
// is a conclusion rather than a shrug. StateSocket refuses a path over the
// length bind(2) accepts, and `ember serve` resolves the same path from the
// same state directory through the same constructor -- so a state directory
// that is too long is one no daemon can have started under, and "there is no
// daemon" is not a finding, it is the ordinary case. Printing it would put a
// line about a channel the operator did not ask for on every `ember up` in a
// deeply nested checkout, and `ember up`'s output is a contract this
// repository greps. The operator learns the real thing from `ember serve`,
// which refuses to start and names state_dir.
func serveSocket(cfg configPaths) (loopback.Socket, bool) {
	sock, err := loopback.StateSocket(cfg.State)
	if err != nil {
		return loopback.Socket{}, false
	}
	ctx, cancel := context.WithTimeout(context.Background(), upDial)
	defer cancel()
	var d net.Dialer
	c, err := d.DialContext(ctx, "unix", sock.Path())
	if err != nil {
		return loopback.Socket{}, false
	}
	_ = c.Close()
	return sock, true
}

// configPaths is the one thing the client reads the config for.
//
// A named type rather than a bare string, so the call site says what the
// string is: the state directory, which is where the socket lives. The client
// reads the operator's config ONLY for this -- the daemon reads the same file
// for everything else, which is the reversal RunAsk documents -- and two reads
// of one file for two different questions is cheaper than a path on the wire
// that the daemon would have to trust.
type configPaths struct{ State string }

// upClient talks to a running ember over its unix socket.
//
// NO CREDENTIAL ANYWHERE IN THIS FILE, which is the point of the channel:
// connect(2) on a node at 0600 in a directory at 0700 has already answered who
// is asking, so there is no token to find, none to put in a URL and none to
// leak. The loopback port and its mandatory token are untouched and are what a
// browser uses.
func upClient(sock loopback.Socket) *http.Client {
	return &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", sock.Path())
		},
	}}
}

// upHost is the authority in the client's own URLs.
//
// A unix socket has no host and Go needs one to build a request line.
// `localhost` with no port is what `curl --unix-socket` sends, and the socket's
// server deliberately does not apply checkHost -- a request that arrived on a
// socket came through no port, so requiring one would be a channel that
// refuses everything rather than a stricter check.
const upHost = "http://localhost"

// upOverSocket submits this run to a running ember and streams what happens.
//
// It returns nil when the run finished, the run's own reason when it did not,
// and nil on a detach -- because detaching is what the operator asked for and
// a non-zero exit would say something failed.
func upOverSocket(ctx context.Context, out io.Writer, sock loopback.Socket, ask app.RunAsk) error {
	c := upClient(sock)

	id, err := submitRun(ctx, c, ask)
	if err != nil {
		return err
	}
	outf(out, "submitted to the ember serve on %s as %s\n", sock.Path(), id)
	return followRun(ctx, out, c, id)
}

// submitRun posts the run and waits for the daemon's one rental loop to take
// it.
//
// THE WAIT IS THE HANDLER'S, NOT A POLL. queue.Submit is a rendezvous, so this
// request does not return until the drain goroutine owns the run -- which is
// how a client can be told a request id for a rental that is actually
// starting. While it waits, the daemon is finishing something else; the line
// below says so, because a command that prints nothing for forty minutes is
// indistinguishable from one that is wedged.
func submitRun(ctx context.Context, c *http.Client, ask app.RunAsk) (genstore.ReqID, error) {
	body, err := json.Marshal(webapi.RunBody{
		Config:       ask.Config,
		Workload:     ask.Overrides.Workload,
		Provider:     ask.Overrides.Provider,
		Image:        ask.Overrides.Image,
		GPU:          ask.Overrides.GPU,
		MaxHourlyUSD: ask.Overrides.MaxHourlyUSD,
		TTL:          durStr(ask.Overrides.TTL),
		Hold:         durStr(ask.Hold),
		Ports:        ask.Overrides.Ports,
		Cmd:          ask.Overrides.Cmd,
	})
	if err != nil {
		return "", err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, upHost+webapi.RunPath, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.Do(req)
	if err != nil {
		return "", fmt.Errorf("ask the running ember serve to start this run: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusAccepted {
		// THE SERVER'S SENTENCE IN FULL. Every refusal on this path is a
		// refusal `ember up` would have made itself -- an unpinned image, a
		// card with no image, a store missing the weights, a provider that is
		// not free -- and those messages carry the remediation. Reporting a
		// status code instead would throw away the only part an operator can
		// act on, which is the defect the cancel button had for the life of
		// #108.
		return "", errors.New(serverReason(raw, resp.StatusCode))
	}
	var acc webapi.RunAccepted
	if err := json.Unmarshal(raw, &acc); err != nil || acc.ID == "" {
		return "", fmt.Errorf("the running ember serve accepted this run and did not say which "+
			"request it is; nothing can be streamed. It answered: %s", trim(raw))
	}
	return genstore.ReqID(acc.ID), nil
}

// followRun renders the run's event stream until it ends or the operator
// detaches.
//
// IT RENDERS THROUGH report.Text, which is why the output is `ember up`'s
// output rather than a second format. The events are the ones the run emitted,
// the renderer is the one function in the program where a progress line is
// spelled, and a KindLog event's text is written verbatim with its terminator
// -- so bytes from the node reach this terminal unchanged, through a socket and
// a database, exactly as they reach the direct path's terminal through an
// io.Writer.
func followRun(ctx context.Context, out io.Writer, c *http.Client, id genstore.ReqID) error {
	text := report.Text(out)
	var (
		after    int64
		delay    = upPoll
		deadline time.Time
		node     string
	)

	for {
		es, next, err := runEvents(ctx, c, id, after)
		if err != nil {
			if ctx.Err() != nil {
				break
			}
			return err
		}
		render(out, text, es, &deadline, &node)
		after = next

		view, err := runView(ctx, c, id)
		if err != nil {
			if ctx.Err() != nil {
				break
			}
			return err
		}
		if node == "" {
			node = view.Node
		}
		if genstore.Status(view.Status).Terminal() {
			// The events are read once more before the outcome: the row's
			// terminal status and the last flush of the stream are two writes,
			// and a reader that stopped at the first would drop whatever the
			// run said last -- which on a failure is the reason.
			if es, _, eerr := runEvents(ctx, c, id, after); eerr == nil {
				render(out, text, es, &deadline, &node)
			}
			return runOutcome(view)
		}

		if len(es) > 0 {
			delay = upPoll
		} else if delay < upPollMax {
			delay *= 2
		}
		select {
		case <-ctx.Done():
		case <-time.After(delay):
			continue
		}
		break
	}

	detached(out, id, node, deadline)
	return nil
}

// render writes a page of events and keeps what the detach notice will need.
//
// ONE FUNCTION BECAUSE THERE ARE TWO CALL SITES, which is this repository's
// journal-subdirectory rule: the loop reads a page each poll and reads one
// more after the row goes terminal, and the first version of that second read
// called only text.Emit -- so a run_cost or a destroy_failed landing in the
// last flush would have been rendered as nothing, which is exactly the half
// clientSays exists for. Two places that each have to remember the same two
// calls is one place for the expensive one to be forgotten.
func render(out io.Writer, text report.Sink, es []report.Event, deadline *time.Time, node *string) {
	for _, e := range es {
		text.Emit(e)
		if e.Kind == report.KindNodeReady {
			// Kept for the detach notice: the deadline is the only thing that
			// stops a detached rental, so a client that could not name it
			// would be telling the operator to go and look.
			*deadline, *node = e.Deadline, e.Node
		}
		clientSays(out, e)
	}
}

// clientSays prints the three events report.Text is deliberately silent
// about, and the silence's own reason is why this exists.
//
// report.Text renders KindNodeReady, KindRunCost and KindDestroyFailed as
// NOTHING, and its comment says exactly why: "the CLI already reports all
// three through its slog handler [...] printing them here as well would both
// duplicate `ember up`'s output and leave two spellings of one fact". That
// premise is true of every caller it had and FALSE of this one. The slog
// handler for a submitted run is the DAEMON's -- its logger is the writer
// `ember serve` was started with -- so on this path the three are printed to a
// terminal nobody is watching for them and the operator who typed `ember up`
// gets nothing at all between "submitted" and the workload's first byte, which
// on a real rental is minutes.
//
// SO IT IS NOT A SECOND SPELLING, IT IS THE FIRST ONE ON THIS CHANNEL. The
// three are not decoration either: the deadline is now the whole of what
// stops the bill once a client has detached, the cost is the figure the run
// was for, and destroy_failed is the one event an operator must not miss --
// the node may still be running, and running means billing.
//
// Everything else goes through report.Text unchanged, which is what keeps the
// rendered run byte-for-byte what `ember up` prints.
func clientSays(out io.Writer, e report.Event) {
	switch e.Kind {
	case report.KindNodeReady:
		outf(out, "rented %s", e.Node)
		if e.Endpoint != "" {
			outf(out, " at %s", e.Endpoint)
		}
		if !e.Deadline.IsZero() {
			outf(out, "; it destroys itself at %s (%s from now)",
				e.Deadline.Format(time.RFC3339), time.Until(e.Deadline).Round(time.Second))
		}
		outf(out, "\n")
	case report.KindRunCost:
		outf(out, "the rental cost %v over %s (%s billable)\n",
			e.Cost, e.Elapsed.Round(time.Second), e.Billable.Round(time.Second))
	case report.KindDestroyFailed:
		// The wording is the page's, because it is the one line this program
		// must not understate: a destroy that failed leaves a machine that is
		// running, and running means billing.
		outf(out, "DESTROY FAILED for %s: %s\n"+
			"  THE NODE MAY STILL BE RUNNING AND BILLING -- check `ember ls`\n", e.Node, e.Err)
	}
}

// detached is what the operator reads when they press Ctrl-C.
//
// IT NAMES THE EXPOSURE RATHER THAN THE COMMAND IT REPLACED. On the direct
// path Ctrl-C destroys the node, so the guarantee was "no leak when you walk
// away". Here the run carries on, and what stops it is the TTL -- which is
// mandatory, and enforced in three independent places including the node's own
// self-destruct timer. So the default ttl IS the bill an operator is now
// exposed to, and that is a thing to be told at the moment of detaching rather
// than to discover.
//
// It names `ember down` because that is the command that ends a rental early,
// and `ember ls` because that one needs no daemon -- it asks every provider by
// label, which is the property #146 is careful to keep.
func detached(out io.Writer, id genstore.ReqID, node string, deadline time.Time) {
	outf(out, "\ndetached from %s. THE RUN IS STILL GOING and the node is still billing.\n", id)
	if !deadline.IsZero() {
		outf(out, "  it destroys itself at %s (%s from now), whether or not ember is running.\n",
			deadline.Format(time.RFC3339), time.Until(deadline).Round(time.Second))
	} else {
		outf(out, "  no node has been reported yet; when one is, its ttl is what stops it.\n")
	}
	outf(out, "  ember ls           what is billing, asked of every provider and not of the daemon\n")
	if node != "" {
		outf(out, "  ember down %-7s destroy it now\n", node)
	} else {
		outf(out, "  ember down <node>  destroy it now\n")
	}
	outf(out, "  ember history      what this run came to\n")
}

// runOutcome turns a finished row into this command's exit status.
//
// A FAILED RUN RETURNS THE ROW'S OWN REASON, which is the sentence the run
// wrote into requests.err -- the same one `ember history` prints and the page
// shows. A cancelled one is also an error, because that is what the direct
// path does: Plan.Do hands back the cancelled context and `ember up` returns
// it, so a run stopped by its deadline exits non-zero on both paths.
func runOutcome(v runRow) error {
	switch genstore.Status(v.Status) {
	case genstore.StatusDone:
		return nil
	default:
		if v.Error != "" {
			return fmt.Errorf("the run ended as %s: %s", v.Status, v.Error)
		}
		return fmt.Errorf("the run ended as %s", v.Status)
	}
}

// runRow is the part of a request the client reads.
//
// A NAMED TYPE AND NOT A map[string]any, for the reason every other route on
// this seam has one: a field nobody reads is a field nobody finds out about,
// which is how `cancelling` came to have a writer, a wire name and no reader.
// It decodes four of requestView's fields and ignores the rest, which is what
// it wants -- the images, the jobs and the cost belong to a page.
type runRow struct {
	Status string `json:"status"`
	Error  string `json:"error"`
	Node   string `json:"node"`
}

// runEventPage is the cursor route's answer.
type runEventPage struct {
	Next   int64          `json:"next"`
	Events []report.Event `json:"events"`
}

func runEvents(ctx context.Context, c *http.Client, id genstore.ReqID, after int64) ([]report.Event, int64, error) {
	var page runEventPage
	path := fmt.Sprintf("/api/requests/%s/events?after=%d", id, after)
	if err := getJSON(ctx, c, path, &page); err != nil {
		return nil, after, err
	}
	return page.Events, page.Next, nil
}

func runView(ctx context.Context, c *http.Client, id genstore.ReqID) (runRow, error) {
	var v runRow
	err := getJSON(ctx, c, "/api/requests/"+string(id), &v)
	return v, err
}

// getJSON reads one answer off the socket.
//
// The body is bounded for internal/s3's do() reason, one axis over: a reply
// larger than this client will read is refused rather than truncated, because
// a cut-off JSON document parses right up to where it does not and the error
// names neither the size nor the cause. Eight megabytes is generous for a page
// of events whose largest field is a line of a node's log.
func getJSON(ctx context.Context, c *http.Client, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, upHost+path, nil)
	if err != nil {
		return err
	}
	resp, err := c.Do(req)
	if err != nil {
		return fmt.Errorf("read %s from the running ember serve: %w", path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	const maxBody = 8 << 20
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil {
		return err
	}
	if len(raw) > maxBody {
		return fmt.Errorf("the answer to %s is larger than the %d bytes this client will read; "+
			"a truncated one would parse as far as the cut", path, maxBody)
	}
	if resp.StatusCode != http.StatusOK {
		return errors.New(serverReason(raw, resp.StatusCode))
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("could not read the answer to %s: %w", path, err)
	}
	return nil
}

// serverReason is the server's own sentence, or the status when there is none.
func serverReason(raw []byte, code int) string {
	var e struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(raw, &e); err == nil && e.Error != "" {
		return e.Error
	}
	return fmt.Sprintf("the running ember serve answered %d: %s", code, trim(raw))
}

// trim bounds a body quoted in an error, so a refusal cannot print a page.
func trim(raw []byte) string {
	const most = 512
	s := string(bytes.TrimSpace(raw))
	if len(s) > most {
		return s[:most] + "..."
	}
	if s == "" {
		return "(nothing)"
	}
	return s
}

// durStr renders a duration for the wire, and "" for none.
//
// A STRING AND NOT NANOSECONDS, because this channel is one an operator can
// drive with curl and `"ttl": "2h"` is the spelling they wrote in their
// config. time.ParseDuration reads back exactly what String wrote, so the
// round trip is lossless; the refusal for anything else is reachable from a
// hand-written request, which is a client this channel exists to have.
func durStr(d time.Duration) string {
	if d == 0 {
		return ""
	}
	return d.String()
}
