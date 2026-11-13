//! What is queued, running and finished.
//!
//! A PAGE AND NOT THE NARROW HALF OF A GRID, which is #139's own arithmetic:
//! this table has six columns and sat in the `1fr` side of a `1.3fr 1fr`
//! layout, so at a 1280px viewport it was 407px wide. On a page of its own it
//! is the width of the body.
//!
//! EXTRACTED AND NOT REDESIGNED. The columns, the reason row, the cancel
//! button and every comment below moved out of `views.rs` unchanged; the one
//! edit a route forces is the id cell, which was a `<button>` writing a
//! `watching` signal and is a link to a page now.

use leptos::prelude::*;
use leptos::task::spawn_local;

use crate::api;
use crate::model::{Cancelled, RequestList, Stopping};
use crate::route::{Link, Route};

/// The queue table's price cell for one request, read from the listing every
/// time it is drawn.
///
/// A NAMED FUNCTION OVER THE SIGNAL, for the reason `listed` is one: this is
/// the half `cargo test` can hold, and the reactive capture is the defect. It
/// is the shape the two cells below it already use for `stopping`, eight
/// lines under the comment that states the rule -- which the price cell did
/// not follow.
///
/// WHAT MOVES ON ITS OWN IN THIS ROW, said out loud rather than left to be
/// discovered one field at a time. `status` is in the key, so a change to it
/// rebuilds the row; `jobs` and `gpu` are written at enqueue and never again;
/// `reason` is written by `fail`/`finish` WITH the status, so the key moves
/// with it. `cost` is the one field written after the key can no longer
/// change, because `requests.cost` comes from `provision.WithNode`'s deferred
/// teardown and `finish` has already ended the request -- so it is the one
/// cell that must read the signal, and the next field somebody adds has to be
/// asked the same question.
fn price_cell(list: RwSignal<RequestList>, id: String) -> impl Fn() -> String {
    move || list.get().requests.price(&id)
}

