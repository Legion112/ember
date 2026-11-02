// Package labdaemon is the only thing in the lab code that looks at a whole
// Docker daemon, and the only thing allowed to remove what it finds there.
//
// It exists because of a defect that cost two suite runs and an afternoon.
// Lab.Nodes listed containers by the mere *presence* of ember's run-id label,
// with no value, and Lab.Reap force-removed everything it returned plus every
// labelled network and volume -- so a lab's cleanup was daemon-wide. That was
// safe while this repository was one checkout. Agent worktrees put several on
// one machine, each able to run `EMBER_INTEGRATION=1 go test -tags=integration
// ./test/...` against the one daemon, and the two runs then deleted each
// other's live nodes mid-test: `No such container` and `connection refused`
// for a container that existed a moment earlier, a suite wall-clock of 170s
// against the usual 85s, and every failing test passing when run alone.
//
// The breadth is not the bug and must not be narrowed. A sweep that removes
// only what the harness remembered creating is how the leaked bridge network
// stayed invisible until Docker had subnetted its whole address pool and every
// run failed with "all predefined address pools have been fully subnetted".
// The property wanted is "nothing ember left behind in this suite run", not
// "nothing of mine".
//
// So the answer here is exclusivity rather than a filter: Acquire makes one
// test binary the only one on this machine allowed to act on this daemon, and
// List taken once under that lock is the set of objects that were already
// there -- which the harness subtracts from every listing, so a container
// somebody started by hand or a leak from yesterday's SIGKILLed lab is left
// strictly alone instead of being deleted by whoever runs the suite next.
// Both halves live in this one package for the reason net.Listen lives in
// exactly one: an unscoped listing is the dangerous operation here, and a
// guard above it is worth nothing if the next caller spells its own.
package labdaemon

import (
	"context"
	"fmt"
	"strings"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/volume"
	"github.com/docker/docker/client"

	"github.com/Legion112/ember/internal/dockerengine"
	"github.com/Legion112/ember/internal/provider/local"
)

// Objects is everything on a daemon wearing one of ember's labels.
//
// The three kinds travel together because they are removed together and in
// this order: a volume still mounted by a container cannot be removed, and a
// network with a container attached cannot either.
type Objects struct {
	Containers []container.Summary
	Networks   []network.Summary
	Volumes    []*volume.Volume
}

// Empty reports whether a look at the daemon found nothing at all.
func (o Objects) Empty() bool {
	return len(o.Containers) == 0 && len(o.Networks) == 0 && len(o.Volumes) == 0
}

// Lines describes each object on its own line, for a message to a human.
func (o Objects) Lines() []string {
	var out []string
	for _, c := range o.Containers {
		out = append(out, fmt.Sprintf("container  %s  %s  %s", c.ID[:12], c.State, strings.Join(c.Names, ",")))
	}
	for _, n := range o.Networks {
		out = append(out, fmt.Sprintf("network    %s  %s", n.ID[:12], n.Name))
	}
	for _, v := range o.Volumes {
		out = append(out, fmt.Sprintf("volume     %s", v.Name))
	}
	return out
}

// List is every ember-labelled container, network and store volume on the
// daemon.
//
// It asks Docker directly rather than running `ember ls`, because lab code
// that used ember to check ember would pass whenever both sides were wrong in
// the same direction -- which is exactly what a label-matching bug looks like.
//
// Matching is on the presence of the label and never on its value. That is
// what makes it catch what nobody thought to look for, and it is also why
// every caller is either holding the lock or reporting rather than removing.
func List(ctx context.Context, cli *client.Client) (Objects, error) {
	var o Objects
	var err error
	if o.Containers, err = Containers(ctx, cli); err != nil {
		return Objects{}, err
	}
	if o.Networks, err = Networks(ctx, cli); err != nil {
		return Objects{}, err
	}
	if o.Volumes, err = Volumes(ctx, cli); err != nil {
		return Objects{}, err
	}
	return o, nil
}

// Containers is every container wearing ember's run-id label, running or not.
//
// Separate from List because the labs poll it -- WaitNode and WaitNoNodes ask
// several times a second -- and asking the daemon for networks and volumes on
// every one of those would be three calls where one is wanted.
func Containers(ctx context.Context, cli *client.Client) ([]container.Summary, error) {
	cs, err := cli.ContainerList(ctx, container.ListOptions{
		All:     true,
		Filters: filters.NewArgs(filters.Arg("label", dockerengine.LabelRunID)),
	})
	if err != nil {
		return nil, fmt.Errorf("list containers: %w", err)
	}
	return cs, nil
}

// Networks is every network wearing ember's run-id label.
func Networks(ctx context.Context, cli *client.Client) ([]network.Summary, error) {
	ns, err := cli.NetworkList(ctx, network.ListOptions{
		Filters: filters.NewArgs(filters.Arg("label", dockerengine.LabelRunID)),
	})
	if err != nil {
		return nil, fmt.Errorf("list networks: %w", err)
	}
	return ns, nil
}

