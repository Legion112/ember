//! One request: its progress, what it ran with, and its images.
//!
//! A PAGE AND NOT THE PANEL WITH THE `grid-column: 1 / -1` HACK, which is
//! #139. It was the third `<section class="panel">` in one `<main>`, spanning
//! both columns of a two-column grid by a `:nth-child(3)` selector, and
//! `class:idle` at 60% opacity whenever nothing was being watched. The id is
//! in the path now, so there is no idle state to render: this page exists
//! because an address names a request.
//!
//! THE ID IS A PROP AND NOT A SIGNAL, which is what deleted the second
//! carrier. `views::Watch` took an `RwSignal<String>` written by the queue and
//! by a finished `queue()`, and under a router that id is in the URL as well;
//! two writable carriers of one fact is `queue.Cost`'s shape. The route is the
//! authority, `App`'s match hands the id over, and leaving this page is an
//! unmount rather than a signal going empty.
//!
//! AND THAT UNMOUNT IS THE ONE HAZARD THIS SLICE INTRODUCED. See `Watcher` and
//! `connect`: a pending re-open outliving its owner is a `StoredValue` read
//! after disposal, in a crate built `panic = "abort"`.

use leptos::prelude::*;

use crate::api;
use crate::model::{Envelope, Job, Progress, Rental, Request, Stream};

/// Watcher is everything one request's progress stream writes into.
///
/// A STRUCT AND NOT A LIST OF ARGUMENTS, because a re-open is the same code as
/// the first open and a Rust closure cannot name itself: `connect` and `retry`
/// are therefore free functions that call each other, and a free function
/// taking five Copy handles is a signature nobody can read. Everything in it
/// is `Copy`, which is what lets the close handler hold one.
#[derive(Clone, Copy)]
struct Watcher {
    progress: RwSignal<Progress>,
    request: RwSignal<Option<Request>>,
    stream: RwSignal<Stream>,

    /// How many re-opens have been made since the last one that succeeded.
    ///
    /// RESET BY THE SOCKET'S `onopen` AND NOT BY `Socket::open` RETURNING OK,
    /// which is the difference between a bounded retry and a loop: the
    /// constructor succeeds for a server that is not there, because the
    /// handshake is asynchronous.
    attempt: StoredValue<u32>,

    /// Held in a LOCAL store, because a web_sys::WebSocket is neither Send nor
    /// Sync -- nothing in a browser is -- and Leptos's ordinary signals
    /// require both so that the same component can run on a server. This one
    /// never will.
    socket: StoredValue<Option<api::Socket>, LocalStorage>,
}

impl Watcher {
    /// alive answers whether this watcher's component is still mounted.
    ///
    /// THE ONE THING #139 HAD TO ADD HERE, and it is the hazard rather than a
    /// tidiness. `retry` schedules `set_timeout(move || connect(...), wait)`
    /// with a backoff to five seconds, and a timeout is held by the BROWSER
    /// rather than by Leptos's arena -- so under a router it fires up to five
    /// seconds after the operator navigated away, on an owner that has been
    /// disposed. Every signal and every `StoredValue` in this struct is an
    /// arena handle whose non-`try` read is an `expect`, and this crate is
    /// built `panic = "abort"`: the first read traps the whole wasm instance,
    /// which is a page that stops dead with a `RuntimeError` in the console
    /// and nothing else.
    ///
    /// IT WAS AN EPOCH COUNTER, AND THE EPOCH GUARD WAS THE READ THAT WOULD
    /// FAULT. `connect`, `on_open`, `on_envelope` and `retry` each opened with
    /// `w.epoch.get_value()`, which answered "is this the open the page is
    /// still waiting for" -- a real question while `Watch` was a static child
    /// of `App` that was never unmounted, and no answer at all to "is the page
    /// still here". The counter is gone with it rather than guarded: it was
    /// bumped by the effect that re-opened when a `watching` signal changed,
    /// and this component is created per id by the route, so there is one open
    /// per mount and the comparison could never be false. A guard whose
    /// condition cannot be taken is a guard nobody can show is right.
    ///
    /// ONE MECHANISM AND NOT TWO. `set_timeout_with_handle` plus `on_cleanup`
    /// is what `Queue`'s poll does and is the obvious second half, and it is
    /// deliberately not here: `clearTimeout` cannot un-queue a callback the
    /// browser has already dispatched, so it is not sufficient on its own,
    /// while this is -- `try_get_value` answers `None` for a disposed arena
    /// entry whenever it is asked. A clear beside it would be a second path
    /// for one check whose other path is the live one, which is `queue.Cost`'s
    /// mistake, and it would make neither half's exit code its own. What it
    /// would have bought is one callback not running, which costs nothing.
    ///
    /// `attempt` AND NOT A FIELD OF ITS OWN, because every handle in this
    /// struct is created in one component body and so belongs to one owner:
    /// disposal takes all of them at once, and a dedicated "am I alive" flag
    /// would be a second thing able to disagree with the five it speaks for.
    fn alive(&self) -> bool {
        self.attempt.try_get_value().is_some()
    }
}

