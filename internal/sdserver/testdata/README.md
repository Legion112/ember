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

`capabilities.json` is also where `features.cancel_generating` is read from.
The CPU build answers `true` for it too, which the cancel lab asserts against
a live `/capabilities` rather than against this file — the flag decides
whether ember asks a card that is already billing to stop, and reading a
committed fixture to answer that would be the `allowedCudaVersions` mistake
again.

**`capabilities_no_model.json` is the only response here that says no, and it
says two different kinds of no.** It is a server with no weights at all,
captured by the same load-failure lab. Every one of the twenty-seven
`generation_features` is `false`, because `model_generation_features` returns
its whole default table inverted when nothing is loaded — and `features` is
*not* a map with false values in it, it is a map with only
`cancel_generating` and `cancel_queued` in it, because `supported_modes` is
empty and the per-mode map is therefore never built. So one captured response
carries a published denial and a genuine silence side by side, which is the
evidence the pointers in `sdserver.Supported` are for: a bool would have
turned the silence into a denial and greyed a control against every build that
does not publish these maps. It is the fixture the whole capability gate is
tested against, and the only one that can drive its deny path — see the note
below on why no server ember starts reaches it.

**`job_completed_img2img.json` is the evidence for the one image-dependent
parameter that ships, and nothing reads it.** It is a 64x64 six-step
generation from a hand-built 344-byte PNG at `strength: 0.3`, captured from
the CPU build the same way the files above were. What it is for is the field
`generation_info.strength`: it comes back as `0.30000001192092896`, which is
the only published evidence anywhere that `strength` reached the generation at
all — and the same probe with **no** `init_image` returns a completed job
whose `generation_info` carries no `strength` key whatsoever. Accepted,
dropped, reported nowhere. That asymmetry is why ember refuses a strength
without an image and why four of the five image-dependent parameters are still
absent: `img_cfg` sent at `0`, `-1` and `99` is echoed at none of them, so
there is no response that could tell an operator whether it did anything.

It is also what a request body ember does not send looks like from the other
side — `generation_info` echoes the parameters, which is the closest thing
sd-server has to an acknowledgement, and the next person adding one of those
four needs to see which fields appear there and which do not. ember reads none
of `generation_info`, so this fixture is read by nothing, which is the same
decision `capabilities_loras.json` records below.

**`capabilities_loras.json` is a capture with nothing reading it yet, on
purpose.** It is a CPU server started with `--lora-model-dir`, so `loras[]` is
non-empty: `{"name":"real","path":"real.safetensors","display_path":
"real.safetensors"}`. ember has no LoRA field and this change deliberately did
not add one — the deliverable was the answer about `..`, which is in
`AGENTS.md`. What the file is for is the next person: the `path` here is
exactly the string `get_lora_full_path` matches on, so it shows what a
`Capabilities.Loras` allow-list would be checking against, captured before the
client code exists. That is the habit
`internal/provider/runpod/testdata/README.md` set and the reason the volume
API's two disagreeing field names were known before anything called them.

**`job_lora_not_loaded_completed.json` is the fixture that says nothing, and
that is what it is evidence of.** It is a completed `img_gen` job whose
request named a LoRA the server published and could not read -- a
`.safetensors` file whose contents are the line `not a lora`, which
`is_supported_model_ext` accepts and `ModelLoader::add_file` then answers
`unknown format` for. Captured by
`TestPreview_ALoraThatDidNotLoadIsNowhereInTheResponse` in the preview lab
(`make test-preview-lab`, `EMBER_CAPTURE_DIR` set), from the CPU image serving
sd15 Q4_0 at 64x64 in six steps.

Read it beside `job_completed.json` and nothing distinguishes them:
`"status": "completed"`, `"error": null`, one image, `cancel_requested` false.
The only mention of a LoRA anywhere in it is
`result.images[0].generation_info.loras[0]`, which carries
`{"is_high_noise":false,"multiplier":1,"path":"unloadable.safetensors"}` --
and `generation_info.cpp:121` builds that array from the request's own
`sd_img_gen_params_t`, before sampling starts. So the one field that names the
LoRA asserts it was applied, on a response about a generation that did not
apply it. The server's own account of what happened exists only on its stderr,
as two lines ember passes through as `KindLog` and records nowhere:

```
[W] unknown format /tmp/ember-lab-deadlora/unloadable.safetensors --- model_loader.cpp:232
[W] cannot register LoRA source '/tmp/ember-lab-deadlora/unloadable.safetensors' --- model_manager.cpp:122
```

