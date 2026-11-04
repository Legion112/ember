// Package webapi is ember's local web server.
//
// IT OWNS NOTHING. The queue drains, internal/app rents, internal/genstore
// records and internal/blob holds the bytes; this package parses requests,
// projects answers and refuses what should not reach any of them. That split
// is what makes it testable with httptest and no Docker: every decision worth
// a test is a decision about a request, not about a rental.
//
// WHAT IT REFUSES, which is the part worth reading:
//
//   - A request may NARROW the operator's ceilings and never widen them. The
//     config is read once at startup and never from a request, or the browser
//     would be naming which file sets its own limits.
//   - A request names a provider, a card and a batch of jobs, and nothing
//     else. No image, no store, no mount, no ports, no serve argv, no gpu
//     probe, no assets, no `extra`. Those stay in ember.yaml, which keeps
//     every seal in node, exposure and loopback upstream of anything a
//     request can influence.
//   - A paid provider needs --allow-paid, checked here and again in the
//     planner.
//
// THE LISTENER. loopback.Listen and (*http.Server).Serve, never
// http.ListenAndServe -- which takes an address as a string and hands it
// straight to net.Listen, walking around every refusal in
// loopback.ParseBind. TestNoListenOutsideLoopback fails the build on it now,
// and this package is exactly why that was added.
//
// BOTH LOOPBACK FAMILIES ARE BOUND. A v4-only listener is invisible to a
// browser that resolves localhost to ::1 first, and the operator reads
// connection-refused on a working server as a bug in ember. That rule has
// been in AGENTS.md since the first port forward; this is the one feature an
// actual browser opens, so it is the case it was written for.
//
// AND A SECOND CHANNEL, which is socket.go: a unix socket at 0600 serving the
// same routes, where the operating system has already answered who is asking
// and no token is required. It is additive -- the credential is still
// mandatory and still the only answer on the port -- and the one property
// worth knowing before reading any of this file is that "a request arrived on
// the socket" is a value on the request's CONTEXT, set by one ConnContext on
// one http.Server, so no header can claim it.
package webapi

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Legion112/ember/internal/app"
	"github.com/Legion112/ember/internal/blob"
	"github.com/Legion112/ember/internal/config"
	"github.com/Legion112/ember/internal/genstore"
	"github.com/Legion112/ember/internal/loopback"
	"github.com/Legion112/ember/internal/node"
	"github.com/Legion112/ember/internal/provider"
	"github.com/Legion112/ember/internal/report"
	"github.com/Legion112/ember/internal/sdserver"
	"github.com/Legion112/ember/internal/tts"
)

// DefaultPort is where `ember serve` listens.
//
// Not 8080 or 3000: those are the ports every other local development server
// takes, and a collision here means ember fails to start for a reason that
// has nothing to do with ember.
const DefaultPort uint16 = 7777

// readHeaderTimeout bounds a client that opens a connection and says nothing.
// gosec G112 asks for it; cmd/emberwl is the precedent.
const readHeaderTimeout = 10 * time.Second

// Drain is the drain loop as this package uses it.
//
// Three methods, all of which this package calls and none of which it
// implements: there is one Queue in the program and this is the narrow view a
// request handler needs of it. *queue.Queue satisfies it.
type Drain interface {
	// Kick says there may be work. Never blocks.
	Kick()

	// Running is which request is generating right now, if any.
	Running() (genstore.ReqID, bool)

	// Cancel stops one request's generation and reports whether it reached
	// it. False means that request is not the one generating, which a caller
	// must not report as "stopped" -- see cancelRequest.
	Cancel(genstore.ReqID) bool

	// Holding is whether this process has claimed that request at all --
	// anywhere between the claim and the end of its batch, generating or
	// not.
	//
	// NOT A SECOND SPELLING OF Running, and the difference is a rental the
	// operator is paying for: see cancelRequest's two 409s.
	Holding(genstore.ReqID) bool
}

