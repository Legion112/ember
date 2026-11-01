//! The page.
//!
//! Four panels, in the order the flow runs: what can be rented, what to
//! generate, what is queued, and what is happening. The decisions are all in
//! `model` -- this module is signals, markup and the calls in `api`.
//!
//! WHY SIGNALS AND NOT A VDOM. A watched generation emits a progress event
//! several times a second, and each one changes three numbers. With
//! fine-grained reactivity that updates three text nodes; with a vdom it
//! re-renders the page and diffs it. That is the whole reason this crate is
//! Leptos rather than something smaller.

use leptos::prelude::*;
use leptos::task::spawn_local;

use crate::api;
use crate::model::{Config, Envelope, Job, NewRequest, Offers, Progress, Request, RequestList};

/// App is the whole page.
#[component]
pub fn App() -> impl IntoView {
    // Loaded once. It is the ceiling and the list of what can be rented, and
    // neither changes while the process runs.
    let config = RwSignal::new(Config::default());
    let fatal = RwSignal::new(String::new());

    spawn_local(async move {
        match api::get::<Config>("/api/config").await {
            Ok(c) => config.set(c),
            Err(e) => fatal.set(e),
        }
    });

    // Which request the progress panel is watching. Set by queueing one and
    // by clicking one in the queue.
    let watching = RwSignal::new(String::new());

    view! {
        <header>
            <h1>"ember"</h1>
            <Posture config=config />
        </header>

        <Show when=move || !fatal.get().is_empty()>
            <p class="fatal">
                "ember serve could not be reached: " {move || fatal.get()}
            </p>
        </Show>

        <main>
            <Compose config=config watching=watching />
            <Queue watching=watching />
            <Watch watching=watching />
        </main>
    }
}

/// Posture is the line that says what this server will and will not do.
///
/// On screen rather than in the help text, because the one thing a person has
/// to decide here is whether to leave the process running -- and what bounds
/// the damage of there being no authentication is whether paid providers are
/// reachable at all.
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
            " · no authentication: any page in this browser can reach this port"
        </p>

        <Show when=move || !config.get().servable && !config.get().providers.is_empty()>
            <p class="fatal">
                "This configuration has no " <code>"sd:"</code> " block, so there is no
                 generation server to start and a queued request would have nowhere to run."
            </p>
        </Show>
    }
}

