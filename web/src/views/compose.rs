//! What to rent and what to generate: the landing page.
//!
//! EXTRACTED AND NOT CHANGED, which is the half of #139 slice 3 worth saying
//! out loud: everything below this line moved out of a 2,549-line `views.rs`
//! and the only edits are the two a route forces -- `watching` is gone and
//! queueing a request navigates to its page instead.

use leptos::prelude::*;
use leptos::task::spawn_local;

use crate::api;
use crate::model::{
    Answer, Composing, Config, Hires, Input, Job, Listing, NewRequest, Offers, Utterance,
    VaeTiling, Voice,
};
use crate::route::{self, Route};

/// Compose is the card picker and the batch.
///
/// THE DRAFT AND THE BATCH ARE STILL IN HERE, which is a known gap rather
/// than a decision: this component is the page now, so navigating away
/// disposes both and a composed batch does not survive a trip to the queue
/// and back. #139's own done-when asks for that and its slice plan puts it in
/// the slice after this one, where the draft and the batch move above the
/// route match. Written down because the failure is quiet -- the form comes
/// back empty and nothing says why.
#[component]
pub fn Compose(config: RwSignal<Config>, route: RwSignal<Route>) -> impl IntoView {
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

    // WHICH WORKLOAD, and the empty string is an image generation -- which is
    // what `genstore.KindOf` resolves an absent `kind` to, so the panel's
    // opening state is the one that was here before speech existed and the
    // body it posts is unchanged.
    let kind = RwSignal::new(String::new());

    // The utterance being composed, which is the speech half's draft.
    //
    // A SECOND DRAFT AND NOT MORE FIELDS ON THE FIRST, because the server
    // refuses a job that sets both halves: `crossKind` compares a speech
    // job's remaining fields against the zero value, and this panel's image
    // draft opens at 512, 512 and 20 -- so sharing one draft would post
    // exactly the three leftovers that refuse the whole request. They share
    // the `batch`, because a batch is a list of `Job` and the utterance is a
    // field on one.
    let utterance = RwSignal::new(Utterance::default());

    // The voice defaults to the first one ember published, so the chooser is
    // usable without a choice being made -- the same shape the provider
    // selector uses, and the first of the two mechanisms that stop an empty
    // `voice_id` reaching the server. The second is `Utterance::refusal`.
    Effect::new(move |_| {
        let c = config.get();
        if utterance.get_untracked().voice_id.is_empty() {
            if let Some(v) = c.voices.first() {
                let id = v.id.clone();
                utterance.update(|u| u.voice_id = id);
            }
        }
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
            let mut path = format!("/api/offers?provider={}", api::urlish(&p));
            if !g.is_empty() {
                path += &format!("&gpu={}", api::urlish(&g));
            }
            match api::get::<Offers>(&path).await {
                Ok(o) => offers.set(o),
                Err(e) => error.set(e),
            }
        });
    };

    // Whether there is a choice of kind to offer. Built outside the macro
    // because a bare `>` inside a `view!` attribute closes the tag, and the
    // parenthesised form is a lint `make web-test` turns into an error.
    let choosable = move || config.get().kinds.len() > 1;

    let queue = move || {
        error.set(String::new());
        let mut jobs = batch.get();

        // WHAT IS SENT IS DECIDED BY THE SAME METHOD THAT DECIDED WHAT IS ON
        // SCREEN, so the panel cannot show one kind's controls and post the
        // other's. A refusal here is the one `composing` already built -- the
        // sentence the panel is printing -- rather than a second wording of
        // it.
        let speech = match config.get().composing(&kind.get()) {
            Composing::Image => false,
            Composing::Speech(_) => true,
            Composing::Refused(why) => {
                error.set(why);
                return;
            }
        };

        // A single composed job needs no "add" first: the obvious thing to do
        // with one prompt, or one sentence, is to run it.
        if jobs.is_empty() {
            if speech {
                let u = utterance.get();
                if let Some(why) = u.refusal() {
                    error.set(why);
                    return;
                }
                // FROM `Job::default()` AND NOT FROM THE IMAGE DRAFT, which is
                // the whole reason there are two drafts: every image field has
                // to be its zero or `crossKind` refuses the request naming the
                // index.
                jobs.push(Job {
                    tts: Some(u),
                    ..Default::default()
                });
            } else if !draft.get().prompt.trim().is_empty() {
                jobs.push(draft.get());
            }
        }
        if jobs.is_empty() {
            error.set("nothing to generate: type a prompt".into());
            return;
        }

        let body = NewRequest {
            kind: kind.get(),
            provider: provider.get(),
            gpu: gpu.get(),
            ttl: ttl.get(),
            max_hourly_usd: max_hourly.get(),
            // NOT SENT FOR SPEECH, and that is not tidiness: a preview is a
            // frame of a sampling pass, piper has no pass and no frames, and
            // the columns would record a mode no driver on that rental reads.
            // A figure stored for a run that cannot produce it is the kind of
            // value that reads like a fact.
            preview: if speech {
                String::new()
            } else if preview.get().is_empty() {
                config.get().preview
            } else {
                preview.get()
            },
            preview_interval: if speech {
                0
            } else if preview_every.get() > 0 {
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
                // AND THE PAGE FOLLOWS IT. This used to write a `watching`
                // signal that a panel on the same screen read; the watcher is
                // a page of its own now, so the thing to do with a queued id
                // is go to it -- as a navigation and not a redraw, because
                // the operator has to be able to come back here with the back
                // button and because the address they end up on is the one
                // worth reloading or sending to somebody.
                Ok(q) => {
                    batch.set(Vec::new());
                    route::go(route, Route::Request(q.id));
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

            // THE KIND, FIRST, because it decides which of the controls below
            // it mean anything -- and an operator who has typed a prompt into
            // a panel that was going to speak it has lost the thing they
            // typed.
            //
            // BUILT FROM `config.kinds` AND NEVER SPELLED HERE, which is the
            // same rule the voice list follows: `genstore.Kinds()` is the one
            // list, and a copy in this crate is a place for the kind added
            // last to be forgotten. It is also what closes the window
            // `/api/config` leaves open -- the list is empty until the config
            // answers, so there is no speech option to pick before ember has
            // said whether it has a voice. `Config::composing`'s unanswered
            // arm is the second mechanism rather than the first.
            //
            // ONE OPTION IS NO CHOICE, so the selector is hidden rather than
            // shown with a single entry: a `<select>` an operator cannot
            // change reads as a control that is broken.
            <Show when=choosable>
                <div class="row">
                    <label>
                        <Help text="kind" title="Which workload to rent for. sd generates images through a resident sd-server; tts speaks text with piper. One rental runs one of them, so changing this empties the batch."/>
                        <select on:change=move |ev| {
                            kind.set(event_target_value(&ev));
                            // THE BATCH GOES WITH IT, because a batch of image
                            // jobs posted as a speech request is a 400 naming
                            // an index the operator cannot see, and the reverse
                            // is a generation billed for saying nothing about
                            // what they asked for. `kind` is on the request, so
                            // a mixed batch is not a thing the wire can carry;
                            // this is the page not building one.
                            batch.set(Vec::new());
                            error.set(String::new());
                        }>
                            {move || config.get().kinds.into_iter().map(|k| {
                                let label = k.clone();
                                view! { <option value=k>{label}</option> }
                            }).collect::<Vec<_>>()}
                        </select>
                    </label>
                </div>
            </Show>

            // WHICH CONTROLS, FROM THE METHOD AND NOT FROM A CHAIN OF `if`
            // HERE. Read inside the closure, which is the rule four keyed-list
            // defects on this page were paid for: a kind changed after the
            // first render has to rebuild these, and a value read outside
            // would leave the image controls on screen over a speech request.
            {move || match config.get().composing(&kind.get()) {
                Composing::Image => view! {
                    <JobForm config=config draft=draft />
                    <PreviewForm config=config preview=preview preview_every=preview_every />
                }.into_any(),
                Composing::Speech(voices) => view! {
                    <SpeechForm voices=voices utterance=utterance />
                }.into_any(),
                // A SENTENCE AND NOT A BLANK PANEL. Both reasons this fires
                // are states an operator can be in -- a config with no
                // `tts.voices`, and a page older than the server -- and a
                // panel that simply had nothing in it would read as a page
                // that failed to load.
                Composing::Refused(why) => view! {
                    <p class="hint denied">{why}</p>
                }.into_any(),
            }}

            <div class="row">
                <button on:click=move |_| {
                    // THE SAME TWO DRAFTS, and which one is added is the same
                    // decision the controls came from. Each row is checked as
                    // it goes in, because `queue` only checks the draft it
                    // would push itself.
                    match config.get().composing(&kind.get()) {
                        Composing::Image => {
                            let j = draft.get();
                            if j.prompt.trim().is_empty() {
                                error.set("nothing to add: type a prompt".into());
                            } else {
                                error.set(String::new());
                                batch.update(|b| b.push(j));
                            }
                        }
                        Composing::Speech(_) => {
                            let u = utterance.get();
                            match u.refusal() {
                                Some(why) => error.set(why),
                                None => {
                                    error.set(String::new());
                                    batch.update(|b| b.push(Job {
                                        tts: Some(u),
                                        ..Default::default()
                                    }));
                                }
                            }
                        }
                        Composing::Refused(why) => error.set(why),
                    }
                }>"add to batch"</button>

                <button class="primary" on:click=move |_| queue()>
                    {move || {
                        let speech = matches!(
                            config.get().composing(&kind.get()),
                            Composing::Speech(_)
                        );
                        // The verb is the kind's, because "generate 3 job(s)"
                        // over three sentences of speech is a label describing
                        // the other workload.
                        let verb = if speech { "speak" } else { "generate" };
                        let n = batch.get().len();
                        if n == 0 { verb.to_string() }
                        else { format!("{verb} {n} job(s) on one rental") }
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

/// SpeechForm is one utterance.
///
/// WHAT IS NOT HERE IS THE IMAGE HALF, which is the whole of #190's browser
/// side: the geometry, the sampler, the steps, the seed, the preview mode and
/// the advanced section are all parameters of a sampling pass, and piper has
/// no pass. A page that showed them over a speech request would be accepting
/// values that reach the server and are refused -- `crossKind` compares a
/// speech job against the zero value -- or, worse, values that are dropped,
/// which reads as "the model ignored them".
///
/// THE VOICES ARE A PARAMETER AND NOT A SIGNAL READ IN HERE, so this component
/// cannot be rendered without a list: an empty `<select>` posts a blank
/// `voice_id`, which the server refuses after the operator has typed. The
/// decision that there IS a list to hand over belongs to `Config::composing`,
/// where `cargo test` reaches it.
///
/// THE FIVE PIPER PARAMETERS ARE ABSENT ON PURPOSE. `speaker`,
/// `length_scale`, `noise_scale`, `noise_w` and `sentence_silence` are on the
/// server's wire type and none of them has been measured from this page -- so
/// a slider whose effect nobody has heard would be the "reads as working"
/// defect in a control. They stay reachable from `ember.yaml`.
#[component]
fn SpeechForm(voices: Vec<Voice>, utterance: RwSignal<Utterance>) -> impl IntoView {
    view! {
        <label class="wide">
            <Help text="text" title="What to say. Arbitrary text: it is sent in the request body and never on a command line, so nothing in it can be read as a flag. piper splits it into sentences and speaks them in one pass."/>
            <textarea
                rows="4"
                prop:value=move || utterance.get().text
                on:input=move |ev| utterance.update(|u| u.text = event_target_value(&ev))
            />
        </label>

        <div class="row">
            <label>
                <Help text="voice" title="Which speech model says it. The list is the one your ember.yaml published: a request names a voice by id and has no field for a path, so this is the only way a page could know what to send."/>
                <select on:change=move |ev| {
                    let v = event_target_value(&ev);
                    utterance.update(|u| u.voice_id = v);
                }>
                    // NO KEY, AND NO `<For>`. A keyed list re-renders a row
                    // only when its key changes and hands the item over by
                    // value, which is how this page once showed one
                    // provider's first answer for ever. There is no identity
                    // to retain across two configs here, and the list is
                    // rebuilt with the block that holds it.
                    {
                        let now = utterance.get_untracked().voice_id;
                        voices.into_iter().map(|v| {
                            let sel = v.id == now;
                            let label = v.label().to_string();
                            view! { <option value=v.id.clone() selected=sel>{label}</option> }
                        }).collect::<Vec<_>>()
                    }
                </select>
            </label>
            <label>
                <Help text="name" title="The filename stem for this utterance's audio, written as name-00.wav. Sanitised rather than refused, and a stem that cleans away to nothing is an error."/>
                <input placeholder="greeting"
                    prop:value=move || utterance.get().output
                    on:input=move |ev| utterance.update(|u| u.output = event_target_value(&ev))
                />
            </label>
        </div>

        <p class="hint">
            "piper runs on a CPU, so a speech rental needs no card -- pick a provider's
             cheapest machine. There is no progress inside one utterance: a sentence takes
             a fraction of a second, so a job either has not started or is done."
        </p>
    }
}

/// PreviewForm is how an in-progress frame is rendered.
///
/// EXTRACTED FROM THE PANEL BECAUSE IT IS THE IMAGE KIND'S, and that is the
/// correction rather than tidiness: it was unconditional, and a preview mode
/// is a frame of a sampling pass. piper has no pass and no frames, so these
/// two controls over a speech request would be setting a value no driver on
/// that rental reads -- and the cost is not nothing, because `preview` is a
/// stored column that `ember history` prints.
#[component]
fn PreviewForm(
    config: RwSignal<Config>,
    preview: RwSignal<String>,
    preview_every: RwSignal<i32>,
) -> impl IntoView {
    view! {
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
pub fn Listings(offers: RwSignal<Offers>, gpu: RwSignal<String>) -> impl IntoView {
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

// `urlish` MOVED TO `api`, which is where the second caller is: #139 slice 3
// made `location.pathname` an input to the websocket's query string, and two
// copies of one escaping rule is two places for one of them to drift. The
// call sites here are unchanged apart from the module name.

#[cfg(test)]
mod tests {
    use super::listed;
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
}