/// connect opens the stream, from the cursor the watcher has reached.
///
/// `progress.cursor` AND NOT ZERO, which is the defect. `Socket::open` has
/// taken an `after` since it was written and its one caller passed the literal
/// `0`, so a reconnect -- had there been one -- would have replayed the whole
/// generation, and a page reload and a dropped connection were not the same
/// thing to this page at all: the first re-opened from zero and the second did
/// nothing. Resuming is safe to overlap either way, because `Progress::apply`
/// is idempotent per sequence number; what the cursor buys is that it does not
/// have to.
///
/// THE FIRST LINE IS THE LIVENESS GUARD, and it is first because everything
/// after it reads an arena handle. This is the one entry point a disposed owner
/// can be reached through: the browser holds the timeout `retry` scheduled, so
/// it calls this with no reference to anything Leptos owns.
fn connect(w: Watcher, id: String) {
    if !w.alive() {
        return;
    }
    let after = w.progress.get_untracked().cursor;

    // NO GUARD ON THE THREE HANDLERS, and that is a measurement rather than an
    // omission. They are reached only through the `api::Socket` stored in
    // `w.socket`, which is an arena entry: disposing the owner drops it, and
    // `Socket::drop` clears `onopen`, `onmessage` and `onclose` BEFORE closing
    // the connection -- the ordering that type already carries for its own
    // reason. So a disposed owner has no socket to call them. A guard here
    // would be a branch nothing can take, which this repository refuses even
    // when it looks free.
    let on_open = move || {
        w.attempt.set_value(0);
        w.stream.set(Stream::Live);
    };

    let on_envelope = move |env: Envelope| {
        w.progress.update(|p| p.fold(&env));
        if let Some(r) = env.request {
            w.request.set(Some(r));
        }
    };

    let closed_id = id.clone();
    let on_closed = move || retry(w, closed_id.clone());

    match api::Socket::open(&id, after, on_open, on_envelope, on_closed) {
        // The previous socket is dropped here, which closes it: a connection
        // to a generation nobody is watching holds a goroutine on the other
        // end polling for it. Its handlers are cleared first, so its close
        // cannot report a stop over this one's state -- see Socket::drop.
        Ok(s) => w.socket.set_value(Some(s)),
        // The constructor itself refused. There is no socket, so no `onclose`
        // is coming and this is the only place the retry can be scheduled
        // from.
        Err(_) => retry(w, id),
    }
}

