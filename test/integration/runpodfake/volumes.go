package runpodfake

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// The network-volume half, and why it is backed by a directory.
//
// `ember store` has seven subcommands against RunPod and not one of them had
// ever run in a lab, because this package served no volume route: a request
// for `/v2/network-volumes` 404'd. Neither had the pre-flight that asks a
// store whether this run's weights are on it before a card is rented, for a
// sharper reason -- every store lab runs on the local provider, whose stores
// report no region, and `CheckStoreAssets` skips on an empty region by design.
// So the lab suite could not tell that check from its own absence.
//
// A DIRECTORY AND NOT A MAP OF NAMES TO SIZES. The point of the pre-flight is
// that the store's own endpoint and the node's own filesystem are the same
// place: the sync fills the store over SSH and the next plan asks the S3
// endpoint whether it is full. Two in-memory maps would agree with each other
// whatever either half did, which is the closed loop `capabilities.model` sat
// in -- so a volume here is one host directory, `Create` bind-mounts it into
// the container the way `mounts.network` attaches it on RunPod, and s3.go
// serves object calls by reading that same directory. What the node writes is
// what the endpoint lists, or the lab fails.
//
// It lives under the test's own temporary directory, which matters for a
// reason worth writing down: `embernode fetch` runs as root in the container,
// so the asset lands root-owned on this machine. Unlinking a file needs write
// permission on its *directory* rather than ownership of the file, and the
// directory belongs to the test process -- so the cleanup works for flat keys,
// which is every key ember writes here (`assets[].as` and a pushed file's base
// name). A nested key whose parent `embernode` created would be root-owned and
// would not come away, which is recorded rather than guarded against.

// volumePath is where this fake serves network volumes.
//
// Spelled here rather than imported from the provider's own VolumePath, for
// cpuMaxDiskGiB's reason: this is the fake's account of where the service
// answers and that constant is ember's account of where it must therefore
// ask. Sharing one would mean a lab could never catch ember asking the wrong
// path -- and it has one to catch, because `/v2/networkvolumes` is a 404 and
// `/v2/network-volumes` is the live route, which a client written from the
// published documentation discovers on its first create.
const volumePath = "/v2/network-volumes"

// VolumeDataCenter is the site every volume this fake creates reports, unless
// the create names another.
//
// It is one of the fifteen datacentres that have an S3 endpoint, because the
// region is also the signing region: `s3.EndpointFor` and `awssig.Sign` both
// read it, so a lab whose store is somewhere RunPod does not serve would be
// testing a hostname rather than a signature.
const VolumeDataCenter = "EU-RO-1"

// volume is one network volume, plus the directory standing in for its bytes.
type volume struct {
	ID         string
	Name       string
	Size       int
	DataCenter string
	Type       string

	// Dir is the host directory this volume's objects live in. It is what
	// Create bind-mounts into the node and what the S3 endpoint reads.
	Dir string
}

// StoreDir is the host directory backing one volume, by id.
//
// Exported so a lab can read what the node wrote without going through the S3
// endpoint. That is the join the pre-flight lab turns on: a listing and a file
// on disk are two different claims, and a fake that could only be asked
// through its own endpoint would let the two be one.
func (f *Fake) StoreDir(id string) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.volumes[id]
	if !ok {
		return "", false
	}
	return v.Dir, true
}

