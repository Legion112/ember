package cli

import (
	"bytes"
	"go/ast"
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/tools/go/packages"

	"github.com/Legion112/ember/internal/config"
	"github.com/Legion112/ember/internal/dockerengine"
	"github.com/Legion112/ember/internal/genstore"
	"github.com/Legion112/ember/internal/node"
	"github.com/Legion112/ember/internal/queue"
)

// The period ceilings on the two rentals a SHELL starts, which had none.
//
// WHAT WAS WRONG (#165). Five paths write `requests.cost` -- the column
// `genstore.Spend` sums with no status filter -- and only two of them were
// projected against `max_day_usd` and `max_month_usd`: a queued request and
// the queue's own store fill. `ember up` in a shell, `ember up` submitted over
// the socket and `ember store sync` in a shell reached no ceiling at all. The
// issue read as a socket defect and it was not: the cause predates the socket,
// because `config.PeriodCeilings` had exactly TWO non-test callers when it was
// filed -- the one gate in `internal/queue` and a startup PRINT in
// `cmd_serve.go` -- so neither config was read on any of the three.
//
// WHY A UNIT TEST AND NOT A LAB. The gate fires before anything is created, on
// a projection built from the offer's own price, so the only thing it needs is
// a PAID provider that can reach an offer. Nothing under `test/` configures
// either key and every lab rents from the local provider at zero, where a
// money ceiling must never refuse -- so a lab would assert a free rental
// against a free rental and pass against the defect, which is exactly how
// #155 and #157 found their gates unable to fail.
//
// THE PAID PROVIDER IS `fakeCatalogue`, which was already here: its `gpuBody`
// prices the RTX 4090 at $0.89/hr secure, and it serves no `/v2/pods`, so a
// run that got PAST the gate stops at the create call -- which is what the
// acceptance arms want, because what they have to prove is that the gate let
// it through and not that a pod was made.