/// retry schedules the next open, or says the stream is gone.
///
/// GUARDED FOR `connect`'s REASON: it is reached from `connect`, which the
/// browser's own timeout calls, so a disposed owner arrives here too.
fn retry(w: Watcher, id: String) {
    if !w.alive() {
        return;
    }
    let terminal = w
        .request
        .get_untracked()
        .map(|r| r.is_terminal())
        .unwrap_or(false);
    let attempt = w.attempt.get_value();

    match crate::model::reopen_after(terminal, attempt) {
        Some(wait) => {
            w.attempt.set_value(attempt + 1);
            w.stream.set(Stream::Reconnecting);
            let again = id.clone();
            set_timeout(move || connect(w, again), wait);
        }
        // THE STATE IS LEFT ALONE FOR A TERMINAL REQUEST, which is the one
        // case where a stream that ended has still said everything there was:
        // the final status and the files are on screen, nothing is stale, and
        // a line reporting the stop would be noise over a finished
        // generation. A spent bound is the opposite -- the step on screen is
        // the last one that arrived and the generation may still be running.
        None if terminal => {}
        None => w.stream.set(Stream::Lost),
    }
}

/// RequestPage is the live progress of the request the URL names.
///
/// `id: String` AND NOT A SIGNAL, which is the state lift #139 asked for.
/// `Watch` took an `RwSignal<String>` and re-opened its socket from an
/// `Effect` watching it; the id is in the path now, so `App`'s route match
/// creates this component per request and disposes the last one. What that
/// deletes is the empty case -- there is no `class:idle`, no "pick a request"
/// hint and no `socket.set_value(None)` branch, because an address naming no
/// request is `Route::Unknown` and never reaches here.
///
/// ONE OPEN PER MOUNT. The cursor starts at zero, so the backlog arrives
/// before anything live and a request that finished a minute ago is as
/// watchable as one generating now. The cost is that coming BACK to a long
/// generation replays its whole tail: `Progress::apply` is idempotent per
/// sequence number so nothing is wrong, and it is rows over loopback rather
/// than anything billed. Worth knowing, not worth a second carrier for the
/// cursor to live in.
#[component]
pub fn RequestPage(id: String) -> impl IntoView {
    let progress = RwSignal::new(Progress::default());
    let request: RwSignal<Option<Request>> = RwSignal::new(None);
    // WHAT THE SOCKET IS DOING, AND NOW WITH A READER. This was an
    // `RwSignal<bool>` with three writers and no reader at all, so a stream
    // that ended mid-generation left the last step on screen for ever under a
    // panel captioned as though the card were sampling -- while the
    // generation carried on and was billed.
    let stream = RwSignal::new(Stream::Live);
    let socket: StoredValue<Option<api::Socket>, LocalStorage> = StoredValue::new_local(None);
    let attempt = StoredValue::new(0u32);

    let w = Watcher {
        progress,
        request,
        stream,
        attempt,
        socket,
    };

    connect(w, id.clone());

    let short: String = id.chars().take(8).collect();

    view! {
        <section class="panel">
            <h2>"what is happening"</h2>
            // THE ID, BECAUSE THIS IS AN ADDRESS SOMEBODY MAY HAVE ARRIVED AT
            // COLD. A deep link, a reload or a bookmark opens this page with
            // nothing on screen until the socket's first envelope lands, and
            // an operator who pasted a request id has to be able to see that
            // the page is about the one they pasted.
            <p class="hint">"request " <code>{short}</code></p>

                // THE STREAM STOPPED, which the page could not say at all.
                //
                // In the register the money row beside it already uses for a
                // fact ember cannot do anything about -- "not priced yet",
                // "—" -- because that is what this is: a dropped socket, a
                // restarted `ember serve`, a laptop that slept or a forwarded
                // port that blipped all look the same from here, and the
                // generation may well still be running and billing. So the
                // sentence says the one thing that is true, which is that the
                // step on screen is the last one that arrived.
                //
                // ABSENT WHILE THE SOCKET IS OPEN, which is half the point: a
                // line that is always there is not a signal. Stream::note is
                // what decides, in `model`, where cargo test reaches it.
                {move || match stream.get().note() {
                    None => ().into_any(),
                    Some(note) => view! { <p class="stream">{note}</p> }.into_any(),
                }}

                <dl class="rental">
                    <Show when=move || !progress.get().node.is_empty()>
                        <dt>"node"</dt>
                        <dd><code>{move || progress.get().node}</code></dd>
                    </Show>
                    <Show when=move || !progress.get().resident.is_empty()>
                        <dt>"weights resident"</dt>
                        // ember's own measurement, not the server's reported
                        // figure: sd-server said 551ms for weights that are
                        // 56 GiB.
                        <dd>{move || progress.get().resident} " of loading, paid once"</dd>
                    </Show>
                    // THE RENTAL, AND THE CAPTION NAMES THE NUMBER IT SHOWS.
                    //
                    // This row was gated on `progress.spent` -- the card time
                    // the queue emits per job -- captioned "card time", and
                    // rendered `request.cost`, which is the whole rental:
                    // written from `run_cost`, which arrives from
                    // provision.WithNode's DEFERRED teardown, and routed by
                    // Queue.lifecycle to the request that opened the rental
                    // and to no other. So it appeared at the first completed
                    // job and read `$0` until the node was destroyed, and
                    // `$0` for ever on a rider -- a money row of zeros, which
                    // reads as free.
                    //
                    // Which of the five states it is in is Progress::rental,
                    // so it can be checked without a browser; what is here is
                    // the sentence each one gets. A dash for the rider, which
                    // is the cell `ember history` prints, and words for the
                    // two states that have no figure at all -- because the
                    // distinction between "nobody has priced this yet",
                    // "somebody else paid for it" and "it cost nothing" is
                    // the whole content of the row.
                    //
                    // NO DOLLAR FIGURE FOR THE CARD TIME, and that is a limit
                    // rather than an omission: `Money` arrives rendered by Go
                    // because this page must not be a second formatter of
                    // money, and nothing on the browser's wire carries a
                    // rendered card-time figure -- `JobRun.Cost` is kept off
                    // `Job` on purpose, so that the figure an operator is
                    // billed against is not settable by the thing being
                    // billed. `ember history` is where that column lives.
                    {move || {
                        let cost = request.get().map(|r| r.cost).unwrap_or_default();
                        match progress.get().rental() {
                            Rental::Silent => ().into_any(),
                            Rental::Pending => view! {
                                <dt>"rental"</dt>
                                <dd>"not priced yet"
                                    <span class="hint">
                                        " — what a rental cost is recorded when its node
                                         is destroyed"
                                    </span>
                                </dd>
                            }.into_any(),
                            Rental::Accruing => view! {
                                <dt>"rental"</dt>
                                <dd>"accruing"
                                    <span class="hint">
                                        " — card time has been charged to this request;
                                         what the rental cost is recorded when its node
                                         is destroyed"
                                    </span>
                                </dd>
                            }.into_any(),
                            Rental::Rode => view! {
                                <dt>"rental"</dt>
                                <dd>"—"
                                    <span class="hint">
                                        " this generated on a rental another request
                                         opened, and that request is the one charged
                                         for it"
                                    </span>
                                </dd>
                            }.into_any(),
                            Rental::Charged => view! {
                                <dt>"rental"</dt>
                                <dd>{cost.text().to_string()}
                                    // SAID IN WORDS, because `$0` on its own
                                    // is the reading this whole row exists to
                                    // stop: here it is the recorded figure
                                    // and the rental really was free, which
                                    // is a different claim from the four
                                    // zeros above it.
                                    <Show when=move || cost.is_free()>
                                        <span class="hint">" — this rental was free"</span>
                                    </Show>
                                </dd>
                            }.into_any(),
                        }
                    }}
                </dl>

                <Show when=move || !progress.get().failure.is_empty()>
                    // The one an operator must not miss.
                    //
                    // THIS SENTENCE IS ABOUT A DESTROY THAT FAILED AND
                    // NOTHING ELSE, which is why the request's own reason is
                    // a separate line below rather than another writer of
                    // `Progress::failure`. `destroy_failed` is the one kind
                    // folded into that field, and "the node may still be
                    // running" is a claim about a node that was created: over
                    // a request a money ceiling refused there was never one,
                    // and a false alarm on the page an operator reads to
                    // decide whether to check their bill is worse than
                    // silence. Same shape as the `proj` caption that was true
                    // of one preview mode and wrong for the other two.
                    <p class="fatal">
                        "THE NODE MAY STILL BE RUNNING AND BILLING: "
                        {move || progress.get().failure}
                        " — check " <code>"ember ls"</code>
                    </p>
                </Show>

                // WHY IT ENDED, which the watcher could not say at all.
                //
                // The reason is on the row, not in the stream: `queue.fail`
                // writes `requests.err` for a request refused before a
                // provider was reached and emits nothing, because there is no
                // sink for a rental that was never opened. So this reads
                // `Request::error` -- see `Request::reason` for why it is not
                // gated on `failed`, and `wsCursor` in `internal/webapi` for
                // the push that makes it arrive: the socket used to write an
                // envelope only when an EVENT was new, so a row that changed
                // with nothing behind it never reached an open tab.
                //
                // NO VERDICT AND NO REMEDIATION, deliberately. The server's
                // own sentence already carries the remediation where there is
                // one -- a ceiling names the figures, a refused sampler names
                // the list -- and a line ember added about what to do next
                // would be a guess over the only one of these states it
                // cannot identify from here.
                {move || match request.get().as_ref().and_then(Request::reason) {
                    None => ().into_any(),
                    Some(why) => view! {
                        <p class="why">"this request ended early: " {why.to_string()}</p>
                    }.into_any(),
                }}

                // A PLAIN MAP AND NOT <For>, and the difference is a bug
                // that only a browser found. A keyed <For> re-renders a row
                // when its KEY changes -- that is what the key is for -- and
                // a job's key is its index, which never changes. So the row
                // rendered once, when the job first appeared, and then froze:
                // the step never advanced and "starting" stayed on screen
                // while the finished image was already in the gallery below
                // it. Re-rendering a handful of rows on each progress event
                // costs nothing; a frozen progress display costs the operator
                // their belief that ember is working.
                <ol class="jobs">
                    {move || progress.get().jobs.into_iter().map(|j| view! {
                        <li>
                            <span class="label">
                                {format!("job {}/{}", j.index, j.total)}
                            </span>
                            <code>{j.desc.clone()}</code>
                            {
                                if j.cancelled && !j.done {
                                    // GUARDED ON `!done`, because a job can
                                    // be both: the server finished the last
                                    // step before it read the cancel, so
                                    // the images arrived anyway. "cancelled"
                                    // printed over a row whose picture is in
                                    // the gallery below is the caption that
                                    // is true of one case and wrong for the
                                    // other, so files win.
                                    let why = if j.cancel_error.is_empty() {
                                        " cancelled".to_string()
                                    } else {
                                        format!(" cancelled, but it will finish and be paid for: {}",
                                                j.cancel_error)
                                    };
                                    view! { <span class="step">{why}</span> }.into_any()
                                } else if j.done {
                                    view! {
                                        <span class="done">
                                            {format!(" {} file(s)", j.files)}
                                        </span>
                                    }.into_any()
                                } else if j.total_steps > 0 {
                                    // A pass and a step within it, never a
                                    // percentage: passes restart for each
                                    // batch item and each stage, so a bar
                                    // built from them runs to the end and
                                    // starts again, which reads as a hang.
                                    view! {
                                        <span class="step">
                                            {format!(" pass {}, step {}/{}",
                                                     j.pass, j.step, j.total_steps)}
                                        </span>
                                    }.into_any()
                                } else {
                                    view! { <span class="step">" starting"</span> }.into_any()
                                }
                            }
                        </li>
                    }).collect::<Vec<_>>()}
                </ol>

                // WHAT IT RAN WITH, which the page could not say at all.
                //
                // Every layer below this render was already done: genstore
                // keeps the parameters as one JSON column, `viewRequest`
                // sends all of them outbound under a comment arguing they
                // must be shown, and `model::Job` decodes twenty-three
                // fields. Twenty-two of them were read only by the COMPOSE
                // form, which builds a new job, so a past generation showed
                // a truncated prompt and a file count and nothing a
                // reproduction needs. Measured: making the parameters
                // `skip_deserializing` left `cargo test` green but for one
                // round-trip assertion whose own comment says "a job on
                // screen has to be the job that was queued" -- so the claim
                // was in the tree and the render was not.
                //
                // INSIDE THE CLOSURE, and that is not style. Three keyed
                // `<For>` defects in this page came from reading a signal
                // outside the closure that renders it -- the frozen progress
                // row, the stale offers table and the money column that read
                // `$0` for every paid rental -- and `request` is `None` until
                // the socket's first envelope arrives, so a table built once
                // against a request id would be those defects with twenty
                // more fields. A plain map for the same reason the job rows
                // above are one.
                //
                // IT MOVED, AND THE ARRANGEMENT HELD. #181 sited this in the
                // watcher and wrote down that #139 would put it on a
                // `/requests/{id}` page behind a router that did not exist
                // yet, with `Parameters` taking a `Job` and nothing else so
                // that the slice would move the call site and not the
                // component. Slice 3 is that slice: this call site is on the
                // request page now and `Parameters` is byte-for-byte what it
                // was. What did NOT move is the closure, which is the half
                // that paragraph was really about.
                {move || {
                    let Some(r) = request.get() else {
                        return ().into_any();
                    };
                    if r.jobs.is_empty() {
                        return ().into_any();
                    }
                    let total = r.jobs.len();
                    r.jobs
                        .into_iter()
                        .enumerate()
                        .map(|(i, job)| view! { <Parameters job index=i total=total /> })
                        .collect::<Vec<_>>()
                        .into_any()
                }}

                // The jobs that never started have no rows above -- nothing
                // ever named them -- so this is the only place that can say
                // they are not coming.
                // `!= 0` and not `> 0`: the view macro ends an attribute at
                // the first `>`, so the comparison would parse as the end of
                // the tag and a stray `0`, and clippy refuses the
                // parentheses that would fix it. `dropped` is clamped at
                // zero where it is set.
                <Show when=move || progress.get().dropped != 0>
                    <p class="hint">
                        {move || format!("cancelled: {} job(s) were dropped and will not run",
                                         progress.get().dropped)}
                    </p>
                </Show>

                <Gallery request=request progress=progress />

                <Show when=move || !progress.get().log.is_empty()>
                    <details>
                        <summary>"what the node said"</summary>
                        <pre class="log">{move || progress.get().log_text()}</pre>
                    </details>
                </Show>
        </section>
    }
}

