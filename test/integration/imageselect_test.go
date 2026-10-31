//go:build integration

package integration

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Legion112/ember/internal/node"
	"github.com/Legion112/ember/internal/provision"
	"github.com/Legion112/ember/test/integration/harness"
	"github.com/Legion112/ember/test/integration/runpodfake"
)

// TestImageSelect_TheChosenImageIsTheOneAPodRuns.
//
// The one claim about per-architecture images that a unit test cannot make.
// Whether ember picks the right reference is decided in internal/cli and
// asserted there against `--dry-run`, cheaply and with no Docker. What that
// proves is which string was printed. This proves the string reaches a pod:
// the provider is handed the reference, a real container starts from it, and
// the thing running is what the architecture asked for.
//
// It mattered because nothing in this suite checked it. The fake recorded the
// image a create call named and no lab ever read it back, so an ember that
// sent the right reference to a provider which then ran something else would
// have looked exactly like a working run.
//
// Two tags off ember:lab rather than two builds. What is under test is which
// name was used, not what the bytes are, and `docker tag` copies nothing.
func TestImageSelect_TheChosenImageIsTheOneAPodRuns(t *testing.T) {
	p, f, l := runpodLab(t, runpodfake.Options{})
	ctx := t.Context()
	port := harness.FreePort(t)

	// The captured catalogue's only available card is an A100 80GB PCIe,
	// which normalises to a100-80g and is sm_80. Declared by the fake rather
	// than read off this machine, so the architecture under test is the same
	// on a workstation with a 2080 Ti and on a runner with no card at all.
	wrong := l.TagImage(ctx, harness.LabImage, "ember:lab-sm90")
	want := l.TagImage(ctx, harness.LabImage, "ember:lab-sm80")

	// Built the way runUp builds it: the image is already resolved, because
	// the offer was chosen first.
	spec, err := node.NewSpec(node.SpecOptions{
		Image:  want,
		TTL:    10 * time.Minute,
		Ports:  []node.PortMap{{Name: "sshd", Remote: 22, Local: uint16(port)}},
		Filter: node.Filter{GPU: "a100"},
	})
	if err != nil {
		t.Fatal(err)
	}

	offer, err := provision.PickOffer(ctx, p, spec.Filter, spec.TTL, labOptions(t, l))
	if err != nil {
		t.Fatalf("no offer for an a100: %v", err)
	}
	// The offer is what the image was chosen for, so assert the fake really
	// handed back the card this lab is about. A catalogue change that moved
	// availability to another card would otherwise quietly test sm_89.
	if cap, how := offer.ComputeCap(); how != node.CapKnown || cap != 80 {
		t.Fatalf("the offer is %s, which resolves %s/%v; this lab is about sm_80",
			offer.GPU, cap, how)
	}

	var ran string
	err = provision.WithNode(ctx, p, spec, offer, labOptions(t, l),
		func(ctx context.Context, s provision.Session) error {
			live := f.Live()
			if len(live) != 1 {
				t.Fatalf("%d live pods, want 1", len(live))
			}
			img, _, _, ok := f.Pod(live[0])
			if !ok {
				t.Fatalf("the fake does not know pod %s", live[0])
			}
			ran = img
			return nil
		})
	if err != nil {
		t.Fatalf("the run failed: %v", err)
	}

	if ran != want {
		t.Errorf("the pod ran %q, want %q: the image is chosen from the architecture of the card "+
			"in the offer, and an sm_90 image on an sm_80 card does not fail -- it finds no usable "+
			"kernel, runs on the CPU backend, and bills the GPU for it", ran, want)
	}
	if ran == wrong {
		t.Errorf("the pod ran the image for another architecture (%q)", wrong)
	}
}

// TestImageSelect_ARefusedRunRentsNothing.
//
// The refusal has to happen before the create call, not after: a node refused
// once it exists is a node that has already started billing for the
// discovery. Same ordering as the store resolution and the budget check.
//
// Driven here rather than only in a unit test because what has to hold is
// that nothing reached the provider -- and the provider is the thing a unit
// test replaces.
func TestImageSelect_ARefusedRunRentsNothing(t *testing.T) {
	p, f, l := runpodLab(t, runpodfake.Options{})
	ctx := t.Context()
	_ = l

	// A filter for a card the catalogue does not have. PickOffer refuses, and
	// the point is where: before any POST to the pods endpoint.
	_, err := provision.PickOffer(ctx, p, node.Filter{GPU: "h100"}, 10*time.Minute, labOptions(t, l))
	if err == nil {
		t.Fatal("an offer was found for a card the catalogue does not list")
	}

	for _, r := range f.Requests() {
		if r.Method == "POST" && strings.Contains(r.Path, "pods") {
			t.Errorf("ember sent %s %s before refusing; a refusal has to be free", r.Method, r.Path)
		}
	}
	if live := f.Live(); len(live) != 0 {
		t.Errorf("%d pod(s) exist after a refusal: %v", len(live), live)
	}
}

// TestImageSelect_AStandInTagSurvivesNothingAndLeaksNothing.
//
// TagImage's cleanup removes a name and not an image. A forced remove would
// delete ember:lab out from under every lab that runs after this one, and
// AssertNoStray cannot catch that: a tag inherits its source's labels, so a
// stray tag is indistinguishable from ember:lab by label and ownership is the
// only mechanism there is.
func TestImageSelect_AStandInTagSurvivesNothingAndLeaksNothing(t *testing.T) {
	l := harness.New(t)
	ctx := t.Context()

	tag := l.TagImage(ctx, harness.LabImage, "ember:lab-sm75")
	if !l.HaveImage(ctx, tag) {
		t.Fatalf("%s was not tagged", tag)
	}
	// The source is still there, which is the property that matters: every
	// later lab needs it.
	if !l.HaveImage(ctx, harness.LabImage) {
		t.Fatalf("tagging removed %s", harness.LabImage)
	}
}
