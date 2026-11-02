package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Legion112/ember/internal/catalogue"
	"github.com/Legion112/ember/internal/config"
	"github.com/Legion112/ember/internal/node"
	"github.com/Legion112/ember/internal/provider/local"
)

// fakeCatalogue is a RunPod-shaped catalogue endpoint.
//
// Through CloudBaseURL, which exists so a test that registers a cloud
// provider cannot reach it: left to resolve itself this would read the
// developer's own RUNPOD_API_KEY and make live calls, behaving differently
// from one machine to the next.
//
// The GPU body is the shape the captured fixture has and the shape the whole
// feature is about: a card reading NONE overall with one site reading LOW.
type fakeCatalogue struct {
	gpus string
	cpus string

	// failGPUs and failCPUs make that half of the catalogue answer 500.
	failGPUs bool
	failCPUs bool

	paths []string
}

const gpuBody = `{"gpus":[
 {"id":"NVIDIA GeForce RTX 4090","name":"RTX 4090","memory":24,
  "secure":true,"community":true,
  "price":{"secure":0.89,"community":0.34},
  "maxCount":{"secure":8,"community":8},
  "availability":"NONE",
  "dataCenters":[{"id":"US-IL-1","name":"US-IL-1","availability":"LOW"},
                 {"id":"EU-RO-1","name":"EU-RO-1","availability":"HIGH"}]},
 {"id":"AMD Instinct MI300X OAM","name":"MI300X","memory":192,
  "secure":true,"community":false,
  "price":{"secure":2.39,"community":0.5},
  "maxCount":{"secure":8,"community":0},
  "availability":"NONE"}
]}`

const cpuBody = `{"cpus":[
 {"id":"cpu3c","name":"Compute-Optimized","group":"cpu3",
  "vcpu":{"min":2,"max":32},"ramGbPerVcpu":2,
  "price":{"securePerVcpu":0.03,"serverlessPerVcpu":0.036},
  "availability":"HIGH",
  "dataCenters":[{"id":"EU-RO-1","name":"EU-RO-1","availability":"HIGH"}]}
]}`