// Options are what a server needs.
type Options struct {
	// Config is the operator's own file, read once at startup. It is the
	// ceiling and everything a request may not choose.
	Config config.Config

	// Registry is what this process can reach. A provider with no credential
	// is deliberately absent, so the UI does not offer a button that cannot
	// work.
	Registry *provider.Registry

	Store genstore.Store
	Blobs blob.Store

	// Inputs is where an uploaded image is staged between the file picker
	// and the generation that uses it.
	//
	// A CONCRETE *blob.Inputs AND NOT AN INTERFACE, which is the opposite of
	// Blobs beside it. Blobs is an interface because a generated image is
	// going to live in an S3 bucket one day; a staged input exists for the
	// minutes between a picker and an enqueue, on the machine running `ember
	// serve`, and there is no second place for it to be. A handler test
	// points it at t.TempDir(), which needs no fake.
	//
	// Nil is a server with no staging area -- which is what every handler
	// test that does not care about images passes -- and the upload route
	// answers 501 rather than panicking. cmd_serve always supplies one.
	Inputs *blob.Inputs

	// Queue is kicked after an enqueue, so a pushed request starts without
	// waiting for a poll, and asked to stop one that is generating.
	//
	// AN INTERFACE RATHER THAN *queue.Queue, and the reason is the same one
	// that put `scan` on the Server: the behaviour worth testing here is a
	// decision about a request, and reaching the real drain loop would mean
	// a handler test that rents something. A nil value is `ember serve`
	// before the loop exists and every handler test; cmd_serve always
	// supplies a real one, so there is no typed-nil hazard in production.
	Queue Drain

	// AllowPaid gates every provider that is not free. Checked here and again
	// in the planner, because this is not the last statement before a create
	// call and that one is.
	AllowPaid bool

	// Token is the credential every request has to carry.
	//
	// MANDATORY, AND THAT IS THE POINT. New refuses a zero value, so an
	// unauthenticated `ember serve` is not a configuration this program can
	// be in -- there is no flag that turns it off and no branch that skips
	// the check. The alternative was an optional token, which closes the
	// threat only for the operator who read the documentation; the one who
	// did not is the one with another account on the machine.
	//
	// It is node.Secret so that a stray slog.Any or %v prints a fingerprint
	// rather than the credential to a port that can rent GPUs. It never
	// reaches argv: cmd_serve reads it from EMBER_SERVE_TOKEN or
	// credentials.yaml, or mints one, because /proc/<pid>/cmdline is
	// world-readable and that is the same second-uid threat the token is for.
	Token node.Secret

	// Port is the port this server answers for. The guard compares the Host
	// header against it, so it has to be the real one.
	Port uint16

	// Socket is the local client channel: a unix socket at 0600 whose
	// requests the operating system has already authenticated.
	//
	// THE ZERO VALUE IS A SERVER WITH NO SOCKET, which is the stricter
	// configuration and therefore the right default -- every request then has
	// to carry the token, which is what every handler test in this package
	// wants and what it had before this existed. cmd_serve always supplies
	// one, resolved from the state directory through loopback.StateSocket;
	// ListenSocket refuses a zero value rather than inventing a path.
	//
	// It is a loopback.Socket and not a string for Bind's reason: the field
	// is unexported in that package and StateSocket is its only constructor,
	// so there is no path this server could be handed that ember did not
	// place inside a directory it owns.
	Socket loopback.Socket

	// Runs starts a run the way `ember up` does, on the one goroutine that can
	// rent.
	//
	// IT IS REACHABLE FROM THE SOCKET'S MUX AND FROM NOWHERE ELSE, which is
	// the whole shape of run.go: the route carries a config path and ceilings
	// that replace rather than narrow, so it must not exist on a channel a
	// browser or a forwarded port can reach. Nil is every handler test and a
	// server with no loop; the route answers 501.
	Runs Runs

	// Scan is what `ember ls` asks: every provider, for what ember's label is
	// on, classified by internal/reconcile.
	//
	// AN INJECTED FUNCTION AND NOT A SECOND SCANNER, which is the only shape
	// this route is allowed to have. `GET /api/nodes` projects
	// internal/reconcile's own findings; two scanners is two answers to "what
	// am I paying for", and the one that disagrees is the one that misses a
	// node. cmd_serve wires app.ScanWith over the registry and the journal it
	// already holds, so the web UI and `ember ls` share one classifier and one
	// config read.
	//
	// IT WAS NOT A FIELD AT ALL FOR THE LIFE OF THIS PACKAGE, and that is why
	// it is here. `scan` existed on the Server, was set by two test files
	// reaching past the constructor, and had no production assignment -- so
	// `GET /api/nodes` answered 501 in every shipped `ember serve`, and the
	// whole projection behind it (nodesView, findingView, the monthly storage
	// charge, the attention count) was reachable only from this package's own
	// tests. Nothing automated reported it: the functions all have callers,
	// and a nil function field is not a symbol TestNoProductionDeadCode
	// reasons about. The browser had no type for any of it either, which is
	// why nobody had ever seen the 501.
	//
	// IT REACHES PROVIDERS, SO IT IS NOT CACHED AND MUST NOT BE POLLED. Three
	// sweeps per call -- nodes, stores, leftovers -- over every configured
	// provider, on the HTTP request's own context, which is getOffers' shape
	// and getOffers' exposure. Memoising the answer would be local.DetectGPU's
	// defect pointed at money: a cached "RunPod said nothing" is exactly "I
	// could not check RunPod" rendering as a clean bill of health, and a cached
	// listing of what is billing is a stale bill. The page asks when a person
	// presses a button, as the offers panel does, and never on a timer.
	//
	// Nil is every handler test and a server built without one; the route
	// answers 501, as the upload and the run routes do for theirs.
	Scan func(ctx context.Context) (app.Scanned, error)

	// Dist serves the UI from disk instead of the embedded copy, for
	// development. Empty uses what was built into the binary.
	Dist string

	Log *slog.Logger
}

// Server is the HTTP surface.
type Server struct {
	opts Options
	mux  *http.ServeMux
	port string

	// sockMux is the loopback mux plus the routes only the operator's own
	// account may reach.
	//
	// TWO MUXES AND NOT ONE WITH A CONDITION IN IT. The difference between the
	// channels is a missing registration, exactly as the difference between
	// the two http.Servers is a missing ConnContext -- so the privileged route
	// is absent from the port rather than refused on it, and nothing a request
	// on the port can be composed of reaches a handler that does not exist for
	// it. It delegates `/` to s.mux rather than re-registering the API, so
	// there is one route table and a route added later is on both channels
	// without anybody remembering.
	sockMux *http.ServeMux

	// tokenDigest is what every presented credential is compared against.
	//
	// Computed once so the comparison is a fixed thirty-two bytes whatever
	// arrived -- see tokenMatches, where hashing both sides is what keeps the
	// token's length and its matching prefix out of the response time.
	tokenDigest [sha256.Size]byte

	// scan is injected so a handler test does not need a Docker daemon. The
	// production value asks the providers, and for the life of this package
	// there was not one: see Options.Scan, which New now copies here. A test
	// may still set this field directly, which is how the 502 path is driven
	// -- but a test setting it is no longer indistinguishable from `ember
	// serve` having one, because TestNew_CarriesTheScannerFromItsOptions
	// asserts the constructor carries it and a lab asserts the real binary
	// answers 200.
	scan func(ctx context.Context) (app.Scanned, error)
}

