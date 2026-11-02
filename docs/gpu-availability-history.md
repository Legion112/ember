# Collecting GPU availability over time

**Status: implemented, as `ember catalogue snapshot` and `ember catalogue
report`. See README. This note is kept because it is the argument the
implementation follows rather than a record of a decision; where the two
differ, the differences are listed at the bottom.**

## The question

RunPod reports whether a GPU is available right now. Does it report what was
available last week?

**No.** Checked against both live OpenAPI documents on 2026-10-30:

| API | paths | anything historical |
|---|---|---|
| `api.runpod.io/v2` | 44 | none |
| `rest.runpod.io/v1` | 23 | none |

`availability` is a bare enum — `HIGH`, `MEDIUM`, `LOW`, `NONE` — on
`GpuType`, `CpuType` and `DataCenterAvailability`. It carries no timestamp, no
window, and no history. Every read is a point sample, and nothing on the
provider's side remembers the previous one.

So if ember wants to know how capacity behaves over time, ember has to keep
the samples.

## Why it is worth keeping, and why it became worth keeping *now*

Two observations.

**Availability moves on the scale of a day.** The committed fixture
`internal/provider/runpod/testdata/gpus.json`, captured 2026-10-29, has the
RTX 4090 at `NONE` overall with `US-IL-1` at `LOW`. A live read on 2026-10-30
showed the same card at `HIGH` in `EU-RO-1`. One day apart, a different
answer, and not a small one — `NONE` means you cannot rent it at all.

**A store turns the datacentre into a commitment.** Before stores, the
datacentre was a per-run detail: ember took whatever had capacity and the
question never came up twice. A network volume lives in exactly one site and
pins every node that mounts it there, and `volumeId` is immutable after
create — so choosing a site is choosing where every future run of that store
is placed, for as long as the store exists. Moving later means creating a
second store somewhere else and re-uploading the weights.

That is the change that makes this data worth having. One-off, the question
is "where can I rent a 4090 today"; with a store it is "which site will
reliably have a 4090 for the next six months", and a single sample cannot
answer it. The sample we happened to take could be the good day or the bad
one, and there is no way to tell which from inside it.

The constraint narrows further than it first looks. Of RunPod's 23
volume-capable datacentres, the S3 API serves only 15, and the intersection
is **10**. Pick outside those and weights can only be uploaded by renting
something to download them — so the real choice is across ten sites, and
their capacity is exactly what nobody has a record of.

## What to collect

The catalogue endpoint already returns everything, for free and with no
compute running:

```
GET /v2/catalog/gpus?include=AVAILABILITY&product=POD
```

One row per `(observed_at, gpu_id, datacentre)`:

| field | source | why |
|---|---|---|
| `observed_at` | the collector | the only field RunPod does not provide, and the whole point |
| `gpu_id` | `gpus[].id` | `"NVIDIA GeForce RTX 4090"` |
| `datacentre` | `gpus[].dataCenters[].id` | per-site, never the global summary — see below |
| `availability` | `gpus[].dataCenters[].availability` | the measurement |
| `overall` | `gpus[].availability` | kept to show how often it disagrees with every site |
| `price_secure`, `price_community` | `gpus[].price` | prices move too, and nobody records that either |
| `max_count` | `gpus[].maxCount` | how many of one card a single pod may have |
| `cloud` | derived | `secure` and `community` have separate capacity |

**Per-datacentre, not the summary.** The global `availability` field reads
`NONE` while an individual site still has capacity — that is a real
observation from the live catalogue, and it is why `hasCapacity` exists in
`map.go`. A collector that stored only the summary would record "unavailable"
for cards that were rentable all along, which is worse than no data: it is
data that looks like an answer.

CPU types carry the same field and would be collected the same way. They
matter for the CPU-pod population path, where the question is whether a cheap
CPU instance is reliably available in the store's own datacentre.

## How, if it is built

- **A Go subcommand**, `ember catalogue snapshot`, following the project rule
  that non-trivial logic is Go and never a shell script. `make setup` already
  calls `ember setup` for the same reason.
- **Scheduled by something outside ember** — a systemd timer or cron entry
  invoking the binary. ember does not become a daemon for this; a process
  that must stay running to collect data is a process that will be found
  stopped.
- **Append-only, one file per day**, JSON lines under the state directory.
  Append-only because the value is entirely in the series: a format that can
  rewrite history is one that can lose it to a bug in the writer.
