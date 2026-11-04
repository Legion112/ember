//! Which page the browser is on, and how it gets to another one.
//!
//! A HAND-ROLLED HISTORY ROUTER AND NOT `leptos_router`, which is the decision
//! #139 took and the reason is written in the file that would otherwise have
//! changed: `web/Cargo.toml` puts fetch and WebSocket in `web-sys` rather than
//! a wrapper crate for *"about eighty lines in api.rs against another
//! dependency tree"*, and routing is the same size. What it costs is two
//! entries on that features list -- `History` and `PopStateEvent` -- under a
//! comment whose own words are "FEATURES AND NOT A CRATE, which is the whole
//! reason web-sys is here rather than a wrapper". `leptos_router` is in no
//! lockfile here and would arrive with its own tree.
//!
//! THE SERVER NEEDS NOTHING, which is what makes this cheap. `ember serve`
//! answers `index.html` on any path whose last segment holds no dot, the
//! `?t=` exchange is scoped to that same predicate, and the page's own asset
//! references are root-relative since #139 slice 1 -- so a deep link, a
//! reload and a back button all arrive at the same mount with nothing on the
//! Go side to add.
//!
//! WHAT A MISSED INTERCEPT COSTS, said here because it is the one failure in
//! this module that is invisible by construction: an `<a href>` the page does
//! not handle is a FULL DOCUMENT LOAD, which re-fetches 1,921,981 bytes of
//! wasm under `Cache-Control: no-store` -- and everything still works. Nothing
//! on screen is wrong, no error is logged, and the only symptom is a page that
//! takes a second to come back. So `Link` is the one way a page names another
//! page, and the browser lab asserts a sentinel on `window` survives a
//! navigation, because a document load is the only thing that can clear one.
//!
//! AND THAT FIGURE IS WHAT THIS MODULE ADDED TO, which is written down rather
//! than left for somebody to find. Measured, release profile, same toolchain,
//! the only difference being this commit: 1,597,604 bytes before and
//! 1,921,981 after, **+324,377 (+20.3%)**. It is not `web-sys`'s two features
//! and it is not these three hundred lines -- it is that `App`'s `<main>` went
//! from four statically typed children to a `move ||` match whose five arms
//! each `into_any()`, so four large view types get a full `AnyView` vtable
//! where the compiler used to see them in place. The trade is taken and the
//! number is here because the alternative to a match over a route is not
//! having pages; `web/Cargo.toml`'s release-profile comment claiming
//! `opt-level = "s"` plus lto "takes the wasm well under a megabyte" was
//! already 50% wrong before this and is its own issue, which this makes worse
//! rather than causes.

use leptos::prelude::*;
use wasm_bindgen::prelude::*;

/// Route is one page of the app.
///
/// A CLOSED SET AND NOT A PATH STRING, so that every page the app owns is
/// named in one place and `App`'s match is exhaustive -- the compiler is then
/// what reports a page added without a view, rather than a browser showing
/// "there is no page here" for a link the nav prints.
///
/// `Unknown` CARRIES THE PATH IT WAS GIVEN, because the page it renders has to
/// be able to say which address it was asked for: a dotless typo is served the
/// app by design, so without it the operator gets a blank refusal for a URL
/// they can no longer see in a message.
#[derive(Clone, Debug, PartialEq, Eq)]
pub enum Route {
    /// What to rent and what to generate.
    Compose,

    /// What is queued, running and finished.
    Queue,

    /// One request: its progress, what it ran with, and its images.
    Request(String),

    /// What is billing, which is what `ember ls` prints.
    Nodes,

    /// A path this build does not own.
    Unknown(String),
}

