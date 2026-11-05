# The ledger

What every run records, what `ember history` and `ember spend` read, the
nodes route, and the measurements that live nowhere else.

Back to [the money index](../money.md) · [AGENTS.md](../../AGENTS.md)

- **The queue's own store-filling rental was in no ledger, and the question
  worth an issue was which row it belongs to.** #142 taught `ember serve` to
  rent a machine with no card when a queued request's store is missing its
  weights -- the measurement behind it is $0.61 of a $0.63 rental spent
  downloading at a card's rate -- and `fillStore` called `SyncAssets`, which
  took no `report.Sink` and passed `p.Options` rather than
  `p.lifecycle(sink)`. So neither of the two moments a caller cannot otherwise
  see reached anybody: the node becoming ready, and what the rental cost,
  which arrives from `provision.WithNode`'s **deferred** teardown. `ember
  history` and `ember spend` were blind to a rental ember chose to make with
  nobody at the keyboard, which is the one kind of spending a ledger exists
  for -- "a total that is missing part of its history says so, every time",
  against the entry that says it. The issue's own first wording overstated it
  as one sync per request; `fillStore` runs on an `AssetsMissingError` and the
  next plan finds the store full, so it is one unrecorded rental per fill.

  **A ROW OF ITS OWN, and the other two candidates are wrong for reasons this
  file already records.** Charging it to the triggering request is the cheap
  answer and makes `requests.cost` hold two rentals, in the one column
  documented, read and summed as one -- so `ember spend`'s cost-per-image
  would divide a bill nobody can decompose, and it is wrong for every request
  after the first, which gets the filled store for free. A `rentals` table is
  the thorough answer and buys nothing today: every rental ember has ever
  recorded is already a request row, `Entry.Rode` already says which rows
  carry a rental's cost, and a second thing for `ember spend` to sum is a
  second place for the total to be wrong -- plus a migration, a fake method
  and conformance rows for a shape with one reader. What the sync IS is a
  rental nobody queued, which is exactly what `genstore.Adopt` and
  `queue.AdoptRun` exist for, so there is **no new column, no migration and no
  reader to teach**: `ember history` already prints a jobs-less run with a
  dash for images and card time and counts it in the footnote saying how many
  rows it left figures out of. What is NOT recorded is which request caused
  it -- there is no column for a parent and inventing one would be a migration
  for a field nothing reads -- so an operator sees two rentals, which is what
  happened.

  **`ember store sync` was the same hole with an operator at the keyboard**,
  and it is the same fix rather than a second one: `recordRental` -- `ember
  up`'s own helper, renamed because a name claiming one caller while two use
  it is the `AssertNoStray` defect -- opens the store, writes the row before
  the create call and cannot fail the run. `report.Text` renders nothing for
  any event a sync emits, so that command's output is byte-for-byte what it
  was.

  **A planner that cannot record refuses to rent, and the governing rule is
  `out()`'s rather than `refuseOverPeriod`'s.** The first wording of this entry
  cited the ceiling, which is the weaker of the two available: that one refuses
  a nil ledger only when a ceiling is **configured**, because what it honours
  is an operator who wrote `max_month_usd`, while this keys on wiring. What
  licenses keying on wiring is `AppPlanner.out()`'s argument -- a nil interface
  here is a crash, and `recordSync`'s first call is `Adopt`, so the panic lands
  in the drain goroutine **before** the sync's node exists: it takes `ember
  serve` down rather than leaving a machine running, and a panic unwinds
  `provision.WithNode`'s deferred destroy on the way out in any case. So
  answering the nil is cheap and the useful answer is a refusal, because an
  unrecorded rental is the defect being fixed and a nil recorder would reopen
  it silently. Nothing an operator can type reaches it: `cmd_serve.go` is the
  one production construction and always wires `Runs`, so it guards the shape.
  `AppPlanner` therefore holds a `RunStore` BESIDE its `Spender` rather than
  one `genstore.Store` -- five methods that can record a run and cannot
  `Enqueue` or `ClaimNext`, because a queued row is what a drain loop takes
  and a layer able to write one could rent a second card from the record of
  the run already in flight. Two narrow types over one store, which is
  `Spender`'s own argument in the other direction: what a layer can do to the
  database is visible in its type. And `Plan`'s doc comment said "rents
  nothing", which stopped being true the day `fillStore` landed; it is
  corrected rather than left as one more argument that persuades about a
  mechanism nobody re-read.

  **The reinstatement found the `--allow-paid` gate on that rental could not
  fail, which is a worse defect than the one being fixed.** This file says the
  sync "is a rental and is gated like one"; the test for it asserted `err !=
  nil`, and `internal/queue`'s fake served **no CPU catalogue** -- so the run
  stopped at a 404 before `refusePaid` was ever reached, and deleting that
  gate outright left the test GREEN. Measured both ways rather than argued:
  against the tree as it was, no CPU route and the weak assertion, deleting
  the gate is **exit 0**; with the captured `cpus.json` served and the
  assertion naming the flag, it is **exit 1**. The fake serves the provider's
  own committed fixture rather than a second copy of a field shape, and there
  is still no `/v2/pods` route -- which is what makes the create failure
  reachable from a unit test at all, and the create failure is the rental most
  worth recording, because the row goes down before the call.

  **Eight defects reinstated, each judged on an exit code, beside a negative
  control (a reworded comment, reported as not caught), that historical
  measurement, and a control at both ends.** Seven of the eight went red where
  expected and the eighth is an asymmetry worth writing down rather than a
  miss. The first wording here said nine and eight -- in this file, which
  records that "a running total in prose is the thing this file has been wrong
  about before" -- while the table it describes lists eight defects, a control
  and a measurement. So the count was wrong about itself on the first run,
  which is one more instance of the thing rather than an exception to it.
  The sink wired and the lifecycle hooks not -- `p.Options` in place of
  `p.lifecycle(sink)`, which is the original defect's own shape -- is **GREEN
  in the unit suite and RED in the lab**, because the unit row's create fails
  before a node exists, so it has no node and no cost either way. The lab is
  the only place in this tree that can assert the MONEY: every other rents
  from the local provider, whose price is zero, so a cost assertion there
  would compare a blank against a blank and pass against the defect.
  `runpodfake` prices a CPU pod at $0.06/hr and a GPU pod at $0.89/hr, read
  back off the pod as the bill rather than as a list price, so the sync row
  carries a real rate and `ember spend`'s period total has to equal both
  rentals -- which is also the first assertion in this repository that the
  aggregate counts a rental ember started on its own initiative.

- **`GET /api/nodes` answered 501 in every `ember serve` there has ever been,
  and it is WIRED UP rather than deleted because the browser has no second
  mechanism for the one charge nothing ends (#105).** `webapi.Options` had no
  `Scan` field, `New` never assigned the Server's `scan`, `cmd_serve` could not
  have set it if it had wanted to, and the three assignments in the tree were
  all in test files reaching past the constructor into an unexported field of
  their own package -- so the suite could not tell "the production value is
  missing" from "this test did not set it", which is exactly how it survived.
  `web/` had no type for the answer either, which is why nobody had ever seen
  the refusal. One more of the things here written, covered and connected to
  nothing, and the first where the missing join is one struct field in the
  package's own constructor.

  **The test is this file's own and the answer is `sdserver.Client.Cancel`'s,
  not `queue.Cost`'s.** `queue.Cost` went because `genstore.Spend` answered the
  same question **to the same reader through the same command**; `RateFromFloat`
  went because `Offer.Validate` was the live copy of its ceiling; `exposure.Probe`
  went because the join was not available at all. Here the reader is a browser
  and `ember ls` is a terminal command: there is no second path from the page to
  "what is billing", and the join is one field and one closure. The sharp half
  is that `ember ls` is *not* the second mechanism it looks like -- the one-time
  token URL means the operator had a shell to get the page, but `ssh -L
  7777:localhost:7777` is the deployment this file already argues about, and
  that operator's shell is on the other machine.
  **The forwarded-port operator carries that argument alone, and the stronger
  version of it was written down first and is not supported.** "An operator who
  only ever opens the web UI" needs a configured `serve_token` as well as the
  forward -- a minted one does not survive a restart and is printed on the
  machine running the server -- and no lab and no deployment in `README.md`
  exercises that pair, so the pure case is a shape rather than an observation.
  What does not need it is that **`ember ls` on that laptop is a different
  installation and not the same mechanism reached differently** -- and the
  reason offered for that in review was that #146 made the CLI a socket client
  and a unix socket does not travel through `ssh -L`. **Checked, and it is not
  the reason**: `s.sockMux` carries exactly one pattern beyond the API,
  `POST /api/runs`, and `runLs` calls `app.Scan` directly at both of its sites.
  `ember ls` has never been a client of `ember serve` at all. The true reason
  is larger: it loads the config, reads the credentials and asks the providers
  **itself**, so reaching it from the laptop means a second ember installation
  *with the provider credentials on the laptop* -- which is the thing the
  browser is there to avoid rather than a packaging detail. Recorded at this
  length because the weaker reason is the plausible one, and this file's own
  rule is that an argument about a mechanism is worth one grep.
  And the fact with no other carrier for that operator is the narrow one: the
  **standing storage charge**, which `ember spend` and `ember history`
  deliberately do not price because pricing a store means asking a provider.

  **Wiring the Go half ALONE would have moved the defect rather than closed
  it**, which is why the panel is in the same change: a route that answers, a
  projection that is correct and nothing reading either is "a query layer with
  no reader", one layer out. The issue offered the browser half as possibly its
  own change and that is the one part of it refused.

  **Three defects were found by connecting it, and the first is the whole
  lesson in one line.** `viewFinding` wrote `Kind: string(f.Kind)` and
  `reconcile.Kind` is a `uint8`, so the conversion took the numeric value as a
  rune: every finding reached a client with a kind of `"\x03"` or `"\a"` rather
  than `expired` or `store`. `go vet`'s `stringintconv` does not report it --
  byte-to-string is a legal, ordinary conversion -- and no test could, because
  nothing in either language had ever read the field. Second,
  `nodesView` dropped `app.Scanned.Leftovers` entirely while its doc comment
  said "`ember ls`'s answer" and `Attention` claimed to be "the number `ember
  ls` exits 2 for": the projection was two of three slices and a count over
  two, so a bridge network whose node is gone -- the finding whose cost is
  every local run failing at once weeks later -- was absent from the UI, and
  `viewFinding`'s id switch had no arm for one either, so it would have
  arrived anonymous. Third, the 501 itself.

  **`app.ScanWith` exists so the server does not re-read the authority.**
  `app.Scan` takes a config PATH: it loads the file, reads the credentials and
  builds a registry, every call. `runServe` reads that file once on purpose --
  it is the ceiling, and "a request must never be able to name which file sets
  its own limits" is why the path comes from a flag -- so a handler calling
  `Scan` would have built the second pair `ember reap`'s own comment refuses,
  where "the one that disagrees is the one that does not destroy the node". The
  closure in `cmd_serve` is over the registry and the journal that process
  already holds, and `Scan` is now `ScanWith` plus one config read, so there is
  exactly one classifier.

  **NOT CACHED, AND NOT POLLED, and those are two separate decisions.** Three
  provider sweeps per call, on the HTTP request's own context, which is
  `getOffers`' shape and `getOffers`' exposure. Memoising would be
  `local.DetectGPU`'s defect pointed at money -- a cached "RunPod said nothing"
  is precisely "I could not check RunPod" rendering as a clean bill of health,
  and a cached listing of what is billing is a stale bill -- so the only lever
  left is how often it is asked, and the panel is a button. No clock either:
  `DetectGPU` refused to invent one ("a clock nobody here has measured") and
  the request's context is the bound, as it is for the offers panel.

  **An unreachable provider is a 200 and the 502 is for a sweep that could not
  START.** `reconcile` emits a `KindUnreachable` finding per provider it could
  not ask, so a sweep that reached the local daemon and not RunPod is complete
  about the first and named about the second -- and losing the first to report
  the second would hide what the daemon just said. What the 502 means is that
  the journal of live nodes would not read, where every classification below it
  compares a provider's answer against records that are not there.

  **The `Line` field is deleted and the judgement is kept, which is the
  `Envelope::next` shape.** `findingView` carried `reconcile.Finding.String()`
  beside the parts it is built from, argued for as "an operator reading both
  should see the same sentence in each" -- and that argument does not survive
  contact with a browser: `String` is `%-14s %-28s %s` and HTML collapses runs
  of whitespace, so the string in a page is not the sentence a terminal shows
  unless the page also asks for `white-space: pre`, which would put a
  terminal's column widths inside a table that has columns of its own. The
  parts are what both channels can agree on, because Go still formats every
  money figure exactly once. `Monthly` is added beside `Accrued` for
  reconcile's own reason -- money already spent stops when the node is
  destroyed and a standing charge does not -- and the test of the whole type is
  now that **every field on it has a reader in `web/`**, which is the property
  it did not have. `Provider` is the one that cannot be trimmed: an unreachable
  finding has no node, no record, no store and no leftover, so its id is empty
  by construction.

  **`Attention` stays on the wire and is never derived in the page**, although
  every kind needed to derive it is there. `Kind.NeedsAttention` is "a switch
  rather than `k != KindOK`, so that a kind added later needs attention by
  default", and a second copy of that switch in Rust is a copy whose author
  will not be reading the Go one -- which is why `Money` crosses this wire
  twice as well. It is an `i64` in the page because the Go field is an `int`
  and this seam has already lost a whole table to a number the server could say
  and the client could not hear.

  **No per-row tolerance, and that is `Rows`' own rule rather than an
  omission.** That entry says `requests` ONLY: it is the envelope where one bad
  row costs the view of every good one and the one with somewhere to say so.
  This envelope has an error of its own on the way in -- a 502 reaches the
  panel as `api::reason`'s sentence -- and its numbers are a count and two
  money figures rather than ten `i32`s a request can set.

  **What a browser can check and what it cannot.** It can check the join:
  whether the button fetches, whether Chrome's cookie reaches `/api/nodes`,
  whether Go's bytes decode into `Nodes`, and whether the rows reach the DOM --
  which is the `<For>` class this file has now been bitten by three times, and
  the reason `findings_table` has no key at all. The first assertion is that
  the panel says **nothing** before the first press: `Nodes::default()` has
  three empty slices, so its verdict is "nothing is running and nothing was
  left behind", and rendered unasked that is a clean bill of health over a scan
  that never happened -- the signal is an `Option` for exactly that, which is
  `AssetCheck.Asked`'s distinction one package over. What it cannot check is
  the money: the local provider prices a Docker volume at nothing, so
  `monthly_storage` is zero in every lab here and only its RENDERING is
  assertable -- the same bound that stops a lab producing a non-zero
  `requests.cost`. A non-zero standing charge needs a rented store.
  `Nodes::verdict`'s unreachable clause has no lab either, for the reason the
  offers panel's error path has none: `app.Deps.CloudBaseURL` is a Go field, so
  a lab running `ember` as a subprocess cannot point a cloud provider at a
  closed port. `cargo test` drives it instead, in both directions and over the
  case where the store listing failed and the node listing did not.

  **Reinstated, judged on exit codes, with a control at both ends of every
  pass and four negative controls. Eleven defects, each red.** Through
  `go test`: the `scan: o.Scan` assignment removed from `New` (exit 1 at
  `TestNew_CarriesTheScannerFromItsOptions`); `string(f.Kind)` back (exit 1);
  the `sc.Leftovers` loop removed from `viewNodes` (exit 1); `Attention`
  summed over two slices (exit 1 at 3-want-4); the leftover arm removed from
  `viewFinding`'s id switch (exit 1); and `var v nodesView` in place of the
  three `[]findingView{}` (exit 1 on the bytes). Through `cargo test`, all
  exit 101: `unreachable()` narrowed to `findings`; the verdict's blind
  clause deleted; and `nothing_running` back to two clauses. Through the labs,
  which are the half that proves the production wiring: **`Scan` not set in
  `cmd_serve`** is exit 1 with **two** labs failing, at
  `GET /api/nodes answered 501: this server was built without a node scanner`
  -- the refusal an operator would have got for the life of the feature; the
  dropped leftovers again over the real wire (exit 1, `leftovers = []`);
  `string(f.Kind)` again over the real wire, which names the characters
  themselves (`kind = "\a"`, `kind = "\x06"`); the panel rendering
  `Some(Nodes::default())` instead of `None` (exit 1 in Chrome at *the panel
  answered before anything was asked: verdict="nothing is running and nothing
  was left behind"*); and `findings_table` returning nothing, which is the
  `<For>` class the browser lab exists for (exit 1, *never reported the
  leftover within 1m0s*, with the verdict still reading "1 finding(s) need
  attention" -- so only the rows went, which is the defect's exact shape).
  Four negative controls -- a reworded Go comment, a reworded Rust doc
  comment, a reworded comment through the labs and one through the browser --
  all green. **One attempt reported BUILD-ERROR rather than a result**, which
  is this file's own trap working: a marker appended to `Kind: string(f.Kind),
  Provider` put a `//` comment inside a composite literal, and the harness
  refused to read a tree that would not compile as a failing suite. Both
  controls green at both ends of all four passes, and `git diff` empty over
  every file the harness touched -- which is the check added after a pass
  shipped claiming a deletion that a stale backup had put back.

