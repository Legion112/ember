package control

import (
	"encoding/json"
	"errors"
	"path"
	"strings"
	"testing"

	"github.com/coder/websocket"
)

// TestFrame_RoundTripsEveryFieldItCarries.
//
// A WIRE TYPE BETWEEN TWO BINARIES THAT SHIP SEPARATELY, which is what makes
// this worth a test rather than trusting encoding/json: the node image is
// built and pushed and the ember driving it is on somebody's laptop, so a
// field that silently fails to survive the trip is a generation that runs,
// bills and reports the wrong thing. internal/webapi's own wire tests exist
// for the same reason one hop out.
//
// THE SEED IS THE CASE THIS WAS WRITTEN FOR. A job body relayed through
// map[string]any comes back with every number as a float64, and a seed above
// 2^53 is then re-encoded as a DIFFERENT number -- so the node would generate
// a picture ember did not ask for and record the seed it did ask for. Job is a
// json.RawMessage precisely so there is no round trip to get wrong, and the
// assertion is on the exact bytes rather than on a decoded value.
func TestFrame_RoundTripsEveryFieldItCarries(t *testing.T) {
	const seedBody = `{"prompt":"a paper boat","seed":9007199254740993,"steps":6}`

	in := Frame{
		Kind: KindJob, Version: Version,
		Index: 3, Total: 8, Stem: "boat",
		Job:     json.RawMessage(seedBody),
		Caps:    json.RawMessage(`{"model":{"name":"sd15"}}`),
		Timeout: 4000000000, Elapsed: 12500000000,
		Pass: 2, Step: 0, TotalSteps: 6,
		PNG:    []byte{0x89, 'P', 'N', 'G', 0x00, 0xff},
		Dir:    "/tmp/ember-results/3",
		Files:  []File{{Name: "boat-00.png", Bytes: 4096}},
		Status: "completed", Cancelled: true, InFlight: true,
		Err: "sd-server refused it", Text: "the weights loaded in 12s\n",
	}

	body, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out Frame
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	// The job body byte for byte, because this is the field a decode-and
	// -re-encode would quietly change.
	if string(out.Job) != seedBody {
		t.Errorf("the job body changed in transit:\n got %s\nwant %s", out.Job, seedBody)
	}
	if !strings.Contains(string(body), "9007199254740993") {
		t.Errorf("the seed is not on the wire verbatim:\n%s", body)
	}
	if string(out.PNG) != string(in.PNG) {
		t.Errorf("PNG = %q, want %q", out.PNG, in.PNG)
	}
	if out.Kind != in.Kind || out.Index != in.Index || out.Total != in.Total ||
		out.Stem != in.Stem || out.Timeout != in.Timeout || out.Elapsed != in.Elapsed ||
		out.Pass != in.Pass || out.TotalSteps != in.TotalSteps || out.Dir != in.Dir ||
		out.Status != in.Status || out.Cancelled != in.Cancelled || out.InFlight != in.InFlight ||
		out.Err != in.Err || out.Text != in.Text || out.Version != in.Version {
		t.Errorf("a scalar field did not survive:\n got %+v\nwant %+v", out, in)
	}
	if len(out.Files) != 1 || out.Files[0] != in.Files[0] {
		t.Errorf("Files = %+v, want %+v", out.Files, in.Files)
	}
	if string(out.Caps) != string(in.Caps) {
		t.Errorf("Caps = %s, want %s", out.Caps, in.Caps)
	}
}

// TestFrame_AZeroStepIsNotAnAbsentOne.
//
// Step is `omitempty` and step zero is a real step: sd-server's first preview
// of a pass is step 0, and ember's own dedupe starts at -1 for exactly that
// reason. Dropped from the JSON and decoded back to zero is the same value, so
// nothing is lost -- which is the thing to check rather than assume, because
// the moment a zero means "unset" for one field somebody gives Index the same
// treatment and job 0 stops being distinguishable from no job at all.
func TestFrame_AZeroStepIsNotAnAbsentOne(t *testing.T) {
	body, err := json.Marshal(Frame{Kind: KindProgress, Index: 1, Total: 1, Step: 0, TotalSteps: 6})
	if err != nil {
		t.Fatal(err)
	}
	var out Frame
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatal(err)
	}
	if out.Step != 0 || out.TotalSteps != 6 {
		t.Errorf("step %d of %d, want 0 of 6", out.Step, out.TotalSteps)
	}
	// And the index is one-based, so this frame names a job rather than
	// reading as one that names none.
	if out.Index != 1 {
		t.Errorf("Index = %d; a one-based index is what keeps 'job 1' and 'no job' different bytes", out.Index)
	}
}