impl Route {
    /// parse reads a route out of a pathname.
    ///
    /// THE PATHNAME AND NOTHING ELSE: `Location::pathname` excludes the query
    /// and the fragment, so a `?t=` the server has not yet exchanged and a
    /// `#anchor` cannot reach this at all. That is deliberate rather than
    /// incidental -- the token's whole exchange is the server's, and a router
    /// that read the query would be a second reader of a credential.
    ///
    /// AN EMPTY ID IS NOT A REQUEST, which is the one case worth a test of its
    /// own: `Request(String::new())` would mount the watcher and open
    /// `/ws?id=&after=0`, so `/requests/` has to be a page that does not
    /// exist rather than a request that does not. The same for a path below a
    /// request -- `/requests/abc/images` is a page this build does not own,
    /// and rendering the request page for it would make a wrong URL look like
    /// a right one.
    ///
    /// ONE SPLIT THAT DROPS EMPTY SEGMENTS, DOING BOTH OF THOSE AND A THIRD
    /// JOB, and the three of them are why there is no guard beside it. A
    /// leading slash, a trailing one and a doubled one are all empty segments,
    /// so `/queue`, `/queue/` and `//queue//` are one page -- which an address
    /// bar, a pasted link and a `pushState` all produce and none of them means
    /// anything by. And `/requests/` is therefore ONE segment, so
    /// `["requests", id]` cannot match with an empty id at all: the state is
    /// unrepresentable rather than refused.
    ///
    /// A `trim_end_matches('/')` AND AN `!id.is_empty()` WERE BOTH HERE FIRST
    /// AND BOTH WERE DEAD, which the reinstatement pass found rather than
    /// review: removing each one left every test in this module GREEN, because
    /// the filter had already answered what they were guarding. A branch that
    /// cannot be taken is a branch nobody can show is right, and two of them
    /// in front of one `match` is a reader believing the parser is careful
    /// about something it is not. The tests stayed, because what they assert
    /// is the ANSWER rather than the mechanism -- and the mechanism they now
    /// rest on has an exit code: deleting the filter turns four of them red.
    pub fn parse(pathname: &str) -> Route {
        let segments: Vec<&str> = pathname.split('/').filter(|s| !s.is_empty()).collect();
        match segments[..] {
            [] => Route::Compose,
            ["queue"] => Route::Queue,
            ["nodes"] => Route::Nodes,
            ["requests", id] => Route::Request(id.to_string()),
            _ => Route::Unknown(pathname.to_string()),
        }
    }

    /// path is the address this route lives at, and is the only place an href
    /// is spelled.
    ///
    /// ONE SPELLING FOR THE LINK AND THE PARSE, so a page cannot be linked to
    /// at an address its own parser does not recognise --
    /// `every_nav_entry_round_trips` is the exit code on that, and the defect
    /// it refuses is silent: the href would resolve, the server would answer
    /// the app, and the app would render "there is no page here".
    pub fn path(&self) -> String {
        match self {
            Route::Compose => "/".to_string(),
            Route::Queue => "/queue".to_string(),
            Route::Request(id) => format!("/requests/{id}"),
            Route::Nodes => "/nodes".to_string(),
            Route::Unknown(p) => p.clone(),
        }
    }

    /// nav is the pages the header links to, in the order the flow runs.
    ///
    /// NOT EVERY ROUTE, and the two that are missing are missing for different
    /// reasons: `Request` needs an id, which only a queue row or a generation
    /// can supply, and `Unknown` is not a page anybody should be sent to.
    pub fn nav() -> Vec<(Route, &'static str)> {
        vec![
            (Route::Compose, "generate"),
            (Route::Queue, "the queue"),
            (Route::Nodes, "what is billing"),
        ]
    }
}

/// intercepts answers whether a click on an internal link is this page's to
/// handle.
///
/// A FUNCTION AND NOT A CHAIN OF `if` IN THE HANDLER, for the reason
/// `Listing::answer` is one: this is a decision, it has four ways to be wrong,
/// and a browser is the only thing that could check it where it was written.
///
/// EVERY MODIFIER IS A BROWSER AFFORDANCE THIS MUST NOT TAKE AWAY.
/// ctrl-click and cmd-click open a link in a new tab, shift-click in a new
/// window, alt-click downloads it, and the middle button is the new tab again
/// -- so `preventDefault` on any of them is a page that has quietly broken
/// four things people do with links, with no error anywhere. The primary
/// button with no modifier is the only click that means "go there, here".
pub fn intercepts(button: i16, ctrl: bool, meta: bool, shift: bool, alt: bool) -> bool {
    button == 0 && !ctrl && !meta && !shift && !alt
}

/// current is the route the browser is on.
fn current() -> Route {
    let path = web_sys::window()
        .and_then(|w| w.location().pathname().ok())
        // A location with no pathname is not a state a browser can be in, so
        // this is the root rather than an `Unknown` naming nothing: the one
        // way to reach it is a window that is gone, where the page is not
        // running either.
        .unwrap_or_else(|| "/".to_string());
    Route::parse(&path)
}

