// Package dockerengine is ember's narrow view of the Docker Engine API.
//
// The interface exists so the local provider's logic — which labels to set,
// which port to read back, what order to tear things down in — is tested
// against a recording fake in milliseconds, and so the only code that talks to
// a real daemon is the thin adapter in client.go, which has no branches worth
// testing.
//
// It is also where the one rule about Docker lives: ember never uses host
// networking. See RefuseHostNetwork.
package dockerengine

import (
	"context"
	"errors"
	"fmt"
)

// LabelRunID is the container and network label carrying ember's run id.
// Everything ember creates wears it, which is what makes `ember ls` able to
// find an orphan without consulting local state.
const LabelRunID = "ember.run-id"

// ErrNotFound means the daemon has no such object. Destroy treats it as
// success.
var ErrNotFound = errors.New("docker: no such object")

// Container is what ember needs to know about one container.
type Container struct {
	ID     string
	Name   string
	Image  string
	State  string // "created" | "running" | "exited" | "paused" | "dead"
	Labels map[string]string

	// Ports maps a container port to the host port the daemon published it
	// on, read back from the daemon rather than guessed. Asking for host port
	// 0 and then assuming you know which port you got is how a forward
	// silently points at somebody else's service.
	Ports map[uint16]uint16

	// IPs maps network name to the container's address on it.
	IPs      map[string]string
	ExitCode int
}

// Network is a user-defined bridge ember created.
type Network struct {
	ID     string
	Name   string
	Labels map[string]string
}

// ContainerSpec is a container ember wants created.
type ContainerSpec struct {
	Name    string
	Image   string
	Cmd     []string
	Env     map[string]string
	Labels  map[string]string
	Network string

	// PublishTCP lists container ports to publish on an ephemeral loopback
	// host port. It is deliberately not a map to a chosen host port: letting
	// the daemon pick and reading it back removes the race where two runs
	// want the same number.
	PublishTCP []uint16

	// Binds are host:container mounts.
	Binds []string

	// NetworkMode is normally empty, meaning the user-defined Network above.
	// It exists only so RefuseHostNetwork has something to refuse.
	NetworkMode string
}

// API is the subset of the Docker Engine API ember uses.
type API interface {
	Ping(ctx context.Context) error
	ImageExists(ctx context.Context, ref string) (bool, error)

	NetworkCreate(ctx context.Context, name string, labels map[string]string) (string, error)
	NetworkRemove(ctx context.Context, id string) error
	NetworkList(ctx context.Context, label string) ([]Network, error)

	ContainerCreate(ctx context.Context, spec ContainerSpec) (string, error)
	ContainerStart(ctx context.Context, id string) error
	ContainerInspect(ctx context.Context, id string) (Container, error)
	ContainerRemove(ctx context.Context, id string) error
	ContainerList(ctx context.Context, label string) ([]Container, error)

	// Exec runs a command in a running container and returns its combined
	// output. ember uses it for exactly one thing: reading the host key it
	// told the container to generate.
	Exec(ctx context.Context, id string, cmd []string) (string, error)
}

// RefuseHostNetwork rejects host networking.
//
// --network host makes every listener inside the container a listener on the
// operator's machine, which walks straight past internal/loopback: a workload
// that binds 0.0.0.0:8188 would then be published on the LAN by the container
// runtime, with nothing in ember having made a decision about it.
//
// It is a function with its own test rather than a line in a document,
// following the same pattern as go-tun's lab harness.
func RefuseHostNetwork(mode string) error {
	switch mode {
	case "", "bridge", "none":
		return nil
	case "host":
		return fmt.Errorf("refusing host networking: every listener in the container would become a listener on this machine, bypassing the loopback guard entirely")
	default:
		// An allow-list. A mode this function does not positively recognise
		// is refused, because the next one Docker adds might also share the
		// host's network namespace and nobody would revisit this switch.
		return fmt.Errorf("refusing unrecognised network mode %q: only an ember-owned bridge is allowed", mode)
	}
}
