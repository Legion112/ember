//! What is billing, which is what `ember ls` prints.
//!
//! A PAGE AND NOT THE FOURTH PANEL, which closes the thing #139 measured and
//! nobody had named: since this panel landed it auto-placed into column 1 of a
//! third grid row, so the `1fr` column beside it was EMPTY on every viewport
//! over 60rem -- half a blank row, shipped, mentioned nowhere. One panel per
//! route has no such thing to place.
//!
//! EXTRACTED AND NOT CHANGED. The button, the three tables and every comment
//! below moved out of `views.rs` as they were; nothing a route decides reaches
//! this page, because it has no id and no state above it.

use leptos::prelude::*;
use leptos::task::spawn_local;

use crate::api;
use crate::model::Nodes;

/// Billing is what `ember ls` prints: every node at every provider, what a
/// destroyed node left behind, and the one charge nothing ever ends.
///
/// WHY A PANEL AT ALL, since `ember ls` exists. A store bills by the
/// gibibyte-month whether or not anything is attached, and it is the first
/// resource here that can leak while every node is correctly destroyed and
/// `ember ls` prints a clean bill of health -- and the browser had no path to
/// that number. `GET /api/nodes` was the path that looked like one: a
/// registered, authenticated route that answered 501 in every `ember serve`
/// there has ever been, because nothing assigned the scanner and no Rust type
/// existed for the answer. Wiring the Go half and leaving this panel out would
/// have moved that defect rather than closed it.
///
/// A BUTTON AND NEVER A POLL, which is the whole difference between this panel
/// and the queue table beside it. The queue reads SQLite four times a minute;
/// this reaches every configured provider -- three sweeps per press, over
/// RunPod's REST API -- on the request's own context. The offers panel has the
/// same property and the same answer. A two-second poll here would be an API
/// call per second per open tab, which is the `local.DetectGPU` lesson pointed
/// the other way: that defect was a probe on an HTTP request's context being
/// memoised, and the fix was not to cache, so the only remaining lever is how
/// often it is asked.
///
/// NOTHING IS RENDERED UNTIL SOMEBODY ASKS, and that is the sharpest thing
/// here. `Nodes::default()` has three empty slices, so its `verdict` is
/// "nothing is running and nothing was left behind" -- a clean bill of health
/// over a scan that never happened, which is the one answer this panel must
/// not give wrongly. The signal is an `Option`: `None` is "not asked" and is
/// spelled differently from "asked and found nothing", the same distinction
/// `AssetCheck.Asked` and `node.Exposure`'s second result exist for.
#[component]
pub fn Billing() -> impl IntoView {
    // None until the first press. See the paragraph above: an empty answer and
    // an answer nobody asked for must not render the same.
    let sweep = RwSignal::new(Option::<Nodes>::None);
    let error = RwSignal::new(String::new());
    let asking = RwSignal::new(false);

    let ask = move |_| {
        // The previous answer is left on screen while the next is fetched,
        // and the error is cleared: a stale listing is better than a blank
        // panel, and a stale error beside a fresh listing would be a refusal
        // the operator has already had answered.
        error.set(String::new());
        asking.set(true);
        spawn_local(async move {
            match api::get::<Nodes>("/api/nodes").await {
                Ok(n) => sweep.set(Some(n)),
                // The server's own sentence, in full. A 502 here means the
                // sweep could not be STARTED -- the journal of live nodes
                // would not read -- and a page that said "something went
                // wrong" over it would hide the one detail worth having.
                Err(e) => error.set(e),
            }
            asking.set(false);
        });
    };

    view! {
        <section class="panel">
            <h2>"what is billing"</h2>
            <p class="hint">
                "Asks every configured provider what is wearing ember's label, and
                 prices the stores. This is what "
                <code>"ember ls"</code>
                " prints; it is not asked on a timer, because it reaches the
                 providers."
            </p>

            <button on:click=ask disabled=move || asking.get()>
                {move || if asking.get() { "asking the providers…" } else { "check what is running" }}
            </button>

            <Show when=move || !error.get().is_empty()>
                <p class="error">{move || error.get()}</p>
            </Show>

            {move || match sweep.get() {
                // Not asked. Deliberately silent rather than reassuring.
                None => ().into_any(),
                Some(n) => {
                    let verdict = n.verdict();
                    let needs = n.attention > 0 || !n.unreachable().is_empty();
                    // A SECOND SENTENCE AND NOT A SECOND TERM IN THE FIRST.
                    // `verdict` counts what `ember ls` exits 2 for, which is
                    // reconcile's answer about what exists at the providers; a
                    // worker that has stopped answering is THIS process's own
                    // observation and no `ember ls` has ever seen one. Folding
                    // it in would make a number mean something different
                    // depending on whether an `ember serve` happened to be
                    // running when it was taken.
                    let wedged = n.wedged();
                    let storage = n.monthly_storage.clone();
                    let charged = !storage.is_free();
                    let total = storage.text().to_string();
                    view! {
                        <p class="verdict" class:bad=move || needs>{verdict}</p>
                        // THE FLEET FIRST, because it is what THIS process is
                        // doing and the three tables below are what exists at
                        // the providers. An operator who pressed this button
                        // while `ember serve` is renting is asking about their
                        // own rentals; the sweep is how they find out about
                        // everything else.
                        <Show when=move || wedged != 0>
                            <p class="verdict bad">
                                {move || format!(
                                    "{wedged} rental(s) are not answering and are still billing")}
                            </p>
                        </Show>
                        {fleet_table(n.workers.clone())}
                        {findings_table(n.findings.clone())}
                        {findings_table(n.leftovers.clone())}
                        {findings_table(n.stores.clone())}
                        <Show when=move || charged>
                            <p class="hint">
                                "storage: " <span class="price">{total.clone()}</span>
                                "/month, which no deadline or reaper ends."
                            </p>
                        </Show>
                    }.into_any()
                }
            }}
        </section>
    }
}

