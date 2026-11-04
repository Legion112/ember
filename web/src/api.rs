//! fetch and WebSocket, straight from web-sys.
//!
//! No helper crate: about a hundred lines here against another dependency
//! tree, and what is needed is narrow -- four GETs, three POSTs (one of them
//! a file) and one socket.
//!
//! EVERYTHING IS SAME-ORIGIN AND RELATIVE. The server refuses any request
//! whose `Origin` is not its own and any whose `Host` is not a loopback name
//! at its own port, so a URL with a host in it would be refused by the thing
//! it was pointed at. Relative paths also mean the port `ember serve` chose
//! needs no plumbing: whatever served this page is what answers.

use serde::de::DeserializeOwned;
use wasm_bindgen::prelude::*;
use wasm_bindgen_futures::JsFuture;
use web_sys::{Request, RequestInit, Response};

/// Error is what a call came to, as something a UI can print.
pub type Error = String;

fn window() -> Result<web_sys::Window, Error> {
    web_sys::window().ok_or_else(|| "no window".to_string())
}

async fn send(req: Request) -> Result<(u16, String), Error> {
    let resp = JsFuture::from(window()?.fetch_with_request(&req))
        .await
        .map_err(describe)?;
    let resp: Response = resp.dyn_into().map_err(|_| "not a response".to_string())?;
    let text = JsFuture::from(resp.text().map_err(describe)?)
        .await
        .map_err(describe)?;
    Ok((resp.status(), text.as_string().unwrap_or_default()))
}

/// get fetches and decodes.
pub async fn get<T: DeserializeOwned>(path: &str) -> Result<T, Error> {
    let req = Request::new_with_str(path).map_err(describe)?;
    let (status, body) = send(req).await?;
    if status != 200 {
        return Err(reason(status, &body));
    }
    serde_json::from_str(&body).map_err(|e| format!("{path}: {e}"))
}

/// post sends JSON and decodes the answer.
pub async fn post<B: serde::Serialize, T: DeserializeOwned>(
    path: &str,
    body: &B,
) -> Result<T, Error> {
    let opts = RequestInit::new();
    opts.set_method("POST");
    let json = serde_json::to_string(body).map_err(|e| e.to_string())?;
    opts.set_body(&JsValue::from_str(&json));

    let req = Request::new_with_str_and_init(path, &opts).map_err(describe)?;
    req.headers()
        .set("Content-Type", "application/json")
        .map_err(describe)?;

    let (status, text) = send(req).await?;
    // 200 and 202 are both success here: a queued request is accepted, not
    // finished, and the server says so.
    if status != 200 && status != 202 {
        return Err(reason(status, &text));
    }
    serde_json::from_str(&text).map_err(|e| format!("{path}: {e}"))
}

/// post_bytes uploads a file's contents and decodes the answer.
///
/// RAW BYTES AND NOT BASE64, which is a decision about the one route in this
/// program that carries a file. The generation API does take base64, but that
/// is the hop from `ember serve` to the node; this hop is a browser to
/// loopback, and encoding here would make the body a third larger and put a
/// base64 encoder in the page for no gain. The server derives the key from
/// the content, so there is nothing else to send: no filename, no declared
/// type, no form envelope.
///
/// NOT multipart/form-data FOR THE SAME REASON. A multipart body carries a
/// filename, and a filename is the thing this path is designed not to have --
/// it would arrive at a server that must then be trusted never to join it
/// onto a path. One file per request, as the bytes, is the shape with nothing
/// in it to refuse.
///
/// `Content-Type: application/octet-stream` is set because something has to
/// be, and the server does not consult it: the bytes decide what they are,
/// and a declared type is a claim rather than a check.
pub async fn post_bytes<T: DeserializeOwned>(path: &str, bytes: &[u8]) -> Result<T, Error> {
    let opts = RequestInit::new();
    opts.set_method("POST");
    // A copy into a JS-side array, which is what a fetch body has to be.
    // `from` copies; the alternative -- a view onto the wasm heap -- is a
    // pointer into memory the allocator may move while the request is in
    // flight.
    let buf = js_sys::Uint8Array::from(bytes);
    opts.set_body(&buf);

    let req = Request::new_with_str_and_init(path, &opts).map_err(describe)?;
    req.headers()
        .set("Content-Type", "application/octet-stream")
        .map_err(describe)?;

    let (status, text) = send(req).await?;
    // 201: the upload created something that now has a name.
    if status != 201 {
        return Err(reason(status, &text));
    }
    serde_json::from_str(&text).map_err(|e| format!("{path}: {e}"))
}

