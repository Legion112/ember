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
use crate::model::{
    Answer, Cancelled, Config, Envelope, Hires, Input, Job, Listing, NewRequest, Offers, Progress,
    Rental, Request, RequestList, Stopping, VaeTiling,
};

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

    // Empty until the config arrives, and sent as the config's own value
    // then: an empty string means "whatever ember.yaml says", so the form is
    // correct before /api/config has answered.
    let preview = RwSignal::new(String::new());
    let preview_every = RwSignal::new(0);

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

    // AND THE SAME ANSWER APPLIED TO WHAT HAS ALREADY BEEN TYPED.
    //
    // The config arrives after the panel is on screen, so an operator can type
    // a clip skip -- or add a whole job to the batch -- before ember knows
    // whether the server takes one. The controls below grey out when the
    // answer lands, and without this the value stays in the draft and in the
    // batch: a request carrying a parameter whose control is visibly
    // unavailable, which is the defect `drop_denied` names. The batch is
    // cleaned too and not only the draft, because a batch row is a job nothing
    // re-renders from a control.
    //
    // Guarded on a change rather than written unconditionally, so a config
    // that denies nothing -- which is every server here -- does not notify the
    // batch's subscribers on load.
    //
    // NOTHING COVERS THESE TEN LINES, and it is written down rather than
    // assumed away. `drop_denied` has a unit test; this effect does not,
    // because the window it exists for -- a value typed before /api/config
    // answers -- is not a state the browser lab can be driven into. Removing
    // it was reinstated and no test went red, the only one of seventeen that
    // did not. The decision is in `model` on purpose, so what is untested here
    // is the wiring and not the rule.
    Effect::new(move |_| {
        let f = match config.get().caps {
            Some(c) => c.features,
            None => return,
        };
        // Untracked, so this effect depends on the config alone. Reading the
        // draft reactively here would re-run it on every keystroke -- and
        // worse, the effect writes the draft, so it would be subscribing to
        // its own output.
        let mut d = draft.get_untracked();
        let before = d.clone();
        d.drop_denied(&f);
        if d != before {
            draft.set(d);
        }
        let mut b = batch.get_untracked();
        let kept = b.clone();
        for j in b.iter_mut() {
            j.drop_denied(&f);
        }
        if b != kept {
            batch.set(b);
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
            preview: if preview.get().is_empty() {
                config.get().preview
            } else {
                preview.get()
            },
            preview_interval: if preview_every.get() > 0 {
                preview_every.get()
            } else {
                config.get().preview_interval
            },
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
                    <Help text="provider" title="Which account the card is rented from. A provider marked (needs --allow-paid) costs money and this server was started without that flag, so it is refused."/>
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
                    <Help text="card" title="GPU model to rent, e.g. h100 or a100-80g. A prefix matches at the capacity boundary: a100 accepts a100-40g and a100-80g but not a1000. Empty means any card the provider has."/>
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
                    <Help text="ttl" title="How long the rental may live before the node destroys itself, e.g. 30m. It is the ceiling on what a wedged run can cost. It may only narrow what ember.yaml allows; asking for longer gets the config's."/>
                    <input
                        placeholder=move || config.get().ceilings.ttl
                        prop:value=move || ttl.get()
                        on:input=move |ev| ttl.set(event_target_value(&ev))
                    />
                </label>
                <label>
                    <Help text="max $/hr" title="The most this rental may cost per hour. An offer above it is not selected. It may only narrow what ember.yaml allows."/>
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
                <label>
                    <Help text="preview" title="How the in-progress frame is rendered. proj projects the latent and is nearly free, so a 512x512 generation previews as a 64x64 smear; tae and vae decode a real image every frame, which is a decode billed to the card. none turns previews off."/>
                    <select on:change=move |ev| preview.set(event_target_value(&ev))>
                        {move || {
                            let c = config.get();
                            // Before the first rental there are no
                            // capabilities to read, so the four sd-server
                            // publishes are offered and the server refuses
                            // anything the running one does not have.
                            let modes: Vec<String> = match &c.caps {
                                Some(caps) if !caps.preview_modes.is_empty() => {
                                    caps.preview_modes.clone()
                                }
                                _ => ["none", "proj", "tae", "vae"]
                                    .iter().map(|s| s.to_string()).collect(),
                            };
                            let now = if preview.get().is_empty() { c.preview.clone() } else { preview.get() };
                            modes.into_iter().map(|m: String| {
                                let sel = m == now;
                                let label = m.clone();
                                view! { <option value=m selected=sel>{label}</option> }
                            }).collect::<Vec<_>>()
                        }}
                    </select>
                </label>
                <label>
                    <Help text="every N steps" title="How often a preview frame is produced, in sampling steps. Lower is smoother and costs more; upstream's default of 1 is a frame every step. A negative value is refused rather than clamped, because sd-server would read it as 1 -- the most expensive setting rather than the least."/>
                    <input type="number" min="1"
                        prop:value=move || {
                            let n = preview_every.get();
                            if n > 0 { n } else { config.get().preview_interval }
                        }
                        on:input=move |ev| preview_every.set(num(&ev)) />
                </label>
            </div>
            <p class="hint">
                // Said because it decides whether a step is worth watching
                // and what it costs, and the two pull in opposite directions.
                <code>"proj"</code> " is a projection of the latent -- cheap, and a
                 512x512 generation previews as a 64x64 smear. " <code>"tae"</code> " and "
                <code>"vae"</code> " decode a real image every frame, which is a decode
                 billed to the card: on a rented GPU that is the expensive setting and
                 on this machine it is free."
            </p>

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

/// What every provider answered, as of NOW.
///
/// A NAMED FUNCTION OVER THE SIGNAL, which is the whole fix and the only
/// shape `cargo test` can hold: it returns a closure that reads `offers`
/// every time it is called, so the answers on screen are the answers of the
/// last query rather than of the first.
///
/// WHAT IT REPLACED WAS A `<For>` KEYED ON THE PROVIDER'S NAME. A keyed list
/// re-renders a row only when its KEY changes -- `tachys` calls the view
/// function for added keys and moves retained ones -- and `leptos::For`
/// hands the item over BY VALUE, so a child that reads it reads it once. The
/// provider's name does not change between two queries of that provider, and
/// the one control that varies between two presses of "what is available" is
/// the card filter, so narrowing it rendered the FIRST answer for ever. The
/// milder reading is a stale price; the measured one is worse -- a server
/// answering zero offers and "dial tcp: lookup rest.runpod.io: no such host"
/// left an H100 at $2.49 in US-KS-2 on screen with no error anywhere, over a
/// provider that could not be reached. That is the thing this component's own
/// comment forbids, two lines from the key that caused it.
///
/// NOT A KEY THAT ENCODES THE ROW, which would also have worked and is the
/// worse answer: a key is stable identity, and one built out of the data
/// rebuilds the DOM for a price change and leaves the next field somebody
/// adds in the same hole. There is no identity to preserve across queries
/// here anyway -- rebuilding a handful of provider blocks on each press
/// costs nothing, which is what AGENTS.md records about the progress rows
/// this same rule was paid for once already.
fn listed(offers: RwSignal<Offers>) -> impl Fn() -> Vec<Answer> + Copy {
    move || offers.get().listings.iter().map(Listing::answer).collect()
}

/// Listings shows what each provider answered.
#[component]
fn Listings(offers: RwSignal<Offers>, gpu: RwSignal<String>) -> impl IntoView {
    let answers = listed(offers);
    view! {
        {move || answers().into_iter().map(|a| answered(a, gpu)).collect::<Vec<_>>()}
    }
}

/// One provider's answer, laid out. The decision of WHICH of the three it is
/// belongs to `Listing::answer`, where cargo test reaches it.
fn answered(a: Answer, gpu: RwSignal<String>) -> AnyView {
    match a {
        // A provider that could not be asked says so. "I could not reach
        // RunPod" must never render as "RunPod has nothing".
        Answer::Failed { provider, why } => view! {
            <p class="error">{provider} " could not be asked: " {why}</p>
        }
        .into_any(),
        Answer::Nothing { provider } => {
            view! { <p class="hint">{provider} " has nothing matching"</p> }.into_any()
        }
        // NO KEY ON THE INNER LIST EITHER, and not because a token is a bad
        // key -- it is a good one. The rows are rebuilt with the block that
        // holds them, so there is nothing for a key to retain, and leaving
        // the inner `<For>` keyed on the offer token while fixing the outer
        // list would have put the same frozen row one level down.
        Answer::Rentable { offers } => view! {
            <table class="offers">
                <tbody>
                    {offers.into_iter().map(|o| view! {
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
                                    // Worth seeing before choosing rather
                                    // than discovering: the provider can take
                                    // it back mid-run.
                                    "interruptible"
                                } else { "" }}
                            </td>
                        </tr>
                    }).collect::<Vec<_>>()}
                </tbody>
            </table>
        }
        .into_any(),
    }
}

/// Help is a field's name with an explanation on hover.
///
/// On the TEXT and not on the whole `<label>`: a title on the label fires
/// while the operator is inside the field it names, so the tooltip covers the
/// value being typed. It is a plain `title`, which is the one tooltip that
/// works on a page with no JavaScript of its own and that a screen reader
/// already announces -- a hand-rolled one would be a popup to position, trap
/// and dismiss for no gain.
#[component]
fn Help(text: &'static str, title: &'static str) -> impl IntoView {
    view! { <span class="help" title=title>{text}</span> }
}

/// JobForm is one image's parameters.
#[component]
fn JobForm(config: RwSignal<Config>, draft: RwSignal<Job>) -> impl IntoView {
    view! {
        <label class="wide">
            <Help text="prompt" title="What to generate. Arbitrary text: it is sent in the request body and never on a command line, so nothing in it can be read as a flag."/>
            <textarea
                rows="3"
                prop:value=move || draft.get().prompt
                on:input=move |ev| draft.update(|j| j.prompt = event_target_value(&ev))
            />
        </label>
        <label class="wide">
            <Help text="negative" title="What to steer away from. Some models ignore it entirely -- FLUX.2 among them. Empty sends nothing."/>
            <input
                prop:value=move || draft.get().negative_prompt
                on:input=move |ev| draft.update(|j| j.negative_prompt = event_target_value(&ev))
            />
        </label>

        <div class="row">
            <label><Help text="width" title="Output width in pixels. Most checkpoints want a multiple of 64. Sampling cost grows with the area, so doubling both sides is four times the card time. 0 leaves it to the server."/> <input type="number"
                prop:value=move || draft.get().width
                on:input=move |ev| draft.update(|j| j.width = num(&ev))
            /></label>
            <label><Help text="height" title="Output height in pixels. Most checkpoints want a multiple of 64. 0 leaves it to the server."/> <input type="number"
                prop:value=move || draft.get().height
                on:input=move |ev| draft.update(|j| j.height = num(&ev))
            /></label>
            <label><Help text="steps" title="Sampling steps per image. Card time is roughly linear in this, so it is the main price knob. 0 leaves it to the server."/> <input type="number"
                prop:value=move || draft.get().steps
                on:input=move |ev| draft.update(|j| j.steps = num(&ev))
            /></label>
            <label><Help text="batch" title="How many images this one job generates from the same prompt. They share the rental and the resident weights; each gets its own file. 0 means the server's default of one."/> <input type="number"
                prop:value=move || draft.get().batch_count
                on:input=move |ev| draft.update(|j| j.batch_count = num(&ev))
            /></label>
            <label><Help text="seed" title="The starting noise. The same seed with the same prompt and parameters reproduces the same image, which is what makes two runs comparable. 0 is not sent, so the server picks one."/> <input type="number"
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
                <Help text="sampler" title="The algorithm that turns noise into an image. The menu is what the running server reported it supports; before the first rental there is nothing to check a name against, so it is free text."/>
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
            <label><Help text="cfg" title="Classifier-free guidance: how hard the sampler is pushed toward the prompt. Not the same number as guidance -- the FLUX.2 recipe sets cfg 1 and guidance 4. 0 leaves it to the server."/> <input type="number" step="0.1"
                prop:value=move || draft.get().cfg
                on:input=move |ev| draft.update(|j| j.cfg = numf(&ev))
            /></label>
            // Two numbers that both get called "cfg" in conversation: the
            // flux2-dev recipe sets cfg-scale 1 and guidance 4, so conflating
            // them generates at the wrong one.
            <label><Help text="guidance" title="Distilled guidance, which distilled models such as FLUX.2 take instead of cfg. Both get called cfg in conversation and setting the wrong one generates at the wrong strength. 0 leaves it to the server."/> <input type="number" step="0.1"
                prop:value=move || draft.get().guidance
                on:input=move |ev| draft.update(|j| j.guidance = numf(&ev))
            /></label>
            <label><Help text="name" title="The filename stem for this job's images, written as name-00.png. Sanitised rather than refused, and a stem that cleans away to nothing is an error."/> <input placeholder="cat"
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

        // COLLAPSED, because the five rows above are the whole job on almost
        // every run and a panel that opens with thirty controls is a panel
        // nobody reads. Everything in here is optional and unset by default:
        // an untouched advanced section posts exactly the body it posted
        // before this section existed.
        <details>
            <summary>"advanced"</summary>
            <AdvancedForm config=config draft=draft/>
        </details>
    }
}

/// ImageInput is the file picker and the one parameter it unblocks.
///
/// WHY THE STRENGTH IS INSIDE THIS COMPONENT and not beside the other
/// numbers. It cannot be sent without an image: a live sd-server accepts
/// `strength` with no `init_image`, generates, and reports the strength
/// nowhere in what it echoes back -- so ember refuses the combination, and a
/// control sitting there accepting a value before a file is picked would be
/// a control whose only outcome is a 400. It appears when there is something
/// for it to apply to.
///
/// WHAT THE PICKER SENDS is the file's bytes and nothing else: no filename,
/// no form envelope, no declared type. The server answers with the sha256 of
/// what it stored, and that digest is what the job carries -- so the name of
/// the file on the operator's disk never reaches ember at all, which is the
/// strongest available answer to "could an uploaded filename reach a path
/// join".
///
/// WHAT IT SHOWS BACK is the server's own reading of the header: the format
/// and the geometry. That is not decoration -- it is the only way to tell a
/// staged file from one the browser has in hand but ember refused, and the
/// refusal is a sentence from the server rather than a guess made here.
#[component]
fn ImageInput(config: RwSignal<Config>, draft: RwSignal<Job>) -> impl IntoView {
    // What the server said about the file that is currently staged, and what
    // it said about one it would not take. Separate signals because they are
    // different states: a refused upload leaves the previous image staged,
    // and a picker that cleared it would lose a file the operator had
    // already uploaded over one that failed.
    let staged = RwSignal::new(Option::<Input>::None);
    let error = RwSignal::new(String::new());
    let busy = RwSignal::new(false);

    let offered = move || config.get().offers(|f| f.init_image);

    let pick = move |ev: leptos::ev::Event| {
        let input: web_sys::HtmlInputElement = event_target(&ev);
        let file = input.files().and_then(|fs| fs.get(0));
        error.set(String::new());
        let Some(file) = file else {
            // The picker was cleared. The reference AND the strength go, for
            // the reason the strength lives here: one without the other is a
            // request ember refuses.
            staged.set(None);
            draft.update(|j| {
                j.init_image = String::new();
                j.strength = None;
            });
            return;
        };
        busy.set(true);
        spawn_local(async move {
            let bytes = match api::read_file(&file).await {
                Ok(b) => b,
                Err(e) => {
                    busy.set(false);
                    error.set(e);
                    return;
                }
            };
            match api::post_bytes::<Input>("/api/inputs", &bytes).await {
                Ok(input) => {
                    draft.update(|j| j.init_image = input.id.clone());
                    staged.set(Some(input));
                }
                // THE PREVIOUS IMAGE IS LEFT STAGED, deliberately. A refusal
                // here is "this file is too large" or "this file is not a png
                // or a jpeg" -- the operator picked the wrong one, and taking
                // away the right one they picked a minute ago would make the
                // mistake cost more than it should.
                Err(e) => error.set(e),
            }
            busy.set(false);
        });
    };

    view! {
        <div class="row image-input">
            <label class:denied=move || !offered()>
                <Help text="image" title="A png or a jpeg to generate from instead of from noise. It is uploaded to ember, which checks the header and keys it by the sha256 of the bytes -- the name of the file on your disk is never sent. At most 8 MiB and 8192 pixels on each axis; the server resizes it to the width and height above."/>
                <input type="file" accept="image/png,image/jpeg"
                    disabled=move || !offered() || busy.get()
                    on:change=pick
                />
            </label>
            <Show when=move || !draft.get().init_image.is_empty()>
                <label>
                    <Help text="strength" title="How much of the image is replaced, 0 to 1. 0 keeps it and 1 generates as though it were not there. Empty leaves the server's own default of 0.75, and an explicit 0 is a different request from an empty box -- so ember sends them differently."/>
                    <input type="number" step="0.05" min="0" max="1" placeholder="0.75"
                        prop:value=move || shown(draft.get().strength)
                        on:input=move |ev| draft.update(|j| j.strength = numfo(&ev))
                    />
                </label>
            </Show>
        </div>
        <Show when=move || staged.get().is_some()>
            <p class="hint staged">
                "staged: " {move || staged.get().map(|i| i.describe()).unwrap_or_default()}
            </p>
        </Show>
        <Show when=move || !error.get().is_empty()>
            <p class="hint error">{move || error.get()}</p>
        </Show>
    }
}

/// AdvancedForm is the optional parameters.
///
/// Its own component rather than more markup in JobForm, because JobForm was
/// already the longest view here and these controls share nothing with the
/// ones above except the draft they write to.
///
/// WHAT IS NOT HERE, AND IT IS NO LONGER THE WHOLE GROUP OF FIVE. `strength`
/// is here now, because there is an upload route for the image it applies to.
/// `img_cfg`, `control_strength`, `ip_adapter_strength` and
/// `increase_ref_index` are still absent, and the reason is sharper than "no
/// image input": each was sent to a live sd-server and none of them changed
/// anything observable. `control_strength` and `ip_adapter_strength` were
/// accepted and echoed nowhere -- they want `control_image` and
/// `ip_adapter_image` plus a ControlNet or an IP-Adapter in the serve argv.
/// `increase_ref_index` came back as `ref_image_args=ref_index_mode=increase`
/// and wants `ref_images`, which is an edit model's input. `img_cfg` was
/// accepted at 0, at -1 and at 99 and echoed at none of them, on a build that
/// publishes `generation_features.img_cfg` as true -- so there is no value
/// ember could send and no flag it could read that would tell an operator
/// whether it did anything. A control that cannot affect the generation reads
/// as working, which is worse than its absence.
///
/// `cache_mode`, `scm_mask`, `qwen_image_layers` and the two `custom_sigmas`
/// are absent because nothing here knows their value sets; they stay
/// reachable from `ember.yaml`'s `extra`.
///
/// WHAT IS GREYED, AND WHY IT IS GREYED RATHER THAN REFUSED LATER. Six of
/// these controls have a flag in one of the server's two feature maps, and a
/// control the server has already said no to is `disabled` and marked
/// `.denied` here. Without it the operator types a value, queues, rents a
/// card and reads the refusal -- or, where the server drops the parameter
/// instead of refusing it, reads an image that looks as though the model
/// ignored them. `eta` has no flag in either map and is never greyed; the
/// `.denied` hint below says so out loud, because a section where some
/// controls are gated and others are not is one an operator will otherwise
/// read as all-or-nothing.
#[component]
fn AdvancedForm(config: RwSignal<Config>, draft: RwSignal<Job>) -> impl IntoView {
    view! {
        // FIRST IN THE SECTION, because it is the only control here that
        // changes what the generation starts from rather than how it
        // proceeds.
        <ImageInput config=config draft=draft/>

        <div class="row">
            <label class:denied=move || !config.get().offers(|f| f.clip_skip)>
                <Help text="clip skip" title="How many of the text encoder's last layers to skip. Many SD1.5 finetunes are trained for 2. Empty leaves the model's own default, which is not 0 -- so an empty box and a 0 are different requests and ember sends them differently."/>
                <input type="number" placeholder="model"
                    disabled=move || !config.get().offers(|f| f.clip_skip)
                    prop:value=move || shown(draft.get().clip_skip)
                    on:input=move |ev| draft.update(|j| j.clip_skip = numo(&ev))
                />
            </label>
            // NOT GATED, and the only one in this row that is not: `eta`
            // appears in neither `features` nor `generation_features`, so
            // ember has no published answer about it. Inventing one would be
            // reading a schema in place of a response.
            <label><Help text="eta" title="Noise added back at each step by the ancestral samplers. 0 is the deterministic path and a real value, so empty means 'leave it to the server' and 0 does not. The server publishes no feature flag for eta, so ember cannot tell you in advance whether this model takes it."/>
                <input type="number" step="0.01" placeholder="server"
                    prop:value=move || shown(draft.get().eta)
                    on:input=move |ev| draft.update(|j| j.eta = numfo(&ev))
                />
            </label>
            <label class:denied=move || !config.get().offers(|f| f.flow_shift)>
                <Help text="flow shift" title="Shifts the flow-matching timestep schedule, which the rectified-flow models (FLUX, SD3) use. Empty leaves the server's own, which is not 0."/>
                <input type="number" step="0.1" placeholder="server"
                    disabled=move || !config.get().offers(|f| f.flow_shift)
                    prop:value=move || shown(draft.get().flow_shift)
                    on:input=move |ev| draft.update(|j| j.flow_shift = numfo(&ev))
                />
            </label>
            <label class:denied=move || !config.get().offers(|f| f.shifted_timestep)>
                <Help text="shifted timestep" title="Offsets the timestep the sampler starts from. The server's own default is 0, so here 0 and empty mean the same thing."/>
                <input type="number"
                    disabled=move || !config.get().offers(|f| f.shifted_timestep)
                    prop:value=move || draft.get().shifted_timestep
                    on:input=move |ev| draft.update(|j| j.shifted_timestep = num(&ev))
                />
            </label>
        </div>

        // One line naming what this server will not take, listed rather than
        // left to be inferred from which boxes are grey -- a disabled input is
        // easy to read as "not implemented in ember yet", which is the wrong
        // conclusion and the one that sends somebody to the issue tracker.
        <Show when=move || !denials(config.get()).is_empty()>
            <p class="hint denied">
                "This model does not take " {move || denials(config.get()).join(", ")}
                ", so those controls are unavailable and ember sends nothing for them.
                 It is the server's own answer, from its feature map, not a limit of
                 ember's: serve a model or recipe that supports them and they come back."
            </p>
        </Show>

        <div class="row">
            <label>
                <Help text="format" title="The image format the server encodes in. The menu is the server's own list, and a name outside it is refused at enqueue rather than after a rental."/>
                {move || match config.get().caps {
                    Some(c) if !c.output_formats.is_empty() => view! {
                        <select on:change=move |ev| draft.update(|j| j.output_format = event_target_value(&ev))>
                            <option value="">"default"</option>
                            <For each=move || c.output_formats.clone() key=|s| s.clone() let:s>
                                <option value=s.clone()>{s.clone()}</option>
                            </For>
                        </select>
                    }.into_any(),
                    _ => view! {
                        <input placeholder="png"
                            prop:value=move || draft.get().output_format
                            on:input=move |ev| draft.update(|j| j.output_format = event_target_value(&ev))
                        />
                    }.into_any(),
                }}
            </label>
            <label><Help text="compression" title="Quality for jpeg and webp, 1 to 100. png ignores it. 0 leaves the server's default of 100."/>
                <input type="number" min="0" max="100"
                    prop:value=move || draft.get().output_compression
                    on:input=move |ev| draft.update(|j| j.output_compression = num(&ev))
                />
            </label>
        </div>

        // Each block is gated on its own switch and sent only when the switch
        // is on. Unchecking it removes the whole object from the request
        // rather than sending it with enabled false, so what is on screen and
        // what is on the wire cannot disagree.
        <div class="row">
            <label class:denied=move || !config.get().offers(|f| f.hires)>
                <Help text="hires pass" title="Generates at a smaller size and then re-samples at a larger one, which is how a detailed large image is made without sampling the whole thing at full size. Turning it on with nothing else set uses the server's own scale and denoising."/>
                <input type="checkbox"
                    disabled=move || !config.get().offers(|f| f.hires)
                    prop:checked=move || draft.get().hires.is_some()
                    on:change=move |ev| {
                        let on = event_target_checked(&ev);
                        draft.update(|j| j.hires = if on {
                            Some(Hires { enabled: true, ..Default::default() })
                        } else {
                            None
                        });
                    }
                />
            </label>
        </div>
        <Show when=move || draft.get().hires.is_some()>
            <div class="row">
                <label>
                    <Help text="upscaler" title="How the smaller image is enlarged before the second pass. The menu is the server's own list: a model-backed upscaler resolves to a file in the server's upscaler directory, so a name it did not publish is refused here rather than being passed to a path join on the node."/>
                    {move || match config.get().caps {
                        Some(c) if !c.upscalers.is_empty() => view! {
                            <select on:change=move |ev| {
                                let v = event_target_value(&ev);
                                draft.update(|j| if let Some(h) = j.hires.as_mut() { h.upscaler = v });
                            }>
                                <option value="">"default"</option>
                                <For each=move || c.upscalers.clone() key=|s| s.clone() let:s>
                                    <option value=s.clone()>{s.clone()}</option>
                                </For>
                            </select>
                        }.into_any(),
                        _ => view! {
                            <input placeholder="Latent"
                                on:input=move |ev| {
                                    let v = event_target_value(&ev);
                                    draft.update(|j| if let Some(h) = j.hires.as_mut() { h.upscaler = v });
                                }
                            />
                        }.into_any(),
                    }}
                </label>
                <label><Help text="scale" title="How much larger the second pass is. 2 doubles each side, so it samples four times the area. 0 leaves the server's default of 2."/>
                    <input type="number" step="0.1"
                        prop:value=move || draft.get().hires.map(|h| h.scale).unwrap_or_default()
                        on:input=move |ev| { let v = numf(&ev);
                            draft.update(|j| if let Some(h) = j.hires.as_mut() { h.scale = v }); }
                    />
                </label>
                <label><Help text="hires steps" title="Sampling steps for the second pass. 0 leaves it to the server. This is paid on top of the first pass, so it is a second helping of the main price knob."/>
                    <input type="number"
                        prop:value=move || draft.get().hires.map(|h| h.steps).unwrap_or_default()
                        on:input=move |ev| { let v = num(&ev);
                            draft.update(|j| if let Some(h) = j.hires.as_mut() { h.steps = v }); }
                    />
                </label>
                <label><Help text="denoising" title="How much of the enlarged image the second pass is allowed to rewrite, 0 to 1. Low keeps the composition and adds detail; high reinvents it. 0 leaves the server's default of 0.7."/>
                    <input type="number" step="0.05" min="0" max="1"
                        prop:value=move || draft.get().hires.map(|h| h.denoising_strength).unwrap_or_default()
                        on:input=move |ev| { let v = numf(&ev);
                            draft.update(|j| if let Some(h) = j.hires.as_mut() { h.denoising_strength = v }); }
                    />
                </label>
                <label><Help text="target w" title="An explicit second-pass width instead of a scale. Checked against the server's own limits before anything is rented. 0 means use the scale."/>
                    <input type="number"
                        prop:value=move || draft.get().hires.map(|h| h.target_width).unwrap_or_default()
                        on:input=move |ev| { let v = num(&ev);
                            draft.update(|j| if let Some(h) = j.hires.as_mut() { h.target_width = v }); }
                    />
                </label>
                <label><Help text="target h" title="An explicit second-pass height instead of a scale. 0 means use the scale."/>
                    <input type="number"
                        prop:value=move || draft.get().hires.map(|h| h.target_height).unwrap_or_default()
                        on:input=move |ev| { let v = num(&ev);
                            draft.update(|j| if let Some(h) = j.hires.as_mut() { h.target_height = v }); }
                    />
                </label>
                <label><Help text="tile" title="Tile size for a model-backed upscaler, which lets it enlarge an image larger than its own working size. 0 leaves the server's default."/>
                    <input type="number"
                        prop:value=move || draft.get().hires.map(|h| h.upscale_tile_size).unwrap_or_default()
                        on:input=move |ev| { let v = num(&ev);
                            draft.update(|j| if let Some(h) = j.hires.as_mut() { h.upscale_tile_size = v }); }
                    />
                </label>
            </div>
        </Show>

        <div class="row">
            <label class:denied=move || !config.get().offers(|f| f.vae_tiling)>
                <Help text="vae tiling" title="Decodes the latent in overlapping tiles instead of all at once, which is how a large image fits in VRAM that could not hold the whole decode. Costs a little time and can leave faint seams."/>
                <input type="checkbox"
                    disabled=move || !config.get().offers(|f| f.vae_tiling)
                    prop:checked=move || draft.get().vae_tiling.is_some()
                    on:change=move |ev| {
                        let on = event_target_checked(&ev);
                        draft.update(|j| j.vae_tiling = if on {
                            Some(VaeTiling { enabled: true, ..Default::default() })
                        } else {
                            None
                        });
                    }
                />
            </label>
        </div>
        <Show when=move || draft.get().vae_tiling.is_some()>
            <div class="row">
                <label><Help text="tile w" title="Tile width for the decode, in latent units. 0 leaves the server's own."/>
                    <input type="number"
                        prop:value=move || draft.get().vae_tiling.map(|t| t.tile_size_w).unwrap_or_default()
                        on:input=move |ev| { let v = num(&ev);
                            draft.update(|j| if let Some(t) = j.vae_tiling.as_mut() { t.tile_size_w = v }); }
                    />
                </label>
                <label><Help text="tile h" title="Tile height for the decode, in latent units. 0 leaves the server's own."/>
                    <input type="number"
                        prop:value=move || draft.get().vae_tiling.map(|t| t.tile_size_h).unwrap_or_default()
                        on:input=move |ev| { let v = num(&ev);
                            draft.update(|j| if let Some(t) = j.vae_tiling.as_mut() { t.tile_size_h = v }); }
                    />
                </label>
                <label><Help text="overlap" title="How much neighbouring tiles share, 0 to 1. More overlap hides the seams and costs more decode. 0 leaves the server's default of 0.5."/>
                    <input type="number" step="0.05" min="0" max="1"
                        prop:value=move || draft.get().vae_tiling.map(|t| t.target_overlap).unwrap_or_default()
                        on:input=move |ev| { let v = numf(&ev);
                            draft.update(|j| if let Some(t) = j.vae_tiling.as_mut() { t.target_overlap = v }); }
                    />
                </label>
                <label><Help text="rel w" title="Tile width as a fraction of the image rather than an absolute size, 0 to 1. 0 means use the absolute tile width."/>
                    <input type="number" step="0.05" min="0" max="1"
                        prop:value=move || draft.get().vae_tiling.map(|t| t.rel_size_w).unwrap_or_default()
                        on:input=move |ev| { let v = numf(&ev);
                            draft.update(|j| if let Some(t) = j.vae_tiling.as_mut() { t.rel_size_w = v }); }
                    />
                </label>
                <label><Help text="rel h" title="Tile height as a fraction of the image, 0 to 1. 0 means use the absolute tile height."/>
                    <input type="number" step="0.05" min="0" max="1"
                        prop:value=move || draft.get().vae_tiling.map(|t| t.rel_size_h).unwrap_or_default()
                        on:input=move |ev| { let v = numf(&ev);
                            draft.update(|j| if let Some(t) = j.vae_tiling.as_mut() { t.rel_size_h = v }); }
                    />
                </label>
            </div>
        </Show>

        // Scale is the switch here rather than a checkbox, because upstream's
        // own "off" IS a zero scale: a checkbox would be a second way to say
        // the same thing and the two could disagree.
        <div class="row">
            <label class:denied=move || !config.get().offers(|f| f.slg)>
                <Help text="slg scale" title="Skip-layer guidance: re-runs some of the model's own layers with their output suppressed and pushes away from it. Scale is the switch -- 0 is off, and ember sends no skip-layer block at all, so the layers below do nothing until this is set."/>
                <input type="number" step="0.1"
                    disabled=move || !config.get().offers(|f| f.slg)
                    prop:value=move || draft.get().slg.map(|s| s.scale).unwrap_or_default()
                    on:input=move |ev| {
                        let v = numf(&ev);
                        draft.update(|j| {
                            if v == 0.0 {
                                j.slg = None;
                            } else {
                                j.slg.get_or_insert_with(Default::default).scale = v;
                            }
                        });
                    }
                />
            </label>
        </div>
        <Show when=move || draft.get().slg.is_some()>
            <div class="row">
                <label><Help text="slg layers" title="Which of the model's layers to skip, as a comma-separated list. Upstream's own default is 7,8,9. Which layers help is model-specific and found by trying."/>
                    <input placeholder="7,8,9"
                        on:input=move |ev| { let v = layers(&ev);
                            draft.update(|j| if let Some(s) = j.slg.as_mut() { s.layers = v }); }
                    />
                </label>
                <label><Help text="layer start" title="When in sampling the skipping begins, as a fraction of the run, 0 to 1. Upstream's default is 0.01."/>
                    <input type="number" step="0.01" min="0" max="1"
                        prop:value=move || draft.get().slg.map(|s| s.layer_start).unwrap_or_default()
                        on:input=move |ev| { let v = numf(&ev);
                            draft.update(|j| if let Some(s) = j.slg.as_mut() { s.layer_start = v }); }
                    />
                </label>
                <label><Help text="layer end" title="When the skipping stops, as a fraction of the run, 0 to 1. Upstream's default is 0.2. A start after the end is refused before anything is rented."/>
                    <input type="number" step="0.01" min="0" max="1"
                        prop:value=move || draft.get().slg.map(|s| s.layer_end).unwrap_or_default()
                        on:input=move |ev| { let v = numf(&ev);
                            draft.update(|j| if let Some(s) = j.slg.as_mut() { s.layer_end = v }); }
                    />
                </label>
            </div>
        </Show>

        <p class="hint">
            "Inpainting, ControlNet, IP-Adapter and LoRA are not here. Each needs an
             image slot or a model root of its own, and each was tried against a real
             server first: sent without the slot they are accepted, dropped and
             reported nowhere, which is worse than being absent. They are reachable
             from "<code>"ember.yaml"</code>"'s "<code>"extra"</code>", which a request
             may not set."
        </p>
    }
}

/// Queue is what is waiting, running and finished.
#[component]
fn Queue(watching: RwSignal<String>) -> impl IntoView {
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
                <tbody>
                    <For each=move || list.get().requests.items key=|r| (r.id.clone(), r.status.clone()) let:r>
                        {
                            let id = r.id.clone();
                            let running = list.get().running == r.id;
                            let cancellable = r.is_cancellable();
                            let cancel_id = r.id.clone();
                            let asked_id = r.id.clone();
                            let pending_id = r.id.clone();
                            view! {
                                <tr class:running=running
                                    class:watching=move || watching.get() == id>
                                    <td>
                                        <button class="link" on:click={
                                            let id = r.id.clone();
                                            move |_| watching.set(id.clone())
                                        }>{r.id.chars().take(8).collect::<String>()}</button>
                                    </td>
                                    // THE REASON, IN THE ROW THAT HAS IT.
                                    //
                                    // `Request::error` reached this page on
                                    // every route and the socket and was read
                                    // by no view, so a request stopped by a
                                    // money ceiling or by a sampler the caps
                                    // do not list said `failed` and nothing
                                    // else. The operator-visible state of a
                                    // failed generation was one word.
                                    //
                                    // IN THE ROW AND NOT UNDER THE TABLE,
                                    // which is where the panel's other two
                                    // lines live. A listing can hold several
                                    // failed requests at once and a panel
                                    // line cannot say which one it is about;
                                    // the cancel refusal can, because there
                                    // is one ask in flight. It also keeps
                                    // this out of `p.error`, the class the
                                    // cancel lab counts as siblings of the
                                    // table to assert a refusal is gone --
                                    // and a reason that is still true is not
                                    // a refusal anybody dismissed.
                                    //
                                    // A `title` as well as the text, because
                                    // `queue.fail` writes whatever the
                                    // planner said and some of those are long
                                    // enough to wrap a cell.
                                    <td class="status">{r.status.clone()}
                                        {match r.reason() {
                                            None => ().into_any(),
                                            Some(why) => view! {
                                                <span class="why" title=why.to_string()>
                                                    {why.to_string()}
                                                </span>
                                            }.into_any(),
                                        }}
                                    </td>
                                    <td>{format!("{} job(s)", r.jobs.len())}</td>
                                    <td>{r.gpu.clone()}</td>
                                    <td class="price">
                                        {if r.cost.is_free() { String::new() }
                                         else { r.cost.text().to_string() }}
                                    </td>
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
                            }
                        }
                    </For>
                </tbody>
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

/// numo and numfo read a field whose zero is a real value.
///
/// An empty box is None and is not sent at all; a box holding 0 is Some(0) and
/// is. clip_skip and eta both default to something other than zero on the
/// server -- -1 and null -- so collapsing "empty" into 0 the way `num` does
/// would turn "leave it alone" into an instruction. Unparseable is None rather
/// than 0 for the same reason: a half-typed "-" must not post a value.
fn numo(ev: &leptos::ev::Event) -> Option<i32> {
    event_target_value(ev).trim().parse().ok()
}

fn numfo(ev: &leptos::ev::Event) -> Option<f64> {
    event_target_value(ev).trim().parse().ok()
}

/// layers reads a comma-separated list of layer indices.
///
/// Unparseable entries are dropped rather than refused, because this is typed
/// into live: "7,8," while adding a ninth is a normal intermediate state and
/// clearing the field on every keystroke would make it impossible to use. The
/// server checks the result, and so does ember before anything is rented.
fn layers(ev: &leptos::ev::Event) -> Vec<i32> {
    event_target_value(ev)
        .split(',')
        .filter_map(|s| s.trim().parse().ok())
        .collect()
}

/// shown turns an optional number back into a box's contents.
///
/// An empty string for None, so the control reads as "not set" rather than as
/// a 0 the operator did not type -- which would then be posted on the next
/// edit to any other field.
fn shown<T: std::fmt::Display>(v: Option<T>) -> String {
    v.map(|v| v.to_string()).unwrap_or_default()
}

/// denials names the advanced controls this server has said no to.
///
/// Built from the same `offers` calls the controls are gated on, so the
/// sentence and the grey boxes cannot disagree: a control added to the gate
/// without a name here, or a name here without a gate, is what would make the
/// hint a caption that is true of some of them.
///
/// The names are the control labels and not the API's field names, because the
/// operator is looking at the labels. The flag each one came from is in the
/// refusal `sdserver.featureRefusal` builds, which is where somebody who wants
/// to check the server for themselves ends up.
fn denials(c: Config) -> Vec<&'static str> {
    let mut out = Vec::new();
    for (name, offered) in [
        // The same words ember's own refusal uses -- sdserver's
        // featureRefusal says "an image input" -- so an operator who reads
        // the hint and then reads a 400 sees one name for one thing.
        ("an image input", c.offers(|f| f.init_image)),
        ("clip skip", c.offers(|f| f.clip_skip)),
        ("flow shift", c.offers(|f| f.flow_shift)),
        ("shifted timestep", c.offers(|f| f.shifted_timestep)),
        ("skip-layer guidance", c.offers(|f| f.slg)),
        ("vae tiling", c.offers(|f| f.vae_tiling)),
        ("the hires pass", c.offers(|f| f.hires)),
    ] {
        if !offered {
            out.push(name);
        }
    }
    out
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
    use super::{listed, urlish};
    use crate::model::{Answer, Listing, Offer, Offers};
    use leptos::prelude::*;

    fn one(provider: &str, offers: Vec<Offer>, error: &str) -> Offers {
        Offers {
            listings: vec![Listing {
                provider: provider.into(),
                offers,
                error: error.into(),
            }],
        }
    }

    fn card(token: &str, gpu: &str) -> Offer {
        Offer {
            token: token.into(),
            gpu: gpu.into(),
            ..Default::default()
        }
    }

    /// THE SECOND QUERY'S ANSWER, which is #130.
    ///
    /// The provider's name is the same in both, which is exactly why the
    /// keyed `<For>` this replaced could never show the second: a retained
    /// key is moved and never rebuilt, and the item is handed to the child by
    /// value. So the assertion is that the closure answers from the signal
    /// each time it is called, and the signal is written the way the button
    /// writes it -- `offers.set` with one provider's new listing.
    ///
    /// Three presses and not two: a table that has gone and come back is the
    /// ordinary way the card filter is used, and a fix that only cleared the
    /// rows once would pass against two.
    #[test]
    fn a_second_query_of_one_provider_replaces_the_first_answer() {
        let sig = RwSignal::new(one("runpod", vec![card("t1", "h100-80g")], ""));
        let answers = listed(sig);

        match &answers()[..] {
            [Answer::Rentable { offers }] => {
                assert_eq!(offers.len(), 1);
                assert_eq!(offers[0].gpu, "h100-80g");
            }
            other => panic!("the first answer is not a table: {other:?}"),
        }

        // Narrowed to a card this provider does not have. Same provider, so
        // the same key.
        sig.set(one("runpod", Vec::new(), ""));
        match &answers()[..] {
            [Answer::Nothing { provider }] => assert_eq!(provider, "runpod"),
            other => panic!("a narrowed filter still shows the first answer: {other:?}"),
        }

        // And back, because an operator widens the filter again.
        sig.set(one("runpod", vec![card("t2", "rtx4090")], ""));
        match &answers()[..] {
            [Answer::Rentable { offers }] => assert_eq!(offers[0].gpu, "rtx4090"),
            other => panic!("the third answer did not arrive: {other:?}"),
        }
    }

    /// The error case beside the rows, because a page that refreshed its rows
    /// and still swallowed the error would pass the test above on its own --
    /// which is the `AssertNoStray` shape, a check that can pass for either
    /// reason.
    ///
    /// This is the measured failure in #130: the server switched to zero
    /// offers plus a `dial tcp` sentence and the table went on showing a card
    /// at a price.
    #[test]
    fn a_provider_that_could_not_be_asked_replaces_the_table_it_had() {
        let sig = RwSignal::new(one("runpod", vec![card("t1", "h100-80g")], ""));
        let answers = listed(sig);
        assert!(matches!(answers()[0], Answer::Rentable { .. }));

        sig.set(one(
            "runpod",
            Vec::new(),
            "get https://rest.runpod.io/v1/pods: dial tcp: no such host",
        ));
        match &answers()[..] {
            [Answer::Failed { provider, why }] => {
                assert_eq!(provider, "runpod");
                assert!(why.contains("no such host"), "{why}");
            }
            other => panic!("the failure did not reach the page: {other:?}"),
        }
    }

    /// The precedence `Listing::answer` exists for, asserted on the one shape
    /// no writer produces today: an error beside offers. The wire decides
    /// what this page can be handed, and the direction the mistake has to
    /// fall is towards saying the provider could not be asked.
    #[test]
    fn an_error_wins_over_the_offers_beside_it() {
        let l = Listing {
            provider: "runpod".into(),
            offers: vec![card("t1", "h100-80g")],
            error: "dial tcp: no such host".into(),
        };
        assert!(matches!(l.answer(), Answer::Failed { .. }));
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
}