// Volumes is every store volume wearing ember's kind label.
func Volumes(ctx context.Context, cli *client.Client) ([]*volume.Volume, error) {
	resp, err := cli.VolumeList(ctx, volume.ListOptions{
		Filters: filters.NewArgs(filters.Arg("label", local.LabelKind)),
	})
	if err != nil {
		return nil, fmt.Errorf("list volumes: %w", err)
	}
	var out []*volume.Volume
	for _, v := range resp.Volumes {
		// Stores only. The other kinds ember labels -- a node, a network --
		// are not volumes, and a kind nobody has seen yet must not be removed
		// by a cleanup written before it existed.
		if v != nil && v.Labels[local.LabelKind] == "store" {
			out = append(out, v)
		}
	}
	return out, nil
}

// Remove force-removes everything in o and returns what went wrong.
//
// Containers first, then networks, then volumes: a network with a container
// attached and a volume with a container mounting it both refuse to go while
// the container is there, and the resulting failure would leave exactly the
// leak this is for.
//
// Errors are returned rather than logged because the two callers want
// different things from them. A lab's cleanup ignores them -- it is tidying up
// after assertions that have already been recorded, and the stray report is
// what says a lab leaked -- while `make reap-labs` is a tool an operator ran
// on purpose and has to be told what it could not remove.
func Remove(ctx context.Context, cli *client.Client, o Objects) []error {
	var errs []error
	for _, c := range o.Containers {
		if err := cli.ContainerRemove(ctx, c.ID, container.RemoveOptions{Force: true}); err != nil {
			errs = append(errs, fmt.Errorf("remove container %s: %w", c.ID[:12], err))
		}
	}
	for _, n := range o.Networks {
		if err := cli.NetworkRemove(ctx, n.ID); err != nil {
			errs = append(errs, fmt.Errorf("remove network %s: %w", n.Name, err))
		}
	}
	for _, v := range o.Volumes {
		if err := cli.VolumeRemove(ctx, v.Name, true); err != nil {
			errs = append(errs, fmt.Errorf("remove volume %s: %w", v.Name, err))
		}
	}
	return errs
}

// Reported is the subset of o that ember's own listings would report.
//
// ember looks at the whole daemon on purpose: that is how `ember ls` finds an
// untracked node nothing recorded, and how `ember reap` sweeps a leftover
// network whose run is gone. So a foreign object of one of those kinds makes
// every lab that asserts on that output fail, whatever the harness filters on
// -- and the harness must not delete it, because an object it did not create
// is exactly what it cannot be sure about.
//
// MEASURED, not reasoned. With one stale `ember.kind=node` container on the
// daemon, TestLeftover_ReapRemovesANetworkNothingElseWould fails with "ls
// still reports findings after reap" and ember names the container
// `untracked`. The failure is correct and says nothing about the daemon, which
// is why the harness refuses up front instead.
//
// A container wearing only `ember.run-id` is deliberately not in here.
// local.List filters on `ember.kind`, so nothing in ember ever looks at one;
// StartNode without EmberLabels makes exactly that shape, and a run may leave
// one around to be poked at. Those are tolerated: left alone, and subtracted
// from every listing by Except.
func (o Objects) Reported() Objects {
	var out Objects
	for _, c := range o.Containers {
		if c.Labels[local.LabelKind] != "" {
			out.Containers = append(out.Containers, c)
		}
	}
	// Every labelled network, because a network with no node container beside
	// it is what `ember ls` calls a leftover, and the deadline that decides
	// whether it says so yet is a clock away from being past.
	out.Networks = o.Networks
	// Every store, because `ember store ls` prices what is on the daemon.
	out.Volumes = o.Volumes
	return out
}

// Except returns o without the objects in keep.
//
// This is the subtraction that makes a daemon-wide cleanup safe: keep is the
// snapshot taken when the suite took the lock, so what comes back is what this
// suite run produced -- including, deliberately, the objects it produced
// without meaning to.
func (o Objects) Except(keep Objects) Objects {
	var out Objects
	ids := make(map[string]bool, len(keep.Containers)+len(keep.Networks))
	for _, c := range keep.Containers {
		ids[c.ID] = true
	}
	for _, n := range keep.Networks {
		ids[n.ID] = true
	}
	names := make(map[string]bool, len(keep.Volumes))
	for _, v := range keep.Volumes {
		names[v.Name] = true
	}

	for _, c := range o.Containers {
		if !ids[c.ID] {
			out.Containers = append(out.Containers, c)
		}
	}
	for _, n := range o.Networks {
		if !ids[n.ID] {
			out.Networks = append(out.Networks, n)
		}
	}
	for _, v := range o.Volumes {
		if v != nil && !names[v.Name] {
			out.Volumes = append(out.Volumes, v)
		}
	}
	return out
}

// DaemonID identifies the daemon a client is talking to.
//
// The lock is about one daemon and not about one machine, so this is what it
// is keyed by. DOCKER_HOST would have been free to read and is the wrong
// answer: an unset variable and an explicit `unix:///var/run/docker.sock` name
// the same socket and would hash to two different lock files, which is a lock
// that silently does not serialize -- the failure mode it exists to end. The
// daemon's own id cannot be spelled two ways.
func DaemonID(ctx context.Context, cli *client.Client) (string, error) {
	info, err := cli.Info(ctx)
	if err != nil {
		return "", fmt.Errorf("ask the daemon for its id: %w", err)
	}
	if info.ID == "" {
		return "", fmt.Errorf("the daemon reported no id; the lab lock cannot be keyed to it")
	}
	return info.ID, nil
}
