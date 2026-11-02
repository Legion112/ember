package report

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/token"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/Legion112/ember/internal/node"
)

// renderings is every kind and the exact line it produces.
//
// THIS TABLE IS THE CONTRACT. `ember up`'s output is parsed -- by
// test/integration, and by whoever TestRunSteps_ASingleStepLooksExactlyLikeIt
// AlwaysDid was written for -- so these strings are an interface and not
// prose. Every value below was taken from the format strings this package
// replaced, so a rendering that drifts fails here rather than in a lab that
// greps for a line six commits later.
//
// A kind whose rendering is deliberately empty says so with an empty string
// rather than being left out, because left out is indistinguishable from
// forgotten.
var renderings = map[Kind]struct {
	event Event
	want  string
}{
	KindLog: {
		// Verbatim, terminator included, no prefix. These bytes came off a
		// machine ember rented.
		event: Event{Kind: KindLog, Text: "loading weights\n"},
		want:  "loading weights\n",
	},
	KindServerStarting: {
		event: Event{Kind: KindServerStarting, Cmd: "sd-server --model /opt/models/sd15.gguf"},
		want:  "starting sd-server --model /opt/models/sd15.gguf\n",
	},
	KindModelResident: {
		event: Event{Kind: KindModelResident, Measured: 6*time.Second + 40*time.Millisecond, Total: 2},
		want:  "the weights are resident; 6s of loading is paid once rather than 2 times\n",
	},
	KindUnitStarted: {
		event: Event{Kind: KindUnitStarted, Unit: UnitJob, Index: 1, Total: 2, Desc: `"a cat" 512x512 4 steps seed 1`},
		want:  "job 1/2: \"a cat\" 512x512 4 steps seed 1\n",
	},
	KindJobProgress: {
		event: Event{Kind: KindJobProgress, Unit: UnitJob, Index: 1, Total: 2, Pass: 1, Step: 1, TotalSteps: 4},
		want:  "job 1/2: pass 1, step 1/4\n",
	},
	KindJobImages: {
		event: Event{Kind: KindJobImages, Unit: UnitJob, Index: 1, Total: 2, Files: 1, Bytes: 2048},
		want:  "job 1/2: 1 file(s), 2.0 KiB\n",
	},
	KindUnitCost: {
		event: Event{Kind: KindUnitCost, Unit: UnitJob, Index: 1, Total: 2, Cost: 30000, Spent: 50000},
		want:  "job 1/2 took $0.03 of card time, $0.05 so far\n",
	},
	KindPreviewFailed: {
		event: Event{Kind: KindPreviewFailed, Unit: UnitJob, Index: 1, Total: 2, Err: "disk full"},
		want:  "job 1/2: could not write the preview frame: disk full\n",
	},

	// Both views of memory, in bytes, with no verdict: what to do about a
	// node that cannot hold the weights is a decision to take once there are
	// measurements, and a line saying "this will be slow" would be that
	// decision taken from a guess.
	KindNodeMemory: {
		event: Event{
			Kind: KindNodeMemory, MemCgroup: 2,
			MemLimit: 64 << 30, MemCurrent: 12 << 30,
			MemTotal: 700 << 30, MemCached: 48 << 30,
			Bytes: 56 << 30,
		},
		want: "node memory: cgroup v2 limit 64.0 GiB, using 12.0 GiB; " +
			"host reports 700.0 GiB total, 48.0 GiB cached; weights are 56.0 GiB\n",
	},

	// Silent on purpose: the CLI reports all five through slog, and
	// lifecycle_test.go greps for the slog spelling of "node ready".
	KindNodeReady:     {event: Event{Kind: KindNodeReady, Node: "local/abc"}, want: ""},
	KindRunCost:       {event: Event{Kind: KindRunCost, Cost: 120000}, want: ""},
	KindDestroyFailed: {event: Event{Kind: KindDestroyFailed, Err: "boom"}, want: ""},
	KindRunFinished:   {event: Event{Kind: KindRunFinished}, want: ""},
	KindRunFailed:     {event: Event{Kind: KindRunFailed, Err: "boom"}, want: ""},
}