// New builds the server and its routes.
func New(o Options) (*Server, error) {
	if o.Store == nil {
		return nil, errors.New("webapi needs a store: a queue nothing records is a queue nothing can report")
	}
	// A SERVER WITH NO CREDENTIAL IS NOT CONSTRUCTIBLE. Every listener here
	// is loopback, and a loopback port is reachable by every uid on the
	// machine -- so a zero token is the configuration where the account next
	// door can queue a paid rental, and refusing it is what makes that
	// unrepresentable rather than merely documented. webapi.NewToken mints
	// one; there is no flag and no option that gets past this.
	if o.Token.IsZero() {
		return nil, errors.New("webapi needs a token: every listener is loopback and a loopback " +
			"port is reachable by every uid on this machine, so a server with no credential lets " +
			"another account queue a rental. Use webapi.NewToken, or set serve_token in " +
			"credentials.yaml")
	}
	if o.Port == 0 {
		o.Port = DefaultPort
	}
	s := &Server{
		opts:        o,
		mux:         http.NewServeMux(),
		sockMux:     http.NewServeMux(),
		port:        strconv.Itoa(int(o.Port)),
		tokenDigest: sha256.Sum256(o.Token.Bytes()),
		scan:        o.Scan,
	}
	s.routes()
	return s, nil
}

func (s *Server) log() *slog.Logger {
	if s.opts.Log != nil {
		return s.opts.Log
	}
	return slog.New(slog.DiscardHandler)
}

// URL is this server's address, with no credential in it.
func (s *Server) URL() string { return "http://127.0.0.1:" + s.port + "/" }

// TokenURL is the line `ember serve` prints for the operator to open.
//
// 127.0.0.1 AND NOT localhost, which matters because of what it sets: the
// cookie exchange.go hands back is scoped to the host in the address bar and
// not to the port, so a UI reached as "localhost" shares a cookie namespace
// with every other local development server on that name. The address does
// not.
//
// It is printed on stdout and nowhere else. Nothing logs it, no error body
// carries it, and the URL the browser is left on after the exchange has no
// token in it at all.
func (s *Server) TokenURL() string {
	return s.URL() + "?" + tokenParam + "=" + string(s.opts.Token.Bytes())
}

// TokenFingerprint is the token as it is safe to repeat.
//
// node.Secret's own discipline: carry the credential, print a fingerprint.
// `ember serve` prints this beside a configured token so an operator can tell
// which of two credentials a running server is using without the server
// echoing one back at them.
func (s *Server) TokenFingerprint() string { return s.opts.Token.Fingerprint() }

// Handler is the whole surface, exported so a test can drive it with
// httptest and no listener.
func (s *Server) Handler() http.Handler { return s.mux }

func (s *Server) routes() {
	s.mux.HandleFunc("GET /api/config", s.guarded(s.getConfig))
	s.mux.HandleFunc("GET /api/offers", s.guarded(s.getOffers))
	s.mux.HandleFunc("GET /api/nodes", s.guarded(s.getNodes))

	// GUARDED LIKE EVERYTHING ELSE, which for the one route that accepts
	// bytes is the part that matters: an ungated upload is a loopback port on
	// which any other uid on this machine can write files into the
	// operator's state directory.
	s.mux.HandleFunc("POST /api/inputs", s.guarded(s.postInput))

	s.mux.HandleFunc("POST /api/requests", s.guarded(s.postRequest))
	s.mux.HandleFunc("GET /api/requests", s.guarded(s.listRequests))
	s.mux.HandleFunc("GET /api/requests/{id}", s.guarded(s.getRequest))
	s.mux.HandleFunc("POST /api/requests/{id}/cancel", s.guarded(s.cancelRequest))
	s.mux.HandleFunc("GET /api/requests/{id}/events", s.guarded(s.getEvents))
	s.mux.HandleFunc("GET /api/requests/{id}/images/{job}/{index}", s.guarded(s.getImage))

	s.mux.HandleFunc("GET /ws", s.serveWS)

	// The UI itself, last, so it does not shadow an API route.
	s.mux.Handle("/", s.static())

	// AND THE SOCKET'S OWN TABLE: everything above, plus what only the
	// operator's own account may ask for.
	//
	// The delegation comes first in this function and the privileged pattern
	// second, which is not an ordering ServeMux cares about -- it prefers the
	// more specific pattern whichever order they were registered in -- but
	// reads in the order the channels differ: one table, plus one route.
	s.sockMux.Handle("/", s.mux)
	s.sockMux.HandleFunc("POST "+RunPath, s.guarded(s.postRun))
}

// guarded wraps a handler in the Host and Origin checks and the credential.
//
// EVERY ROUTE, not only the mutating ones. A read is a side channel: a page on
// another site cannot see the response, but it can time it and it can tell
// whether it succeeded -- and `GET /api/requests` is a list of the operator's
// prompts. There is nothing to be gained by letting a foreign page ask.
//
// ALL THREE OR NOTHING, and the credential comes LAST. The two header checks
// cost nothing and bound a browser; the credential is what bounds everything
// that is not one, which is where a second uid on this machine and a shared
// SSH forward both live. Neither replaces the other, so neither is skipped
// when the other passes -- a request that fails the Host check never reaches
// the compare, and one that passes it still has to prove who it is.
func (s *Server) guarded(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := guard(r, s.port); err != nil {
			s.log().Warn("refused a request that did not come from this server's own page",
				"path", r.URL.Path, "origin", r.Header.Get("Origin"), "host", r.Host, "err", err)
			s.fail(w, http.StatusForbidden, err)
			return
		}
		if err := s.authenticate(r); err != nil {
			s.refuseCredential(w, r)
			return
		}
		h(w, r)
	}
}

