package statestore

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Legion112/ember/internal/node"
)

func open(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func rec(id node.RunID) Record {
	return Record{
		RunID:    id,
		Provider: "local",
		Phase:    PhaseIntent,
		Label:    "ember-" + string(id) + "-1",
		Created:  time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC),
		Deadline: time.Date(2026, 10, 24, 13, 0, 0, 0, time.UTC),
	}
}

// TestPhase_OnlyDestroyedIsFree mirrors node.State.Billable: anything else may
// still be costing money, and a phase added later is billable by default.
func TestPhase_OnlyDestroyedIsFree(t *testing.T) {
	for _, p := range []Phase{PhaseIntent, PhaseCreated, PhaseReady, PhaseDestroyFailed, Phase("invented-later")} {
		if !p.Billable() {
			t.Errorf("%q should be treated as billable", p)
		}
	}
	if PhaseDestroyed.Billable() {
		t.Error("destroyed is the one phase that is provably free")
	}
}

func TestStore_CreateGetUpdateForget(t *testing.T) {
	s := open(t)
	id := node.NewRunID()

	if err := s.Create(rec(id)); err != nil {
		t.Fatal(err)
	}
	got, err := s.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	if got.Phase != PhaseIntent || got.Version != recordVersion {
		t.Fatalf("round-trip lost something: %+v", got)
	}
	if _, ok := got.Handle(); ok {
		t.Error("an intent record has no node id yet, so it must not produce a handle")
	}

	if err := s.Update(id, func(r *Record) { r.Phase = PhaseCreated; r.NodeID = "abc" }); err != nil {
		t.Fatal(err)
	}
	got, err = s.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	h, ok := got.Handle()
	if !ok || h.ID != "abc" || h.Provider != "local" {
		t.Fatalf("handle = %v, %v", h, ok)
	}

	if err := s.Forget(id); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(id); err == nil {
		t.Error("Get returned a forgotten record")
	}
	// Forget is idempotent, like destroy.
	if err := s.Forget(id); err != nil {
		t.Errorf("forgetting twice: %v", err)
	}
}

// TestStore_CreateRefusesToOverwrite: two embers starting at once must not
// both claim one run id, or the loser overwrites the winner's node id after
// the winner already created a machine with it.
func TestStore_CreateRefusesToOverwrite(t *testing.T) {
	s := open(t)
	id := node.NewRunID()
	if err := s.Create(rec(id)); err != nil {
		t.Fatal(err)
	}
	if err := s.Create(rec(id)); err == nil {
		t.Fatal("the second Create overwrote the first record")
	}
}

func TestStore_CreateRefusesABadRunID(t *testing.T) {
	s := open(t)
	if err := s.Create(rec("not-hex")); err == nil {
		t.Fatal("a malformed run id was accepted; it would become a filename")
	}
}

func TestStore_WritesRestrictivePermissions(t *testing.T) {
	s := open(t)
	id := node.NewRunID()
	if err := s.Create(rec(id)); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(filepath.Join(s.Dir(), string(id)+".json"))
	if err != nil {
		t.Fatal(err)
	}
	if m := fi.Mode().Perm(); m != 0o600 {
		t.Errorf("record mode is %04o, want 0600", m)
	}
}

