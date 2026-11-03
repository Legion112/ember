package app

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/Legion112/ember/internal/config"
	"github.com/Legion112/ember/internal/node"
	"github.com/Legion112/ember/internal/s3"
)

// AssetsMissingError is this run's weights not being on the store, found
// before anything was rented.
//
// WHY IT IS A TYPE AND NOT A SENTENCE. The two callers want opposite things
// from the same fact. `ember up` refuses with it, because an interactive
// command must not rent a second machine nobody asked for; `ember serve`
// catches it and fills the store on a machine with no card, which is the
// whole saving. A plain error would make the second one a string match.
type AssetsMissingError struct {
	// Store is the store that was asked, by the name the config gives it.
	Store string

	// Missing names the assets that are absent or zero-length, in the order
	// the config lists them.
	Missing []node.Asset
}

func (e *AssetsMissingError) Error() string {
	names := make([]string, 0, len(e.Missing))
	for _, a := range e.Missing {
		names = append(names, a.As)
	}
	return fmt.Sprintf("%d of this run's asset(s) are not on the store %q: %s. "+
		"A rented card would spend its first minutes downloading them, billed at the card's rate. Fill the store first with:\n"+
		"  ember store sync\n"+
		"which rents a machine with no card to do exactly this",
		len(e.Missing), e.Store, strings.Join(names, ", "))
}

// AssetCheck is what one pre-flight asked, and whether it could ask.
//
// Skipped is not an empty Missing. A store this build cannot read over S3 and
// a store with every asset on it are the same zero slice, and treating the
// first as the second is how "I could not check" becomes "it is fine" -- the
// rule `node.Exposure` already follows with its second result, and the one
// `ember ls` follows when a provider could not be reached.
type AssetCheck struct {
	// Asked is false when the question could not be put to the store at all.
	Asked bool

	// Why says what stopped it, for the one log line a skipped check prints.
	// Empty when Asked.
	Why string

	Missing []node.Asset
}

// CheckStoreAssets asks the store whether this run's weights are on it.
//
// ONE HEAD PER ASSET, OVER THE STORE'S OWN ENDPOINT, FOR NOTHING. Assets are
// verified by digest on the node, over the SSH connection the workload uses,
// which is the right place for the digest and the wrong place to discover an
// absence: ember rented an RTX PRO 6000 at $2.49/hr on 2026-11-03 and spent
// 14m47s of it -- 97.9% of the rental -- pulling 56.16 GiB from HuggingFace at
// 68 MB/s with the card at 0% utilisation. The same download on the CPU
// machine `ember store sync` rents is $0.06/hr. The check that would have
// caught it existed the whole time, as a command an operator had to know to
// type; this is the same question asked by the code that is about to rent.
//
// IT DOES NOT CHECK DIGESTS, for the reason `ember store verify` does not: a
// digest means reading every object back through this process to re-derive a
// number the node derives anyway, at datacentre bandwidth, before the workload
// starts. This answers "is it there, and is it a plausible file"; the node
// still answers "is it the right bytes", and a mismatch there is still a
// refusal.
//
// A ZERO-LENGTH OBJECT COUNTS AS MISSING. That is what a PUT which failed
// after creating the key leaves behind, and under `verify: present` the node
// would find it, call it there, and load nothing.
func CheckStoreAssets(
	ctx context.Context,
	cfg config.Config,
	creds config.Credentials,
	store Attached,
	assets []node.Asset,
	d Deps,
) (AssetCheck, error) {
	switch {
	case len(assets) == 0:
		return AssetCheck{Asked: true}, nil
	case store.IsZero():
		// Unreachable through NewSpec, which refuses assets with nowhere to
		// land, and answered here rather than assumed because this runs
		// before BuildSpec does.
		return AssetCheck{Why: "this run mounts no store"}, nil
	case store.Region == "":
		// The local provider reports no region for its volumes, because a
		// Docker volume is not in a datacentre and there is no endpoint to
		// ask. Named rather than attempted against a hostname that does not
		// exist.
		return AssetCheck{Why: "this provider does not expose its stores over S3, so there is nothing to ask"}, nil
	case creds.S3.IsZero():
		// NOT A REFUSAL. RunPod issues S3 keys separately from the API key
		// and an account can have one and not the other, so an operator who
		// has never made them runs today and must keep running -- refusing
		// here would break working setups in order to save money on broken
		// ones. It is said out loud, because a check that silently does
		// nothing is how the saving is lost again.
		return AssetCheck{Why: "no S3 credentials, which RunPod issues separately from the API key"}, nil
	}

	endpoint := s3.EndpointFor(store.Region)
	if d.S3BaseURL != "" {
		endpoint = d.S3BaseURL
	}
	c := &s3.Client{Endpoint: endpoint, Region: store.Region, Creds: creds.S3}
	bucket := string(store.Handle.ID)

	var missing []node.Asset
	for _, a := range assets {
		o, found, err := c.Head(ctx, bucket, a.As)
		if err != nil {
			// "I could not ask" is not "it is not there". A 403 means the
			// credentials cannot see this store, and counting that as an
			// absence would send a `serve` run off to rent a second machine
			// and re-download forty gigabytes over a permissions problem.
			return AssetCheck{}, fmt.Errorf("ask the store %q whether %s is on it: %w", cfg.Store.Name, a.As, err)
		}
		if !found || o.Size == 0 {
			missing = append(missing, a)
		}
	}
	return AssetCheck{Asked: true, Missing: missing}, nil
}

// MissingAssets returns the error for a check that found some, or nil.
//
// Separate from CheckStoreAssets so that a caller which wants the finding
// without the refusal -- the queue, which is about to act on it rather than
// report it -- is not reading an error to learn a fact.
func MissingAssets(name string, c AssetCheck) error {
	if len(c.Missing) == 0 {
		return nil
	}
	return &AssetsMissingError{Store: name, Missing: c.Missing}
}

// IsAssetsMissing reports whether err is this run's weights being absent.
func IsAssetsMissing(err error) (*AssetsMissingError, bool) {
	var e *AssetsMissingError
	ok := errors.As(err, &e)
	return e, ok
}
