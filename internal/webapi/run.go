package webapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/Legion112/ember/internal/app"
	"github.com/Legion112/ember/internal/genstore"
)

// THE PRIVILEGED ROUTE, AND WHY IT DOES NOT EXIST ON THE PORT.
//
// Everything else in this package is written against a browser: a request
// names a provider, a card and a batch of jobs, and nothing else -- no image,
// no store, no ports, no serve argv, no `extra`, and above all no config
// path, because "the browser would be naming which file sets its own limits".
// This route breaks every one of those: it carries a config path, argv, and
// ceilings that REPLACE the file's rather than narrowing them. It is `ember
// up`'s command line.
//
// SO IT IS NOT REFUSED AT RUNTIME, IT IS ABSENT. The route is registered on a
// second mux, the one ServeSocket hands to its own http.Server, and the
// loopback port's mux never learns the pattern -- so a page, a curl on the
// port, a forwarded `ssh -L` and a second uid with the token all get whatever
// `/` answers, because there is no handler for them to reach. A check on
// arrivedOnSocket would have been one line and would have been the wrong
// shape: it makes the route exist, and a route that exists is one somebody can
// make reachable by moving a condition. The two muxes differ by one
// registration, as the two http.Servers differ by one field.
//
// THE SECOND MUX DELEGATES RATHER THAN RE-REGISTERING. sockMux handles `/` with
// s.mux, so there is ONE route table plus one extra pattern: a route added to
// the API is on both channels without anybody remembering, and the socket
// cannot come to serve a stale set. Go's ServeMux prefers the more specific
// pattern, so "POST /api/runs" wins over "/" on the socket and nothing else
// changes.
//
// AND WHAT AUTHORISES IT IS NOT A HEADER. The channel is a unix socket at 0600
// in a directory at 0700, so connect(2) refused every account but the
// operator's own before ember saw a byte -- which is the same authority as
// typing `ember up` in their own shell. See socket.go for why "arrived on the
// socket" is a context value nothing outside this package can construct.

// RunPath is the privileged route, named once.
//
// EXPORTED BECAUSE THE CLIENT IS IN THIS REPOSITORY. internal/cli builds the
// request, and a path spelled in two places is the journal's "nodes"
// subdirectory: they drift, and the client then posts to a route that answers
// with the UI's own HTML.
const RunPath = "/api/runs"

// Runs is what the privileged channel can do: start a run the way `ember up`
// does.
//
// ONE METHOD, AND THIS PACKAGE IMPLEMENTS NONE OF IT -- the Drain seam's own
// shape and for the same reason. What makes "a handler cannot start a rental"
// true is that webapi has no way to name one: it does not construct a plan, a
// provider, a budget or a node, and the only thing on the far side of this
// interface is queue.DirectRunner, which hands the run to the one goroutine
// that can rent. A nil value is every handler test in this package and `ember
// serve` before the loop exists; the route answers 501 rather than panicking.
type Runs interface {
	// Start plans the run, records it, and returns once the one goroutine that
	// can rent has taken it.
	//
	// IT BLOCKS WHILE THAT GOROUTINE IS BUSY, which is the honest answer to
	// "rent now" from a process that is already renting -- the alternatives
	// are a refusal, a backlog `up` does not mean, or a second GPU. ctx is the
	// HTTP request's, so a client that goes away cancels its own wait and
	// nothing has been rented.
	Start(ctx context.Context, ask app.RunAsk) (genstore.ReqID, error)
}

