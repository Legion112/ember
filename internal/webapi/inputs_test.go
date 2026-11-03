package webapi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Legion112/ember/internal/blob"
	"github.com/Legion112/ember/internal/genstore"
	"github.com/Legion112/ember/internal/sdserver"
)

// samplePNG is a real PNG, encoded rather than canned: the server reads the
// header, so the header has to be one an encoder wrote.
func samplePNG(t *testing.T, w, h int) []byte {
	t.Helper()
	im := image.NewRGBA(image.Rect(0, 0, w, h))
	im.Set(0, 0, color.RGBA{R: 9, G: 8, B: 7, A: 255})
	var buf bytes.Buffer
	if err := png.Encode(&buf, im); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func refOf(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// TestPostInput_StagesAnImageAndNamesItByItsContent.
//
// The answer is the digest and the header the server read, and the digest is
// what a job carries. Nothing in the answer comes from the client: there is
// no filename in the request to echo, which is the whole point of posting the
// bytes rather than a form.
func TestPostInput_StagesAnImageAndNamesItByItsContent(t *testing.T) {
	h := newHarness(t)
	b := samplePNG(t, 12, 6)

	w := h.upload(t, b)
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d (%s), want 201", w.Code, w.Body.String())
	}
	got := decode[inputView](t, w)
	if got.ID != refOf(b) {
		t.Errorf("id = %q, want the sha256 of the bytes (%q)", got.ID, refOf(b))
	}
	if got.Format != "png" || got.Width != 12 || got.Height != 6 || got.Bytes != len(b) {
		t.Errorf("staged %+v, want png 12x6 of %d bytes", got, len(b))
	}

	// And the bytes are there under that name, verified by the same function
	// a rental loads them with.
	back, err := h.inputs.Load(got.ID)
	if err != nil {
		t.Fatalf("the reference the server answered with does not load: %v", err)
	}
	if !bytes.Equal(back, b) {
		t.Error("what was staged is not what was uploaded")
	}
}

// TestPostInput_RefusesPastTheBoundRatherThanTruncating.
//
// 413 AND NOT 400, because the remediation differs in kind: a smaller image
// rather than a different one. And a refusal rather than a short read, which
// is internal/s3's `do` defect -- a truncated image whose header survived
// still decodes, so ember would stage something that is not the file somebody
// picked.
//
// Both directions, because a bound that refused everything would pass the
// first half alone.
func TestPostInput_RefusesPastTheBoundRatherThanTruncating(t *testing.T) {
	h := newHarness(t)

	// A PNG padded past the bound. The header is valid, so what refuses it
	// can only be the size.
	b := samplePNG(t, 4, 4)
	over := append(b, bytes.Repeat([]byte{0}, blob.MaxInputBytes+1-len(b))...)
	w := h.upload(t, over)
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want 413", w.Code)
	}
	if !strings.Contains(w.Body.String(), fmt.Sprint(blob.MaxInputBytes)) {
		t.Errorf("the refusal does not name the bound: %s", w.Body.String())
	}

	// Nothing was staged, so a refused upload leaves no file behind.
	if err := verifyStaged(h.inputs, refOf(over)); err == nil {
		t.Error("the oversize upload was staged anyway")
	}
	// And the truncation it would have been is not staged either, which is
	// the assertion a silently-truncating server would fail.
	if err := verifyStaged(h.inputs, refOf(over[:blob.MaxInputBytes])); err == nil {
		t.Error("the upload was truncated to the bound and staged")
	}

	// Something inside the bound still works, so the bound is a bound.
	if w := h.upload(t, b); w.Code != http.StatusCreated {
		t.Errorf("an ordinary upload got %d (%s)", w.Code, w.Body.String())
	}
}

// TestPostInput_RefusesWhatIsNotAnImage, by name, before a card is rented.
func TestPostInput_RefusesWhatIsNotAnImage(t *testing.T) {
	h := newHarness(t)
	w := h.upload(t, []byte("#!/bin/sh\necho not an image\n"))
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
	if !strings.Contains(w.Body.String(), "not a png or a jpeg") {
		t.Errorf("the refusal does not say what is accepted: %s", w.Body.String())
	}
}