- **Every measurement that was only in a config file's header is in
  `docs/configurations.md` now, and the entry above about the placeholders
  named two files that no longer exist and is REPOINTED rather than left
  (#144).** Leaving it was argued for first, as the record of #18's lesson --
  and the standing practice in this situation is to repoint, which the same
  change applied to `imagelab_test.go` and `served_test.go`, so leaving this
  one was inconsistent with its own commit. One phrase each keeps the #18
  record intact and stops a reader following a filename to nothing: the
  refusals that entry describes are unchanged, and what moved is only where
  the two configurations are written down. The
  numbers that had no second home anywhere in the tree, listed because the
  one unacceptable outcome of that change was losing one: the 70 GiB store's
  standing charge (**$0.0067/hr, $0.16/day, $4.90/month**, which `ember ls`
  prints and only `ember store rm` stops); the weight breakdown behind the
  56.16 GiB (**23.33 GiB of it a prompt-reading LLM, 32.60 GiB the diffusion
  file alone**, against a registry that refuses a layer above ~10 GiB); the
  push arithmetic that `store sync` exists to avoid (**56.16 GiB at 67 Mbps
  is 1h52m**, after about an hour of pulling it home first); the **11 seconds
  and two hundredths of a cent** a sync cost to learn that the older node
  image read `embernode fetch` as "be a node"; **EUR-IS-1 is the only one of
  the ten S3 datacentres with three cards at 80 GiB or more**; the per-image
  dollar table for a resident model at four batch sizes on an RTX PRO 6000 at
  $2.49/hr ($0.2378/$0.1302/$0.1249/$0.1055, crossover between one and two
  images); **sd15 Q8_0 at 256x256, 0.44 s per job served against 1.91 s
  fresh**; and **56 GiB of weights in a 125 GiB machine behind an 11 GiB
  card**. The
  ones that already had a home in `README.md`, `AGENTS.md`, `docs/` or a Go
  comment -- 67 Mbps, 868 MiB/s, $1.79, 4412 MiB, 35 GB per step, the $0.37
  CPU-fallback run and its 122-second text encoder, 14 GB per Wan expert, the
  33/77/81 frame counts -- were checked for by grep rather than assumed.
