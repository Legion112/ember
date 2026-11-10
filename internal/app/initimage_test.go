package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"image"
	"image/color"
	"image/png"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Legion112/ember/internal/blob"
	"github.com/Legion112/ember/internal/config"
	"github.com/Legion112/ember/internal/node"
	"github.com/Legion112/ember/internal/report"
	"github.com/Legion112/ember/internal/sdserver"
	"github.com/Legion112/ember/internal/session"
)

// stagePNG puts a real image in a staging directory and returns both.
func stagePNG(t *testing.T, w, h int) (dir string, ref string, pixels []byte) {
	t.Helper()
	im := image.NewRGBA(image.Rect(0, 0, w, h))
	im.Set(0, 0, color.RGBA{R: 3, G: 2, B: 1, A: 255})
	var buf bytes.Buffer
	if err := png.Encode(&buf, im); err != nil {
		t.Fatal(err)
	}

	dir = filepath.Join(t.TempDir(), "inputs")
	in, err := blob.OpenInputs(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = in.Close() }()
	got, err := in.Stage(buf.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	return dir, got.Ref, buf.Bytes()
}

func unstagedRef(content string) string {
	sum := sha256.Sum256([]byte(content))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// TestJobsFromRequest_VerifiesAnImageBeforeAnythingIsRented.
//
// THE CHEAP HALF OF THE TWO-MOMENT CHECK. Verifying costs one read of a file
// on this machine and happens before the create call; the bytes are not
// needed until a generation server is up and the weights are resident, which
// on a rented card is about a minute of billing later. A reference that was
// never staged must not be discovered then.
func TestJobsFromRequest_VerifiesAnImageBeforeAnythingIsRented(t *testing.T) {
	dir, ref, pixels := stagePNG(t, 8, 4)

	run, err := jobsFromRequest(Request{
		Jobs: []sdserver.Job{{Prompt: "a cat", Params: sdserver.Params{InitImage: ref}}},
	}, dir)
	if err != nil {
		t.Fatalf("a staged image was refused: %v", err)
	}
	if got := run.Jobs[0].Params.InitImage; got != ref {
		t.Errorf("the reference did not survive: %q", got)
	}
	// AND THE BYTES DO NOT TRAVEL. This Run reaches the dry run and the
	// batch-count line and nothing that submits -- the drain loop builds its
	// own from the stored request for every batch it claims, including the
	// first -- so megabytes carried here would be megabytes held for the life
	// of a rental by nothing that reads them.
	if n := len(run.Jobs[0].InitImageBytes); n != 0 {
		t.Errorf("the plan carries %d bytes of image; it has no reader", n)
	}
	if len(pixels) == 0 {
		t.Fatal("the fixture is empty")
	}
}

// TestJobsFromRequest_RefusesAnImageThatIsNotThere, naming the job.
func TestJobsFromRequest_RefusesAnImageThatIsNotThere(t *testing.T) {
	dir, _, _ := stagePNG(t, 4, 4)
	missing := unstagedRef("never uploaded")

	_, err := jobsFromRequest(Request{
		Jobs: []sdserver.Job{
			{Prompt: "a cat"},
			{Prompt: "a boat", Params: sdserver.Params{InitImage: missing}},
		},
	}, dir)
	if err == nil {
		t.Fatal("a reference nothing staged was accepted")
	}
	if !strings.Contains(err.Error(), "jobs[1]") {
		t.Errorf("the refusal does not name the job: %v", err)
	}
	// Not asserted as an ErrUsage here: NewPlan wraps everything this
	// function returns, which is where the exit-3 contract lives. Asserting
	// it twice would make the wrapping look like this function's job.
}

// TestJobsFromRequest_RefusesBytesThatAreNotTheOnesNamed.
//
// The digest is re-checked at both moments, so something that wrote into the
// staging directory after an enqueue cannot have its pixels generated from
// under a reference a queued request already names. Reported and never
// repaired: ember does not know what wrote them, and overwriting would
// destroy the evidence.
func TestJobsFromRequest_RefusesBytesThatAreNotTheOnesNamed(t *testing.T) {
	dir, ref, _ := stagePNG(t, 4, 4)
	key, err := blob.ParseRef(ref)
	if err != nil {
		t.Fatal(err)
	}
	// Out of band, through the filesystem, because the premise is a writer
	// that is not ember.
	if werr := os.WriteFile(filepath.Join(dir, key), []byte("different pixels"), 0o600); werr != nil {
		t.Fatal(werr)
	}

	_, err = jobsFromRequest(Request{
		Jobs: []sdserver.Job{{Prompt: "a cat", Params: sdserver.Params{InitImage: ref}}},
	}, dir)
	if err == nil || !strings.Contains(err.Error(), "not the bytes it names") {
		t.Errorf("err = %v, want a digest mismatch", err)
	}
}

// TestJobsFromRequest_AConfigPathCannotNameAnImage.
//
// THE SEAL, ASSERTED RATHER THAN ASSUMED. `ember up` takes its jobs from
// ember.yaml and config.SDJob has no field for an image input, so there is no
// reference for anything to resolve -- and a run with no staging directory
// refuses one outright rather than generating a plain text-to-image image for
// a request that asked for an input.
func TestJobsFromRequest_AConfigPathCannotNameAnImage(t *testing.T) {
	_, err := jobsFromRequest(Request{
		Jobs: []sdserver.Job{{Prompt: "a cat",
			Params: sdserver.Params{InitImage: unstagedRef("x")}}},
	}, "")
	if err == nil || !strings.Contains(err.Error(), "no staging directory") {
		t.Errorf("err = %v, want a refusal naming the missing staging directory", err)
	}

	// And a batch with no image needs no staging directory at all, so
	// planning one does not create a directory as a side effect.
	run, err := jobsFromRequest(Request{Jobs: []sdserver.Job{{Prompt: "a cat"}}}, "")
	if err != nil {
		t.Fatalf("a batch with no image needed a staging directory: %v", err)
	}
	if len(run.Jobs) != 1 {
		t.Errorf("run carries %d job(s)", len(run.Jobs))
	}
}

// TestJobsFromRequest_ReportsAStagingDirectoryItCannotOpen.
func TestJobsFromRequest_ReportsAStagingDirectoryItCannotOpen(t *testing.T) {
	blocked := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocked, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := jobsFromRequest(Request{
		Jobs: []sdserver.Job{{Prompt: "a cat",
			Params: sdserver.Params{InitImage: unstagedRef("x")}}},
	}, filepath.Join(blocked, "inputs"))
	if err == nil {
		t.Error("a staging directory that cannot exist was opened anyway")
	}
}

// TestServed_GenerateSendsTheStagedPixels.
//
// THE JOIN THIS WHOLE CHANGE IS, and the one a unit test of either half would
// miss. The drain loop builds its Run from the STORED request, so the
// resolution NewPlan did cannot travel with it -- this is the layer that sees
// the jobs which will actually be submitted, and the assertion is on what
// reached the server rather than on what ember intended.
//
// A feature built end to end and never connected passes every test it has;
// this is the test that crosses the gap.
func TestServed_GenerateSendsTheStagedPixels(t *testing.T) {
	dir, ref, pixels := stagePNG(t, 8, 4)

	f := newFakeSD(t)
	n := startNode(t, f)
	spec := servedSpec(t, []string{"sd-server"})
	strength := 0.0

	err := servedRun(t, dir, n, spec, testRun(sdserver.Job{
		Prompt: "a paper boat", Output: "boat",
		Params: sdserver.Params{InitImage: ref, Strength: &strength},
	}))
	if err != nil {
		t.Fatal(err)
	}

	if f.submits() != 1 {
		t.Fatalf("%d job(s) submitted, want 1", f.submits())
	}
	got, ok := f.submitted[0]["init_image"].(string)
	if !ok {
		t.Fatalf("the submission carries no init_image: %v", f.submitted[0])
	}
	if got != base64.StdEncoding.EncodeToString(pixels) {
		t.Error("the submission carries something other than the staged pixels")
	}
	// The explicit zero travelled, which is the whole reason Strength is a
	// pointer: the server's own default is 0.75.
	if v, ok := f.submitted[0]["strength"]; !ok || v != 0.0 {
		t.Errorf("strength = %v (present %v), want an explicit 0", v, ok)
	}
}

// TestServed_GenerateReadsOneReferenceOnce.
//
// A batch of variations on one picture names the same digest for every job,
// and content addressing is what makes that a fact rather than a
// coincidence -- so the file is read once and the slice is shared. Asserted
// by what the server received, because "it is the same slice" is not
// something the submissions can show: what they can show is that every job
// got the right pixels.
func TestServed_GenerateReadsOneReferenceOnce(t *testing.T) {
	dir, ref, pixels := stagePNG(t, 4, 4)
	want := base64.StdEncoding.EncodeToString(pixels)

	f := newFakeSD(t)
	n := startNode(t, f)
	spec := servedSpec(t, []string{"sd-server"})

	jobs := make([]sdserver.Job, 3)
	for i := range jobs {
		jobs[i] = sdserver.Job{Prompt: "a boat", Params: sdserver.Params{InitImage: ref}}
	}
	if err := servedRun(t, dir, n, spec, testRun(jobs...)); err != nil {
		t.Fatal(err)
	}
	if f.submits() != 3 {
		t.Fatalf("%d job(s) submitted, want 3", f.submits())
	}
	for i, s := range f.submitted {
		if s["init_image"] != want {
			t.Errorf("job %d did not get the staged pixels", i)
		}
	}
}

// TestServed_GenerateRefusesTheWholeBatchForAMissingImage.
//
// The cheap refusals already happened at enqueue and again before the create
// call, so reaching here with an unstaged reference means the file went away
// in between. The whole batch is refused rather than the first job failing
// after the others have been paid for -- the same order Validate keeps for a
// mistyped sampler.
func TestServed_GenerateRefusesTheWholeBatchForAMissingImage(t *testing.T) {
	dir, _, _ := stagePNG(t, 4, 4)

	f := newFakeSD(t)
	n := startNode(t, f)
	spec := servedSpec(t, []string{"sd-server"})

	err := servedRun(t, dir, n, spec, testRun(
		sdserver.Job{Prompt: "a", Params: sdserver.Params{InitImage: unstagedRef("gone")}},
	))
	if err == nil {
		t.Fatal("generated from an image that is not staged")
	}
	if !errorsIsUsage(err) {
		t.Errorf("err = %v, want an ErrUsage", err)
	}
	if f.submits() != 0 {
		t.Errorf("%d job(s) were submitted before the refusal", f.submits())
	}
}

// TestServed_GenerateRefusesAnImageWithNoStagingDirectory.
//
// This is `ember up`'s shape -- runServed passes no directory, deliberately
// -- so a job that somehow names an image there is refused rather than having
// the field dropped. Dropping it would generate a plain text-to-image image
// and bill for it, which is the quiet half of the defect.
func TestServed_GenerateRefusesAnImageWithNoStagingDirectory(t *testing.T) {
	f := newFakeSD(t)
	n := startNode(t, f)
	spec := servedSpec(t, []string{"sd-server"})

	err := runServed(t.Context(), io.Discard, report.Discard, n, &nodeShell{t: t}, spec,
		testRun(sdserver.Job{Prompt: "a",
			Params: sdserver.Params{InitImage: unstagedRef("x")}}),
		session.Options{Log: discardLog()}, filepath.Join(t.TempDir(), "r"), 0)
	if err == nil {
		t.Fatal("ember up generated from an image it has no route for")
	}
	if !strings.Contains(err.Error(), "no staging directory") {
		t.Errorf("err = %v, want a refusal naming the missing staging directory", err)
	}
	if f.submits() != 0 {
		t.Errorf("%d job(s) were submitted", f.submits())
	}
}

// TestResolvePaths_PutsInputsUnderTheStateDirectory.
//
// NOT UNDER THE OUTPUTS, which is the decision the whole staging area turns
// on: an uploaded image is an argument to a run rather than something a run
// produced, and under the outputs it would be a file `ember history prune`
// treats as a generated image it is free to remove.
func TestResolvePaths_PutsInputsUnderTheStateDirectory(t *testing.T) {
	home := t.TempDir()
	ps, err := ResolvePaths(config.Config{}, Deps{HomeDir: home})
	if err != nil {
		t.Fatal(err)
	}
	if ps.Inputs != filepath.Join(ps.State, blob.InputsDirName) {
		t.Errorf("inputs are at %q, want %q", ps.Inputs,
			filepath.Join(ps.State, blob.InputsDirName))
	}
	if strings.HasPrefix(ps.Inputs, ps.Outputs+string(filepath.Separator)) {
		t.Errorf("the staging directory is inside the output directory (%q)", ps.Inputs)
	}
}

// servedRun is runServed with a staging directory, which is the queue's shape.
//
// runServed itself passes none on purpose -- `ember up` has no route for an
// image -- so a test about images has to go through withServer the way
// WithServed does.
func servedRun(t *testing.T, inputsDir string, n *fakeNode, spec node.Spec,
	run sdserver.Run) error {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "run")
	return withServer(t.Context(), io.Discard, report.Discard, n, &nodeShell{t: t}, spec,
		session.Options{Log: discardLog()}, 0, len(run.Jobs), inputsDir,
		func(ctx context.Context, s Served) error {
			return s.Generate(ctx, report.Discard, run, dir, nil)
		})
}