// Listen binds both loopback families and hands back what it got.
//
// SEPARATE FROM Serve SO THE PORT IS BOUND BEFORE ANYTHING IS PRINTED.
// `ember serve` announces a URL, and announcing one that is not listening yet
// means an operator who clicks it immediately gets connection-refused and
// concludes ember is broken. It also means "is another ember serve running?"
// is answered before the process claims to have started. A lab found this by
// racing its own helper against the announcement.
//
// Both families, because a v4-only listener is invisible to a browser that
// resolves localhost to ::1 first. One of the two missing is not fatal -- a
// machine with IPv6 disabled is ordinary -- and both missing is.
func (s *Server) Listen(ctx context.Context) ([]net.Listener, error) {
	lns := make([]net.Listener, 0, 2)
	for _, b := range []loopback.Bind{loopback.Port(s.opts.Port), loopback.Port6(s.opts.Port)} {
		ln, err := loopback.Listen(ctx, b)
		if err != nil {
			s.log().Warn("could not listen on one loopback family", "bind", b.String(), "err", err)
			continue
		}
		lns = append(lns, ln)
	}
	if len(lns) == 0 {
		return nil, fmt.Errorf("could not listen on 127.0.0.1:%s or [::1]:%s; is another ember serve running?",
			s.port, s.port)
	}
	return lns, nil
}

// Serve accepts on lns until ctx ends, then drains and closes them.
//
// IT RETURNS AFTER THE LISTENERS ARE CLOSED AND NOT AFTER THE RUNS ARE. The
// queue's own Run is what owns a rental's lifetime, and a caller must wait for
// both -- waiting only for this one would exit while a node is alive, which is
// the single most expensive bug available here. cmd_serve does both.
//
// THE PLAIN http.Server IS THE WHOLE DIFFERENCE FROM ServeSocket: no
// ConnContext, so nothing a request arriving here can do marks it as
// authenticated and the credential in auth.go is the only answer. The two are
// two servers rather than one serving both kinds of listener precisely so
// that this is a missing field rather than a condition.
func (s *Server) Serve(ctx context.Context, lns []net.Listener) error {
	return serveOn(ctx, &http.Server{Handler: s.mux, ReadHeaderTimeout: readHeaderTimeout}, lns...)
}

// serveOn is the accept-drain-close shape both channels share.
//
// Factored out so the socket's server cannot come to shut down differently
// from the port's: one process holding a rental has one correct shutdown
// order, and two copies of it is one place for the expensive half to drift.
func serveOn(ctx context.Context, srv *http.Server, lns ...net.Listener) error {
	errs := make(chan error, len(lns))
	var wg sync.WaitGroup
	for _, ln := range lns {
		wg.Add(1)
		go func(ln net.Listener) {
			defer wg.Done()
			if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errs <- err
			}
		}(ln)
	}

	<-ctx.Done()

	// WithoutCancel: ctx is already done, and a shutdown on a cancelled
	// context closes connections instantly instead of draining them.
	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(sctx)
	wg.Wait()

	select {
	case err := <-errs:
		return err
	default:
		return nil
	}
}

// --- handlers ---

func (s *Server) getConfig(w http.ResponseWriter, r *http.Request) {
	var caps *sdserver.Capabilities
	if c, ok, err := s.opts.Store.Caps(r.Context()); err != nil {
		// Reported and not fatal: a UI without the capability list offers
		// free-text controls instead of menus, which is worse than ideal and
		// better than a blank page.
		s.log().Warn("could not read the cached capabilities", "err", err)
	} else if ok {
		caps = &c
	}
	s.ok(w, viewConfig(s.opts.Config, s.registry(), s.opts.AllowPaid, caps))
}

func (s *Server) getOffers(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	// Built from the config and narrowed by the query, never widened: the
	// price ceiling an operator set is the one a listing is filtered by, so
	// the UI cannot show them a card they are not allowed to rent.
	cfg := s.opts.Config
	cfg.GPU = q.Get("gpu")
	if v := q.Get("max_hourly_usd"); v != "" {
		cfg.MaxHourlyUSD = v
	}
	filter, err := cfg.Filter()
	if err != nil {
		s.fail(w, http.StatusBadRequest, err)
		return
	}
	// A listing's ceiling is still the operator's: a query asking for a
	// higher one gets theirs.
	if hourly, _, berr := s.opts.Config.Budgets(); berr == nil && hourly > 0 {
		if filter.MaxPrice == 0 || filter.MaxPrice > hourly {
			filter.MaxPrice = hourly
		}
	}
	filter.CPUOnly = q.Get("cpu_only") == "1"

	ls, err := app.Offers(r.Context(), s.registry(), filter, q.Get("provider"))
	if err != nil {
		s.fail(w, http.StatusBadRequest, err)
		return
	}
	s.ok(w, viewOffers(ls))
}