// RunBody is the wire shape of a run, exported because both sides are here.
//
// ONE TYPE FOR BOTH ENDS. The Go-to-Rust seam in this program is two
// declarations that can disagree, and this repository has paid for that twice
// -- a nil slice that serde read as a missing field, and a Go `int` a Rust
// `i32` could not hold. There is no second language on this channel: the client
// is internal/cli, so sharing the struct costs nothing and removes the whole
// class.
//
// DURATIONS ARE STRINGS, because an operator can drive this channel with curl
// and `"ttl": "2h"` is the spelling they already wrote in their config.
// time.ParseDuration reads back exactly what Duration.String wrote, so the
// client's round trip is lossless -- and the refusal for anything else is
// reachable from a hand-written request rather than being an unprovokable
// branch.
type RunBody struct {
	// Config is the file to read, absolute. See app.RunAsk.Validate for why
	// relative is refused rather than resolved.
	Config string `json:"config"`

	Provider     string `json:"provider,omitempty"`
	Image        string `json:"image,omitempty"`
	GPU          string `json:"gpu,omitempty"`
	MaxHourlyUSD string `json:"max_hourly_usd,omitempty"`

	TTL string `json:"ttl,omitempty"`
}

// RunAccepted is the 202.
//
// A NAMED TYPE AND NOT map[string]string, for `cancelling`'s reason: the
// client decodes this, and a field nobody reads is a field nobody finds out
// has gone.
type RunAccepted struct {
	ID string `json:"id"`
}

// toAsk parses the body into the value internal/app takes.
func (b RunBody) toAsk() (app.RunAsk, error) {
	ttl, err := parseDur("ttl", b.TTL)
	if err != nil {
		return app.RunAsk{}, err
	}
	ask := app.RunAsk{
		Config: b.Config,
		Overrides: app.Overrides{
			Provider:     b.Provider,
			Image:        b.Image,
			GPU:          b.GPU,
			MaxHourlyUSD: b.MaxHourlyUSD,
			TTL:          ttl,
		},
	}
	// Validated here as well as in Start, because this is the layer that can
	// answer 400: Start's error is the same sentence and reaching it would
	// cost a config read first.
	if err := ask.Validate(); err != nil {
		return app.RunAsk{}, err
	}
	return ask, nil
}

func parseDur(name, v string) (time.Duration, error) {
	if v == "" {
		return 0, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("%s %q is not a length of time; write it as Go spells one, like 2h or 30m", name, v)
	}
	return d, nil
}

// postRun starts a run the way `ember up` does, and answers once it has
// started.
//
// 202 AND NOT 200, because the run is not finished: what has happened by the
// time this returns is that the plan was built, the row was written and the one
// goroutine that can rent has taken the run. The client then follows the event
// stream, which is the carrier the browser already uses -- a second one would
// be a second place for a progress line to be spelled.
//
// EVERY REFUSAL HERE IS FREE. Nothing is rented until the drain loop runs the
// closure Start built, so a config that will not load, an image that is not
// pinned, a card ember has no image for, a store missing the weights and a
// paid provider with no --allow-paid are all answered as a 4xx with the
// operator's own error text -- which is `ember up`'s behaviour and the reason
// this does not answer 202 and fail into a database.
func (s *Server) postRun(w http.ResponseWriter, r *http.Request) {
	if s.opts.Runs == nil {
		s.fail(w, http.StatusNotImplemented, errors.New("this server was built without a rental "+
			"loop, so there is nothing to run a submitted run on"))
		return
	}

	var body RunBody
	dec := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	// A misspelled field is a typo and not a comment -- postRequest's own
	// decision, and here it matters more: a `max_hourly` that silently did
	// nothing would run at the config's ceiling and the operator would never
	// know which number bounded their card.
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		s.fail(w, http.StatusBadRequest, fmt.Errorf("read the run: %w", err))
		return
	}

	ask, err := body.toAsk()
	if err != nil {
		s.fail(w, http.StatusBadRequest, err)
		return
	}

	id, err := s.opts.Runs.Start(r.Context(), ask)
	if err != nil {
		// 400 AND NOT 500, because every error this can be is a statement
		// about the ask: a config that will not load, an offer that does not
		// exist, a ceiling that refuses, or a client that went away while the
		// previous rental was still finishing. None of them is ember failing,
		// and a 500 would send an operator looking for a bug in the server
		// rather than reading the sentence it just handed them.
		s.fail(w, http.StatusBadRequest, err)
		return
	}
	s.write(w, http.StatusAccepted, RunAccepted{ID: string(id)})
}
