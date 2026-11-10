# Reaching a node, and the progress socket

The far end of a connection into a node is built, never parsed from input.
And the socket that noticed its own close and never re-opened.

Back to [the threat model index](../threat-model.md) · [AGENTS.md](../../AGENTS.md)

- **The far end is built by `tunnel.RemoteLoopback(port)` and never parsed
  from input.** `direct-tcpip` connects anywhere the node can reach, including
  the provider's metadata service; a free-form remote host makes every ember
  node an open relay billed to the operator. It takes a port and not an
  address, so there is nothing for a config or a request to name.
  **And there is now one port rather than two**: the forward to sd-server's
  own HTTP API is gone, because the worker reaches that server on the node's
  loopback where the call is a syscall instead of a round trip to the
  operator's machine. Every connection ember opens into a node goes to
  `control.Port` — see
  [control-channel.md](control-channel.md).
- **The progress socket noticed its own close, wrote a signal nothing read,
  and never re-opened -- and the parameter that would have made a reconnect
  lossless had one caller passing the literal `0`.** `connected` was an
  `RwSignal<bool>` with three writers and, by `grep -rn connected web/src`,
  no reader at all -- the only other hit in the crate was the word in a doc
  comment; `api::Socket::open` took an `after`, put it in the URL, and was
  called once with a constant. So a stream that ended mid-generation left the
  last frame on screen under a panel still captioned as though the card were
  sampling -- #132 measured it on the real wasm, the jobs row and the whole
  panel byte-for-byte identical six seconds after the server closed the
  socket, with no console output of any kind. Three comments in
  `web/` described the reconnect, one of them promising that "a page reload
  and a dropped connection are the same thing to it", and the two were not the
  same thing at all: a reload re-opened from zero and a drop did nothing. One
  more of the things here written, documented and connected to nothing, with
  `cancelling` and the preview frame; what is different is that this one's
  three readerless pieces were a signal, a parameter and a wire field at once.
  **The cases are the operator's ordinary ones and the generation carries on
  through all of them**: `ember serve` restarted, which is the remediation
  ember's own cancel 409 prints; a laptop that slept; a forwarded port that
  blipped. The tab is then permanently half-live, because the queue panel
  polls and recovers while the watcher does not -- two panels on one page
  disagreeing about whether ember is there.
  **Three states and not a bool, and the quiet one is the point.**
  `Stream::note` answers `None` while the socket is up, because a line that is
  always there is not a signal, and the absence is asserted beside the
  sentences -- an assertion that only looks for the presence passes against a
  page that prints it unconditionally. The two sentences differ because the
  remedies do: one says ember is trying again, the other says it is not coming
  back and that reloading is what fixes it. Neither offers a verdict about the
  generation, which is the `destroy_failed` caption's rule -- ember cannot
  tell a dropped socket from a stopped server from a sleeping laptop, so the
  line says the one thing that is true, that the step on screen is the last
  one that arrived.
  **The retry is bounded against the SLOWEST case rather than the fastest.**
  Twelve attempts, 250ms doubling to a five-second cap, about forty-three
  seconds: a socket that dropped on its own is back inside a second, and the
  figure is set by a restart typed by hand, because a bound a restart cannot
  fit inside would make the headline case one this never covers. Bounded at
  all because the alternative cannot report -- a page that retries for ever
  can never print the second sentence. A terminal request is not retried,
  which is also what makes the restart case terminate: `queue.Run` sweeps
  everything it finds running to `interrupted` at startup, so the first
  successful reconnect after one arrives at a terminal row and stops.
  **`onopen` is not decoration, and leaving it out turns the bound back into a
  loop.** `WebSocket::new` returns Ok against a server that is not there --
  the handshake is asynchronous -- so "the socket opened" is a thing only the
  browser can say, and a counter reset on the constructor's Ok is reset on
  every attempt. The old code read that Ok as `connected.set(true)`, which was
  a lie nobody could see because nothing read the signal.
  **`Socket::drop` clears the handlers before closing, and that stopped being
  cosmetic the moment the signal got a reader.** `close()` makes the browser
  call `onclose` on a closure about to be freed with the struct --
  `closure invoked recursively or after being dropped`, observed by clicking a
  queue row twice -- and it was harmless only BECAUSE the write it could not
  land was one nobody was looking at. With a reader it is the old socket
  reporting a stop over the new socket's state, and a retry scheduled against
  a request nobody is watching. The ordering that used to save it -- the new
  `Socket` stored before the old one dropped -- was luck rather than a
  decision, and the epoch counter beside it is the guard that does not depend
  on it.
  **`Envelope::next` is not decoded, because it is fully derivable, and the
  derivation is measured rather than argued.** The issue's terms were that it
  either gets a reader or goes, and the first version of this change gave it
  one: `Progress::fold` took the larger of the server's figure and the one
  `apply` derives from the events. That is the `queue.Cost` shape -- two
  writers of one field, where a reader of the code cannot tell which is live
  -- and deleting the read left every `cargo test` green, which is the
  measurement and not a hole. Driven over a real store afterwards to settle
  WHY: `next` is the last event's own seq and not one past it, falling back to
  the cursor the client sent on an empty envelope; `genstore` selects the
  whole tail ascending with no limit; nothing filters the slice;
  `AppendEvents` is `INSERT OR REPLACE` on `(request, seq)`, so a redelivery
  overwrites in place rather than leaving a counted hole; urgent events travel
  the same rows; and a prune leaves `next` at the maximum of what remains.
  Seven event-set shapes times eight cursors, three of them above the
  maximum, plus eight more after a prune: **64 envelopes, and in every one
  `next == max(cursor sent, max seq in the envelope)`** -- with a deliberate
  `+1` the harness goes red, so the figure is not vacuous.
  **And it is the less safe of the two in the one direction that matters.**
  The day this page tolerates an event it cannot decode by dropping it --
  which is exactly what `Rows` already does for a request it cannot read --
  the client-derived cursor stays below that event and a reconnect asks for it
  again, while the server's figure is above it and the reconnect skips it in
  silence. A cursor too LOW costs a replay the fold discards; a cursor too
  HIGH costs the operator the events they never learn about, which is the
  frozen row this whole feature is about.
  **The field's own doc comment argued the opposite and was BACKWARDS**, which
  is the sharper half of why it went rather than being kept with a better
  comment. It read "`fold` takes the larger of the two, so it can never move
  past an event that was not folded" -- and `max` is precisely the operator
  that can; `min` is the one that cannot. Harmless only because `next` never
  exceeds the envelope's maximum on anything this server produces, which makes
  it a comment wrong about why a seal holds: the thing that gets a seal
  removed by somebody being helpful, and here it would have licensed the next
  reader to KEEP the read believing it was a safeguard. So the two judgements
  are a note where the field was, the `node.RateFromFloat` shape -- `min` and
  never `max`, and advance AFTER applying, because `apply` ignores anything at
  or below the cursor and advancing first discards every event in the envelope
  that set it, the whole content of the first send after a reconnect. The Go
  field stays, because `GET /api/requests/{id}/events` answers the same
  question to a client that would rather poll and `next` is its documented
  cursor.
  **THIS ENTRY SHIPPED CLAIMING THE DELETION WHILE THE FIELD WAS STILL THERE
  AND STILL READ, and the cause is the reinstatement harness rather than the
  edit.** The browser pass and the fast pass shared one backup directory, and
  the backup of `model.rs` was taken by the FAST pass -- before the deletion
  -- so the first `restore_all` of the browser pass copied the pre-deletion
  file back over it. That is this file's own fifth harness shape, a rollback
  that restores a stale backup, reached ACROSS two passes rather than within
  one: `backup()` takes one copy per path by design, which is right inside a
  pass and is exactly wrong between two. What let it through to a commit is
  that the control which catches it -- a closing control that is the same
  green as the opening one -- *was* green, because the deletion's own case was
  green either way, so nothing about the suite could tell a restored tree from
  a repaired one. **A control at both ends is not sufficient when the change
  under test is one no test reports**; the check that was missing is reading
  the diff of every file the harness touched before committing, and the rule
  is that a reinstatement pass that patched a file must hand back its diff and
  not its exit code.
  **WHAT NO ASSERTION ON THE PAGE CAN SEE is which cursor the reconnect asked
  from**, because the fold is idempotent per sequence number: a client that
  replayed an hour of progress renders exactly what one that resumed correctly
  renders. That is why the lab reads it off `ember serve --log-level debug`
  instead -- the handler is the only place that knows, which is the same
  argument this file already makes for a `took=` on a debug line rather than a
  span exporter. The line is sealed by
  `TestWS_RecordsTheCursorItWasAskedToResumeFrom`, because a lab resting on a
  log line nothing promises to print is a lab resting on a grep, and the
  cursor is checked in BOTH directions: zero is the defect, and a cursor above
  the request's last sequence number is the other one -- a watcher that never
  learns what it missed, which freezes exactly as it used to.
  **The lab drives THREE drops on one generation, and the first is the one
  that is about the defect.** A close made by the browser, with `ember serve`
  untouched, is the only arrangement in which the generation carries on
  underneath -- which is the whole story: a frozen watcher over a card that is
  still sampling and still billing. The step on screen moves past what it was,
  and nothing else on the page reads a sampling step, so only a resumed stream
  can have brought it. A restart cannot do that, because it kills the thing
  that would produce the new events; what it proves instead is the other half
  of the terminal rule, since a rental stopped with Ctrl-C ENDS its request --
  a page that has learned that says nothing about the stream and re-opens
  nothing. The second drop is the one the note is read on, and the entry at
  the end of this one says why there are three of them rather than the two
  this paragraph used to describe. Reinstated separately, each catches what
  the other cannot: the no-reconnect defect goes red on the step that never
  moved, and the resume-from-zero defect goes red on the cursor.
  **The browser-side close is injected script and not a seam in the page**,
  which is the line `sshx.Runner` is not widened across either: `api::Socket`
  holds the only reference and nothing in `web/` hands it out, so the lab
  wraps the browser's own `WebSocket` constructor -- the same category as
  `offersProbe`. Chrome's own offline emulation would do it and needs
  `cdproto/network`, an indirect requirement this would promote, for a weaker
  drop.
  **The restart's port and token are the lab's own**: a reconnect to a
  different port is not a reconnect, and a MINTED token does not survive a
  restart, so the browser would be holding a cookie for a credential that no
  longer exists and the resumed handshake would be refused. That is a real
  property of the program and the reason the announcement says to set
  `serve_token`; a lab about the reconnect must not be a lab about that. And
  it is **what is NOT asserted anywhere**: an operator who restarts without a
  configured token gets the refusal and has to open the new one-time URL, and
  the page cannot tell that from any other close -- it retries twelve times
  against a server that will refuse all twelve and then says the stream is not
  coming back, which is the right sentence for the wrong reason.
  Distinguishing it means a handshake's refusal reaching a page whose only
  hook is `onclose`, and a close event carries no status for a handshake
  answered with a 401.
  **The note WAS asserted on the restart and is not any more**, because the
  premise it rested on is gone and the entry at the end of this one has the
  measurement. What is still true is why it cannot go on the plain
  browser-side close: a close with the server up is answered on the first
  attempt, 250ms later, so the line is on screen for less time than any poll
  can see. Correct behaviour, and not a thing to assert around -- so the
  window is now MADE, by holding the page off its re-open, rather than found.
  **What a browser here cannot reach, asserted rather than left to be
  noticed -- and the "cannot" is now a "does not", which is corrected rather
  than left to be credited wrongly.** `Stream::Lost` -- the bound spent, "it
  did not come back: reload this page" -- needs a request the page still
  believes is live whose server never returns, and that was argued here as
  structurally unreachable, because a page that has learned its request ended
  stops retrying by design and after a restart `queue.Run`'s sweep has ended
  it. The instrument the entry at the end of this one adds reaches it: holding
  the page off its re-open for the whole bound is twelve attempts over a
  request that is still generating. It is deliberately not asserted -- that is
  forty-three seconds of wall clock for a sentence `cargo test` already
  decides and which renders through the same `<p class="stream">` the
  reconnecting one proves is drawn -- so the lab asserts the sentence it DOES
  see is the reconnecting one and **not** that one. What that assertion is NOT
  is a guard on the bound -- and
  the history of that clause is worth keeping, because it has now been wrong
  in both directions. The first wording claimed it was a guard. That was
  **measured and false of the arrangement it described**: with the note read
  on the RESTART, at `RETRY_ATTEMPTS = 1` -- 250ms, which fits no restart at
  all -- the lab was GREEN, because its first `streamNote()` landed a fraction
  of a millisecond after the close and so always read the reconnecting
  sentence, and `startServeOn` brought the second server up inside that one
  attempt anyway. **It is true again of the arrangement that replaced it**, and
  measured the same way: `noteOnALostStream` captures the line 600ms after the
  close, by which time a page with one attempt has spent it and says the
  stream is not coming back, so the "it says reconnecting" and "and not the
  other sentence" assertions both go red -- observed three times running,
  which is in fact how the stale-wasm shape below was found. The bound is
  still sealed by `cargo test` --
  `the_retry_is_bounded_and_backs_off`, red at exit 101 with
  `RETRY_ATTEMPTS = 1` -- and that remains the gate to credit, because the lab
  is a bound on 600ms of behaviour and not on the figure. Two instances of the
  thing this file keeps recording about itself in one clause: a sentence here
  asserting that a test goes red is worth exactly the exit code nobody
  checked, and both of these were found by somebody checking it.
  **And one line has no exit code anywhere, which is said rather than papered
  over.** Resetting the retry counter from `onopen` rather than from
  `Socket::open` returning Ok is the difference between a bounded retry and a
  loop, and reinstating it was GREEN: the page sets `Live` a moment early, the
  note still appears on the restart because the attempts keep failing while
  the server is down, and the consequence -- a bound that is never reached --
  shows only on a server that never returns, which is the state above. What
  stands in its place is that the constructor's Ok is documented as not a
  connection, where somebody would otherwise read it as one, and that the
  defect it replaced did exactly that. This is the
  `subtle.ConstantTimeCompare` answer to "which of these has no test".
  **The console assertion this lab already had could not fail, and the
  reinstatement is what said so.** `Socket::drop`'s defect is an uncaught
  throw, and `chromedp.Console` streams the console API's own messages while
  an exception nobody caught arrives as a different CDP event -- and
  `consoleLog.errors()` filters to "panicked" and "undecodable" besides, which
  a dropped-closure throw says neither of. So the first run of that case was
  GREEN against a check written for it. The listener is the page's own `error`
  handler now, and deleting that assertion with the defect still in place is
  green, which is what proves nothing else in the tree reports it.
  **The first version of the lab asserted the request's end REASON and it
  never arrived, which is a finding about ember and is NOT fixed here.** A
  request whose rental SIGINT cancelled came back from the restart with an
  empty `requests.err`, so the watcher had nothing to print.
  `Queue.endIfUnfinished` writes through `Store.Update` on the drain loop's
  own context, which is the context the signal cancelled, and that write
  cannot land: measured against a real store, `Update` on a cancelled context
  answers `begin: context canceled` and leaves the row exactly as it was. So
  the row keeps the `running` `ClaimNext` wrote, and the next start's
  `Interrupt` sweep turns it into `interrupted` with no reason at all. That is
  the entry above about a rental that failed after the node existed leaving
  the row reading `running`, reached one layer further out -- and the
  `context.WithoutCancel` rule this file states for a destroy, reached through
  a store write instead. Filed rather than repaired in a change about a
  websocket.
  **Reinstated, judged on exit codes, with a control at both ends of every
  pass -- and, after the restore above, on the DIFF of every file each case
  touched.** Through `cargo test` and `go test`, eight red: the note printed
  unconditionally, an unbounded retry, a terminal request retried, a flat
  cadence in place of the backoff, `RETRY_ATTEMPTS` shortened to one,
  `apply` losing its per-sequence idempotence, the handler recording no
  cursor, and the handler recording a constant one. Two more that are the
  `Envelope::next` read and are the point of it: **the read on its own is a
  COMPILE ERROR** now the field is gone -- `error[E0609]: no field next on
  type &model::Envelope` -- which is this file's own "prefer making a bug
  unrepresentable over testing for it" landing where a red test would have
  done; and **the whole first version put back, the field re-declared and the
  fold reading it, is GREEN at exit 0**, said plainly because it is the
  measurement rather than a gap. Nothing catches that shape and nothing can:
  the figure is derivable, so what makes the deletion right is the derivation
  and the backwards comment, not an exit code.
  Five through the browser: the resume from zero (red on the cursor), nothing
  re-opening at all (red on the step that never moved), the note printed
  unconditionally (red on both its halves), `Socket::drop` keeping its
  handlers (green against the console assertion that was already there, red
  against the page's own `error` handler), and the retry counter reset by the
  constructor (green, the entry above). A reworded comment was green in every
  pass, and the suite was green before the first patch and after the last.
  The harness's own first run reported BUILD-ERROR for six cases that had all
  gone red correctly, because `error: test failed` is how cargo reports a red
  suite and the detector read it as a compile failure -- the "cannot tell a
  build failure from a pass" trap inverted, and the reason that detector now
  looks for `error[E...]` and `could not compile` instead.
- **`make test-serve-lab` exited 2 for twenty-two of the twenty-six hours that
  lab had existed, and the lab was asking for a sentence a correct page does
  not print (#185).** The entry above is where the reconnect landed; this is
  what happened to its own regression test three hours later.
  `TestServe_UIResumesAProgressStreamItLost` failed at
  `the watcher never reported a stopped stream within 1m0s; it says ""`, and
  the first thing to get right is what that empty string was: `streamNote`
  answers `""` for `querySelector` returning **null** as well as for an empty
  element, so the element was ABSENT -- #185's own reading, "found and empty,
  not absent", is wrong in the direction that sends a reader to the renderer
  rather than to the state behind it.
  **MEASURED, in the browser and in the journal, rather than reasoned.** Right
  after the first `ember serve` exited: `requests.status="cancelled"`,
  `requests.err="job 1/1: context canceled"`, `ended` stamped -- and
  `window.__emberSockets.length` **2**, still **2** twenty seconds later with
  no server on the port. So the page had made no reconnect attempt at all, and
  no note, because it had learned the request was over while the socket was
  still open. `retry`'s `None if terminal => {}` and `reopen_after`'s terminal
  rule, doing exactly what they are written to do: nothing more will be said
  about a request that is done, failed, cancelled or interrupted, the final
  status and its reason are on screen, nothing is stale, and a line reporting
  the stop would be noise over a generation that has ended.
  **SO THE PAGE WAS RIGHT AND THE LAB WAS WRONG**, which is the less valuable
  of the two possible outcomes and is said plainly because the other one was
  the fear: the reconnect feature is not broken in production, and its
  headline case -- a browser-side drop with the server up, the step moving on
  afterwards, the cursor above zero and no further than the rows -- passed
  throughout.
  **WHAT BROKE IT IS THE FIX FOR A DEFECT THIS ENTRY'S OWN PREDECESSOR
  FILED.** The paragraph above ends "Filed rather than repaired in a change
  about a websocket": the lab was built on the measurement that
  `endIfUnfinished`'s write could not land on the cancelled context, so a
  restart left the row reading `running` and the watched request therefore
  live. #159 (821a92c) made that write land -- and its entry in **Money**
  opens by NAMING this lab as the thing that filed the defect it is repairing,
  "the websocket lab's first version, which spent twelve minutes waiting for a
  reason that was never going to appear". So the two changes are linked in
  this document, in both directions, 2,401 lines apart, and nobody re-ran the
  lab the first one owns. The interval is the measure of it: the lab landed at
  3d0426c, 2026-11-03 22:26:05, and 821a92c is 2026-11-04 01:29:32 and the
  **first** commit after it to touch `internal/queue/queue.go` -- so it was
  green for 3h03m27s and 14 commits, and red for **22h47m31s** and 23, five
  of which edited that very file. **The first figure written here was 20h35m
  and understated it**, because it measured to when the lab first went green
  again rather than to the commit that fixed it -- a review recomputed it from
  the timestamps and corrected it UPWARD, which is the direction a figure
  about one's own finding is least likely to be wrong in by accident. It was
  red for 88% of a 25h51m life and was found by a review of an unrelated PR.
  **THE REPAIR IS THREE DROPS, AND THE NOTE'S WINDOW IS MADE RATHER THAN
  FOUND.** A browser-side close with the server up is answered on the first
  attempt 250ms later, which is why the note was never asserted there; a
  restart ends the request, which is why it cannot be asserted there either.
  So `refuseSockets` holds the page off its re-open -- the same browser-side
  instrument as `recordSockets`, a flag the wrapped `WebSocket` constructor
  checks before reaching the real one -- over a request that is still
  generating and still billing, which is a **stronger** subject than the
  restart ever had, where the request had ended. Both branches that schedule a
  re-open are exercised: the first attempt arrives through the socket's
  `onclose` and every one after it through `Socket::open` returning Err, which
  is the only place a refused constructor can be retried from. The refusal is
  a flag and not a count of refusals, because a count would have to be
  guessed against the backoff and a guess too small clears the line before the
  poll reads it.
  **AND THE RESTART ARM NOW ASSERTS THE BRANCH THAT BROKE IT**, which no
  browser covered: nothing more will be said about a terminal request, so the
  page says nothing about the stream and re-opens nothing. It is required to
  say nothing for ten seconds with the server gone, to go on saying nothing
  for fifteen more with a server back on the port, and to have opened **no**
  handshake against it -- fifteen being over the backoff's own five-second
  cap, so a watcher still retrying cannot hide inside it. And
  `requireNoStreamNote` spends its whole window even once it has failed,
  precisely so the count after it is read fifteen seconds in rather than a
  fraction of a second in: the first shape returned on the first offending
  read, which would have made the count 0 for a page that was about to
  connect.
  **THE FIRST SHAPE OF THAT ARM WAS A RACE, AND THE NEGATIVE CONTROL IS WHAT
  SAID SO.** It let the SIGINT end the watched request and asserted on what
  the page did afterwards -- which needs the terminal row, written during the
  shutdown, to reach the browser on one more 250ms poll before the process
  exits, and nothing bounds the gap between those two events. Measured rather
  than inferred: a reinstatement case whose patch was **a reworded comment**
  went red at exactly those assertions, which is a flake, and a lab that fails
  because the machine was busy teaches people to re-run rather than to read.
  So the watched request is ended through the API while the server is up and
  healthy, and the page's knowledge is something the lab WAITS FOR: the jobs
  row says `cancelled` only once a `job_cancelled` event has been folded,
  which is this watcher's own socket rather than the queue panel's poll, and
  the queue panel's own `cancelled` then says the row is terminal server-side
  -- a fetch at two seconds against a socket at 250ms, so by then the watcher
  has had several chances at the same row. That is the opposite of the window
  the shutdown offered.
  **AND THE SECOND REQUEST HAS TO BE UNABLE TO FINISH, which the first
  version got wrong and a merge caught.** The arm needs a request the signal
  INTERRUPTS, and `waitForStatus(rider, "running")` establishes that it is
  running *now* rather than that it still will be when the signal lands a
  moment later -- the same gap as the watched request's own precondition, and
  the third time tonight that "it is in state X" was read as "it will be in
  state X". At sixty steps the rider finished inside the arm's own windows, so
  the row read `done`: terminal, so the premise passed, and carrying no reason
  at all, so the sentence the arm ends on could never appear and the failure
  surfaced sixty lines away from its cause. Measured from the journal of a
  failing run -- the watched request `cancelled`, the rider `done` -- and the
  premise now asserts `cancelled` rather than merely terminal, so a rider that
  finishes early is one named message instead of a confusing one sixty lines
  later.
  **AND RAISING THE STEP COUNT DID NOT FIX IT, WHICH IS THE INSTRUCTIVE
  HALF.** Sixty steps became six hundred and the next run's journal said
  `done` again. The gap is not one round trip: the arm's own two
  sixty-second waits and its ten-second window sit between the claim and the
  signal, so on a loaded machine the rider has MINUTES, and `steps` only buys
  a linear multiple of something already fast -- measured, sd15 at 192x512
  runs about three steps a second, so six hundred is three minutes and the
  windows ate it. What cannot be got through at all is a BATCH: ember submits
  one job at a time, so six jobs is six serial generations, and it keeps the
  property that made the rider ride the same rental rather than pay for a
  second one. The general shape, met three times in this one change: a
  precondition that reads a STATE is worth only as much as the gap between
  reading it and depending on it, and the repair is to make the state unable
  to change rather than to read it again closer to the moment.
  **AND THE ARM'S OTHER PRECONDITION SPENT THE WHOLE BUDGET TWICE, ON THE
  TRAP THIS FILE DOCUMENTS.** It waited for the watcher's `.jobs li` row to
  say "cancelled", and `waitForText` is `chromedp.Text`, which **blocks until
  the CONTEXT ends** when its selector resolves nothing instead of returning
  so that its own sixty-second bound can fire -- which is precisely what
  `waitForStreamNote` was written against and what the entry above records as
  having once cost this lab twelve minutes. Measured with a phase clock
  rather than guessed at, after two runs died at the LAST assertion with
  "context deadline exceeded" and a raised backstop made the second one
  slower rather than greener: arms one and two complete in **15 seconds**,
  and the remaining twenty minutes were all in that one wait. The signal is
  the queue panel's now, whose selector always resolves, so the bound is
  real -- and the general rule, which this file had already written and this
  change still walked into, is that a wait for something that may never
  appear must be an evaluation that can answer "not there" rather than a
  `Text` that can only answer late. Three documented hazards walked into in
  one change is the argument for reading the file one is editing.
  **A SECOND REQUEST IS WHAT KEEPS #159 IN THE ARM**, queued before the cancel
  so the drain loop claims it onto the same rental rather than renting again.
  The shutdown then has a request it really interrupts, its row is read out of
  the journal BEFORE the second server starts -- afterwards `queue.Run`'s own
  `Interrupt` sweep makes a row terminal whether or not the shutdown managed
  to, so a read taken then could not tell the two apart -- and reverting #159
  turns that read red at a named precondition. Without the second request the
  assertion would have been about a row the API cancel had already made
  terminal, which is #159's defect being waved through by the test for it.
  **AND THE ARM'S POSITIVE HALF IS THE SENTENCE #159 BUILT AND NOTHING IN A
  BROWSER ASSERTED.** "It says nothing" is satisfied by a page that renders
  nothing at all -- a wasm that failed to mount answers every absence in that
  arm -- so the lab also requires the interrupted request's recorded reason to
  be on screen, through the queue table's own poll now that there is a server
  to poll. That is the assertion the lab's first version wanted, could not
  have, and filed; so both halves of 821a92c now land in one place, which is
  the arrangement that would have caught this on the day: the reason appears
  and the note does not.
  **AND THE NOTE IS READ IN THE BROWSER, WHICH IS NOT TIDINESS EITHER.**
  `Stream::Reconnecting` is a state the PAGE bounds -- twelve attempts and
  about forty-three seconds -- so a note polled from Go gave the lab that
  whole budget to make one chromedp round trip in, which is enormous and
  still not a bound: measured, with three lab suites on one machine, this lab
  spent seven minutes waiting for the Docker lock and then read
  `Stream::Lost` on its first look, starved past the page's entire retry
  budget. So `noteOnALostStream` is one evaluation and a browser timer: the
  close, the capture 600ms later -- about two refused attempts in -- and the
  release of the refusal all happen in the page, so a Go side that stalls for
  a minute afterwards finds a page that has already recovered rather than one
  that has given up. `waitForStreamNote` is gone with it, and the two
  judgements worth keeping are a note where it was: it must not be
  `waitForText`, which cannot see an element that is absent, and "stopped"
  matches BOTH of the page's sentences, so a poll that was late read the
  wrong one as the answer.
  **AND THE WAY IT BREAKS THE SOCKET IS #194's, NOT ITS OWN, WHICH IS A
  MEASUREMENT RATHER THAN A MERGE RESOLUTION.** Two changes landed within
  hours of each other needing the same browser-side instrument and both
  edited `recordSockets`' one wrapper: #194 rewrites the URL to a path that
  answers no upgrade (`/api/no-such-socket`, a 404 rather than a 101), and
  this one threw out of `WebSocket::new` before the real constructor was
  reached. **The path form subsumes the throw and is also the better
  instrument**, so there is one mechanism: a handshake that reaches a real
  server and is refused is what a dropped stream actually looks like, while a
  constructor that throws is not a state any server can put a browser in --
  the throw was simulating a defect that does not happen. Measured rather
  than reasoned, which is the whole of why the throw went: the path holds the
  page in `Stream::Reconnecting` across the entire window this arm reads, and
  `RETRY_ATTEMPTS = 1` still turns the arm red under it.
  **WHAT UNIFYING COST IS ONE BRANCH, and it is named rather than quietly
  dropped**: a throw made `Socket::open` return Err, which is `connect`'s
  `Err(_) => retry(…)` arm and the only place a constructor that refused can
  be retried from, so that line is now reached by no lab at all. It is a
  defensive branch for a URL no page constructs, and one shared helper
  carrying two mechanisms is the drift this file keeps paying for -- but a
  reader looking for its coverage should know there is none. The path lives in
  `brokenSocketPath` because two labs now break these sockets and a second
  spelling is a second thing to keep in step with what `ember serve` 404s;
  `mendSockets` is the other half of the switch, which the restart arm needs
  and #194's does not, since that one only ever needs the retry to keep
  failing.
  **WHAT IS LOST IS THE CROSS-PROCESS CURSOR CLAIM, and it is lost rather than
  moved.** "A NEW process reading the same rows resumes from at least where
  the page had got to" required the page to reconnect after the restart, which
  a page watching a request that ended correctly does not do. Keeping it would
  mean a second request left queued across the restart, and the restarted
  server would then claim it and rent a second node for the claim's sake. What
  stands in its place is the first drop's own `resumed <= lastSeq`, which reads
  the same number back over the HTTP route and so says the cursor is in the
  rows rather than in anything one connection was holding. Stated here because
  the paragraph above used to claim it and a claim in this file is worth
  exactly what the code does.
  **SIXTY STEPS, because two of the three drops need the request to be still
  generating** and the page's own default of twenty is a job short enough that
  a slow chromedp round trip could see it finish. It costs nothing: the rental
  is local and free, the first step arrives no later for sixty than for twenty,
  and this lab never waits for the job to end -- measured, 47.6s against
  47.9s. No published limit bounds `steps`, which is why the number is not a
  guess at one.
  **Reinstated over three passes, judged on exit codes, controls at both ends
  of each, negative controls reported as not-caught, and every case ending in
  a byte diff against one backup taken once per path.** The third pass is the
  authority, because it is the only one run against the arm shapes that
  shipped and the only one whose restores could not leave a stale artifact;
  its opening and closing controls are green and report the same wasm size,
  which is the eighth shape's own witness. Six red, each at a named
  assertion: `Stream::Reconnecting` printing nothing, which is
  `"(the page printed nothing)"` at both halves of the note AND at the
  handshake pair; a terminal request's lost stream reported as a loss, at
  both of the third drop's silences; `reopen_after` ignoring `terminal`, at
  those two AND at the handshake count, which is what making
  `requireNoStreamNote` spend its whole window bought; the recorded reason
  emptied, at the one assertion that reads what the page DOES say -- the
  queue table showing two `cancelled` rows and no sentence; the lab's own
  refusal instrument made a no-op, at the note, which measures that the
  refusal is load-bearing rather than decoration; and **#159, which is the
  interaction this entry is about**, twice over. Three green and each a
  measurement rather than a gap: two negative controls, and **the interval**
  -- the OLD lab against the PRE-#159 write, the tree as it stood between
  3d0426c and 821a92c -- so the red was introduced by that commit and by
  nothing else. The whole target is 28 labs, 0 failures, 0 skips, 285s,
  exit 0.
  **#159 TWICE BECAUSE THE FIRST PATCH WAS CRUDER THAN THE DEFECT, and it went
  red one assertion earlier than the claim written for it.** Cancelling
  `writeCtx`'s context immediately makes `finish`'s write fail on EVERY path,
  so the row the arm cancels through the API never becomes terminal either and
  the red lands at that precondition -- a true red about the right writers and
  the wrong subject, which is this file's own "ask why a case went the way it
  did" catching a sentence before it shipped. The faithful revert restores the
  parameter the fix removed, `finish(ctx, …)` taking the drain loop's own
  context, which leaves the API-cancel write working and breaks only the write
  the signal races: red at the premise, naming it --
  *the request the shutdown interrupted reads "running", so the write that
  records how a Ctrl-C'd rental ended did not land.*
  **Three of the passes' own cases were void and the three shapes below are
  why**, which is this file's own expectation about a first round holding:
  every pass that reported a result had cases whose verdict was about the
  harness rather than about the patch, and chasing them is what produced the
  race the negative control found and the stale artifact no diff could see.
  **A SEVENTH HARNESS SHAPE, and it is a rollback that restored every file it
  touched.** `make web` compiles `web/src` into `internal/webapi/dist`, which
  `go build` embeds -- so a Rust case's defective wasm survived the restore of
  its `.rs` file, and the next case, which was Go-only and therefore triggered
  no rebuild, ran against it. Both Go cases of the first pass reported **red,
  AS EXPECTED, at the previous case's assertion** -- `serve_image_test.go:1961`,
  "the progress row still reads ... so nothing the resumed stream carried
  reached it", three times in a row at 247, 247 and 248 seconds, which is the
  no-reconnect defect's own signature and nothing to do with either patch. It
  reads exactly like a result: the byte diff against the backup was clean, the
  build check passed, the marker each patch introduced was present, and the
  verdict was about somebody else's defect. Caught by asking *why* a case went
  the way it did rather than by reading its exit code -- the same question that
  caught the false RED in #159's own pass -- and specifically by noticing that
  two unrelated patches produced the same failure at the same line and the
  same duration. The rule to carry forward is that a restore has to cover the
  tree the suite READS and not only the files the patch WROTE.
  **AN EIGHTH SHAPE, WHICH IS THAT FIX NOT WORKING, AND NO DIFF CAN SEE IT.**
  The second pass built the page before every run and was still contaminated,
  because `shutil.copy2` **preserves mtime**: a restored source is therefore
  OLDER than the artifacts cargo built from the patched one, so cargo's own
  freshness check declares the crate up to date and `make web` is a no-op that
  leaves the defect running. Every run after the `RETRY_ATTEMPTS = 1` case
  used that case's wasm -- including the **closing control**, which was GREEN,
  because its one note read landed inside the single remaining attempt. The
  sources were byte-identical the whole time, which is why the diff check this
  file already asks for could not report it, and it then produced two false
  REDs in the repair that followed, each looking exactly like a real defect in
  the page. The witness is the artifact rather than the source: 1,597,559
  bytes patched against 1,597,604 clean, which the third pass prints per case
  and compares at both ends. `os.utime(path, None)` after every restore is the
  fix, and the rule is that a rollback has to touch what it restores, because
  a build system that reads clocks cannot see a file that went backwards.
  **AND A NINTH, WHICH IS A LOCK TIMEOUT READ AS A VERDICT.** Four agent
  worktrees were running lab suites on this one daemon, so
  `labdaemon.Acquire` waited its ten minutes and failed with the sentence it
  is written to fail with -- and a harness that read THAT exit code as a
  result would have reported a defect as caught, or a negative control as
  caught, because the machine was busy. The third pass's own opening control
  caught it and voided the pass, which is the control-at-both-ends rule
  earning its keep for the third time in one change; a retry around each run,
  keyed on the holder sentence rather than on the exit code, is the fix. The
  general rule the three shapes share: a harness has to be able to tell "the
  suite reported" from "the suite never ran", and an exit code alone cannot.