// getNodes is `ember ls` for the browser.
//
// AN UNREACHABLE PROVIDER IS A 200 AND NOT A 502, which is the one thing worth
// reading here. reconcile emits a KindUnreachable finding per provider it could
// not ask, so a sweep that reached one provider and not another is a complete
// answer about the first and a named failure about the second -- and losing the
// first in order to report the second would be "I could not check RunPod"
// hiding what the local daemon just said. What a 502 means is that the sweep
// could not START: the journal of live nodes would not read, so there is
// nothing to compare a provider's answer against and every classification
// below would be wrong in the direction that says a billing node is fine.
//
// THE 501 IS A SERVER WITH NO SCANNER, and it is not a configuration `ember
// serve` can be in: cmd_serve always wires one. It stays because the field is
// injectable and a handler test passes none -- the same shape as the upload and
// run routes -- and because answering 501 is better than panicking in a
// handler.
func (s *Server) getNodes(w http.ResponseWriter, r *http.Request) {
	if s.scan == nil {
		s.fail(w, http.StatusNotImplemented, errors.New("this server was built without a node scanner, "+
			"so it cannot say what is billing; `ember ls` asks the providers directly"))
		return
	}
	sc, err := s.scan(r.Context())
	if err != nil {
		s.fail(w, http.StatusBadGateway, err)
		return
	}
	s.ok(w, viewNodes(sc))
}

// postRequest queues a batch.
//
// Every refusal here happens before anything is rented, which is the same
// discipline app.NewPlan keeps one layer down. The expensive mistake this
// prevents is a sampler typo discovered a minute into a rented card, and the
// cached capabilities are what make it preventable at all.
func (s *Server) postRequest(w http.ResponseWriter, r *http.Request) {
	var body newRequest
	dec := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	// A misspelled field is a typo and not a comment -- the same decision
	// config.Load makes with KnownFields. A UI sending `step` instead of
	// `steps` would otherwise generate at the server's default and the
	// operator would never know why.
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		s.fail(w, http.StatusBadRequest, fmt.Errorf("read the request: %w", err))
		return
	}

	req, err := body.toRequest()
	if err != nil {
		s.fail(w, http.StatusBadRequest, err)
		return
	}
	if req.TTL == 0 {
		// The operator's own default rather than a refusal: they set a ttl in
		// their config, and a UI that did not send one meant that.
		req.TTL = s.opts.Config.TTL
	}

	if err := s.refusePaid(req.Provider); err != nil {
		s.fail(w, http.StatusForbidden, err)
		return
	}
	if len(s.opts.Config.SD.Serve) == 0 {
		s.fail(w, http.StatusConflict, errors.New("this configuration has no sd.serve block, so "+
			"there is no generation server to start and a queued request would have nowhere to run"))
		return
	}
	if err := s.validateJobs(r.Context(), req); err != nil {
		s.fail(w, http.StatusBadRequest, err)
		return
	}

	id, err := s.opts.Store.Enqueue(r.Context(), req)
	if err != nil {
		s.fail(w, http.StatusBadRequest, err)
		return
	}
	if s.opts.Queue != nil {
		// So a pushed request starts now rather than at the next poll.
		s.opts.Queue.Kick()
	}

	s.write(w, http.StatusAccepted, map[string]string{"id": string(id)})
}

// refusePaid is the --allow-paid gate at the edge.
//
// Also enforced in the planner, at the last statement before a create call.
// Two checks because this one can be gone around -- a request that reached the
// store another way never passed through here -- and the money must not depend
// on which door it came in by.
func (s *Server) refusePaid(name string) error {
	if s.opts.AllowPaid || name == "" {
		return nil
	}
	p, err := s.registry().Get(node.ProviderID(name))
	if err != nil {
		return err
	}
	if p.Caps().Free {
		return nil
	}
	return fmt.Errorf("provider %s is not free and ember serve was started without --allow-paid, "+
		"so it will not rent anything. Restart ember serve with --allow-paid to let the web UI "+
		"spend money", name)
}

// validateJobs refuses a batch before it is queued.
//
// TWO HALVES, AND ONLY ONE OF THEM NEEDS A CARD. The static half --
// sdserver.ValidateStatic -- is the same function config.ParseSD uses, so a
// batch from a browser and one from a file are held to the same rules. The
// other half needs the server's own capability list, which exists only after
// a rental; it is read from the cache when there is one, and skipped before
// this process's first rental.
//
// WHAT THE SKIPPED HALF COSTS, because this comment used to wave at it. The
// caps half is caught on the node, by app.Served.Generate, after the card is
// rented and the weights are in -- not by app.NewPlan, which calls only the
// static half. So every refusal `haveCaps` guards is a refusal an operator
// pays a rental for on the first request of a process, and a geometry is the
// one case where that was also a defect the browser could see: a width Go's
// `int` holds and the page's `i32` does not took the whole request listing
// with it. sdserver.MaxAxis is the static bound that closes it; the samplers,
// the schedulers and the format lists have no static answer, because a list
// ember has not been told is not a list it can check against.
func (s *Server) validateJobs(ctx context.Context, req genstore.Request) error {
	preview, err := sdserver.ParsePreview(req.Preview)
	if err != nil {
		return fmt.Errorf("preview: %w", err)
	}
	interval, err := sdserver.ParsePreviewInterval(req.PreviewInterval)
	if err != nil {
		return fmt.Errorf("preview_interval: %w", err)
	}

	caps, haveCaps, cerr := s.opts.Store.Caps(ctx)
	if cerr != nil {
		s.log().Warn("could not read the cached capabilities; a bad sampler will be caught "+
			"after the node is up instead of now", "err", cerr)
	}

	// The image inputs first, because ValidateStatic builds the request body
	// and the body refuses a reference with no bytes. It is also the
	// cheapest refusal in the feature: a reference nothing staged is a
	// request that can never generate, and this is before it is even
	// queued.
	jobs, err := s.resolveInputs(req.Jobs)
	if err != nil {
		return err
	}

	for i, sd := range jobs {
		if err := sdserver.ValidateStatic(sd, preview, interval); err != nil {
			return fmt.Errorf("jobs[%d]: %w", i, err)
		}
		if haveCaps {
			if err := sdserver.Validate(sd, caps); err != nil {
				return fmt.Errorf("jobs[%d]: %w", i, err)
			}
		}
	}
	return nil
}

