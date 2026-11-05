# Reaching a node, and the progress socket

The far end of a connection into a node is built, never parsed from input.
And the socket that noticed its own close and never re-opened.

Back to [the threat model index](../threat-model.md) · [AGENTS.md](../../AGENTS.md)

- **The far end is built by `tunnel.RemoteLoopback(port)` and never parsed
  from input.** `direct-tcpip` connects anywhere the node can reach, including
  the provider's metadata service; a free-form remote host makes every ember
  node an open relay billed to the operator. It takes a port and not an
  address, so there is nothing for a config or a request to name.
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
  **The lab drives TWO drops on one generation, and the first is the one that
  is about the defect.** A close made by the browser, with `ember serve`
  untouched, is the only arrangement in which the generation carries on
  underneath -- which is the whole story: a frozen watcher over a card that is
  still sampling and still billing. The step on screen moves past what it was,
  and nothing else on the page reads a sampling step, so only a resumed stream
  can have brought it. A restart cannot do that, because it kills the thing
  that would produce the new events; what it proves instead is that a NEW
  process reading the same rows resumes from at least where the page had got
  to, and that the note appears and then goes away. Reinstated separately,
  each catches what the other cannot: the no-reconnect defect goes red on the
  step that never moved, and the resume-from-zero defect goes red on the
  cursor.
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
  **The note is asserted on the restart and NOT on the browser-side close**,
  and that is about the fix rather than about the lab: a close with the server
  up is answered on the first attempt, 250ms later, so the line is on screen
  for less time than any poll can see. Correct behaviour, and not a thing to
  assert around.
  **What a browser here cannot reach, asserted rather than left to be
  noticed.** `Stream::Lost` -- the bound spent, "it did not come back: reload
  this page" -- needs a request the page still believes is live whose server
  never returns, and that is the one case which cannot be followed by the
  reconnect the lab exists for: a page that has learned its request ended
  stops retrying by design, and after a restart `queue.Run`'s sweep has ended
  it. So the lab asserts the sentence it DOES see is the reconnecting one and
  **not** that one. What that assertion is NOT is a guard on the bound, and
  the first wording of this entry claimed it was: "it goes red the day
  somebody shortens the bound past a hand restart" was **measured and is
  false** -- at `RETRY_ATTEMPTS = 1`, which is 250ms and fits no restart at
  all, the lab is GREEN, because the lab's first `streamNote()` lands a
  fraction of a millisecond after the close and so always reads the
  reconnecting sentence, and `startServeOn` brings the second server up inside
  that one attempt anyway. At 0 the lab does go red, and at the browser drop's
  "the step never moved" assertion rather than at the note. The bound is
  sealed by `cargo test` -- `the_retry_is_bounded_and_backs_off`, red at exit
  101 with `RETRY_ATTEMPTS = 1` -- and that is the gate to credit. One more
  instance of the thing this file keeps recording about itself: a sentence
  here asserting that a test goes red is worth exactly the exit code nobody
  checked, and this one was found by somebody checking it.
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