/// Queue is what is waiting, running and finished.
#[component]
pub fn Queue(route: RwSignal<Route>) -> impl IntoView {
    let list = RwSignal::new(RequestList::default());

    // WHAT A REFUSED CANCEL SAYS. This panel had no error signal at all --
    // the only two in the page are Compose's and the file picker's -- so the
    // cancel button was the one control here whose failure went nowhere: the
    // server's 409 names the remediation ("restart ember serve to have the
    // queue reconcile it"), `api::reason` pulls that sentence out of the body
    // in full, and `let _ =` dropped it one frame later. A row that keeps its
    // `running` status and its `cancel` button is indistinguishable from a
    // click that never registered, over a card that may still be billing.
    let error = RwSignal::new(String::new());

    // WHEN THE TABLE IS NOT THE WHOLE QUEUE. Two faults say that, and both
    // used to say it by saying nothing: a row this page cannot decode, and a
    // listing it could not read at all. The second is the `if let Ok(l)`
    // below, which keeps the signal's last value -- so one undecodable
    // request left the table reading "nothing queued" for the life of the tab
    // while the two-second poll re-fetched the same bytes. One signal and one
    // line, because both mean the same thing to whoever is reading it: what
    // is on screen is less than what ember has. The first sentence is written
    // by `Rows::note`, in `model`, where cargo test reaches it.
    let unreadable = RwSignal::new(String::new());

    // The cancels this page has asked for and the rows have not caught up
    // with, holding the server's own answer about each.
    //
    // A LIST AND NOT A SINGLE ID, because queued requests are cancellable
    // independently and a slow answer about one must not hide the button on
    // another. Entries are forgotten in `reload` rather than on a timer: the
    // row's own status is what says the queue has recorded the outcome, and
    // until it does, this is the only thing on screen that knows a stop was
    // accepted.
    let stopping: RwSignal<Vec<Stopping>> = RwSignal::new(Vec::new());

    // Polled rather than pushed. The socket carries one request's progress;
    // the queue changes when somebody queues or cancels something, which is
    // rare enough that two seconds is imperceptible and cheap enough that a
    // second socket is not worth it.
    let reload = move || {
        spawn_local(async move {
            match api::get::<RequestList>("/api/requests?limit=50").await {
                Ok(l) => {
                    unreadable.set(l.requests.note());
                    // FORGOTTEN WHEN THE ROW ITSELF SAYS SO, and never
                    // before: a request gone from the listing or ended is one
                    // whose cancel button `is_cancellable` has already taken
                    // away, so there is nothing left for this to guard -- and
                    // dropping an entry any earlier would put the button back
                    // under a stop that is still travelling.
                    stopping.update(|s| {
                        s.retain(|c| {
                            l.requests
                                .items
                                .iter()
                                .any(|r| r.id == c.id && !r.is_terminal())
                        })
                    });
                    list.set(l);
                }
                // THE TABLE IS LEFT AS IT WAS, which is the only honest thing
                // to do with a listing that did not arrive -- and it is why
                // this says so out loud. A stale table nobody is told about
                // is the state this panel sat in.
                Err(e) => unreadable.set(format!(
                    "the queue could not be read, so this table is whatever it last showed: {e}"
                )),
            }
        });
    };
    reload();
    let handle = set_interval_with_handle(reload, std::time::Duration::from_secs(2));
    on_cleanup(move || {
        if let Ok(h) = handle {
            h.clear();
        }
    });

    view! {
        <section class="panel">
            <h2>"the queue"</h2>
            // "NOTHING QUEUED" IS A CLAIM AND NOT AN EMPTY TABLE, so it is
            // not said over rows that were dropped: a listing whose every row
            // this page could not read is exactly the state that used to
            // print it, and it is the wrong answer in the one direction that
            // matters -- the operator's requests are queued and about to rent
            // a card.
            <Show when=move || list.get().requests.nothing_queued()>
                <p class="hint">"nothing queued"</p>
            </Show>
            <table class="queue">
                // A `<tbody>` PER REQUEST, which is what makes the reason a
                // row rather than a sentence inside a cell.
                //
                // THE DEFECT: the reason rendered inside `td.status`, one
                // cell of a six-column table under `table-layout: auto`, in
                // the `1fr` half of the page's `1.3fr 1fr` grid. Measured at
                // a 1280px viewport: the table is 407px, the status cell gets
                // 174px of it, and `app.AttachStore`'s store refusal -- 170
                // characters naming `ember store create` -- rendered 174px
                // tall, NINE lines at two or three words each, with the row
                // 204px. Five failed requests fill the viewport and the queue
                // stops being readable. The sentence was never the problem;
                // the column was.
                //
                // WHY A ROW GROUP AND NOT TWO LOOSE `<tr>`s. Three things
                // follow from the grouping and would otherwise be three
                // separate decisions that can disagree: the `running`
                // highlight covers the pair rather than its first half, one
                // border is drawn under the request rather than between its
                // two rows, and a reader can tell which request a reason
                // belongs to -- which with loose rows is genuinely ambiguous,
                // because `td`'s bottom border would sit between them and
                // attach the reason to the request BELOW.
                //
                // THERE WERE TWO HIGHLIGHTS AND THERE IS ONE. `watching`
                // marked the row the watcher panel beside this table was
                // showing, and there is no such panel: the watcher is a page
                // of its own, so on this page nothing is being watched and a
                // mark saying otherwise would be a claim about a screen the
                // operator is not looking at. What went with it is the exit
                // code on "the mark is on the group and not on the row" --
                // see `assertReasonReadsAtTheTablesWidth`, where that is
                // recorded rather than quietly dropped.
                //
                // `<table>` takes several `<tbody>` elements by spec and this
                // is what they are for. The lab's `rowOf` matches `//tr`, so
                // it reads through this unchanged.
                <For each=move || list.get().requests.items key=|r| (r.id.clone(), r.status.clone()) let:r>
                    {
                        let running_id = r.id.clone();
                        // The eight characters the row prints, and the lab's
                        // `rowOf` finds a row by: taken outside the view so
                        // the link's children do not move the id the price
                        // cell still needs.
                        let short: String = r.id.chars().take(8).collect();
                        let cancellable = r.is_cancellable();
                        let cancel_id = r.id.clone();
                        let asked_id = r.id.clone();
                        let pending_id = r.id.clone();
                        // `Request::reason` IS the decision about which rows
                        // get a reason row, and it already has the two cargo
                        // tests that hold it: a refused request's sentence is
                        // its own and not the destroy warning, and a request
                        // with nothing to explain says nothing. A second
                        // method answering the same question here would be
                        // `queue.Cost`'s mistake -- two paths for one
                        // decision, where a reader cannot tell which is live.
                        let why = r.reason().map(str::to_string);
                        view! {
                            // BOTH CLASSES READ THE SIGNAL, and the first
                            // one did not. `list.get().running` was read
                            // at row-render time, so the highlight on the
                            // request being generated could only be as
                            // fresh as the last change to `(id, status)`
                            // -- a row claimed from `queued` does get
                            // rebuilt, so the stale window is one poll
                            // and the cost is a misplaced highlight
                            // rather than a wrong figure. Swept anyway,
                            // because leaving one read outside the
                            // closure in a row whose other one was the
                            // defect is leaving the next reader to work
                            // out which is which.
                            //
                            // ON THE GROUP RATHER THAN ON THE ROW, so that
                            // one `class:` drives the pair. Two `<tr>`s each
                            // carrying it would draw two outlines with a seam
                            // between them, which is the thing the grouping
                            // exists to avoid.
                            //
                            // AND `running` IS A LIST SINCE max_nodes (#204),
                            // so this is a membership test rather than an
                            // equality. At max_nodes>1 several requests
                            // generate at once, one per rental, and an
                            // equality against a single id would have marked
                            // one of them and left the others looking queued
                            // -- on the table an operator reads to see what
                            // their cards are doing. The `iter().any()` stays
                            // INSIDE the closure for the reason above.
                            <tbody class:running=move || list.get().is_running(&running_id)>
                                <tr>
                                    <td>
                                        // A LINK AND NOT A BUTTON, which is
                                        // the one edit a route forces in this
                                        // row. It wrote a `watching` signal a
                                        // panel on the same screen read; the
                                        // watcher is a page now, so this is
                                        // an address -- which means
                                        // middle-click opens it in a tab,
                                        // "copy link address" gives an
                                        // operator something to paste, and a
                                        // screen reader announces a link.
                                        // `Route::path` is the only spelling
                                        // of that address, so the href and
                                        // the parse cannot drift. The class
                                        // is unchanged and the STYLESHEET
                                        // moved to meet it: that rule was
                                        // `button.link` alone, so an `<a>`
                                        // carrying the same class rendered as
                                        // the browser's own blue, which in
                                        // dark mode is close to unreadable
                                        // against this panel.
                                        <Link to=Route::Request(r.id.clone()) route=route
                                            class="link">
                                            {short}
                                        </Link>
                                    </td>
                                    <td class="status">{r.status.clone()}</td>
                                    <td>{format!("{} job(s)", r.jobs.len())}</td>
                                    <td>{r.gpu.clone()}</td>
                                    <td class="price">{price_cell(list, r.id.clone())}</td>
                                    <td class="stop">
                                        // READ INSIDE A CLOSURE, which is the
                                        // keyed-list rule this page has paid
                                        // for once already: a <For> re-renders
                                        // a row only when its KEY changes, and
                                        // the key is (id, status), so a value
                                        // computed at row-render time freezes
                                        // -- which is how the progress row sat
                                        // at "starting" while the step advanced
                                        // in the database. These two read the
                                        // signal each time, so the button and
                                        // the word swap without the row being
                                        // rebuilt or the status having moved.
                                        <Show when={
                                            let id = asked_id.clone();
                                            move || stopping.get().iter().any(|c| c.id == id)
                                        }>
                                            <span class="hint stopping">{
                                                let id = asked_id.clone();
                                                move || stopping.get().iter()
                                                    .find(|c| c.id == id)
                                                    .map(|c| c.label().to_string())
                                                    .unwrap_or_default()
                                            }</span>
                                        </Show>
                                        // THE BUTTON GOES AWAY RATHER THAN
                                        // GOING GREY, because what it must not
                                        // do is send a second request: a
                                        // double-clicked button is the ordinary
                                        // case here, the Go side survives it
                                        // (`Cancel` checks the id and forgets
                                        // the channel rather than closing a
                                        // closed one), and the second ask is
                                        // the one that comes back 409 and reads
                                        // as the first having failed.
                                        <Show when={
                                            let id = pending_id.clone();
                                            move || cancellable
                                                && !stopping.get().iter().any(|c| c.id == id)
                                        }>
                                            <button class="link" on:click={
                                                let id = cancel_id.clone();
                                                move |_| {
                                                    let id = id.clone();
                                                    error.set(String::new());
                                                    // MARKED BEFORE THE
                                                    // REQUEST IS SENT, so the
                                                    // round trip is not a
                                                    // window in which the
                                                    // button is still there.
                                                    // With no answer yet the
                                                    // cell says "asking",
                                                    // which is a state of its
                                                    // own and not the word the
                                                    // server's answer gets --
                                                    // see Stopping for why
                                                    // that distinction is what
                                                    // a browser can check.
                                                    stopping.update(|s| {
                                                        s.retain(|c| c.id != id);
                                                        s.push(Stopping {
                                                            id: id.clone(),
                                                            answer: None,
                                                        });
                                                    });
                                                    spawn_local(async move {
                                                        match api::post::<_, Cancelled>(
                                                            &format!("/api/requests/{id}/cancel"),
                                                            &serde_json::json!({}),
                                                        ).await {
                                                            // The server's own
                                                            // answer replaces
                                                            // the ask, so the
                                                            // word on screen is
                                                            // the handler's and
                                                            // never this page's
                                                            // guess.
                                                            Ok(c) => stopping.update(|s| {
                                                                s.retain(|x| x.id != id);
                                                                s.push(Stopping {
                                                                    id: id.clone(),
                                                                    answer: Some(c),
                                                                });
                                                            }),
                                                            // RELEASED ON A
                                                            // REFUSAL: nothing
                                                            // was stopped, so
                                                            // the button has to
                                                            // come back -- the
                                                            // 409's remediation
                                                            // is a restart, and
                                                            // after one the
                                                            // retry is the right
                                                            // move.
                                                            Err(e) => {
                                                                stopping.update(|s| {
                                                                    s.retain(|c| c.id != id)
                                                                });
                                                                error.set(e);
                                                            }
                                                        }
                                                    });
                                                }
                                            }>"cancel"</button>
                                        </Show>
                                    </td>
                                </tr>
                                // THE REASON, ON ITS OWN ROW, AT THE WHOLE WIDTH
                                // OF THE TABLE.
                                //
                                // `Request::error` reached this page on every
                                // route and the socket and was read by no view,
                                // so a request stopped by a money ceiling or by a
                                // sampler the caps do not list said `failed` and
                                // nothing else. The operator-visible state of a
                                // failed generation was one word. Putting it in
                                // the status cell fixed that and bought the
                                // nine-line cell measured above; a `colspan` is
                                // what buys the sentence the width it needs.
                                //
                                // STILL INSIDE THE TABLE, which is a constraint
                                // and not an accident: the cancel lab counts
                                // `table.queue ~ p.error, table.queue + p.error`
                                // -- error elements that are SIBLINGS of the table
                                // -- to assert a refused cancel's message is gone.
                                // A reason moved out from under the table would
                                // put a `.why` in that neighbourhood and leave the
                                // next person to add a class there reasoning about
                                // a selector whose subject had quietly changed. It
                                // also keeps the reason out of `p.error` itself:
                                // a reason that is still true is not a refusal
                                // anybody dismissed.
                                //
                                // IN THE TABLE AND NOT UNDER THE PANEL, which is
                                // where the panel's other two lines live. A
                                // listing can hold several failed requests at once
                                // and a panel line cannot say which one it is
                                // about; the cancel refusal can, because there is
                                // one ask in flight.
                                //
                                // NOTHING AT ALL WHEN THERE IS NO REASON, rather
                                // than an empty `<tr>`: a blank row under every
                                // request would double the table's height and draw
                                // a border under each half, and a reader would
                                // take the gap for a reason that was withheld.
                                //
                                // `colspan` IS A LITERAL AND THE LAB IS WHAT
                                // CHECKS IT. Nothing in Rust can count the `<td>`s
                                // a view macro emits, so the number here and the
                                // row above it can only be held together by
                                // something that renders them:
                                // TestServe_UINamesWhyARequestFailed asserts the
                                // reason cell is as wide as the table, which goes
                                // red the day a seventh column arrives and this
                                // still says six. It cannot catch a colspan that
                                // is too LARGE, because the table model clamps one
                                // to the columns the table has -- and too small is
                                // the defect a new column produces.
                                //
                                // NO `title`. The old span carried one "because
                                // some of those are long enough to wrap a cell",
                                // which was true of the cell and is not true of a
                                // row: the whole sentence is on screen, selectable
                                // and copyable, and the remediation in it is a
                                // command an operator has to copy. A native
                                // tooltip is a second carrier of one string that
                                // cannot be selected, does not appear on a touch
                                // device and truncates differently per platform.
                                {match why {
                                    None => ().into_any(),
                                    Some(why) => {
                                        // THE BUTTON SITS WITH THE REASON,
                                        // because the reason is why somebody
                                        // is about to press it -- a retry
                                        // control in the status column would
                                        // be a click away from the sentence
                                        // explaining what it is retrying.
                                        //
                                        // OFFERED ONLY WHERE THE SERVER WOULD
                                        // ACCEPT IT: `is_retryable` spells the
                                        // same three statuses as `Retry`'s own
                                        // WHERE, so this is not a control that
                                        // exists in order to be refused. A
                                        // `done` row has a reason row of its
                                        // own sometimes and no button.
                                        let retryable = r.is_retryable();
                                        let rid = r.id.clone();
                                        view! {
                                            <tr class="reason">
                                                <td class="why" colspan="6">
                                                    {why}
                                                    <Show when=move || retryable>
                                                        {
                                                            let rid = rid.clone();
                                                            view! {
                                                                <button on:click=move |_| {
                                                                    let rid = rid.clone();
                                                                    spawn_local(async move {
                                                                        // The answer is the next
                                                                        // poll's: the row flips to
                                                                        // `queued` from the store
                                                                        // rather than from a word
                                                                        // this page chose, which
                                                                        // is the Stopping rule
                                                                        // applied to the other
                                                                        // direction.
                                                                        let _ = api::post::<_, serde_json::Value>(
                                                                            &format!("/api/requests/{rid}/retry"),
                                                                            &serde_json::json!({}),
                                                                        ).await;
                                                                    });
                                                                }>"retry"</button>
                                                            }
                                                        }
                                                    </Show>
                                                </td>
                                            </tr>
                                        }.into_any()
                                    }
                                }}
                            </tbody>
                        }
                    }
                </For>
            </table>

            // IN FULL, the same as the other two panels: `ember serve` is
            // reachable only from this machine, so there is nobody to withhold
            // the server's own sentence from -- and the alternative is a UI
            // saying nothing at all over an error that names the command that
            // fixes it.
            // A SEPARATE CLASS FROM `error`, because these are different
            // facts and the cancel lab counts `p.error` under this table to
            // assert a refusal is gone. A listing short of a row is not a
            // refusal of anything.
            <Show when=move || !unreadable.get().is_empty()>
                <p class="unreadable">{move || unreadable.get()}</p>
            </Show>

            <Show when=move || !error.get().is_empty()>
                <p class="error">"the cancel was refused: " {move || error.get()}</p>
            </Show>

            // THE OLD CAPTION HERE SAID THE OPPOSITE, and it was wrong about
            // its own premise: "a request that is already generating cannot
            // be cancelled: it would mean destroying the node, which takes
            // every other request on the same rental with it". That is true
            // of cancelling a rental and was never true of cancelling a job.
            // Replaced rather than deleted, because what an operator needs
            // to know is still the distinction -- what cancel does to the
            // rental, and therefore what it does to the money.
            <p class="hint">
                "Cancelling a generating request stops its current image and drops the jobs
                 behind it. The rental keeps going, so anything else queued onto it still
                 runs on the same warm card. Stop " <code>"ember serve"</code>
                 " to end the rental itself."
            </p>
        </section>
    }
}

