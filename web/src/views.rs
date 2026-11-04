//! The page, and the pages it is made of.
//!
//! FOUR PAGES AND NOT FOUR PANELS, which is #139. They were four
//! `<section class="panel">` in one `<main>`, sharing a two-column grid in
//! which the queue got the narrow half and the watcher a `grid-column: 1 / -1`
//! hack -- and, since the billing panel landed, half of a third row was blank
//! on every viewport over 60rem. One panel per route means the grid is one
//! column at every width and there is nothing left to place by hand.
//!
//! The order the nav prints is the order the flow runs: what to rent and what
//! to generate, what is queued, and the question an operator comes back to,
//! which is what is still billing. One request's progress has no nav entry
//! because it needs an id, which only a queue row or a generation can supply.
//!
//! WHAT SURVIVES A NAVIGATION IS WHAT IS CREATED ABOVE THE MATCH, and that is
//! the whole of the state rule here: `config` is fetched once in `App` and is
//! never re-fetched per page, because it is the ceiling and the list of what
//! can be rented and neither changes while the process runs. Everything else
//! lives inside the page that uses it and goes with it -- which for the
//! watcher is a websocket, and see `views::request` for the one hazard that
//! makes.
//!
//! WHICH REQUEST IS BEING WATCHED IS THE URL'S, and there is no second
//! carrier. It used to be an `RwSignal<String>` in `App`, written by the queue
//! and by a finished `queue()`; under a router the id is in the path as well,
//! and two writable carriers of one fact is the shape this repository deletes
//! on sight -- `queue.Cost`, `Envelope::next`, `Progress::rental_cost`. The
//! signal is gone and the route is the authority.
//!
//! WHY SIGNALS AND NOT A VDOM. A watched generation emits a progress event
//! several times a second, and each one changes three numbers. With
//! fine-grained reactivity that updates three text nodes; with a vdom it
//! re-renders the page and diffs it. That is the whole reason this crate is
//! Leptos rather than something smaller.

mod billing;
mod compose;
mod queue;
mod request;

use leptos::prelude::*;
use leptos::task::spawn_local;

use crate::api;
use crate::model::Config;
use crate::route::{self, Link, Route};

use billing::Billing;
use compose::Compose;
use queue::Queue;
use request::RequestPage;

/// App is the whole document: the header, and whichever page the URL names.
#[component]
pub fn App() -> impl IntoView {
    // Loaded once. It is the ceiling and the list of what can be rented, and
    // neither changes while the process runs.
    //
    // ABOVE THE MATCH, which is what makes "never re-fetch /api/config per
    // page" structural rather than a thing to remember: the signal belongs to
    // `App`, which is mounted once for the life of the document, so a page
    // reads a config that is already there and a navigation cannot ask for it
    // again.
    let config = RwSignal::new(Config::default());
    let fatal = RwSignal::new(String::new());

    spawn_local(async move {
        match api::get::<Config>("/api/config").await {
            Ok(c) => config.set(c),
            Err(e) => fatal.set(e),
        }
    });

    // Which page is on screen, and the one authority for it.
    let route = route::install();

    view! {
        <header>
            <h1>"ember"</h1>
            <Posture config=config />
            <Nav route=route />
        </header>

        <Show when=move || !fatal.get().is_empty()>
            <p class="fatal">
                "ember serve could not be reached: " {move || fatal.get()}
            </p>
        </Show>

        <main>
            // ONE `move ||` OVER THE ROUTE, which is the whole router on this
            // side of it. A match arm's component is created when the closure
            // runs and its owner is disposed when the closure runs again, so
            // leaving a page is an unmount -- which is what releases the
            // watcher's socket and is also the hazard `views::request` is
            // written around.
            {move || match route.get() {
                Route::Compose => view! { <Compose config=config route=route /> }.into_any(),
                Route::Queue => view! { <Queue route=route /> }.into_any(),
                // THE ID COMES FROM THE PATH AND FROM NOWHERE ELSE, so a deep
                // link, a reload, the back button and a queue row are one code
                // path rather than four.
                Route::Request(id) => view! { <RequestPage id=id /> }.into_any(),
                Route::Nodes => view! { <Billing /> }.into_any(),
                Route::Unknown(path) => view! { <NoPage path=path route=route /> }.into_any(),
            }}
        </main>
    }
}

/// Nav is the way between the pages.
///
/// IN THE HEADER AND NOT IN A PAGE, because a page an operator cannot leave is
/// a page that needs the back button to be usable -- and the header is the one
/// part of this document that is mounted once and never rebuilt.
///
/// DERIVED FROM `Route::nav`, so the links and the match in `App` cannot come
/// to name different sets: `every_nav_entry_round_trips` is what holds each
/// label's href to the router that has to read it back, and the defect it
/// refuses is silent -- `ember serve` answers the app for any dotless path, so
/// a link to a page the router does not own renders "there is no page here"
/// rather than failing.
#[component]
fn Nav(route: RwSignal<Route>) -> impl IntoView {
    view! {
        <nav>
            {Route::nav().into_iter().map(|(to, label)| view! {
                <Link to=to route=route>{label}</Link>
            }).collect::<Vec<_>>()}
        </nav>
    }
}

