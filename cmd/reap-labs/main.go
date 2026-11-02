// Command reap-labs removes every ember-labelled object from the local Docker
// daemon.
//
// It is a developer tool and says so: it is not an assertion, nothing in CI
// runs it, and it will delete objects no test created. It exists because the
// lab harness stopped doing this on its own.
//
// The harness used to force-remove every container, network and volume
// wearing ember's label at the end of every lab, which made a lab's cleanup
// daemon-wide: two agents running the integration suite in two worktrees
// against the one daemon deleted each other's live nodes mid-test. It now
// subtracts the objects that were on the daemon before the suite took the lab
// lock, so a leak from a run that was SIGKILLed yesterday -- or a container
// somebody started by hand to poke at -- survives instead of being removed by
// whoever runs the suite next. That is the right default for an assertion and
// a nuisance for a developer, which is what this is for. `docker rm -f` by
// hand is the alternative, and it needs the label spelled correctly, in three
// kinds, in the right order.
//
// It takes the same lock the suite takes, and refuses when a suite holds it.
// A tool whose whole job is a daemon-wide delete is the one place where
// "another integration run is in progress" has to be a refusal rather than a
// wait: waiting would mean removing the objects of the run that just finished,
// seconds after it finished, which is indistinguishable from the bug.
package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/docker/docker/client"

	"github.com/Legion112/ember/test/integration/harness/labdaemon"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "reap-labs: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return fmt.Errorf("construct a Docker client: %w", err)
	}
	defer func() { _ = cli.Close() }()

	id, err := labdaemon.DaemonID(ctx, cli)
	if err != nil {
		return err
	}

	// No wait at all: see the refusal argument above.
	lock, err := labdaemon.Acquire(id, 0, nil)
	if err != nil {
		return err
	}
	defer func() { _ = lock.Release() }()

	found, err := labdaemon.List(ctx, cli)
	if err != nil {
		return err
	}
	if found.Empty() {
		fmt.Println("nothing wearing ember's label is on this daemon")
		return nil
	}
	for _, line := range found.Lines() {
		fmt.Println("removing  " + line)
	}

	errs := labdaemon.Remove(ctx, cli, found)
	for _, e := range errs {
		fmt.Fprintf(os.Stderr, "reap-labs: %v\n", e)
	}
	n := len(found.Containers) + len(found.Networks) + len(found.Volumes)
	if len(errs) > 0 {
		// Named rather than counted away. What is left is what the next suite
		// run will leave alone, so an operator who thinks this cleared the
		// daemon and finds it did not is the person this tool was supposed to
		// help.
		return fmt.Errorf("%d of %d could not be removed", len(errs), n)
	}
	fmt.Printf("removed %d object(s)\n", n)
	return nil
}
