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
| `job_generating.json` | same | a preview frame: `pass`, `step`, `total_steps` and a base64 body |
| `job_completed.json` | same | `result.images[].b64_json`, the only place a finished image comes from |
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

The job fixtures are the other gap, and the bigger one. They were all written
from api.md and none has been seen on the wire: a queued job, a preview frame,
a completed result and a failed one. The orchestration has been driven against
a real server end to end — sixteen jobs, byte-identical output — so the shapes
are very likely right, but "very likely" is what `capabilities.model` was
before it turned out to be an object. Capture them on the next live run and
move their rows up.