func (s *Server) listRequests(w http.ResponseWriter, r *http.Request) {
	f := genstore.Filter{Status: genstore.Status(r.URL.Query().Get("status"))}
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			s.fail(w, http.StatusBadRequest, fmt.Errorf("limit %q is not a positive number", v))
			return
		}
		f.Limit = n
	}
	rs, err := s.opts.Store.List(r.Context(), f)
	if err != nil {
		s.fail(w, http.StatusInternalServerError, err)
		return
	}

	out := make([]requestView, 0, len(rs))
	for _, req := range rs {
		// Without the images: a listing is the queue, and reading every
		// request's gallery to render it would be a query per row.
		out = append(out, viewRequest(req, nil))
	}

	running, _ := s.running()
	s.ok(w, map[string]any{"requests": out, "running": running})
}

func (s *Server) running() (string, bool) {
	if s.opts.Queue == nil {
		return "", false
	}
	id, ok := s.opts.Queue.Running()
	return string(id), ok
}

func (s *Server) getRequest(w http.ResponseWriter, r *http.Request) {
	id := genstore.ReqID(r.PathValue("id"))
	req, err := s.opts.Store.Get(r.Context(), id)
	if err != nil {
		s.failStore(w, err)
		return
	}
	ims, err := s.opts.Store.Images(r.Context(), id)
	if err != nil {
		s.fail(w, http.StatusInternalServerError, err)
		return
	}
	s.ok(w, viewRequest(req, ims))
}

// cancelRequest stops a request, queued or generating.
//
// IT USED TO REFUSE A RUNNING ONE, with a 409 whose stated reason was that
// cancelling it "would mean destroying that node, which would take every
// other request on the same rental with it". That was a true sentence about
// cancelling a RENTAL and a false one about cancelling a JOB, and the
// difference is the whole shape of the served path: sd-server's own
// /jobs/{id}/cancel ends one generation and leaves the process, the node and
// the resident weights exactly as they were. The mechanism existed the whole
// time -- sdserver.Client.Cancel, written, covered, and called by nothing but
// its own test -- so the refusal was protecting against a thing nobody had
// built. queue.Cancel is what the old message was missing: a signal that ends
// one batch without going near the drain loop's context.
//
// THE TWO CASES WRITE THE ROW IN DIFFERENT PLACES, deliberately.
//
// A queued request has no rental and nothing to tell, so it is terminal the
// moment this handler says so. A running one is still generating when this
// returns: the queue is polling sd-server for the job's own answer, and the
// status it ends up with -- how far it got, whether the job in flight could be
// stopped -- is the queue's to record when the batch returns. Writing
// "cancelled" here as well would be a second writer for one row, racing the
// one that actually knows, and the loser would overwrite the truth.
func (s *Server) cancelRequest(w http.ResponseWriter, r *http.Request) {
	id := genstore.ReqID(r.PathValue("id"))

	req, err := s.opts.Store.Get(r.Context(), id)
	if err != nil {
		s.failStore(w, err)
		return
	}
	if req.Status.Terminal() {
		s.fail(w, http.StatusConflict, fmt.Errorf("request %s already ended as %s", id, req.Status))
		return
	}

	if req.Status == genstore.StatusRunning {
		if !s.cancelRunning(id) {
			// Running in the row and not running in this process. Reported
			// rather than marked cancelled, for the reason `ember ls` exits
			// non-zero on a provider it could not ask: "I could not stop it"
			// and "it is stopped" are different answers, and the first must
			// not render as the second over a card that may still be
			// billing.
			//
			// TWO STATES REACH HERE AND ONLY ONE OF THEM IS THE ONE THE
			// MESSAGE USED TO DESCRIBE. The row reads `running` from
			// ClaimNext and the queue registers its stop channel in
			// `generate`, so the whole of provisioning -- the plan, the
			// create, the gpu probe, the asset verify, the model load -- is
			// a window in which the row is cancellable and `Cancel` has
			// nothing to signal. That is the ORDINARY way to get here: an
			// operator who clicks cancel while a node is coming up. The
			// other is a row left `running` by an ember that is gone, which
			// queue.Run's Interrupt sweep clears on the next start.
			//
			// One message for both told the first one to restart
			// `ember serve`, which destroys the rental they are paying for
			// in order to reconcile a request that was seconds from
			// generating -- a destructive remedy for a condition that
			// resolves itself. The queue can tell them apart without a
			// counter, because there is one drain goroutine and a rental
			// lives inside one iteration of its loop, so the two answers are
			// two answers.
			if s.holdingRequest(id) {
				s.fail(w, http.StatusConflict, fmt.Errorf("request %s has a rental coming up "+
					"and has not started generating yet, so there is nothing to stop; "+
					"ask again once it is generating, or leave it queued -- do not restart "+
					"ember serve, which would destroy the rental it is waiting for", id))
				return
			}
			s.fail(w, http.StatusConflict, fmt.Errorf("request %s is recorded as generating, "+
				"but no rental in this process holds it; restart ember serve to have "+
				"the queue reconcile it", id))
			return
		}
		// Still running when this returns -- see above. The status is the one
		// the row currently holds, which is honest, and `cancelling` is what
		// says a stop is on its way.
		s.ok(w, map[string]any{
			"id": string(id), "status": string(req.Status), "cancelling": true,
		})
		return
	}

	if err := s.opts.Store.Update(r.Context(), id, func(rq *genstore.Request) {
		rq.Status = genstore.StatusCancelled
		rq.Ended = time.Now()
	}); err != nil {
		s.fail(w, http.StatusInternalServerError, err)
		return
	}
	// AND THE SIGNAL TOO, although the row said queued. ClaimNext is one
	// statement and it can run between the Get above and the Update that
	// follows it, so a request read as queued may be generating by now --
	// in which case the row this handler just wrote is about to be
	// overwritten by the queue's own finish, and the batch would carry on.
	// Signalling closes that window: if it landed, the generation stops and
	// the queue records it; if it did not, there was nothing running and
	// this is a no-op.
	s.cancelRunning(id)
	s.ok(w, map[string]any{"id": string(id), "status": string(genstore.StatusCancelled)})
}

