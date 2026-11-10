# The web UI

The page split into routes, keyed lists that froze, and the controls that were
built, serialized, parsed and thrown away.

Back to [the threat model index](../threat-model.md) · [AGENTS.md](../../AGENTS.md)

- **The page became four pages, and the one thing that made it a slice rather
  than a file move is that a component is now UNMOUNTED for the first time in
  this program's life (#139 slice 3).** Four `<section class="panel">` shared
  one `<main>` and a `1.3fr 1fr` grid: the queue's six-column table got the
  narrow half -- 407px of a 1088px body, which is what #138 was about -- the
  watcher spanned both columns through a `.panel:nth-child(3)` selector that
  counted its siblings, and since the billing panel landed the `1fr` column
  beside it was EMPTY on every viewport over 60rem, which nothing had named.
  There is one panel per route now, so the grid is one column at every width
  and `nth-child`, `.panel.idle` and the empty half-row are all gone with the
  thing they were placing. Measured on the repaired tree: `/queue` is 1040px
  of a 1040px `<main>` against the 444px it used to get.

  **A HAND-ROLLED HISTORY ROUTER, and the precedent was in the file that would
  otherwise have changed.** `web/Cargo.toml` puts fetch and WebSocket in
  `web-sys` rather than a wrapper crate for *"about eighty lines in api.rs
  against another dependency tree"*, under a comment whose own words are
  "FEATURES AND NOT A CRATE, which is the whole reason web-sys is here rather
  than a wrapper" -- so `History` and `PopStateEvent` are two entries on that
  list against `leptos_router`, which is in no lockfile here and arrives with
  its own tree. **Declared although the build already compiled without them**,
  because cargo features are additive across the graph: `leptos` enables both
  for its own reasons, so `web/` was spending somebody else's feature
  selection, which is the `// indirect` marker read as evidence one ecosystem
  over.

  **WHAT IT COST IS MEASURED AND IS NOT THE FEATURES.** Release profile, same
  toolchain, one commit apart: **1,597,604 bytes of wasm before and 1,921,981
  after, +324,377 (+20.3%)**. It is not the three hundred lines of routing
  either -- it is that `App`'s `<main>` went from four statically typed
  children to a `move ||` match whose five arms each `into_any()`, so four
  large view types get a full `AnyView` vtable where the compiler used to see
  them in place. The trade is taken, because the alternative to a match over a
  route is not having pages, and the figure is written down in three places
  rather than left for somebody to find. `web/Cargo.toml`'s release-profile
  comment claiming `opt-level = "s"` plus lto "takes the wasm well under a
  megabyte" was already 50% wrong before this and is its own issue; this makes
  it worse rather than causes it.

  **AND A THIRD TREE SAYS THE ROUTER IS NOT WHAT COSTS IT**, which is the
  measurement to read before believing the figure above: this branch with the
  five-arm match replaced by static children -- `route.rs` and all four view
  modules still compiled and instantiated -- is **1,558,267 bytes, 39,337
  BELOW the base's 1,597,604** against the 1,921,981 it ships at, so the
  routing and the pages cost nothing net and the whole +324,377 is the
  `AnyView` erasure. Recorded because `+20.3%` is exactly the figure somebody
  will one day blame the hand-rolled router for and propose deleting it over,
  and the erasure is a property of matching on a route rather than of routing.

  **THE FAILURE MODE IS INVISIBLE BY CONSTRUCTION, so the assertion is a
  sentinel and the lab carries its own control.** A link the page does not
  intercept is a full document load: 1.9 MiB of wasm re-fetched under
  `Cache-Control: no-store`, the app mounted again -- and everything WORKS.
  The right page is on screen, no error is logged, and the only symptom is a
  second of latency. A resource count cannot see it either, because the
  timeline starts over with the document, so the probe is a value on `window`,
  which a new document is the only thing that clears. And a sentinel that
  nothing in the lab can clear is an assertion that cannot fail -- so the
  deep link at the end of `TestServe_UINavigatesWithoutReloadingTheApp` is a
  real typed address and the sentinel is asserted GONE after it, which is what
  makes the four `sameDocument` calls above it mean something.

  **`watching` IS DELETED AND THE URL IS THE AUTHORITY.** An
  `RwSignal<String>` in `App`, written by the queue and by a finished
  `queue()`, became a second writable carrier of a fact that is now in the
  path -- which is `queue.Cost`'s shape, and #139 named it in advance. So
  `Watch` is `RequestPage(id: String)`, created per request by the route
  match; the empty case goes with it, because an address naming no request is
  `Route::Unknown` and never reaches the component. The queue's id cell is a
  link rather than a button writing a signal, which is what makes a request's
  page an address somebody can open in a tab, copy, reload or send on.

  **THE HAZARD: A PENDING RE-OPEN OUTLIVING ITS OWNER, AND THE OLD GUARD WAS
  THE READ THAT WOULD FAULT.** `retry` schedules
  `set_timeout(move || connect(w, again), wait)` with a backoff to five
  seconds, and that timeout is held by the BROWSER -- not an arena entry, not
  an `on_cleanup`, nothing Leptos disposes can reach it. While the watcher was
  a static child of `App` that was never unmounted it always landed on live
  storage; under a router it lands on a disposed owner up to five seconds
  after the operator navigated away. Every signal and `StoredValue` in
  `Watcher` is an arena handle whose non-`try` read is an `expect`, and this
  crate is built `panic = "abort"`, so the first read is an `unreachable`
  trap: a `RuntimeError` out of a timer callback and a page that stops. The
  epoch counter each of those four functions opened with was not a substitute
  -- it was the read.

  **`Watcher::alive` IS `try_get_value`, AND IT IS ONE MECHANISM RATHER THAN
  TWO.** `set_timeout_with_handle` plus `on_cleanup` is what `Queue`'s poll
  does and is the obvious second half; it is deliberately not here, because
  `clearTimeout` cannot un-queue a callback the browser has already dispatched
  -- so it is not sufficient alone while this is, and a clear beside it would
  be a second path for one check whose other path is the live one. It would
  also have made neither half's exit code its own. **The three socket handlers
  get no guard and that is a measurement rather than an omission**: they are
  reached only through the `api::Socket` in `w.socket`, which is an arena entry
  disposal drops, and `Socket::drop` clears `onopen`, `onmessage` and
  `onclose` before closing -- so a disposed owner has no socket to call them,
  and a guard there would be a branch nothing can take.

  **AND THE EPOCH COUNTER IS DELETED, because the router made its condition
  unreachable.** It was bumped by the `Effect` that re-opened when `watching`
  changed; the component is created per id now, so there is one open per mount
  and `w.epoch.get_value() != epoch` could never be true. A guard whose
  condition cannot be taken is a guard nobody can show is right.

  **THE LAB MAKES THE BACKOFF WINDOW A WAIT RATHER THAN A RACE, and that is
  the whole of why it is reliable.** A drop with `ember serve` up is answered
  on the first attempt 250ms later, so "navigate inside the window" would be a
  coin flip; `breakSockets` points the page's own `WebSocket` constructor at a
  path that answers no upgrade, so every re-open fails, the backoff walks to
  its five-second cap, and twelve attempts is forty-three seconds of them --
  there is certainly a pending timeout at the moment of the navigation. The
  reinstated hazard is red at `error: Uncaught RuntimeError: unreachable`.

  **A RESTART USUALLY DOES NOT PRODUCE THE WINDOW, AND "CANNOT" IS WHAT THIS
  ENTRY SAID AND IS WRONG -- corrected here before it was merged, by the agent
  who measured it rather than by review of the sentence.** The design argument
  is sound: SIGINT ends the rental, the row is written terminal, and `retry`
  returns `None` for a terminal request, so there is nothing left to schedule.
  What makes it a race rather than an exclusion is DELIVERY -- the page only
  knows the request is terminal if the socket carried that view before it
  closed, and nothing orders those two. When it does not arrive, `retry` sees
  a request it still believes is live and schedules exactly the timeout this
  hazard is about. The evidence is sharp and belongs to **#185**'s pass: a
  reinstatement case whose patch was *a reworded comment* -- a negative
  control, meant to be caught by nothing -- went RED at the restart
  assertions. The window is not structurally excluded; it merely usually
  loses. So the constructor break is what makes the hazard lab deterministic
  rather than lucky, and the next person to read this must not drop it and
  lean on a restart.

  **AND THE WRITER IS `finish`, NOT `endIfUnfinished`**, which this entry also
  had wrong. `drain` calls `q.finish(req.ID, err)` with the error as it came
  back and only then wraps it as `request %s:` for its own return, so the
  reason recorded on that path carries NO prefix -- which is the evidence,
  because `endIfUnfinished` is the outer net for the paths `finish` never
  reached and a row it wrote would show one.

  **AND THE PAGE IS BEHAVING CORRECTLY THROUGHOUT, which is the half that
  makes this worth leaning on at all.** `reopen_after`'s terminal rule and
  `retry`'s `None if terminal` are doing what they were written to do, and the
  browser-side drop arm of the reconnect lab passed the whole way through
  #185's pass: the reconnect feature was never broken in production. So what
  the hazard lab rests on is a deliberate behaviour rather than an accident,
  and `TestServe_UIResumesAProgressStreamItLost`'s empty stream note is about
  the delivery race above and not about a page that stopped reconnecting.

  **AND `console.errors()` WOULD HAVE CAUGHT THIS ONE TOO, which corrects the
  reason rather than the listener.** This file records that `chromedp.Console`
  is blind to an uncaught throw, measured against the `Socket::drop` defect --
  and that defect is a JS-level throw out of wasm-bindgen's glue with no Rust
  panic behind it. A disposed-arena read IS a Rust panic, so `lib.rs`'s own
  hook writes `ember-web panicked: Tried to access a reactive value that has
  already been disposed` to the console before the abort, and `errors()`
  filters for exactly that word. Measured: **both fire.** The page's `error`
  handler stays the assertion because it is the one that sees the TRAP rather
  than the hook's account of it -- which is what a build whose panic hook had
  been changed would leave -- and because the sentence it prints names the
  mechanism. So the rule is narrower than it reads: the console is blind to a
  throw that is not a Rust panic, and a Rust panic in this crate is logged
  twice.

  **THE PASS FOUND TWO DEAD CONDITIONS IN `Route::parse`, which is what a
  reinstatement pass is for and is a finding about the change rather than
  about the suite.** It had a `trim_end_matches('/')` and an `!id.is_empty()`
  guard in front of its `match`, and removing EACH left every test in the
  module green: the one `filter(|s| !s.is_empty())` on the split had already
  answered both, since a leading slash, a trailing one and a doubled one are
  all empty segments and `/requests/` is therefore ONE segment. Both are gone
  and the tests stayed, because what they assert is the ANSWER rather than the
  mechanism -- and the mechanism they now rest on has an exit code: deleting
  the filter turns four of them red, and the obvious alternative
  implementation, `pathname.strip_prefix("/requests/")`, turns three red
  because it hands back `Some("")`.

  **THE SPLIT TOOK TWO OF #138'S OWN ASSERTIONS AND BOTH DELETIONS ARE
  MEASURED RATHER THAN ARGUED.**

  **The height bound first.** It was five rendered lines, and what made it a
  bound was the separation #138 measured: the same 170-character store refusal
  at NINE lines inside `td.status` at the 174px that cell got in the `1fr`
  half of the grid. On the queue's own page the table is 1003px and the cell
  is 62.5px tall -- and `colspan="1"` reinstated is **62.5px too**, because the
  status column simply takes 745px of the leftovers and the sentence wraps to
  the same three lines. So the bound could not fail over the one defect it
  exists for, which is worse than no check. The WIDTH assertion is red on that
  same patch at *"745px inside a 1003px table, 257px short"*, and it is the
  half that holds the `colspan` literal to the six cells beside it, so the
  height is a figure in the measurement log now rather than an assertion.

  **And `onTbody`/`markedRows`, which read `tbody.watching` -- the one
  group-level mark this page could produce, and the discriminator #138 earned
  when moving `class:watching` onto the `<tr>` left the lab green.**
  `class:running` has exactly the same shape and is the only mark left, and
  that lab cannot produce it: `list.running` is the queue's live rental, read
  from the API rather than a column, so it cannot be seeded and the lab is
  deliberately about a request that never reached a provider. The obvious
  replacement was the weaker "nothing marks a `<tr>`", and **measured, with
  `class:running` moved onto the row that count is still ZERO** -- a check
  that reads as protection and reports nothing, which is `AssertNoStray`. Both
  fields are deleted and the gap is written where they were.

  **What survives has its own exit code and the two cases discriminate.** One
  `<tbody>` per request and the reason inside the group that holds its request
  need no mark: reinstated as loose `<tr>`s the pair is red with nothing else
  firing -- Leptos builds this DOM with `createElement`, so there is no HTML
  parser to insert a `<tbody>` and a dropped group really is absent -- while a
  reason given a `<tbody>` of its own is red at `whyOf`'s `following-sibling`
  instead. Two defects, two fixes, two failures. It is the
  `TestConformance_CannotCatchARederivedRunID` arrangement for the mark: the
  suite states what it cannot see, so whoever next moves one in that table
  decides again rather than trusting a green run.
  **And the precondition changed subject with them.** It was "the queue panel
  is narrower than the one the stylesheet makes span both columns", which was
  what stopped the height bound being satisfied by a full-width panel -- and
  there is no such grid and no such bound. What it asserts now is the SPLIT'S
  OWN CLAIM, that the queue has the page: a `Fatalf`, because the width
  measurement under it is about a table in that panel, and the thing it refuses
  is somebody putting a second panel back on this route. **`<main>` and not
  `<body>`**, because the body carries the page's 1.5rem padding, so a panel
  filling its grid is 48px inside it at any width -- a comparison against a
  constant, which the first run printed as a 48px failure.

  **THE ELEVEN LABS WERE THE REAL COST AND IT FAILED LOUDLY, as #139
  predicted.** Each found its panel by heading, which survives a panel moving
  file and does not survive the panel not being on `/`, so **five** needed a
  navigation step: the cost lab, the reason lab and the cancel lab to the
  queue, the billing lab to its own page, and the reconnect lab twice round a
  loop. The cancel lab needs two, and they are two different mechanisms --
  a header link to the queue, and the BACK BUTTON after the row's link, because
  the cancel button is on the queue and the evidence that a job started is on
  the request's page. `navTo` clicks the header link
  rather than navigating, because a `chromedp.Navigate` is a document load and
  would make every lab after it an assertion about a fresh page -- and a page
  whose links were never intercepted would pass all of them. **`rowOf` was
  `td/button` and is `td/a`**, which the first run of the new routing lab is
  what said: the row was on screen, the selector matched nothing, and the
  failure printed the row it could not find.
  **One lab's wait was a match on the stylesheet**, found the same way: `h2`
  carries `text-transform: uppercase` and `innerText` is what is RENDERED, so
  an assertion looking for "there is no page here" was handed "THERE IS NO
  PAGE HERE".

  **AND THE ROUTER MADE `location.pathname` AN INPUT TO A QUERY STRING, which
  is the one correctness defect this slice introduced away from the watcher.**
  `api::Socket::open` builds `ws://{host}/ws?id={id}&after={after}`, and for
  the life of that function its one caller passed a `watching` signal written
  from the server's own listing -- sixteen hex characters. The id comes off the
  path now, so `/requests/x&after=99` is an address an operator can type, and
  unescaped it sends `?id=x&after=99&after=0`: Go's `Query().Get` takes the
  FIRST value of a duplicated key, so the watcher resumes request `x` from a
  cursor nobody chose. No money and no security -- same origin, the cookie,
  loopback, and the operator doing it to themselves -- but it is
  `escapes_what_would_break_a_query`'s own rule, which this repository already
  wrote down for a card filter where the consequence was "the filter silently
  matched something else". So `urlish` **moved into `api`**, where both callers
  now are, rather than being copied: two spellings of one escaping rule is two
  places for one to drift, and a copy in `api` reading one in `views` would
  have the layering backwards besides. For the id ember actually sends it is
  the identity, which is exactly why nothing would have reported it; the test
  is a cargo one and patching `urlish` to `s.to_string()` turns two red.

  **THE EDITOR RACING THE HARNESS, which is the stale-backup shape met from
  the other side: the harness racing itself is what the one-backup-per-path
  rule already fixed, and this is something else writing the tree under it.**
  No ordinal, and that is the file's own rule rather than modesty: the
  sequence above counts to a sixth and then has an unnumbered shape after it,
  because #186's entry dropped its own number -- so a seventh here would be
  wrong, and this count has now been wrong three times. The NAME is the part
  worth keeping.
  The pass takes one backup per path before anything is patched, which is the
  fix for a half-patched file being backed up mid-case -- and it assumes
  nothing else writes the tree. Three files were edited while it ran, so its
  restore put the pre-edit copies back and two comment rewrites were silently
  gone; a fourth file was left half-patched when the pass was stopped, with a
  `REINSTATED` marker still in it. Nothing in the pass could report either:
  every case's own verdict was correct, and `pristine()` compares against the
  backup the edits were made after. What caught it is the tooling's own
  notice that a file had changed on disk, and what closes it is the rule
  rather than a check: **while a reinstatement pass is running the tree is the
  harness's, and an edit waits.** Recovered by stopping the pass, restoring
  the half-patched file, re-applying the two rewrites, re-taking the backup
  and resuming from the case that was interrupted -- then grepping the whole
  tree for `REINSTATED` before believing anything.

  **Three gaps are named rather than left.** A composed batch does not survive
  leaving the compose page -- the draft and the batch live in that component,
  #139 puts the lift in slice 4, and the symptom is a form that comes back
  empty, so `README.md` says so. A request page re-opens its stream from
  cursor zero, so coming back to a long generation replays its tail: nothing
  is wrong, because `Progress::apply` is idempotent per sequence number, and
  it is rows over loopback rather than anything billed. And `#183` -- the
  provider `<select>`'s keyed `<For>` reading `config.get()` in the row body
  -- moved file with no change at all: its semantics are exactly as they were,
  which is deliberate, because half-moving a filed defect is how a reader
  comes to believe it was fixed.
