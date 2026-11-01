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
package webapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
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
	"github.com/Legion112/ember/internal/queue"
	"github.com/Legion112/ember/internal/report"
	"github.com/Legion112/ember/internal/sdserver"
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

	// Queue is kicked after an enqueue, so a pushed request starts without
	// waiting for a poll.
	Queue *queue.Queue

	// AllowPaid gates every provider that is not free. Checked here and again
	// in the planner, because this is not the last statement before a create
	// call and that one is.
	AllowPaid bool

	// Port is the port this server answers for. The guard compares the Host
	// header against it, so it has to be the real one.
	Port uint16

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

	// scan is injected so a handler test does not need a Docker daemon. The
	// production value asks the providers.
	scan func(ctx context.Context) (app.Scanned, error)
}

// New builds the server and its routes.
func New(o Options) (*Server, error) {
	if o.Store == nil {
		return nil, errors.New("webapi needs a store: a queue nothing records is a queue nothing can report")
	}
	if o.Port == 0 {
		o.Port = DefaultPort
	}
	s := &Server{opts: o, mux: http.NewServeMux(), port: strconv.Itoa(int(o.Port))}
	s.routes()
	return s, nil
}

func (s *Server) log() *slog.Logger {
	if s.opts.Log != nil {
		return s.opts.Log
	}
	return slog.New(slog.DiscardHandler)
}

// URL is what `ember serve` prints.
func (s *Server) URL() string { return "http://127.0.0.1:" + s.port + "/" }

// Handler is the whole surface, exported so a test can drive it with
// httptest and no listener.
func (s *Server) Handler() http.Handler { return s.mux }

func (s *Server) routes() {
	s.mux.HandleFunc("GET /api/config", s.guarded(s.getConfig))
	s.mux.HandleFunc("GET /api/offers", s.guarded(s.getOffers))
	s.mux.HandleFunc("GET /api/nodes", s.guarded(s.getNodes))

	s.mux.HandleFunc("POST /api/requests", s.guarded(s.postRequest))
	s.mux.HandleFunc("GET /api/requests", s.guarded(s.listRequests))
	s.mux.HandleFunc("GET /api/requests/{id}", s.guarded(s.getRequest))
	s.mux.HandleFunc("POST /api/requests/{id}/cancel", s.guarded(s.cancelRequest))
	s.mux.HandleFunc("GET /api/requests/{id}/events", s.guarded(s.getEvents))
	s.mux.HandleFunc("GET /api/requests/{id}/images/{job}/{index}", s.guarded(s.getImage))

	s.mux.HandleFunc("GET /ws", s.serveWS)

	// The UI itself, last, so it does not shadow an API route.
	s.mux.Handle("/", s.static())
}

// guarded wraps a handler in the Origin and Host checks.
//
// EVERY ROUTE, not only the mutating ones. A read is a side channel: a page on
// another site cannot see the response, but it can time it and it can tell
// whether it succeeded -- and `GET /api/requests` is a list of the operator's
// prompts. There is nothing to be gained by letting a foreign page ask.
func (s *Server) guarded(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := guard(r, s.port); err != nil {
			s.log().Warn("refused a request that did not come from this server's own page",
				"path", r.URL.Path, "origin", r.Header.Get("Origin"), "host", r.Host, "err", err)
			s.fail(w, http.StatusForbidden, err)
			return
		}
		h(w, r)
	}
}