// cancelRunning asks the drain loop to stop one request, tolerating no queue.
//
// A nil Queue is a handler test and `ember serve`'s own shape before the loop
// starts; running() already treats it the same way, so the two cannot
// disagree about whether there is a loop to talk to.
func (s *Server) cancelRunning(id genstore.ReqID) bool {
	if s.opts.Queue == nil {
		return false
	}
	return s.opts.Queue.Cancel(id)
}

// holdingRequest asks whether a rental for this request is in this process,
// tolerating no queue the way cancelRunning and running() do.
//
// NO LOOP IS NOT "A RENTAL IS COMING UP", which is why the nil case answers
// false rather than the gentler sentence: with no drain goroutine there is
// nothing to come up, and the gentler sentence would tell an operator to wait
// for a rental nothing is provisioning. The uninformative answer is the one
// that does not promise anything -- Caps.PublicSSH's direction, in a message.
func (s *Server) holdingRequest(id genstore.ReqID) bool {
	if s.opts.Queue == nil {
		return false
	}
	return s.opts.Queue.Holding(id)
}

// getEvents replays from a cursor.
//
// THE SAME CODE PATH AS THE LIVE STREAM, which is the point of storing events
// as rows: a page reload and a dropped websocket both come back here with the
// last sequence number they saw, and neither can be told anything the other
// could not. A ring buffer would have to answer "your cursor is gone", and
// nothing sensible can be done with that answer.
func (s *Server) getEvents(w http.ResponseWriter, r *http.Request) {
	id := genstore.ReqID(r.PathValue("id"))
	after, err := cursor(r.URL.Query().Get("after"))
	if err != nil {
		s.fail(w, http.StatusBadRequest, err)
		return
	}
	es, err := s.opts.Store.Events(r.Context(), id, after)
	if err != nil {
		s.fail(w, http.StatusInternalServerError, err)
		return
	}
	// An empty list and never null; see the same line in ws.go.
	if es == nil {
		es = []report.Event{}
	}
	next := after
	if n := len(es); n > 0 {
		next = es[n-1].Seq
	}
	s.ok(w, map[string]any{"next": next, "events": es})
}

func cursor(v string) (int64, error) {
	if v == "" {
		return 0, nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("after %q is not a sequence number", v)
	}
	return n, nil
}

// getImage serves one file a request produced.
func (s *Server) getImage(w http.ResponseWriter, r *http.Request) {
	id := genstore.ReqID(r.PathValue("id"))
	job, jerr := strconv.Atoi(r.PathValue("job"))
	idx, ierr := strconv.Atoi(r.PathValue("index"))
	if jerr != nil || ierr != nil {
		s.fail(w, http.StatusBadRequest, errors.New("the job and index must be numbers"))
		return
	}
	preview := r.URL.Query().Get("preview") == "1"

	ims, err := s.opts.Store.Images(r.Context(), id)
	if err != nil {
		s.fail(w, http.StatusInternalServerError, err)
		return
	}
	var key string
	for _, im := range ims {
		if im.Job == job && im.Index == idx && im.Preview == preview {
			key = im.Key
			break
		}
	}
	if key == "" {
		s.fail(w, http.StatusNotFound, fmt.Errorf("request %s has no image %d/%d", id, job, idx))
		return
	}
	if s.opts.Blobs == nil {
		s.fail(w, http.StatusNotImplemented, errors.New("this server was built without a blob store"))
		return
	}

	f, err := s.opts.Blobs.Open(r.Context(), key)
	if errors.Is(err, blob.ErrNotFound) {
		// The row says it exists and the bytes do not. Said plainly, because
		// the operator may have moved or deleted the directory and the row is
		// not wrong about what was produced.
		s.fail(w, http.StatusNotFound, fmt.Errorf("%s was recorded but is not on disk", key))
		return
	}
	if err != nil {
		s.fail(w, http.StatusInternalServerError, err)
		return
	}
	defer func() { _ = f.Close() }()

	// Declared, never sniffed: these are bytes a rented machine produced, and
	// letting a browser decide what they are is letting the node decide.
	//
	// DERIVED FROM THE KEY AND NO LONGER THE LITERAL image/png. This route
	// served `image/png` unconditionally, which was true of everything ember
	// produced and stopped being true the moment a second workload kind made
	// a wav: a .wav declared as a PNG is a file a browser will not play and,
	// with nosniff beside it, cannot be talked into playing -- the failure
	// #36 predicted in words ("audio served through that route is labelled as
	// an image and, correctly, will not play").
	//
	// A CLOSED SWITCH AND NOT mime.TypeByExtension, which is the shorter
	// answer and the wrong one twice over: that function reads
	// /etc/mime.types, so what ember declares about bytes a rented machine
	// produced would depend on which packages are installed on the operator's
	// laptop, and it answers for every extension there is where the set ember
	// writes is two. The key's extension is ember's own, written by one of
	// two drivers, so a switch is a list of what this program can produce
	// rather than a guess about a filename.
	ctype, err := artifactType(key)
	if err != nil {
		// Refused rather than guessed, and a 500 rather than a 415, because
		// the row is ember's own: an extension nothing here writes means a
		// writer and this switch have come apart, which is a defect and not a
		// bad request. Serving it as octet-stream would be a download prompt
		// for an image, which reads as ember being broken in a way that
		// points nowhere.
		s.fail(w, http.StatusInternalServerError, err)
		return
	}
	w.Header().Set("Content-Type", ctype)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if preview {
		// A preview is written over as sampling proceeds, so a cached one is
		// a progress display that appears to freeze.
		w.Header().Set("Cache-Control", "no-store")
	} else {
		// A result never changes, so it is worth caching -- a gallery of
		// twenty images should not re-fetch them on every render.
		w.Header().Set("Cache-Control", "private, max-age=31536000, immutable")
	}
	// The key's own basename, and not the key with ".png" stuck on the end.
	// ServeContent reads the name only to guess a type when none is declared,
	// and one is -- so what this changes is nothing on the wire and
	// everything about what a reader of the line believes it does.
	http.ServeContent(w, r, path.Base(key), time.Time{}, f)
}