// A node with no cgroup limit says "none" rather than "0 B".
//
// A limit of zero bytes would be a container that cannot allocate anything,
// which is the opposite claim -- and the direction that would make an
// unbounded node look like the problem. Its own test rather than a second
// entry in the table above, because that table is keyed by Kind and a second
// row for one kind would have to invent a key that is not one.
func TestText_ANodeWithNoMemoryLimitSaysSo(t *testing.T) {
	var buf bytes.Buffer
	Text(&buf).Emit(Event{
		Kind: KindNodeMemory, MemCgroup: 1,
		MemTotal: 700 << 30, Bytes: 56 << 30,
	})
	want := "node memory: cgroup v1 limit none, using 0 B; " +
		"host reports 700.0 GiB total, 0 B cached; weights are 56.0 GiB\n"
	if got := buf.String(); got != want {
		t.Errorf("rendered %q, want %q", got, want)
	}
}

// And a node that answered nothing at all names the hierarchy as unreadable
// rather than printing "cgroup v0".
func TestText_ANodeThatAnsweredNothingSaysThatToo(t *testing.T) {
	var buf bytes.Buffer
	Text(&buf).Emit(Event{Kind: KindNodeMemory})
	want := "node memory: unreadable limit none, using 0 B; " +
		"host reports 0 B total, 0 B cached; weights are 0 B\n"
	if got := buf.String(); got != want {
		t.Errorf("rendered %q, want %q", got, want)
	}
}

func TestText_RendersEachKindExactly(t *testing.T) {
	for kind, c := range renderings {
		var buf bytes.Buffer
		Text(&buf).Emit(c.event)
		if got := buf.String(); got != c.want {
			t.Errorf("%s rendered %q, want %q", kind, got, c.want)
		}
	}
}

// TestText_DecidesAboutEveryKind is the completeness guard.
//
// It reads the Kind constants out of this package's own source rather than
// trusting Kinds() to list them, because the failure being prevented is
// somebody adding a constant and nothing else: render's switch would fall
// through, the line would simply stop appearing, and no test would notice. A
// default case in render would hide exactly that.
func TestText_DecidesAboutEveryKind(t *testing.T) {
	declared := kindConstants(t)
	if len(declared) == 0 {
		t.Fatal("found no Kind constants; the parse is broken, not the package")
	}

	listed := map[Kind]bool{}
	for _, k := range Kinds() {
		listed[k] = true
	}

	for _, name := range declared {
		k := kindByName(t, name)
		if !listed[k] {
			t.Errorf("%s is declared but Kinds() does not list it, so nothing checks what it renders", name)
		}
		if _, ok := renderings[k]; !ok {
			t.Errorf("%s has no entry in renderings, so no test says what it looks like; add one, with \"\" if it is deliberately silent", name)
		}
	}
	if len(listed) != len(declared) {
		t.Errorf("Kinds() lists %d kinds but %d are declared", len(listed), len(declared))
	}
}

// kindConstants returns the names of every constant declared with type Kind.
func kindConstants(t *testing.T) []string {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "report.go", nil, 0)
	if err != nil {
		t.Fatalf("parse report.go: %v", err)
	}

	var out []string
	ast.Inspect(f, func(n ast.Node) bool {
		vs, ok := n.(*ast.ValueSpec)
		if !ok {
			return true
		}
		id, ok := vs.Type.(*ast.Ident)
		if !ok || id.Name != "Kind" {
			return true
		}
		for _, name := range vs.Names {
			out = append(out, name.Name)
		}
		return true
	})
	return out
}

// kindByName maps a constant's name to its value, so the AST walk above can
// be compared against real values.
func kindByName(t *testing.T, name string) Kind {
	t.Helper()
	byName := map[string]Kind{
		"KindLog": KindLog, "KindServerStarting": KindServerStarting,
		"KindModelResident": KindModelResident, "KindUnitStarted": KindUnitStarted,
		"KindJobProgress": KindJobProgress, "KindJobImages": KindJobImages,
		"KindUnitCost": KindUnitCost, "KindPreviewFailed": KindPreviewFailed,
		"KindNodeReady": KindNodeReady, "KindNodeMemory": KindNodeMemory,
		"KindRunCost":       KindRunCost,
		"KindDestroyFailed": KindDestroyFailed,
		"KindRunFinished":   KindRunFinished, "KindRunFailed": KindRunFailed,
	}
	k, ok := byName[name]
	if !ok {
		t.Fatalf("%s is declared in report.go but this test does not know its value; add it to kindByName", name)
	}
	return k
}

