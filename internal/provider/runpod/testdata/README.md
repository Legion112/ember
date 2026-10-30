# RunPod fixtures

These are pinned external inputs. A test maps each one and compares against an
expected ember value, so an upstream API change surfaces here as a test diff
rather than at runtime with a GPU attached.

## Where they came from, and what that means

`gpus.json` is **captured from the live API** on 2026-10-29: four real entries
from `GET /v2/catalog/gpus?include=AVAILABILITY&product=POD`, trimmed to the
ones that exercise the interesting cases. The prices and availability levels
in it are real.

The `pod_*.json` files are still built from RunPod's **published OpenAPI
schema** — field names, types, enum values and the documented examples — and
have not been checked against a live response.

That distinction is worth stating plainly, because it is the limit of what
these fixtures prove:

- Field **names, shapes and enum values** are as authoritative as the schema
  is. Those are what `types.go` and `map.go` are written against.
- Field **combinations** are ember's reading of the documentation, not an
  observation. The schema says `runtime` and `ssh.direct` are null when
  listing pods and until a public `22/tcp` mapping is assigned; the fixtures
  encode that, but nobody has watched it happen.
- Anything the schema does not describe is absent here. A real response may
  carry fields these do not.

So the mapping is tested, and for the `pod_*.json` files the *assumption that
these are what RunPod actually sends* is not. Replace each with a captured live
response: a fixture that changes shape when replaced is exactly the finding
worth having.

That is not hypothetical. Replacing `gpus.json` with a real response changed
two things at once. `dataCenters` turned out to be an array of objects rather
than of strings, which the live call simply refused to decode. And an RTX 4090
reported `availability: "NONE"` overall while its `US-IL-1` entry read `"LOW"`,
so the filter that dropped anything NONE would have had ember answer "no
capacity" for a card that was rentable. Neither was visible from the schema.

## Files

| file | what it is |
|---|---|
| `pod_running.json` | a ready pod: RUNNING, `ssh.direct` assigned, one public `22/tcp` |
| `pod_provisioning.json` | PROVISIONING, no runtime, no ssh |
| `pod_running_no_ssh.json` | RUNNING but no `ssh.direct` yet — the gap between status and reachability |
| `pod_exposed_http.json` | a pod with a public `8188/http` port, which must fail the exposure assertion |
| `pod_exited.json` | EXITED, which ember reads as failed |
| `pod_unknown_status.json` | a status ember does not recognise, which must be an error |
| `pods_list.json` | a list response: no runtime, no ssh, plus one pod that is not ember's |
| `gpus.json` | the GPU catalogue, including entries ember must filter out |

## Network volumes: what the live API answered, 2026-10-30

Captured before any client code was written, because the documentation and the
third-party clients disagreed and one free `GET` settles it. All of this is
from real responses, not from the docs.

**There are two APIs, and they are not the same.** Both answer:

| request | result |
|---|---|
| `GET api.runpod.io/v2/networkvolumes` | 404 |
| `GET api.runpod.io/v2/network-volumes` | 200 — note the hyphen |
| `GET rest.runpod.io/v1/networkvolumes` | 200 — no hyphen |

ember uses the **v2** one, on the same host it already uses for pods. That
removes the second `httpx.Client` the plan expected to need: `httpx.resolve`
refuses a path containing `://`, so two hosts would have meant two clients.

**The field is `dataCenter` on v2 and `dataCenterId` on v1.** A create with
`dataCenterId` against v2 is refused:

```
422 {"errors":["$: missing property 'dataCenter'",
               "$: additional properties 'dataCenterId' not allowed"]}
```

The published docs describe only the v1 spelling. A client written from them
would fail on its first create — after the operator had already decided to
spend money.

**The minimum size is 10 GiB, not 0.** The same 422 carries
`"$.size: minimum: got 1, want 10"`. The docs give the minimum as 0 and the
maximum as 4000; the schema says 10 and 4096. `node.MinStoreGiB` and
`MaxStoreGiB` hold the measured pair, so a size ember will refuse locally
costs nothing instead of a round trip.

**The list is paginated**, which the v1 bare array is not:

```json
{"networkVolumes":[], "pagination":{"nextCursor":null,"hasNextPage":false}}
```

A client reading only the first page under-reports stores — and under-reporting
a resource that bills monthly with nothing attached is precisely the leak
`ScanStores` exists to catch. The cursor is followed.