// artifactType is what ember declares the bytes under a key to be.
//
// THE EXTENSIONS ARE THE WRITERS', one arm each, and the writer is named so
// that deleting a driver and leaving an arm behind is visible:
//
//   - .png -- sdserver.WriteImages, every generated image and every preview
//     frame. `output_format` can ask sd-server for a jpg, and that is NOT a
//     third arm by omission: WriteImages names every file it writes `.png`
//     whatever the format of the bytes inside it, so a jpg is already served
//     as a PNG today. That is this same defect one driver over, it predates
//     this function, and it is named here rather than fixed because the name
//     is a key already in operators' databases.
//   - .wav -- the speech driver, through tts.Ext.
//
// Nothing else, because nothing else writes an artifact row.
func artifactType(key string) (string, error) {
	switch strings.ToLower(path.Ext(key)) {
	case ".png":
		return "image/png", nil
	case tts.Ext:
		// audio/wav and not audio/x-wav or audio/wave: it is the spelling
		// every browser's <audio> accepts, and the x- one is the historical
		// form.
		return "audio/wav", nil
	}
	return "", fmt.Errorf("%s has an extension ember does not declare a type for; "+
		"a browser must never be left to guess what a rented machine produced", key)
}

// --- plumbing ---

func (s *Server) registry() *provider.Registry {
	if s.opts.Registry != nil {
		return s.opts.Registry
	}
	return provider.NewRegistry()
}

func (s *Server) ok(w http.ResponseWriter, v any) { s.write(w, http.StatusOK, v) }

// write declares the response, sends the status, and encodes the body, in
// that order.
//
// THE ORDER IS THE WHOLE FUNCTION, and the reason it is one function rather
// than a rule four call sites each have to remember. net/http snapshots the
// header map when WriteHeader runs, so a Content-Type set afterwards is
// thrown away and the body goes out as http.DetectContentType's guess --
// which for a JSON object is `text/plain; charset=utf-8`. `ok` wrote the
// status first and set the type second, so every 200 this server has ever
// sent was declared text/plain over a JSON body, while every *error* was
// declared correctly because `fail` happened to do it the other way round:
// the declaration was exactly inverted from what the code read as.
// postRequest's 202 and postInput's 201 had the same slip. One name for a
// path, not one per call site -- the journal's "nodes" subdirectory rule,
// reached through a header.
//
// NOTHING IN THE PROGRAM COULD NOTICE, which is why it survived. web/'s
// api.rs reads the body as text and parses it unconditionally, with no
// resp.json() and no content-type check anywhere in the page, so the seam's
// data half was right and only its declared type was wrong.
//
// nosniff for the reason getImage sets it: a declared type a browser is
// allowed to second-guess is not a declared type. Said once here, where every
// JSON response goes through, rather than reasoned about per route -- and the
// route that already had the argument written above it is the one that
// silently lost the header when the ordering broke.
func (s *Server) write(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(code)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		// The status is already sent, so there is nothing to tell the client.
		// A dropped connection is the ordinary cause. The status is logged
		// because a 4xx or 5xx that failed to write is the case that matters:
		// the operator never got the reason.
		s.log().Debug("could not write a response", "status", code, "err", err)
	}
}

// fail answers with a status and the reason.
//
// THE REASON, in full. This server is reachable only from the operator's own
// machine, so there is nobody to leak an internal message to -- and the
// alternative is a UI that says "something went wrong" over an error that
// names the exact config key.
func (s *Server) fail(w http.ResponseWriter, code int, err error) {
	s.write(w, code, map[string]string{"error": err.Error()})
}

func (s *Server) failStore(w http.ResponseWriter, err error) {
	if errors.Is(err, genstore.ErrNotFound) {
		s.fail(w, http.StatusNotFound, err)
		return
	}
	s.fail(w, http.StatusInternalServerError, err)
}
