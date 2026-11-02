package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/Legion112/ember/internal/app"
	"github.com/Legion112/ember/internal/blob"
	"github.com/Legion112/ember/internal/config"
	"github.com/Legion112/ember/internal/genstore"
	"github.com/Legion112/ember/internal/node"
	"github.com/Legion112/ember/internal/queue"
	"github.com/Legion112/ember/internal/webapi"
)

func cmdServe(args []string, out io.Writer) error { return runServe(args, out, deps{}) }

// runServe starts the web UI and the queue that drains it.
//
// WHAT THIS COMMAND IS. One process holding one rental at a time, draining a
// queue a browser fills. The queue is a single goroutine, so two concurrent
// rentals cannot be expressed -- see internal/queue -- and this function's job
// is to start the two halves and, much more importantly, to stop them in the
// right order.
func runServe(args []string, out io.Writer, d deps) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(out)
	var (
		cfgPath   = fs.String("config", config.FileName, "path to ember.yaml")
		port      = fs.Uint("port", uint(webapi.DefaultPort), "loopback port to serve on")
		allowPaid = fs.Bool("allow-paid", false, "let the web UI rent from providers that cost money")
		dist      = fs.String("dist", "", "serve the UI from this directory instead of the built-in copy")
	)
	fs.Usage = func() {
		outf(out, "ember serve runs the web UI and a queue that drains onto one rental.\n\n")
		outf(out, "It listens on loopback only. Nothing ember serves is reachable from\nanother machine.\n\n")
		outf(out, "THERE IS NO AUTHENTICATION. Any page open in your browser can reach a\nloopback port, so a site you visit could queue a generation. What bounds\nthat is --allow-paid, which is off by default: without it this server\nrefuses every provider that costs money.\n\n")
		outf(out, "A request may narrow the ceilings in your config and never widen them.\nThe image, the store, the ports and the sd.serve argv are read from the\nfile and cannot be chosen over HTTP.\n\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return usageError{err}
	}
	if fs.NArg() != 0 {
		return usageError{fmt.Errorf("ember serve takes no arguments; it reads %s", *cfgPath)}
	}
	if *port == 0 || *port > 65535 {
		return usageError{fmt.Errorf("port %d is outside 1..65535", *port)}
	}

	// READ ONCE, HERE. The config is the ceiling, and a request must never be
	// able to name which file sets its own limits -- so the path comes from
	// this flag and is never accepted over HTTP.
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	if len(cfg.SD.Serve) == 0 {
		return usageError{fmt.Errorf("%s has no sd.serve block, so there is no generation server "+
			"to start and a queued request would have nowhere to run. See the sd: section of "+
			"README.md, or ember.local.sd-server.example.yaml for a configuration that costs nothing",
			*cfgPath)}
	}

	// Installed here and nowhere lower. internal/app takes a context and
	// installs nothing, because a handler per rental would mean one SIGINT
	// handler per rental -- swallowing Ctrl-C while a run is in flight and
	// killing the process when none is.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	return serveUntil(ctx, out, d, cfg, serveOptions{
		Port: uint16(*port), AllowPaid: *allowPaid, Dist: *dist,
	})
}

// serveOptions are what the flags decided.
type serveOptions struct {
	Port      uint16
	AllowPaid bool
	Dist      string
}