// TestCheckHello_RefusesEveryFirstFrameThatIsNotOne.
//
// THIS IS THE SEAL ON A NODE IMAGE OLDER THAN THE ember DRIVING IT, and it is
// the one thing on this channel that cannot be exercised against a real node
// without building an old image -- so every wrong answer is handed to it here
// instead.
//
// A version of zero is the interesting row: Version is `omitempty`, so a
// sender that never set it produces a hello with no version in it at all, and
// that decodes to zero. A check written as "refuse anything greater than
// mine" would wave it through, which is the direction that matters -- an older
// image is exactly the thing that arrives in practice.
func TestCheckHello_RefusesEveryFirstFrameThatIsNotOne(t *testing.T) {
	for _, tc := range []struct {
		name string
		f    Frame
		want string // substring the refusal must carry, so it explains itself
	}{
		{"a frame that is not a hello", Frame{Kind: KindProgress, Version: Version}, "not an ember control channel"},
		{"no kind at all", Frame{Version: Version}, "not an ember control channel"},
		{"a version from an older image", Frame{Kind: KindHello, Version: Version - 1}, "rebuild and republish the node image"},
		{"a version from a newer image", Frame{Kind: KindHello, Version: Version + 1}, "rebuild and republish the node image"},
		{"no version at all", Frame{Kind: KindHello}, "rebuild and republish the node image"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := checkHello(tc.f)
			if err == nil {
				t.Fatalf("checkHello(%+v) was accepted", tc.f)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("the refusal does not say what to do about it: %v", err)
			}
		})
	}

	// The negative control, without which a checkHello that refused
	// everything would satisfy every case above -- which is this
	// repository's own TestNoListenOutsideLoopback story.
	if err := checkHello(Frame{Kind: KindHello, Version: Version}); err != nil {
		t.Errorf("the hello this build sends was refused: %v", err)
	}
}

// TestClosed_OnlyATidyHangUpIsErrClosed.
//
// The two have to stay different, because the worker and ember act on them
// differently: for the worker a closed channel means stop, and for ember a
// channel that DROPPED means the rental failed and the operator needs the
// reason. Folding them together would file every dead laptop as a successful
// rental.
func TestClosed_OnlyATidyHangUpIsErrClosed(t *testing.T) {
	for _, status := range []websocket.StatusCode{websocket.StatusNormalClosure, websocket.StatusGoingAway} {
		err := closed(websocket.CloseError{Code: status})
		if !errors.Is(err, ErrClosed) {
			t.Errorf("a %v close is %v, want ErrClosed", status, err)
		}
	}
	for _, err := range []error{
		websocket.CloseError{Code: websocket.StatusAbnormalClosure},
		websocket.CloseError{Code: websocket.StatusMessageTooBig},
		errors.New("read tcp 127.0.0.1:7311: connection reset by peer"),
	} {
		if errors.Is(closed(err), ErrClosed) {
			t.Errorf("%v reads as a tidy hang-up, which would make a dropped channel look like a finished rental", err)
		}
	}
}

// TestResultsDir_IsAbsoluteNotUnderTheOutputsAndNotTheStore.
//
// Three properties, each with a defect behind it.
//
// Absolute, because internal/transfer refuses a relative output path -- so a
// worker writing to a relative directory would produce a landed frame ember
// could not fetch, after the generation had been paid for. The same assertion
// logx.NodeLogPath carries, and for the same mechanism.
//
// Not under /workspace, which is where outputs are written: a file there is
// swept into the run's own archive and arrives indistinguishable from a
// result -- logx.NodeLogPath's second rule, and the reason the fetch marker is
// in /tmp too.
//
// AND NOT THE STORE, which is the correction this change makes to the plan it
// implements. docs/money/stores-and-assets.md: a store "bills by the
// gigabyte-month whether or not anything is attached", nothing in ember
// deletes a file off one, and /workspace is on node.Mount's refusal list
// precisely because "a store there bills for every render for as long as the
// store exists". A worker writing results onto a store would be that leak,
// created by the mechanism meant to make a rental tidier.
func TestResultsDir_IsAbsoluteNotUnderTheOutputsAndNotTheStore(t *testing.T) {
	if !path.IsAbs(ResultsDir) {
		t.Errorf("ResultsDir = %q, which internal/transfer refuses", ResultsDir)
	}
	if path.Clean(ResultsDir) != ResultsDir {
		t.Errorf("ResultsDir = %q, which transfer.TarCommand refuses as unclean", ResultsDir)
	}
	if strings.HasPrefix(ResultsDir, "/workspace") {
		t.Errorf("ResultsDir = %q, which is where outputs are written and where a store is mounted", ResultsDir)
	}
}

// TestPort_IsNotOneTheNodeAlreadyUses.
//
// sd-server listens on the node's loopback too, and two services on one port
// means whichever started first wins -- which on a rented machine presents as
// a control channel that answers with JSON ember cannot parse, or a generation
// server that never came up. Neither message would name the collision.
func TestPort_IsNotOneTheNodeAlreadyUses(t *testing.T) {
	// Named rather than imported: internal/sdserver is a heavier package than
	// this one needs, and the number is the fact.
	const sdServerPort = 1234
	if Port == sdServerPort {
		t.Errorf("Port = %d, which is sd-server's", Port)
	}
	if Port == 22 {
		t.Error("Port = 22, which is sshd's and the one port a node publishes")
	}
	if Port < 1024 {
		t.Errorf("Port = %d, which is privileged; the worker should not need to be root to bind its own channel", Port)
	}
}
