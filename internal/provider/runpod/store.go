package runpod

import (
	"context"
	"fmt"
	"net/url"

	"github.com/Legion112/ember/internal/node"
	"github.com/Legion112/ember/internal/provider"
)

// listPageLimit is how many volumes to ask for per page.
//
// The API's ceiling is 1000. Asking for it does not make the cursor loop
// below unnecessary -- an account with more volumes than that still paginates
// -- but it does make the common case one request.
const listPageLimit = 1000

// maxListPages bounds the cursor loop.
//
// A server that returned hasNextPage forever would otherwise spin here making
// requests until something else gave out. At a thousand volumes a page this
// ceiling is far beyond any real account, so hitting it means the pagination
// is broken rather than that somebody has a lot of storage -- which is worth
// an error naming that, rather than a listing silently missing the rest.
const maxListPages = 100

// StoreCreate makes a network volume.
//
// Only ever reached from `ember store create`. Nothing in the rent-a-node path
// calls this, and that is the point: a store is a standing monthly charge with
// no deadline, no reaper and no Ctrl-C that ends it, so it exists because
// somebody typed a command rather than because a config mentioned a name.
func (p *Provider) StoreCreate(ctx context.Context, name string, sizeGiB int, region string) (node.Store, error) {
	if err := provider.ValidateStoreName(name); err != nil {
		return node.Store{}, err
	}
	if sizeGiB < node.MinStoreGiB || sizeGiB > node.MaxStoreGiB {
		// Refused here rather than by the server. The bounds are the measured
		// ones, and a round trip to be told "minimum: got 1, want 10" teaches
		// the operator the same thing more slowly.
		return node.Store{}, fmt.Errorf("store size %d GiB is outside RunPod's range of %d to %d",
			sizeGiB, node.MinStoreGiB, node.MaxStoreGiB)
	}
	if region == "" {
		// There is no sensible default. A volume lives in exactly one
		// datacentre and pins every pod that mounts it to that site, so
		// letting RunPod choose would mean ember picking where every future
		// run of this store must be placed, silently, on the operator's
		// behalf.
		return node.Store{}, fmt.Errorf("a store needs a datacentre: it lives in one site " +
			"and every node that mounts it must be placed there. Set store.region in ember.yaml")
	}

	body := createVolumeRequest{
		Name:       provider.StoreLabel(name),
		Size:       sizeGiB,
		DataCenter: region,
		Type:       volumeTypeStandard,
	}

	var created networkVolume
	if err := p.HTTP.Post(ctx, VolumePath, body, &created); err != nil {
		return node.Store{}, fmt.Errorf("create store %s: %w", name, err)
	}

	s, ours, err := mapVolume(created)
	if err != nil {
		return node.Store{}, err
	}
	if !ours {
		// The label went out and did not come back. Something renamed it, and
		// a store ember cannot recognise is a store `ember ls` will not report
		// and `ember store rm` will not remove -- a leak created by the very
		// command meant to be the careful one.
		return node.Store{}, fmt.Errorf("created store %q but RunPod reports its name as %q, "+
			"which ember cannot recognise as its own; remove it in the console", name, created.Name)
	}
	return s, nil
}

// StoreList returns every store ember owns at RunPod.
//
// Volumes ember did not create are skipped rather than reported. Same rule as
// nodes: somebody else's volume in the same account is not ember's to name,
// and certainly not ember's to offer to remove.
func (p *Provider) StoreList(ctx context.Context) ([]node.Store, error) {
	var out []node.Store
	cursor := ""

	for page := 0; ; page++ {
		if page >= maxListPages {
			return nil, fmt.Errorf("RunPod is still reporting more pages of volumes after %d requests; "+
				"ember stopped rather than loop, and the listing above it is incomplete", maxListPages)
		}

		q := url.Values{}
		q.Set("limit", fmt.Sprint(listPageLimit))
		if cursor != "" {
			q.Set("cursor", cursor)
		}

		var resp listVolumesResponse
		if err := p.HTTP.Get(ctx, VolumePath+"?"+q.Encode(), &resp); err != nil {
			return nil, fmt.Errorf("list stores: %w", err)
		}

		for _, v := range resp.NetworkVolumes {
			s, ours, err := mapVolume(v)
			if err != nil {
				return nil, err
			}
			if ours {
				out = append(out, s)
			}
		}

		// Both halves are required. hasNextPage without a cursor is a page
		// ember cannot ask for, and following a cursor while hasNextPage is
		// false would re-read the last page forever.
		if !resp.Pagination.HasNextPage || resp.Pagination.NextCursor == nil || *resp.Pagination.NextCursor == "" {
			return out, nil
		}
		cursor = *resp.Pagination.NextCursor
	}
}

// StoreDestroy removes a volume and everything on it.
//
// Not found counts as success, as it does for a node: the caller asked for the
// store to be gone and it is.
func (p *Provider) StoreDestroy(ctx context.Context, h node.StoreHandle) error {
	if h.ID == "" {
		return fmt.Errorf("store handle names no volume")
	}
	if err := p.HTTP.Delete(ctx, VolumePath+"/"+url.PathEscape(string(h.ID))); err != nil {
		return fmt.Errorf("destroy store %s: %w", h, err)
	}
	return nil
}

var _ provider.StoreAPI = (*Provider)(nil)