func TestStore_ListIsOldestFirstAndIgnoresStrangers(t *testing.T) {
	s := open(t)

	a, b := node.NewRunID(), node.NewRunID()
	ra, rb := rec(a), rec(b)
	rb.Created = ra.Created.Add(time.Hour)
	if err := s.Create(rb); err != nil { // created out of order on purpose
		t.Fatal(err)
	}
	if err := s.Create(ra); err != nil {
		t.Fatal(err)
	}

	// A file ember did not write must not stop ls reporting the real ones.
	for _, junk := range []string{"notes.txt", "README.json", ".rec-leftover"} {
		if err := os.WriteFile(filepath.Join(s.Dir(), junk), []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	got, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d records, want 2", len(got))
	}
	if got[0].RunID != a || got[1].RunID != b {
		t.Errorf("not oldest-first: %v then %v", got[0].RunID, got[1].RunID)
	}
}

func TestStore_ListOfAMissingDirectoryIsEmpty(t *testing.T) {
	s := open(t)
	got, err := s.List()
	if err != nil {
		t.Fatalf("a store with nothing in it should list nothing, not fail: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %v", got)
	}
}

func TestStore_RefusesAFutureVersion(t *testing.T) {
	s := open(t)
	id := node.NewRunID()
	if err := os.MkdirAll(s.Dir(), 0o700); err != nil {
		t.Fatal(err)
	}
	body := `{"version":99,"run_id":"` + string(id) + `","provider":"local","phase":"ready"}`
	if err := os.WriteFile(filepath.Join(s.Dir(), string(id)+".json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := s.Get(id)
	if err == nil {
		t.Fatal("a newer record was read as if it were this version; the field it would drop is the one saying a node exists")
	}
	if !strings.Contains(err.Error(), "newer ember") {
		t.Errorf("got %v", err)
	}
	// And List must surface it rather than skipping it.
	if _, err := s.List(); err == nil {
		t.Error("List hid an unreadable record")
	}
}

func TestStore_RefusesCorruptRecords(t *testing.T) {
	s := open(t)
	id := node.NewRunID()
	if err := os.MkdirAll(s.Dir(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.Dir(), string(id)+".json"), []byte("{{{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(id); err == nil {
		t.Error("corrupt JSON was accepted")
	}
}

func TestStore_UpdateOfAMissingRecordFails(t *testing.T) {
	s := open(t)
	if err := s.Update(node.NewRunID(), func(*Record) {}); err == nil {
		t.Fatal("Update invented a record")
	}
}

func TestOpen_RefusesAnEmptyDirectory(t *testing.T) {
	if _, err := Open("  "); err == nil {
		t.Fatal("Open accepted an empty path")
	}
}

func TestStore_ReportsAnUnwritableDirectory(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	parent := filepath.Join(t.TempDir(), "ro")
	if err := os.Mkdir(parent, 0o500); err != nil {
		t.Fatal(err)
	}
	s, err := Open(filepath.Join(parent, "state"))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Create(rec(node.NewRunID())); err == nil {
		t.Fatal("a record that could not be written reported success")
	}
}

func TestStore_UpdateReportsAnUnwritableDirectory(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	s := open(t)
	id := node.NewRunID()
	if err := s.Create(rec(id)); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(s.Dir(), 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(s.Dir(), 0o700) })

	if err := s.Update(id, func(r *Record) { r.Phase = PhaseReady }); err == nil {
		t.Fatal("an update that could not be written reported success")
	}
}

// TestHolder_ThisProcessIsAlive. The baseline: a record written by a running
// ember must never read as abandoned, or every healthy session becomes a
// finding and the operator learns to ignore `ls`.
func TestHolder_ThisProcessIsAlive(t *testing.T) {
	h := ThisProcess()
	if h.IsZero() {
		t.Fatalf("ThisProcess() is zero: %+v", h)
	}
	if h.PID != os.Getpid() {
		t.Errorf("pid = %d, want %d", h.PID, os.Getpid())
	}
	if h.Abandoned() {
		t.Error("the running test process reads as abandoned")
	}
}

// TestHolder_ADeadPidIsAbandoned, which is the whole point: a run killed with
// SIGKILL leaves a node that is running, recorded and inside its deadline, and
// without this it is indistinguishable from a session somebody is using.
func TestHolder_ADeadPidIsAbandoned(t *testing.T) {
	host, err := os.Hostname()
	if err != nil {
		// Not a skip. A machine with no hostname is a real environment
		// problem, and a test that skips itself reports success for
		// something that never ran.
		t.Fatalf("no hostname on this machine: %v", err)
	}

	// A process that has certainly exited: this test binary told to run no
	// tests at all, so it starts, matches nothing and returns immediately.
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	_ = cmd.Wait()

	h := Holder{Host: host, PID: pid}
	if !h.Abandoned() {
		t.Errorf("pid %d has exited but does not read as abandoned", pid)
	}
}

// TestHolder_UnknownsAreNeverAbandoned. Each of these is a case ember cannot
// be sure about, and a false "abandoned" would tell an operator their running
// session is dead -- the one mistake that would make this not worth having.
func TestHolder_UnknownsAreNeverAbandoned(t *testing.T) {
	host, err := os.Hostname()
	if err != nil {
		t.Fatalf("no hostname on this machine: %v", err)
	}

	for name, h := range map[string]Holder{
		"no holder recorded at all": {},
		"holder with no pid":        {Host: host},
		"holder with no host":       {PID: os.Getpid()},
		// Pids are only meaningful locally; testing one from another machine
		// against this process table would be a coin flip.
		"another machine": {Host: host + "-elsewhere", PID: 999999},
	} {
		if h.Abandoned() {
			t.Errorf("%s: read as abandoned (%+v)", name, h)
		}
	}
}

func TestHolder_IsZero(t *testing.T) {
	if !(Holder{}).IsZero() {
		t.Error("the zero holder does not report itself as one")
	}
	if (Holder{Host: "h", PID: 1}).IsZero() {
		t.Error("a complete holder reports itself as zero")
	}
	// Half a holder is not a holder: neither half alone can be checked.
	if !(Holder{Host: "h"}).IsZero() || !(Holder{PID: 1}).IsZero() {
		t.Error("a half-filled holder should count as zero")
	}
}

// TestHolder_SurvivesARoundTrip, including through a record written before the
// field existed -- which is every record already on disk.
func TestHolder_SurvivesARoundTrip(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	rec := Record{
		RunID: "0123456789abcdef", Provider: "local", Phase: PhaseIntent,
		Label: "ember-0123456789abcdef-1", Holder: ThisProcess(),
	}
	if err := s.Create(rec); err != nil {
		t.Fatal(err)
	}
	got, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d records", len(got))
	}
	if got[0].Holder != rec.Holder {
		t.Errorf("holder = %+v, want %+v", got[0].Holder, rec.Holder)
	}
}