/// Parameters is every setting one job of a past generation ran with.
///
/// SELF-CONTAINED AND TAKING A `Job`, which is what let #139's
/// `/requests/{id}` page move the call site rather than this component --
/// written down by #181 as a prediction and spent by slice 3, which moved one
/// `view!` line into another file and changed nothing here. It reads no signal
/// and holds no state: the job's parameters are written at enqueue and never
/// change, so what has to be reactive is the request arriving, which is the
/// caller's closure.
///
/// THE WORDING IS `Job::settings` AND NOT A CHAIN OF `if` HERE. "Unset"
/// against "zero" against "off" is a decision, and this page's settled
/// pattern puts a decision in `model` where `cargo test` reaches it --
/// `Rows::note`, `Request::reason`, `Stream::note`. A precedence written
/// inside a view macro can only be checked by a browser.
///
/// SEED AND SAMPLER ARE WHAT A REPRODUCTION TURNS ON, so they are the first
/// two rows and the summary names them: an operator scrolling a finished
/// generation should not have to open anything to find out whether the seed
/// was pinned.
#[component]
fn Parameters(job: Job, index: usize, total: usize) -> impl IntoView {
    let settings = job.settings();
    let seed = settings
        .iter()
        .find(|s| s.label == "seed")
        .map(|s| s.value.clone())
        .unwrap_or_default();
    let sampler = settings
        .iter()
        .find(|s| s.label == "sampler")
        .map(|s| s.value.clone())
        .unwrap_or_default();
    // OPEN FOR A SINGLE JOB AND CLOSED FOR A BATCH. One job is the ordinary
    // case and its parameters are the answer; sixteen expanded blocks are a
    // page nobody can read, and the summary still carries the two that
    // matter.
    let open = total == 1;
    view! {
        <details class="params" open=open>
            <summary>
                {format!("job {} of {total}: seed {seed}, sampler {sampler}",
                         index + 1)}
            </summary>
            // THE FULL PROMPT, because the row above it is the server's
            // composed `desc`, which is truncated -- so the one thing an
            // operator most often wants to copy was the one thing the page
            // would not give them whole.
            <p class="prompt"><code>{job.prompt.clone()}</code></p>
            <Show when={
                let n = job.negative_prompt.clone();
                move || !n.is_empty()
            }>
                <p class="prompt">
                    "negative: " <code>{job.negative_prompt.clone()}</code>
                </p>
            </Show>
            <dl class="params">
                {settings.clone().into_iter().map(|s| view! {
                    <dt>{s.label}</dt>
                    <dd>{s.value}</dd>
                }).collect::<Vec<_>>()}
            </dl>
        </details>
    }
}