/// install reads the route the page was opened at and keeps it current.
///
/// A SIGNAL AND NOT A READ PER RENDER, because `location.pathname` is not
/// reactive: a `move ||` that read it would answer the right page once and
/// then never change. The signal is the one authority for which page is on
/// screen -- including for which request the watcher is watching, which used
/// to be a second `RwSignal<String>` beside it.
///
/// THE LISTENER IS NEVER REMOVED, and that is a consequence of where it is
/// installed rather than an oversight: `App` is mounted once for the life of
/// the document and is never unmounted, so a `Closure` leaked here is leaked
/// exactly once. The hazard this module is careful about is the opposite shape
/// -- a callback outliving a component that IS unmounted -- and it lives in
/// `views::request`.
pub fn install() -> RwSignal<Route> {
    let route = RwSignal::new(current());

    let Some(window) = web_sys::window() else {
        return route;
    };
    let on_pop = Closure::wrap(Box::new(move |_: web_sys::PopStateEvent| {
        // THE LOCATION AND NOT THE EVENT'S STATE. `pushState` is given no
        // state object, so `event.state` is null for every entry this page
        // made and for the document's own first entry alike -- and the browser
        // has already moved the address bar by the time this fires, so the
        // location is both authoritative and free.
        route.set(current());
    }) as Box<dyn FnMut(_)>);
    window.set_onpopstate(Some(on_pop.as_ref().unchecked_ref()));
    on_pop.forget();

    route
}

/// go moves to a route, as a navigation the browser can come back from.
///
/// `pushState` AND THEN THE SIGNAL, in that order, because the two are one
/// move: the address bar is what a reload and a copied link read, and the
/// signal is what the page renders. A page that set only the signal would
/// render the new page under the old URL, so a reload would go back.
///
/// THE SAME ROUTE IS NOT A NAVIGATION. Clicking the link for the page you are
/// already on would otherwise push a history entry per click, so the back
/// button walks through a list of the same page -- and for a request page it
/// would also re-run the component, dropping and re-opening the progress
/// socket for nothing.
pub fn go(route: RwSignal<Route>, to: Route) {
    if route.get_untracked() == to {
        return;
    }
    let path = to.path();
    if let Some(h) = web_sys::window().and_then(|w| w.history().ok()) {
        // Ignored: a `pushState` refused is a history entry this page did not
        // get, which is worth no worse than the signal going ahead -- the page
        // then renders the right thing under the wrong address, which is what
        // a browser refusing history always costs. There is nothing to report
        // and nothing an operator could do.
        let _ = h.push_state_with_url(&JsValue::NULL, "", Some(&path));
    }
    route.set(to);
}

/// Link is how one page names another.
///
/// A REAL `<a href>` AND NOT A BUTTON, which is the whole of why this is a
/// component rather than an `on:click`. A link is what middle-click opens in
/// a tab, what the status bar previews, what a screen reader announces as a
/// link and what "copy link address" copies -- and `intercepts` is what keeps
/// every one of those working while the ordinary click stays in the page. A
/// `<button>` that called `go` would look identical and have none of it.
#[component]
pub fn Link(
    /// The route this goes to.
    to: Route,
    /// The one authority for which page is on screen.
    route: RwSignal<Route>,
    #[prop(optional)] class: &'static str,
    children: Children,
) -> impl IntoView {
    let href = to.path();
    // Read inside the closure, for the rule three keyed-`<For>` defects in
    // this page were all one instance of: `aria-current` is a property of the
    // route rather than of the link, so it has to be read each time the route
    // moves and not once when the row was built.
    let here = to.clone();
    let current = move || route.get() == here;
    view! {
        <a
            class=class
            href=href
            aria-current=move || if current() { Some("page") } else { None }
            on:click=move |ev| {
                if !intercepts(ev.button(), ev.ctrl_key(), ev.meta_key(), ev.shift_key(),
                               ev.alt_key()) {
                    return;
                }
                ev.prevent_default();
                go(route, to.clone());
            }
        >
            {children()}
        </a>
    }
}

#[cfg(test)]
mod tests {
    use super::{intercepts, Route};

    /// The three pages the nav links to, and the root.
    #[test]
    fn a_path_is_the_page_it_names() {
        assert_eq!(Route::parse("/"), Route::Compose);
        assert_eq!(Route::parse(""), Route::Compose);
        assert_eq!(Route::parse("/queue"), Route::Queue);
        assert_eq!(Route::parse("/nodes"), Route::Nodes);
        assert_eq!(
            Route::parse("/requests/080f21c0"),
            Route::Request("080f21c0".to_string())
        );
    }

    /// A TRAILING SLASH IS NOT A DIFFERENT PAGE. An address bar, a pasted
    /// link and somebody's shell completion all produce one, and a router that
    /// read `/queue/` as a page it does not own would answer "there is no page
    /// here" for the queue. A doubled slash beside it, because the one split
    /// in `parse` is what answers both and a test of one of them would leave
    /// the other to a reader's confidence.
    #[test]
    fn a_trailing_slash_is_the_same_page() {
        assert_eq!(Route::parse("/queue/"), Route::Queue);
        assert_eq!(Route::parse("//queue//"), Route::Queue);
        assert_eq!(Route::parse("/nodes/"), Route::Nodes);
        assert_eq!(
            Route::parse("/requests/abc/"),
            Route::Request("abc".to_string())
        );
    }