/// read_file is a picked file's bytes.
///
/// `Blob::array_buffer` AND NOT A FileReader. The buffer form is a Promise,
/// which wasm-bindgen-futures awaits directly; a FileReader needs an onload
/// closure kept alive for exactly as long as the read, which is the one thing
/// in this file that has to be done by hand for the websocket -- and a
/// dropped closure there is a callback the browser never calls again.
pub async fn read_file(file: &web_sys::File) -> Result<Vec<u8>, Error> {
    let buf = JsFuture::from(file.array_buffer())
        .await
        .map_err(describe)?;
    Ok(js_sys::Uint8Array::new(&buf).to_vec())
}

/// reason pulls the server's own message out of a failure.
///
/// IN FULL, because `ember serve` is reachable only from this machine: there
/// is nobody to withhold an internal message from, and the alternative is a UI
/// that says "something went wrong" over an error naming the exact config key.
fn reason(status: u16, body: &str) -> Error {
    #[derive(serde::Deserialize)]
    struct Wire {
        error: String,
    }
    match serde_json::from_str::<Wire>(body) {
        Ok(w) if !w.error.is_empty() => w.error,
        _ => format!("the server answered {status}"),
    }
}

/// urlish escapes anything that would end a query parameter early.
///
/// IN `api` AND NOT IN A VIEW, because there are two callers now and they are
/// both here in spirit: the offers query's provider and card filter, and the
/// progress socket's request id. It lived beside the first of those until #139
/// slice 3, which made `location.pathname` an input to the second -- and two
/// copies of one escaping rule is two places for one of them to drift, with
/// the layering the wrong way round besides (`api` must not read `views`).
///
/// Hand-written and tiny rather than a dependency: the set is `a-z0-9-_.~`,
/// which is every unreserved character, and everything else is percent-encoded.
/// The server parses the query itself, so a value that arrives mangled is a
/// filter that silently matched the wrong thing -- or, for the socket, a
/// cursor nobody asked for, since Go's `Query().Get` takes the FIRST value of
/// a duplicated key and a request id holding an `&` would supply a second
/// `after=`.
pub fn urlish(s: &str) -> String {
    let mut out = String::with_capacity(s.len());
    for b in s.bytes() {
        match b {
            b'a'..=b'z' | b'A'..=b'Z' | b'0'..=b'9' | b'-' | b'_' | b'.' | b'~' => {
                out.push(b as char)
            }
            _ => out.push_str(&format!("%{b:02X}")),
        }
    }
    out
}

fn describe(v: JsValue) -> Error {
    v.as_string()
        .or_else(|| {
            js_sys::Reflect::get(&v, &JsValue::from_str("message"))
                .ok()
                .and_then(|m| m.as_string())
        })
        .unwrap_or_else(|| "the request could not be sent".to_string())
}