/// Compose is the card picker and the batch.
#[component]
fn Compose(config: RwSignal<Config>, watching: RwSignal<String>) -> impl IntoView {
    let offers = RwSignal::new(Offers::default());
    let provider = RwSignal::new(String::new());
    let gpu = RwSignal::new(String::new());
    let ttl = RwSignal::new(String::new());
    let max_hourly = RwSignal::new(String::new());
    let error = RwSignal::new(String::new());

    // The batch being assembled, and the job being edited.
    let batch: RwSignal<Vec<Job>> = RwSignal::new(Vec::new());
    let draft = RwSignal::new(Job {
        width: 512,
        height: 512,
        steps: 20,
        ..Default::default()
    });

    // The provider defaults to the config's first one that can actually be
    // rented from, so the form is usable without a choice being made.
    Effect::new(move |_| {
        let c = config.get();
        if provider.get().is_empty() {
            if let Some(p) = c.providers.iter().find(|p| c.can_rent(&p.name)) {
                provider.set(p.name.clone());
            }
        }
    });

    let load_offers = move || {
        let p = provider.get();
        let g = gpu.get();
        spawn_local(async move {
            let mut path = format!("/api/offers?provider={}", urlish(&p));
            if !g.is_empty() {
                path += &format!("&gpu={}", urlish(&g));
            }
            match api::get::<Offers>(&path).await {
                Ok(o) => offers.set(o),
                Err(e) => error.set(e),
            }
        });
    };

    let queue = move || {
        error.set(String::new());
        let mut jobs = batch.get();
        // A single composed job needs no "add" first: the obvious thing to do
        // with one prompt is generate it.
        if jobs.is_empty() && !draft.get().prompt.trim().is_empty() {
            jobs.push(draft.get());
        }
        if jobs.is_empty() {
            error.set("nothing to generate: type a prompt".into());
            return;
        }

        let body = NewRequest {
            provider: provider.get(),
            gpu: gpu.get(),
            ttl: ttl.get(),
            max_hourly_usd: max_hourly.get(),
            preview: config.get().preview,
            preview_interval: config.get().preview_interval,
            jobs,
            ..Default::default()
        };
        spawn_local(async move {
            #[derive(serde::Deserialize)]
            struct Queued {
                id: String,
            }
            match api::post::<_, Queued>("/api/requests", &body).await {
                Ok(q) => {
                    batch.set(Vec::new());
                    watching.set(q.id);
                }
                Err(e) => error.set(e),
            }
        });
    };

    view! {
        <section class="panel">
            <h2>"what to rent"</h2>

            <div class="row">
                <label>
                    "provider"
                    <select on:change=move |ev| {
                        provider.set(event_target_value(&ev));
                        offers.set(Offers::default());
                    }>
                        <For
                            each=move || config.get().providers
                            key=|p| p.name.clone()
                            let:p
                        >
                            {
                                let c = config.get();
                                let usable = c.can_rent(&p.name);
                                view! {
                                    <option value=p.name.clone() disabled=!usable>
                                        {p.name.clone()}
                                        {if p.free { " (free)" } else if usable { " (paid)" }
                                         else { " (needs --allow-paid)" }}
                                    </option>
                                }
                            }
                        </For>
                    </select>
                </label>

                <label>
                    "card"
                    <input
                        placeholder="any"
                        prop:value=move || gpu.get()
                        on:input=move |ev| gpu.set(event_target_value(&ev))
                    />
                </label>

                <button on:click=move |_| load_offers()>"what is available"</button>
            </div>

            <Listings offers=offers gpu=gpu />

            <div class="row">
                <label>
                    "ttl"
                    <input
                        placeholder=move || config.get().ceilings.ttl
                        prop:value=move || ttl.get()
                        on:input=move |ev| ttl.set(event_target_value(&ev))
                    />
                </label>
                <label>
                    "max $/hr"
                    <input
                        placeholder=move || config.get().ceilings.max_hourly.text().to_string()
                        prop:value=move || max_hourly.get()
                        on:input=move |ev| max_hourly.set(event_target_value(&ev))
                    />
                </label>
                // Said out loud, because the alternative is an operator
                // believing a number they typed is in force when the file's
                // is.
                <p class="hint">
                    "Either may only narrow what " <code>"ember.yaml"</code> " allows."
                </p>
            </div>

            <h2>"what to generate"</h2>
            <JobForm config=config draft=draft />

            <div class="row">
                <button on:click=move |_| {
                    let j = draft.get();
                    if !j.prompt.trim().is_empty() {
                        batch.update(|b| b.push(j));
                    }
                }>"add to batch"</button>

                <button class="primary" on:click=move |_| queue()>
                    {move || {
                        let n = batch.get().len();
                        if n == 0 { "generate".to_string() }
                        else { format!("generate {} job(s) on one rental", n) }
                    }}
                </button>
            </div>

            <Show when=move || !batch.get().is_empty()>
                <ol class="batch">
                    {move || batch.get().into_iter().enumerate().map(|(i, j)| view! {
                        <li>
                            <code>{j.describe()}</code>
                            <button class="link" on:click=move |_| {
                                batch.update(|b| { if i < b.len() { b.remove(i); } });
                            }>"remove"</button>
                        </li>
                    }).collect::<Vec<_>>()}
                </ol>
                <p class="hint">
                    "One rental serves the whole batch: provisioning, the image pull, the
                     asset verify and the model load are paid once rather than once per image."
                </p>
            </Show>

            <Show when=move || !error.get().is_empty()>
                <p class="error">{move || error.get()}</p>
            </Show>
        </section>
    }
}