#[cfg(test)]
mod tests {
    use super::price_cell;
    use crate::model::RequestList;
    use leptos::prelude::*;

    /// THE RENTAL'S COST AFTER THE STATUS CAN NO LONGER MOVE, which is #131.
    ///
    /// The status is `done` in both listings and the only thing that changes
    /// is the money, which is the exact sequence the Go side produces:
    /// `finish` writes the terminal status and `Recorder` writes the cost
    /// through afterwards, out of `provision.WithNode`'s deferred teardown.
    /// So the key `(id, "done")` is the same string before and after, and a
    /// cell built from the row it was handed showed the pre-teardown zero for
    /// the life of the tab.
    #[test]
    fn the_price_cell_reads_the_listing_and_not_the_row_it_was_built_from() {
        let sig = RwSignal::new(listing_of("req-open", "done", 0, ""));
        let cell = price_cell(sig, "req-open".to_string());
        assert_eq!(cell(), "", "a rental that has not been priced is blank");

        // The teardown's figure, with the status untouched.
        sig.set(listing_of("req-open", "done", 1_240_000, "$1.24"));
        assert_eq!(
            cell(),
            "$1.24",
            "the cost written after the status was terminal never reached the cell"
        );
    }

    /// A rider's zero is blank BY DESIGN, and that is why the cell cannot be
    /// gated on "is there a figure at all": the rental is charged to the
    /// request that opened it and to no other, so most rows are legitimately
    /// empty and the opener's is the only one with money in it.
    #[test]
    fn a_free_rental_and_a_rider_are_both_blank() {
        let sig = RwSignal::new(listing_of("rider", "done", 0, "$0"));
        assert_eq!(price_cell(sig, "rider".to_string())(), "");
    }

    /// A row that has left the listing says nothing rather than keeping what
    /// it last said. The cell is on screen because the row is, so this is the
    /// half-step where a listing has been replaced and the row has not been
    /// dropped yet -- and the honest answer there is the blank one, not a
    /// price belonging to a request that is gone.
    #[test]
    fn a_row_that_left_the_listing_prices_nothing() {
        let sig = RwSignal::new(listing_of("req-open", "done", 1_240_000, "$1.24"));
        let cell = price_cell(sig, "req-open".to_string());
        assert_eq!(cell(), "$1.24");
        sig.set(RequestList::default());
        assert_eq!(cell(), "");
    }

    fn listing_of(id: &str, status: &str, micro: i64, display: &str) -> RequestList {
        let json = serde_json::json!({
            "requests": [{
                "id": id,
                "status": status,
                "gpu": "h100-80g",
                "jobs": [{"prompt": "a paper boat"}],
                "cost": {"micro_usd": micro, "display": display},
            }],
            "running": [],
        });
        serde_json::from_value(json).expect("the listing decodes")
    }
}