/// Socket is a live progress stream for one request.
///
/// It resumes from a cursor, which is what makes a reconnect lossless: the
/// server replays from the rows, so a page reload and a dropped connection are
/// the same thing to it. For the life of this type that sentence was a claim
/// about a parameter nobody used -- `open` took an `after` and its one caller
/// passed the literal `0`, and nothing re-opened a stream that ended at all.
/// `views::Watch` is where the cursor and the re-open live, because the cursor
/// is `Progress`'s and this type holds no state worth resuming from.
pub struct Socket {
    ws: web_sys::WebSocket,
    // Kept alive for as long as the socket is: a dropped closure is a
    // callback the browser will never call again, which here is a progress
    // stream that silently stops.
    _on_open: Closure<dyn FnMut(web_sys::Event)>,
    _on_message: Closure<dyn FnMut(web_sys::MessageEvent)>,
    _on_close: Closure<dyn FnMut(web_sys::CloseEvent)>,
}

impl Socket {
    /// open starts watching a request from a sequence number.
    ///
    /// `on_open` IS NOT DECORATION, and leaving it out is how a bounded retry
    /// becomes an unbounded one. `WebSocket::new` returns Ok for a server that
    /// is not there -- the handshake is asynchronous, so the constructor
    /// succeeds and `onclose` arrives afterwards -- so "the socket opened" is
    /// a thing only the browser can say. A retry counter reset on the
    /// constructor's Ok is reset on every attempt, which is a loop that never
    /// reaches its bound and never prints the sentence that tells an operator
    /// to reload.
    pub fn open(
        id: &str,
        after: i64,
        mut on_open: impl FnMut() + 'static,
        mut on_envelope: impl FnMut(crate::model::Envelope) + 'static,
        mut on_closed: impl FnMut() + 'static,
    ) -> Result<Self, Error> {
        let loc = window()?.location();
        let host = loc.host().map_err(describe)?;
        // Same scheme as the page: http on loopback, so ws and never wss.
        //
        // THE ID IS ESCAPED, which it did not need to be until #139 slice 3.
        // Its one caller used to pass a `watching` signal written from the
        // server's own listing -- sixteen hex characters -- and the id comes
        // off `location.pathname` now, which an operator can type anything
        // into. `/requests/x&after=99` would otherwise send
        // `?id=x&after=99&after=0`, and Go's `Query().Get` takes the first
        // value: a watcher resuming from a cursor nobody chose, on this
        // machine, over the page that says which step is on screen.
        let url = format!("ws://{host}/ws?id={}&after={after}", urlish(id));

        let ws = web_sys::WebSocket::new(&url).map_err(describe)?;

        let opened = Closure::wrap(Box::new(move |_: web_sys::Event| {
            on_open();
        }) as Box<dyn FnMut(_)>);
        ws.set_onopen(Some(opened.as_ref().unchecked_ref()));

        let on_message = Closure::wrap(Box::new(move |e: web_sys::MessageEvent| {
            if let Some(text) = e.data().as_string() {
                match serde_json::from_str::<crate::model::Envelope>(&text) {
                    Ok(env) => on_envelope(env),
                    // Logged and not surfaced: a message this build cannot
                    // decode is a newer ember talking to an older page, and
                    // dropping one envelope is better than ending the stream.
                    Err(err) => web_sys::console::warn_1(&JsValue::from_str(&format!(
                        "ember-web: undecodable envelope: {err}"
                    ))),
                }
            }
        }) as Box<dyn FnMut(_)>);
        ws.set_onmessage(Some(on_message.as_ref().unchecked_ref()));

        let on_close = Closure::wrap(Box::new(move |_: web_sys::CloseEvent| {
            on_closed();
        }) as Box<dyn FnMut(_)>);
        ws.set_onclose(Some(on_close.as_ref().unchecked_ref()));

        Ok(Self {
            ws,
            _on_open: opened,
            _on_message: on_message,
            _on_close: on_close,
        })
    }
}