- **`report.KindRunFinished` and `KindRunFailed` are deleted, and the test is
  whether a second live mechanism exists.** Both were declared, both were in
  `Kinds()`, both were rendered as deliberately silent by `Text`, and
  `KindRunFailed` was in the queue's `urgent` list -- the one reserved for
  events that "would be lost rather than late". **Nothing ever emitted
  either**, so that list named an event that never arrived, and `report.go`'s
  comment claimed they "end a run" while nothing in the program ended one by
  saying so. One more of the things found here written, listed, covered and
  connected to nothing -- deliberately not given a number, because the running
  count above reads third, fourth and sixth and no entry claims the fifth.
  Deleted and not wired up, which is the opposite of the `sdserver.Client.Cancel`
  call and the same as `queue.Cost`: for Cancel there was no second mechanism,
  so deleting it would have deleted the mechanism, and here the fact is
  `requests.status` and `requests.err` -- columns, already on every route and
  the socket. An event for the same fact is the "two carriers of one figure"
  mistake, and it is the worse carrier of the two, because current state
  survives a reconnect with no backlog to replay and an event has to be folded
  from one. The only thing a column cannot say is "the stream is over", which
  is all `KindRunFinished` could have meant, and a terminal status answers it.
  `TestText_DecidesAboutEveryKind` walks `report.go` for declared constants, so
  re-adding one without rendering it fails -- checked by doing exactly that.