// serveUntil builds both halves and runs them until ctx ends.
//
// IT TAKES A CONTEXT AND INSTALLS NO SIGNAL HANDLER, which is the same rule
// internal/app follows and for the same reason: whoever owns the process owns
// the signals. It is also what makes the wiring here testable -- a test hands
// it a context it can cancel, rather than this function being untestable
// because it waits for SIGINT.
func serveUntil(ctx context.Context, out io.Writer, d deps, cfg config.Config, o serveOptions) error {
	// GUARDED, because two goroutines write to it.
	//
	// The queue hands this writer to the node's own streams -- an sd-server's
	// stdout, a model load's complaint -- and the HTTP server's slog handler
	// wraps the same one. slog holds its own mutex; the node's streams do
	// not, and report.Writer guards itself for exactly this reason. With
	// os.Stdout the worst case is interleaved lines; with anything that is
	// not a file descriptor it is a data race, and a caller should not have
	// to know which they passed.
	out = &syncWriter{w: out}

	ps, err := app.ResolvePaths(cfg, d)
	if err != nil {
		return err
	}
	creds, err := d.Credentials(ps.Config)
	if err != nil {
		return err
	}
	reg, err := app.BuildRegistry(d, creds, cfg)
	if err != nil {
		return err
	}

	store, err := genstore.Open(ps.State)
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()

	// Rooted at the output directory and not at one run's, so a blob key
	// spans requests: "<request>/<stem>-00.png" is one name for a path, used
	// by the row in the database and by the file on disk.
	blobs, err := blob.OpenDir(ps.Outputs)
	if err != nil {
		return err
	}
	defer func() { _ = blobs.Close() }()

	// The node's own output -- an sd-server's complaint, a model load that is
	// about to fail -- goes to this terminal. ember's structured progress
	// does not: it goes to the database, which is what the browser reads.
	logger := slog.New(slog.NewTextHandler(out, &slog.HandlerOptions{Level: slog.LevelInfo}))

	q := &queue.Queue{
		Store: store,
		// Ledger is the same store, as a reader: the period ceilings are
		// checked against what the generation history says was spent, which
		// is the one number that is already recorded, numbered and durable.
		Planner: queue.AppPlanner{
			Config: cfg, Deps: d, AllowPaid: o.AllowPaid, Out: out, Ledger: store,
		},
		Outputs: ps.Outputs,
		Out:     out,
		Log:     logger,
	}

	srv, err := webapi.New(webapi.Options{
		Config: cfg, Registry: reg, Store: store, Blobs: blobs, Queue: q,
		AllowPaid: o.AllowPaid, Port: o.Port, Dist: o.Dist, Log: logger,
	})
	if err != nil {
		return err
	}

	// BOUND BEFORE ANNOUNCED. Printing a URL that is not listening yet means
	// an operator who clicks it immediately gets connection-refused, and it
	// means "another ember serve is already running" is reported after this
	// one has claimed to start.
	lns, err := srv.Listen(ctx)
	if err != nil {
		return err
	}

	announce(out, srv, cfg, o.AllowPaid, o.Dist, ps.Outputs, standing(ctx, store, cfg))

	// SHUTDOWN ORDER IS THE WHOLE POINT OF WHAT FOLLOWS.
	//
	// Both halves are started and BOTH are waited for. http.Server.Shutdown
	// drains requests, and a rental does not live in a request -- it lives in
	// the queue's goroutine, so waiting only for the server would exit while
	// a node is alive. provision.WithNode's deferred destroy runs on a
	// context that survives the cancellation, and queue.Run does not return
	// until it has, so waiting for queue.Run is waiting for the node to be
	// gone.
	//
	// That is the single most expensive thing to get wrong here: a process
	// that exits with a GPU still rented bills by the hour with nobody
	// watching, and the TTL is the only thing that eventually stops it.
	var (
		wg               sync.WaitGroup
		serveErr, runErr error
	)
	wg.Add(2)
	go func() {
		defer wg.Done()
		serveErr = srv.Serve(ctx, lns)
	}()
	go func() {
		defer wg.Done()
		runErr = q.Run(ctx)
	}()
	wg.Wait()

	outf(out, "\nevery rental has been destroyed.\n")
	return errors.Join(serveErr, runErr)
}