// TestText_StepsReadExactlyAsTheyDidBeforeTheyWereEvents pins the other
// spelling of the two lines UnitStep shares with UnitJob.
//
// `step 1/3 took $... of card time` and `job 1/2 took $... of card time` were
// two format strings in two functions -- one sentence maintained twice. They
// are one now, and this is the half that outputs_test.go matches with a
// regexp.
func TestText_StepsReadExactlyAsTheyDidBeforeTheyWereEvents(t *testing.T) {
	var buf bytes.Buffer
	s := Text(&buf)
	s.Emit(Event{Kind: KindUnitStarted, Unit: UnitStep, Index: 1, Total: 3, Desc: "sd-cli --prompt a"})
	s.Emit(Event{Kind: KindUnitCost, Unit: UnitStep, Index: 1, Total: 3, Cost: 30000, Spent: 30000})

	got := buf.String()
	if want := "step 1/3: sd-cli --prompt a\n"; !strings.Contains(got, want) {
		t.Errorf("got %q, want it to contain %q", got, want)
	}

	// The regexp outputs_test.go uses, so a rendering that stops matching it
	// fails here rather than in a lab.
	re := regexp.MustCompile(`step (\d+)/(\d+) took \$([0-9.e+-]+) of card time, \$([0-9.e+-]+) so far`)
	if !re.MatchString(got) {
		t.Errorf("got %q, want it to match %v", got, re)
	}
}

// TestText_IgnoresAKindItHasNeverHeardOf.
//
// Kind is a string, so an unknown value is representable -- and it has a real
// source: events are persisted, so a database written by a newer ember can
// hand this renderer a kind that did not exist when it was compiled. Rendering
// nothing is the right answer there. Panicking or printing a Go value would
// turn reading old history into a crash, and the record is the thing an
// operator came to look at.
func TestText_IgnoresAKindItHasNeverHeardOf(t *testing.T) {
	var buf bytes.Buffer
	Text(&buf).Emit(Event{Kind: "something_a_later_ember_added", Text: "x"})

	if got := buf.String(); got != "" {
		t.Errorf("rendered %q for an unknown kind, want nothing", got)
	}
}

func TestLabel(t *testing.T) {
	if got, want := Label(UnitJob, 1, 2), "job 1/2"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	if got, want := Label(UnitStep, 10, 10), "step 10/10"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestHumanBytes(t *testing.T) {
	for _, c := range []struct {
		in   int64
		want string
	}{
		{0, "0 B"},
		{1023, "1023 B"},
		{1024, "1.0 KiB"},
		{2048, "2.0 KiB"},
		{1536 * 1024, "1.5 MiB"},
		{3 * 1024 * 1024 * 1024, "3.0 GiB"},
		{5 * 1024 * 1024 * 1024 * 1024, "5.0 TiB"},
	} {
		if got := HumanBytes(c.in); got != c.want {
			t.Errorf("HumanBytes(%d) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestText_MoneyIsRenderedByTheMoneyType guards against the web ever
// formatting a price itself.
//
// MicroUSD.String() is "$%.6g", which nobody reproduces by accident. The
// renderer must go through the type, so there is one spelling of a price in
// the program and a UI showing a different one is impossible.
func TestText_MoneyIsRenderedByTheMoneyType(t *testing.T) {
	var buf bytes.Buffer
	cost, spent := node.MicroUSD(289999), node.MicroUSD(1_234_567)
	Text(&buf).Emit(Event{Kind: KindUnitCost, Unit: UnitJob, Index: 1, Total: 1, Cost: cost, Spent: spent})

	if got := buf.String(); !strings.Contains(got, cost.String()) || !strings.Contains(got, spent.String()) {
		t.Errorf("got %q, want it to contain %q and %q", got, cost, spent)
	}
}