/// NoPage is a path this build does not own.
///
/// IT NAMES THE PATH, because the server answered the app rather than a 404 --
/// `isAppRoute` is every dotless path at any depth, which is what makes a
/// reload on `/requests/{id}` work -- so the browser's own error page is not
/// what an operator with a typo or a stale bookmark sees. A blank panel there
/// reads as a page that failed to load, which is the one thing this must not
/// look like.
#[component]
fn NoPage(path: String, route: RwSignal<Route>) -> impl IntoView {
    view! {
        <section class="panel">
            <h2>"there is no page here"</h2>
            <p class="hint">
                "ember serve has no page at " <code>{path}</code>
                ". It answers this app on any address without a dot in it, so a
                 mistyped one arrives here rather than at a 404."
            </p>
            <p><Link to=Route::Compose route=route>"start again"</Link></p>
        </section>
    }
}

/// Posture is the line that says what this server will and will not do.
///
/// On screen rather than in the help text, because the one thing a person has
/// to decide here is whether to leave the process running -- and the two facts
/// that decide it answer different questions. Authentication answers who is
/// asking: another account on this machine, and anything reaching a forwarded
/// port, cannot queue a rental. `--allow-paid` is a ceiling on what the answer
/// permits. Neither bounds the other, so the line carries both.
///
/// AND THE CREDENTIAL'S LIMIT IS PRINTED WITH IT, because a control whose
/// limits are not printed is a control that gets believed past them -- the
/// rule `cli.announceToken` states for the terminal, which the operator this
/// page is for may never see: the one-time URL means somebody had a shell, and
/// `ssh -L 7777:localhost:7777` means that shell is on the other machine. So
/// the wording is that paragraph's rather than a second account of the same
/// control -- its two limits and not its two mechanisms, which is a lossy
/// compression and is said to be one at the clause itself rather than implied
/// to be lossless here.
///
/// WHAT THIS LINE USED TO SAY was "no authentication: any page in this browser
/// can reach this port", unconditionally, in every `ember serve` from the one
/// that made the token mandatory (#29) to #167 -- a verbatim survival of the
/// state that issue closed. It was false twice over by then: `webapi.New`
/// refuses a zero token, so an unauthenticated `ember serve` is not a
/// configuration this program can be in, and a page on another site is refused
/// by `checkOrigin` before the credential is even asked for -- while
/// `SameSite=Strict` means a cross-site request carries no cookie, which is
/// the half the clause was actually about. A working control reported as
/// absent, on the one line an operator reads to decide whether to walk away
/// from a process that can rent GPUs.
#[component]
fn Posture(config: RwSignal<Config>) -> impl IntoView {
    // Built outside the macro: a closure returning a view, nested inside
    // another closure inside `view!`, is more than the parser will follow.
    let ceiling = move || {
        let c = config.get();
        if c.ceilings.max_run.is_free() {
            String::new()
        } else {
            format!(", {} per run", c.ceilings.max_run.text())
        }
    };

    view! {
        <p class="posture">
            {move || {
                let c = config.get();
                if c.allow_paid {
                    view! {
                        <span class="paid">"paid providers enabled"</span>
                        " · at most " <code>{c.ceilings.ttl.clone()}</code>
                        " per rental, " <code>{c.ceilings.max_hourly.text().to_string()}</code>
                        {ceiling()}
                    }.into_any()
                } else {
                    view! {
                        <span class="free">"paid providers refused"</span>
                        " · restart with " <code>"--allow-paid"</code> " to spend money"
                    }.into_any()
                }
            }}
            // UNCONDITIONAL, AND STATIC, which is what it can be now the
            // credential is mandatory: there is no state of this program in
            // which it is false, so there is nothing for a `Config` field to
            // switch and nothing for a `cargo test` to decide. The exit code
            // available is the browser lab's, which reads this text in both
            // directions -- the sentence present AND "no authentication"
            // absent, because a check that only looked for the new wording
            // would pass against a page printing both.
            " · every request on this port needs a token; it does not keep out your own uid or
             root, and it never expires"
        </p>

        // WHAT CANNOT RUN, AND THE SENTENCE IS NOT THE SAME FOR BOTH. This
        // line used to read "no sd: block, so a queued request would have
        // nowhere to run" whenever `servable` was false -- which was true
        // while an image generation was the only workload, and became wrong
        // for the configuration speech is FOR: a `tts.voices` list with no
        // `sd:` block runs speech perfectly and this said nothing could run
        // at all, in red, above a panel that was about to work. A warning
        // that is wrong is worse than no warning, because the one it is
        // wrong about is the one the operator is looking at.
        //
        // `Config::unrunnable` is the decision and `cargo test` reaches it;
        // this is the render.
        {move || match config.get().unrunnable() {
            None => ().into_any(),
            Some(why) => view! { <p class="fatal">{why}</p> }.into_any(),
        }}
    }
}