// TestPostInput_RefusesAnEmptyBody, which is what a dropped upload leaves.
func TestPostInput_RefusesAnEmptyBody(t *testing.T) {
	h := newHarness(t)
	if w := h.upload(t, nil); w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

// TestPostInput_SaysSoWithNoStagingArea.
//
// A nil Inputs is every handler test that does not care about images, and
// `ember serve` always supplies one -- so this is 501 rather than a panic,
// the same shape as the blob store's own absence on the image route.
func TestPostInput_SaysSoWithNoStagingArea(t *testing.T) {
	h := newHarness(t, func(o *Options) { o.Inputs = nil })
	w := h.upload(t, samplePNG(t, 2, 2))
	if w.Code != http.StatusNotImplemented {
		t.Errorf("status = %d, want 501 (%s)", w.Code, w.Body.String())
	}
}

// TestPostRequest_RefusesAnImageNothingStaged.
//
// THE CHEAPEST REFUSAL IN THE FEATURE. A reference nothing staged is a
// request that can never generate, and this happens before it is even
// queued -- the alternative is finding out about a minute into a rented card,
// after the image is pulled and the weights are loaded.
func TestPostRequest_RefusesAnImageNothingStaged(t *testing.T) {
	h := newHarness(t)
	missing := refOf([]byte("never uploaded"))
	body := fmt.Sprintf(`{"provider":"local","jobs":[
		{"prompt":"a cat","init_image":%q}]}`, missing)

	w := h.do(t, "POST", "/api/requests", body)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (%s)", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), missing) {
		t.Errorf("the refusal does not name the reference: %s", w.Body.String())
	}
	for _, c := range h.store.Calls {
		if strings.HasPrefix(c, "Enqueue") {
			t.Errorf("a request naming an unstaged image was queued: %v", h.store.Calls)
		}
	}
}

// TestPostRequest_RefusesAReferenceThatIsNotOne.
//
// The field a job carries is sixty-four lowercase hex characters or it is
// refused, which is what keeps it from ever being a path. Checked through the
// same function the staging store looks names up with, so the set a request
// may name and the set the store can find are one set.
func TestPostRequest_RefusesAReferenceThatIsNotOne(t *testing.T) {
	h := newHarness(t)
	for _, tc := range []struct{ name, ref string }{
		{"a traversal", "../../etc/passwd"},
		{"an absolute path", "/etc/passwd"},
		{"a traversal inside the scheme", "sha256:../../etc/passwd"},
		{"a url", "http://evil.example/x.png"},
		{"a bare digest", strings.Repeat("ab", sha256.Size)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := fmt.Sprintf(`{"provider":"local","jobs":[
				{"prompt":"a cat","init_image":%q}]}`, tc.ref)
			w := h.do(t, "POST", "/api/requests", body)
			if w.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400 (%s)", w.Code, w.Body.String())
			}
		})
	}
}

// TestPostRequest_QueuesAStagedImageAndKeepsTheDigest.
//
// THE REFERENCE IS STORED AND THE BYTES ARE NOT, which is what keeps a
// request row at a few hundred bytes while the picture stays in one place on
// disk. The round trip back through the view carries the digest too, so the
// page can say which image a finished request used.
func TestPostRequest_QueuesAStagedImageAndKeepsTheDigest(t *testing.T) {
	h := newHarness(t)
	b := samplePNG(t, 8, 8)
	staged := decode[inputView](t, h.upload(t, b))

	body := fmt.Sprintf(`{"provider":"local","jobs":[
		{"prompt":"a cat","init_image":%q,"strength":0}]}`, staged.ID)
	w := h.do(t, "POST", "/api/requests", body)
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (%s)", w.Code, w.Body.String())
	}
	id := decode[map[string]string](t, w)["id"]

	req, err := h.store.Get(t.Context(), genstore.ReqID(id))
	if err != nil {
		t.Fatal(err)
	}
	if got := req.Jobs[0].Params.InitImage; got != staged.ID {
		t.Errorf("the stored job names %q, want %q", got, staged.ID)
	}
	// The explicit zero survived, which is the whole reason the field is a
	// pointer: the server's own default is 0.75.
	if s := req.Jobs[0].Params.Strength; s == nil || *s != 0 {
		t.Errorf("strength = %v, want an explicit 0", s)
	}
	// AND NOT THE PIXELS. A stored job carrying the image would put
	// megabytes in the params column of every job that used it, and send
	// them back to the browser on every listing.
	raw, merr := json.Marshal(req.Jobs[0])
	if merr != nil {
		t.Fatal(merr)
	}
	if len(raw) > 1024 {
		t.Errorf("the stored job is %d bytes, so something large travelled with it: %s",
			len(raw), raw)
	}

	// And the view hands the digest back.
	v := decode[requestView](t, h.do(t, "GET", "/api/requests/"+id, ""))
	if len(v.Jobs) != 1 || v.Jobs[0].InitImage != staged.ID {
		t.Errorf("the view does not name the image: %+v", v.Jobs)
	}
}