**Attachment is `mounts.network`, not `networkVolumeId`.** The pod create body
takes:

```json
{"mounts": {"network": [{"volumeId": "...", "path": "/models"}]}}
```

`maxItems` is 1 today. `path` has **no default — it must be specified**, so
ember choosing `/models` is not an override of `/workspace` but the only way
to answer the field at all.

**`persistent` and `network` are mutually exclusive**, enforced with a 400:
"upstream drops a persistent disk whenever a volume is attached". So a store
and a container volume disk cannot both be asked for.

**`volumeId` is immutable after create**; only `path` may be PATCHed, and
`network: []` is rejected rather than clearing the mount.

Two more facts worth having: `VolumeType` is `STANDARD | HIGH_PERFORMANCE`,
and there is a `GET /v2/billing/network-volumes` endpoint that reports what
was actually charged — which is a better source for the standing-charge line
than multiplying a published rate by a size.

Both OpenAPI documents are public and unauthenticated
(`api.runpod.io/v2/openapi.json`, `rest.runpod.io/v1/openapi.json`), which is
where the schema details above came from.

### Volume fixtures

| file | what it is |
|---|---|
| `volumes_list.json` | three volumes: one of ember's, one that is **not** ember's and must be skipped, and a high-performance one at the bulk size so the rate is picked from the volume rather than assumed |
| `volume_created.json` | the 201 body from a create |

Both are built from the live v2 schema and the field names observed above
(`dataCenter`, not `dataCenterId`). The `pagination` object is real: it is what
the live `GET /v2/network-volumes` returned on an empty account.

### What the first live store run showed, 2026-10-30

Verified against a real volume and two real pods, for under a cent in total.

**Create, list and remove all work as mapped.** `POST /v2/network-volumes`
with `dataCenter` returned `72rbq2xyad` in EU-RO-1, the listing matched
ember's own report field for field, and `DELETE` removed it. The label
`ember-store-verify` round-tripped, so `ParseStoreLabel` recognises what
`StoreCreate` writes.

**The S3 upload lands real bytes.** SigV4 was accepted on the first attempt --
no 403 -- and a file pushed from the laptop was read back byte for byte by a
pod that mounted the volume. That is the whole upload path proven: signing,
single PUT, and the object actually existing.

**`mounts.network` is honoured, and the custom path with it.** The volume
appeared at `/models` and not at RunPod's default, backed by
`mfs#euro-2.runpod.net:9421` -- MooseFS, which matches what RunPod's own
engineering write-up says the network-volume tier runs on.

**Attaching a network volume removes `/workspace` entirely.** The schema says
`persistent` and `network` are mutually exclusive because "upstream drops a
persistent disk whenever a volume is attached"; what that means in practice is
that the directory is not there at all. A pod with a store has no
`/workspace` unless its image creates one. The workload images already do
(`RUN mkdir -p /workspace/out`); the bare node image did not, and a run that
asked for outputs from `/workspace/out` failed at the tar with
"Cannot open: No such file or directory".

That is a real interaction and not a documentation detail: every config that
mounts a store and writes outputs to `/workspace` depends on it.

### Network volume throughput, measured 2026-10-30

RunPod documents 200-400 MB/s for the standard tier. Measured on an RTX 4090
in EU-RO-1, writing and then reading a 2.21 GB checkpoint:

| | |
|---|---|
| write, 2.21 GB onto the volume | **2 s** |
| read, cold, on a *different* pod | **2.13 s** for 2.11 GB of parameters |

That is roughly **1.0 GB/s**, two and a half to five times the documented
figure. The read is sd-cli's own tensor-load accounting, on a node that had
never seen the file -- a second pod entirely, which is also the proof that
weights outlive the machine.

Two caveats worth keeping with the number. The file was written about a
minute earlier on the same storage cluster, so MooseFS may have served it
from its own cache; a read of something written last week could be slower.
And the figure this is usually compared against -- 56 MB/s of container pull
-- is still *inferred* from a single 62-second time-to-ready, never measured
on its own.

What it changes is less than it looks. For a 40 GB model set on an H100 the
pull is about 11.9 minutes either way, so dropping the volume read from a
documented 133 s to a measured 40 s moves the break-even from roughly six
runs a month to five. The conclusion was robust because the pull dominates,
which is worth knowing even when a measurement agrees with you.