**The image is the no-LoRA image, measured.** The same lab generates the same
prompt and seed with no `lora` key at all and compares the decoded pixels: 0
of 4096 differ. The PNGs are *not* byte-identical -- 10,261 against 10,156 --
and the difference is a `tEXt` chunk under the key `parameters` in which the
encoder writes `a paper boat <lora:unloadable:1.0>`, the A1111 spelling every
gallery reads. So the artifact on disk outlives the run carrying the same
claim the response made. A comparison done on bytes rather than on pixels
would have reported that the LoRA changed something.

**There is no stricter mode to ask for**, which is why this is a fixture and
not a feature. `ModelManager::LoraSpec` has a `required` flag and
`model_manager.cpp:120` honours it -- a required source that will not register
fails the whole call instead of warning. Nothing on the wire sets it:
`parse_lora_json_field` at `examples/common/common.cpp:2088` reads `path`,
`multiplier` and `is_high_noise` and no fourth key, and `apply_loras` at
`diffusion_engine.cpp:1814` builds every spec from a request with `required`
left at its default `false`.

**`error_lora_not_found.json` is a third spelling of `error`.** A `lora[].path`
the server has not published is refused with
`{"error":"invalid generation parameters"}` — a string, like the
model-management form, but with **no `code` beside it**. `APIError.Error` has
a branch for a message with no code, and until this capture that branch was
written for a shape nobody had observed.

`job_generating.json` and `job_completed.json` were captured by the preview
lab (`make test-preview-lab`, `test/integration/preview_test.go`) from the CPU
image serving sd15 Q4_0, generating a 64x64 image in six steps with
`preview_interval` at 1. They are evidence, and they can be re-captured by
setting `EMBER_CAPTURE_DIR` on that lab.

`job_cancelled.json` was captured by the same lab
(`TestPreview_ARealServerStopsAGenerationWhenAsked`) from a sixty-step
generation stopped at step 2. It is evidence, and it is the most expensive
thing any fixture here has corrected.

`job_failed.json`, `model_status_error.json`, `model_status_empty.json` and
`error_model_not_loaded.json` were captured by the two failure labs in the
same file — `TestPreview_AFailedJobCarriesTheObjectFormOfError` and
`TestPreview_AModelThatCannotLoadIsAnErrorStateAndNotASilence`. They are the
four that decide what an operator reads when a run goes wrong, and all four
were guesses until this. Nothing in them is machine-specific: the only paths
are relative model ids under a model root the lab itself names, and the
timestamps are the server's own milliseconds.

**A failed job says less than the guess did, and the gap is the finding.** The
guessed fixture carried `{"code":"invalid_request","message":"width must be a
multiple of 16"}` and both halves were invented — `invalid_request` appears
nowhere in the server binary, and a width it does not like is *rounded* rather
than refused. A real failure says `{"code":"generation_failed","message":
"generate_image returned no results"}`, which is the generic answer: the
reason was `VAE tile size must be at least 32 image pixels on this axis`, and
that sentence exists only in the server's own log, which ember passes through
as `KindLog`. The same thing the cancel capture showed from the other side —
a client that read the outcome out of the log would call a cancel a crash, and
one that reads only the API's `message` cannot say why a generation failed.
What the object form *does* carry is the combination: an error object beside
`status: "failed"`, `result: null`, `cancel_requested: false` and a `started`
the guess did not have.

**`load_elapsed_seconds` is `null` on a server with no weights**, where the
guessed `model_status_empty.json` said `0`. It decodes into ember's `float64`
as a no-op rather than an error, so the guess passed for a reason that had
nothing to do with being right — the same null-is-not-zero asymmetry that took
a websocket envelope down in `internal/webapi`, where serde's `default` covers
a field that is missing and not one that is present and null. A fixture that
passes by accident is the one worth replacing.

**The model-status `error` is a string, now checked rather than assumed.**
That is what this README said nobody had confirmed, and
`mapModelStatus` reads it as a `*string` while a job's `error` goes through
`decodeAPIError` because it is an object. A loader that spelled it the job's
way would not print a worse message — the whole response would fail to decode
and ember would complain about Go structs. The lab asserts the JSON *type*
against the bytes, because a decoded struct cannot tell the two apart
afterwards. It is one build and one loader path; a third spelling from
somewhere else in the loader remains possible and is now a named risk rather
than an unexamined assumption.

