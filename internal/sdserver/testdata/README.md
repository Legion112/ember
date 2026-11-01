# sd-server fixtures, and which of them are evidence

The convention here is `internal/provider/runpod/testdata/README.md`'s: a
fixture captured from a running server is evidence about that server, and a
fixture built from documentation is a guess about it. The two are not
interchangeable and the difference is written down per file, because a test
that passes against a guess proves ember is self-consistent and nothing more.

That distinction has already cost this repository money once. RunPod's
published OpenAPI schema named `allowedCudaVersions`; the endpoint ember
actually creates pods on declares `unevaluatedProperties: false` and answers
422 to it. The lesson did not stick the first time because the field had been
read from a **schema** rather than from a **response**.

## Captured from a running sd-server

`capabilities.json` and `model_status_ready.json` came off a real
`sd-server` built from `workload-sd-cuda.Dockerfile` at `CUDA_ARCH=75`,
serving sd15 Q8_0 on an RTX 2080 Ti. They are evidence.

`job_generating.json` and `job_completed.json` were captured by the preview
lab (`make test-preview-lab`, `test/integration/preview_test.go`) from the CPU
image serving sd15 Q4_0, generating a 64x64 image in six steps with
`preview_interval` at 1. They are evidence, and they can be re-captured by
setting `EMBER_CAPTURE_DIR` on that lab.

What they corrected is smaller than what `capabilities.json` corrected, and
that is worth saying plainly: **every field ember reads was already right.**
`pass`, `step`, `total_steps` and `b64_json` are shaped as api.md implies, and
so is `result.images[].b64_json`. What the guesses had wrong was everything
around them -- a real response also carries `cancel_requested`,
`result.output_format` and a whole `result.images[].generation_info` object
echoing the parameters, and its timestamps are milliseconds where the guessed
fixtures used seconds. ember reads none of those, so nothing broke; the point
of replacing the files anyway is that the next person to add a field can see
what a response actually looks like rather than what a field list implies.

One thing only the capture shows: **a `proj` preview frame is the latent, not
the image.** The 64x64 generation produces 8x8 PNGs of about 250 bytes. Anyone
reading "preview frame" expects a thumbnail of the output and would size a UI
for one.

They also corrected the guesses, on the first run, after a node was already
up: **`capabilities.model` is an object** -- `{"name","path","stem"}` -- and
not the string api.md's field list implies, and `models/status` carries no
`revision`. The guessed fixture said `"model": "flux2-dev"` and decoded
happily, which is exactly how a doc-derived shape passes its own test and
fails the service. Everything else held: `samplers`, `schedulers`,
`preview_modes` and `limits` are shaped as documented, and `limits` carries
two upscale bounds ember does not read.

## Source: `examples/server/api.md` at stable-diffusion.cpp `77b72a2`

Every file in the table below was written from the documented field lists at
the revision both workload images pin, and **none of them has been seen on the
wire.** What they pin is that ember's wire structs decode the *documented*
shape, which is weaker than it sounds: a field the server spells differently,
omits, or sends as a different JSON type decodes to a zero value here and
passes. That is precisely what `capabilities.model` did.

| file | endpoint | what it is meant to exercise |
|---|---|---|
| `model_status_empty.json` | `GET /sdcpp/v1/models/status` | a server up with no weights, the state it reports before a load finishes |
| `model_status_loading.json` | same | the state `WaitModel` retries |
| `model_status_error.json` | same | the state it refuses on, with the server's own message |
| `job_accepted.json` | `POST /sdcpp/v1/img_gen` | the 202, and that the id is read from `id` rather than parsed back out of `poll_url` |
| `job_queued.json` | `GET /sdcpp/v1/jobs/{id}` | `queue_position` while queued |
| `job_failed.json` | same | the **object** form of `error` |
| `error_model_not_loaded.json` | model management | the **string** form of `error`, beside a separate `code` |

The last two are the point of having both. sd-server spells `error` two
different ways in one API — a string for model management, an object for a
failed job — so a client written against either one alone decodes the other as
nothing and reports a JSON complaint in place of the reason. `decodeAPIError`
reads both and these two files are what hold it to that.

## Replacing a guess with evidence

The three `model_status_*` files above are still written from the field list:
a load that is in progress, and one that failed, are both states a healthy
local run does not produce on demand. `model_status_error.json` is the one
most worth replacing, because its `error` is a free-text string from
somewhere in the loader and nothing has checked that it is a string in every
case.

`job_queued.json` and `job_failed.json` are the job-lifecycle guesses that
remain. Both are states a healthy run does not produce on demand: the queue is
empty because ember submits one job at a time on purpose, and a failure has to
be provoked. `job_failed.json` is the one that matters more, because it
carries the object form of `error` and nothing has confirmed that a real
failed job spells it that way.

Provoking one is cheap now that the preview lab exists — a job naming a
sampler the server does not know, or a width past `limits.max_width`. The
reason it is not done here is that `Validate` refuses both before they reach
the wire, which is the behaviour ember wants; reaching around it needs a
deliberate hole, and this package already decided once that an escape hatch
for a test is an escape hatch.
