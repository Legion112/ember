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
	"net"
	"net/netip"
	"strconv"
)

// LabelRunID is the container and network label carrying ember's run id.
// Everything ember creates wears it, which is what makes `ember ls` able to
// find an orphan without consulting local state.
const LabelRunID = "ember.run-id"

// ErrNotFound means the daemon has no such object. Destroy treats it as
// success.
var ErrNotFound = errors.New("docker: no such object")

// Binding is where the daemon published one container port.
type Binding struct {
	// IP is the host address the port is bound to. Docker's own default is
	// 0.0.0.0 -- every interface -- so this being loopback is a fact to read
	// back and check, never one to take for granted.
	IP   string
	Port uint16
}

// IsLoopback reports whether only this machine can reach the binding.
//
// It parses rather than comparing against "127.0.0.1": 127.0.0.1, ::1 and
// 127.0.0.53 are all loopback and only one of them is the obvious spelling.
// An unparseable or empty IP is treated as not loopback, because the safe
// reading of "I cannot tell where this is bound" is "somewhere public".
func (b Binding) IsLoopback() bool {
	addr, err := netip.ParseAddr(b.IP)
	if err != nil {
		return false
	}
	return addr.IsLoopback()
}

func (b Binding) String() string { return net.JoinHostPort(b.IP, strconv.Itoa(int(b.Port))) }

// Container is what ember needs to know about one container.
type Container struct {
	ID     string
	Name   string
	Image  string
	State  string // "created" | "running" | "exited" | "paused" | "dead"
	Labels map[string]string

	// Ports maps a container port to where the daemon published it, read back
	// from the daemon rather than guessed. Asking for host port 0 and then
	// assuming you know which port you got is how a forward silently points
	// at somebody else's service.
	//
	// The host IP is part of the value and not dropped, because it is the
	// difference between a port only this machine can reach and one the whole
	// LAN can. An exposure assertion that has to assume which of those
	// happened is not an assertion.
	Ports map[uint16]Binding

	// IPs maps network name to the container's address on it.
	IPs map[string]string

	// Binds is what was mounted into this container, as host:container
	// strings. Reported so a test can tell a node created with a store from
	// one created without; the daemon is the source for a real container.
	Binds []string

	// GPUs is how many devices the daemon says were handed to this
	// container. Reported for the same reason Binds is: a container created
	// without a GPU is indistinguishable from one created with it until
	// something asks the daemon, and the failure mode is a workload that
	// silently runs on the CPU at full price.
	GPUs int

	ExitCode int
}

// Volume is a Docker named volume ember created.
//
// It is the local provider's stand-in for a cloud network volume: it outlives
// every container that mounts it, it is listed and removed independently, and
// it is the only thing in the lab that behaves like a resource nothing
// destroys for you. A bind mount into a temp directory would have been less
// code and would not have had that property, which is the one being tested.
type Volume struct {
	Name   string
	Labels map[string]string
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

	// GPUs is how many NVIDIA devices to hand the container. Zero is none,
	// which is what every container ember created before local GPU runs
	// existed.
	//
	// A count rather than a bool, because the Docker API takes one and
	// because "all" is a thing a machine with two cards should have to ask
	// for deliberately. It becomes a DeviceRequest with the nvidia driver,
	// which needs nvidia-container-toolkit installed on the host -- without
	// it the daemon refuses the create, which is the right failure: loudly,
	// before anything runs, rather than a container that quietly has no card.
	GPUs int
}

// API is the subset of the Docker Engine API ember uses.
type API interface {
	Ping(ctx context.Context) error
	ImageExists(ctx context.Context, ref string) (bool, error)

	// HasGPURuntime reports whether the daemon can hand a container an
	// NVIDIA device.
	//
	// Separate from "is there a card in this machine", because the two fail
	// independently and only together make a GPU offer honest. A desktop
	// with a 2080 Ti and no nvidia-container-toolkit has a perfectly good
	// card that Docker cannot pass through, and an offer advertising it
	// would be a promise ember cannot keep -- discovered at create, after an
	// operator wrote a config around it.
	HasGPURuntime(ctx context.Context) (bool, error)

	NetworkCreate(ctx context.Context, name string, labels map[string]string) (string, error)
	NetworkRemove(ctx context.Context, id string) error
	NetworkList(ctx context.Context, label string) ([]Network, error)

	// VolumeCreate is idempotent: Docker returns the existing volume for a
	// name that already exists rather than failing, which matches what
	// `ember store create` should do when run twice.
	VolumeCreate(ctx context.Context, name string, labels map[string]string) (Volume, error)
	VolumeRemove(ctx context.Context, name string) error
	VolumeList(ctx context.Context, label string) ([]Volume, error)

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
