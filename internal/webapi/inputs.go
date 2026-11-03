package webapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/Legion112/ember/internal/blob"
	"github.com/Legion112/ember/internal/genstore"
	"github.com/Legion112/ember/internal/sdserver"
)

// THE ONE ROUTE THAT TAKES BYTES INBOUND, which is what makes it the one
// worth reading twice.
//
// Every other request this server accepts is a few hundred bytes of JSON
// describing a provider, a card and a batch of jobs. This one is a file a
// browser picked, and the bytes end up base64'd into a generation request on
// a machine ember is paying for by the second. So the refusals are stacked in
// the order that costs least:
//
//  1. The CREDENTIAL and the two header checks, through `guarded`, exactly as
//     every other route. An upload route left ungated would be a loopback
//     port on which any uid on this machine, or anything reached through an
//     `ssh -L`, can write files into the operator's state directory.
//  2. The SIZE BOUND, which refuses rather than truncating -- blob.ReadInput
//     reads one byte past it so the two answers can be told apart, which is
//     the discipline internal/s3's `do` had to be taught after a truncated
//     listing page parsed as XML right up to where it did not.
//  3. The HEADER, through image.DecodeConfig: a png or a jpeg, with its
//     declared geometry inside a bound, or it is refused by name. sd-server
//     does refuse a payload that is not an image -- measured, a 400 -- but
//     that refusal arrives after a card is rented.
//  4. The DIGEST, which is the key. The uploaded filename is never looked at.
//
// WHAT IS DELIBERATELY NOT CHECKED is the Content-Type. A client's declared
// type is a claim about the bytes and DecodeConfig is a check of them, so
// reading the header would be reading the weaker of the two answers -- and a
// browser's `File.type` comes from the file extension on most platforms. The
// response is the one that matters and ember sets that itself.

// inputTTL is how old a staged image has to be before a sweep will remove it.
//
// IT IS THE `ember store abort --older-than` RULE, and for the same reason: an
// upload in progress and an abandoned one are the same thing to the thing
// storing them. A file picked a second ago belongs to no request yet, because
// the operator is still composing the batch -- so a sweep with no age bound
// would delete exactly the input that is about to be used, and the symptom
// would be a file picker that works except when it does not.
//
// A day, which is longer than any compose session and shorter than the
// interval at which an operator would notice the directory.
const inputTTL = 24 * time.Hour

// sweepKeepLimit bounds the listing the keep-set is built from.
//
// AND AN INCOMPLETE LISTING SKIPS THE SWEEP RATHER THAN SHORTENING THE
// KEEP-SET. genstore.List is limited by construction -- an unbounded listing
// of every request ever made is not an answer to "what is in the queue" -- so
// a keep-set built from a truncated page would be missing exactly the
// unfinished requests whose inputs must not be removed, and the failure would
// be a queued batch losing its image. The rule internal/s3's paginated
// listings follow: a page ember cannot be sure is the whole answer is not an
// answer, and hitting the bound is said out loud.
const sweepKeepLimit = 10000

// postInput stages one image and answers with the reference that names it.
func (s *Server) postInput(w http.ResponseWriter, r *http.Request) {
	in := s.opts.Inputs
	if in == nil {
		s.fail(w, http.StatusNotImplemented,
			errors.New("this server was built without an image staging area"))
		return
	}

	body, err := blob.ReadInput(r.Body)
	if errors.Is(err, blob.ErrInputTooLarge) {
		// 413 and not 400, because the remediation is different in kind: a
		// smaller image rather than a different one, and a client that wants
		// to retry needs to be able to tell which.
		s.fail(w, http.StatusRequestEntityTooLarge, err)
		return
	}
	if err != nil {
		s.fail(w, http.StatusBadRequest, err)
		return
	}

	staged, err := in.Stage(body)
	if err != nil {
		s.fail(w, http.StatusBadRequest, err)
		return
	}

	// AFTER THE UPLOAD SUCCEEDED, AND NEVER INSTEAD OF IT. The sweep is
	// housekeeping on a directory this request has just added to, so a sweep
	// that fails is logged and the upload still answers: the bytes are
	// staged, the reference is valid, and failing the request would make the
	// operator re-upload a file that is already there. It is reported rather
	// than silent for the reason an aborted multipart upload's failure is --
	// the operator now has bytes they do not know about.
	if swept, serr := s.sweepInputs(r.Context()); serr != nil {
		s.log().Warn("could not sweep the staged images; the directory keeps what it has",
			"dir", in.Name(), "err", serr)
	} else if swept.Files > 0 {
		s.log().Info("swept staged images no unfinished request names",
			"files", swept.Files, "bytes", swept.Bytes)
	}

	s.write(w, http.StatusCreated, inputView{
		ID: staged.Ref, Format: staged.Format,
		Width: staged.Width, Height: staged.Height, Bytes: staged.Bytes,
	})
}