// paidUpConfig is a config `ember up` would run against RunPod, over a whole
// hour so the projection at $0.89/hr is exactly $0.89.
//
// No `state_dir`, so the ledger lands where `app.ResolvePaths` puts it for
// this test's HomeDir -- the same path `recorded` reads and `seedRental` writes.
// One name for a path: a second spelling here is how a test ends up asserting
// against a directory nothing wrote to.
func paidUpConfig(t *testing.T, extra string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), config.FileName)
	body := "provider: runpod\n" +
		"image: ghcr.io/example/sd@sha256:" + strings.Repeat("a", 64) + "\n" +
		"ttl: 1h\n" +
		extra
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// seedRental puts one finished rental's cost into the ledger this command will
// read, through the store's own interface rather than by writing SQL.
//
// `created` is `time.Now()` at every call site, because `boundAndRecord` reads
// the ambient clock: `runUp` has no clock to inject and giving it one would be
// a production signature shaped by a test's convenience. What that costs is
// written where it is paid -- see the month-key check in the test below.
func seedRental(t *testing.T, d deps, created time.Time, cost node.MicroUSD) {
	t.Helper()
	s, err := genstore.Open(filepath.Join(d.HomeDir, "state"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()

	id, err := s.Enqueue(t.Context(), genstore.Request{
		Provider: "runpod", GPU: "rtx4090", TTL: time.Hour, Created: created,
		Jobs: []genstore.Job{{Prompt: "a cat"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Update(t.Context(), id, func(r *genstore.Request) {
		r.Status = genstore.StatusDone
		r.RunID = "run-" + string(id)
		r.Started, r.Ended = created, created.Add(time.Hour)
		r.Cost, r.Billable = cost, time.Hour
	}); err != nil {
		t.Fatal(err)
	}
}

// TestUp_ProjectsTheChosenCardAgainstThePeriodCeilings.
//
// THE JOIN, which is the half neither `Ceilings.Allow`'s own tests nor the
// queue's can make: that `ember up` in a shell ARRIVES at the gate, with the
// card it chose and the deadline the node will get. A gate and a test that
// each agree with themselves across a gap nothing crosses is the failure this
// repository has paid for repeatedly.
//
// THREE ARMS, and the first two do not read a clock at all.
//
//   - A $0.50 ceiling against a $0.89 projection refuses with NOTHING spent,
//     so it is red for a gate that is missing, for one whose projection is a
//     literal zero, and for one that reads a zero TTL -- the two halves #157
//     measured as gates that cannot fail.
//   - A $20 ceiling against the same projection ALLOWS, which is the negative
//     control: without it every refusal here is consistent with a gate that
//     refuses every paid rental there is, and the row that then appears in the
//     ledger is the witness that it really went through.
//   - $19.45 already spent against a $20 ceiling refuses at $20.34, which is
//     the only arm that proves the LEDGER is read. #164's three numbers: a
//     gate that ignored the ledger compares $0.89 against $20 and allows; one
//     that REPORTED rather than PROJECTED compares $19.45 against $20 and
//     allows; the live one adds them and refuses.
//
// THE CEILING IS MATCHED AS "ceiling of $20," AND NOT AS "$20", because "$20"
// is a substring of the "$20.34" two fields later in the same message --
// measured in #164 as an assertion held up by the total beside it.
//
// AND A REFUSED RUN LEAVES NO ROW, which is the second witness that the gate
// fired where it claims to: `boundAndRecord` checks the ceiling before Adopt
// writes one, so a row would mean the ledger had been told a rental was coming
// when none was.
func TestUp_ProjectsTheChosenCardAgainstThePeriodCeilings(t *testing.T) {
	t.Run("a ceiling under the projection refuses with nothing spent", func(t *testing.T) {
		d, _ := (&fakeCatalogue{}).deps(t)
		cfg := paidUpConfig(t, "max_month_usd: \"0.50\"\n")

		var out bytes.Buffer
		err := runUp([]string{"--config", cfg}, &out, d)
		if err == nil {
			t.Fatal("a rental projected past the month's ceiling was run")
		}
		for _, want := range []string{"max_month_usd", "ceiling of $0.5,", "$0.89"} {
			if !strings.Contains(err.Error(), want) {
				// The price is `gpuBody`'s, so a fake that no longer prices
				// the 4090 at $0.89/hr reads here as a gate that did not
				// fire. Said in the message, because a figure that moved and
				// a gate that broke are otherwise the same red line.
				t.Errorf("the refusal does not mention %q, so it is not the period ceiling's "+
					"(or gpuBody no longer prices the 4090 at $0.89/hr): %v", want, err)
			}
		}
		if rows := recorded(t, d); len(rows) != 0 {
			t.Errorf("a refused run wrote %d row(s): the ceiling is checked before Adopt, so a "+
				"row means the ledger was told a rental was coming when none was", len(rows))
		}
	})

	t.Run("a ceiling the projection fits inside does not", func(t *testing.T) {
		d, _ := (&fakeCatalogue{}).deps(t)
		cfg := paidUpConfig(t, "max_month_usd: \"20\"\n")

		var out bytes.Buffer
		err := runUp([]string{"--config", cfg}, &out, d)
		if err == nil {
			// The fake serves no /v2/pods, so this cannot be reached.
			t.Fatal("the run rented against a fake with no pods route")
		}
		if strings.Contains(err.Error(), "max_month_usd") {
			t.Fatalf("a rental well inside its ceiling was refused, so the gate refuses "+
				"everything rather than refusing what crosses a ceiling: %v", err)
		}
		// Positively: it got past the gate and the row was adopted. Asserting
		// only "not the ceiling refusal" would pass for a run that stopped
		// somewhere else entirely.
		if rows := recorded(t, d); len(rows) != 1 {
			t.Errorf("the history holds %d row(s), want the run the gate allowed", len(rows))
		}
	})

	t.Run("what has been spent is read", func(t *testing.T) {
		d, _ := (&fakeCatalogue{}).deps(t)
		cfg := paidUpConfig(t, "max_month_usd: \"20\"\n")

		before := time.Now().UTC()
		seedRental(t, d, before, 19_450_000)

		var out bytes.Buffer
		err := runUp([]string{"--config", cfg}, &out, d)
		if err == nil {
			t.Fatal("a rental projected past the month's ceiling was run")
		}
		if queue.PeriodMonth.Key(before) != queue.PeriodMonth.Key(time.Now().UTC()) {
			// The one clock dependence in this file, named rather than hidden:
			// `runUp` reads time.Now, so a UTC month boundary crossed between
			// the seed and the gate puts the seeded rental in the previous
			// period and the gate correctly allows. A red line here is that
			// and not a defect.
			t.Fatalf("the UTC month turned over during this test, so the seeded rental is in " +
				"the previous period: re-run it")
		}
		for _, want := range []string{"max_month_usd", "$19.45", "ceiling of $20,", "$0.89", "$20.34"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("the refusal does not mention %q, so what has been spent was not "+
					"read: %v", want, err)
			}
		}
		// Still only the seeded rental: nothing was adopted.
		if rows := recorded(t, d); len(rows) != 1 {
			t.Errorf("the history holds %d row(s), want only the seeded rental", len(rows))
		}
	})
}

// TestUp_RefusesAPeriodCeilingThatIsNotAPrice.
//
// A PATH THE KEY HAD NEVER BEEN PARSED ON. `max_day_usd: "banana"` in the
// config an `ember up` names was accepted and the run went ahead before this
// change -- not refused, not warned about -- because nothing between
// `config.Load` and the create call read the key at all. `KnownFields(true)`
// catches a key that does not exist; it has nothing to say about a value that
// is not a price.
//
// The refusal names the key and quotes the string as typed, which is
// `ParseUSD`'s own contract, and it happens before anything is created.
func TestUp_RefusesAPeriodCeilingThatIsNotAPrice(t *testing.T) {
	d, _ := (&fakeCatalogue{}).deps(t)
	cfg := paidUpConfig(t, "max_day_usd: \"banana\"\n")

	var out bytes.Buffer
	err := runUp([]string{"--config", cfg}, &out, d)
	if err == nil {
		t.Fatal("a ceiling that is not a price was accepted and the run went ahead")
	}
	for _, want := range []string{"max_day_usd", "banana"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not mention %q: %v", want, err)
		}
	}
	if rows := recorded(t, d); len(rows) != 0 {
		t.Errorf("a run refused for an unparseable ceiling wrote %d row(s)", len(rows))
	}
}

// TestUp_ADryRunIsNotRefusedByAPeriodCeiling.
//
// `--dry-run` rents nothing, so a money ceiling that refused it would be
// refusing a command that cannot spend -- the same mistake as refusing a free
// rental, which `Ceilings.Allow` is careful not to make. Both shell callers
// return before `boundAndRecord` for that reason.
//
// The ceiling here is one no rental could satisfy, so this is red for a gate
// moved above the dry-run return.
func TestUp_ADryRunIsNotRefusedByAPeriodCeiling(t *testing.T) {
	d, _ := (&fakeCatalogue{}).deps(t)
	cfg := paidUpConfig(t, "max_month_usd: \"0.000001\"\n")

	var out bytes.Buffer
	if err := runUp([]string{"--config", cfg, "--dry-run"}, &out, d); err != nil {
		t.Fatalf("a dry run was refused by a money ceiling: %v", err)
	}
	if !strings.Contains(out.String(), "nothing was created") {
		t.Errorf("--dry-run did not say it created nothing:\n%s", out.String())
	}
}

// TestUp_AFreeRentalIsNeverRefusedByASpentCeiling.
//
// The local provider's price is zero, so its projection is zero and it adds
// nothing to any period. Refusing one because a paid month is spent would stop
// the one kind of run that costs nothing, which is the opposite of what the
// ceiling is for -- and it is the direction this change could most easily have
// got wrong, because `ember up` against the local provider is how most of
// README's measurements were taken.
//
// A millionth of a dollar as the ceiling and a thousand dollars already spent,
// so there is no reading of the arithmetic under which a non-free rental would
// be allowed. It gets as far as the create call, which the recording engine is
// told to fail.
func TestUp_AFreeRentalIsNeverRefusedByASpentCeiling(t *testing.T) {
	d := labDeps(t)
	d.Engine.(*dockerengine.RecordingEngine).FailOn = "ContainerCreate"
	seedRental(t, d, time.Now().UTC(), 1_000_000_000)

	cfg := filepath.Join(t.TempDir(), config.FileName)
	body := "provider: local\nimage: x:v1\nttl: 5m\n" +
		"max_day_usd: \"0.000001\"\nmax_month_usd: \"0.000001\"\n"
	if err := os.WriteFile(cfg, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	err := runUp([]string{"--config", cfg}, &out, d)
	if err == nil {
		t.Fatal("the create call was supposed to fail")
	}
	if strings.Contains(err.Error(), "max_day_usd") || strings.Contains(err.Error(), "max_month_usd") {
		t.Fatalf("a free rental was refused by a money ceiling: %v", err)
	}
	// And it was recorded, which is the witness that it reached the rental:
	// the seeded row plus this one.
	if rows := recorded(t, d); len(rows) != 2 {
		t.Errorf("the history holds %d row(s), want the seeded rental and this free one", len(rows))
	}
}

// breakLedger puts a DIRECTORY where `generations.db` goes, which is a failure
// neither `app.ResolvePaths` nor the run itself cares about: only the thing
// that opens the database sees it.
func breakLedger(t *testing.T, d deps) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(d.HomeDir, "state", genstore.FileName), 0o700); err != nil {
		t.Fatal(err)
	}
}

// TestUp_ALedgerThatWillNotOpenRefusesOnlyWhatACeilingCouldRefuse.
//
// THE ONE ASYMMETRY IN boundAndRecord, asserted in THREE directions because
// any two of them are consistent with the defect review found.
//
// Recording is best-effort: a database that will not open is a diagnostic
// problem, and refusing to rent over it would be ember declining to do the
// thing it is for because it could not take notes. That rule is older than
// this change and `recordRental` states it.
//
// Bounding is not: an operator who wrote `max_month_usd` has said unattended
// spending must stop somewhere, and "I could not read what has been spent" is
// not "it is allowed" -- the rule that makes `ember ls` exit non-zero over a
// provider it could not ask. A ceiling that silently does nothing is worse
// than one that was never configured.
//
// AND A FREE RENTAL IS EXEMPT FROM BOTH, which is the arm this test was built
// without and the reason it is three arms rather than two. #165's first form
// asked `cs.IsZero()` here and nothing about the price, so
// `ember up --provider local` -- projection zero, adding nothing to any period
// -- was refused outright by a ceiling ember could not check. **The free arm
// used to BE the refusal arm**: its config read `provider: local` with
// `max_month_usd: "50"`, so this test asserted the defect and went red when
// the missing condition was added. So the refusal arm is re-pointed at a PAID
// provider and the free one put beside it, which is the pair that
// discriminates -- a refusal arm on a free provider cannot tell "a ceiling
// with no ledger refuses" from "a ceiling with no ledger refuses everything".
func TestUp_ALedgerThatWillNotOpenRefusesOnlyWhatACeilingCouldRefuse(t *testing.T) {
	t.Run("a PAID rental with a ceiling is refused", func(t *testing.T) {
		d, _ := (&fakeCatalogue{}).deps(t)
		breakLedger(t, d)
		cfg := paidUpConfig(t, "max_month_usd: \"50\"\n")

		var out bytes.Buffer
		err := runUp([]string{"--config", cfg}, &out, d)
		if err == nil {
			t.Fatal("a configured ceiling with nothing to read it against was treated as satisfied")
		}
		if !strings.Contains(err.Error(), "will not rent") {
			t.Errorf("the refusal does not say nothing was rented: %v", err)
		}
		if !strings.Contains(err.Error(), "$50") {
			t.Errorf("the refusal does not name the ceiling it could not check: %v", err)
		}
		// Not the printed line, which is what a run the ceiling cannot refuse
		// gets.
		if strings.Contains(out.String(), "not recording") {
			t.Errorf("a configured ceiling declined to record and ran anyway:\n%s", out.String())
		}
	})

	t.Run("a FREE rental with the same ceiling is not", func(t *testing.T) {
		d := labDeps(t)
		d.Engine.(*dockerengine.RecordingEngine).FailOn = "ContainerCreate"
		breakLedger(t, d)
		cfg := filepath.Join(t.TempDir(), config.FileName)
		body := "provider: local\nimage: x:v1\nttl: 5m\nmax_month_usd: \"50\"\n"
		if err := os.WriteFile(cfg, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}

		var out bytes.Buffer
		err := runUp([]string{"--config", cfg}, &out, d)
		if err == nil {
			t.Fatal("the create call was supposed to fail")
		}
		if strings.Contains(err.Error(), "will not rent") {
			t.Fatalf("a FREE rental was refused by a money ceiling ember could not check, which "+
				"is the one kind of run that costs nothing: %v", err)
		}
		if !strings.Contains(out.String(), "not recording") {
			t.Errorf("a run that could not be recorded said nothing about it:\n%s", out.String())
		}
	})

	t.Run("a paid rental with no ceiling is not either", func(t *testing.T) {
		d, _ := (&fakeCatalogue{}).deps(t)
		breakLedger(t, d)
		cfg := paidUpConfig(t, "")

		var out bytes.Buffer
		err := runUp([]string{"--config", cfg}, &out, d)
		if err == nil {
			// The fake serves no /v2/pods, so this cannot be reached.
			t.Fatal("the run rented against a fake with no pods route")
		}
		if strings.Contains(err.Error(), "will not rent") {
			t.Fatalf("a run with no ceiling configured was refused for an unreadable ledger: %v", err)
		}
		if !strings.Contains(out.String(), "not recording") {
			t.Errorf("a run that could not be recorded said nothing about it:\n%s", out.String())
		}
	})
}

// TestServe_WiresTheDaemonsOwnConfigIntoTheSubmittedRunGate.
//
// THE #105 SHAPE, REFUSED IN ADVANCE. `webapi.Options` had no `Scan` field for
// the life of `ember serve`, `New` never assigned the Server's `scan`, and the
// three assignments in the tree were all in TEST files reaching past the
// constructor -- so the suite could not tell "the production value is missing"
// from "this test did not set it", and `GET /api/nodes` answered 501 in every
// shipped build. Every test in this file and in `internal/queue`'s
// moneygate_test.go sets `DirectRunner.Config` itself, which is exactly that
// arrangement: a daemon wired without it would be bounded by the run's own
// config alone and every one of them would stay green.
//
// NOT UNREPRESENTABLE, WHICH WAS THE FIRST CHOICE. A constructor that took the
// config would make forgetting it a compile error, which this repository
// prefers to a test -- and it cannot help here, because a ZERO config is a
// legitimate value: an operator who set no ceiling has one. So no runtime
// check and no type can tell "nobody wired it" from "nothing is configured",
// which is #105's own sentence about why its defect survived.
//
// THREE VERSIONS, AND THE FIRST TWO BOTH OVER-CLAIMED IN THEIR PROSE -- which
// is the failure this PR corrected once and then committed again, so the
// history is the useful part.
//
// v1 checked that the literal NAMED `Config` and said nothing about what it
// held: `Config: config.Config{}` was green across `go build`, `go vet`,
// `golangci-lint` and `go test ./...`, the guard included.
//
// v2 required the value to be an identifier the enclosing function "bound to a
// config.Config" -- and implemented that as a set of NAMES, so it was a
// name-plus-type check wearing a value check's prose. Three more walkarounds
// were green: `cfg = config.Config{}` one line above the wiring, because the
// parameter's own name still covered it; an inner block shadowing it with
// `cfg := config.Config{}`; and an immediately-invoked
// `func(cfg config.Config){...}(config.Config{})`. Only the last of those is
// the bound v2's prose claimed -- the other two write the zero INSIDE the
// function the guard analyses, one line from the construction site.
//
// SO v3 ASKS THE TYPE CHECKER, and the shadow stops being a heuristic. The
// value's `types.Object` must BE the object a `config.Config` parameter bound,
// or the one a `:= config.Load` defined. A shadow is a different object by
// construction, so it fails without this guard knowing what a block, a range
// clause, a type switch or a nested closure is -- which is the whack-a-mole
// that a name-keyed version would have had to chase one syntax form at a time.
// `go/packages` is already a direct dependency and `TestNoProductionDeadCode`
// already loads the tree through it, so this is the tool the repository
// reaches for when a question is about identity rather than spelling.
//
// AND A WRITE IS THE ONE THING IDENTITY CANNOT ANSWER, because `cfg =
// config.Config{}` and `cfg.MaxDayUSD = ""` both leave the object alone and
// change what is in it -- which is dataflow. It is refused rather than
// reasoned about: **an object the function wrote to BY ANY STATEMENT is no
// longer accepted**, and the failure says which way. Getting that sentence to
// be true took six attempts and five wrong ones:
//
//   - v2's prose said "bound to a config.Config" over a set of NAMES;
//   - its fix said "assigned to ... anywhere in the function" while the code
//     looked only for a bare identifier on the left, so `cfg.MaxDayUSD = ""`
//     was green -- the most plausible walkaround of the lot, since it reads as
//     normalisation and zeroes exactly the two ceiling fields;
//   - and that fix still missed the two write forms that are not an
//     `*ast.AssignStmt` at all: a range clause with `token.ASSIGN`, where
//     `for _, cfg.MaxDayUSD = range []string{""} {}` zeroed a ceiling with the
//     guard green, and `*ast.IncDecStmt`.
//
// Go has exactly those three statement nodes that write to an existing
// variable, plus `&x`; all four are recorded, each through `rootIdent`. A
// type-switch guard is `:=`-only, a `SendStmt` writes to a channel rather than
// to a variable, `var x T = cfg` binds a new object, and a `select` clause's
// assignment is an `AssignStmt`. The choice at the last step was two `case`
// arms or another attempt at wording a bound, and the code is the cheaper and
// more durable answer -- which is the general lesson rather than a fact about
// this guard.
//
// AND "BY ANY STATEMENT" IS THE FIFTH WRONG WORDING, CORRECTED: the sentence
// used to claim no qualifying clause at all, and a write can reach the object
// through an EXPRESSION instead. A pointer-receiver method takes the address
// implicitly, with no `&` anywhere in the AST -- measured, by adding a
// `func (c *Config) ZeroCeilings()` to `internal/config` and calling
// `cfg.ZeroCeilings()` above the wiring: the guard stays green and the whole
// tree with it, both ceilings zeroed. The claim was vacuous rather than wrong,
// because there is no such method; the boast was what over-reached.
//
// SO THE PROPERTY IT RESTS ON IS ASSERTED NOW, by
// TestConfig_DeclaresNoPointerReceiverMethodOnConfig below, which is an
// unenforced dependency turned into an exit code. A `Normalise()` or
// `ApplyDefaults()` on `Config` is a thing people write for good reasons, and
// nothing would have reported this guard going quiet when they did.
//
// That is the allow-list direction and a refusal rather than an analysis, which
// is what keeps it from becoming the scope-aware dataflow pass that would be
// the wrong tool here. Measured: no production function in this package that
// builds a DirectRunner writes to its config by any of those statements.
//
// WHAT IS STILL OPEN, and it is now the bound the prose claims rather than two
// more: a config handed ACROSS A CALL. Any function or closure that TAKES a
// `config.Config` and builds a DirectRunner is accepted, whatever its callers
// pass -- so `func(c config.Config){ ... }(config.Config{})` is green, and so
// is the same thing through a named helper. This guard stops at the function
// boundary, as `declaredFlags` does.
//
// THE INLINE FORM COULD BE CLOSED AND DELIBERATELY IS NOT, because an
// immediately-invoked closure is the one call whose arguments are right there
// to read -- about fifteen lines. It is declined because it would make the
// guard refuse `func(c config.Config){ ... }(zero)` and accept `helper(zero)`
// where `helper` is identical, which is one rule answering two ways: the same
// reason a closure's parameters are accepted at all. The bound is the function
// in both forms, which is a bound somebody can hold in their head; "the
// function, except when the call happens to be visible" is not. Measured both
// ways rather than argued: the inline form is green and every same-function
// walkaround below it is red.
//
// What IS closed is every one-step version: a zero at the construction site, a
// zero assigned into the parameter, a zero written into one of its FIELDS, a
// pointer taken to it, and a shadow in any scope -- a block, a range clause, an
// if-statement initialiser or a closure with no config parameter, none of which
// this guard has to know the syntax of, because a shadow is a different object.
//
// A POSITIONAL literal is a FATAL rather than a skip, because the alternative
// to failing on a literal this guard cannot read is reading straight past the
// one that matters -- TestFlagTablesClassifyEveryFlagSetMethod's rule. The
// floor is the other half: a walk that found no construction passes every
// assertion.
//
// A POINTER FIELD REFUSED AS NIL IN Start WAS WEIGHED AND IS NOT THE ANSWER.
// It is this package's own shape for a dependency that must be wired -- Start
// already refuses a nil Queue and a nil Store -- and it catches strictly less:
// a nil says "nobody wired it" and says nothing about any of the walkarounds
// above, every one of which hands over a zero rather than nothing.
func TestServe_WiresTheDaemonsOwnConfigIntoTheSubmittedRunGate(t *testing.T) {
	pkgs, err := packages.Load(&packages.Config{
		// Syntax and type info for THIS package's production files. Tests are
		// left out deliberately: the subject is what `ember serve` wires, and
		// every test in the tree sets the field itself, which is the whole
		// reason this guard exists.
		Mode: packages.NeedName | packages.NeedSyntax | packages.NeedTypes | packages.NeedTypesInfo,
	}, ".")
	if err != nil {
		t.Fatalf("load internal/cli: %v", err)
	}
	// A BELT WHOSE BRACES ARE THE COMPILER, said accurately rather than
	// credited with the catch: a type error in this package means no test
	// binary at all, so `go test` fails before this line is ever reached.
	// What this answers is the case go/packages reports without the compiler
	// having an opinion -- a load that resolved nothing, which would otherwise
	// reach the floor below as "no DirectRunner found" and blame the wrong
	// thing.
	if n := packages.PrintErrors(pkgs); n > 0 {
		t.Fatalf("internal/cli did not load cleanly (%d error(s)), so this guard cannot read it", n)
	}

	found := 0
	for _, pkg := range pkgs {
		for _, f := range pkg.Syntax {
			for _, d := range f.Decls {
				fn, ok := d.(*ast.FuncDecl)
				if !ok || fn.Body == nil {
					continue
				}
				c := configObjects(pkg.TypesInfo, fn)
				ast.Inspect(fn.Body, func(n ast.Node) bool {
					lit, ok := n.(*ast.CompositeLit)
					if !ok || !isDirectRunner(lit) {
						return true
					}
					found++
					checkDirectRunnerConfig(t, pkg.Fset, pkg.TypesInfo, lit, c)
					return true
				})
			}
		}
	}
	if found == 0 {
		t.Fatal("found no production queue.DirectRunner to check, so this guard asserted nothing")
	}
}

// TestConfig_DeclaresNoPointerReceiverMethodOnConfig.
//
// THE ONE PROPERTY THE WIRING GUARD RESTS ON AND DID NOT CHECK. That guard
// refuses a `DirectRunner` whose `Config` is an object the function wrote to,
// and it finds writes by walking STATEMENTS -- an assignment, a range clause,
// an inc/dec, an `&`. A write can also arrive through an EXPRESSION: a
// pointer-receiver method takes the address of its receiver implicitly, so
// `cfg.ZeroCeilings()` mutates the config with no `&` and no assignment
// anywhere in the AST, and the guard stays green. Measured, by adding such a
// method to `internal/config` and calling it above the wiring: green across
// `go build`, `go vet`, `golangci-lint` and `go test ./...`, with both
// ceilings zeroed and `Tightest` composing a zero -- which is #165 again.
//
// IT IS VACUOUS TODAY AND THAT WAS THE WHOLE DEFENCE, which is exactly the
// shape this repository refuses to leave on trust: `internal/config` declares
// no pointer-receiver method on `Config`, so there is no such call to catch --
// and nothing said so with an exit code. A `Normalise()` or an
// `ApplyDefaults()` on `Config` is a thing people write for good reasons, and
// the day somebody does, the wiring guard goes quiet and no test anywhere
// reports it.
//
// SO THE ASSERTION IS HERE RATHER THAN IN internal/config, because the rule
// being enforced is this GUARD's bound and not a fact about that package's
// design. A reader who adds the method needs to be told which guard just
// stopped seeing writes and what the two options are; a failure in the config
// package's own tests would read as "you may not add a pointer-receiver
// method", which is not the rule.
//
// THE FLOOR IS THE OTHER HALF. `Config` has value-receiver methods --
// `Budgets`, `PeriodCeilings`, `ParsePorts` and more -- so a load that
// resolved nothing would otherwise find no pointer receivers and pass. Zero
// methods is a fatal.
//
// IT ASKS ONE QUESTION OF THE TYPE, AND THAT IS WHY IT IS NOT AN AST WALK.
// `sig.Recv().Type().(*types.Pointer)` is the whole test, so genericity cannot
// hide a receiver from it. Measured rather than reasoned, by type-checking a
// synthetic `type Config[T any]` with both receiver forms:
// `*p.Config[T]` asserts to `*types.Pointer` TRUE and `p.Config[T]` asserts
// FALSE -- so a generic pointer receiver is caught and a generic value
// receiver is still correctly ignored.
//
// `TestTrustHasOneImplementationAndItIsPin` is the cautionary one: it switched
// on receiver SYNTAX, SKIPPED the shape it could not name, and a generic
// receiver walked straight past it, with `go build`, `go vet`,
// `golangci-lint` and the whole suite green while ember dialled every node it
// rented with no host key verification. Whoever writes the next guard here
// should know which of the two approaches survived contact: one question of
// the type, never a switch over the spellings somebody thought of.
//
// ITS OWN BOUND, because the subject is narrower than the sentence it
// defends: this asks what methods are DECLARED ON `Config`, and
// `NumMethods()` is exactly that -- declared, not promoted. Two measured
// greens follow from it, neither of them a counterexample to "wrote to by any
// statement", since a method call is not a statement:
//
//   - `Config` EMBEDDING a type that has a pointer-receiver method. Measured
//     by embedding one: this test stays green, because the promoted method is
//     in `Config`'s method SET and not in its declared list.
//   - a pointer-receiver method on a FIELD's type, called as
//     `cfg.SD.ZeroField()`. Measured by adding one to `SDConfig`: green, for
//     the same reason.
//
// AND NEITHER CAN REACH THIS ISSUE'S SUBJECT, which is what makes the bound
// acceptable rather than merely stated. `MaxDayUSD` and `MaxMonthUSD` are
// DIRECT fields of `Config`: a promoted method can only mutate the embedded
// struct, and a field-type method can only mutate `cfg.SD`. `Config` embeds
// nothing today, measured.
//
// So the residue this guard leaves rests on **the ceilings being direct fields
// of `Config`** -- a far more local and stable property than the one it
// replaced, which was "nobody adds a pointer-receiver method to a
// forty-field config struct". That narrowing is the real thing this guard
// bought, and it is written here so the next reader does not have to
// rediscover it by attacking the guard.
func TestConfig_DeclaresNoPointerReceiverMethodOnConfig(t *testing.T) {
	pkgs, err := packages.Load(&packages.Config{
		Mode: packages.NeedName | packages.NeedTypes,
	}, "github.com/Legion112/ember/internal/config")
	if err != nil {
		t.Fatalf("load internal/config: %v", err)
	}
	if n := packages.PrintErrors(pkgs); n > 0 {
		t.Fatalf("internal/config did not load cleanly (%d error(s))", n)
	}
	if len(pkgs) != 1 {
		t.Fatalf("loaded %d package(s) for internal/config, want 1", len(pkgs))
	}

	obj := pkgs[0].Types.Scope().Lookup("Config")
	if obj == nil {
		t.Fatal("internal/config declares no Config, so this guard asserted nothing")
	}
	named, ok := obj.Type().(*types.Named)
	if !ok {
		t.Fatalf("internal/config.Config is a %T rather than a named type", obj.Type())
	}

	// NumMethods is the methods DECLARED on this type in this package, which
	// is the set that matters: a method somewhere else cannot have a receiver
	// of this type.
	if named.NumMethods() == 0 {
		t.Fatal("found no methods on internal/config.Config, so this guard asserted nothing: " +
			"Config has value-receiver methods, and finding none means the load resolved nothing")
	}
	for i := range named.NumMethods() {
		m := named.Method(i)
		sig, ok := m.Type().(*types.Signature)
		if !ok || sig.Recv() == nil {
			continue
		}
		if _, ptr := sig.Recv().Type().(*types.Pointer); !ptr {
			continue
		}
		t.Errorf("internal/config.Config has a pointer-receiver method %s, which can write to a "+
			"config without an assignment or an & anywhere in the caller -- so "+
			"TestServe_WiresTheDaemonsOwnConfigIntoTheSubmittedRunGate can no longer see every "+
			"write and a daemon could be wired with zeroed ceilings (#165). Either make the "+
			"receiver a value, or teach that guard to record a write when a pointer-receiver "+
			"method is called on a tracked root", m.Name())
	}
}

// isDirectRunner reports whether a literal builds a queue.DirectRunner.
//
// ON THE SPELLING, where isConfigConfig below goes through the type checker,
// and the asymmetry is safe rather than an oversight: this one decides which
// literals to LOOK AT, so a miss lands on the `found == 0` floor and fails,
// while isConfigConfig decides what to ACCEPT, where a miss would pass. An
// import alias -- `q "…/internal/queue"` -- is therefore a red at the floor
// and not a hole, which is the direction a selector match has to fail in to be
// allowed to stay one.
func isDirectRunner(lit *ast.CompositeLit) bool {
	sel, ok := lit.Type.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "DirectRunner" {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	return ok && pkg.Name == "queue"
}

// configBindings is what one function did with the daemon's config: which
// objects are one, and which of those stopped being trustworthy.
type configBindings struct {
	// bound is every object the function bound to a config.Config, by identity
	// and not by name.
	bound map[types.Object]bool

	// moved is every object the function WROTE TO rather than merely read. An
	// object in here is not accepted even if it is bound, because identity
	// says which variable it is and says nothing about what is in it -- so the
	// honest answer to a variable that was written to is a refusal rather than
	// a guess.
	//
	// THE RULE IS "WROTE TO BY ANY STATEMENT", AND THAT TOOK SIX ATTEMPTS. Go
	// has exactly three statement nodes that write to an existing variable --
	// `*ast.AssignStmt`, `*ast.RangeStmt` with `token.ASSIGN`, and
	// `*ast.IncDecStmt` -- plus `&x`, which hands a writer to something this
	// guard cannot see into. All four are recorded, each through `rootIdent`.
	//
	// WHAT THE CLAUSE EXCLUDES IS AN EXPRESSION, and the fifth wrong wording
	// was leaving it off: a pointer-receiver method takes the address of its
	// receiver implicitly, so `cfg.ZeroCeilings()` writes with no `&` and no
	// assignment anywhere in the AST. Vacuous today and asserted rather than
	// assumed -- see TestConfig_DeclaresNoPointerReceiverMethodOnConfig. Every
	// earlier wording was a bound somebody had to be trusted to have stated
	// correctly, and five of the six were wrong: see the history on the test
	// above.
	//
	// THE FIELD CASE WAS THE FOURTH WALKAROUND AND IT IS THE MOST PLAUSIBLE OF
	// THEM ALL. The first version of this looked only for an `*ast.Ident` on
	// the left, so a `SelectorExpr` was skipped and `cfg.MaxDayUSD = ""` left
	// the object `bound` and never `moved` -- measured green across the whole
	// tree, with `CeilingsOf(r.Config)` getting two empty strings and
	// `Tightest` composing a zero, which is #165 reinstated by two lines that
	// read as normalisation. No shadow, no closure, no reassignment of the
	// variable: the least contrived of the four and the only one that zeroes
	// exactly the two fields that matter. So the left-hand side is reduced to
	// its ROOT identifier, which is the variable being written to however many
	// selectors, indices or dereferences stand between.
	moved map[types.Object]string
}

// configObjects reads what fn bound to a config.Config, and what it then did
// to those bindings.
//
// Two bindings are accepted, which are the two this package uses: a parameter
// of that type -- `serveUntil`'s `cfg`, and any closure's, because accepting a
// top-level helper and refusing the closure form would be one rule answering
// two ways -- and a `:=` whose right-hand side is `config.Load`, which is
// `runUp`'s and `runStoreSync`'s shape and where a DirectRunner could
// plausibly move.
//
// The type is compared through the type checker rather than by matching the
// spelling `config.Config`, so a dot-import or an alias cannot slip past.
func configObjects(info *types.Info, fn *ast.FuncDecl) configBindings {
	c := configBindings{bound: map[types.Object]bool{}, moved: map[types.Object]string{}}

	params := func(fl *ast.FieldList) {
		if fl == nil {
			return
		}
		for _, p := range fl.List {
			if !isConfigConfig(info.TypeOf(p.Type)) {
				continue
			}
			for _, n := range p.Names {
				if obj := info.Defs[n]; obj != nil {
					c.bound[obj] = true
				}
			}
		}
	}

	// wrote records whatever variable this expression is a write TARGET for,
	// through rootIdent, so the target does not have to be the bare variable.
	//
	// ONE FUNCTION BECAUSE THERE ARE THREE STATEMENT NODES THAT WRITE, which
	// is the journal's "one name for a path" rule met in an AST walk: the
	// first version handled `*ast.AssignStmt` alone and `*ast.RangeStmt` and
	// `*ast.IncDecStmt` reached none of its logic, so
	// `for _, cfg.MaxDayUSD = range []string{""} {}` zeroed a ceiling and the
	// guard was green. Three call sites that each have to remember to reduce
	// their own left-hand side is one place for the next one to be forgotten.
	wrote := func(e ast.Expr, how string) {
		id := rootIdent(e)
		if id == nil {
			return
		}
		obj := info.Uses[id]
		if obj == nil {
			return
		}
		if _, bare := e.(*ast.Ident); bare {
			c.moved[obj] = how
			return
		}
		c.moved[obj] = "something inside it " + how
	}

	params(fn.Type.Params)
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		switch v := n.(type) {
		case *ast.FuncLit:
			params(v.Type.Params)
		case *ast.UnaryExpr:
			// &cfg hands a writer to something this guard cannot see into.
			if v.Op == token.AND {
				if id, ok := v.X.(*ast.Ident); ok {
					if obj := info.Uses[id]; obj != nil {
						c.moved[obj] = "its address is taken"
					}
				}
			}
		case *ast.AssignStmt:
			if v.Tok == token.DEFINE {
				if len(v.Lhs) > 0 && len(v.Rhs) == 1 && isConfigLoad(v.Rhs[0]) {
					if id, ok := v.Lhs[0].(*ast.Ident); ok {
						if obj := info.Defs[id]; obj != nil {
							c.bound[obj] = true
						}
					}
				}
				return true
			}
			// Anything that is not a definition WRITES to what is already
			// there, which is the one walkaround identity cannot see.
			for _, l := range v.Lhs {
				wrote(l, "is assigned to")
			}
		case *ast.RangeStmt:
			// A RANGE CLAUSE ASSIGNS, and only when it is not a definition.
			// `for _, x := range …` is token.DEFINE and binds a NEW object,
			// which must stay acceptable or a legitimate loop would be refused
			// -- and which the bound check already handles, since a new object
			// is not one this function bound to a config. `for _, cfg = range`
			// and `for _, cfg.MaxDayUSD = range` are token.ASSIGN and write to
			// something that already exists.
			if v.Tok == token.ASSIGN {
				if v.Key != nil {
					wrote(v.Key, "is assigned to by a range clause")
				}
				if v.Value != nil {
					wrote(v.Value, "is assigned to by a range clause")
				}
			}
		case *ast.IncDecStmt:
			// `cfg.MinVRAMGiB++` is a write and is not an AssignStmt. It
			// cannot zero either ceiling, because both are strings -- so this
			// arm exists for the RULE rather than for an exploit, which is the
			// whole reason it is code: the sentence "an object the function
			// wrote to is no longer accepted" is then true with no clause
			// narrowing it, and this guard's prose has over-claimed four times.
			wrote(v.X, "is incremented or decremented")
		}
		return true
	})
	return c
}

// rootIdent is the variable an assignment target names, with whatever
// selectors, indices, dereferences and parentheses stand in front of it peeled
// off.
//
// WHY A WALK AND NOT A TYPE SWITCH ON ONE LEVEL: `cfg.MaxDayUSD`, `cfg.SD.Serve`,
// `cfgs[0].MaxDayUSD` and `(*p).MaxDayUSD` are all writes to something, and the
// thing they are writes to is whatever is leftmost.
//
// AND THE SET IS EXHAUSTIVE RATHER THAN MERELY SAFE, which is worth stating
// because the first wording of this comment claimed only the weaker thing.
// Go's assignable expression forms are exactly `Ident`, `SelectorExpr`,
// `IndexExpr`, `IndexListExpr`, `StarExpr` and `ParenExpr` -- the six below --
// so there is no assignment target this cannot reduce. The nil return is
// therefore unreachable from a real target rather than a tolerated gap, and it
// is kept because an AST this guard is handed is not a thing it should trust to
// be well-formed: an unrecognised target records nothing, which is safe in the
// one direction that matters, since acceptance at the construction site needs
// the value there to BE a bound object.
//
// TWO OF THE SIX ARMS ARE DEFENSIVE AND CANNOT FIRE FOR THIS GUARD'S SUBJECT,
// said rather than left to be mistaken for live code. A tracked object is a
// `config.Config` VALUE, a struct is not indexable, so `cfg[…] = x` is not
// legal Go and the `IndexExpr` and `IndexListExpr` arms are unreachable from a
// write to one. They are here because this function is about assignment
// targets in general and exhaustiveness is the property claimed above -- not
// because anything measured has taken them. The walkaround that looked like it
// did is W19, and it does not: see the note on it in `AGENTS.md`.
func rootIdent(e ast.Expr) *ast.Ident {
	for {
		switch v := e.(type) {
		case *ast.Ident:
			return v
		case *ast.SelectorExpr:
			e = v.X
		case *ast.IndexExpr:
			e = v.X
		case *ast.IndexListExpr:
			e = v.X
		case *ast.StarExpr:
			e = v.X
		case *ast.ParenExpr:
			e = v.X
		default:
			return nil
		}
	}
}

// isConfigConfig reports whether t is internal/config.Config.
func isConfigConfig(t types.Type) bool {
	named, ok := t.(*types.Named)
	if !ok {
		return false
	}
	obj := named.Obj()
	return obj != nil && obj.Name() == "Config" &&
		obj.Pkg() != nil && obj.Pkg().Path() == "github.com/Legion112/ember/internal/config"
}

// isConfigLoad reports whether e is a call to config.Load.
func isConfigLoad(e ast.Expr) bool {
	call, ok := e.(*ast.CallExpr)
	if !ok {
		return false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "Load" {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	return ok && pkg.Name == "config"
}

// checkDirectRunnerConfig is the per-literal half, so the walk above reads as
// the walk and this reads as the rule.
func checkDirectRunnerConfig(
	t *testing.T,
	fset *token.FileSet,
	info *types.Info,
	lit *ast.CompositeLit,
	c configBindings,
) {
	t.Helper()
	for _, el := range lit.Elts {
		kv, ok := el.(*ast.KeyValueExpr)
		if !ok {
			t.Fatalf("%s: a queue.DirectRunner is built with a positional field this guard "+
				"cannot read, so it cannot say whether the daemon's config reaches the "+
				"period ceiling: name the fields",
				fset.Position(el.Pos()))
		}
		key, ok := kv.Key.(*ast.Ident)
		if !ok || key.Name != "Config" {
			continue
		}
		val, ok := kv.Value.(*ast.Ident)
		if !ok {
			t.Errorf("%s: Config is set from a %T rather than from a config.Config this "+
				"function bound, so the daemon's own ceilings may reach the submitted-run "+
				"gate nowhere -- which is #165. Pass the config ember serve read",
				fset.Position(kv.Value.Pos()), kv.Value)
			return
		}
		obj := info.Uses[val]
		if obj == nil {
			t.Fatalf("%s: the type checker has no object for %q, which this guard cannot read "+
				"past", fset.Position(val.Pos()), val.Name)
		}
		if why, bad := c.moved[obj]; bad {
			t.Errorf("%s: Config is set from %q and %s in the same function, so this guard "+
				"cannot say it still holds the file ember serve read -- which is #165. Pass "+
				"the config through without writing to it",
				fset.Position(val.Pos()), val.Name, why)
			return
		}
		if !c.bound[obj] {
			t.Errorf("%s: Config is set from %q, which is not an object this function bound to "+
				"a config.Config -- a config.Config parameter, or a := from config.Load. A "+
				"SHADOW of the right name is a different variable and lands here. So this may "+
				"not be the file ember serve read, and max_day_usd would go back to bounding "+
				"the queue alone (#165)",
				fset.Position(val.Pos()), val.Name)
		}
		return
	}
	t.Errorf("%s: this queue.DirectRunner does not set Config, so a run submitted over "+
		"the socket is bounded by the config it named and not by the one ember serve "+
		"read -- which is #165, and max_day_usd goes back to bounding the queue alone",
		fset.Position(lit.Pos()))
}

// TestStoreSync_ProjectsTheCheapMachineAgainstThePeriodCeilings.
//
// `ember store sync` rents too -- the cheapest machine with no card, for as
// long as fifty-six gigabytes take to arrive, which `AGENTS.md` measures at
// $0.06/hour for eighty-one minutes -- and it reached no ceiling either.
// `ember serve`'s own store fill has had both gates since #142; this is the
// command an operator types for the same work.
//
// `cpuOnlyRunPod` prices the fill at $0.03 per vCPU with a minimum of two, so
// the projection over the config's ten minutes is a cent. The ceiling here is
// a tenth of that, which is red for a missing gate and for a projection of
// zero, and the acceptance arm beside it is what stops a gate that refuses
// every sync.
// THE ACCEPTANCE ARM HAS A POSITIVE WITNESS, which it did not: it asserted
// only that the refusal was not the ceiling's, which is also what a sync that
// stopped somewhere else entirely would say. The row `boundAndRecord` adopts
// after the gate is the same witness the `up` arms use, and it is read through
// `recorded` -- the spelling `TestStoreSync_RecordsTheRentalItMakes` already
// uses. The first version of this opened the store under the config's own
// `state_dir` instead and found nothing, which is the journal's
// "one name for a path, not one per call site" rule met in a test: a second
// spelling is how a test ends up asserting against a directory nothing wrote
// to, and here it did.
func TestStoreSync_ProjectsTheCheapMachineAgainstThePeriodCeilings(t *testing.T) {
	run := func(t *testing.T, ceiling string) (deps, string, error) {
		t.Helper()
		d := labDeps(t)
		d.CloudBaseURL = cpuOnlyRunPod(t)
		d.Creds = &config.Credentials{RunPod: node.NewSecret([]byte("rp-token"))}
		cfg := storeConfigFor(t, d, "runpod", storeBlock+"\n"+assetBlock+"\n"+ceiling)

		var out bytes.Buffer
		err := runStore([]string{"sync", "-config", cfg}, &out, d)
		return d, out.String(), err
	}

	d, _, err := run(t, "max_day_usd: \"0.001\"\n")
	if err == nil {
		t.Fatal("a store sync projected past the day's ceiling was run")
	}
	for _, want := range []string{"max_day_usd", "ceiling of $0.001,"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not mention %q, so it is not the period ceiling's: %v", want, err)
		}
	}
	if rows := recorded(t, d); len(rows) != 0 {
		t.Errorf("a refused sync wrote %d row(s): the ceiling is checked before Adopt, so a row "+
			"means the ledger was told a rental was coming when none was", len(rows))
	}

	// The negative control. The fake serves no `/v2/pods`, so a sync that got
	// past the gate stops at the create call -- which is the point: what has
	// to be proved is that the ceiling let it through.
	d, out, err := run(t, "max_day_usd: \"20\"\n")
	if err == nil {
		t.Fatalf("the sync rented against a fake with no pods route:\n%s", out)
	}
	if strings.Contains(err.Error(), "max_day_usd") {
		t.Fatalf("a sync well inside its ceiling was refused, so the gate refuses every sync "+
			"there is: %v", err)
	}
	// Positively: it got past the gate and the rental was adopted.
	if rows := recorded(t, d); len(rows) != 1 {
		t.Errorf("the history holds %d row(s), want the sync the ceiling allowed", len(rows))
	}
}
