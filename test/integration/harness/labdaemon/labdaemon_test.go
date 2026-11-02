package labdaemon

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/volume"
)

// These need no Docker daemon, which is why they are here rather than in a
// lab: what they are about is the arithmetic that decides whether a cleanup
// may remove something, and that decision must be checkable without the thing
// it would destroy.

// TestExcept_LeavesWhatWasAlreadyOnTheDaemon is the defect, as a table.
//
// Lab.Nodes listed containers by the presence of ember's run-id label and Reap
// force-removed everything it returned, so a lab's cleanup was a whole-daemon
// delete: two worktrees running the labs against one daemon destroyed each
// other's nodes mid-test. Except is the subtraction that fixed it, and its two
// directions are both load-bearing -- what it keeps out, and what it must
// still let through.
func TestExcept_LeavesWhatWasAlreadyOnTheDaemon(t *testing.T) {
	foreign := Objects{
		Containers: []container.Summary{{ID: "aaaa"}},
		Networks:   []network.Summary{{ID: "nnnn"}},
		Volumes:    []*volume.Volume{{Name: "ember-store-old"}},
	}
	now := Objects{
		Containers: []container.Summary{{ID: "aaaa"}, {ID: "bbbb"}},
		Networks:   []network.Summary{{ID: "nnnn"}, {ID: "mmmm"}},
		Volumes:    []*volume.Volume{{Name: "ember-store-old"}, {Name: "ember-store-new"}},
	}

	got := now.Except(foreign)

	if len(got.Containers) != 1 || got.Containers[0].ID != "bbbb" {
		t.Errorf("containers: %v\nthe one that was already there must not be reaped by whoever runs the suite next", got.Containers)
	}
	// The leak this breadth exists for: a network nobody listed, found by the
	// label, still comes back. Dropping it is how Docker ran out of address
	// pools.
	if len(got.Networks) != 1 || got.Networks[0].ID != "mmmm" {
		t.Errorf("networks: %v\na network this run produced must still be reported and reaped", got.Networks)
	}
	if len(got.Volumes) != 1 || got.Volumes[0].Name != "ember-store-new" {
		t.Errorf("volumes: %v", got.Volumes)
	}
}

// TestExcept_OfNothingKeepsEverything. The ordinary case on a clean daemon:
// the snapshot is empty, so every assertion in every lab sees exactly what it
// saw before this change.
func TestExcept_OfNothingKeepsEverything(t *testing.T) {
	now := Objects{
		Containers: []container.Summary{{ID: "aaaa"}},
		Networks:   []network.Summary{{ID: "nnnn"}},
		Volumes:    []*volume.Volume{{Name: "ember-store"}},
	}
	got := now.Except(Objects{})
	if len(got.Containers) != 1 || len(got.Networks) != 1 || len(got.Volumes) != 1 {
		t.Errorf("an empty snapshot dropped something: %+v", got)
	}
	if got.Empty() {
		t.Error("Empty reports nothing on a daemon holding three objects")
	}
}

// TestReported_IsWhatEmberItselfWouldName.
//
// The refusal condition, and the measurement behind it: a stale node container
// makes `ember ls` report an untracked node, so a lab asserting that `ls`
// exits 0 cannot pass while it is there -- while a container wearing only
// ember's run-id is invisible to every ember listing and is tolerated. Get
// this wrong in the lenient direction and the suite fails ninety seconds in
// with a message about ember; get it wrong in the strict direction and
// `make reap-labs` becomes a prerequisite of running the labs at all.
func TestReported_IsWhatEmberItselfWouldName(t *testing.T) {
	o := Objects{
		Containers: []container.Summary{
			{ID: "aaaa", Labels: map[string]string{"ember.run-id": "x"}},
			{ID: "bbbb", Labels: map[string]string{"ember.run-id": "y", "ember.kind": "node"}},
		},
		Networks: []network.Summary{{ID: "nnnn"}},
		Volumes:  []*volume.Volume{{Name: "ember-store"}},
	}

	got := o.Reported()

	if len(got.Containers) != 1 || got.Containers[0].ID != "bbbb" {
		t.Errorf("containers: %v\nwant only the one ember ls calls untracked", got.Containers)
	}
	if len(got.Networks) != 1 {
		t.Errorf("networks: %v\na labelled network with no node beside it is what ls calls a leftover", got.Networks)
	}
	if len(got.Volumes) != 1 {
		t.Errorf("volumes: %v\n`ember store ls` prices what is on the daemon", got.Volumes)
	}
	if !(Objects{Containers: []container.Summary{{ID: "aaaa", Labels: map[string]string{"ember.run-id": "x"}}}}).Reported().Empty() {
		t.Error("a container wearing only ember's run-id is refused; nothing in ember looks at one, and StartNode makes exactly that shape")
	}
}

// TestAcquire_RefusesWhileHeldAndYieldsAfterRelease.
//
// Two open descriptors on one file conflict under flock even inside one
// process, which is what makes this testable without starting a second suite.
// The property is the one the labs depend on: while a run holds the daemon, a
// second one does not get to act on it.
func TestAcquire_RefusesWhileHeldAndYieldsAfterRelease(t *testing.T) {
	id := fmt.Sprintf("labdaemon-test-%d", os.Getpid())
	t.Cleanup(func() { _ = os.Remove(LockPath(id)) })

	first, err := Acquire(id, 0, nil)
	if err != nil {
		t.Fatalf("take the lock on an idle daemon: %v", err)
	}

	if _, err := Acquire(id, 0, nil); err == nil {
		t.Fatal("a second run took the lock while the first held it; its cleanup would then delete the first's live nodes")
	}

	// Waiting rather than refusing is the suite's answer, so a wait that
	// cannot be satisfied has to end in an error naming the holder -- never in
	// a silent pass, and never in a hang.
	// Longer than the one-second grace before the first progress report: the
	// holder stamps its identity after taking the lock, so a report sent
	// immediately can name the previous holder -- which is what happened the
	// first time two suites were started together.
	waited := false
	_, err = Acquire(id, 1500*time.Millisecond, func(time.Duration, string) { waited = true })
	if err == nil {
		t.Fatal("a bounded wait succeeded while the lock was held")
	}
	if !waited {
		t.Error("the wait reported nothing to the operator; a queued suite that says nothing looks hung")
	}
	if holder := err.Error(); !strings.Contains(holder, fmt.Sprintf("pid %d", os.Getpid())) {
		t.Errorf("the failure does not name the holder, so nobody can find it:\n%s", holder)
	}

	if err := first.Release(); err != nil {
		t.Fatalf("release: %v", err)
	}
	second, err := Acquire(id, 0, nil)
	if err != nil {
		t.Fatalf("the lock was not released, so the next suite can never run: %v", err)
	}
	if err := second.Release(); err != nil {
		t.Fatalf("release: %v", err)
	}
}