// sweepInputs removes staged images nothing unfinished is waiting on.
//
// WHY HERE AND NOT IN `ember history prune`. The history commands remove a
// request's rows and the images that request was billed for. A staged input
// is neither: it may belong to no request at all, and the request it does
// belong to may have finished, in which case the input was an argument to
// something already paid for. Putting it in the prune scheme would mean
// deciding that an operator's uploaded photograph is as removable as a
// generated image, which is a different judgement about different bytes.
//
// WHY ON UPLOAD. The thing that creates the garbage is the thing that sweeps
// it, so the staging area is bounded by what one session uploads rather than
// by all of history -- and there is no new command, no flag and no timer to
// explain. What it does not do is clean up after an operator who uploads once
// and never again; that is one session's worth of files, which is the bound
// being claimed and not more.
func (s *Server) sweepInputs(ctx context.Context) (blob.Swept, error) {
	keep, ok, err := s.keepRefs(ctx)
	if err != nil {
		return blob.Swept{}, err
	}
	if !ok {
		return blob.Swept{}, errors.New("there are more unfinished requests than this listing " +
			"can hold, so ember cannot be sure which staged images are still needed and swept " +
			"none of them")
	}
	return s.opts.Inputs.Sweep(time.Now().Add(-inputTTL), keep)
}

// keepRefs is every image reference an unfinished request names, and whether
// the listing was complete.
//
// QUEUED AND RUNNING, which is genstore.Status.Terminal inverted. A finished
// request's input is not kept: the generation it was an argument to has been
// paid for, the images it produced are what the operator has, and keeping the
// input forever would make the staging area grow with history -- which is the
// thing this function exists to stop.
func (s *Server) keepRefs(ctx context.Context) ([]string, bool, error) {
	var refs []string
	for _, st := range []genstore.Status{genstore.StatusQueued, genstore.StatusRunning} {
		rs, err := s.opts.Store.List(ctx, genstore.Filter{Status: st, Limit: sweepKeepLimit})
		if err != nil {
			return nil, false, err
		}
		if len(rs) >= sweepKeepLimit {
			return nil, false, nil
		}
		for _, req := range rs {
			for _, j := range req.Jobs {
				if j.Params.InitImage != "" {
					refs = append(refs, j.Params.InitImage)
				}
			}
		}
	}
	return refs, true, nil
}

// resolveInputs loads every image a batch names, so that the refusal lands at
// enqueue.
//
// THE EARLIEST POSSIBLE MOMENT, which is the whole point. A reference that
// was never staged, or whose bytes no longer hash to it, is a request that
// cannot generate -- and the alternative to refusing it here is refusing it
// after a card has been rented, an image pulled and the weights loaded. It is
// also what makes sdserver.ValidateStatic usable on a job with an image: that
// function builds the request body, and Body refuses a reference with no
// bytes rather than quietly sending a text-to-image generation.
//
// THE BYTES ARE DISCARDED. They are loaded to be checked and to let the body
// be built, and then this handler returns: what the store holds is the
// reference, and the rental loads the file again when there is a server to
// send it to. Keeping them would mean a request row carrying megabytes
// through the enqueue path for no reader.
func (s *Server) resolveInputs(jobs []genstore.Job) ([]sdserver.Job, error) {
	out := make([]sdserver.Job, 0, len(jobs))
	for i, j := range jobs {
		sd := j.SD()
		if ref := sd.Params.InitImage; ref != "" {
			if s.opts.Inputs == nil {
				return nil, fmt.Errorf("jobs[%d] names the image %s and this server was built "+
					"without an image staging area", i, ref)
			}
			b, err := s.opts.Inputs.Load(ref)
			if err != nil {
				return nil, fmt.Errorf("jobs[%d]: %w", i, err)
			}
			sd.InitImageBytes = b
		}
		out = append(out, sd)
	}
	return out, nil
}
