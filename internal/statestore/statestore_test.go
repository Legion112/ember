package statestore

import (
	"os"
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
