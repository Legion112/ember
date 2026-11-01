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

## Source: `examples/server/api.md` at stable-diffusion.cpp `77b72a2`

Every file below was written from the documented field lists at the revision
both workload images pin. **None of them has been seen on the wire yet.** What
they pin is that ember's wire structs decode the documented shape, which is
weaker than it sounds: a field the server spells differently, omits, or sends
as a different JSON type would decode to a zero value here and pass.

| file | endpoint | what it is meant to exercise |
|---|---|---|
| `capabilities.json` | `GET /sdcpp/v1/capabilities` | the trimmed read: samplers, schedulers, preview modes, limits |
| `model_status_empty.json` | `GET /sdcpp/v1/models/status` | a server up with no weights — what `--diffusion-model` has not finished doing yet, and the state the image lab sees |
| `model_status_loading.json` | same | the state `WaitModel` retries |
| `model_status_ready.json` | same | the state it returns on, carrying `load_elapsed_seconds` |
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

`test/integration/sdserver_test.go` starts the published workload image, runs
`sd-server` in it with no model, and reads `/sdcpp/v1/capabilities` and
`/sdcpp/v1/models/status` over a real SSH connection. It needs no GPU and no
weights, which makes it the cheapest way to turn the first two rows of that
table into captured responses. When it runs, overwrite those two files from
what it saw and move their rows down to a "captured" section here, naming the
image digest.

The rest need a loaded model and therefore a rented card. Capture them on the
first live run rather than leaving them doc-derived forever, and record the
digest and the card alongside — a job response is the one place where what
upstream documents and what upstream sends have had no opportunity to be
checked against each other.