/// findings_table lays out one of the three slices.
///
/// NO KEY AND NO `<For>`, which is the rule the offers panel was repaired to
/// and the queue row after it: a key is stable identity, and there is none to
/// preserve across two presses of a button that replaces the whole answer.
/// Keying on the finding's id would freeze a row whose accrued cost had moved,
/// which is the one number on these rows that changes while a node runs.
///
/// The kind is a column rather than a badge class, because `reconcile.Kind`'s
/// own names -- expired, orphan, untracked, destroy-failed, unreachable,
/// leftover, store -- are the words `ember ls` prints and an operator
/// comparing the two should read the same vocabulary in each.
fn findings_table(fs: Vec<crate::model::Finding>) -> AnyView {
    if fs.is_empty() {
        return ().into_any();
    }
    view! {
        <table class="ledger">
            <tbody>
                {fs.into_iter().map(|f| {
                    // The accrued cost where ember knows enough to say it.
                    // "An orphaned H100 burns money in silence" is the whole
                    // reason this listing exists, and a figure is what turns
                    // that into something an operator acts on this afternoon.
                    let spent = f.accrued.as_ref().map(|m| m.text().to_string());
                    // Per month, and said so, because every other figure on
                    // these rows is a total already spent -- a reader who
                    // skimmed "$2.80" off a store row and off an orphan row
                    // would have read two very different numbers the same way.
                    let monthly = f.monthly.as_ref().map(|m| format!("{}/month", m.text()));
                    // IMMEDIATELY BEFORE THE MONEY, because it is what the
                    // money is measured from: Go accrues a finding's cost as
                    // `Estimate(price, record.Created, now)`, so these two
                    // cells are one claim split in half and a column between
                    // them would be a reader having to hold the first in mind
                    // to read the second. Empty where ember has no record,
                    // which is the same absence that leaves the cost blank --
                    // an untracked node is untracked in both columns at once.
                    let when = f.created_text();
                    view! {
                        <tr>
                            <td class="status">{f.kind.clone()}</td>
                            <td><code>{f.names()}</code></td>
                            <td>{f.reason.clone()}</td>
                            <td class="when">{when}</td>
                            <td class="price">{spent.or(monthly).unwrap_or_default()}</td>
                        </tr>
                    }
                }).collect::<Vec<_>>()}
            </tbody>
        </table>
    }
    .into_any()
}