// Serve binds both loopback families and serves until ctx ends.
//
// IT RETURNS AFTER THE LISTENERS ARE CLOSED AND NOT AFTER THE RUNS ARE. The
// queue's own Run is what owns a rental's lifetime, and a caller must wait for
// both -- waiting only for this one would exit while a node is alive, which is
// the single most expensive bug available here. cmd_serve does both, in that
// order.
func (s *Server) Serve(ctx context.Context) error {
	lns := make([]net.Listener, 0, 2)
	// Both families, for the reason the package comment gives.
	for _, b := range []loopback.Bind{loopback.Port(s.opts.Port), loopback.Port6(s.opts.Port)} {
		ln, err := loopback.Listen(ctx, b)
		if err != nil {
			// One family missing is not fatal -- a machine with IPv6 disabled
			// is ordinary -- but both are.
			s.log().Warn("could not listen on one loopback family", "bind", b.String(), "err", err)
			continue
		}
		lns = append(lns, ln)
	}
	if len(lns) == 0 {
		return fmt.Errorf("could not listen on 127.0.0.1:%s or [::1]:%s; is another ember serve running?",
			s.port, s.port)
	}

	srv := &http.Server{Handler: s.mux, ReadHeaderTimeout: readHeaderTimeout}

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

func (s *Server) getNodes(w http.ResponseWriter, r *http.Request) {
	if s.scan == nil {
		s.fail(w, http.StatusNotImplemented, errors.New("this server was built without a node scanner"))
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

	w.WriteHeader(http.StatusAccepted)
	s.write(w, map[string]string{"id": string(id)})
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
// this process's first rental, where app.NewPlan will catch it instead.
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

	for i, j := range req.Jobs {
		sd := j.SD()
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

// cancelRequest stops a request that has not started.
//
// WHAT IT CANNOT DO, said plainly rather than implied: it does not stop a
// generation that is already running. Doing that means cancelling the drain
// loop's context, which destroys the node -- and a node destroyed mid-batch
// takes every later request on that rental with it. A queued request is
// cancelled here; a running one is stopped by stopping ember serve, which is
// what Ctrl-C has always done.
func (s *Server) cancelRequest(w http.ResponseWriter, r *http.Request) {
	id := genstore.ReqID(r.PathValue("id"))

	req, err := s.opts.Store.Get(r.Context(), id)
	if err != nil {
		s.failStore(w, err)
		return
	}
	switch {
	case req.Status.Terminal():
		s.fail(w, http.StatusConflict, fmt.Errorf("request %s already ended as %s", id, req.Status))
		return
	case req.Status == genstore.StatusRunning:
		s.fail(w, http.StatusConflict, fmt.Errorf("request %s is generating on a rented node. "+
			"Cancelling it would mean destroying that node, which would take every other request "+
			"on the same rental with it; stop ember serve to end the rental", id))
		return
	}

	if err := s.opts.Store.Update(r.Context(), id, func(rq *genstore.Request) {
		rq.Status = genstore.StatusCancelled
		rq.Ended = time.Now()
	}); err != nil {
		s.fail(w, http.StatusInternalServerError, err)
		return
	}
	s.ok(w, map[string]string{"id": string(id), "status": string(genstore.StatusCancelled)})
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
	w.Header().Set("Content-Type", "image/png")
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
	http.ServeContent(w, r, strings.TrimSuffix(key, "/")+".png", time.Time{}, f)
}

// --- plumbing ---

func (s *Server) registry() *provider.Registry {
	if s.opts.Registry != nil {
		return s.opts.Registry
	}
	return provider.NewRegistry()
}

func (s *Server) ok(w http.ResponseWriter, v any) {
	w.WriteHeader(http.StatusOK)
	s.write(w, v)
}

func (s *Server) write(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		// The status is already sent, so there is nothing to tell the client.
		// A dropped connection is the ordinary cause.
		s.log().Debug("could not write a response", "err", err)
	}
}

// fail answers with a status and the reason.
//
// THE REASON, in full. This server is reachable only from the operator's own
// machine, so there is nobody to leak an internal message to -- and the
// alternative is a UI that says "something went wrong" over an error that
// names the exact config key.
func (s *Server) fail(w http.ResponseWriter, code int, err error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	if werr := json.NewEncoder(w).Encode(map[string]string{"error": err.Error()}); werr != nil {
		s.log().Debug("could not write an error response", "err", werr)
	}
}

func (s *Server) failStore(w http.ResponseWriter, err error) {
	if errors.Is(err, genstore.ErrNotFound) {
		s.fail(w, http.StatusNotFound, err)
		return
	}
	s.fail(w, http.StatusInternalServerError, err)
}