- **The socket's own comment claimed it sent the request "every time something
  changed" and it sent it every time an EVENT changed.** `send` returned early
  on `len(es) == 0 && !force`, which is right for every state that emits -- and
  the one state that does not is the one this is about: `queue.fail` writes the
  row for a request refused before a provider was reached and emits nothing,
  because there is no sink for a rental that was never opened. So a tab already
  watching such a request sat on the state the socket sent when it connected,
  with the reason in the database and on the wire type, for as long as it stayed
  open. The queue table never had the gap, because it polls -- which is why
  #103's "the watcher and the queue row can both read it without touching Go"
  was half right, and the half that was wrong is a `bytes.Equal` on the
  marshalled view. **The marshalled view and not a few chosen fields**, so the
  comparison cannot come to disagree with what the client is sent: a field
  added later is in it by construction. Both directions are live --
  `TestWS_SaysNothingWhileNothingHappens` is what stops the fix being an
  unconditional re-send four times a second, which would hide the difference
  between "no progress" and "ember stopped".

- **A caption that is true of one mode is wrong for the others.** The preview
  figure said "the latent, not the output" unconditionally. That is right for
  `proj`, which projects the latent and previews a 512x512 generation as a
  64x64 smear, and wrong for `tae` and `vae`, which decode a real image every
  frame -- in the direction that makes a working preview look broken.
