# Why each dependency is here

The argument for every module in `go.mod`'s direct list, and the
alternatives that were measured and refused. Indexed from `AGENTS.md`.

OpenTelemetry has its own page: [opentelemetry.md](opentelemetry.md).

- Prefer the standard library and the dependencies already here over new ones.
  The list is `golang.org/x/crypto`, `github.com/docker/docker`,
  `gopkg.in/yaml.v3`, `golang.org/x/tools` (the coverage gate, and since #165
  the wiring guard in `internal/cli`, which needs `go/packages` because its
  subject is which VARIABLE a struct field was handed and that is a question
  about `types.Object` identity rather than about a spelling) and
  `modernc.org/sqlite`. **Adding one needs a reason written down**, which is
  what the rest of this bullet is.

  **This list and `go.mod`'s direct requirements are checked against each
  other, by `make tidy-check` and the CI step that calls it.** They had
  drifted: `go build` and `go get` add a requirement without correcting its
  `// indirect` marker, so `yaml.v3`, `coder/websocket`, `modernc.org/sqlite`
  and `chromedp` were each recorded as something nobody asked for, while three
  of the four are argued for at length below. What made that worth a gate
  rather than a tidy is that `TestOTelStaysIndirect` reads the marker as
  evidence -- a seal whose one signal was known to be stale four lines away in
  the same file. `chromedp` is direct through `TestServe_UI` alone, behind
  `integration && imagelab`: `go mod tidy` considers every build
  configuration, so a tag-gated test import is a direct requirement, and
  promoting it changed no package in either the plain or the tagged build
  graph.

  `modernc.org/sqlite` is the queue and the generation history. ember had no
  state that outlives a process: `internal/statestore` is a journal of *live*
  nodes, deliberately empty after a clean run, which is the right shape for
  "what is billing right now" and the wrong one for "what was asked for, what
  came back, and what did it cost". A queue outlives the process that filled
  it, a request has a history worth keeping after its node is gone, and a
  reconnecting client has to be told what it missed from a cursor -- which a
  ring buffer cannot answer once it has wrapped.

  Rust lives in `web/` and nothing in `internal/` imports anything from it.
  `go build ./...` and `go test ./...` work on a clean clone with no Rust
  installed, because `internal/webapi/dist` carries a committed placeholder --
  `//go:embed` fails at *compile* time when its pattern matches nothing, so an
  empty directory there would make the Go toolchain depend on the Rust one.
  `Cargo.lock` is committed and `wasm-bindgen` is pinned to exactly the
  installed CLI, because a mismatch between the generated glue and the binary
  that processes it is a hard error; `make web` checks the two agree and names
  which to change, the shape `require-arch` already uses. The built `ember.js`
  and `ember_bg.wasm` are **not** committed: a wasm blob changes on every
  build and nobody can review one.

  `github.com/coder/websocket` is the web UI's progress stream. It has **zero
  dependencies of its own**, which is most of the argument, and it refuses a
  cross-origin handshake by default -- so the one thing an attacker's page
  could do with the socket is the one thing it already cannot. The alternative
  was long-polling with the standard library, which works and was the first
  plan; a socket is one connection per watched request instead of four
  requests a second, and the latency floor stops being a poll interval.

  It is also **the control channel to the worker on a rented node**, which is
  its second consumer and the reason it is in `cmd/embernode`'s dependency
  graph as well as `ember`'s. The alternative there was protobuf over Connect
  (#192), and the written reason against it is in
  [the control channel](threat-model/control-channel.md): Connect's
  bidirectional streaming needs HTTP/2, so over HTTP/1.1 -- the property #192
  recommends it for -- one full-duplex channel would be two calls; and the
  module that would pay is the node binary, which cannot be redeployed to a
  pod that is already running. Nothing here has measured Connect + protobuf,
  which #192 says itself, and measuring it means adding the dependency.

  It is the **pure-Go** driver and not `mattn/go-sqlite3`, because cgo would
  mean `CGO_ENABLED=0` no longer builds ember and `go test ./...` would need a
  C toolchain, which is the kind of requirement that turns "it just works on a
  clean clone" into a paragraph of instructions. `internal/genstore` puts it
  behind a narrow interface with a `RecordingStore` fake, as every other
  external system here is, so the engine is swappable and the layers above it
  are testable with no file on disk. Flat JSON files -- what `statestore`
  does -- were the alternative and would have meant hand-rolling the one
  operation the queue cannot do without: claim the next request atomically,
  in a single statement.
