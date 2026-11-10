package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/Legion112/ember/internal/app"
	"github.com/Legion112/ember/internal/blob"
	"github.com/Legion112/ember/internal/config"
	"github.com/Legion112/ember/internal/genstore"
	"github.com/Legion112/ember/internal/logx"
	"github.com/Legion112/ember/internal/loopback"
	"github.com/Legion112/ember/internal/node"
	"github.com/Legion112/ember/internal/queue"
	"github.com/Legion112/ember/internal/tts"
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
	logs := addLogFlags(fs)
	fs.Usage = func() {
		outf(out, "ember serve runs the web UI and a queue that drains onto up to max_nodes rentals.\n\n")
		outf(out, "It listens on loopback only. Nothing ember serves is reachable from\nanother machine.\n\n")
		outf(out, "EVERY REQUEST NEEDS A TOKEN. A loopback port is reachable by every uid on\nthis machine, and a request with no Origin -- which is what curl sends -- is\nindistinguishable from your own tooling, so the second account over could\notherwise queue a paid rental. ember serve mints a token for each run and\nprints a URL ending in ?t=<token>; open that and the browser is authorised\nfor as long as the process lives. Set serve_token in credentials.yaml, or\nEMBER_SERVE_TOKEN, for one that survives a restart. There is no --token\nflag: /proc/<pid>/cmdline is world-readable, which is the same uid the\ntoken exists to keep out.\n\n")
		outf(out, "The token is not a substitute for --allow-paid, which stays off by default:\nauthentication answers who is asking and --allow-paid is a ceiling on what\nthe answer permits.\n\n")
		outf(out, "IT ALSO SERVES A UNIX SOCKET under the state directory, at mode 0600, which\nanswers the same routes with no token at all: connect(2) needs write\npermission on the socket, so the operating system refuses the account next\ndoor rather than ember refusing it afterwards. A browser cannot open a unix\nsocket, so the loopback port and its token are still what a browser uses.\n\n")
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
	logOpts, err := logs.resolve()
	if err != nil {
		return err
	}

	// READ ONCE, HERE. The config is the ceiling, and a request must never be
	// able to name which file sets its own limits -- so the path comes from
	// this flag and is never accepted over HTTP.
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	// THE REFUSAL IS "THIS CAN SERVE NOTHING" AND NOT "THIS HAS NO sd.serve",
	// which is the same sentence while there is one kind and a wall the moment
	// there are two.
	//
	// FOUND BY THE LAB AND BY NOTHING ELSE. Every unit test of the speech path
	// builds a `webapi.Options` or a `queue.Queue` directly, so none of them
	// goes through this function at all -- `go test ./...` is entirely green
	// over a configuration `ember serve` refuses to start from, and what
	// reported it was the first run of the lab that posts a request to a real
	// server. The whole of the browser half was reachable by nothing.
	//
	// It names BOTH keys, because an operator who has written neither needs to
	// know there are two and an operator who has written one of them has a
	// working server.
	voices, err := cfg.TTSCatalogue()
	if err != nil {
		return err
	}
	if len(cfg.SD.Serve) == 0 && len(voices) == 0 {
		return usageError{fmt.Errorf("%s has no sd.serve block and publishes no tts.voices, so "+
			"this server could offer nothing: a queued request would have nowhere to run. See the "+
			"sd: and tts: sections of README.md, or ember.local.sd-server.example.yaml for a "+
			"configuration that costs nothing", *cfgPath)}
	}

	// Installed here and nowhere lower. internal/app takes a context and
	// installs nothing, because a handler per rental would mean one SIGINT
	// handler per rental -- swallowing Ctrl-C while a run is in flight and
	// killing the process when none is.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	return serveUntil(ctx, out, d, cfg, serveOptions{
		Port: uint16(*port), AllowPaid: *allowPaid, Dist: *dist, Log: logOpts,
		Voices: voices,
	})
}