/// Listings shows what each provider answered.
#[component]
fn Listings(offers: RwSignal<Offers>, gpu: RwSignal<String>) -> impl IntoView {
    view! {
        <For each=move || offers.get().listings key=|l| l.provider.clone() let:l>
            {
                // A provider that could not be asked says so. "I could not
                // reach RunPod" must never render as "RunPod has nothing".
                if !l.error.is_empty() {
                    view! {
                        <p class="error">
                            {l.provider.clone()} " could not be asked: " {l.error.clone()}
                        </p>
                    }.into_any()
                } else if l.offers.is_empty() {
                    view! { <p class="hint">{l.provider.clone()} " has nothing matching"</p> }
                        .into_any()
                } else {
                    view! {
                        <table class="offers">
                            <tbody>
                                <For each=move || l.offers.clone() key=|o| o.token.clone() let:o>
                                    <tr on:click={
                                        let model = o.gpu.clone();
                                        move |_| gpu.set(model.clone())
                                    }>
                                        <td>{o.gpu.clone()}</td>
                                        <td>{format!("{} GiB", o.vram_gib)}</td>
                                        <td class="price">{o.price.text().to_string()}</td>
                                        <td>{o.region.clone()}</td>
                                        <td>
                                            {if o.interruptible {
                                                // Worth seeing before choosing
                                                // rather than discovering: the
                                                // provider can take it back
                                                // mid-run.
                                                "interruptible"
                                            } else { "" }}
                                        </td>
                                    </tr>
                                </For>
                            </tbody>
                        </table>
                    }.into_any()
                }
            }
        </For>
    }
}

/// JobForm is one image's parameters.
#[component]
fn JobForm(config: RwSignal<Config>, draft: RwSignal<Job>) -> impl IntoView {
    view! {
        <label class="wide">
            "prompt"
            <textarea
                rows="3"
                prop:value=move || draft.get().prompt
                on:input=move |ev| draft.update(|j| j.prompt = event_target_value(&ev))
            />
        </label>
        <label class="wide">
            "negative"
            <input
                prop:value=move || draft.get().negative_prompt
                on:input=move |ev| draft.update(|j| j.negative_prompt = event_target_value(&ev))
            />
        </label>

        <div class="row">
            <label>"width" <input type="number"
                prop:value=move || draft.get().width
                on:input=move |ev| draft.update(|j| j.width = num(&ev))
            /></label>
            <label>"height" <input type="number"
                prop:value=move || draft.get().height
                on:input=move |ev| draft.update(|j| j.height = num(&ev))
            /></label>
            <label>"steps" <input type="number"
                prop:value=move || draft.get().steps
                on:input=move |ev| draft.update(|j| j.steps = num(&ev))
            /></label>
            <label>"batch" <input type="number"
                prop:value=move || draft.get().batch_count
                on:input=move |ev| draft.update(|j| j.batch_count = num(&ev))
            /></label>
            <label>"seed" <input type="number"
                prop:value=move || draft.get().seed
                on:input=move |ev| draft.update(|j| j.seed = num64(&ev))
            /></label>
        </div>

        <div class="row">
            // Menus when the server has told ember what it can do, free text
            // before that: the capability list exists only after a rental, and
            // greying out every control before the operator's first one would
            // make the first request impossible.
            <label>
                "sampler"
                {move || match config.get().caps {
                    Some(c) if !c.samplers.is_empty() => view! {
                        <select on:change=move |ev| draft.update(|j| j.sampler = event_target_value(&ev))>
                            <option value="">"default"</option>
                            <For each=move || c.samplers.clone() key=|s| s.clone() let:s>
                                <option value=s.clone()>{s.clone()}</option>
                            </For>
                        </select>
                    }.into_any(),
                    _ => view! {
                        <input placeholder="euler"
                            prop:value=move || draft.get().sampler
                            on:input=move |ev| draft.update(|j| j.sampler = event_target_value(&ev))
                        />
                    }.into_any(),
                }}
            </label>
            <label>"cfg" <input type="number" step="0.1"
                prop:value=move || draft.get().cfg
                on:input=move |ev| draft.update(|j| j.cfg = numf(&ev))
            /></label>
            // Two numbers that both get called "cfg" in conversation: the
            // flux2-dev recipe sets cfg-scale 1 and guidance 4, so conflating
            // them generates at the wrong one.
            <label>"guidance" <input type="number" step="0.1"
                prop:value=move || draft.get().guidance
                on:input=move |ev| draft.update(|j| j.guidance = numf(&ev))
            /></label>
            <label>"name" <input placeholder="cat"
                prop:value=move || draft.get().output
                on:input=move |ev| draft.update(|j| j.output = event_target_value(&ev))
            /></label>
        </div>

        <Show when=move || config.get().caps.is_none()>
            <p class="hint">
                "ember has not asked a generation server what it can do yet, so a sampler
                 cannot be checked until the first rental."
            </p>
        </Show>
    }
}