// Volumes returns the ids of every volume that has not been deleted.
func (f *Fake) Volumes() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.volumes))
	for id := range f.volumes {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// minVolumeGiB is the smallest volume the API will sell.
//
// Ten, which is the measured figure and not the documented one: the published
// docs give the minimum as 0 and a create with `size: 1` is refused with
// `"$.size: minimum: got 1, want 10"`. Spelled here rather than imported from
// node.MinStoreGiB for the reason cpuMaxDiskGiB is: this is the fake's account
// of what the service does and that constant is ember's account of what it must
// therefore ask for, and one constant shared between them would mean a lab
// about the refusal could never fail.
const minVolumeGiB = 10

// maxVolumeGiB is the other end of the same measured pair.
const maxVolumeGiB = 4096

// listVolumes answers GET /v2/network-volumes.
//
// One page, always, with `hasNextPage` false and `nextCursor` null -- which is
// what the live call returned on an account with no volumes. The cursor loop in
// the provider is NOT exercised by this: ember asks for a thousand volumes a
// page and no lab creates a thousand stores, so paginating here would be a
// branch nothing could reach. It is covered where it can be, against canned
// bodies in the provider's own tests.
func (f *Fake) listVolumes(w http.ResponseWriter) {
	f.mu.Lock()
	vs := make([]*volume, 0, len(f.volumes))
	for _, v := range f.volumes {
		vs = append(vs, v)
	}
	f.mu.Unlock()
	sort.Slice(vs, func(i, j int) bool { return vs[i].ID < vs[j].ID })

	out := make([]map[string]any, 0, len(vs))
	for _, v := range vs {
		out = append(out, renderVolume(v))
	}
	f.ok(w, map[string]any{
		"networkVolumes": out,
		"pagination":     map[string]any{"nextCursor": nil, "hasNextPage": false},
	})
}

// renderVolume is one volume in the v2 API's field names.
//
// `dataCenter`, with no `dataCenterId`. The two APIs disagree about this one
// field -- v1 spells it `dataCenterId` -- and a create against v2 carrying the
// v1 spelling is refused with a 422 naming both halves of the mistake. That
// disagreement stayed invisible while the only account of the shape was a
// fixture, which is why this spelling is asserted rather than assumed.
func renderVolume(v *volume) map[string]any {
	return map[string]any{
		"id":         v.ID,
		"name":       v.Name,
		"size":       v.Size,
		"dataCenter": v.DataCenter,
		"type":       v.Type,
	}
}

// createVolume answers POST /v2/network-volumes.
//
// The refusals are the live ones. A 422 with an `errors` array is what the API
// answers for a body it will not accept, and the two entries reproduced here
// are the two that were measured: the v1 field name, and a size below the
// minimum.
func (f *Fake) createVolume(w http.ResponseWriter, body map[string]any) {
	// The v1 spelling, refused by name. A client written from the published
	// documentation sends this and fails on its first create -- after the
	// operator has already decided to spend money -- so it is worth a fake
	// that can produce the failure rather than a comment saying it exists.
	if _, wrong := body["dataCenterId"]; wrong {
		f.failErrors(w, http.StatusUnprocessableEntity,
			"$: missing property 'dataCenter'",
			"$: additional properties 'dataCenterId' not allowed")
		return
	}
	site := str(body["dataCenter"])
	if site == "" {
		f.failErrors(w, http.StatusUnprocessableEntity, "$: missing property 'dataCenter'")
		return
	}

	size := int(num(body["size"]))
	if size < minVolumeGiB || size > maxVolumeGiB {
		// The live wording, which names both the figure sent and the bound --
		// the documented minimum is 0 and the server's is 10.
		f.failErrors(w, http.StatusUnprocessableEntity,
			fmt.Sprintf("$.size: minimum: got %d, want %d", size, minVolumeGiB))
		return
	}

	name := str(body["name"])
	if name == "" {
		f.failErrors(w, http.StatusUnprocessableEntity, "$: missing property 'name'")
		return
	}

	typ := str(body["type"])
	if typ == "" {
		typ = "STANDARD"
	}

	f.mu.Lock()
	f.nextVolume++
	id := fmt.Sprintf("fakevol%04d", f.nextVolume)
	dir := filepath.Join(f.volRoot, id)
	f.mu.Unlock()

	// 0o755 and owned by the test process. The container writes into it as
	// root, which is what RunPod's node does; what the mode buys is that the
	// test process can still list and unlink what arrived.
	if err := os.MkdirAll(dir, 0o755); err != nil { //nolint:gosec // a lab's own scratch directory, written into by a container running as root
		f.t.Fatalf("make the directory backing volume %s: %v", id, err)
	}

	v := &volume{ID: id, Name: name, Size: size, DataCenter: site, Type: typ, Dir: dir}
	f.mu.Lock()
	f.volumes[id] = v
	f.mu.Unlock()

	f.ok(w, renderVolume(v))
}

// destroyVolume answers DELETE /v2/network-volumes/{id}.
//
// Not-found is a 404, by analogy with the pod route rather than from an
// observation: nobody has deleted the same volume twice against the live API.
// It is the safe direction -- `httpx.Delete` turns a 404 into success, which is
// what `StoreDestroy` wants -- and it is the direction that cannot make a
// destroy-is-idempotent assertion pass without the not-found path being taken,
// which is the defect the pod route had.
func (f *Fake) destroyVolume(w http.ResponseWriter, id string) {
	f.mu.Lock()
	v, ok := f.volumes[id]
	if ok {
		delete(f.volumes, id)
	}
	f.mu.Unlock()

	if !ok {
		f.fail(w, http.StatusNotFound, "network volume not found")
		return
	}
	// The bytes go with it. A store that outlived its own removal would be the
	// leak `ember store rm` exists to end, reproduced in the fake.
	if err := os.RemoveAll(v.Dir); err != nil {
		f.t.Errorf("remove the directory backing volume %s: %v", id, err)
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{}`))
}

// failErrors is the API's 422 shape: a list of complaints rather than one
// sentence.
//
// Different from fail's envelope on purpose. A create that the schema rejects
// answers `{"errors":[...]}` and a route that does not exist answers
// `{"detail":...}`, and ember prints whichever body it got -- so a fake with
// one shape for both would make the error an operator reads unlike the one the
// service sends.
func (f *Fake) failErrors(w http.ResponseWriter, code int, msgs ...string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]any{"errors": msgs})
}

// mountsOf reads the storage a create body asks for.
//
// `mounts.network[]` with `volumeId` and `path`, which is the v2 spelling: a
// top-level `networkVolumeId` belongs to the other API. `path` has no default
// -- the API requires it -- so a mount with no path is refused here rather
// than quietly landing at RunPod's own /workspace, where ember writes outputs
// and a store would bill for every render.
func (f *Fake) mountsOf(body map[string]any) ([]Mount, error) {
	m, ok := body["mounts"].(map[string]any)
	if !ok {
		return nil, nil
	}
	entries, ok := m["network"].([]any)
	if !ok {
		return nil, nil
	}

	var out []Mount
	for _, e := range entries {
		entry, ok := e.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("mounts.network entry is not an object")
		}
		id, path := str(entry["volumeId"]), str(entry["path"])
		if id == "" {
			return nil, fmt.Errorf("mounts.network entry names no volumeId")
		}
		if path == "" {
			return nil, fmt.Errorf("mounts.network entry for %s names no path, which this API requires", id)
		}
		dir, found := f.StoreDir(id)
		if !found {
			return nil, fmt.Errorf("network volume %s does not exist", id)
		}
		out = append(out, Mount{Host: dir, Path: path})
	}
	return out, nil
}

// Mount is one network volume attached to a node.
//
// The data plane has to honour it, which is the whole reason this seam is
// wider than it was. A fake that accepted `mounts.network` and started a
// container with nothing at that path would be tidier than the service it
// stands in for: the directory really is there on RunPod, backed by MooseFS,
// and a workload that found it empty is the bug this exists to catch.
type Mount struct {
	// Host is the directory on this machine standing in for the volume.
	Host string

	// Path is where the node sees it, which is what `mounts.network[].path`
	// asked for -- never a default, because the API has none.
	Path string
}

// objectKeys lists the keys on one volume, sorted.
//
// A walk rather than a flat read, because a key may carry separators: `as:` is
// joined onto the mount path on the node, so `embernode fetch` can legitimately
// write `checkpoints/sd15.gguf`.
func (v *volume) objectKeys() ([]string, error) {
	var out []string
	err := filepath.WalkDir(v.Dir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, rerr := filepath.Rel(v.Dir, p)
		if rerr != nil {
			return rerr
		}
		out = append(out, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(out)
	return out, nil
}

// s3Time is the ISO8601 spelling S3 puts in a listing, which is what
// internal/s3's parseS3Time reads.
func s3Time(t time.Time) string { return t.UTC().Format(time.RFC3339) }

// trimKey turns a request path into a bucket and a key.
func splitBucketKey(p string) (bucket, key string) {
	p = strings.TrimPrefix(p, "/")
	if i := strings.IndexByte(p, '/'); i >= 0 {
		return p[:i], p[i+1:]
	}
	return p, ""
}
