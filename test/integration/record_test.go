//go:build integration

package integration

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/Legion112/ember/internal/genstore"
	"github.com/Legion112/ember/test/integration/harness"
)

// The lab that crosses the gap no unit test can.
//
// WHAT IT IS FOR. Every half of this was testable in isolation and the
// interesting failure is the join: the store's tests prove Adopt adopts, the
// recorder's prove an event becomes a column, and `ember up`'s prove the row
// is written -- and all of them would pass with the two halves wired to
// nothing, which is the closed loop this repository has now shipped three
// times (the preview frame nothing inserted, `capabilities.model` read from a
// doc, a query layer with no reader). So this one runs the real binary against
// a real container and then asks the real database, through `ember history`,
// what it knows.
//
// It is also the only thing that can see the two columns #30 added arriving
// from an `ember up`, which is what those columns were added for: every
// measurement in README was taken with this command.
func TestRecord_UpIsInTheHistoryItRanFrom(t *testing.T) {
	l := harness.New(t)
	port := harness.FreePort(t)

	res := l.Run(t.Context(), "up",
		"--hold", "2s",
		"--port", fmt.Sprintf("sshd:22:%d", port),
	)
	if res.ExitCode != 0 {
		t.Fatalf("ember up exited %d\n%s", res.ExitCode, res.Combined())
	}
	out := res.Combined()

	// THE OUTPUT DID NOT CHANGE, which is the hard constraint on this whole
	// change: `ember up`'s lines are a contract and the labs grep them. The
	// only line recording may add is the one that says it could not, and a
	// lab whose state directory is writable must never see it.
	if !strings.Contains(out, "node ready") {
		t.Errorf("the lifecycle line a lab greps for is gone:\n%s", out)
	}
	if strings.Contains(out, "not recording") {
		t.Fatalf("ember could not record this run:\n%s", out)
	}

	rows := history(t, l)
	if len(rows) != 1 {
		t.Fatalf("got %d row(s) in the history after one `ember up`, want 1: %+v", len(rows), rows)
	}
	got := rows[0]
	t.Logf("against a real container: %+v", got)

	if got.Status != genstore.StatusDone {
		t.Errorf("status = %q, want done", got.Status)
	}
	if got.Provider != "local" {
		t.Errorf("provider = %q", got.Provider)
	}
	// THE NODE, which is the join to what `ember ls` reported at the time and
	// the only one an `ember up` row has: the run id is minted after the row
	// is written, so this is what ties a ledger row to a container. It arrives
	// from the node_ready event, so it is also the proof that the run reported
	// to the recorder and not only to the terminal.
	if got.Node == "" {
		t.Error("no node was recorded, so nothing joins this row to the rental it describes")
	}
	// A forwarded run has no jobs, and says so rather than reporting zeros:
	// the per-job columns were never going to be filled, which is a different
	// fact from figures that went missing.
	if got.Jobs != 0 || !got.Unrecorded {
		t.Errorf("jobs = %d, unrecorded = %v; a forwarded run has no jobs and no per-job figures",
			got.Jobs, got.Unrecorded)
	}
	// Billable rather than cost, because the local provider is free: a $0
	// rental is the honest figure here, and what proves the run_cost event
	// arrived is the duration beside it.
	if got.Billable <= 0 || got.Elapsed <= 0 {
		t.Errorf("elapsed/billable = %v/%v; the rental's own cost event never reached the row",
			got.Elapsed, got.Billable)
	}
	// And the memory probe, the other thing only a real container reports.
	// A lab container has no cgroup limit, so mem_limit is zero BY DESIGN --
	// mem_cgroup is what says anything was read at all, which is exactly why
	// there are two columns.
	if got.MemCgroup == 0 || got.MemTotal == 0 {
		t.Errorf("cgroup %d, host total %d: the node's memory facts were reported to the "+
			"terminal and recorded nowhere", got.MemCgroup, got.MemTotal)
	}

	// And the ledger prints it, with the footnote that is true of this run
	// rather than the one about a schema it predates.
	table := l.Run(t.Context(), "history").Combined()
	if !strings.Contains(table, string(got.ID)) {
		t.Errorf("`ember history` does not list the run:\n%s", table)
	}
	if !strings.Contains(table, "have no jobs") {
		t.Errorf("`ember history` does not say why the per-job columns are dashes:\n%s", table)
	}
	if strings.Contains(table, "before ember recorded them") {
		t.Errorf("the ledger blames the schema for a run made a second ago:\n%s", table)
	}
}

// history reads the ledger the way an operator does, through the command.
//
// Through `ember history --json` rather than by opening the database here: the
// path to the file is app.ResolvePaths' decision, and a second spelling of it
// in a lab is how a harness ends up reading a directory nothing wrote to --
// which this repository has already done once, to a lab that then asserted
// nothing.
func history(t *testing.T, l *harness.Lab) []genstore.Entry {
	t.Helper()
	res := l.Run(t.Context(), "history", "--json")
	if res.ExitCode != 0 {
		t.Fatalf("ember history exited %d\n%s", res.ExitCode, res.Combined())
	}
	var rows []genstore.Entry
	if err := json.Unmarshal([]byte(res.Stdout), &rows); err != nil {
		t.Fatalf("ember history --json is not JSON: %v\n%s", err, res.Combined())
	}
	return rows
}