/// Queue is what is waiting, running and finished.
#[component]
fn Queue(watching: RwSignal<String>) -> impl IntoView {
    let list = RwSignal::new(RequestList::default());

    // Polled rather than pushed. The socket carries one request's progress;
    // the queue changes when somebody queues or cancels something, which is
    // rare enough that two seconds is imperceptible and cheap enough that a
    // second socket is not worth it.
    let reload = move || {
        spawn_local(async move {
            if let Ok(l) = api::get::<RequestList>("/api/requests?limit=50").await {
                list.set(l);
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
            <Show when=move || list.get().requests.is_empty()>
                <p class="hint">"nothing queued"</p>
            </Show>
            <table class="queue">
                <tbody>
                    <For each=move || list.get().requests key=|r| (r.id.clone(), r.status.clone()) let:r>
                        {
                            let id = r.id.clone();
                            let running = list.get().running == r.id;
                            let cancellable = r.is_cancellable();
                            let cancel_id = r.id.clone();
                            view! {
                                <tr class:running=running
                                    class:watching=move || watching.get() == id>
                                    <td>
                                        <button class="link" on:click={
                                            let id = r.id.clone();
                                            move |_| watching.set(id.clone())
                                        }>{r.id.chars().take(8).collect::<String>()}</button>
                                    </td>
                                    <td class="status">{r.status.clone()}</td>
                                    <td>{format!("{} job(s)", r.jobs.len())}</td>
                                    <td>{r.gpu.clone()}</td>
                                    <td class="price">
                                        {if r.cost.is_free() { String::new() }
                                         else { r.cost.text().to_string() }}
                                    </td>
                                    <td>
                                        <Show when=move || cancellable>
                                            <button class="link" on:click={
                                                let id = cancel_id.clone();
                                                move |_| {
                                                    let id = id.clone();
                                                    spawn_local(async move {
                                                        let _ = api::post::<_, serde_json::Value>(
                                                            &format!("/api/requests/{id}/cancel"),
                                                            &serde_json::json!({}),
                                                        ).await;
                                                    });
                                                }
                                            }>"cancel"</button>
                                        </Show>
                                    </td>
                                </tr>
                            }
                        }
                    </For>
                </tbody>
            </table>
            <p class="hint">
                "A request that is already generating cannot be cancelled: it would mean
                 destroying the node, which takes every other request on the same rental
                 with it. Stop " <code>"ember serve"</code> " to end a rental."
            </p>
        </section>
    }
}

/// Watch is the live progress of one request.
#[component]
fn Watch(watching: RwSignal<String>) -> impl IntoView {
    let progress = RwSignal::new(Progress::default());
    let request: RwSignal<Option<Request>> = RwSignal::new(None);
    let connected = RwSignal::new(false);

    // One socket, re-opened whenever the watched request changes. The cursor
    // starts at zero for a new one, so the backlog arrives before anything
    // live -- a request that finished a minute ago is as watchable as one
    // generating now.
    //
    // Held in a LOCAL store, because a web_sys::WebSocket is neither Send nor
    // Sync -- nothing in a browser is -- and Leptos's ordinary signals
    // require both so that the same component can run on a server. This one
    // never will.
    let socket: StoredValue<Option<api::Socket>, LocalStorage> = StoredValue::new_local(None);

    Effect::new(move |_| {
        let id = watching.get();
        if id.is_empty() {
            socket.set_value(None);
            return;
        }
        progress.set(Progress::default());
        request.set(None);

        let on_envelope = move |env: Envelope| {
            progress.update(|p| {
                for e in &env.events {
                    p.apply(e);
                }
            });
            if let Some(r) = env.request {
                request.set(Some(r));
            }
        };
        let on_closed = move || connected.set(false);

        match api::Socket::open(&id, 0, on_envelope, on_closed) {
            Ok(s) => {
                connected.set(true);
                // The previous socket is dropped here, which closes it: a
                // connection to a generation nobody is watching holds a
                // goroutine on the other end polling for it.
                socket.set_value(Some(s));
            }
            Err(_) => connected.set(false),
        }
    });

    view! {
        <section class="panel" class:idle=move || watching.get().is_empty()>
            <h2>"what is happening"</h2>

            <Show when=move || watching.get().is_empty()>
                <p class="hint">"pick a request, or generate one"</p>
            </Show>

            <Show when=move || !watching.get().is_empty()>
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
                    <Show when=move || progress.get().spent != 0>
                        <dt>"card time"</dt>
                        <dd>{move || {
                            request.get().map(|r| r.cost.text().to_string())
                                .unwrap_or_default()
                        }}</dd>
                    </Show>
                </dl>

                <Show when=move || !progress.get().failure.is_empty()>
                    // The one an operator must not miss.
                    <p class="fatal">
                        "THE NODE MAY STILL BE RUNNING AND BILLING: "
                        {move || progress.get().failure}
                        " — check " <code>"ember ls"</code>
                    </p>
                </Show>

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
                                if j.done {
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

                <Gallery request=request progress=progress />

                <Show when=move || !progress.get().log.is_empty()>
                    <details>
                        <summary>"what the node said"</summary>
                        <pre class="log">{move || progress.get().log_text()}</pre>
                    </details>
                </Show>
            </Show>
        </section>
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
                progress.get().jobs.into_iter()
                    .filter(|j| !j.done && j.step > 0)
                    .filter_map(|j| {
                        let im = r.images.iter().find(|i| i.preview && i.job == j.index)?;
                        Some((j.index, format!("{}&step={}", im.path, j.step)))
                    })
                    .map(|(job, src)| view! {
                        <figure class="preview">
                            <img src=src alt=format!("job {job} in progress") />
                            <figcaption>
                                {format!("job {job}, in progress")}
                                // Said because it is surprising: a `proj`
                                // frame is the latent, not a thumbnail, so a
                                // 64x64 generation previews as an 8x8 image.
                                <span class="hint">" (the latent, not the output)"</span>
                            </figcaption>
                        </figure>
                    })
                    .collect::<Vec<_>>()
            }}
        </div>
    }
}

// --- small helpers ---

fn num(ev: &leptos::ev::Event) -> i32 {
    event_target_value(ev).trim().parse().unwrap_or(0)
}

fn num64(ev: &leptos::ev::Event) -> i64 {
    event_target_value(ev).trim().parse().unwrap_or(0)
}

fn numf(ev: &leptos::ev::Event) -> f64 {
    event_target_value(ev).trim().parse().unwrap_or(0.0)
}

/// urlish escapes the few characters a GPU name or a provider could carry.
///
/// Hand-written and tiny rather than a dependency: these are provider names
/// and card models, so the set is `a-z0-9-_.` plus the space a card name can
/// have. Anything else is percent-encoded so it cannot end a query parameter
/// early -- and the server parses the query itself, so a value that arrives
/// mangled is a filter that silently matched the wrong thing.
fn urlish(s: &str) -> String {
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

#[cfg(test)]
mod tests {
    use super::urlish;

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
}