- **A keyed list only re-renders when the key changes.** The progress rows
  were a Leptos `<For>` keyed on the job's index -- which is exactly what a
  key should be, stable -- so a row rendered once when the job appeared and
  then froze: the step never advanced on screen while it advanced in the
  database. Re-rendering a handful of rows on each event costs nothing; a
  frozen progress display costs the operator their belief that ember is
  working. Only a browser could find this, which is the argument for the
  chromedp lab.
  **This entry read as a sweep of the page and was a repair of one list.**
  Two more `<For>`s were sitting on the same defect when it was written, and
  the second of them is eight lines above the comment that states the rule.
  The offers panel was keyed on the **provider's name**, which does not
  change between two queries of one provider -- so narrowing the card filter
  and pressing the button again rendered the first answer, and a server
  answering zero offers plus `dial tcp: lookup rest.runpod.io: no such host`
  left an H100 at $2.49 in US-KS-2 on screen with no error at all. That is
  the thing the component's own comment forbids ("I could not reach RunPod
  must never render as RunPod has nothing"), defeated by the key two lines
  above it, and the real behaviour is worse than the comment's wording: not
  "RunPod has nothing" but "RunPod has this card at this price". The fix is
  a plain map over the signal and no key at all -- for the outer list and for
  the inner one over a provider's offers, because leaving the inner `<For>`
  keyed on an offer token would have put the same frozen row one level down.
  **Not a key that encodes the row, which is the other answer and the worse
  one**: a key is stable identity, so one built out of the data rebuilds the
  DOM for a price change and leaves the next field somebody adds in the same
  hole. Reading inside the closure keeps the key honest, and here there was
  no identity worth keeping across two queries at all.
  **The decision moved to `Listing::answer`**, which is where `Rows::note`
  and `Request::reason` live and the reason both are methods: a precedence
  written as a chain of `if` inside a view macro can only be checked by a
  browser, and the error-wins-over-the-offers-beside-it rule now has a
  `cargo test` instead of a comment. The error path is the half no lab can
  reach -- `CloudBaseURL` is a Go field on `app.Deps` and not an environment
  variable, so a lab that runs `ember` as a subprocess cannot point a cloud
  provider at a closed port -- so the browser asserts the rows in both
  directions across three presses and `cargo test` asserts the precedence,
  which is the pair rather than either alone.
  **The third was the queue table's price column, and that one is money.**
  Its key is `(id, status)` -- the status is in the key *precisely* so that a
  status change rebuilds the row -- and `requests.cost` is written **after**
  the status is terminal: `finish` writes done/failed/cancelled and
  `Recorder` writes the cost through from `provision.WithNode`'s **deferred**
  teardown, which is the ordering `queue.go` states in words. So the key
  `(id, "done")` is created with a zero and can never change again, and the
  two-second poll re-fetched the right figure hundreds of times into a row
  that would not take it. A rider's zero is blank by design, so the opener's
  cell is the **only** place in that table a paid rental appears -- which
  means an `ember serve` session that rented an H100 for an hour showed a
  queue of finished requests with no money in it at all, and nothing said the
  column was incomplete. The same row one panel over had already been fixed
  for the same reason, and `Progress::rental`'s own doc comment quotes it.
  **The fix is the closure and NOT `(id, status, cost)`**: a key that carries
  the money rebuilds the DOM for a price change and leaves the next field
  somebody adds in the same hole. `Rows::price` is the decision and
  `price_cell` is the read, and the comment now names which fields in that
  row can move on their own -- `status` is in the key, `jobs` and `gpu` are
  written at enqueue, `reason` is written *with* the status, and `cost` is the
  one written after the key is fixed. `class:running` was read outside its
  closure too and is swept: it can only be stale for one poll and it is a CSS
  highlight, so it was not the defect, but one read outside the closure left
  in the row whose other one was leaves the next reader to work out which is
  which.
  **No lab here can produce a non-zero `requests.cost`, and the lab is
  therefore a mid-run seed.** Every lab rents from the local provider, whose
  price is zero -- the same reason `Progress::rental`'s `Accruing` and its
  non-free `Charged` are unreachable from one -- so a lab that rented would
  assert a blank cell against a blank cell and pass against the defect. And
  the seed has to land while the page is ALREADY OPEN, because a page loaded
  after the write shows the figure either way: that is the reload the issue's
  own negative control used to prove the bytes were arriving. Two writers on
  one SQLite file is what `genstore.OpenFile`'s `journal_mode(WAL)` and
  `busy_timeout(5000)` are for, and nothing is rented: the seeded row is
  `running` only for the instant between `Adopt` and the `Update` that ends
  it, and `ClaimNext` takes queued rows alone.