- **Hourly is plenty.** Capacity is reported as four levels, not a number;
  sampling faster than it can visibly change buys nothing and makes a year of
  data large enough to need thinking about.
- **No credentials beyond the existing API key**, no pod, no GPU. The
  collector costs nothing to run, which is the main argument for starting
  early: the data cannot be collected retroactively, and a year of it is
  worth more than the hour it takes to write.

## What it would answer

- Which of the ten store-capable datacentres actually has a 4090 most of the
  time, rather than which had one on the afternoon somebody looked.
- Whether `NONE` on a card means "sold out for an hour" or "sold out for a
  week" — the difference between waiting and moving the store.
- Whether prices drift, and whether the cheap sites are the ones that are
  always empty.
- Whether a card worth buying is one that is persistently unrentable, which
  is the rent-versus-own question this project exists to answer, asked from
  the supply side instead of the cost side.

## Why not now

Nothing in ember needs it to work, and a collector with three days of data
answers nothing a single live query does not. The cost of starting late is
only that the series starts late — and the cost of building it now is a
feature nobody is using while the store path is unfinished.

The argument for starting *soon*, rather than never: this is the only kind of
data that cannot be backfilled. Every day without a collector is a day that
cannot be recovered later, and the decision it informs — which datacentre
holds the weights — gets harder to change the longer the store exists.

That argument won. The store path is further along than it was when the
paragraph above was written — `store create`, `rm`, `push`, `sync`, `ls`,
`verify` and `abort` all exist and a live store run happened — so one of its
two clauses weakened and the backfill argument did not. The report says the
objection out loud every time it runs: a series under a week prints *"closer
to a point sample than to a history"*, which is this section rendered as
output rather than left in a document nobody re-reads.

## Where the implementation differs from the note above

Four places, each because writing the code answered a question the note did
not ask.

**`cloud` is not a column; two tiers are.** The table above derives a `cloud`
field because "secure and community have separate capacity". They have
separate *prices*, separate *per-pod ceilings* and separate "is it offered at
all" flags — but **availability is one level per site, not one per tier**,
in both the captured response and the live v2 schema. One row per cloud would
have duplicated a single observation into two rows and made the series look
like twice the measurement it is. So a row carries `secure` and `community`
as nested tiers, each with its own quote and ceiling, and one `availability`.

**A type with no datacentre still gets a row.** Three of the four entries in
the captured catalogue list no `dataCenters` array at all — the field is not
required by the schema — and every one of those reads `NONE` overall. Skipping
them would make a card that is sold out everywhere indistinguishable from a
card that was never offered, and *"is this card persistently unrentable"* is
one of the four questions this file exists to answer. Such a row has an empty
`datacentre`, its `availability` stays **unreported** rather than being filled
in from the summary, and the report marks it with a `*` and a footnote so a
summary is never read as a site.

**"Not reported" is a third state, beside NONE and a level.** The note treats
availability as four levels. An absent field is a fifth answer and it is not
`NONE`: one is the provider saying there is nothing, the other is the provider
not saying. They are counted in separate columns, and a site that only ever
reported nothing shows `-` for its rentable share rather than 0%.

**CPU types are collected in the same file and the same shape**, with
`kind: cpu` and the price per vCPU per hour, which is what the endpoint
quotes. There is no `price_unit` column: `kind` already decides it, and a
second field saying the same thing is a second thing that can disagree.

## What was checked for free while building it, 2026-11-02

`api.runpod.io/v2/openapi.json` is public and unauthenticated, so this cost
nothing and confirmed three things the code depends on:

- `AvailabilityLevel` enumerates exactly `NONE`, `LOW`, `MEDIUM`, `HIGH`.
  Which is why an unrecognised level is **kept verbatim and ranked between
  NONE and MEDIUM** rather than refused: an enum is a promise about today and
  this file is read years later.
- `availability` and `dataCenters` are **not** in `GpuType`'s or `CpuType`'s
  required list, so the two absences above are the schema's own position and
  not just an artefact of one capture.
- `include=AVAILABILITY` *"requires `product`"*, in the endpoint's own
  description. ember spells both in one constant per catalogue rather than
  building them at three call sites.

And one thing worth writing down because it reads the other way: the schema's
`CudaVersionAvailability` description points at `gpu.allowedCudaVersions` *"on
pod create"*, while `POST /v2/pods` still declares no such property. That is
the field AGENTS.md records as having broken every create — the documentation
string has not caught up with the request body, which is the same lesson a
third time: a schema is not a response, and a description is not even a
schema.