**The string form was right, and that is recorded too.**
`error_model_not_loaded.json` had been written from api.md's field list as
`{"error":"No model is loaded","code":"model_not_loaded"}`, and a real 503
from `POST /img_gen` on a weightless server says exactly that. A convention
that only ever reports its own corrections stops being read.

**What was wrong was the code that read it.** `wireErrorEnvelope` was typed,
fixtured and decoded by *nothing but the test that unmarshalled the fixture
into it*: every non-2xx reached the operator as httpx's raw body, so the
reason arrived as JSON inside a sentence about a status code. `sdserver.refusal`
reads it on all five calls now, and the lab asserts against a live 503 that
the shape it reads is the shape a server sends. The third thing in this
repository found written, covered and connected to nothing; see `queue.Cost`.

**`state: "error"` is unreachable on ember's own startup path**, which is why
the lab provokes it through `POST /sdcpp/v1/models/load` — a route ember never
calls, because it hands the weights to sd-server as argv. A `--model` that is
not a model makes the process exit 1 *before it opens its port*, with `get sd
version from file failed` on stderr and nothing listening, which the lab also
asserts: that is the premise `orExited` was written on and nothing had checked
it. So the status this fixture holds is a state a server reports about a load
ember did not ask for; what ember reads from it — the field names, their JSON
types, the shape of the envelope beside them — is the same code either way.

**A cancelled job carries an error object.** `{"code":"cancelled","message":
"job cancelled by client"}` sits beside `"status": "cancelled"`, with `result`
null — and `app.runJobs` tested `State.Err` *before* the status, so every
successful cancel read as `sd-server refused it: cancelled: job cancelled by
client`. One layer up, a plain failure is what the drain loop destroys a
rental for: pressing the cancel button would have taken down the node and
every other request draining onto it, which is the exact outcome the feature
exists to avoid. The fake in `internal/app` answered a cancelled status with
no error at all, so the entire unit suite agreed with itself. Nothing but a
real server could have said otherwise.

One more thing only the capture shows, and it is why the status is read rather
than the log: the server writes `[E] Diffusion model sampling failed` and
`[E] sampling for image 1/1 failed after 0.51s` to its own stderr for a job it
reports as `cancelled`. Those lines reach the operator's terminal through
`KindLog`. A client that inferred the outcome from the output would call a
cancel a crash.

What the preview fixtures corrected is smaller than what `capabilities.json` corrected, and
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
`revision` on a command-line load. (It does carry one after a `models/load`,
as the captured error status shows, so "no `revision`" was true of the
response in front of us and not of the endpoint -- which is the narrower claim
a single capture can support.) The guessed fixture said `"model": "flux2-dev"` and decoded
happily, which is exactly how a doc-derived shape passes its own test and
fails the service. Everything else held: `samplers`, `schedulers`,
`preview_modes` and `limits` are shaped as documented, and `limits` carries
two upscale bounds ember does not read.

## Per file: captured or guessed