impl Drop for Socket {
    fn drop(&mut self) {
        // THE HANDLERS GO FIRST, and that ordering is the whole of this.
        //
        // `close()` makes the browser call `onclose`, and the closure it would
        // call is about to be freed with this struct -- which throws
        // `closure invoked recursively or after being dropped` in the console,
        // observed by clicking a queue row twice. It was harmless only
        // BECAUSE nothing read the signal that handler wrote: the write that
        // could not land was one nobody was looking at. Now that the page says
        // when a stream stopped, that same close would be the OLD socket
        // reporting a stop over the NEW socket's state -- a watcher that
        // reads "reconnecting" for a stream that is open, and a retry
        // scheduled against a request nobody is watching.
        //
        // Clearing them makes the two sockets independent rather than
        // relying on the order in which `views::Watch` happens to store one
        // and drop the other, which was luck rather than a decision.
        self.ws.set_onopen(None);
        self.ws.set_onmessage(None);
        self.ws.set_onclose(None);
        // Closed explicitly. Letting it fall out of scope leaves the browser
        // holding a connection to a generation nobody is watching, and the
        // server holding a goroutine polling for it.
        let _ = self.ws.close();
    }
}

#[cfg(test)]
mod tests {
    use super::{reason, urlish};

    /// The 409 the cancel route answers, verbatim, carrying the remediation.
    ///
    /// IN THIS MODULE so `reason` stays private: it is called from one place
    /// and widening it to be reachable from a test file would be a hole in
    /// the wall for a test's convenience.
    ///
    /// The sentence is the whole point of the body. For the life of the page
    /// it was built, serialized, parsed here and dropped one frame later by a
    /// `let _ =` at the call site, so an operator pressing cancel on a
    /// request ember cannot stop saw a row that still said `running` with a
    /// `cancel` button still on it -- indistinguishable from a click that
    /// never registered, over a card that may still be billing.
    #[test]
    fn a_refusal_reaches_the_operator_with_the_remediation_in_it() {
        const BODY: &str = r#"{"error":"request 080f21c0 is recorded as generating, but no rental in this process is generating it; restart ember serve to have the queue reconcile it"}"#;
        let got = reason(409, BODY);
        assert!(
            got.contains("restart ember serve"),
            "the remediation did not survive: {got}"
        );
        assert!(
            !got.contains("409"),
            "the status code stood in for a message that was there: {got}"
        );

        // AND A BODY THAT IS NOT ONE still says something, because a UI that
        // prints nothing over a failure is the state this whole path was in.
        assert_eq!(reason(500, "not json at all"), "the server answered 500");
        assert_eq!(reason(409, r#"{"error":""}"#), "the server answered 409");
    }

    /// A card model with a space in it -- "NVIDIA H100 80GB HBM3" is how
    /// RunPod spells one -- must not end the query parameter early, or the
    /// filter silently matches something else.
    #[test]
    fn escapes_what_would_break_a_query() {
        assert_eq!(urlish("rtx4090"), "rtx4090");
        assert_eq!(urlish("h100-80g"), "h100-80g");
        assert_eq!(urlish("a b"), "a%20b");
        assert_eq!(urlish("a&gpu=b"), "a%26gpu%3Db");
        assert_eq!(urlish("a#b"), "a%23b");
        assert_eq!(urlish(""), "");
    }

    /// AND A REQUEST ID IS A SECOND CALLER SINCE #139 SLICE 3, which is why
    /// this function is here rather than beside the offers form.
    ///
    /// The id comes off `location.pathname`, so `/requests/x&after=99` is an
    /// address an operator can type: unescaped it sends
    /// `?id=x&after=99&after=0`, Go's `Query().Get("after")` takes the FIRST
    /// value, and the watcher resumes request `x` from a cursor nobody chose.
    /// The one it would send today is sixteen hex characters, for which this
    /// is the identity -- which is exactly why nothing would have reported it.
    #[test]
    fn a_request_id_cannot_smuggle_a_cursor() {
        assert_eq!(urlish("3f9a1c20d4e7b865"), "3f9a1c20d4e7b865");
        assert_eq!(urlish("x&after=99"), "x%26after%3D99");
        assert_eq!(urlish("x#y"), "x%23y");
    }
}