// serveOptions are what the flags decided.
type serveOptions struct {
	// Voices is the catalogue runServe already built and validated, so this
	// process reads the key once.
	Voices tts.Catalogue

	Port      uint16
	AllowPaid bool
	Dist      string

	// Log is the level and the format. It reaches two places from here -- the
	// logger this process writes with, and the Request every queued rental is
	// planned from -- so a `--log-level debug` covers the server's own lines
	// and the lines a rental emits inside internal/provision alike. Without
	// the second, turning the level up on the one long-running command would
	// have left the rental, which is the expensive half, silent.
	Log logx.Options
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

	// SEPARATE FROM THE BLOB STORE ABOVE, AND UNDER THE STATE DIRECTORY
	// RATHER THAN THE OUTPUTS. An uploaded image is not something a run
	// produced: it exists before the request that uses it, so it cannot be
	// keyed by one -- and writing it under the outputs would make `ember
	// history prune` treat an operator's own photograph as a generated image
	// it is free to remove. internal/blob's own header says the rest.
	inputs, err := blob.OpenInputs(ps.Inputs)
	if err != nil {
		return err
	}
	defer func() { _ = inputs.Close() }()

	// The node's own output -- an sd-server's complaint, a model load that is
	// about to fail -- goes to this terminal. ember's structured progress
	// does not: it goes to the database, which is what the browser reads.
	logger := logx.New(out, o.Log)

	// THE SPEECH CATALOGUE, DERIVED ONCE AND HANDED TO BOTH READERS.
	//
	// config.Load has already validated it -- a duplicate or malformed id
	// refuses the file -- so the error here is unreachable from a
	// configuration this process could have started from. It is returned
	// anyway rather than dropped, because "unreachable" is a claim about
	// today's Load and a dropped error is how that claim stops being checked.
	//
	// ONE DERIVATION FOR THE QUEUE AND THE SERVER. Two calls would be two
	// readings of one file, and the thing that makes them able to disagree is
	// not today's code but the next `--config` reload somebody adds: the
	// browser would then be offering ids the drain loop could not resolve.
	// #105's defect was the opposite shape of the same seam -- a field the
	// constructor never assigned -- and the lesson is that a wiring like this
	// is worth one line and one guard.
	// THE CATALOGUE THE CALLER ALREADY BUILT, which is the one-derivation rule
	// this feature is wired on: runServe validates it before deciding whether
	// this server can offer anything, and a second call here would be a second
	// reading of one file. `serveUntil` is also what the labs call directly,
	// so it derives its own when handed none -- which is the one place two
	// readings are unavoidable and is why it is a parameter rather than a
	// field somebody can forget.
	voices := o.Voices
	if voices == nil {
		var err error
		if voices, err = cfg.TTSCatalogue(); err != nil {
			return err
		}
	}

	// WHAT THIS PROCESS HAS AGREED TO SPEND, built here because it is shared:
	// the drain loop's slots and a run submitted over the socket have to see
	// each other's commitments or the ceilings are checked against a ledger
	// in which a rental in flight is worth nothing. One value, handed to the
	// queue and read back from it by DirectRunner -- see queue/commit.go,
	// which has the measurement and the two residues.
	commit := &queue.Commitments{}

	// AND ONE STORE IS FILLED BY ONE SLOT AT A TIME. At max_nodes>1 two plans
	// run at once, so two slots can both be told a store is missing its
	// weights and both go and rent a machine to fetch the same fifty-six
	// gigabytes -- which the one-rental loop made unrepresentable by being
	// serial. See queue/storefill.go.
	fills := &queue.StoreFills{}

	q := &queue.Queue{
		Store: store,
		// THE NODE CEILING, from the operator's own file. Any value below 1
		// reads as 1 inside the queue, so a zero here would silently pin an
		// operator who wrote `max_nodes: 2` to one node with nothing
		// reporting it -- the #105 class, which is why
		// TestServe_WiresTheNodeCeilingIntoTheDrainLoop parses this file.
		MaxNodes: cfg.MaxNodes,
		Commit:   commit,
		// The catalogue a stored voice id is resolved against. This is the
		// only thing on the drain loop that can turn an id into a path, and
		// it comes from the config this process was started with.
		Voices: voices,
		// Ledger is the same store, as a reader: the period ceilings are
		// checked against what the generation history says was spent, which
		// is the one number that is already recorded, numbered and durable.
		//
		// AND Runs IS THE SAME STORE AS A WRITER, narrowed to what recording
		// a rental needs: the planner fills an empty store on a machine with
		// no card, and a rental this process chose to make must be in the
		// ledger the ceiling above reads. The two fields are two types over
		// one store on purpose -- see queue.RunStore.
		Planner: queue.AppPlanner{
			Config: cfg, Deps: d, AllowPaid: o.AllowPaid, Out: out,
			Ledger: store, Runs: store, Commit: commit, Fills: fills,
			Log: o.Log,
		},
		Outputs: ps.Outputs,
		Log:     logger,
	}

	// THE CREDENTIAL, RESOLVED BEFORE THE SERVER EXISTS. webapi.New refuses a
	// zero token, so there is no ordering in which an unauthenticated server
	// gets built and then has one attached.
	token, minted := serveToken(creds)

	// AND THE SECOND CHANNEL'S PATH, NAMED BY loopback AND NOT HERE. The
	// caller hands over the state directory and that package appends its own
	// subdirectory and basename, so there is no path this file could choose
	// -- `loopback.Bind`'s rule, with a filesystem in place of a network.
	// Resolved before the server is built for the same reason the token is:
	// a server that exists and then has a channel attached has an ordering in
	// which it does not.
	sock, err := loopback.StateSocket(ps.State)
	if err != nil {
		return err
	}

	// AND THE PRIVILEGED CHANNEL'S ONE CAPABILITY.
	//
	// It is reachable from the socket's mux alone -- see internal/webapi/run.go
	// -- and what it does is run `ember up`'s own pipeline on the one goroutine
	// that can rent. It is wired here rather than inside webapi because webapi
	// must have no way to name a plan, a provider or a rental: the seam is one
	// interface with one method, and the implementation lives beside the drain
	// loop whose single-rental invariant is what makes it safe.
	// Config IS THIS PROCESS'S OWN AND IT IS THERE FOR THE PERIOD CEILINGS.
	// A submitted run names its own config and that one travels -- the
	// reversal app.RunAsk documents -- but the ledger it is recorded in is
	// `store` here, so the file that owns that ledger has to be consulted
	// about `max_day_usd` as well, or a submitted run spends the queue's
	// allowance without being bounded by it (#165). See DirectRunner.Config,
	// which has the argument and the pair.
	runs := queue.DirectRunner{
		Deps: d, Store: store, Queue: q, AllowPaid: o.AllowPaid, Out: out, Log: o.Log,
		Config: cfg,
	}

	// AND THE ONE THING `GET /api/nodes` NEEDS, which it did not have in any
	// shipped `ember serve`: `webapi.Options` had no Scan field, nothing
	// assigned the Server's `scan`, and the route therefore answered 501 every
	// time -- with the whole projection behind it, the monthly storage charge
	// included, reachable only from webapi's own tests. See webapi.Options.Scan.
	//
	// THE REGISTRY AND THE JOURNAL THIS PROCESS ALREADY HOLDS, through
	// app.ScanWith rather than app.Scan. Scan loads the config and builds a
	// registry of its own, which here would be a second read of the authority
	// -- the config is the ceiling and is deliberately read once, at startup --
	// and `ember reap`'s own comment says why a second pair is wrong: two
	// reads are two chances for the two copies to disagree about which
	// provider a finding belongs to.
	//
	// THE JOURNAL IS OPENED ONCE FOR THE REASON ABOVE AND FOR NO OTHER.
	// statestore.Open holds no file handle, so opening per request would cost
	// nothing; what it would cost is a second place this server decides where
	// the state directory is, and `ps` came from the one config read. That is
	// the whole argument, and the previous spelling of this comment added a
	// safety one that `statestore` does not support -- it said "every write is
	// write-then-rename, so a read here cannot see half a record", which is
	// true of `Update` alone.
	//
	// A SCAN CAN RACE A RENTAL'S OWN JOURNAL WRITES, and the two reasons are
	// both structural. `Create` is `O_WRONLY|O_CREATE|O_EXCL` and then a
	// `Write`, so the record exists at its final name with length ZERO
	// between the two statements; `Forget` is a bare `os.Remove`, which
	// `List`'s ReadDir-then-read races with no write involved at all. And the
	// mutex is per `*statestore.Store` INSTANCE: `app.BuildOptions` opens its
	// own per rental, so the one opened here is a second instance over one
	// directory and the lock serialises the two against each other not at
	// all.
	//
	// Measured in exactly this arrangement, two instances from
	// `app.OpenStore` over one directory, 20,000 creates against a hammering
	// List: with every record FORGOTTEN after it is written -- which is what
	// a rental does, and why this journal is "deliberately empty after a
	// clean run" -- 34,779 Lists and 1,230 failures, 1,041 of them the
	// `Forget` window and 189 the zero-length one. Without the forget the
	// directory grows to 20,000 files, List slows to 77 passes and samples
	// neither. So the near-empty journal is not the safe case, it is the
	// regime where the race is reachable, and the wider of the two windows is
	// the one with no write in it.
	//
	// What that costs is a 502 from `GET /api/nodes` if the button is pressed
	// in the microseconds a rental is recording or forgetting a record -- and
	// nothing worse, because `List` returns the error rather than dropping the
	// row, so the failure is loud and transient rather than a billing node
	// reported as fine. It is #168, it predates this wiring (`ember ls`
	// against a running `ember up` has always had it), and what this change
	// adds is that the hazard is now INTRA-process and reachable from a
	// browser. Fixing it belongs in `statestore`, where `Create` can claim a
	// name with `link(2)` over a fully written temp and `List` can tell a
	// record that was forgotten from one that will not parse.
	journal, err := app.OpenStore(ps.State)
	if err != nil {
		return err
	}
	scan := func(ctx context.Context) (app.Scanned, error) {
		return app.ScanWith(ctx, reg, journal, cfg)
	}

	srv, err := webapi.New(webapi.Options{
		Config: cfg, Registry: reg, Store: store, Blobs: blobs, Inputs: inputs, Queue: q,
		AllowPaid: o.AllowPaid, Port: o.Port, Dist: o.Dist, Log: logger,
		Token: token, Socket: sock, Runs: runs, Scan: scan, Voices: voices,
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

	// BOTH CHANNELS BOUND BEFORE EITHER IS ANNOUNCED, and a socket that
	// cannot be opened stops the server rather than being left out. A channel
	// the announcement describes as authenticated by the operating system and
	// that is quietly absent is the "reads as working" defect this repository
	// keeps recording -- and the client #146 is for would get
	// connection-refused with nothing to read.
	sockLn, err := srv.ListenSocket(ctx)
	if err != nil {
		return err
	}

	announce(out, srv, cfg, o.AllowPaid, o.Dist, ps.Outputs, standing(ctx, store, cfg), minted)

	// SHUTDOWN ORDER IS THE WHOLE POINT OF WHAT FOLLOWS.
	//
	// Both halves are started and BOTH are waited for. http.Server.Shutdown
	// drains requests, and a rental does not live in a request -- it lives on
	// one of the queue's slot goroutines, so waiting only for the server would
	// exit while a node is alive. provision.WithNode's deferred destroy runs
	// on a context that survives the cancellation, and queue.Run does not
	// return until EVERY slot's has, so waiting for queue.Run is waiting for
	// every node to be gone.
	//
	// THAT SENTENCE USED TO BE ABOUT ONE NODE and is amended rather than left
	// to be falsified: at max_nodes>1 there are up to that many, queue.Run
	// holds a WaitGroup over them and the wait is deliberately unbounded,
	// because a bound here abandons a GPU. See internal/queue's package doc.
	//
	// That is the single most expensive thing to get wrong here: a process
	// that exits with a GPU still rented bills by the hour with nobody
	// watching, and the TTL is the only thing that eventually stops it.
	var (
		wg                        sync.WaitGroup
		serveErr, sockErr, runErr error
	)
	wg.Add(3)
	go func() {
		defer wg.Done()
		serveErr = srv.Serve(ctx, lns)
	}()
	// THE SOCKET IS WAITED FOR LIKE THE PORT AND FOR THE SAME REASON. It is
	// its own http.Server -- see webapi/socket.go, where that separation is
	// what stops a TCP request being marked as authenticated -- so it has its
	// own drain, and a process that returned while it was still accepting
	// would be a process whose rental is gone and whose channel is not.
	go func() {
		defer wg.Done()
		sockErr = srv.ServeSocket(ctx, sockLn)
	}()
	go func() {
		defer wg.Done()
		runErr = q.Run(ctx)
	}()
	wg.Wait()

	outf(out, "\nevery rental has been destroyed.\n")
	return errors.Join(serveErr, sockErr, runErr)
}

// serveToken decides which credential this server will accept, and reports
// whether ember had to mint one.
//
// THE OPERATOR'S OWN TOKEN WINS, so a stable credential is a line in
// credentials.yaml or one environment variable and nothing else -- which is
// what a bookmark and a script against the local API both need. With neither,
// ember mints one rather than refusing to start: a server that will not come
// up until the operator has invented a secret is a server somebody runs an
// older binary to avoid, and there is no honest default that is weaker,
// because `ember serve` has no unauthenticated mode to fall back to.
//
// The minted one lives in this process and is never written anywhere. That is
// deliberate: it means a URL from yesterday's run does not work, and it means
// nothing on disk has to be cleaned up. The announcement says both.
func serveToken(creds config.Credentials) (tok node.Secret, minted bool) {
	if !creds.ServeToken.IsZero() {
		return creds.ServeToken, false
	}
	return webapi.NewToken(), true
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
	// queue.CeilingsOf and not config.PeriodCeilings directly, so that the
	// announcement and the five gates read the key through one function: this
	// print was one of exactly two non-test callers of PeriodCeilings when
	// #165 was filed, and the other was the only gate there was.
	cs, err := queue.CeilingsOf(cfg)
	if err != nil {
		return periodStanding{Err: err}
	}
	st := periodStanding{Ceilings: cs}
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

func announce(out io.Writer, srv *webapi.Server, cfg config.Config, allowPaid bool, dist, outputs string, st periodStanding, minted bool) {
	outf(out, "ember serve\n\n")
	// THE URL WITH THE TOKEN IN IT ONLY WHEN EMBER MINTED ONE.
	//
	// It is the only way an operator can get at a credential they did not
	// choose, so it has to be printed. A CONFIGURED token is not reprinted:
	// they already have it, and a second copy on a stdout that a systemd unit
	// may be writing to a world-readable journal is a copy they did not ask
	// for. The fingerprint goes there instead, which is node.Secret's own
	// discipline and enough to tell two credentials apart.
	if minted {
		outf(out, "  %s\n", srv.TokenURL())
	} else {
		outf(out, "  %s\n", srv.URL())
	}
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
		// THE NODE CEILING BESIDE THE MONEY ONE, because an operator deciding
		// whether to walk away needs both: this is the number that says how
		// many cards an unattended queue may be holding at once, and it is 2
		// by default rather than the 1 that used to be structural.
		announceNodes(out, cfg.MaxNodes)
		announcePeriods(out, st)
	} else {
		outf(out, "  Paid providers are refused. Start with --allow-paid to let the UI spend money.\n")
	}
	announceToken(out, srv, minted)
	announceSocket(out, srv)

	outf(out, "  Images land in %s\n", outputs)
	outf(out, "\nCtrl-C to stop. Any rental is destroyed before this exits.\n\n")
}

// announceToken is the paragraph that replaced "there is no authentication".
//
// IT SAYS WHAT IS STILL NOT GUARDED, which is the half an operator has to act
// on. The token holds against another uid on this machine and against
// everything reaching a forwarded port; it holds against nothing that this
// uid can already read, and it does not expire. A control whose limits are
// not printed is a control that gets believed past them.
func announceToken(out io.Writer, srv *webapi.Server, minted bool) {
	if minted {
		outf(out, "  Every request needs the token in the URL above. It was minted for this run\n"+
			"  and is not written anywhere, so it changes when ember restarts; set\n"+
			"  serve_token in credentials.yaml for one that does not.\n")
	} else {
		outf(out, "  Every request needs your configured serve_token (%s). Open\n"+
			"  %s?t=<token> once to authorise a browser.\n", srv.TokenFingerprint(), srv.URL())
	}
	outf(out, "  That keeps out another account on this machine and anything reaching a\n"+
		"  forwarded port. It does not keep out your own uid or root, which can read\n"+
		"  the token, and it never expires -- restart ember to revoke it.\n")
}

// announceSocket names the channel that needs no token.
//
// SAID BESIDE THE TOKEN AND NOT INSTEAD OF IT, because the two answer the
// same question for different clients and an operator has to know which one
// they are on. The mode is printed because it IS the authentication: a reader
// who knows the socket is 0600 knows why no token was asked for, and one who
// does not would reasonably assume the channel is unauthenticated.
func announceSocket(out io.Writer, srv *webapi.Server) {
	outf(out, "  A unix socket at %s (mode 0600) serves the same routes with no\n"+
		"  token: connect(2) needs write permission on it, so the operating system\n"+
		"  refuses every account but yours before ember sees the request. A browser\n"+
		"  cannot reach it; the port above is what a browser uses.\n", srv.SocketPath())
}

// announceNodes says how many rentals this process may hold at once.
//
// ONLY UNDER --allow-paid, with announcePeriods and for its reason: a free
// provider's rentals cost nothing, so how many of them there may be is not a
// number an operator has to weigh before walking away.
//
// IT SAYS WHAT max_nodes > 1 DOES AND DOES NOT BUY, because the honest answer
// is counter-intuitive and an operator who expects two cards for two images
// will conclude ember is broken. Packing is strict: a request a live rental
// can serve never gets a node of its own, so the second node is for work the
// first one cannot take at all.
func announceNodes(out io.Writer, maxNodes int) {
	if maxNodes < 1 {
		maxNodes = 1
	}
	if maxNodes == 1 {
		outf(out, "  At most one rental at a time (max_nodes: 1).\n")
		return
	}
	outf(out, "  At most %d rentals at a time (max_nodes). A queued request that a rental\n"+
		"  already up can serve joins it rather than starting another, so a second node is\n"+
		"  only for work the first cannot take -- a different provider, card or kind.\n", maxNodes)
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