| file | endpoint | captured? | what it exercises |
|---|---|---|---|
| `capabilities.json` | `GET /sdcpp/v1/capabilities` | **captured** — CUDA build, sd15 Q8_0, RTX 2080 Ti | every allow-list `Validate` reads, `features.cancel_generating`, and both feature maps answering **true** |
| `capabilities_no_model.json` | same | **captured** — `make test-preview-lab`, a CPU server with no weights | every `generation_features` flag **false**, beside a `features` map with no `hires` key at all |
| `capabilities_loras.json` | same | **captured** — same lab, a CPU server started with `--lora-model-dir` | the shape of the published LoRA allow-list; read by no test yet, and the note above says why |
| `error_lora_not_found.json` | `POST /sdcpp/v1/img_gen` | **captured** — the LoRA lab, a `lora[].path` the server has not published (400) | the string form of `error` with **no `code`** beside it |
| `model_status_ready.json` | `GET /sdcpp/v1/models/status` | **captured** — same run | the state `WaitModel` stops on |
| `model_status_empty.json` | same | **captured** — `make test-preview-lab`, a CPU server started with `--model-dir` and no model | a server up with no weights, and `load_elapsed_seconds: null` |
| `model_status_error.json` | same | **captured** — same lab, after `models/load` on a file that is not a model | the state `WaitModel` refuses on, and that `error` is a JSON **string** |
| `model_status_loading.json` | same | **guessed** | the state `WaitModel` retries |
| `job_accepted.json` | `POST /sdcpp/v1/img_gen` | **guessed** | the 202, and that the id is read from `id` rather than parsed out of `poll_url` |
| `job_generating.json` | `GET /sdcpp/v1/jobs/{id}` | **captured** — preview lab, six steps at `preview_interval` 1 | a preview frame, its pass and its step |
| `job_completed.json` | same | **captured** — same run | the finished image |
| `job_completed_img2img.json` | same | **captured** — CPU build, 64x64 from a 344-byte PNG at `strength: 0.3` | that `generation_info` echoes `strength` when an init image was taken, and nothing when it was not; read by no test, and the note above says why |
| `job_cancelled.json` | same | **captured** — cancel lab, a sixty-step job stopped at step 2 | an error object beside a status that is **not** a failure |
| `job_lora_not_loaded_completed.json` | same | **captured** — the unloadable-LoRA lab, a published id whose file is not a model | a clean success carrying no account of a LoRA that never loaded, and the request echoed back as though it had |
| `job_failed.json` | same | **captured** — failure lab, vae tiling with a one-pixel tile | the **object** form of `error`, beside `result: null` |
| `job_queued.json` | same | **guessed** | `queue_position` while queued |
| `error_model_not_loaded.json` | any request the server refuses | **captured** — failure lab, `POST /img_gen` with no weights (503) | the **string** form of `error`, beside a separate `code` |

The last two shapes are the point of having both. sd-server spells `error` two
different ways in one API — a string beside a `code` for a request it rejects
outright, an object for a job that failed after being accepted — so a client
written against either one alone decodes the other as nothing and reports a
JSON complaint in place of the reason. `decodeAPIError` reads the object form
and `refusal` reads the string one, and both are now exercised against bytes a
server sent.

## What provoking a failure took, and what it did not take

**No bypass, and none was added.** `Validate` still refuses a sampler the
server does not list and a width past `limits.max_width`, which is why the
README used to say a failed job could not be captured: reaching around it
needs a deliberate hole, and this package already decided once that an escape
hatch for a test is an escape hatch.

The input that works is one **every check ember has accepts**: vae tiling
switched on with `tile_size_w` and `tile_size_h` of 1. `ValidateStatic`
refuses a negative tile and nothing else, because the server publishes no tile
bound in `limits`; `Validate` has no list to check it against. The server
samples the whole job and then fails in the decoder, where the tiles are used
— so a failed job is a job that has already been paid for, which is the
difference between it and a refusal.

**It is left representable on purpose.** This build's log names a minimum of
32 image pixels per axis; no *response* does. Refusing below it here would be
reading a bound out of an error string — the `allowedCudaVersions` mistake,
which is a schema or a log read in place of a published value — and it would
make the only honest route to a real failed job unreachable, trading evidence
for an invented limit. If upstream ever publishes the bound in `limits`,
`Validate` should refuse it there and this lab should change with it.

## The three that are still guesses, and what each would need

`model_status_loading.json` — **a model whose load outlasts a poll.** The
state is reachable only through `models/load`, since a `--model` load finishes
before the port opens, and sd15 loads in **93 ms** by mmap on this machine.
Catching it would mean polling inside a sub-100 ms window, which is a lab that
fails when the page cache is warm. What it needs is a model set large enough
that loading takes seconds — the FLUX.2 set, which is a rented node — or a
model root on deliberately slow storage. Not a bypass, and not a GPU: a slow
read.

`job_queued.json` — **a second job in flight, which ember never produces.**
`runJobs` submits one at a time on purpose, so sd-server's own queue is one
job deep by construction and `queue_position` is 0 in every response ember
will ever see. Capturing a non-zero one is three lines in a lab and would
describe a state that exists only if that invariant breaks; what the fixture
pins meanwhile is that ember decodes `queued` and prints the number without
reasoning about it.

`job_accepted.json` — **the 202's bytes, which `Submit` throws away.** ember
decodes the body, keeps `id` and discards the rest, so capturing this needs a
raw POST from the lab that creates a job nothing is going to collect. One
field in it is already known wrong: `created` is written as Unix **seconds**,
and the captured `job_completed.json` and `job_cancelled.json` both carry
**milliseconds** for the same field name on the same object. ember reads
neither it nor `poll_url`, which is why this one is last.