/// Gallery is the results, and the preview frame while there are none.
#[component]
fn Gallery(request: RwSignal<Option<Request>>, progress: RwSignal<Progress>) -> impl IntoView {
    view! {
        <div class="gallery">
            <For
                each=move || request.get().map(|r| r.results().cloned().collect::<Vec<_>>())
                    .unwrap_or_default()
                key=|i| i.path.clone()
                let:im
            >
                <figure>
                    <a href=im.path.clone() target="_blank">
                        <img src=im.path.clone() alt=format!("job {}", im.job) />
                    </a>
                    <figcaption>{format!("job {}", im.job)}</figcaption>
                </figure>
            </For>

            // The progress frame, while a job is still sampling. Cache-busted
            // on the step, because the server sends it `no-store` and the file
            // is written over -- without the step in the URL a browser shows
            // the first frame for the whole generation.
            {move || {
                let Some(r) = request.get() else { return Vec::new() };
                // Said for `proj` and NOT for the two decoding modes, because
                // it is only true of one of them: `proj` projects the latent
                // and a 512x512 generation previews as a 64x64 smear, while
                // `tae` and `vae` decode a real image at full size. One
                // caption for both would be wrong half the time, and wrong in
                // the direction that makes a working preview look broken.
                let latent = r.preview == "proj";
                // The predicate is Progress::sampling and not a closure
                // here, so a test can reach it: a component this deep inside
                // a view macro needs a DOM, and "which jobs still have a
                // frame coming" is a decision rather than a layout.
                progress.get().sampling().into_iter()
                    .filter_map(|j| {
                        let im = r.images.iter().find(|i| i.preview && i.job == j.index)?;
                        Some((j.index, format!("{}&step={}", im.path, j.step)))
                    })
                    .map(|(job, src)| view! {
                        <figure class="preview">
                            <img src=src alt=format!("job {job} in progress") />
                            <figcaption>
                                {format!("job {job}, in progress")}
                                <Show when=move || latent>
                                    <span class="hint">" (the latent, not the output)"</span>
                                </Show>
                            </figcaption>
                        </figure>
                    })
                    .collect::<Vec<_>>()
            }}
        </div>
    }
}
