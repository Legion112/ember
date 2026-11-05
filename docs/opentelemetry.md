# OpenTelemetry in ember

Why the otel API is already linked, why it stays a no-op, and the terms on
which an exporter could ever be adopted. Indexed from `AGENTS.md`.

  **OpenTelemetry is already linked, stays a no-op, and the SDK is not
  adopted.** Docker's client wraps its own transport in
  `otelhttp.NewTransport` unconditionally, so
  `internal/dockerengine/dockerclient` drags the otel API into every ember
  binary: **530** `opentelemetry` symbols in `bin/ember`, of which 155 are
  `otel/trace`, 70 `auto/sdk`, 49 `otelhttp` and 25 `otel/metric` -- and
  **0** from `otel/sdk`, 0 from `google.golang.org/grpc` and 0 from
  `go.opentelemetry.io/proto`. That zero is the whole decision: the API with
  no registered provider does nothing, so the conversation was never "add a
  dependency". `internal/logx`'s `TestNoTelemetrySDKRegistered` and
  `TestOTelStaysIndirect` keep it there, because a state that holds only
  while nobody writes one import is not a state anybody chose.

  Three builds of ember's own `main`, go1.27, same machine, differing only in
  what they register -- against *ember's* dependency set and not a trivial
  program's, because what is already linked is what makes the delta smaller
  than it looks:

  | build | binary | delta | new modules | gRPC symbols |
  |---|---|---|---|---|
  | ember as it ships | 22,834,226 | — | — | 0 |
  | + SDK, hand-written JSON-lines file exporter | 23,396,778 | **+0.54 MB, +2.5%** | **1** | 0 |
  | + SDK + `otlptracehttp` | 33,638,911 | **+10.3 MB, +47.3%** | **11** | 1,922 |

  The **local sink is nearly free and the OTLP one costs half the binary**,
  which is the split #33 predicted -- but *both* of its numbers came from a
  trivial program and are overstated, and its module claim is wrong in the
  direction that matters. "Promoting them adds no new module and no new
  `go.sum` entry" is true of `otel`, `otel/trace` and `otel/metric`, and false
  of `otlptracehttp`: `go mod why` lands on
  `github.com/docker/docker/client.test`, so it is pinned by somebody else's
  **test** binary, contributes 0 symbols to ember, and its own requirements
  were never in a build list -- `grpc`, `protobuf`, `proto/otlp`,
  `grpc-gateway/v2`, `genproto` twice, `x/net`, `x/text`, `cenkalti/backoff`
  and `otlptrace` all arrive with it. A module pinned by a dependency's test
  binary is not a module ember has already paid for. **A schema read in place
  of a response, one more time** -- the `allowedCudaVersions` and
  `capabilities.model` mistake, reached through a dependency graph instead of
  an API. (New-module counts are against `go mod tidy` on this tree rather
  than against the committed `go.mod`, which is not tidy; `go.sum` is left out
  of the table because every line either exporter adds is a test-only hash and
  the figure measures nothing.)

  **"Docker API spans for free" is true and worth about 0.2% of the question
  it is offered against.** `grep -rn 'ImagePull'` returns nothing: ember never
  pulls an image through the Docker API, so no span would cover the pull. The
  whole wrapped surface is 15 control-plane methods, measured over this
  daemon's unix socket at 200 calls each -- `/_ping` 68 µs mean,
  `/v1.45/info` 4.42 ms, `/v1.45/containers/json?all=1` 5.45 ms. One of
  ember's five outbound HTTP surfaces is instrumented; RunPod's API, S3, the
  asset fetch and sd-server all go through `internal/httpx`, which sets no
  `Transport` and is wrapped by nothing. So the free spans are tens of
  milliseconds of control plane out of the ~27 s of per-rental overhead they
  were offered to explain, and none of the expensive half.

  **Metrics, no, and that one is not close.** `ember spend` and
  `ember history` answer it out of SQLite over integer-microdollar columns, and
  the rule above already says the event stream is the truth and those columns
  a derived index. A metrics pipeline would be a *third* copy of numbers that
  have a source of truth, aggregated and sampled by design, in float64 where
  this program keeps money as `int64` -- which is `queue.Cost` at a larger
  scale, and `queue.Cost` had never been called by anything but its own test.

  **Traces, not yet, and the measurement that decides it is #35's.** What was
  wanted is where the per-rental overhead goes -- create, wait-for-ready,
  pull, store read, asset verify, model load -- and a span tree is genuinely
  the better *shape* for it. But a shape is only worth something to a viewer,
  and the sink that costs +2.5% has no viewer: a JSON-lines file of spans is
  read with `jq`, which is not better than reading `took=` off logfmt. The
  thing with a viewer is the +47% one. Meanwhile `--log-level debug` and the
  `run` key now exist, so a `took=` on a debug line at each of those stages
  *is* the measurement, filterable to one rental, for no bytes, no new module
  and no egress. The honest order is: take that measurement first, and come
  back for spans when a question survives it.

  **The egress rule, settled once for #33, #34 and #35: a telemetry
  destination is an allow-list, and the allow-list is loopback.** ember's
  posture sentence is about *inbound* -- `dockerclient` already records that
  "outbound is a different question", and ember already originates outbound to
  a provider's API, an asset URL, S3 and a node's sshd. What is new about an
  exporter is therefore not the connection but the **payload**: a span of a
  generation naturally carries the prompt, the seed and the output path, which
  is operator content, and the rule for operator content is an allow-list and
  never a deny-list -- `rejectSecrets` deliberately not scanning `cmd`/`cmds`
  because "a token of affection" is a legitimate prompt is the precedent for
  why. A collector on 127.0.0.1 is not a posture change at all and is where
  #34 independently arrived for the browser half. Anything else is a third
  party receiving operator content and needs its own written yes. So the terms
  of a future adoption, recorded here so they are not re-litigated: the local
  sink first, the destination loopback-only, span attributes by allow-list,
  costs on the wire as integers, and `Shutdown` on `context.WithoutCancel`
  with a **bounded** wait -- the `provision` destroy precedent for the first
  half and `runServed`'s bare `wait()`, which wedged for twenty-three minutes
  after SIGTERM, for why the bound is not optional.
