//! ember's web UI.
//!
//! Client-rendered, served from loopback by `ember serve`, and talking to the
//! JSON API in `internal/webapi`. Four modules: `model` is the wire types and
//! the state they fold into, with no browser in it so `cargo test` can
//! exercise it; `api` is fetch and WebSocket; `route` is which page the
//! browser is on; `views` is the pages.
//!
//! `route` IS NOT IN `model`, although its one decision-shaped function
//! belongs to the family that lives there. `model` holds no browser by design
//! -- that is what lets `cargo test` drive every decision in it -- and a
//! router is a `History` and a `popstate` listener. So the pure parts
//! (`Route::parse`, `Route::path`, `intercepts`) sit beside the impure ones
//! and carry their own tests, which is `api::reason`'s arrangement rather than
//! an exception to `model`'s.

pub mod api;
pub mod model;
pub mod route;
pub mod views;

use wasm_bindgen::prelude::*;

/// start mounts the app.
///
/// Called by the glue `wasm-bindgen --target web` generates, which
/// internal/webapi serves beside this module.
#[wasm_bindgen(start)]
pub fn start() {
    // Panics in the console rather than a silent stop. A client-rendered page
    // that panicked without saying so looks exactly like a server that is not
    // answering, and the operator would go and restart ember.
    console_error_panic_hook();
    leptos::mount::mount_to_body(views::App);
}

/// console_error_panic_hook prints a panic where a developer will see it.
///
/// Hand-written rather than the crate of the same name: it is four lines, and
/// a dependency added to this project needs a reason written down.
fn console_error_panic_hook() {
    use std::panic;
    panic::set_hook(Box::new(|info| {
        web_sys::console::error_1(&JsValue::from_str(&format!("ember-web panicked: {info}")));
    }));
}