- **A sentence in a table cell is a sentence in whatever width is left over,
  and the fix for #103 landed in one (#138).** The reason a request ended was
  rendered as a `<span class="why">` inside `td.status` -- one column of a
  six-column table under `table-layout: auto`, in the `1fr` half of the page's
  `1.3fr 1fr` grid. **Measured at a 1280px viewport rather than argued: the
  body is 1088, the queue panel 444, its table 407, and that cell gets 174 of
  it** -- so `app.AttachStore`'s store refusal, 170 characters naming
  `ember store create`, rendered **174px tall, nine lines of two or three
  words**, in a 204px row. Five failed requests fill the viewport. The entry
  above about `Request::reason` is right that the page had to show it and says
  nothing about where, and the CSS comment that did -- "it sits under the
  status word, on its own line, because the reason is a sentence and the cell
  is one column wide" -- names the problem and settles for it.
  **WHOLE, AND NOT TRUNCATED BEHIND AN INTERACTION, and the reason is that the
  length was never the defect.** The same sentence across the table is **four**
  lines. So the question "should this be clamped" is downstream of a width that
  is now right, and answering it with a `<details>` or a `title`-only
  truncation would put an operator who does not know to click back where #103
  found them -- reading `failed` and nothing else -- over a sentence whose
  whole point is that it carries `ember store create`. The `title` the old span
  had is **gone** for the same reason it existed: it was there "because some of
  those are long enough to wrap a cell", which is true of a cell and not of a
  row, and a native tooltip is a second carrier of one string that cannot be
  selected, does not appear on a touch device and truncates per platform.
  **`white-space: pre-wrap`, BECAUSE EMBER WROTE THE LINE BREAKS AND HTML WAS
  EATING THEM.** `app.AttachStore` writes "Create it with:\n  ember store
  create\n" so the command is on its own indented line; collapsed, it ran
  together with the prose on both sides. This is `.log`'s rule one class over
  -- the server composed the layout and the page is not a second author of it
  -- and it is not decoration: the lab's existing "the page must not paraphrase
  a refusal" equality goes **red** on exactly those two newlines, in the row
  and again in the watcher, because `.why` is one rule for both.
  **A `<tbody>` PER REQUEST AND NOT TWO LOOSE `<tr>`s**, because the grouping
  decides three things at once that would otherwise be three rules able to
  disagree: `running` and `watching` cover the pair rather than its first half,
  one border is drawn under the request rather than between its own two rows,
  and the reason cannot read as belonging to the request BELOW it -- which with
  loose rows it would, since `td`'s bottom border sits between them. Written as
  `.queue td { border-bottom: 0 }` plus a `tbody tr:last-child` rule rather
  than `:has(+ tr.reason)`, which holds whether or not a reason row is there
  and needs no lookahead.
  **The `colspan` is a literal and the lab is what holds it to the row above
  it.** Nothing in Rust can count the `<td>`s a view macro emits, so the six
  here and the six cells beside it are held together only by something that
  renders them: the lab asserts the reason cell is as wide as its table, which
  is red the day a seventh column arrives. It cannot see a colspan that is too
  LARGE, because the table model clamps one to the columns the table has, and
  that is the direction with no defect behind it.
  **THE LAB'S VIEWPORT IS LOAD-BEARING AND IS ASSERTED RATHER THAN TRUSTED.**
  Headless Chrome's own window is 800x600 and the grid is one column below
  60rem, so at the default the queue panel is already the full width of the
  page and a height bound passes against any markup at all -- the
  `AssertNoStray` shape reached through a viewport. `chromedp.WindowSize` sets
  it and the assertion then re-checks that the grid really split, by comparing
  the queue panel against the one the stylesheet makes span both columns.
  **And the trigger had to change for the bound to be able to fail**: the lab
  refused a `gpu` the local provider does not offer, whose reason is one short
  line -- one short line in a 174px cell as much as in a 407px row. It names a
  store the provider does not have instead, which `NewPlan` refuses at
  plan.go:172 against `PickOffer` at 280, so nothing is created and nothing is
  billed, and the reason is the three-line one the issue's screenshot showed.
  **Six reinstatements, no two of which fire the same assertion, which is the
  half worth recording.** Two of them fire a pair, and that is the shape rather
  than a blur: what matters is that no case is indistinguishable from another.
  The old markup is red at "the reason is somewhere in the panel but not on a
  row of its own"; no reason row at all is red at "the panel never named why
  the request failed", and those are two waits rather than one precisely
  because they are two defects with two fixes; `colspan="1"` is red at the
  width (208 of 407) **and** at the height (7 lines against 5);
  `white-space: normal` is red at the row's text equality **and** at the
  watcher's -- two disjoint assertions, and geometry catches neither, since
  collapsing the line breaks makes the cell *shorter* (62.5px); the viewport
  removed is red at the two-column precondition and at nothing else; and a
  reworded comment came back **not caught**. Controls green at both ends, one
  backup per path, every case ending on a diff against the pristine copy.
  **The precondition is the only thing between a narrowed viewport and a false
  pass, and that has an exit code rather than an argument.** Removing the
  window size **and** the precondition together is **exit 0, green**, at 694px,
  where the height bound is satisfied by a queue panel that is already the full
  width of the page.
  **The harness's own defect is a restore that repairs the SOURCE and not the
  ARTEFACT, and it goes both ways.** It rebuilt the wasm only for the cases
  that patched Rust or CSS, so the one Go-only case ran against
  `internal/webapi/dist` as the case before it had left it -- and reported that
  case's `white-space: normal` as its own finding, a second assertion firing
  that had nothing to do with the patch under test. Caught by asking why a case
  had fired an assertion nobody had aimed at it. **The other direction is the
  dangerous one**: patch `web/style.css`, do not rebuild, and the lab is
  **exit 0 with the geometry byte-identical to the control** -- the same root
  cause, that no test run derives `internal/webapi/dist/style.css` from the
  working tree, and a false GREEN gets committed where a false RED gets
  investigated. It rebuilds unconditionally now.
  **One thing was measured by hand and is not asserted anywhere, and it is
  narrower than it first read**: that Chrome paints an `outline` on a
  `<tbody>` at all. That is a test of Chrome, needs a screenshot, and is a note
  here rather than a lab for exactly that reason -- checked against real Chrome
  on a page holding nothing but a table with three groups. What it was wrongly
  used to decline is a **different property**: whether the mark's SUBJECT is
  the group that holds the reason, which is pure DOM, is a test of ember, and
  is four lines in a lab that already clicks that row. It had no exit code --
  moving `class:watching` back onto the `<tr>` left the lab green -- and now it
  does: `groups`, `onTbody`, `holdsTheReason` and `markedRows` read
  `1/true/true/0` as it ships and `0/false/false/1` with the mark on the row.
  Until that probe, the three things the grouping settles had no exit code
  between them, because the geometry is identical whichever element carries
  the class.
