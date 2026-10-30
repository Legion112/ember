package node

import (
	"fmt"
	"path"
	"strings"
)

// A store is persistent storage that outlives every node that mounts it.
//
// It is called a store and not a volume because Spec.Volume already means
// something else here -- RunPod's container disk, which dies with the pod --
// and two things called "volume" with opposite lifetimes is how an operator
// deletes the wrong one. RunPod's own name for this is "network volume".
//
// The lifetime is the whole reason this type needs care. A node is destroyed
// when the run ends; a store is not. It bills by the gigabyte-month whether or
// not anything is attached to it, which makes it the first resource in ember
// that can leak while every node is correctly destroyed and `ember ls` prints
// a clean bill of health. Every exported thing below exists to make that
// leak visible or impossible.

// StoreID is a provider's own identifier for a store.
type StoreID string

// StoreHandle names one store at one provider.
//
// Separate from Handle because a store is not a node: nothing that takes a
// Handle should accept one by mistake, and "destroy this handle" must not be
// able to mean "destroy forty gigabytes of weights".
type StoreHandle struct {
	Provider ProviderID
	ID       StoreID
}

func (h StoreHandle) String() string {
	return string(h.Provider) + "/" + string(h.ID)
}

// IsZero reports whether the handle names nothing.
func (h StoreHandle) IsZero() bool { return h.Provider == "" && h.ID == "" }

// Size bounds, as the provider enforces them.
//
// Measured rather than read: RunPod's documentation gives the minimum as 0,
// and the API answers a request for 1 with "minimum: got 1, want 10". The
// ceiling is 4096 where the docs say 4000. Both figures come from the live
// 422, which is also why they are constants here instead of a comment -- a
// request ember refuses locally costs nothing, and one the provider refuses
// costs a round trip and an error nobody expected.
const (
	MinStoreGiB = 10
	MaxStoreGiB = 4096
)

// Store is one persistent store.
type Store struct {
	Handle StoreHandle

	// Name is ember's name for it, without the label prefix.
	Name string

	// SizeGiB is what the provider bills for, which is what was asked for and
	// not what is used. A store 2% full costs the same as a full one.
	SizeGiB int

	// Region is the datacentre the store lives in, and therefore the only
	// datacentre a node that mounts it can be placed in. Empty means the
	// provider does not pin stores to a region.
	Region string
}

func (s Store) String() string {
	r := s.Region
	if r == "" {
		r = "anywhere"
	}
	return fmt.Sprintf("%s (%s, %d GiB, %s)", s.Name, s.Handle, s.SizeGiB, r)
}

// HoursPerMonth is the divisor between a monthly storage price and an hourly
// one.
//
// 730 is 365*24/12, the convention every cloud bills storage by. It is named
// rather than inlined because a reader comparing a store's hourly cost with a
// GPU's hourly rate is entitled to know which month was meant.
const HoursPerMonth = 730

// StorePrice is what one GiB of store costs for one month.
type StorePrice MicroUSD

func (p StorePrice) String() string { return MicroUSD(p).String() + "/GiB/month" }

// Monthly is what a store of this size costs for one month.
//
// Saturating rather than wrapping, for the same reason Rate.Cost saturates: a
// negative cost compares as under budget. The inputs here are small enough
// that overflow needs a deliberately absurd price, which is exactly the case
// where silently returning a negative number would be worst.
func (p StorePrice) Monthly(sizeGiB int) MicroUSD {
	if p <= 0 || sizeGiB <= 0 {
		return 0
	}
	total := int64(p) * int64(sizeGiB)
	if total/int64(sizeGiB) != int64(p) {
		return MaxMicroUSD
	}
	return MicroUSD(total)
}