// announce is what an operator reads when the server comes up.
//
// It says what is NOT guarded as plainly as what is, because the one thing a
// person has to decide here is whether to leave this running.
// periodStanding is where the period ceilings stand at startup.
//
// SAID AT THE START, because a ceiling an operator learns about by having a
// request refused is a ceiling they find out about after queueing thirty of
// them. This is the one place `ember serve` can tell them their headroom
// before they type anything, and it costs two SELECTs.
type periodStanding struct {
	Ceilings queue.Ceilings
	Day      node.MicroUSD
	Month    node.MicroUSD

	// Err is a read that failed. Reported in the announcement rather than
	// refused at startup, because this is the informational copy -- the
	// planner does the same read before every rental and refuses there, which
	// is the check that stops money.
	Err error
}

// standing reads what the current day and month have come to.
func standing(ctx context.Context, s queue.Spender, cfg config.Config) periodStanding {
	day, month, err := cfg.PeriodCeilings()
	if err != nil {
		return periodStanding{Err: err}
	}
	st := periodStanding{Ceilings: queue.Ceilings{Day: day, Month: month}}
	if st.Ceilings.IsZero() {
		return st
	}
	now := time.Now()
	if st.Day, err = queue.Spent(ctx, s, queue.PeriodDay.Start(now)); err != nil {
		return periodStanding{Ceilings: st.Ceilings, Err: err}
	}
	if st.Month, err = queue.Spent(ctx, s, queue.PeriodMonth.Start(now)); err != nil {
		return periodStanding{Ceilings: st.Ceilings, Err: err}
	}
	return st
}

func announce(out io.Writer, srv *webapi.Server, cfg config.Config, allowPaid bool, dist, outputs string, st periodStanding) {
	outf(out, "ember serve\n\n")
	outf(out, "  %s\n", srv.URL())
	if !srv.Built() {
		where := "make web"
		if dist != "" {
			where = fmt.Sprintf("make web, or point -dist at a built copy (%s has none)", dist)
		}
		outf(out, "  the UI was not built into this binary; run %s.\n  the JSON API under /api/ works either way.\n", where)
	}
	outf(out, "\n")

	if allowPaid {
		hourly, run, _ := cfg.Budgets()
		outf(out, "  PAID PROVIDERS ARE ENABLED. Ceilings from %s: at most %v per rental",
			config.FileName, cfg.TTL)
		if hourly > 0 {
			outf(out, ", %v", hourly)
		}
		if run > 0 {
			outf(out, ", %v per run", run)
		}
		outf(out, ".\n  A request may narrow those and never widen them.\n")
		announcePeriods(out, st)
	} else {
		outf(out, "  Paid providers are refused. Start with --allow-paid to let the UI spend money.\n")
	}
	outf(out, "  There is no authentication: any page in your browser can reach this port.\n")

	outf(out, "  Images land in %s\n", outputs)
	outf(out, "\nCtrl-C to stop. Any rental is destroyed before this exits.\n\n")
}

// announcePeriods says where the period ceilings stand.
//
// Only under --allow-paid, because a free rental is never refused by a money
// ceiling: the local provider's price is zero, so its projection is zero and
// it adds nothing to any period.
func announcePeriods(out io.Writer, st periodStanding) {
	if st.Err != nil {
		outf(out, "  A period ceiling is configured and ember could not read what has been spent: %v\n"+
			"  Every rental will be refused until that read works.\n", st.Err)
		return
	}
	if st.Ceilings.IsZero() {
		return
	}
	if st.Ceilings.Day > 0 {
		outf(out, "  Today has spent %v of %v (max_day_usd).\n", st.Day, st.Ceilings.Day)
	}
	if st.Ceilings.Month > 0 {
		outf(out, "  This month has spent %v of %v (max_month_usd).\n", st.Month, st.Ceilings.Month)
	}
	outf(out, "  A rental projected to take a period past its ceiling is refused, and the\n"+
		"  request says so. Only ember.yaml raises one.\n")
}

// syncWriter serializes writes from the two halves of `ember serve`.
type syncWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (s *syncWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.Write(p)
}
