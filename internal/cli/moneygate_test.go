package cli

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

// TestUp_ALedgerThatWillNotOpenRefusesONLYWhenACeilingIsConfigured.
//
// THE ONE ASYMMETRY IN boundAndRecord, asserted in both directions because
// either half alone reads as working.
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
// The ledger is broken by putting a DIRECTORY where the database file goes,
// which is a failure neither `app.ResolvePaths` nor the run itself cares
// about: only the thing that opens `generations.db` sees it.
func TestUp_ALedgerThatWillNotOpenRefusesOnlyWhenACeilingIsConfigured(t *testing.T) {
	breakLedger := func(t *testing.T, d deps) {
		t.Helper()
		if err := os.MkdirAll(filepath.Join(d.HomeDir, "state", genstore.FileName), 0o700); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("with a ceiling it refuses", func(t *testing.T) {
		d := labDeps(t)
		breakLedger(t, d)
		cfg := filepath.Join(t.TempDir(), config.FileName)
		body := "provider: local\nimage: x:v1\nttl: 5m\nmax_month_usd: \"50\"\n"
		if err := os.WriteFile(cfg, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}

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
		// Not the printed line, which is what an unbounded run gets.
		if strings.Contains(out.String(), "not recording") {
			t.Errorf("a configured ceiling declined to record and ran anyway:\n%s", out.String())
		}
	})

	t.Run("without one it says so and runs", func(t *testing.T) {
		d := labDeps(t)
		d.Engine.(*dockerengine.RecordingEngine).FailOn = "ContainerCreate"
		breakLedger(t, d)
		cfg := filepath.Join(t.TempDir(), config.FileName)
		if err := os.WriteFile(cfg, []byte("provider: local\nimage: x:v1\nttl: 5m\n"), 0o600); err != nil {
			t.Fatal(err)
		}

		var out bytes.Buffer
		err := runUp([]string{"--config", cfg}, &out, d)
		if err == nil {
			t.Fatal("the create call was supposed to fail")
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
// A PARSE AND NOT A GREP. The subject is "every production construction of
// this type names Config", which `go/parser` answers and `grep` cannot: a
// POSITIONAL literal has no field names at all and is a fatal here rather than
// a skip, because the alternative to failing on a literal this guard cannot
// read is reading straight past the one that matters --
// TestFlagTablesClassifyEveryFlagSetMethod's rule. The floor is the other
// half: a walk that found no construction passes every assertion.
func TestServe_WiresTheDaemonsOwnConfigIntoTheSubmittedRunGate(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read internal/cli: %v", err)
	}

	fset := token.NewFileSet()
	found := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, perr := parser.ParseFile(fset, name, nil, 0)
		if perr != nil {
			t.Fatalf("parse %s: %v", name, perr)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			lit, ok := n.(*ast.CompositeLit)
			if !ok {
				return true
			}
			sel, ok := lit.Type.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "DirectRunner" {
				return true
			}
			pkg, ok := sel.X.(*ast.Ident)
			if !ok || pkg.Name != "queue" {
				return true
			}
			found++
			for _, el := range lit.Elts {
				kv, ok := el.(*ast.KeyValueExpr)
				if !ok {
					t.Fatalf("%s: a queue.DirectRunner is built with a positional field this "+
						"guard cannot read, so it cannot say whether the daemon's config "+
						"reaches the period ceiling: name the fields",
						fset.Position(el.Pos()))
				}
				if id, ok := kv.Key.(*ast.Ident); ok && id.Name == "Config" {
					return true
				}
			}
			t.Errorf("%s: this queue.DirectRunner does not set Config, so a run submitted over "+
				"the socket is bounded by the config it named and not by the one ember serve "+
				"read -- which is #165, and max_day_usd goes back to bounding the queue alone",
				fset.Position(lit.Pos()))
			return true
		})
	}
	if found == 0 {
		t.Fatal("found no production queue.DirectRunner to check, so this guard asserted nothing")
	}
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
func TestStoreSync_ProjectsTheCheapMachineAgainstThePeriodCeilings(t *testing.T) {
	run := func(t *testing.T, ceiling string) (string, error) {
		t.Helper()
		d := labDeps(t)
		d.CloudBaseURL = cpuOnlyRunPod(t)
		d.Creds = &config.Credentials{RunPod: node.NewSecret([]byte("rp-token"))}
		cfg := storeConfigFor(t, d, "runpod", storeBlock+"\n"+assetBlock+"\n"+ceiling)

		var out bytes.Buffer
		err := runStore([]string{"sync", "-config", cfg}, &out, d)
		return out.String(), err
	}

	_, err := run(t, "max_day_usd: \"0.001\"\n")
	if err == nil {
		t.Fatal("a store sync projected past the day's ceiling was run")
	}
	for _, want := range []string{"max_day_usd", "ceiling of $0.001,"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not mention %q, so it is not the period ceiling's: %v", want, err)
		}
	}

	// The negative control. The fake serves no `/v2/pods`, so a sync that got
	// past the gate stops at the create call -- which is the point: what has
	// to be proved is that the ceiling let it through.
	out, err := run(t, "max_day_usd: \"20\"\n")
	if err == nil {
		t.Fatalf("the sync rented against a fake with no pods route:\n%s", out)
	}
	if strings.Contains(err.Error(), "max_day_usd") {
		t.Fatalf("a sync well inside its ceiling was refused, so the gate refuses every sync "+
			"there is: %v", err)
	}
}