// TestPostRequest_RefusesAStrengthWithNoImage.
//
// MEASURED, NOT INFERRED: a live sd-server answered 202 to a strength with no
// init_image, generated, and echoed no strength at all. So leaving it
// representable means an operator sets a strength, pays for a generation and
// gets a plain text-to-image image back with nothing saying why.
func TestPostRequest_RefusesAStrengthWithNoImage(t *testing.T) {
	h := newHarness(t)
	w := h.do(t, "POST", "/api/requests",
		`{"provider":"local","jobs":[{"prompt":"a cat","strength":0.4}]}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (%s)", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "no image input") {
		t.Errorf("the refusal does not say what is missing: %s", w.Body.String())
	}
}

// TestPostRequest_RefusesAnImageTheModelDoesNotTake, from the cached
// capabilities, before anything is rented.
func TestPostRequest_RefusesAnImageTheModelDoesNotTake(t *testing.T) {
	h := newHarness(t)
	no := false
	if err := h.store.PutCaps(t.Context(), sdserver.Capabilities{
		Model:     "sd15.gguf",
		Supported: sdserver.Supported{InitImage: &no},
	}); err != nil {
		t.Fatal(err)
	}

	staged := decode[inputView](t, h.upload(t, samplePNG(t, 4, 4)))
	body := fmt.Sprintf(`{"provider":"local","jobs":[
		{"prompt":"a cat","init_image":%q}]}`, staged.ID)
	w := h.do(t, "POST", "/api/requests", body)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (%s)", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "generation_features.init_image") {
		t.Errorf("the refusal does not name the flag: %s", w.Body.String())
	}
}

// TestConfig_PublishesTheImageFlagSoThePageCanGreyThePicker.
//
// Three states, because that is what the page distinguishes: nil is "this
// build published no such flag", false is "the server says no", and only the
// second takes a control away.
func TestConfig_PublishesTheImageFlagSoThePageCanGreyThePicker(t *testing.T) {
	h := newHarness(t)
	no := false
	if err := h.store.PutCaps(t.Context(), sdserver.Capabilities{
		Model:     "sd15.gguf",
		Supported: sdserver.Supported{InitImage: &no},
	}); err != nil {
		t.Fatal(err)
	}
	v := decode[configView](t, h.do(t, "GET", "/api/config", ""))
	if v.Caps == nil || v.Caps.Features.InitImage == nil {
		t.Fatalf("the config does not publish the image flag: %+v", v.Caps)
	}
	if *v.Caps.Features.InitImage {
		t.Error("a denied image flag was published as allowed")
	}

	// And a silent server publishes nothing rather than a false, which the
	// page reads as "gates nothing".
	h2 := newHarness(t)
	if err := h2.store.PutCaps(t.Context(), sdserver.Capabilities{Model: "sd15.gguf"}); err != nil {
		t.Fatal(err)
	}
	raw := h2.do(t, "GET", "/api/config", "").Body.String()
	if strings.Contains(raw, "init_image") {
		t.Errorf("a silent server published an init_image flag: %s", raw)
	}
}

// TestSweep_KeepsWhatAQueuedRequestNamesAndTakesWhatNothingDoes.
//
// WHAT CLEANS UP. An upload nobody generated with is bytes ember is storing
// for nobody, and the thing that creates them is the thing that sweeps them
// -- so the staging area is bounded by what one session uploads rather than
// by all of history. The two bounds are both exercised: a queued request's
// image survives whatever its age, and a fresh upload survives because an
// operator who has just picked a file is still composing.
func TestSweep_KeepsWhatAQueuedRequestNamesAndTakesWhatNothingDoes(t *testing.T) {
	h := newHarness(t)

	wanted := decode[inputView](t, h.upload(t, samplePNG(t, 4, 4)))
	abandoned := decode[inputView](t, h.upload(t, samplePNG(t, 5, 5)))

	// The first is queued, so it must survive. The second is named by
	// nothing.
	body := fmt.Sprintf(`{"provider":"local","jobs":[
		{"prompt":"a cat","init_image":%q}]}`, wanted.ID)
	if w := h.do(t, "POST", "/api/requests", body); w.Code != http.StatusAccepted {
		t.Fatalf("queueing failed: %d %s", w.Code, w.Body.String())
	}

	// Both are younger than the age bound, so an upload now sweeps neither:
	// that bound is what stops a sweep deleting the file an operator picked a
	// second ago.
	if w := h.upload(t, samplePNG(t, 6, 6)); w.Code != http.StatusCreated {
		t.Fatal(w.Body.String())
	}
	for _, ref := range []string{wanted.ID, abandoned.ID} {
		if err := verifyStaged(h.inputs, ref); err != nil {
			t.Errorf("a sweep took an image uploaded moments ago: %v", err)
		}
	}

	// Aged past the bound, the abandoned one goes and the queued one stays.
	// Driven directly rather than by faking a clock, because what is being
	// checked is the keep-set the handler builds from the store.
	ageStagedFiles(t, h)
	swept, err := h.sweepInputs(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if swept.Files == 0 {
		t.Fatal("the sweep freed nothing")
	}
	if err := verifyStaged(h.inputs, wanted.ID); err != nil {
		t.Errorf("the sweep took the image a queued request names: %v", err)
	}
	if err := verifyStaged(h.inputs, abandoned.ID); err == nil {
		t.Error("the abandoned image is still staged")
	}
}

// TestSweep_SweepsNothingWhenItCannotSeeEveryUnfinishedRequest.
//
// A KEEP-SET BUILT FROM A TRUNCATED LISTING IS MISSING EXACTLY THE REQUESTS
// WHOSE IMAGES MUST NOT GO, so the sweep refuses to act rather than acting on
// an incomplete answer. The rule internal/s3's paginated listings follow: a
// page ember cannot be sure is the whole answer is not an answer, and hitting
// the bound is said out loud.
func TestSweep_SweepsNothingWhenItCannotSeeEveryUnfinishedRequest(t *testing.T) {
	h := newHarness(t)
	staged := decode[inputView](t, h.upload(t, samplePNG(t, 4, 4)))
	ageStagedFiles(t, h)

	// A store that answers every listing with a full page, which is what
	// more unfinished requests than the bound looks like from here.
	h.opts.Store = fullPageStore{Store: h.store}

	_, err := h.sweepInputs(t.Context())
	if err == nil {
		t.Fatal("a sweep with an incomplete keep-set reported success")
	}
	if !strings.Contains(err.Error(), "cannot be sure") {
		t.Errorf("the error does not say why it swept nothing: %v", err)
	}
	if verr := verifyStaged(h.inputs, staged.ID); verr != nil {
		t.Errorf("it swept something anyway: %v", verr)
	}
}

// fullPageStore answers every listing with as many rows as were asked for.
type fullPageStore struct{ genstore.Store }

func (f fullPageStore) List(_ context.Context, q genstore.Filter) ([]genstore.Request, error) {
	out := make([]genstore.Request, q.Limit)
	return out, nil
}

// TestSweep_ReportsAStoreItCouldNotAsk, because "I could not check" and
// "nothing is needed" are different answers -- the rule `ember ls`
// exits non-zero for.
func TestSweep_ReportsAStoreItCouldNotAsk(t *testing.T) {
	h := newHarness(t)
	h.store.FailOn = "List"
	if _, err := h.sweepInputs(t.Context()); err == nil {
		t.Error("a sweep whose listing failed reported success")
	}
}

// ageStagedFiles backdates every staged file past the age bound.
//
// OUT OF BAND, THROUGH THE FILESYSTEM, rather than by giving blob.Inputs a
// clock to inject. The age bound is the file's own mtime, so the honest way
// to test it is to change the mtime -- a seam added for the test would be a
// production interface shaped by a test's convenience, which is the rule the
// labs follow when they seed state through Docker.
func ageStagedFiles(t *testing.T, h *harness) {
	t.Helper()
	was := time.Now().Add(-72 * time.Hour)
	ents, err := os.ReadDir(h.inputs.Name())
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range ents {
		if err := os.Chtimes(filepath.Join(h.inputs.Name(), e.Name()), was, was); err != nil {
			t.Fatal(err)
		}
	}
}

// TestPostInput_ReportsABodyItCouldNotRead.
//
// A connection that goes away mid-upload is not an empty file and not a
// refused one: it is a read that failed, and the operator's remedy is to try
// again rather than to pick a different picture.
func TestPostInput_ReportsABodyItCouldNotRead(t *testing.T) {
	h := newHarness(t)
	r := httptest.NewRequest("POST", "/api/inputs", brokenBody{})
	r.Host = fmt.Sprintf("127.0.0.1:%d", testPort)
	r.Header.Set("Origin", fmt.Sprintf("http://127.0.0.1:%d", testPort))
	authorize(r)

	w := httptest.NewRecorder()
	h.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 (%s)", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "read the uploaded image") {
		t.Errorf("the failure is not named as a read: %s", w.Body.String())
	}
}

type brokenBody struct{}

func (brokenBody) Read([]byte) (int, error) { return 0, errors.New("the connection went away") }

// TestPostInput_SucceedsWhenTheSweepCannotRun.
//
// THE SWEEP IS HOUSEKEEPING AND NEVER THE ANSWER TO THE REQUEST. The bytes
// are staged, the reference is valid, and failing the upload over a sweep
// would make the operator re-pick a file that is already there. It is logged
// rather than silent for the reason an aborted multipart upload's failure is
// -- the operator now has bytes they do not know about.
func TestPostInput_SucceedsWhenTheSweepCannotRun(t *testing.T) {
	h := newHarness(t)
	b := samplePNG(t, 4, 4)
	h.store.FailOn = "List"

	w := h.upload(t, b)
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (%s)", w.Code, w.Body.String())
	}
	if err := verifyStaged(h.inputs, refOf(b)); err != nil {
		t.Errorf("the upload that answered 201 is not staged: %v", err)
	}
}

// TestPostInput_SweepsOnTheWayThrough, which is the only thing that bounds
// the staging directory: the thing that creates the garbage is the thing that
// removes it.
func TestPostInput_SweepsOnTheWayThrough(t *testing.T) {
	h := newHarness(t)
	old := decode[inputView](t, h.upload(t, samplePNG(t, 4, 4)))
	ageStagedFiles(t, h)

	fresh := samplePNG(t, 5, 5)
	if w := h.upload(t, fresh); w.Code != http.StatusCreated {
		t.Fatalf("status = %d (%s)", w.Code, w.Body.String())
	}
	if err := verifyStaged(h.inputs, old.ID); err == nil {
		t.Error("an upload did not sweep an aged image nothing names")
	}
	if err := verifyStaged(h.inputs, refOf(fresh)); err != nil {
		t.Errorf("the upload swept itself: %v", err)
	}
}

// TestPostRequest_SaysSoWithNoStagingArea.
//
// A server built without one cannot resolve a reference, so a job that names
// an image is refused by name rather than by a nil dereference. It is the
// state every handler test that does not care about images is in.
func TestPostRequest_SaysSoWithNoStagingArea(t *testing.T) {
	h := newHarness(t, func(o *Options) { o.Inputs = nil })
	body := fmt.Sprintf(`{"provider":"local","jobs":[
		{"prompt":"a cat","init_image":%q}]}`, refOf([]byte("x")))
	w := h.do(t, "POST", "/api/requests", body)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (%s)", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "without an image staging area") {
		t.Errorf("the refusal does not say what is missing: %s", w.Body.String())
	}
}

// verifyStaged is Load with the bytes thrown away.
//
// In this package rather than as a method on blob.Inputs, because
// TestNoProductionDeadCode caught the method version: production code that
// only tests call is the defect this repository keeps finding, and both real
// callers need the bytes anyway.
func verifyStaged(in *blob.Inputs, ref string) error {
	_, err := in.Load(ref)
	return err
}
