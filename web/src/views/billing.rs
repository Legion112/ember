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
                    let storage = n.monthly_storage.clone();
                    let charged = !storage.is_free();
                    let total = storage.text().to_string();
                    view! {
                        <p class="verdict" class:bad=move || needs>{verdict}</p>
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
                    view! {
                        <tr>
                            <td class="status">{f.kind.clone()}</td>
                            <td><code>{f.names()}</code></td>
                            <td>{f.reason.clone()}</td>
                            <td class="price">{spent.or(monthly).unwrap_or_default()}</td>
                        </tr>
                    }
                }).collect::<Vec<_>>()}
            </tbody>
        </table>
    }
    .into_any()
}