/// fleet_table lays out the rentals this process is holding.
///
/// NO KEY AND NO `<For>`, WHICH IS THE RULE THIS PAGE WAS REPAIRED TO FOUR
/// TIMES AND IS LOAD-BEARING HERE FOR THE FIRST TIME. A key is stable
/// identity, and it is right only when a row OUTLIVES the data under it. This
/// panel replaces its whole answer on every press -- `sweep.set(Some(n))` --
/// so there is nothing to preserve across two of them, and the key an author
/// reaches for is the node id: a value that is stable precisely while the
/// things worth reading beside it (the beat, the state, the cost, the request
/// on the card) are the ones that change. A worker row keyed on its node, with
/// its heartbeat read where the key is read, is the frozen progress row again
/// -- the fifth time, in the one table whose entire purpose is a value that
/// moves. There is no `<For>` here, so it cannot be written.
///
/// THE STATE IS A WORD FROM THE SERVER AND THE COLOUR IS THE PAGE'S. Which
/// states exist and where the staleness threshold is are `queue.WorkerState`
/// and `control.HeartbeatStale`; what a page decides is which row is painted
/// as wrong, and that is `Worker::needs_attention` -- `stale` only. A red
/// `starting` would be red on every rental for its first minute, and a red
/// `quiet` would be red on every speech rental for ever.
fn fleet_table(ws: Vec<crate::model::Worker>) -> AnyView {
    if ws.is_empty() {
        return ().into_any();
    }
    view! {
        <p class="hint">
            "what this " <code>"ember serve"</code> " is renting. A rental whose create call has
             not returned is here with no node, because the money is committed from the plan."
        </p>
        <table class="ledger fleet">
            <tbody>
                {ws.into_iter().map(|w| {
                    // READ HERE, INSIDE THE CLOSURE THAT BUILDS THIS ROW, and
                    // every one of them from THIS `w`. The defect this page
                    // keeps finding is a value read once outside the thing
                    // that re-renders; the shape that prevents it is a row
                    // built entirely from its own item.
                    let bad = w.needs_attention();
                    let note = w.note();
                    let name = w.names();
                    // "nothing generating" rather than an empty cell: a blank
                    // there reads as a column that failed to load, and this is
                    // the row an operator most needs to see -- a node that is
                    // up, billing, and has nothing on it.
                    let on = if w.request.is_empty() {
                        "nothing generating".to_string()
                    } else {
                        w.request.clone()
                    };
                    // The card and the image together: "which of these is the
                    // FLUX node" is the question, and the image is what
                    // answers it.
                    let what = [w.card.clone(), w.image.clone()]
                        .into_iter().filter(|s| !s.is_empty())
                        .collect::<Vec<_>>().join(" \u{b7} ");
                    // The beat where there has been one. Empty is left empty
                    // rather than printed as "0s", because "nothing has
                    // arrived yet" and "a moment ago" are different answers
                    // and the second is the reassuring one.
                    let beat = if w.beat.is_empty() {
                        String::new()
                    } else {
                        format!("beat {} ago", w.beat)
                    };
                    let left = w.remaining.clone().map(|r| format!("{r} left")).unwrap_or_default();
                    let spent = w.accrued.as_ref().map(|m| m.text().to_string()).unwrap_or_default();
                    view! {
                        <tr class:bad=bad>
                            <td class="status">{w.state.clone()}</td>
                            <td><code>{name}</code></td>
                            <td>{what}</td>
                            <td><code>{on}</code></td>
                            <td class="status">{if note.is_empty() { beat } else { note }}</td>
                            <td class="status">{left}</td>
                            <td class="price">{spent}</td>
                        </tr>
                    }
                }).collect::<Vec<_>>()}
            </tbody>
        </table>
    }
    .into_any()
}