    /// AN EMPTY ID IS NOT A REQUEST, and this is the case with a cost behind
    /// it rather than a tidiness one: `Request("")` mounts the watcher, which
    /// opens `/ws?id=&after=0` and asks the server to stream a request that
    /// does not exist. A page that does not exist is the honest answer.
    ///
    /// THE OBVIOUS IMPLEMENTATION GETS THIS WRONG, which is what makes it
    /// worth a test over a parser whose shape cannot produce it:
    /// `pathname.strip_prefix("/requests/")` is how somebody would naturally
    /// write this, and it hands back `Some("")` for `/requests/`. Reinstated
    /// as exactly that, this goes red.
    #[test]
    fn a_request_with_no_id_is_not_a_request_page() {
        assert!(matches!(Route::parse("/requests/"), Route::Unknown(_)));
        assert!(matches!(Route::parse("/requests"), Route::Unknown(_)));
        assert!(matches!(Route::parse("/requests//"), Route::Unknown(_)));
    }

    /// A PATH BELOW A REQUEST IS NOT THAT REQUEST. `ember serve` answers the
    /// app on `/requests/abc/images` -- every dotless path at any depth -- so
    /// if this build does not own that page the only alternatives are to say
    /// so or to render the request page under an address that promises
    /// something else.
    #[test]
    fn a_page_below_a_request_is_not_the_request() {
        assert!(matches!(
            Route::parse("/requests/abc/images"),
            Route::Unknown(_)
        ));
        assert!(matches!(Route::parse("/queue/open"), Route::Unknown(_)));
    }

    /// AN UNKNOWN PATH CARRIES THE PATH, because the page it renders has to
    /// name the address it was asked for -- the server served the app rather
    /// than a 404, so the browser's own error page is not what the operator
    /// sees.
    #[test]
    fn an_unknown_path_says_which_one_it_was() {
        assert_eq!(Route::parse("/spend"), Route::Unknown("/spend".to_string()));
        match Route::parse("/no/such/page") {
            Route::Unknown(p) => assert_eq!(p, "/no/such/page"),
            other => panic!("an unowned path is not Unknown: {other:?}"),
        }
    }

    /// EVERY NAV ENTRY ROUND-TRIPS, which is the one property that stops the
    /// header linking to a page the router does not recognise.
    ///
    /// That defect is silent in the worst way available here: the href
    /// resolves, `ember serve` answers the app for it, the app mounts, and
    /// what renders is "there is no page here" -- so the only symptom is a
    /// working link to a missing page. `Route::path` being the only spelling
    /// of an href is what makes this checkable at all.
    #[test]
    fn every_nav_entry_round_trips() {
        let mut seen = Vec::new();
        for (route, label) in Route::nav() {
            let path = route.path();
            assert_eq!(
                Route::parse(&path),
                route,
                "the nav links to {path} and the router reads it as something else"
            );
            assert!(!label.is_empty(), "{path} has no label");
            assert!(
                !seen.contains(&path),
                "two nav entries share the path {path}"
            );
            seen.push(path);
        }
        assert_eq!(
            seen.len(),
            3,
            "the nav is not the three pages it was: {seen:?}"
        );
    }

    /// A ROUTE'S OWN PATH PARSES BACK TO IT, for the two routes the nav cannot
    /// carry. `Request` is the one that matters: the queue's rows build their
    /// href from `path` and a deep link arrives through `parse`, so the two
    /// have to agree about where a request lives.
    #[test]
    fn a_request_path_round_trips() {
        let r = Route::Request("080f21c0a1b2".to_string());
        assert_eq!(Route::parse(&r.path()), r);
        assert_eq!(r.path(), "/requests/080f21c0a1b2");
    }

    /// WHICH CLICKS ARE THE PAGE'S, and the four that are not are four
    /// browser affordances a `preventDefault` would take away with no error
    /// anywhere: ctrl and cmd open a new tab, shift a new window, alt
    /// downloads, and the middle button is the new tab again.
    #[test]
    fn only_a_plain_primary_click_stays_in_the_page() {
        assert!(intercepts(0, false, false, false, false));

        assert!(!intercepts(1, false, false, false, false), "middle button");
        assert!(!intercepts(2, false, false, false, false), "right button");
        assert!(!intercepts(0, true, false, false, false), "ctrl");
        assert!(!intercepts(0, false, true, false, false), "cmd");
        assert!(!intercepts(0, false, false, true, false), "shift");
        assert!(!intercepts(0, false, false, false, true), "alt");
    }
}