func (f *fakeCatalogue) deps(t *testing.T) (deps, string) {
	t.Helper()
	if f.gpus == "" {
		f.gpus = gpuBody
	}
	if f.cpus == "" {
		f.cpus = cpuBody
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		f.paths = append(f.paths, req.URL.RequestURI())
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(req.URL.Path, "catalog/cpus"):
			if f.failCPUs {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			_, _ = fmt.Fprint(w, f.cpus)
		case strings.Contains(req.URL.Path, "catalog/gpus"):
			if f.failGPUs {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			_, _ = fmt.Fprint(w, f.gpus)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)

	d := labDeps(t)
	d.CloudBaseURL = srv.URL
	d.Creds = &config.Credentials{RunPod: node.NewSecret([]byte("rp-token"))}
	return d, catalogueConfig(t)
}

func catalogueConfig(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, config.FileName)
	body := "provider: runpod\nimage: " + local.DefaultImage + "\nttl: 10m\n"
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// atHour pins the clock, because the instant decides both what is recorded
// and which day file it lands in.
func atHour(d deps, day, hour int) deps {
	d.Now = func() time.Time { return time.Date(2026, 11, day, hour, 0, 0, 0, time.UTC) }
	return d
}

func seriesDir(d deps) string { return filepath.Join(d.HomeDir, "state", catalogue.DirName) }

func TestCatalogueSnapshot_WritesOneRowPerSite(t *testing.T) {
	f := &fakeCatalogue{}
	d, cfg := f.deps(t)
	d = atHour(d, 2, 10)

	var out bytes.Buffer
	if err := runCatalogue([]string{"snapshot", "-config", cfg}, &out, d); err != nil {
		t.Fatalf("snapshot: %v\n%s", err, out.String())
	}

	raw, err := os.ReadFile(filepath.Join(seriesDir(d), "2026-11-02.jsonl"))
	if err != nil {
		t.Fatalf("no day file: %v", err)
	}
	lines := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	// Two 4090 sites, one MI300X with no site, one CPU site.
	if len(lines) != 4 {
		t.Fatalf("wrote %d line(s):\n%s", len(lines), raw)
	}

	sites := map[string]catalogue.Sample{}
	for _, line := range lines {
		var s catalogue.Sample
		if err := json.Unmarshal([]byte(line), &s); err != nil {
			t.Fatalf("line does not decode: %v\n%s", err, line)
		}
		sites[s.ID+"@"+s.Datacentre] = s
	}
	il, ok := sites["NVIDIA GeForce RTX 4090@US-IL-1"]
	if !ok {
		t.Fatalf("no US-IL-1 row: %s", raw)
	}
	if il.Availability != catalogue.LevelLow || il.Overall != catalogue.LevelNone {
		t.Errorf("US-IL-1 = %q with summary %q, want LOW with NONE", il.Availability, il.Overall)
	}
	if !il.ObservedAt.Equal(time.Date(2026, 11, 2, 10, 0, 0, 0, time.UTC)) {
		t.Errorf("observed_at = %s", il.ObservedAt)
	}
	if _, ok := sites["cpu3c@EU-RO-1"]; !ok {
		t.Errorf("the cpu catalogue was not collected: %s", raw)
	}

	// Said on the first run and only then, because that is the one moment an
	// operator is reading this output and a collector nobody scheduled
	// collects nothing.
	if !strings.Contains(out.String(), "first snapshot") {
		t.Errorf("the first run does not say it has to be scheduled:\n%s", out.String())
	}

	var again bytes.Buffer
	if err := runCatalogue([]string{"snapshot", "-config", cfg}, &again, atHour(d, 2, 11)); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(again.String(), "first snapshot") {
		t.Errorf("the second run repeats the scheduling notice:\n%s", again.String())
	}
	raw2, err := os.ReadFile(filepath.Join(seriesDir(d), "2026-11-02.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	// Appended, never rewritten: the value is entirely in the series.
	if n := strings.Count(strings.TrimRight(string(raw2), "\n"), "\n") + 1; n != 8 {
		t.Errorf("after two snapshots the file holds %d line(s), want 8", n)
	}
}

// TestCatalogueSnapshot_APartialReadIsStillRecorded. The GPU rows are an hour
// of the series that cannot be taken again, so they are written and the
// failure is a finding -- exit 2, as an unreachable provider is for `ember
// ls`, because here the gap it leaves is permanent.
func TestCatalogueSnapshot_APartialReadIsStillRecorded(t *testing.T) {
	f := &fakeCatalogue{failCPUs: true}
	d, cfg := f.deps(t)
	d = atHour(d, 2, 10)

	var out bytes.Buffer
	err := runCatalogue([]string{"snapshot", "-config", cfg}, &out, d)
	if err == nil {
		t.Fatal("a failing half of the catalogue was not reported")
	}
	if ExitCode(err) != ExitFindings {
		t.Errorf("exit %d, want %d for a provider ember could not fully ask", ExitCode(err), ExitFindings)
	}
	if !strings.Contains(out.String(), "could not be sampled") {
		t.Errorf("the output does not name the failure:\n%s", out.String())
	}
	raw, err := os.ReadFile(filepath.Join(seriesDir(d), "2026-11-02.jsonl"))
	if err != nil {
		t.Fatalf("the gpu rows that did arrive were not recorded: %v", err)
	}
	if n := strings.Count(string(raw), "\n"); n != 3 {
		t.Errorf("recorded %d row(s), want the 3 gpu rows", n)
	}
}

func TestCatalogueSnapshot_AFailedGPUReadRecordsNothing(t *testing.T) {
	f := &fakeCatalogue{failGPUs: true}
	d, cfg := f.deps(t)
	d = atHour(d, 2, 10)

	var out bytes.Buffer
	err := runCatalogue([]string{"snapshot", "-config", cfg}, &out, d)
	if err == nil {
		t.Fatal("a failing gpu catalogue was not reported")
	}
	if _, statErr := os.Stat(seriesDir(d)); !os.IsNotExist(statErr) {
		t.Error("a day file was written for a snapshot that collected nothing")
	}
}

// TestCatalogueSnapshot_WithNoCapableProviderIsAUsageError. A timer that
// silently collects nothing every hour is the failure this whole feature is
// about, so the absence is said out loud and names the fix.
func TestCatalogueSnapshot_WithNoCapableProviderIsAUsageError(t *testing.T) {
	d := labDeps(t) // local only: no credential, so no cloud provider
	cfg := catalogueConfig(t)

	var out bytes.Buffer
	err := runCatalogue([]string{"snapshot", "-config", cfg}, &out, d)
	if err == nil {
		t.Fatal("a snapshot with nothing to sample succeeded")
	}
	if ExitCode(err) != ExitUsage {
		t.Errorf("exit %d, want %d", ExitCode(err), ExitUsage)
	}
	if !strings.Contains(err.Error(), "RUNPOD_API_KEY") {
		t.Errorf("the error should name the fix, got %v", err)
	}
}

func TestCatalogueSnapshot_AnEmptyCatalogueIsNotSuccess(t *testing.T) {
	f := &fakeCatalogue{gpus: `{"gpus":[]}`, cpus: `{"cpus":[]}`}
	d, cfg := f.deps(t)
	d = atHour(d, 2, 10)

	var out bytes.Buffer
	err := runCatalogue([]string{"snapshot", "-config", cfg}, &out, d)
	if err == nil {
		t.Fatal("a provider with nothing in its catalogue reported success")
	}
	if !strings.Contains(err.Error(), "empty") {
		t.Errorf("error = %v", err)
	}
}

// snapshots takes n hourly samples and returns the deps they were written
// with, so a report test has a series rather than a point.
func snapshots(t *testing.T, f *fakeCatalogue, hours ...int) (deps, string) {
	t.Helper()
	d, cfg := f.deps(t)
	for _, h := range hours {
		var out bytes.Buffer
		if err := runCatalogue([]string{"snapshot", "-config", cfg}, &out, atHour(d, 2, h)); err != nil {
			t.Fatalf("snapshot at %d: %v\n%s", h, err, out.String())
		}
	}
	return d, cfg
}

func TestCatalogueReport_SaysWhatTheSeriesShows(t *testing.T) {
	d, cfg := snapshots(t, &fakeCatalogue{}, 10, 11, 12)

	var out bytes.Buffer
	if err := runCatalogue([]string{"report", "-config", cfg}, &out, d); err != nil {
		t.Fatalf("report: %v\n%s", err, out.String())
	}
	got := out.String()

	for _, want := range []string{
		"NVIDIA GeForce RTX 4090",
		"US-IL-1",
		"EU-RO-1",
		// The masked column: three samples where the summary said NONE while
		// the site had capacity. This is the whole reason the summary is
		// stored, and the number is what a summary-only collector would have
		// got wrong.
		"masked",
		"$0.89/hr",
		"(no site)*",
		"cpu3c",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the report does not mention %q:\n%s", want, got)
		}
	}
	// Three samples per site, and the series is short enough that the report
	// says so rather than letting three hours read as a history.
	if !strings.Contains(got, "point sample") {
		t.Errorf("a two-hour series does not warn that it is not a history:\n%s", got)
	}
}

func TestCatalogueReport_MaskedCountsTheSummaryDisagreement(t *testing.T) {
	d, cfg := snapshots(t, &fakeCatalogue{}, 10, 11)

	var out bytes.Buffer
	if err := runCatalogue([]string{"report", "-config", cfg, "-json"}, &out, d); err != nil {
		t.Fatal(err)
	}
	var rows []catalogue.Row
	if err := json.Unmarshal(out.Bytes(), &rows); err != nil {
		t.Fatalf("--json does not decode: %v\n%s", err, out.String())
	}
	var il catalogue.Row
	for _, r := range rows {
		if r.Datacentre == "US-IL-1" {
			il = r
		}
	}
	if il.Samples != 2 || il.Low != 2 {
		t.Fatalf("US-IL-1 row = %+v", il)
	}
	if il.Masked != 2 {
		t.Errorf("masked = %d, want 2: the summary said NONE both times while the site had capacity", il.Masked)
	}
	if il.Secure.Min != node.Rate(890_000) || il.Secure.Max != node.Rate(890_000) {
		t.Errorf("secure prices = %v..%v, want exactly 890000 micro", il.Secure.Min, il.Secure.Max)
	}
	if share, ok := il.Share(); !ok || share != 1 {
		t.Errorf("share = %.2f (%v)", share, ok)
	}
}

// TestCatalogueReport_ASiteThatReportedNothingIsNotZeroPerCent. A level the
// provider did not give is not a provider saying there is nothing, and 0% in
// that column would send an operator to move a store over an absent field.
func TestCatalogueReport_ASiteThatReportedNothingIsNotZeroPerCent(t *testing.T) {
	silent := `{"gpus":[
 {"id":"NVIDIA L40S","name":"L40S","memory":48,"secure":true,"community":false,
  "price":{"secure":1.03,"community":0},"maxCount":{"secure":8,"community":0},
  "availability":"LOW",
  "dataCenters":[{"id":"US-GA-1","name":"US-GA-1"}]}
]}`
	d, cfg := snapshots(t, &fakeCatalogue{gpus: silent, cpus: `{"cpus":[]}`}, 10, 11)

	var out bytes.Buffer
	if err := runCatalogue([]string{"report", "-config", cfg}, &out, d); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if !strings.Contains(got, "US-GA-1") {
		t.Fatalf("the site is missing:\n%s", got)
	}
	// Two samples, both unreported: the share is a dash and the samples are
	// counted under unseen rather than under none.
	line := ""
	for _, l := range strings.Split(got, "\n") {
		if strings.Contains(l, "US-GA-1") {
			line = l
		}
	}
	if strings.Contains(line, "0%") {
		t.Errorf("a site that reported no level reads as never available:\n%s", line)
	}
	fields := strings.Fields(line)
	// site samples rentable high med low none unseen masked ...
	if len(fields) < 8 || fields[2] != "-" || fields[7] != "2" {
		t.Errorf("row = %q, want a dash for the share and 2 under unseen", line)
	}
}

func TestCatalogueReport_FiltersByCardAndKindAndPeriod(t *testing.T) {
	d, cfg := snapshots(t, &fakeCatalogue{}, 10)

	// --gpu matches as `ember up --gpu` does, so "rtx4090" finds "NVIDIA
	// GeForce RTX 4090" and leaves the MI300X and the CPU family out.
	var out bytes.Buffer
	if err := runCatalogue([]string{"report", "-config", cfg, "-gpu", "rtx4090"}, &out, d); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "MI300X") || strings.Contains(out.String(), "cpu3c") {
		t.Errorf("--gpu rtx4090 kept other types:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "US-IL-1") {
		t.Errorf("--gpu rtx4090 dropped the card it names:\n%s", out.String())
	}

	out.Reset()
	if err := runCatalogue([]string{"report", "-config", cfg, "-kind", "cpu"}, &out, d); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "4090") || !strings.Contains(out.String(), "cpu3c") {
		t.Errorf("--kind cpu kept the wrong rows:\n%s", out.String())
	}

	// A period the samples are not in reports nothing, and says which period
	// it looked at -- "no samples recorded" must not read as "ember has
	// never collected anything".
	out.Reset()
	if err := runCatalogue([]string{"report", "-config", cfg, "-since", "2026-11-05"}, &out, d); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "since 2026-11-05") {
		t.Errorf("the empty answer does not name the period:\n%s", out.String())
	}
}

func TestCatalogueReport_RefusesAnImpossiblePeriodAndAnInventedKind(t *testing.T) {
	d, cfg := snapshots(t, &fakeCatalogue{}, 10)

	for _, args := range [][]string{
		{"report", "-config", cfg, "-since", "2026-11-05", "-until", "2026-11-01"},
		{"report", "-config", cfg, "-kind", "tpu"},
		{"report", "-config", cfg, "-since", "yesterday"},
		{"report", "-config", cfg, "extra"},
		{"snapshot", "-config", cfg, "extra"},
		{"nonsense"},
		{},
	} {
		var out bytes.Buffer
		err := runCatalogue(args, &out, d)
		if err == nil {
			t.Errorf("%v was accepted", args)
			continue
		}
		if ExitCode(err) != ExitUsage {
			t.Errorf("%v exited %d, want %d", args, ExitCode(err), ExitUsage)
		}
	}
}

// TestCatalogueReport_ALineItCannotPlaceIsAFinding. A sample that is in the
// file and not in the total is exactly the clean-bill-of-health failure
// `ember ls --all` exits non-zero to avoid, over a file nothing can rebuild.
func TestCatalogueReport_ALineItCannotPlaceIsAFinding(t *testing.T) {
	d, cfg := snapshots(t, &fakeCatalogue{}, 10)

	day := filepath.Join(seriesDir(d), "2026-11-02.jsonl")
	f, err := os.OpenFile(day, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"observed_at":"2026-11-02T1`); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(seriesDir(d), "2026-11-01.jsonl.gz"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	err = runCatalogue([]string{"report", "-config", cfg}, &out, d)
	if ExitCode(err) != ExitFindings {
		t.Fatalf("exit %d, want %d: %v\n%s", ExitCode(err), ExitFindings, err, out.String())
	}
	if !strings.Contains(out.String(), "could not be placed") {
		t.Errorf("the report does not say a line was left out:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "2026-11-01.jsonl.gz") {
		t.Errorf("the report does not name the file it did not read:\n%s", out.String())
	}
	// And the good rows are still there: refusing to read the series over one
	// broken line is the loss the append-only format exists to prevent.
	if !strings.Contains(out.String(), "US-IL-1") {
		t.Errorf("a truncated line hid the rest of the series:\n%s", out.String())
	}

	// With --json the finding travels as the error instead, because a note
	// printed after the array would be inside the thing a caller is parsing.
	out.Reset()
	err = runCatalogue([]string{"report", "-config", cfg, "-json"}, &out, d)
	if ExitCode(err) != ExitFindings {
		t.Fatalf("--json exited %d, want %d", ExitCode(err), ExitFindings)
	}
	var rows []catalogue.Row
	if jsonErr := json.Unmarshal(out.Bytes(), &rows); jsonErr != nil {
		t.Errorf("--json over a damaged series is not parseable: %v\n%s", jsonErr, out.String())
	}
	if !strings.Contains(err.Error(), "not wholly readable") {
		t.Errorf("the error does not say what was left out: %v", err)
	}
}

func TestCatalogueReport_WithNothingCollectedSaysWhereToStart(t *testing.T) {
	d := labDeps(t)
	cfg := catalogueConfig(t)

	var out bytes.Buffer
	if err := runCatalogue([]string{"report", "-config", cfg}, &out, d); err != nil {
		t.Fatalf("report on an empty series: %v", err)
	}
	if !strings.Contains(out.String(), "ember catalogue snapshot") {
		t.Errorf("the empty report does not say how to fill it:\n%s", out.String())
	}
	// --json over an empty series is an empty array and not null: null is not
	// an empty list, and a consumer that iterates fails on it.
	out.Reset()
	if err := runCatalogue([]string{"report", "-config", cfg, "-json"}, &out, d); err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(out.String()) != "[]" {
		t.Errorf("--json over an empty series = %q", out.String())
	}
}

func TestCatalogue_HelpListsBothSubcommands(t *testing.T) {
	var out bytes.Buffer
	if err := runCatalogue([]string{"help"}, &out, labDeps(t)); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"snapshot", "report", "cannot be collected later"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("the usage does not mention %q:\n%s", want, out.String())
		}
	}
}

// TestCatalogue_IsInTheDispatchTable. A subcommand that exists and is not
// dispatched is reachable only from its own test, which is a feature built and
// never connected -- and the `catalogue` verb is nine characters, which is how
// the usage column width turned out to be a literal.
func TestCatalogue_IsInTheDispatchTable(t *testing.T) {
	var out bytes.Buffer
	// Through run and not through the table, so what is asserted is what an
	// operator typing it reaches. -h, so nothing touches the filesystem.
	if err := run([]string{"catalogue", "-h"}, &out); err != nil {
		t.Fatalf("ember catalogue -h: %v", err)
	}
	if !strings.Contains(out.String(), "snapshot") {
		t.Fatalf("ember catalogue is not dispatched:\n%s", out.String())
	}

	out.Reset()
	if err := run([]string{"help"}, &out); err != nil {
		t.Fatal(err)
	}
	// The alignment, asserted on a SHORT verb. `catalogue` is nine
	// characters, so its own line reads identically whether the width is 8 or
	// taken from the longest name -- the lines that move are every other
	// one. Asserting on the long verb proved nothing, which the reinstatement
	// pass found by putting the literal back and watching the test pass.
	if !strings.Contains(out.String(), "  up        rent a node") {
		t.Errorf("the summary column does not line up with the longest verb present:\n%s", out.String())
	}
}