// Hourly is what a store of this size costs for one hour, as a Rate, so it can
// be added to a node's rate and read in the same units.
//
// Rounded up, never truncated. Integer division would report a 10 GiB store at
// $0.07/GiB/month as costing 958 microdollars an hour against a true 958.9,
// and a cost that rounds down is a bill that arrives larger than the estimate.
// RateFromUSD rounds for the same reason.
func (p StorePrice) Hourly(sizeGiB int) Rate {
	m := p.Monthly(sizeGiB)
	if m <= 0 {
		return 0
	}
	if m == MaxMicroUSD {
		return MaxRate
	}
	r := (int64(m) + HoursPerMonth - 1) / HoursPerMonth
	if r > int64(MaxRate) {
		return MaxRate
	}
	return Rate(r)
}

// Mount is where a store is attached inside the node's filesystem.
//
// Sealed, like loopback.Bind and exposure.PortRequest: the field is unexported
// and there is no literal path, so every Mount in the program came through
// MountAt and was checked. The alternative is a plain string, and a plain
// string read from a config file is how a store gets mounted over /root.
type Mount struct {
	path string
}

// Path is where the store is attached. The zero Mount has no path.
func (m Mount) Path() string { return m.path }

// IsZero reports whether this is the zero Mount, which mounts nothing.
func (m Mount) IsZero() bool { return m.path == "" }

func (m Mount) String() string {
	if m.IsZero() {
		return "(no mount)"
	}
	return m.path
}

// refusedMountPaths are the paths a store may not be mounted at or over.
//
// Two different failures, both silent:
//
// /root and /etc hold the authorized_keys and the host key that embernode
// writes at startup. A store mounted over either shadows them, sshd comes up
// with neither, and the node is unreachable -- and a node ember cannot reach
// is a node whose workload cannot run and whose output cannot be fetched. The
// node still gets destroyed, because that goes through the provider's API, so
// the symptom is a GPU rented for nothing and a handshake failure that reads
// exactly like a host-key attack.
//
// /workspace is the expensive one. It is RunPod's default mount path and it is
// where ember's own outputs are written, so a store mounted there puts every
// render on billable persistent storage that nothing destroys. The operator
// finds out a month later, on the invoice.
//
// The rest are the system directories where shadowing anything breaks the node
// in a way that will be blamed on ember.
var refusedMountPaths = []string{
	"/bin", "/boot", "/dev", "/etc", "/lib", "/lib64", "/proc",
	"/root", "/run", "/sbin", "/srv", "/sys", "/usr", "/var",
	"/workspace",
}

// MountAt returns the Mount for an absolute path, or refuses it.
//
// It refuses rather than cleans. "/models/.." and "/" are different requests,
// and an operator who typed the first is owed an error rather than the second
// -- the same rule validateOutputs follows, and the one transfer.safeName
// exists because an earlier version broke.
func MountAt(p string) (Mount, error) {
	if p == "" {
		return Mount{}, fmt.Errorf("mount path is empty")
	}
	if !path.IsAbs(p) {
		return Mount{}, fmt.Errorf("mount path %q is relative; a store is mounted at an absolute path inside the node", p)
	}
	if path.Clean(p) != p {
		return Mount{}, fmt.Errorf("mount path %q is not in its simplest form; write it as %q if that is what you meant", p, path.Clean(p))
	}
	if p == "/" {
		return Mount{}, fmt.Errorf("a store cannot be mounted at /: it would shadow the whole node")
	}
	for _, bad := range refusedMountPaths {
		if p == bad || strings.HasPrefix(p, bad+"/") {
			return Mount{}, fmt.Errorf("a store cannot be mounted at %q: %s", p, whyRefused(bad))
		}
	}
	return Mount{path: p}, nil
}

// whyRefused names the failure each refused path causes, because "not allowed"
// sends an operator to the source and a reason sends them to their config.
func whyRefused(base string) string {
	switch base {
	case "/root", "/etc":
		return base + " holds the host key and authorized_keys ember installs at startup; shadowing it leaves a node nothing can log into"
	case "/workspace":
		return "/workspace is where outputs are written, and a store mounted there bills for every render for as long as the store exists"
	default:
		return base + " is a system directory, and shadowing it breaks the node"
	}
}
