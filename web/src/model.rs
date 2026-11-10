//! The wire types and the state they fold into.
//!
//! NO `web_sys` IN THIS MODULE, deliberately. Everything here is a decision
//! about data -- what an event means, how a request's progress accumulates,
//! which money figure to show -- so it compiles for the host and `cargo test`
//! exercises it with no browser and no headless Chrome. The parts that cannot
//! be tested that way are in `api` and `views`, and they are deliberately
//! thin.
//!
//! The field names mirror `internal/webapi/wire.go`. That file is the one
//! place ember decides what a browser may see; this one is the one place the
//! browser decides what to make of it. Neither marshals a domain type.

use serde::{Deserialize, Deserializer, Serialize};

/// nullable accepts `null` where a value is expected, and uses the default.
///
/// `#[serde(default)]` covers a field that is MISSING and not one that is
/// present and null -- which is a type error, and one that fails the whole
/// decode rather than the field.
///
/// That is not hypothetical here: Go marshals a nil slice as `null`, so an
/// envelope carrying no new events arrived as `{"events":null,...}` and was
/// dropped entirely, taking the request state with it. The UI showed a job
/// stuck at "starting" while the image was already on disk. The server sends
/// `[]` now; this is the other half, because the rule has to hold for a field
/// somebody adds later too.
fn nullable<'de, D, T>(d: D) -> Result<T, D::Error>
where
    D: Deserializer<'de>,
    T: Deserialize<'de> + Default,
{
    Ok(Option::deserialize(d)?.unwrap_or_default())
}

/// Money, as ember sends it: twice.
///
/// The integer is what this UI sorts and compares on; the string is what it
/// displays. `node.MicroUSD.String` in Go is `"$%.6g"`, which nothing here
/// could reproduce by accident -- so formatting a price in Rust would create a
/// second renderer that disagrees with the one `ember up` prints, in a
/// language where the first one's tests cannot reach it.
#[derive(Clone, Debug, Default, PartialEq, Eq, Deserialize, Serialize)]
pub struct Money {
    #[serde(default)]
    pub micro_usd: i64,
    #[serde(default)]
    pub display: String,
}

impl Money {
    /// What to show. Never computed from `micro_usd`.
    pub fn text(&self) -> &str {
        if self.display.is_empty() {
            "$0"
        } else {
            &self.display
        }
    }

    pub fn is_free(&self) -> bool {
        self.micro_usd == 0
    }
}

#[derive(Clone, Debug, Default, Deserialize)]
pub struct Provider {
    pub name: String,
    #[serde(default)]
    pub free: bool,
    #[serde(default)]
    pub stores: bool,
}

#[derive(Clone, Debug, Default, Deserialize)]
pub struct Ceilings {
    #[serde(default)]
    pub ttl: String,
    #[serde(default)]
    pub max_hourly: Money,
    #[serde(default)]
    pub max_run: Money,
}

#[derive(Clone, Debug, Default, Deserialize)]
pub struct Limits {
    #[serde(default)]
    pub min_width: i32,
    #[serde(default)]
    pub max_width: i32,
    #[serde(default)]
    pub min_height: i32,
    #[serde(default)]
    pub max_height: i32,
    #[serde(default)]
    pub max_batch_count: i32,
}

#[derive(Clone, Debug, Default, Deserialize)]
pub struct Caps {
    #[serde(default)]
    pub model: String,
    #[serde(default, deserialize_with = "nullable")]
    pub samplers: Vec<String>,
    #[serde(default, deserialize_with = "nullable")]
    pub schedulers: Vec<String>,
    #[serde(default, deserialize_with = "nullable")]
    pub preview_modes: Vec<String>,
    #[serde(default)]
    pub limits: Limits,

    /// The server's own allow-lists for the output format and the hires
    /// upscaler, read for the same reason `samplers` is: a menu built from
    /// them cannot offer a name that enqueue will refuse.
    #[serde(default, deserialize_with = "nullable")]
    pub output_formats: Vec<String>,
    #[serde(default, deserialize_with = "nullable")]
    pub upscalers: Vec<String>,

    /// What the server said the loaded model takes. See [`Features`].
    #[serde(default)]
    pub features: Features,
}

/// The server's own feature flags, for the controls that have one.
///
/// `Option` per flag, and the three states are all distinct: absent is "this
/// build published no such flag", `Some(true)` is yes and `Some(false)` is no.
/// A page that read absent as no would grey every one of these controls
/// against a server that does not publish the maps -- the same failure
/// `Config::caps` is an `Option` to avoid, one level down. Only `Some(false)`
/// takes a control away.
///
/// `eta` has no flag in either of the server's maps, so it is never gated
/// here; the format menu is built from `output_formats`, which is a list and a
/// better answer than a boolean.
#[derive(Clone, Debug, Default, Deserialize)]
pub struct Features {
    /// Whether the loaded model takes an image input at all. It gates the
    /// file picker and, through it, the strength control -- which ember
    /// refuses without an image, because a server measured accepting one and
    /// silently dropping it.
    #[serde(default)]
    pub init_image: Option<bool>,
    #[serde(default)]
    pub clip_skip: Option<bool>,
    #[serde(default)]
    pub flow_shift: Option<bool>,
    #[serde(default)]
    pub shifted_timestep: Option<bool>,
    #[serde(default)]
    pub slg: Option<bool>,
    #[serde(default)]
    pub vae_tiling: Option<bool>,
    #[serde(default)]
    pub hires: Option<bool>,
}

/// Whether the server has said no to this flag.
///
/// A free function rather than a method on `Option<bool>`, which is not ours
/// to extend, and spelled once so the nil case cannot be written two ways:
/// `!flag.unwrap_or(true)` and `flag == Some(false)` agree, and
/// `!flag.unwrap_or(false)` -- the plausible slip -- greys everything.
pub fn denied(flag: Option<bool>) -> bool {
    flag == Some(false)
}

/// One speech model this process publishes, as a page reads it.
///
/// AN ID AND A NAME AND NOTHING ELSE, which is the server's projection doing
/// its job: `config.TTSVoice` holds two filenames on the node and neither is
/// on this wire. A request names a voice by **id**, so the id is the only
/// thing a page can send and the name is the only thing it can show.
#[derive(Clone, Debug, Default, PartialEq, Eq, Deserialize)]
pub struct Voice {
    pub id: String,
    #[serde(default)]
    pub name: String,
}

impl Voice {
    /// What the chooser shows for it.
    ///
    /// The id where there is no name, because an option with no text is an
    /// option nobody can pick -- and `tts.voices` makes `name` optional while
    /// `id` is what the request carries anyway.
    pub fn label(&self) -> &str {
        if self.name.is_empty() {
            &self.id
        } else {
            &self.name
        }
    }
}

/// What the compose panel offers for a chosen kind.
///
/// A METHOD'S ANSWER AND NOT A CHAIN OF `if` IN THE VIEW, which is the settled
/// shape here (`Rows::note`, `Request::reason`, `Listing::answer`,
/// `Stream::note`, `Progress::rental`): the set of controls a kind shows is a
/// decision, and a decision inside a `view!` macro can only be checked by
/// driving a browser.
#[derive(Clone, Debug, PartialEq, Eq)]
pub enum Composing {
    /// The image controls: a prompt, the geometry, the sampler, the steps.
    /// What is on screen today, and what `sd` and an absent kind both mean.
    Image,

    /// The speech controls: a text area and a chooser over these voices.
    ///
    /// Carries the list rather than a flag, so a view cannot reach the
    /// chooser without having been handed something to put in it -- which is
    /// the state an empty `<select>` posting a blank `voice_id` comes from.
    Speech(Vec<Voice>),

    /// A kind this process will not run, and the sentence saying why.
    Refused(String),
}

/// What the server will let this UI offer.
#[derive(Clone, Debug, Default, Deserialize)]
pub struct Config {
    #[serde(default, deserialize_with = "nullable")]
    pub providers: Vec<Provider>,
    #[serde(default)]
    pub ceilings: Ceilings,
    #[serde(default)]
    pub allow_paid: bool,

    /// Which workloads this process can run, in the order to offer them.
    ///
    /// READ FROM THE SERVER AND NEVER SPELLED HERE, for the reason the voices
    /// are: `genstore.Kinds()` is the one list, and a second copy in this
    /// crate is a place for the kind added last to be forgotten.
    ///
    /// EMPTY MEANS `/api/config` HAS NOT ANSWERED, not that this process runs
    /// nothing -- the `caps` distinction one field over. `genstore.Kinds()`
    /// is derived from nothing and cannot be empty, and
    /// `TestConfig_PublishesAtLeastOneKind` is the exit code behind that, so
    /// the only way this list is empty is that nothing has replaced
    /// `Config::default()` yet.
    #[serde(default, deserialize_with = "nullable")]
    pub kinds: Vec<String>,

    /// The speech models this process publishes, which may legitimately be
    /// none: an `ember serve` whose config has no `tts.voices` serves no
    /// speech to a browser, and `POST /api/requests` refuses one naming that
    /// key.
    #[serde(default, deserialize_with = "nullable")]
    pub voices: Vec<Voice>,
    #[serde(default)]
    pub servable: bool,
    #[serde(default)]
    pub store: String,
    #[serde(default)]
    pub mount: String,
    #[serde(default)]
    pub preview: String,
    #[serde(default)]
    pub preview_interval: i32,

    /// Absent until something has asked a generation server.
    ///
    /// `Option` and not an empty `Caps`, because "nothing has asked yet" and
    /// "the server reported no samplers" are different answers -- and a UI
    /// treating the first as the second would grey out every control before
    /// the operator's first rental.
    #[serde(default, deserialize_with = "nullable")]
    pub caps: Option<Caps>,
}

impl Config {
    /// Whether a provider can be rented from at all.
    ///
    /// A UI that offered a paid card on a server started without
    /// `--allow-paid` would be offering a button that cannot work, and the
    /// refusal would arrive after the operator chose.
    pub fn can_rent(&self, name: &str) -> bool {
        self.providers
            .iter()
            .any(|p| p.name == name && (p.free || self.allow_paid))
    }

    /// Whether a control gated on one of the server's feature flags may be
    /// offered.
    ///
    /// True before anything has asked a server, which is the same answer the
    /// sampler menu gives: no capabilities means nothing to check against, and
    /// greying out the advanced section before the operator's first rental
    /// would make the first request the one that can say least.
    ///
    /// Takes the field as a function pointer rather than a string key, so a
    /// control naming a flag the wire type does not carry is a compile error
    /// rather than a gate that is silently always open.
    pub fn offers(&self, pick: fn(&Features) -> Option<bool>) -> bool {
        match &self.caps {
            None => true,
            Some(c) => !denied(pick(&c.features)),
        }
    }

    /// What this process cannot run, where that is worth saying in red.
    ///
    /// ONE SENTENCE PER STATE AND NOT ONE FOR BOTH, which is the correction.
    /// This was `!servable`, and the line it printed said "a queued request
    /// would have nowhere to run" -- true while an image generation was the
    /// only workload, and wrong for the configuration speech is FOR: a
    /// `tts.voices` list with no `sd:` block runs speech perfectly. A warning
    /// that is wrong about the thing the operator is looking at is worse than
    /// no warning, which is the `proj` caption's rule and the false-abandoned
    /// one.
    ///
    /// SILENT UNTIL `/api/config` HAS ANSWERED, keyed on `providers` as this
    /// line always was: a default `Config` is not servable and publishes no
    /// voices, so an unguarded version shouts "this configuration runs
    /// nothing" at every page load for the moment before the fetch lands.
    /// `kinds` is the discriminator `composing` uses and `providers` is the
    /// one here, and they agree -- both are non-empty in every real response
    /// -- so this is left as it was rather than changed under a sentence that
    /// is about something else.
    pub fn unrunnable(&self) -> Option<&'static str> {
        if self.providers.is_empty() || self.servable {
            return None;
        }
        if self.voices.is_empty() {
            return Some(
                "This configuration has no sd: block and no tts.voices, so there is \
                 nothing for a queued request to run.",
            );
        }
        Some(
            "This configuration has no sd: block, so there is no generation server to \
             start and a queued image request would have nowhere to run. Speech still \
             works.",
        )
    }

    /// Which set of controls a kind shows, and the refusal where it shows
    /// none.
    ///
    /// AN EMPTY STRING IS `sd`, which is `genstore.KindOf`'s rule read from
    /// the other side of the wire: every body this page has ever posted omits
    /// `kind`, and the server resolves the absence to an image generation. So
    /// the default state of this panel is the one that was there before
    /// speech existed, and it is that way by the same comparison the server
    /// makes rather than by a separate convention.
    ///
    /// SPEECH IS REFUSED WHERE THERE ARE NO VOICES, and the two reasons are
    /// different sentences because the remedies are. `/api/config` not having
    /// answered is a wait; a config with no `tts.voices` is an edit to
    /// `ember.yaml` and a restart. Collapsing them would tell an operator
    /// whose file is correct to go and change it, and an operator whose file
    /// is empty to keep waiting.
    ///
    /// THE UNANSWERED ARM IS THE SECOND MECHANISM AND NOT THE FIRST.
    /// `Compose`'s kind selector is built from `kinds`, which is empty until
    /// the config lands -- so there is no option for an operator to pick and
    /// the window cannot be entered through the page at all. This arm is what
    /// holds if that ever stops being true: a `?kind=` in the URL, a
    /// remembered choice, a default taken from somewhere else. Containment
    /// twice, by two mechanisms, and `cargo test` reaches this one because it
    /// is a state of this type rather than of the DOM.
    ///
    /// IT IS KEYED ON `providers` AND IT USED TO BE KEYED ON `kinds`, which is
    /// what had to move before the server could narrow that list at all. The
    /// server now sends the kinds THIS CONFIGURATION can run, so `[]` is a
    /// real answer -- "this process serves nothing" -- and a page reading it
    /// as "ember has not spoken yet" would wait for ever over a server that
    /// had already told it everything. `unrunnable` above was keyed on
    /// `providers` from the start and for exactly this reason; the two now
    /// agree, which is the state its own comment described as already true.
    ///
    /// `providers` is the right discriminator because a registry with no
    /// providers in it is not a configuration anybody can reach: the registry
    /// is built from the config and `ember serve` cannot start without one.
    /// So empty means unanswered and nothing else, which is the property
    /// `kinds` had until it stopped being a constant.
    ///
    /// A KIND THIS BUILD DOES NOT KNOW IS NAMED. The server's own `kinds` is
    /// the only source of the selector's options, so reaching it means the
    /// server published something this page has no controls for -- a newer
    /// ember behind an older page, which is a sentence and not a blank panel.
    pub fn composing(&self, kind: &str) -> Composing {
        match kind {
            "" | "sd" => Composing::Image,
            "tts" if !self.voices.is_empty() => Composing::Speech(self.voices.clone()),
            "tts" if self.providers.is_empty() => {
                Composing::Refused("ember has not said which voices it publishes yet.".into())
            }
            "tts" => Composing::Refused(
                "this ember serve publishes no voices, so it serves no speech. Add a \
                 tts.voices list to ember.yaml and restart it."
                    .into(),
            ),
            other => Composing::Refused(format!(
                "this page has no controls for a {other} request; it is older than the \
                 ember serve it is talking to."
            )),
        }
    }
}

#[derive(Clone, Debug, Default, Deserialize)]
pub struct Offer {
    pub provider: String,
    pub token: String,
    #[serde(default)]
    pub gpu: String,
    #[serde(default)]
    pub gpu_count: i32,
    #[serde(default)]
    pub vram_gib: i32,
    #[serde(default)]
    pub price: Money,
    #[serde(default)]
    pub region: String,
    #[serde(default)]
    pub interruptible: bool,
    #[serde(default)]
    pub vcpu: i32,
    #[serde(default)]
    pub label: String,
}

/// One provider's answer, with its failure beside it.
///
/// "I could not ask RunPod" must never render as "RunPod has nothing" -- the
/// same rule `ember ls` exits 2 for. A flat list of offers could not say it.
#[derive(Clone, Debug, Default, Deserialize)]
pub struct Listing {
    pub provider: String,
    #[serde(default, deserialize_with = "nullable")]
    pub offers: Vec<Offer>,
    #[serde(default)]
    pub error: String,
}

impl Listing {
    /// Which of the three things this provider's answer is.
    ///
    /// A METHOD AND NOT A CHAIN OF `if` INSIDE THE VIEW MACRO, the reason
    /// `Rows::note` and `Request::reason` are: the precedence is a decision
    /// and `cargo test` cannot reach one written as a view branch. The
    /// precedence is the whole content of the component's own comment -- "I
    /// could not reach RunPod" must never render as "RunPod has nothing" --
    /// and until this method existed the only thing asserting it was that
    /// comment.
    ///
    /// THE ERROR WINS OVER THE OFFERS, and the order matters even though
    /// `app.Offers` fills one field or the other and never both: it is the
    /// wire that decides what this page can be handed, not the one writer
    /// that exists today, and the direction a mistake has to fall is towards
    /// saying so. The same rule `Caps.PublicSSH` defaults in.
    pub fn answer(&self) -> Answer {
        if !self.error.is_empty() {
            return Answer::Failed {
                provider: self.provider.clone(),
                why: self.error.clone(),
            };
        }
        if self.offers.is_empty() {
            return Answer::Nothing {
                provider: self.provider.clone(),
            };
        }
        Answer::Rentable {
            offers: self.offers.clone(),
        }
    }
}

/// What a provider's answer says, once the precedence has been applied.
#[derive(Clone, Debug)]
pub enum Answer {
    /// The provider could not be asked. A finding, not a silence: `ember ls`
    /// exits 2 for this and the page says it in `p.error`.
    Failed { provider: String, why: String },

    /// The provider answered and has nothing matching the filter. Said in
    /// words rather than as an empty table, because an empty table is also
    /// what a page that never asked looks like.
    Nothing { provider: String },

    /// What it has, in the order the provider listed it.
    Rentable { offers: Vec<Offer> },
}

#[derive(Clone, Debug, Default, Deserialize)]
pub struct Offers {
    #[serde(default, deserialize_with = "nullable")]
    pub listings: Vec<Listing>,
}

/// KIND_UNREACHABLE is the one `reconcile.Kind` this page compares against.
///
/// A STRING FROM THE OTHER SIDE, deliberately spelled once. The kinds are
/// `reconcile.kindNames` and the page renders whichever one it is handed, so
/// this is not a copy of that table -- it is the single name the precedence
/// below turns on. The event kinds this module already matches on are the
/// precedent.
const KIND_UNREACHABLE: &str = "unreachable";

/// One line of `ember ls`, in parts.
///
/// NOT THE RENDERED LINE. `reconcile.Finding.String` is `%-14s %-28s %s` with
/// `[$x/month]` and `[$x so far]` appended, and HTML collapses runs of
/// whitespace -- so a page printing it would show a terminal's column widths
/// collapsed into single spaces, in a table that has columns of its own. The
/// wire carried that string beside these parts for the life of the type and
/// nothing read either, because nothing read this type at all.
#[derive(Clone, Debug, Default, Deserialize)]
pub struct Finding {
    #[serde(default)]
    pub kind: String,

    /// Who this is about. The ONLY thing naming an unreachable provider: it
    /// has no node, no record, no store and no leftover, so `id` is empty by
    /// construction and a row built from `id` alone would say "I could not
    /// ask" without saying whom.
    #[serde(default)]
    pub provider: String,

    #[serde(default)]
    pub id: String,
    #[serde(default)]
    pub reason: String,

    /// When ember wrote the intent record this row is about, RFC3339 in UTC,
    /// and empty where there is no record.
    ///
    /// It is the instant `accrued` below is measured from -- Go computes that
    /// figure as `Estimate(price, record.Created, now)` -- so the two belong
    /// beside each other: without it a row says "$55.764" and leaves an
    /// operator unable to tell a fast rate over an hour from a slow one over
    /// a fortnight.
    #[serde(default)]
    pub created: String,

    /// Money already spent, which stops when the node is destroyed.
    #[serde(default)]
    pub accrued: Option<Money>,

    /// Money that keeps being spent until somebody removes the store. Two
    /// fields and not one for the reason `reconcile.Finding` keeps them
    /// apart: "$2.80" skimmed off a store row and off an orphan row are two
    /// very different numbers.
    #[serde(default)]
    pub monthly: Option<Money>,
}

impl Finding {
    /// Whether this is a provider ember could not ask.
    pub fn unreachable(&self) -> bool {
        self.kind == KIND_UNREACHABLE
    }

    /// What names this row on screen.
    ///
    /// The id where there is one and the provider where there is not, which
    /// is `reconcile.Finding.String`'s own fallback -- it prints `-` for the
    /// id of an unreachable finding and the provider is in the reason. Here
    /// the provider goes in the name column instead, because a table whose
    /// name column reads `-` is a table an operator cannot scan.
    pub fn names(&self) -> String {
        if self.id.is_empty() {
            self.provider.clone()
        } else {
            self.id.clone()
        }
    }

    /// When this started, as a person reads it.
    ///
    /// THE ZONE IS NAMED AND NOT DROPPED. The server sends UTC and the
    /// operator reading this page is on the machine serving it -- `ember
    /// serve` binds loopback and nothing it serves is reachable from
    /// anywhere else -- so the two clocks are one clock in some offset. A
    /// `2026-11-10 11:26` with the `Z` quietly removed is that offset's worth
    /// of lie about when money started burning, three hours of it from
    /// Moscow, and the lie is in the safe-looking direction: it reads EARLIER
    /// than the truth, so the leak looks fresher than it is.
    ///
    /// SECONDS ARE DROPPED AND THE ZONE IS NOT, which is `ember history`'s
    /// own shape (`2006-01-02 15:04`). A finding is minutes to weeks old; the
    /// seconds on it are noise, and the zone is the part a reader acts on.
    pub fn created_text(&self) -> String {
        // ANYTHING THIS CANNOT PARSE IS SHOWN WHOLE RATHER THAN BLANKED.
        // Parse, don't grep: a `replace('T', ' ')` would happily render an
        // offset instant as a UTC one. If the server ever sends a shape this
        // does not know, the raw string is wrong-looking and readable, where
        // an empty cell is a creation time an operator concludes ember does
        // not have.
        utc_minute(&self.created).unwrap_or_else(|| self.created.clone())
    }
}

/// utc_minute reads one RFC3339 UTC instant and writes `2026-11-10 11:26 UTC`.
///
/// Every byte of the twenty is checked, which is what lets the slices below be
/// taken by index: a string that reaches `format!` here is ASCII in all twenty
/// positions, so there is no character boundary to panic on. `None` is "this
/// is not that shape", and the caller decides what to show instead -- this
/// function never guesses.
fn utc_minute(s: &str) -> Option<String> {
    let b = s.as_bytes();
    if b.len() != 20 || b[19] != b'Z' {
        return None;
    }
    for (i, want) in [(4, b'-'), (7, b'-'), (10, b'T'), (13, b':'), (16, b':')] {
        if b[i] != want {
            return None;
        }
    }
    for i in [0, 1, 2, 3, 5, 6, 8, 9, 11, 12, 14, 15, 17, 18] {
        if !b[i].is_ascii_digit() {
            return None;
        }
    }
    Some(format!("{} {} UTC", &s[..10], &s[11..16]))
}

/// The four states `queue.WorkerState` can be in, as Go spells them.
///
/// CONSTANTS AND NOT AN ENUM WITH A `Deserialize`, which is the same decision
/// [`Finding::kind`] takes one type up and for the same two reasons: a state
/// Go adds later must reach the page as a word an operator can read rather
/// than as a deserialisation failure that drops the whole envelope, and the
/// words themselves are the vocabulary the Go side prints. What these names
/// are for is the one thing the page genuinely decides -- which row is
/// painted as wrong -- and that is `Worker::needs_attention` below.
const STATE_STALE: &str = "stale";
const STATE_STARTING: &str = "starting";

/// One rental `ember serve` is holding right now.
///
/// THE FLEET IS THIS PROCESS'S BOOKKEEPING AND THE FINDINGS ARE THE
/// PROVIDERS', joined on the server by the node's handle. They overlap and
/// they are not the same set, in both directions: an orphan from a crashed run
/// is a finding with no worker, and a rental in the middle of its create call
/// is a worker with no finding -- no node exists yet, so no listing mentions
/// it, and it is money already committed.
///
/// EVERY DERIVED FIGURE ARRIVES DERIVED. `state` is `queue.Worker.State`,
/// `beat` and `up` and `remaining` are Go-formatted durations, and `accrued`
/// is a [`Money`] that carries its own rendering. Nothing here is computed
/// from a timestamp, because the only clock this page has is the BROWSER's --
/// a different clock from the one a beat was stamped by, and one an operator
/// can have set wrong.
#[derive(Clone, Debug, Default, Deserialize)]
pub struct Worker {
    /// `provider/id`, the same string `ember ls` prints and `requests.node`
    /// records. Empty while the rental is being created.
    #[serde(default)]
    pub node: String,
    #[serde(default)]
    pub provider: String,
    #[serde(default)]
    pub card: String,
    #[serde(default)]
    pub image: String,

    /// The request that paid for this rental to exist, and the one generating
    /// on it now. At `max_nodes` > 1 they are usually different, and the
    /// second is empty in the gap between two batches.
    #[serde(default)]
    pub opener: String,
    #[serde(default)]
    pub request: String,

    /// `starting`, `live`, `stale` or `quiet`.
    #[serde(default)]
    pub state: String,

    /// How long ago ember last heard this rental's worker. Empty when it never
    /// has, which is NOT "a moment ago" and must not render as it.
    #[serde(default)]
    pub beat: String,

    /// How long ember has been holding this rental.
    #[serde(default)]
    pub up: String,

    /// What the node has cost so far and how long until its deadline, both
    /// from the sweep. Absent together when the sweep found no node for this
    /// handle -- a rental mid-create -- rather than zero, because a `$0.00`
    /// against a card about to be billed for is ember pricing a rental at
    /// nothing.
    #[serde(default)]
    pub accrued: Option<Money>,
    #[serde(default)]
    pub remaining: Option<String>,
}

impl Worker {
    /// What names this rental on screen: the machine where there is one, and
    /// what it is becoming where there is not.
    ///
    /// A ROW WITH A BLANK NAME COLUMN IS A ROW AN OPERATOR CANNOT SCAN, which
    /// is [`Finding::names`]'s own reason for falling back to the provider.
    /// Here the fallback says what is happening instead, because "the create
    /// call has not returned" is the whole content of that row.
    pub fn names(&self) -> String {
        if self.node.is_empty() {
            format!("{} (being created)", self.provider)
        } else {
            self.node.clone()
        }
    }

    /// Whether this row is one a person has to look at.
    ///
    /// ONLY `stale`, and the three exclusions are each a decision. `live` is
    /// working. `starting` is a create call in flight, which is money
    /// committed and nothing wrong -- a red row there would be red on every
    /// rental this server ever opens, for its first minute. `quiet` is a
    /// speech rental, which has no worker to beat because piper is run over
    /// `ssh exec`: there being nobody to answer is a different fact from not
    /// answering, and painting it red would teach an operator to ignore the
    /// colour -- which is `reconcile.KindStore`'s argument about exit 2,
    /// reached through a stylesheet.
    ///
    /// NOT FOLDED INTO `Nodes::attention`, which is `reconcile`'s count of
    /// what `ember ls` exits 2 for and must stay exactly that: a wedged
    /// worker is this process's own observation and `ember ls` has never seen
    /// one. Two numbers for two questions, rather than one number that means
    /// something different depending on who is running.
    pub fn needs_attention(&self) -> bool {
        self.state == STATE_STALE
    }

    /// What this row says is wrong with it, or nothing.
    ///
    /// THE SENTENCE IS HERE AND NOT IN THE VIEW, for [`Nodes::verdict`]'s
    /// reason: prose chosen by a branch inside a view macro can only be
    /// checked by a browser. And it is a SENTENCE rather than the word
    /// `stale`, because the word alone is read as a cache that needs
    /// refreshing -- the opposite of what it means here, which is that the
    /// card is billing and nobody on it is answering.
    pub fn note(&self) -> String {
        if self.state == STATE_STALE {
            return "not answering, and still billing".into();
        }
        if self.state == STATE_STARTING {
            return "the create call has not returned".into();
        }
        String::new()
    }
}

/// What one sweep of every provider found: `GET /api/nodes`.
///
/// THE SAME ANSWER `ember ls` PRINTS, projected from `internal/reconcile` --
/// never a second scan with rules of its own. For the life of this route there
/// was no Rust type for it at all, and the route answered 501 in every shipped
/// `ember serve`, so an operator in front of the browser had no path to the one
/// charge no deadline, no reaper and no Ctrl-C ever ends.
///
/// THREE SLICES AND NOT ONE, because they are acted on differently: `ember
/// reap` destroys an expired node, removes a leftover through another
/// interface entirely, and must never be handed a store.
///
/// NO PER-ROW TOLERANCE HERE, which is a decision and not an omission.
/// [`Rows`] drops and counts a request it cannot read because that envelope is
/// the one where a single bad row costs the view of every good one and the one
/// with somewhere to say so. This envelope has an `error` of its own on the
/// way in -- a 502 reaches the panel as `api::reason`'s sentence -- and its
/// numbers are a count and two money figures rather than ten `i32`s a request
/// can set, so the eleventh-field hazard that rule was written for is not
/// here.
#[derive(Clone, Debug, Default, Deserialize)]
pub struct Nodes {
    #[serde(default, deserialize_with = "nullable")]
    pub findings: Vec<Finding>,
    #[serde(default, deserialize_with = "nullable")]
    pub leftovers: Vec<Finding>,
    #[serde(default, deserialize_with = "nullable")]
    pub stores: Vec<Finding>,

    /// What the stores cost every month, totalled by `reconcile.MonthlyTotal`.
    #[serde(default)]
    pub monthly_storage: Money,

    /// How many findings need a person: the number `ember ls` exits 2 for.
    ///
    /// `i64` AND NOT `i32`, because the Go field is an `int` and this page has
    /// already lost a whole table to a number the server could say and the
    /// client could not hear. The count cannot realistically exceed an `i32`
    /// -- it is bounded by what the providers listed -- and "realistically"
    /// is exactly the word that was wrong last time.
    ///
    /// READ AND NEVER DERIVED, although every kind needed to derive it is on
    /// the wire. `reconcile.Kind.NeedsAttention` is a switch written so that
    /// a kind added later needs attention by default; a second copy of it
    /// here is a copy whose author will not be reading the Go one, which is
    /// why `Money` crosses this wire twice as well.
    #[serde(default)]
    pub attention: i64,

    /// The rentals `ember serve` is holding, sorted by the request that
    /// opened each.
    ///
    /// A FOURTH SLICE AND NOT A FLAG ON A FINDING. The three above are what
    /// EXISTS at the providers; this is what THIS PROCESS is doing with it,
    /// and the rows that differ are the ones worth seeing -- a rental whose
    /// create has not returned has no finding at all.
    #[serde(default, deserialize_with = "nullable")]
    pub workers: Vec<Worker>,
}

impl Nodes {
    /// The providers ember could not ask, by name and without repeats.
    ///
    /// ALL THREE SLICES, because a provider can fail one listing and answer
    /// another: `reconcile.ScanLeftovers` says so in as many words, and
    /// `ScanStores` emits the same kind. A version of this that looked only
    /// at `findings` would report a complete picture over a store listing
    /// that never happened.
    pub fn unreachable(&self) -> Vec<String> {
        let mut out: Vec<String> = Vec::new();
        for f in self
            .findings
            .iter()
            .chain(self.leftovers.iter())
            .chain(self.stores.iter())
        {
            if f.unreachable() && !out.iter().any(|p| p == &f.provider) {
                out.push(f.provider.clone());
            }
        }
        out
    }

    /// Whether "nothing is running and nothing was left behind" is true,
    /// which is NOT the same as the two tables being empty.
    ///
    /// The third clause is the one that matters and it is not redundant: a
    /// store listing that failed puts its `unreachable` finding in `stores`,
    /// so both of the first two can be empty over a sweep that does not know
    /// what is running. "I could not check RunPod" must never render as a
    /// clean bill of health -- the rule `ember ls` exits non-zero for -- and
    /// without that clause this would print the clean sentence over it.
    pub fn nothing_running(&self) -> bool {
        self.findings.is_empty() && self.leftovers.is_empty() && self.unreachable().is_empty()
    }

    /// The sentence at the top of the panel.
    ///
    /// A METHOD AND NOT A CHAIN OF `if` INSIDE THE VIEW MACRO, which is
    /// [`Listing::answer`]'s reason and [`Rows::note`]'s: the precedence is a
    /// decision, and one written as a view branch can only be checked by a
    /// browser.
    ///
    /// THE UNREACHABLE CLAUSE IS ADDITIVE AND NOT AN ALTERNATIVE. An expired
    /// node here and an unreachable provider there are two facts and the
    /// operator needs both -- so this is not an enum: a `Partial` arm would
    /// have hidden the count, and a count alone would have read as the whole
    /// picture.
    /// How many rentals this process is holding that a person has to look at.
    ///
    /// SEPARATE FROM `attention`, which is reconcile's count of what
    /// `ember ls` exits 2 for. A worker that has stopped answering is this
    /// process's own observation and no `ember ls` has ever seen one, so
    /// adding it in would change what that number means depending on whether
    /// an `ember serve` happened to be running.
    pub fn wedged(&self) -> usize {
        self.workers.iter().filter(|w| w.needs_attention()).count()
    }

    pub fn verdict(&self) -> String {
        let blind = self.unreachable();
        if self.attention == 0 && self.nothing_running() {
            return "nothing is running and nothing was left behind".into();
        }
        let mut s = if self.attention == 0 {
            // Reachable: a sweep whose only findings are `ok` nodes and
            // stores. `ember ls` prints those rows and exits 0.
            "nothing needs attention".to_string()
        } else {
            format!("{} finding(s) need attention", self.attention)
        };
        if !blind.is_empty() {
            s += &format!(
                "; ember could not ask {}, so this is not the whole picture",
                blind.join(", ")
            );
        }
        s
    }
}

/// One image to generate. The same shape ember accepts.
///
/// There is no `extra`, and the absence is the point: that field reaches
/// everything in sd-server's request body ember does not type, including the
/// ones naming file paths on the node. The server has nowhere to put one
/// either, so a UI that tried to send one would be refused as an unknown
/// field.
#[derive(Clone, Debug, Default, PartialEq, Deserialize, Serialize)]
pub struct Job {
    pub prompt: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub negative_prompt: String,
    #[serde(default, skip_serializing_if = "is_zero_i32")]
    pub width: i32,
    #[serde(default, skip_serializing_if = "is_zero_i32")]
    pub height: i32,
    #[serde(default, skip_serializing_if = "is_zero_i32")]
    pub steps: i32,
    #[serde(default, skip_serializing_if = "is_zero_i32")]
    pub batch_count: i32,
    #[serde(default, skip_serializing_if = "is_zero_i64")]
    pub seed: i64,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub sampler: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub scheduler: String,
    #[serde(default, skip_serializing_if = "is_zero_f64")]
    pub cfg: f64,
    #[serde(default, skip_serializing_if = "is_zero_f64")]
    pub guidance: f64,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub output: String,

    /// The optional parameters.
    ///
    /// `Option` where the server's default is not zero, because 0 is a real
    /// value for all three: `clip_skip: 0` and `eta: 0` are things a model is
    /// told, and the server's own defaults are -1 and null. A plain i32 here
    /// would make "leave it alone" and "set it to zero" the same request.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub clip_skip: Option<i32>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub eta: Option<f64>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub flow_shift: Option<f64>,

    #[serde(default, skip_serializing_if = "is_zero_i32")]
    pub shifted_timestep: i32,

    /// The staged image this job generates from, and how much of it survives.
    ///
    /// `init_image` IS A DIGEST AND NOT A FILENAME: the string
    /// `POST /api/inputs` answered with, which is the sha256 of the bytes.
    /// The name of the file the operator picked is never sent anywhere -- the
    /// server derives the key from the content, so there is nothing for a
    /// filename to reach.
    ///
    /// `strength` is an `Option` because the server's own default is 0.75 and
    /// 0 is a real value it echoes back, so an empty box and a 0 are
    /// different requests. It is sent ONLY with an image: ember refuses the
    /// combination, because a live server accepts a strength with no image,
    /// generates, and reports the strength nowhere -- which reads as working.
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub init_image: String,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub strength: Option<f64>,

    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub output_format: String,
    #[serde(default, skip_serializing_if = "is_zero_i32")]
    pub output_compression: i32,

    /// The nested blocks, sent only when they hold something.
    ///
    /// `Option` and not a plain struct so that a job with none of this set
    /// posts the body it used to post. That is not tidiness: the panel builds
    /// one draft and clones it per job, and three empty objects on every
    /// request would be three objects the server has to be asked about.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub slg: Option<Slg>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub hires: Option<Hires>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub vae_tiling: Option<VaeTiling>,

    /// The utterance, for a request whose kind is `tts`, and absent for every
    /// image generation.
    ///
    /// ON THE SAME TYPE AS THE IMAGE FIELDS, which is what makes one batch
    /// hold either kind -- one `Vec<Job>`, one "add to batch" button, one
    /// list on screen. A second draft type would have meant a second batch,
    /// a second list and a second button, for a shape the server already
    /// spells this way: `newJob.TTS` is a nested object on the image job for
    /// exactly this reason.
    ///
    /// AND THE SERVER REFUSES THE MIX, which is why a speech job is built
    /// from `Job::default()` rather than from whatever the image draft held:
    /// `crossKind` compares a speech job's remaining fields against the zero
    /// value and refuses the whole request if any is set, naming the index.
    /// Every image field above is `skip_serializing_if` a zero, so a default
    /// job carrying only this one posts `{"prompt":"","tts":{...}}` -- and
    /// `prompt` is the zero value Go compares against, so it is not "a field
    /// belonging to the other kind".
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub tts: Option<Utterance>,
}

/// One utterance to speak. The same shape ember accepts.
///
/// WHAT IT CANNOT NAME IS THE POINT, and it is shorter to say than what it
/// can: there is no voice path, no espeak directory, no binary, no argv and no
/// `extra`. The only way a request chooses a model is `voice_id`, which names
/// an entry in a list the operator published -- so every seal in `node`,
/// `exposure` and `loopback` stays upstream of anything this page can say,
/// exactly as it does for an image.
///
/// THE FIVE PIPER PARAMETERS ARE NOT HERE, and that is an absence rather than
/// an omission: `speaker`, `length_scale`, `noise_scale`, `noise_w` and
/// `sentence_silence` are on the server's wire type as pointers, and none of
/// them has been measured from this page. A control over an unmeasured value
/// set is the `allowedCudaVersions` mistake, and a slider whose effect nobody
/// has heard is the "reads as working" one. They stay reachable from
/// `ember.yaml`.
#[derive(Clone, Debug, Default, PartialEq, Deserialize, Serialize)]
pub struct Utterance {
    /// The operator's content, and it travels exactly as a prompt does: in
    /// the request body and never on a command line.
    pub text: String,

    /// An id from `/api/config`'s `voices`, and never a path.
    pub voice_id: String,

    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub output: String,
}

impl Utterance {
    /// Why this cannot be sent, where it cannot.
    ///
    /// THE VOICE HALF IS THE ONE WORTH HAVING. An empty `voice_id` is what a
    /// `<select>` with no options posts, and the server's refusal for it
    /// arrives after the operator has typed their text -- so a chooser that
    /// quietly sent a blank id would be a control that reads as working. The
    /// panel also defaults the chooser to the first published voice, which
    /// means nothing here should be reachable through the page: two
    /// mechanisms, and this is the one `cargo test` can hold.
    ///
    /// TRIMMED, because a text area holding a newline is not something to
    /// rent a machine for -- and piper refuses it, which is the same answer
    /// reached after a card.
    pub fn refusal(&self) -> Option<String> {
        if self.text.trim().is_empty() {
            return Some("nothing to say: type some text".into());
        }
        if self.voice_id.is_empty() {
            return Some(
                "no voice chosen: pick one from the list ember published, or add a \
                 tts.voices entry to ember.yaml"
                    .into(),
            );
        }
        None
    }
}

/// One parameter of a past generation, as a page shows it.
///
/// A PAIR AND NOT A FORMATTED LINE, so the view decides the markup and this
/// decides the wording -- which is the settled shape here (`Rows::note`,
/// `Request::reason`, `Stream::note`): a decision is a method with a
/// `cargo test`, never a chain of `if` inside a view macro, because nothing
/// but a browser can read the second one.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Setting {
    pub label: &'static str,
    pub value: String,
}

fn set(label: &'static str, value: impl Into<String>) -> Setting {
    Setting {
        label,
        value: value.into(),
    }
}

/// What a page says for a parameter nobody set.
///
/// ABSENT IS NOT ZERO, and the four `Option` fields above exist for exactly
/// that: the server's own defaults are -1 for `clip_skip`, null for `eta`
/// and `flow_shift` and 0.75 for `strength`, so printing `0` for an unset
/// one states a value nobody chose -- in the one place an operator reads to
/// find out what a generation actually ran with.
pub const UNSET: &str = "unset";

/// What a page says for a block the request did not carry.
///
/// `slg`, `hires` and `vae_tiling` are sent only when they hold something,
/// so their ABSENCE IS A FACT about the run rather than a gap in it: the
/// highres pass was off. That is why it is a word and not a dash -- a dash
/// reads as "ember does not know", which is what `UNSET` means one line up,
/// and the two must not be spelled the same.
pub const OFF: &str = "off";

impl Job {
    /// Every parameter this job ran with, in the order a reproduction needs
    /// them.
    ///
    /// SEED AND SAMPLER FIRST, because they are what a reproduction turns
    /// on: the rest changes an image and those two decide whether the same
    /// request produces the same pixels at all. A seed of zero is not a
    /// seed -- `omitempty` means the browser sent none and the server chose
    /// one -- so it is reported as not pinned rather than as `0`, which is
    /// the same distinction `UNSET` draws for the pointers and the reason a
    /// reproduction of such a request is not available.
    ///
    /// THE ROWS A JOB DID NOT CARRY ARE OMITTED AND THE ONES IT COULD NOT
    /// HAVE ARE NOT ASKED FOR. `strength` does nothing without an image and
    /// ember refuses the combination, so a text-to-image job gets no
    /// strength row at all -- an `unset` there would be a question that
    /// cannot be answered rather than one that was not.
    pub fn settings(&self) -> Vec<Setting> {
        let mut out = Vec::new();

        out.push(set(
            "seed",
            if self.seed == 0 {
                "— not pinned, the server chose one".to_string()
            } else {
                self.seed.to_string()
            },
        ));
        out.push(set("sampler", dashed(&self.sampler)));
        // DECODED SINCE THIS TYPE WAS WRITTEN AND READ NOWHERE -- not even
        // by the compose form, which has no control for it. Shown because a
        // scheduler changes the pixels and a page that omits it leaves an
        // operator unable to tell which of the two silences they are in.
        out.push(set("scheduler", dashed(&self.scheduler)));

        if self.width != 0 && self.height != 0 {
            out.push(set("size", format!("{}x{}", self.width, self.height)));
        }
        if self.steps != 0 {
            out.push(set("steps", self.steps.to_string()));
        }
        if self.batch_count != 0 {
            out.push(set("batch", self.batch_count.to_string()));
        }
        if self.cfg != 0.0 {
            out.push(set("cfg", self.cfg.to_string()));
        }
        if self.guidance != 0.0 {
            out.push(set("guidance", self.guidance.to_string()));
        }

        out.push(set("clip skip", opt(self.clip_skip)));
        out.push(set("eta", opt(self.eta)));
        out.push(set("flow shift", opt(self.flow_shift)));

        if self.shifted_timestep != 0 {
            out.push(set("shifted timestep", self.shifted_timestep.to_string()));
        }
        if !self.init_image.is_empty() {
            // THE DIGEST AND NOT A FILENAME: no filename was ever sent, so
            // there is nothing else this row could say, and the digest is
            // what another request would have to name to run again.
            out.push(set("init image", self.init_image.clone()));
            out.push(set("strength", opt(self.strength)));
        }
        if !self.output_format.is_empty() {
            out.push(set("output format", self.output_format.clone()));
        }
        if self.output_compression != 0 {
            out.push(set(
                "output compression",
                self.output_compression.to_string(),
            ));
        }

        out.push(set(
            "skip-layer guidance",
            match &self.slg {
                None => OFF.to_string(),
                Some(s) => format!(
                    "scale {}, layers {}, {}–{}",
                    s.scale,
                    joined(&s.layers),
                    s.layer_start,
                    s.layer_end
                ),
            },
        ));
        out.push(set(
            "highres pass",
            match &self.hires {
                None => OFF.to_string(),
                Some(h) if !h.enabled => OFF.to_string(),
                Some(h) => {
                    let mut p = Vec::new();
                    if !h.upscaler.is_empty() {
                        p.push(h.upscaler.clone());
                    }
                    if h.scale != 0.0 {
                        p.push(format!("x{}", h.scale));
                    }
                    if h.target_width != 0 && h.target_height != 0 {
                        p.push(format!("{}x{}", h.target_width, h.target_height));
                    }
                    if h.steps != 0 {
                        p.push(format!("{} steps", h.steps));
                    }
                    if h.denoising_strength != 0.0 {
                        p.push(format!("denoising {}", h.denoising_strength));
                    }
                    if p.is_empty() {
                        "on".to_string()
                    } else {
                        p.join(", ")
                    }
                }
            },
        ));
        out.push(set(
            "vae tiling",
            match &self.vae_tiling {
                None => OFF.to_string(),
                Some(t) if !t.enabled => OFF.to_string(),
                Some(t) => {
                    let mut p = Vec::new();
                    if t.tile_size_w != 0 && t.tile_size_h != 0 {
                        p.push(format!("tile {}x{}", t.tile_size_w, t.tile_size_h));
                    }
                    if t.target_overlap != 0.0 {
                        p.push(format!("overlap {}", t.target_overlap));
                    }
                    if t.rel_size_w != 0.0 && t.rel_size_h != 0.0 {
                        p.push(format!("relative {}x{}", t.rel_size_w, t.rel_size_h));
                    }
                    if p.is_empty() {
                        "on".to_string()
                    } else {
                        p.join(", ")
                    }
                }
            },
        ));

        out
    }
}

fn dashed(s: &str) -> String {
    if s.is_empty() {
        "—".to_string()
    } else {
        s.to_string()
    }
}

fn opt<T: std::fmt::Display>(v: Option<T>) -> String {
    match v {
        None => UNSET.to_string(),
        Some(v) => v.to_string(),
    }
}

fn joined(v: &[i32]) -> String {
    v.iter().map(i32::to_string).collect::<Vec<_>>().join(", ")
}

/// Skip-layer guidance.
///
/// `scale` is the switch: ember sends no block at all when it is zero, which
/// is upstream's own "off", so layers with no scale configure nothing.
#[derive(Clone, Debug, Default, PartialEq, Deserialize, Serialize)]
pub struct Slg {
    #[serde(
        default,
        deserialize_with = "nullable",
        skip_serializing_if = "Vec::is_empty"
    )]
    pub layers: Vec<i32>,
    #[serde(default, skip_serializing_if = "is_zero_f64")]
    pub layer_start: f64,
    #[serde(default, skip_serializing_if = "is_zero_f64")]
    pub layer_end: f64,
    #[serde(default, skip_serializing_if = "is_zero_f64")]
    pub scale: f64,
}

/// The second, higher-resolution pass.
///
/// `enabled` gates the block, because the server fills its own defaults in:
/// enabled alone is a complete request for upstream's scale and denoising.
#[derive(Clone, Debug, Default, PartialEq, Deserialize, Serialize)]
pub struct Hires {
    #[serde(default)]
    pub enabled: bool,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub upscaler: String,
    #[serde(default, skip_serializing_if = "is_zero_f64")]
    pub scale: f64,
    #[serde(default, skip_serializing_if = "is_zero_i32")]
    pub target_width: i32,
    #[serde(default, skip_serializing_if = "is_zero_i32")]
    pub target_height: i32,
    #[serde(default, skip_serializing_if = "is_zero_i32")]
    pub steps: i32,
    #[serde(default, skip_serializing_if = "is_zero_f64")]
    pub denoising_strength: f64,
    #[serde(default, skip_serializing_if = "is_zero_i32")]
    pub upscale_tile_size: i32,
}

/// Decoding the latent in tiles, which is how a large image fits in VRAM that
/// could not hold it whole.
#[derive(Clone, Debug, Default, PartialEq, Deserialize, Serialize)]
pub struct VaeTiling {
    #[serde(default)]
    pub enabled: bool,
    #[serde(default, skip_serializing_if = "is_zero_i32")]
    pub tile_size_w: i32,
    #[serde(default, skip_serializing_if = "is_zero_i32")]
    pub tile_size_h: i32,
    #[serde(default, skip_serializing_if = "is_zero_f64")]
    pub target_overlap: f64,
    #[serde(default, skip_serializing_if = "is_zero_f64")]
    pub rel_size_w: f64,
    #[serde(default, skip_serializing_if = "is_zero_f64")]
    pub rel_size_h: f64,
}

fn is_zero_i32(v: &i32) -> bool {
    *v == 0
}
fn is_zero_i64(v: &i64) -> bool {
    *v == 0
}
fn is_zero_f64(v: &f64) -> bool {
    *v == 0.0
}

impl Job {
    /// Removes every parameter the server has said no to.
    ///
    /// IT EXISTS BECAUSE GREYING A CONTROL IS NOT THE SAME AS NOT SENDING ONE.
    /// `/api/config` is fetched after the page mounts, so the compose panel is
    /// on screen and typeable before ember knows what the server takes -- and
    /// a job added to the batch in that window keeps whatever was typed. The
    /// control greys out when the answer arrives and the value stays in the
    /// draft, so the request would carry a parameter whose control is visibly
    /// unavailable: exactly the disagreement between what is on screen and
    /// what is on the wire that each block's switch exists to prevent.
    ///
    /// WHAT IS CLEARED IS WHAT WOULD BE SENT, which is the same set
    /// `sdserver.unsupported` refuses: a zero `shifted_timestep` and a `None`
    /// block are already absent from the body, and for skip-layer guidance the
    /// scale is the switch. Clearing more than that would be clearing nothing.
    pub fn drop_denied(&mut self, f: &Features) {
        if denied(f.init_image) {
            // The strength goes with it, and that pairing is why this is one
            // branch rather than two. ember refuses a strength with no
            // image, so clearing the image alone would turn a job the server
            // merely cannot do into a job ember will not accept -- a 400 at
            // enqueue in place of a greyed control.
            self.init_image = String::new();
            self.strength = None;
        }
        if denied(f.clip_skip) {
            self.clip_skip = None;
        }
        if denied(f.flow_shift) {
            self.flow_shift = None;
        }
        if denied(f.shifted_timestep) {
            self.shifted_timestep = 0;
        }
        if denied(f.slg) {
            self.slg = None;
        }
        if denied(f.vae_tiling) {
            self.vae_tiling = None;
        }
        if denied(f.hires) {
            self.hires = None;
        }
    }

    /// Whether this job's own parameters reached the page.
    ///
    /// FALSE FOR EVERY SPEECH JOB TODAY, and that is a gap in the server's
    /// projection rather than a property of speech: `viewRequest` builds a
    /// job's outbound shape from `newJob`, which has no `tts` field, and the
    /// request view carries no `kind` either -- so a finished utterance
    /// arrives here as one job with every field at its zero. `settings`
    /// pushes `seed`, `sampler`, `scheduler`, `clip skip`, `eta`,
    /// `flow shift` and `skip-layer guidance` unconditionally, so what the
    /// page would otherwise render is seven rows of dashes under an empty
    /// prompt: ember having lost the request, as far as anybody reading it
    /// can tell.
    ///
    /// DERIVED FROM THE TYPE AND NOT FROM A LIST OF FIELD NAMES, which is
    /// `newJob.sdFieldsSet`'s rule one language over: the field that would be
    /// forgotten is whichever is added next. It also stops applying the
    /// moment the projection sends an utterance, because a job carrying one
    /// is no longer the default -- which is the right coupling, since whoever
    /// adds that field has to add its rows.
    ///
    /// THE TEST IS "DID ANYTHING ARRIVE" AND NOT "IS THIS SPEECH", because
    /// the page cannot tell: the kind is not on this wire either. An
    /// all-default job is one the server said nothing about, and an image
    /// generation cannot be one -- enqueue refuses a job with no prompt -- so
    /// the two states are one state from here, and the honest sentence covers
    /// both.
    pub fn has_parameters(&self) -> bool {
        *self != Job::default()
    }

    /// One line identifying this job, the way ember's own output does.
    ///
    /// Deliberately the same shape as `sdserver.Job.Describe` in Go: a prompt
    /// in quotes, truncated, then the geometry only when both sides are set --
    /// because the other would come from the server's default and "512x" would
    /// be a guess printed as a fact.
    pub fn describe(&self) -> String {
        // THE UTTERANCE AND NOT THE PROMPT, where there is one. A speech job
        // is built from `Job::default()`, so every field below it is zero and
        // the image spelling would describe the whole batch as `""` -- a list
        // of identical rows with a remove button each, which is the one thing
        // a batch display must not be. Quoted and truncated the same way,
        // because it is the same kind of thing: the operator's own text.
        if let Some(u) = &self.tts {
            let mut t = u.text.clone();
            if t.chars().count() > 60 {
                t = t.chars().take(57).collect::<String>() + "...";
            }
            let mut out = format!("{t:?}");
            if !u.voice_id.is_empty() {
                out += &format!(" as {}", u.voice_id);
            }
            return out;
        }
        let mut p = self.prompt.clone();
        if p.chars().count() > 60 {
            p = p.chars().take(57).collect::<String>() + "...";
        }
        let mut out = format!("{p:?}");
        if self.width != 0 && self.height != 0 {
            out += &format!(" {}x{}", self.width, self.height);
        }
        if self.steps != 0 {
            out += &format!(" {} steps", self.steps);
        }
        if self.seed != 0 {
            out += &format!(" seed {}", self.seed);
        }
        out
    }
}

/// Input is what `POST /api/inputs` answered with.
///
/// `id` is the digest a job carries. The geometry and the format are here
/// because the upload is the only moment they are free -- the server has just
/// read the header -- and a file picker that cannot say "that is a 1024x768
/// jpeg" gives no way to tell a staged file from a refused one.
#[derive(Clone, Debug, Default, PartialEq, Deserialize)]
pub struct Input {
    pub id: String,
    #[serde(default)]
    pub format: String,
    #[serde(default)]
    pub width: i32,
    #[serde(default)]
    pub height: i32,
    #[serde(default)]
    pub bytes: i64,
}

impl Input {
    /// One line naming what was staged, for the picker to show.
    pub fn describe(&self) -> String {
        format!(
            "{} {}x{}, {}",
            self.format,
            self.width,
            self.height,
            human_bytes(self.bytes)
        )
    }
}

/// human_bytes is a size an operator reads.
///
/// Kibibytes and mebibytes, because that is what the size bound is written in
/// -- `ember serve` refuses an upload over 8 MiB, and a picker reporting
/// "8.4 MB" beside a bound of "8 MiB" would be two units for one number.
pub fn human_bytes(n: i64) -> String {
    const KIB: i64 = 1024;
    const MIB: i64 = 1024 * KIB;
    match n {
        n if n >= MIB => format!("{:.1} MiB", n as f64 / MIB as f64),
        n if n >= KIB => format!("{:.0} KiB", n as f64 / KIB as f64),
        n => format!("{n} B"),
    }
}

/// What a POST sends.
///
/// `ttl` and the budgets are strings. A JSON number is a float on both sides
/// of this wire, and in Go `ParseFloat("0.29")*1e6` is 289999 -- which is the
/// whole reason `node.ParseUSD` is hand-written. A ceiling that changed in
/// transit is a ceiling nobody trusts.
#[derive(Clone, Debug, Default, Serialize)]
pub struct NewRequest {
    /// Which workload this request is for.
    ///
    /// ON THE REQUEST AND NOT ON A JOB, which is the server's shape and makes
    /// a mixed batch unrepresentable rather than refused: one rental runs one
    /// driver. Omitted when empty, because every body this page posted before
    /// speech existed omitted it and the server resolves the absence to `sd`
    /// -- so an image request on the wire is byte-for-byte what it was.
    #[serde(skip_serializing_if = "String::is_empty")]
    pub kind: String,

    pub provider: String,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub gpu: String,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub ttl: String,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub max_hourly_usd: String,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub max_run_usd: String,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub preview: String,
    #[serde(skip_serializing_if = "is_zero_i32")]
    pub preview_interval: i32,
    pub jobs: Vec<Job>,
}

#[derive(Clone, Debug, Default, Deserialize)]
pub struct ImageRef {
    #[serde(default)]
    pub job: i32,
    #[serde(default)]
    pub index: i32,
    #[serde(default)]
    pub bytes: i64,
    #[serde(default)]
    pub path: String,

    /// What fetching `path` will declare these bytes to be.
    ///
    /// The server derives it from the blob key with the same closed switch the
    /// route uses, so this and the `Content-Type` a browser receives are one
    /// derivation -- see `webapi.imageRef.Media`. Empty where ember has no
    /// answer, which is a row the route itself 500s for.
    #[serde(default)]
    pub media: String,

    #[serde(default)]
    pub preview: bool,
}

/// Which element an artifact is rendered with.
///
/// DECIDED FROM THE ARTIFACT AND NOT FROM THE REQUEST'S KIND, and the reason
/// is that a kind is a property of the request while an element is a property
/// of the file. They agree today, because a speech rental writes wavs and an
/// image rental writes pngs; they stop agreeing the first time one kind writes
/// two sorts of artifact -- a spectrogram beside a wav, a json sidecar -- and
/// at that point a page switching on the kind renders every artifact as
/// whichever one the kind implies. The server already answers this question
/// once, for the `Content-Type` it is about to send, so reading that answer is
/// the only spelling in which the element and the bytes cannot disagree.
///
/// THE TOP-LEVEL TYPE AND NOT THE WHOLE STRING, so a second image format --
/// `image/jpeg`, which `output_format` can already ask sd-server for -- needs
/// no change here. What it costs is nothing: `<img>` and `<audio>` are chosen
/// by what the browser does with the bytes, and the browser decides that from
/// the full type it is sent rather than from this.
#[derive(Clone, Debug, PartialEq, Eq)]
pub enum Artifact {
    /// Something to look at.
    Image,

    /// Something to play.
    Audio,

    /// Bytes ember will not name, carrying the type it sent.
    ///
    /// NOT RENDERED AS AN IMAGE, which is the whole reason this arm exists
    /// rather than a fallback to `Image`. An `<img>` whose fetch failed and an
    /// `<audio>` whose source 404s both render perfectly and show nothing, so
    /// guessing here produces a gallery that looks loaded and is empty -- the
    /// "reads as working" defect in one element. The route answers 500 for an
    /// artifact with no declared type, so this is a defect ember is already
    /// refusing to serve, and saying so is the only honest render.
    Unknown(String),
}

/// subtype is the half of a media type a person recognises.
///
/// `audio/wav` is "wav", which is what a download link should say -- and
/// saying it from the declared type rather than from a literal is the same
/// rule the element itself follows: ember writes one audio format today, so a
/// hard-coded "wav" is right and goes on being printed over the first
/// artifact that is not one. A type with no slash answers itself, which is
/// what the empty case and a malformed one both come to.
pub fn subtype(media: &str) -> String {
    media.rsplit('/').next().unwrap_or(media).to_string()
}

impl ImageRef {
    /// Which element this artifact wants.
    pub fn artifact(&self) -> Artifact {
        // Split on the slash rather than matching whole strings, so a
        // parameter the server one day appends -- `audio/wav; rate=22050` --
        // does not fall through to Unknown.
        match self.media.split('/').next().unwrap_or_default() {
            "image" => Artifact::Image,
            "audio" => Artifact::Audio,
            _ => Artifact::Unknown(self.media.clone()),
        }
    }
}

#[derive(Clone, Debug, Default, Deserialize)]
pub struct Request {
    pub id: String,
    #[serde(default)]
    pub status: String,
    #[serde(default)]
    pub created: String,
    #[serde(default)]
    pub provider: String,
    #[serde(default)]
    pub gpu: String,
    #[serde(default)]
    pub offer: String,
    #[serde(default)]
    pub node: String,
    #[serde(default)]
    pub ttl: String,
    #[serde(default)]
    pub started: String,
    #[serde(default)]
    pub ended: String,
    #[serde(default)]
    pub cost: Money,
    #[serde(default)]
    pub error: String,
    #[serde(default)]
    pub preview: String,
    #[serde(default)]
    pub preview_interval: i32,
    #[serde(default, deserialize_with = "nullable")]
    pub jobs: Vec<Job>,
    #[serde(default, deserialize_with = "nullable")]
    pub images: Vec<ImageRef>,
}

impl Request {
    /// Whether anything more will happen to it.
    pub fn is_terminal(&self) -> bool {
        matches!(
            self.status.as_str(),
            "done" | "failed" | "cancelled" | "interrupted"
        )
    }

    /// Whether the cancel button does anything.
    ///
    /// Queued OR generating, and the second half is the correction. The
    /// button used to be absent for a running request because cancelling one
    /// was believed to mean destroying its node and taking every other
    /// request on the same rental with it. That is true of cancelling a
    /// rental and was never true of cancelling a job: sd-server stops one
    /// generation and the process, the node and the resident weights carry
    /// on. So the condition is "anything that has not ended" -- which is
    /// `!is_terminal()`, written as the negation rather than as a list of the
    /// two live statuses, because a status added later is one the server will
    /// answer about and a list here would silently leave it unstoppable.
    pub fn is_cancellable(&self) -> bool {
        !self.is_terminal()
    }

    /// The results, without the progress frames.
    ///
    /// A `proj` preview of a 64x64 generation is an 8x8 PNG of about 250
    /// bytes: it is the latent, not a thumbnail of the output, so a gallery
    /// must not open on one.
    pub fn results(&self) -> impl Iterator<Item = &ImageRef> {
        self.images.iter().filter(|i| !i.preview)
    }

    /// Why it ended, when the row carries a reason.
    ///
    /// THE FIELD WAS DECODED BY NOBODY FOR THE LIFE OF THE PAGE. Go populates
    /// it on every request view, on both JSON routes and the socket, from
    /// `requests.err` -- which `queue.fail` writes for a request refused
    /// before anything was rented and `queue.finish` writes for one that
    /// broke on a card. Deleting `Request::error` from this type left the
    /// crate compiling and all 28 tests green, because serde drops a field
    /// the server sends without a word. So a request stopped by
    /// `max_month_usd`, or by a sampler the cached capabilities do not list,
    /// showed the word `failed` in one table cell and nothing else -- while
    /// AGENTS.md claimed in prose that "a refused request carries the reason,
    /// which the page shows".
    ///
    /// A METHOD AND NOT A CLOSURE IN THE VIEW, the reason `Progress::rental`
    /// and `Progress::sampling` are: this is a decision and the view is a
    /// layout, and a decision inside a view macro can only be checked by a
    /// browser.
    ///
    /// NOT GATED ON `failed`, and that is the decision. A cancelled request
    /// carries a reason too, and it is the one with money in it -- "job 3/8:
    /// cannot be stopped, so it will finish and be paid for" is written to
    /// this same column by `finish`, and hiding it behind a status check
    /// would lose the sentence an operator pressed the button to read. Nor is
    /// it gated on `is_terminal`: nothing writes this column without also
    /// ending the request, so a guard for a live row with a reason would be a
    /// branch nothing can reach, and a branch that can never be taken is a
    /// branch nobody can show is right.
    ///
    /// IT IS NOT `Progress::failure`, which is the trap in the obvious fix.
    /// That one is folded from `destroy_failed` and its view prints "THE NODE
    /// MAY STILL BE RUNNING AND BILLING", which is true of a destroy that
    /// failed and false of a request that never reached a provider. Routing
    /// this reason through that field would print a false alarm about a node
    /// that does not exist, over the one page an operator reads to decide
    /// whether to go and check their bill -- the mirror of the false
    /// "abandoned" the holder check refuses to report, and the same defect as
    /// the preview caption that was true of one mode.
    pub fn reason(&self) -> Option<&str> {
        if self.error.is_empty() {
            None
        } else {
            Some(self.error.as_str())
        }
    }
}

/// Rows is a listing that survives a row it cannot read.
///
/// ONE UNDECODABLE ROW USED TO TAKE THE WHOLE ENVELOPE, and with it every
/// healthy request beside it. `Vec<Request>` decodes element by element into
/// ONE result, so a `width` of 3,000,000,000 -- which Go's `int` holds, which
/// this `i32` cannot, and which `POST /api/requests` answered 202 to whenever
/// there were no cached capabilities -- failed the whole `RequestList`.
/// `views::Queue`'s `if let Ok(l)` then kept the signal's last value, so the
/// table read "nothing queued" for the life of the tab and the two-second
/// poll re-fetched the same undecodable bytes forever. No error anywhere: the
/// row is in the store for good, and the page cannot say what it cannot
/// decode.
///
/// WIDENING THE TEN FIELDS TO i64 WAS THE OTHER CANDIDATE AND IS THE NARROWER
/// FIX. It makes the numbers that cross this seam today exact and says nothing
/// about the eleventh -- a string where a number was expected, a field
/// somebody adds as a `u64`, a shape this page has not been taught. What cost
/// the operator their queue was not the width: it was that one row's badness
/// is the whole list's. So a row that will not decode is dropped and COUNTED,
/// and `note` is what puts it on screen -- a row that vanished silently would
/// be the "reads as working" defect, with the operator's own request as the
/// thing that disappeared.
///
/// THE SERVER ALSO STOPS SENDING IT: `sdserver.MaxAxis` refuses that width
/// before the request is queued. Containment twice, by two mechanisms, as the
/// tar extractor is -- the bound stops the bad row existing, this stops one
/// bad row destroying the view of every good one -- and neither alone is
/// enough. The bound reaches the four geometry fields that have a published
/// maximum and none of the other six, which is written out at MaxAxis; and
/// the rule has to hold for a field somebody adds later, which is the same
/// sentence `nullable` above is here for.
///
/// `requests` ONLY, and not every list on this seam. `Offers` carries a
/// per-provider `error` already and the watcher's events are folded one at a
/// time, so this is the one envelope where a row nobody can read costs the
/// view of the rest -- and the one with somewhere to say so. A `dropped`
/// count nothing rendered would be the feature built end to end and never
/// connected.
#[derive(Clone, Debug, Default)]
pub struct Rows {
    pub items: Vec<Request>,

    /// How many rows the server sent that this page could not read.
    pub dropped: usize,

    /// Why the first of them did not decode, for the operator to paste.
    pub trouble: String,
}

impl Rows {
    /// What to say about the rows that did not decode; empty when there are
    /// none.
    ///
    /// THE SENTENCE IS DECIDED HERE and rendered in `views`, which is this
    /// module's own rule: a decision about data belongs where `cargo test`
    /// can reach it with no browser.
    pub fn note(&self) -> String {
        if self.dropped == 0 {
            return String::new();
        }
        format!(
            "{} request(s) the server listed are missing from this table: \
             this page could not read them ({})",
            self.dropped, self.trouble
        )
    }

    /// Whether "nothing queued" is true, which is not the same as the table
    /// being empty.
    ///
    /// A LISTING WHOSE EVERY ROW WAS DROPPED IS EXACTLY THE STATE THAT USED
    /// TO PRINT IT, and it is wrong in the one direction that matters: those
    /// requests are queued and about to rent a card. The decision is here
    /// rather than in the `<Show>` because this module is where cargo test
    /// reaches it; `views` only renders it.
    pub fn nothing_queued(&self) -> bool {
        self.items.is_empty() && self.dropped == 0
    }

    /// What the queue table's price cell says for one request, out of the
    /// listing this page holds NOW.
    ///
    /// BY ID AND NOT FROM A ROW THE CALLER IS HOLDING, which is the whole
    /// defect. The table's `<For>` is keyed on `(id, status)` -- the status
    /// is in the key precisely so that a status change rebuilds the row --
    /// and `requests.cost` is written AFTER the status is terminal: the
    /// queue's own comment says the rental's cost "arrives from the deferred
    /// teardown, which runs after the closure below has finished", `finish`
    /// writes done/failed/cancelled, and `Recorder` writes the cost through
    /// on `run_cost`. So the key `(id, "done")` is created with a zero cost
    /// and never changes again, and a cell built from the row it was handed
    /// showed that zero for the life of the tab while the two-second poll
    /// re-fetched the right figure hundreds of times.
    ///
    /// THE COLUMN IS THE ONLY PLACE A PAID RENTAL APPEARS IN THAT TABLE. A
    /// rider's zero is blank by design -- the rental is charged to the
    /// request that opened it and to no other -- so the opener's cell is the
    /// one cell with money in it, and it was the one that froze. An
    /// `ember serve` session that rented an H100 for an hour showed a queue
    /// of finished requests with no money in it at all.
    ///
    /// A ROW THAT HAS LEFT THE LISTING SAYS NOTHING rather than keeping what
    /// it last said: this is called from a cell that is on screen because the
    /// row is, so the case is the half-step where a listing has been replaced
    /// and the row has not been dropped yet, and the honest answer there is
    /// the blank one.
    pub fn price(&self, id: &str) -> String {
        match self.items.iter().find(|r| r.id == id) {
            None => String::new(),
            // Free is blank, which is the rule the column is built on: the
            // local provider's rentals really are free, and a rider's zero
            // is somebody else's bill rather than this request's.
            Some(r) if r.cost.is_free() => String::new(),
            Some(r) => r.cost.text().to_string(),
        }
    }
}

impl<'de> Deserialize<'de> for Rows {
    fn deserialize<D: Deserializer<'de>>(d: D) -> Result<Self, D::Error> {
        // Into values first, so the failure is per element. A
        // `Vec<Request>` would make the whole sequence one decode, which is
        // the defect.
        let raw = Vec::<serde_json::Value>::deserialize(d)?;
        let mut out = Rows::default();
        for v in raw {
            match serde_json::from_value::<Request>(v) {
                Ok(r) => out.items.push(r),
                Err(e) => {
                    out.dropped += 1;
                    if out.trouble.is_empty() {
                        out.trouble = e.to_string();
                    }
                }
            }
        }
        Ok(out)
    }
}

#[derive(Clone, Debug, Default, Deserialize)]
pub struct RequestList {
    #[serde(default, deserialize_with = "nullable")]
    pub requests: Rows,

    /// Which requests are generating right now.
    ///
    /// A LIST SINCE `max_nodes` (#204), and that is the one place this change
    /// reaches Rust. ember used to hold one rental by construction, so this
    /// was a single id and `views/queue.rs` compared it; with several rentals
    /// in flight several requests generate at once, and an equality would mark
    /// one row and leave the others looking queued.
    ///
    /// `nullable` FOR THE REASON `requests` HAS IT, which is not about today's
    /// server: `webapi.Server.running` answers `[]` and never nil. The rule
    /// has to hold for the field somebody adds later, because Go marshals a
    /// nil slice as `null` and a `null` into a `Vec` fails the WHOLE envelope
    /// -- which is how this page once showed an empty queue over a table full
    /// of requests.
    #[serde(default, deserialize_with = "nullable")]
    pub running: Vec<String>,
}

impl RequestList {
    /// Whether this request is one of the ones generating right now.
    ///
    /// A METHOD AND NOT AN `iter().any()` IN THE VIEW MACRO, which is this
    /// page's settled pattern -- `Rows::note`, `Rows::price`, `Request::reason`
    /// and `Stream::note` are all here for the same reason. A decision written
    /// inside a `view!` can only be checked by driving a browser, and this one
    /// is the difference between a queue table that marks every generating
    /// request and one that marks the first and leaves the rest looking
    /// queued. The `.get()` that reads the signal stays in the view, inside
    /// the closure, because that is the frozen-row defect this page has been
    /// bitten by four times.
    pub fn is_running(&self, id: &str) -> bool {
        self.running.iter().any(|r| r == id)
    }
}

/// What `POST /api/requests/{id}/cancel` answered.
///
/// A NAMED TYPE AND NOT `serde_json::Value`, which is how `cancelling` came
/// to be a field with a writer, a wire name, a comment explaining it and no
/// reader at all. The call site decoded into a dynamic value and threw the
/// whole `Result` away with `let _ =`, so the handler's 409 -- the one whose
/// message names the remediation -- reached the page and was dropped one
/// frame later, and the operator's only feedback over a card that may still
/// be billing was a row that still said `running` with a `cancel` button
/// still on it. Every other route on this seam has a named type here for the
/// same reason: a field added to the handler must reach something, or nobody
/// finds out it did not.
/// THE ANSWER'S OWN `id` IS NOT ON IT, and that is the same rule rather than
/// an exception to it: the page knows which row it clicked, `Stopping` is
/// keyed on that, and keying on the id the server echoed would mean an answer
/// that arrived without one put the cancel button back under a stop that is
/// already travelling. A field this page cannot act on is a field it does not
/// decode.
#[derive(Clone, Debug, Default, Deserialize)]
pub struct Cancelled {
    /// The status the row holds, which for a generating request is still
    /// `running` and is honest: the queue is polling sd-server for the job's
    /// own answer and is the one that records how it ended.
    #[serde(default)]
    pub status: String,
    /// Whether a stop is on its way to a rental.
    ///
    /// True only on the generating path, where the handler signalled
    /// `queue.Cancel` and returned while the batch is still running. The
    /// queued path has no rental to signal, so the handler makes the row
    /// terminal itself and answers without this.
    #[serde(default)]
    pub cancelling: bool,
}

impl Cancelled {
    /// What the row says between this answer and the next poll.
    ///
    /// THE DISTINCTION IS THE POINT, and it is the one the Go side is careful
    /// to keep: a request whose remaining jobs were dropped while the job in
    /// flight finishes and is paid for, and a request that had no rental at
    /// all, are different answers. Without the flag the only thing the page
    /// could say is the status, which on the generating path is `running` --
    /// so an accepted cancel would read exactly like a click that did not
    /// register, which is what sent an operator clicking again.
    ///
    /// AN ANSWER WITH NO STATUS IN IT reads as the flag being set, which is
    /// the safe direction for a response this page cannot interpret: claiming
    /// a request ended when it did not is the error that leaves a card
    /// billing unwatched. It is not the state before the server answers --
    /// that one is `Stopping` and has a word of its own, deliberately.
    pub fn label(&self) -> &str {
        if self.cancelling || self.status.is_empty() {
            return "cancelling";
        }
        self.status.as_str()
    }
}

/// Stopping is one cancel this page has asked for and the row has not caught
/// up with.
///
/// THE ASK AND THE ANSWER ARE DIFFERENT STATES, and giving them one word is
/// what would make this untestable from a browser. The page marks a request
/// the moment it asks -- the round trip must not be a window in which the
/// button is still there -- so if that placeholder said "cancelling" too,
/// then a page that went back to ignoring the response body would show
/// exactly the same cell, and the only check that could tell them apart would
/// be a Rust unit test of a page agreeing with itself. They are also honestly
/// different: "asking" is this page having sent a request, and "cancelling"
/// is the server having accepted one and a stop being on its way to a rental.
#[derive(Clone, Debug)]
pub struct Stopping {
    pub id: String,
    /// None until the server answers, and never reset once it has.
    pub answer: Option<Cancelled>,
}

impl Stopping {
    /// What the row's stop cell says.
    pub fn label(&self) -> &str {
        match &self.answer {
            None => "asking",
            Some(c) => c.label(),
        }
    }
}

/// One event, as `internal/report` writes it.
///
/// Flat, with everything optional, because that is how it crosses the wire:
/// one serde type here instead of thirteen, and the `kind` says which fields
/// mean anything.
#[derive(Clone, Debug, Default, Deserialize)]
pub struct Event {
    #[serde(default)]
    pub seq: i64,
    #[serde(default)]
    pub at: String,
    #[serde(default)]
    pub kind: String,
    #[serde(default)]
    pub text: String,
    #[serde(default)]
    pub cmd: String,
    #[serde(default)]
    pub unit: String,
    #[serde(default)]
    pub index: i32,
    #[serde(default)]
    pub total: i32,
    #[serde(default)]
    pub desc: String,
    #[serde(default)]
    pub pass: i32,
    #[serde(default)]
    pub step: i32,
    #[serde(default)]
    pub total_steps: i32,
    #[serde(default)]
    pub files: i32,
    #[serde(default)]
    pub bytes: i64,
    #[serde(default)]
    pub cost_micro_usd: i64,
    #[serde(default)]
    pub spent_micro_usd: i64,
    #[serde(default)]
    pub measured_ns: i64,
    #[serde(default)]
    pub node: String,
    #[serde(default)]
    pub endpoint: String,
    #[serde(default)]
    pub error: String,
}

/// What the socket sends.
#[derive(Clone, Debug, Default, Deserialize)]
pub struct Envelope {
    // THE SERVER'S OWN `next` IS DELIBERATELY NOT DECODED, and the judgements
    // that went with it are a note here because whoever reaches for it next
    // makes them again.
    //
    // It is the sequence number the server has handed over, so it looked like
    // the figure a reconnect should resume from -- and three comments in this
    // crate described a reconnect that did not exist while the field had no
    // reader outside one test assertion. It is FULLY DERIVABLE from what is
    // in the envelope: measured over a real store across seven event-set
    // shapes times eight cursors, three of them above the maximum, and again
    // after a prune -- 64 envelopes, and in every one
    // `next == max(cursor sent, max seq in the envelope)`. It is the last
    // event's own seq and not one past it, it falls back to the cursor the
    // client sent on an empty envelope, the tail is selected whole and
    // ascending with no limit, nothing filters the slice, and
    // `INSERT OR REPLACE` on `(request, seq)` means a redelivery overwrites
    // in place rather than leaving a counted hole. So reading it was a SECOND
    // WRITER of one field -- `queue.Cost`'s mistake, where a reader cannot
    // tell which of two paths is live -- and deleting the read left every
    // `cargo test` green, which is the measurement rather than a hole.
    //
    // And it is the LESS SAFE of the two in the one direction that matters.
    // The day this page tolerates an event it cannot decode by dropping it --
    // which is what `Rows` already does for a request it cannot read -- the
    // client-derived cursor stays below that event and a reconnect asks for
    // it again, while the server's figure is above it and the reconnect skips
    // it in silence. A cursor too LOW costs a replay the fold discards; a
    // cursor too HIGH costs the operator events they never learn about,
    // which is the frozen row this whole feature is about.
    //
    // THE FIELD'S OWN DOC COMMENT USED TO ARGUE THE OPPOSITE and was
    // backwards: it said `max` "can never move past an event that was not
    // folded", and `max` is precisely the operator that can -- `min` is the
    // one that cannot. Harmless only because `next` never exceeds the
    // envelope's maximum on anything this server produces, which makes it a
    // comment wrong about why a seal holds: the thing that gets a seal
    // removed by somebody being helpful, and here it would have licensed the
    // next reader to KEEP the read believing it was a safeguard.
    //
    // So if one is ever wanted: `min` of the two and never `max`, and advance
    // AFTER applying, because `apply` ignores anything at or below the cursor
    // and advancing first discards every event in the envelope that set it --
    // the whole content of the first send after a reconnect.
    #[serde(default, deserialize_with = "nullable")]
    pub events: Vec<Event>,
    #[serde(default, deserialize_with = "nullable")]
    pub request: Option<Request>,

    /// Which requests are generating, as the socket reports them.
    ///
    /// A LIST SINCE `max_nodes` (#204), AND THE TYPE IS LOAD-BEARING ALTHOUGH
    /// NOTHING READS THE FIELD. `grep -rn '.running' web/src` finds one
    /// reader and it is the queue table's, not this -- so this is a field with
    /// a writer, a wire name and no reader, which is the `cancelling` shape
    /// and is older than this change. It still has to change type: serde
    /// decodes what it is given whether or not a view asks for it, so an array
    /// arriving into a `String` is an error that drops the whole envelope and
    /// takes the watched request's state with it. A field nothing reads is
    /// cheap to leave and was NOT cheap to leave at the wrong type.
    #[serde(default, deserialize_with = "nullable")]
    pub running: Vec<String>,
}

/// Stream is what a watcher's progress socket is doing, and what the page
/// says about it.
///
/// IT REPLACED AN `RwSignal<bool>` WITH THREE WRITERS AND NO READER.
/// `connected` was set true on open and false on close and on a failed open,
/// and `grep -rn connected web/src` found no other line -- so a stream that
/// ended mid-generation left the last frame on screen under a panel still
/// captioned as though the card were sampling, for ever. The generation
/// carried on and was billed; the page looked like ember had stopped. The
/// same shape as the preview frame nothing inserted and as `cancelling`,
/// which had a writer, a wire name, a comment and no reader.
///
/// THREE STATES AND NOT A BOOL, because the page has to say two different
/// things and sometimes nothing, and "the stream stopped" and "the stream
/// stopped and is not coming back" need different remedies from the operator.
/// `Live` is also the state before the first handshake has completed: the
/// page has nothing to say about a socket it has only just asked for, and the
/// thing that does need to know the difference is the retry bound, which is
/// reset by the open handler rather than by this.
#[derive(Clone, Copy, Debug, Default, PartialEq, Eq)]
pub enum Stream {
    /// Open, or opening for the first time. Nothing is printed.
    #[default]
    Live,

    /// Closed, with a re-open scheduled.
    Reconnecting,

    /// Closed, with the retry bound spent.
    Lost,
}

impl Stream {
    /// note is the line the page prints, and `None` is the state that prints
    /// nothing.
    ///
    /// A LINE THAT IS ALWAYS THERE IS NOT A SIGNAL, which is why `Live`
    /// answers nothing rather than "connected": a tab that says so while
    /// everything is normal teaches the operator to stop reading the line,
    /// and the one state this exists for is the one where the step on screen
    /// is stale.
    ///
    /// NO VERDICT ABOUT THE GENERATION, deliberately. ember cannot tell a
    /// dropped socket from a stopped server from a sleeping laptop, and the
    /// generation may well still be running and billing -- so the sentence
    /// says what is true (the step shown is the last one that arrived) and
    /// names the two things that answer the rest, rather than guessing.
    pub fn note(self) -> Option<&'static str> {
        match self {
            Stream::Live => None,
            Stream::Reconnecting => Some(
                "the progress stream stopped, so the step shown is the last one that \
                 arrived — reconnecting",
            ),
            Stream::Lost => Some(
                "the progress stream stopped and did not come back, so the step shown is \
                 the last one that arrived — the generation may still be running: reload \
                 this page, or ask ember history",
            ),
        }
    }
}

/// RETRY_ATTEMPTS bounds the re-opens a watcher makes.
///
/// TWELVE, which is about forty-three seconds of backoff, and the figure is
/// chosen against the slowest of the three cases rather than the fastest. A
/// socket that dropped on its own and a forwarded port that blipped are back
/// inside a second; a laptop that slept comes back when its timers do; and an
/// `ember serve` RESTART is the one that sets the bound, because it is the
/// remediation ember's own cancel 409 prints and the operator has to type the
/// command again -- a bound a hand restart cannot fit inside would make
/// "including because ember serve restarted" a case this never covers.
/// Forty-three seconds is still under a minute, and the last attempts are one
/// every five seconds against a closed port on loopback, which costs nothing
/// measurable.
///
/// AND IT IS BOUNDED AT ALL because the alternative cannot report: an
/// unbounded retry is a loop nobody asked for, and a page that retries for
/// ever can never print the sentence that tells an operator the stream is not
/// coming back and that reloading is what fixes it.
pub const RETRY_ATTEMPTS: u32 = 12;

/// reopen_after answers when a watcher re-opens a stream that just ended, and
/// `None` when it must not.
///
/// A DECISION IN `model` AND NOT A CHAIN OF `if` IN A CLOSURE, for
/// `Listing::answer`'s reason: a retry policy written inside an event handler
/// can only be checked by driving a browser, and this one has two rules worth
/// an exit code of their own.
///
/// A TERMINAL REQUEST IS NOT A LOSS. Nothing more will be said about a
/// request that is done, failed, cancelled or interrupted, so a stream that
/// ends over one has delivered everything there was -- and re-opening would
/// be a socket, and a goroutine on the other end polling SQLite, for a
/// generation that finished. This is also what makes the restart case
/// terminate: `queue.Run` sweeps everything it finds running to `interrupted`
/// at startup, so the first successful reconnect after a restart arrives at a
/// terminal row and stops.
///
/// BACKOFF RATHER THAN A FIXED CADENCE, which is `awaitJob`'s measurement one
/// layer out: a flat interval is a guess about how long the thing being
/// waited for takes, and here the first attempt -- a socket that dropped on
/// its own with the server still up -- is the one most likely to succeed, so
/// it is the one that must not wait two seconds.
pub fn reopen_after(terminal: bool, attempt: u32) -> Option<std::time::Duration> {
    if terminal || attempt >= RETRY_ATTEMPTS {
        return None;
    }
    // 250ms doubling to a five-second cap. The cap is what keeps twelve
    // attempts inside a minute while still leaving the later ones far enough
    // apart to be worth making.
    let ms = (250_u64 << attempt).min(5_000);
    Some(std::time::Duration::from_millis(ms))
}

/// Where one job has got to.
#[derive(Clone, Debug, Default, PartialEq)]
pub struct JobProgress {
    pub index: i32,
    pub total: i32,
    pub desc: String,

    /// A PASS AND A STEP WITHIN IT, never a percentage.
    ///
    /// sd-server's `step` and `total_steps` describe the current sampling
    /// pass, not the whole job, and passes restart for each batch item, each
    /// high- and low-noise stage and each highres pass. A bar built from them
    /// runs to the end and then starts again, which reads as a hang -- so this
    /// UI shows the three numbers and no bar.
    pub pass: i32,
    pub step: i32,
    pub total_steps: i32,

    pub files: i32,
    pub bytes: i64,
    pub done: bool,

    /// Set when the operator cancelled this request while this job was
    /// sampling, and holding the reason the job could not be stopped when
    /// there was one.
    ///
    /// SEPARATE FROM `done`, because the two answer different questions and
    /// the row has to show both. `done` means images arrived; a cancelled job
    /// usually has none, and marking it done would print "0 file(s)" over a
    /// job that was stopped. Leaving it neither done nor cancelled is worse:
    /// the row keeps its last step for ever, which is the frozen-progress
    /// defect the plain map in views.rs exists to prevent, reached from the
    /// other direction.
    pub cancelled: bool,
    pub cancel_error: String,
}

/// What a watcher may honestly say about the rental's cost.
///
/// THE ROW USED TO BE GATED ON ONE NUMBER AND DISPLAY ANOTHER. It appeared
/// when `Progress::spent` -- the card time the queue emits per job -- became
/// non-zero, under the caption "card time", and printed `Request::cost`,
/// which is the whole rental. Those are different quantities written at
/// different times to different requests: `requests.cost` comes from
/// `run_cost`, which `provision.WithNode` emits from its DEFERRED teardown,
/// and `Queue.lifecycle` routes to the request that opened the rental and to
/// no other. So the row read `$0` from the first completed job until the node
/// was destroyed, and `$0` for ever on a request that rode an existing
/// rental -- which AGENTS.md records as the one thing a money column must not
/// do: "a rider's rental cost is zero by design, and a money column of zeros
/// reads as free".
///
/// `ember history` answers it with `Entry.Rode` per row, and this is the same
/// answer for one request: the state is decided by asking WHO WAS TOLD the
/// rental's facts, never by the zero itself. The local provider's rentals are
/// genuinely free, so cost alone cannot tell "somebody else paid" from
/// "nobody did" -- and `node_ready` reaching this request's stream is the
/// same routing decision that decides which request is charged, so it is one
/// source of truth rather than a second guess at it.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Rental {
    /// Nothing has happened that could have cost anything: no node became
    /// ready for this request and no job of it has started. The row is
    /// absent, because a request that has not run has no rental to price.
    Silent,

    /// This request opened a rental that has not been torn down, so what it
    /// cost does not exist yet rather than being zero.
    Pending,

    /// Pending, and card time has already been charged to this request -- so
    /// the card's rate is above zero and the figure is growing while the
    /// operator reads it. The stronger claim is made only when the event
    /// stream proves it, which is the direction `Caps.PublicSSH` defaults in.
    Accruing,

    /// This request generated on a rental another request opened, which is
    /// the one charged for it. Its zero is by design and is `ember history`'s
    /// dash.
    Rode,

    /// The rental was torn down and this request is the one it is charged to,
    /// so `Request::cost` is the figure and `$0` means free rather than
    /// unpriced.
    Charged,
}

/// What a watcher has folded out of the event stream.
#[derive(Clone, Debug, Default)]
pub struct Progress {
    /// The last sequence number seen, which is the cursor a reconnect resumes
    /// from.
    pub cursor: i64,

    pub node: String,
    pub endpoint: String,

    /// What the resident weights cost to load, once, said in the one line an
    /// operator would size a batch from.
    pub resident: String,

    pub server: String,
    pub jobs: Vec<JobProgress>,

    /// The accumulating card time, from the last cost event.
    ///
    /// A FIGURE AND NOT A DISPLAY STRING, so this is read as evidence and
    /// never printed: a positive card time means this request has been
    /// charged for time on a card whose rate is above zero, which is the one
    /// money fact a watcher has before the teardown writes the rental's own.
    /// What it is NOT is a number this page may render -- see `Money` for
    /// why -- and nothing on the browser's wire carries a rendered card-time
    /// figure, because `JobRun.Cost` is kept off `Job` on purpose. The
    /// figure in dollars is `ember history`'s card-time column.
    pub spent: i64,

    /// Whether the rental's own cost has been recorded for this request.
    ///
    /// IT REPLACED AN `i64` HOLDING THE SAME NUMBER AS `Request::cost`, which
    /// is the `queue.Cost` mistake in miniature: two carriers of one figure,
    /// and the one this page could actually print is the other one. `run_cost`
    /// arrives from `provision.WithNode`'s deferred teardown and carries
    /// `cost_micro_usd`; `Recorder` writes that same integer to `requests.cost`,
    /// which reaches the page as a `Money` with the string Go rendered. So the
    /// event adds exactly one thing the row cannot get anywhere else: that the
    /// figure EXISTS. Without it a recorded $0 -- the local provider's rentals
    /// really are free -- cannot be told from a rental nobody has priced yet,
    /// and `Money::is_free` alone would make "free" and "not yet" the same
    /// cell.
    ///
    /// It is true only for the request that OPENED the rental, because
    /// `Queue.lifecycle` emits `run_cost` to that request's sink and a rider's
    /// sink never sees it.
    pub charged: bool,

    /// Lines the node itself wrote, newest last.
    pub log: Vec<String>,

    /// How many jobs of this request were dropped because it was cancelled.
    ///
    /// ON THE REQUEST AND NOT ON A ROW, because the dropped jobs have no rows
    /// -- they never started, so no unit_started ever named them. A watcher
    /// that only marked the in-flight job would show one cancelled row above
    /// nothing at all, and the question the operator pressed the button to
    /// answer is how many of the ten are still coming.
    pub dropped: i32,

    pub failure: String,
}

/// maxLog bounds what is kept.
///
/// A model load prints hundreds of lines and a `-v` sd-server prints one per
/// tensor, so an unbounded log is a tab that grows until the browser stops.
const MAX_LOG: usize = 400;

impl Progress {
    /// fold takes one envelope.
    ///
    /// ONE PLACE AND NOT A LOOP INSIDE THE VIEW, which is where it was: what
    /// this leaves behind is the cursor a reconnect resumes from, and a
    /// decision that reaches the wire belongs where `cargo test` can see it.
    /// `Envelope`'s own comment has why the server's `next` is not read here.
    pub fn fold(&mut self, env: &Envelope) {
        for e in &env.events {
            self.apply(e);
        }
    }

    /// apply folds one event in.
    ///
    /// IDEMPOTENT PER SEQUENCE NUMBER, because a reconnect can re-deliver:
    /// the socket resumes from a cursor the client chose, and a client that
    /// had already applied an event must not count it twice. Anything at or
    /// below the cursor is ignored.
    pub fn apply(&mut self, e: &Event) {
        if e.seq != 0 && e.seq <= self.cursor {
            return;
        }
        if e.seq > self.cursor {
            self.cursor = e.seq;
        }

        match e.kind.as_str() {
            "log" => {
                self.log.push(e.text.clone());
                if self.log.len() > MAX_LOG {
                    let drop = self.log.len() - MAX_LOG;
                    self.log.drain(0..drop);
                }
            }
            "node_ready" => {
                self.node = e.node.clone();
                self.endpoint = e.endpoint.clone();
            }
            "server_starting" => self.server = e.cmd.clone(),
            "model_resident" => {
                // ember's own measurement, not the server's reported figure.
                // sd-server said 551ms for weights that are 56 GiB, so the
                // span ember times itself is the honest one.
                self.resident = human_duration(e.measured_ns);
            }
            "unit_started" => {
                let j = self.job_mut(e.index, e.total);
                j.desc = e.desc.clone();
            }
            "job_progress" => {
                let j = self.job_mut(e.index, e.total);
                j.pass = e.pass;
                j.step = e.step;
                j.total_steps = e.total_steps;
            }
            "job_images" => {
                let j = self.job_mut(e.index, e.total);
                j.files = e.files;
                j.bytes = e.bytes;
                j.done = true;
            }
            "unit_cost" => {
                self.spent = e.spent_micro_usd;
            }
            "job_cancelled" => {
                // An EMPTY unit means no job was in flight -- the cancel
                // landed between two jobs -- and `index` is then the count
                // that finished rather than a job number. Folding that into
                // a row would mark the last COMPLETED job as cancelled,
                // which is the one row on screen that is not.
                if e.unit == "job" {
                    let j = self.job_mut(e.index, e.total);
                    j.cancelled = true;
                    j.cancel_error = e.error.clone();
                }
                self.dropped = (e.total - e.index).max(0);
            }
            "run_cost" => {
                // THE ARRIVAL AND NOT THE AMOUNT. See Progress::charged: the
                // amount reaches this page as `Request::cost`, already
                // rendered by the one formatter in the program, and folding
                // the integer in here as well would be a second copy of a
                // number this page must not format.
                self.charged = true;
                self.node = e.node.clone();
            }
            "destroy_failed" => {
                // THE ONE AN OPERATOR MUST NOT MISS: the node may still be
                // running, and running means billing.
                //
                // THIS ARM USED TO BE `"destroy_failed" | "run_failed"` and
                // the second half was unreachable: `report.KindRunFailed` was
                // declared, listed and never emitted by anything in Go. It is
                // deleted rather than wired up, and the reason is this view:
                // `Progress::failure` is rendered as "THE NODE MAY STILL BE
                // RUNNING AND BILLING -- check ember ls", which is a claim
                // about a node that exists. Most failures have no node -- a
                // money ceiling, a sampler the capabilities do not list, no
                // capacity -- so a run_failed folded in here would print a
                // false alarm on the page an operator reads to decide whether
                // to check their bill. A request's own reason is
                // `Request::reason`, which is a column and not an event.
                self.failure = e.error.clone();
            }
            _ => {}
        }
    }

    fn job_mut(&mut self, index: i32, total: i32) -> &mut JobProgress {
        if let Some(pos) = self.jobs.iter().position(|j| j.index == index) {
            return &mut self.jobs[pos];
        }
        self.jobs.push(JobProgress {
            index,
            total,
            ..Default::default()
        });
        self.jobs.sort_by_key(|j| j.index);
        let pos = self.jobs.iter().position(|j| j.index == index).unwrap();
        &mut self.jobs[pos]
    }

    /// The jobs that still have a progress frame coming.
    ///
    /// `!cancelled` AS WELL AS `!done`, which is the half a cancel adds: a
    /// stopped job will never produce another frame, so without it the last
    /// half-sampled latent sits on screen under the caption "in progress"
    /// for the rest of the rental -- a picture that reads as a generation
    /// still running on a card nobody is paying for any more.
    ///
    /// A method rather than a closure in the view, because this is a
    /// decision and the view is a layout: a predicate inside the Gallery
    /// component can only be checked by a browser, and this one can be
    /// checked here.
    pub fn sampling(&self) -> Vec<JobProgress> {
        self.jobs
            .iter()
            .filter(|j| !j.done && !j.cancelled && j.step > 0)
            .cloned()
            .collect()
    }

    /// What the money row may say, and in which of five states.
    ///
    /// A METHOD AND NOT A CLOSURE IN THE VIEW, the reason `sampling` is one:
    /// this is a decision and the view is a layout, and a decision inside a
    /// view macro can only be checked by a browser. It also has to be
    /// checkable where it cannot be driven -- every lab rents from the local
    /// provider, whose price is zero, so `Accruing` and a non-free `Charged`
    /// are states no lab on this machine can reach. That is exactly why the
    /// defect this replaces was invisible: with `spent` always zero the old
    /// row never rendered in a lab at all.
    ///
    /// ORDER IS THE WHOLE FUNCTION. `charged` is asked first because it is
    /// the only state in which a figure exists; then the opener, because
    /// `node_ready` reached its stream; then the rider, because a job of this
    /// request has started on a rental whose readiness it was never told
    /// about.
    pub fn rental(&self) -> Rental {
        if self.charged {
            return Rental::Charged;
        }
        if !self.node.is_empty() {
            // `spent` is read as evidence and not as a figure: a positive
            // card time cannot happen at a zero rate, so "accruing" is
            // proved rather than assumed. Zero says nothing either way --
            // the first job may simply not have finished -- so it falls back
            // to the weaker sentence.
            if self.spent != 0 {
                return Rental::Accruing;
            }
            return Rental::Pending;
        }
        // A RIDER IS THE ABSENCE OF `node_ready`, not the zero in the cost.
        // Its own per-job events reach its own sink, so a started job with no
        // node of its own is a request generating on somebody else's rental.
        // Jobs and not `spent`, because a rider on a free rental has card
        // time zero for ever and is still a rider.
        if !self.jobs.is_empty() {
            return Rental::Rode;
        }
        Rental::Silent
    }

    /// The whole log as one string, for a `<pre>`.
    pub fn log_text(&self) -> String {
        self.log.concat()
    }
}

/// human_duration renders nanoseconds the way Go's `time.Duration` does,
/// roughly.
///
/// Only roughly: exactness is not needed because every figure that MATTERS --
/// a price, a cost -- arrives pre-rendered from Go, for the reason `Money`
/// explains. This is for the one line about how long the weights took.
pub fn human_duration(ns: i64) -> String {
    if ns <= 0 {
        return "0s".into();
    }
    let secs = ns as f64 / 1e9;
    if secs < 1.0 {
        return format!("{}ms", (secs * 1000.0).round() as i64);
    }
    if secs < 60.0 {
        return format!("{:.1}s", secs);
    }
    let m = (secs / 60.0).floor() as i64;
    let s = secs - (m as f64) * 60.0;
    format!("{m}m{s:.0}s")
}

#[cfg(test)]
mod tests {
    use super::*;

    fn ev(seq: i64, kind: &str) -> Event {
        Event {
            seq,
            kind: kind.into(),
            ..Default::default()
        }
    }

    /// A reconnect re-delivers, and an event applied twice must not count
    /// twice.
    ///
    /// The socket resumes from a cursor the client chose, so the first
    /// envelope after a reconnect can overlap with what was already folded
    /// in. Without the cursor check, a cost would accumulate on every
    /// reconnect and the UI would report a rental costing more each time the
    /// operator's laptop woke up.
    #[test]
    fn ignores_what_it_has_already_seen() {
        let mut p = Progress::default();
        let mut e = ev(1, "unit_cost");
        e.spent_micro_usd = 30_000;
        p.apply(&e);
        p.apply(&e);
        p.apply(&e);

        assert_eq!(p.cursor, 1);
        assert_eq!(p.spent, 30_000, "a re-delivered event was applied again");
    }

    /// A reconnect that resumed from zero replays the whole generation, and
    /// the fold has to come out of that unchanged.
    ///
    /// THE OTHER HALF OF "no gap and no duplicate", at the layer that decides
    /// it. The lab can see a gap -- a row that never reports its files -- and
    /// cannot see a duplicate, because `apply`'s idempotence is what hides
    /// one; so the duplicate is asserted here, over the whole envelope rather
    /// than one event, which is the shape a resume actually delivers.
    ///
    /// Its negative control is the assertion that the cursor MOVED: an
    /// envelope folded into a Progress that rejected everything in it would
    /// satisfy "nothing was counted twice" perfectly.
    #[test]
    fn folding_a_replayed_envelope_changes_nothing() {
        let mut cost = ev(2, "unit_cost");
        cost.spent_micro_usd = 30_000;
        let env = Envelope {
            events: vec![ev(1, "log"), cost, ev(3, "log")],
            ..Default::default()
        };

        let mut p = Progress::default();
        p.fold(&env);
        let once = (p.cursor, p.spent, p.log.len());
        assert_eq!(once, (3, 30_000, 2), "the first fold did not land");

        p.fold(&env);
        p.fold(&env);
        assert_eq!(
            (p.cursor, p.spent, p.log.len()),
            once,
            "a replayed envelope was folded again"
        );
    }

    /// The cursor a reconnect asks from is the largest sequence number this
    /// page has FOLDED, and an envelope with nothing in it does not move it.
    ///
    /// THE ONE THING `Socket::open`'s `after` MUST NOT DO is run ahead of
    /// what was applied, because the server answers it with everything
    /// STRICTLY after and a cursor one too high is an event the operator
    /// never learns about. The server's own `next` would have been the
    /// obvious source and is deliberately not decoded -- see `Envelope` for
    /// the measurement and for which operator a future reader would need --
    /// so this is the assertion that the number the socket carries is the one
    /// the fold reached: a `force` send, which the server answers with no
    /// events whenever only the request changed, leaves it where it was.
    #[test]
    fn the_cursor_is_what_was_folded() {
        let mut p = Progress::default();
        p.fold(&Envelope {
            events: vec![ev(7, "log")],
            ..Default::default()
        });
        assert_eq!(p.cursor, 7);
        assert_eq!(p.log.len(), 1, "the event in the envelope was dropped");

        p.fold(&Envelope::default());
        assert_eq!(p.cursor, 7, "an envelope with no events moved the cursor");
    }

    /// A stream that is open says nothing, and a stream that stopped says so.
    ///
    /// `connected` was a bool with three writers and NO READER, so this is the
    /// reader -- and the absence in the first arm is as much of the assertion
    /// as the sentences: a line that is always on screen is not a signal.
    #[test]
    fn only_a_stopped_stream_is_reported() {
        assert_eq!(Stream::Live.note(), None);

        let reconnecting = Stream::Reconnecting
            .note()
            .expect("a stopped stream says so");
        assert!(
            reconnecting.contains("last one that arrived"),
            "the operator is not told the step is stale: {reconnecting}"
        );
        assert!(
            reconnecting.contains("reconnecting"),
            "the operator is not told ember is trying again: {reconnecting}"
        );

        let lost = Stream::Lost.note().expect("a stream that is gone says so");
        assert!(
            lost.contains("did not come back"),
            "a spent retry bound reads like one still in progress: {lost}"
        );
        // The remedy, because this is the state nothing else will fix. And
        // the generation may still be running: a watcher that stopped is not
        // a rental that stopped.
        assert!(
            lost.contains("reload") && lost.contains("still be running"),
            "the state nothing recovers from names no remedy: {lost}"
        );
        assert_ne!(
            Stream::Reconnecting.note(),
            Stream::Lost.note(),
            "a page that is still trying and one that has given up read the same"
        );
    }

    /// The retry is bounded, backs off, and does not run at all over a
    /// request that has ended.
    #[test]
    fn the_retry_is_bounded_and_backs_off() {
        // A terminal request has said everything there was, so re-opening
        // would be a socket and a polling goroutine for a finished
        // generation.
        assert_eq!(reopen_after(true, 0), None);

        let first = reopen_after(false, 0).expect("the first re-open is made");
        let second = reopen_after(false, 1).expect("the second re-open is made");
        assert!(
            first < second,
            "the retry does not back off: {first:?} then {second:?}"
        );
        // The first attempt is the one most likely to succeed -- a socket
        // that dropped with the server still up -- so it must not wait the
        // way the last one does.
        assert!(
            first <= std::time::Duration::from_millis(500),
            "the first re-open waits {first:?}, which an operator reads as a freeze"
        );

        // Capped, or the eighth attempt would be two minutes out.
        let last = reopen_after(false, RETRY_ATTEMPTS - 1).expect("the last re-open is made");
        assert!(
            last <= std::time::Duration::from_secs(5),
            "the backoff is uncapped: {last:?}"
        );

        // AND IT STOPS. An unbounded retry is a loop nobody asked for, and a
        // page that retries for ever can never print the sentence telling the
        // operator to reload.
        assert_eq!(reopen_after(false, RETRY_ATTEMPTS), None);
        assert_eq!(reopen_after(false, RETRY_ATTEMPTS + 1), None);

        // The whole bound, which is what the comment above RETRY_ATTEMPTS
        // claims: long enough for an `ember serve` restart, short enough that
        // a tab against a server that has gone says so.
        let total: std::time::Duration = (0..RETRY_ATTEMPTS)
            .map(|a| reopen_after(false, a).expect("every attempt below the bound is made"))
            .sum();
        assert!(
            total >= std::time::Duration::from_secs(30)
                && total <= std::time::Duration::from_secs(60),
            "the retry bound is {total:?}, which is not the forty-odd seconds it is \
             written as -- long enough for a hand restart of ember serve, short enough \
             that a tab against a server that has gone says so"
        );
    }

    /// The same frame arrives on several polls, which is what a real server
    /// does.
    ///
    /// ember's own `awaitJob` deduplicates on the step before it emits, and
    /// the preview lab measured eleven of sixteen consecutive frame pairs
    /// identical -- so by the time events reach here the steps are distinct.
    /// What this checks is that re-applying one anyway is harmless, because a
    /// reconnect is exactly that.
    #[test]
    fn progress_is_the_latest_step_and_not_a_sum() {
        let mut p = Progress::default();
        for (seq, step) in [(1, 1), (2, 2), (3, 3)] {
            let mut e = ev(seq, "job_progress");
            e.index = 1;
            e.total = 2;
            e.pass = 1;
            e.step = step;
            e.total_steps = 4;
            p.apply(&e);
        }
        assert_eq!(p.jobs.len(), 1);
        assert_eq!(p.jobs[0].step, 3);
        assert_eq!(p.jobs[0].total_steps, 4);
        assert_eq!(p.jobs[0].pass, 1);
    }

    /// Jobs come back in order whatever order their events arrive in, because
    /// "job 2/6" is the label on a file the operator will go and look at.
    #[test]
    fn jobs_are_ordered_by_index() {
        let mut p = Progress::default();
        for (seq, index) in [(1, 3), (2, 1), (3, 2)] {
            let mut e = ev(seq, "unit_started");
            e.index = index;
            e.total = 3;
            e.desc = format!("job {index}");
            p.apply(&e);
        }
        assert_eq!(
            p.jobs.iter().map(|j| j.index).collect::<Vec<_>>(),
            vec![1, 2, 3]
        );
    }

    #[test]
    fn folds_a_whole_rental() {
        let mut p = Progress::default();

        let mut ready = ev(1, "node_ready");
        ready.node = "local/n1".into();
        ready.endpoint = "root@127.0.0.1:2222".into();
        p.apply(&ready);

        let mut starting = ev(2, "server_starting");
        starting.cmd = "sd-server --model /m.gguf".into();
        p.apply(&starting);

        let mut resident = ev(3, "model_resident");
        resident.measured_ns = 6_040_000_000;
        resident.total = 2;
        p.apply(&resident);

        let mut started = ev(4, "unit_started");
        started.index = 1;
        started.total = 1;
        started.desc = "\"a cat\" 512x512".into();
        p.apply(&started);

        let mut images = ev(5, "job_images");
        images.index = 1;
        images.total = 1;
        images.files = 2;
        images.bytes = 4096;
        p.apply(&images);

        let mut cost = ev(6, "unit_cost");
        cost.spent_micro_usd = 55_000;
        p.apply(&cost);

        let mut rental = ev(7, "run_cost");
        rental.cost_micro_usd = 120_000;
        rental.node = "local/n1".into();
        p.apply(&rental);

        assert_eq!(p.node, "local/n1");
        assert_eq!(p.endpoint, "root@127.0.0.1:2222");
        assert_eq!(p.resident, "6.0s");
        assert!(p.server.contains("sd-server"));
        assert_eq!(p.jobs.len(), 1);
        assert!(p.jobs[0].done);
        assert_eq!(p.jobs[0].files, 2);
        assert_eq!(p.spent, 55_000);
        // THE ARRIVAL AND NOT THE AMOUNT. The rental's own total is larger
        // than the sum of its jobs by exactly the per-rental overhead the
        // served shape pays once -- and it reaches this page as
        // `Request::cost`, rendered by Go. What the event adds is that the
        // figure now exists, which is what tells a recorded $0 from a rental
        // nobody has priced.
        assert!(p.charged, "run_cost arrived, so the rental has been priced");
        assert_eq!(p.rental(), Rental::Charged);
        assert!(p.failure.is_empty());
    }

    /// The five states the money row can be in, and the two the old row
    /// could not tell apart.
    ///
    /// The row was gated on `spent` and displayed `Request::cost`, so it had
    /// two states on screen -- absent, and a figure -- for five on the wire.
    /// "nothing has been priced yet", "somebody else's rental" and "this one
    /// was free" all rendered as `$0`, which is the reading a money column
    /// must never allow.
    #[test]
    fn the_rental_state_is_who_was_told_and_never_the_zero() {
        let ready = {
            let mut e = ev(1, "node_ready");
            e.node = "local/ember-run".into();
            e
        };
        let started = {
            let mut e = ev(2, "unit_started");
            e.index = 1;
            e.total = 2;
            e
        };
        let paid = {
            let mut e = ev(3, "unit_cost");
            e.unit = "job".into();
            e.index = 1;
            e.total = 2;
            e.spent_micro_usd = 30_000;
            e
        };
        let torn = {
            let mut e = ev(4, "run_cost");
            e.node = "local/ember-run".into();
            e.cost_micro_usd = 120_000;
            e
        };

        for (want, events, why) in [
            (
                Rental::Silent,
                vec![],
                "a request that has not run has no rental to price",
            ),
            (
                Rental::Pending,
                vec![&ready],
                "this request opened a rental that has not been torn down",
            ),
            (
                Rental::Accruing,
                vec![&ready, &started, &paid],
                "card time at a rate above zero, so the figure is growing",
            ),
            (
                Rental::Rode,
                vec![&started],
                "a job started and no node_ready ever reached this stream",
            ),
            (
                // A RIDER ON A FREE RENTAL IS STILL A RIDER, which is why the
                // test is `jobs` and not `spent`: the local provider's price
                // is zero, so a rider there has card time zero for ever and
                // the zero says nothing about who paid.
                Rental::Rode,
                vec![&started, &{
                    let mut e = ev(3, "job_images");
                    e.index = 1;
                    e.total = 2;
                    e.files = 1;
                    e
                }],
                "a finished free rider has no cost of its own and still rode",
            ),
            (
                Rental::Charged,
                vec![&ready, &started, &paid, &torn],
                "run_cost arrived, so Request::cost is a figure and not a gap",
            ),
        ] {
            let mut p = Progress::default();
            for e in events {
                p.apply(e);
            }
            assert_eq!(p.rental(), want, "{why}");
        }
    }

    /// `max_nodes` means several requests generate at once, and the mark is a
    /// MEMBERSHIP rather than an equality (#204).
    ///
    /// WHY THIS IS A TEST AND NOT A COMMENT. `class:running` used to compare
    /// one id, and the shape of the defect being refused is a table that marks
    /// the first generating request and leaves the second looking queued --
    /// with both cards billing. Only a browser can see the class land, which
    /// is why the DECISION is a method: `TestServe_UI` asserts the row and
    /// this asserts what it will be told.
    ///
    /// THE THIRD ID IS THE HALF THAT MATTERS. An assertion that only looked
    /// for the two generating rows being marked would pass against a
    /// membership test that answered true for everything, which is the mark on
    /// every row -- a highlight that says nothing. So a queued request in the
    /// same listing must NOT be marked, and an empty list must mark nothing at
    /// all.
    #[test]
    fn every_generating_request_is_marked_and_nothing_else_is() {
        const LISTING: &str = r#"{
            "requests": [],
            "running": ["req-a", "req-c"]
        }"#;
        let list: RequestList = serde_json::from_str(LISTING).expect("the listing decodes");
        for id in ["req-a", "req-c"] {
            assert!(
                list.is_running(id),
                "{id} is generating and the table would not mark it, so an operator reading \
                 the queue cannot see which of their cards are busy"
            );
        }
        assert!(
            !list.is_running("req-b"),
            "a queued request was marked as generating: a mark on every row is a mark that \
             says nothing"
        );

        let idle: RequestList = serde_json::from_str(r#"{"requests":[]}"#).expect("decodes");
        assert!(
            !idle.is_running("req-a"),
            "nothing is generating and a row was still marked"
        );
        assert!(
            idle.running.is_empty(),
            "a missing `running` did not default to an empty list"
        );

        // AND A NULL DECODES, which is the rule that has to hold for the
        // field somebody adds later rather than for this server: Go marshals a
        // nil slice as `null`, and a `null` into a `Vec` without `nullable`
        // fails the WHOLE envelope -- which is how this page once showed an
        // empty queue over a table full of requests.
        let nulled: RequestList =
            serde_json::from_str(r#"{"requests":[],"running":null}"#).expect("a null decodes");
        assert!(nulled.running.is_empty());
    }

    /// The envelope the old row read `$0` out of, verbatim from the wire.
    ///
    /// `ws.go` sends the request alongside the events, and mid-generation
    /// that request's `cost` is `{"micro_usd":0,"display":"$0"}` -- the
    /// server really does send the string, so `Money::text`'s own `"$0"`
    /// fallback was never even reached. Thirty thousand microdollars of card
    /// time had been reported by then. The assertion is that the page is now
    /// in a state that prints a sentence rather than that figure.
    #[test]
    fn a_running_request_is_not_in_a_state_that_prints_its_zero_cost() {
        const ENVELOPE: &str = r#"{
            "next": 6,
            "events": [
                {"seq":1,"kind":"node_ready","node":"local/ember-run",
                 "endpoint":"root@127.0.0.1:2222"},
                {"seq":5,"kind":"unit_started","unit":"job","index":1,"total":2,
                 "desc":"\"a paper boat\" 192x192"},
                {"seq":6,"kind":"unit_cost","unit":"job","index":1,"total":2,
                 "cost_micro_usd":15000,"spent_micro_usd":30000}
            ],
            "request": {"id":"req-1","status":"running","provider":"local",
                        "gpu":"localhost","cost":{"micro_usd":0,"display":"$0"},
                        "jobs":[],"images":[]},
            "running": ["req-1"]
        }"#;

        let env: Envelope = serde_json::from_str(ENVELOPE).expect("envelope decodes");
        let mut p = Progress::default();
        for e in &env.events {
            p.apply(e);
        }
        let req = env.request.expect("the request travels with the events");

        assert_eq!(
            req.cost.text(),
            "$0",
            "the server sent the string, not a blank"
        );
        assert!(
            !p.charged,
            "no run_cost has arrived, so nothing has been priced"
        );
        assert_eq!(
            p.rental(),
            Rental::Accruing,
            "card time has been charged and the rental has not been priced"
        );
    }

    /// The two bodies the cancel route answers, verbatim, and the one word
    /// each gets.
    ///
    /// `webapi.go` builds them by hand, and they are different shapes on
    /// purpose: a generating request keeps the status the row holds and
    /// carries `cancelling`, because the queue is the one that records how
    /// the batch ended; a queued request has no rental, so the handler makes
    /// it terminal itself and answers with the final status and no flag.
    /// Decoding both into one type is what makes the page able to tell them
    /// apart -- `serde_json::Value` and `let _ =` could not.
    #[test]
    fn a_cancel_answer_says_whether_a_stop_is_still_travelling() {
        let generating: Cancelled =
            serde_json::from_str(r#"{"id":"req-1","status":"running","cancelling":true}"#)
                .expect("the generating answer decodes");
        assert_eq!(
            generating.label(),
            "cancelling",
            "the row's own status is still `running` and saying that would read as a \
             click that did not register"
        );

        let queued: Cancelled = serde_json::from_str(r#"{"id":"req-1","status":"cancelled"}"#)
            .expect("the queued answer decodes");
        assert!(
            !queued.cancelling,
            "a queued request has no rental, so there was nothing to signal"
        );
        assert_eq!(
            queued.label(),
            "cancelled",
            "the handler already ended it, and the server's own word reaches the \
             operator before the two-second poll does"
        );

        // AND THE THREE WORDS THE CELL CAN SAY, which are three states and
        // not two: an empty cell between the click and the answer reads as
        // nothing having happened, and a placeholder spelled the same as the
        // accepted answer would make the two indistinguishable from outside --
        // so a page that stopped reading the body would still look right.
        assert_eq!(
            Stopping {
                id: "req-1".into(),
                answer: None
            }
            .label(),
            "asking",
            "this page has asked and the server has not answered"
        );
        assert_eq!(
            Stopping {
                id: "req-1".into(),
                answer: Some(generating)
            }
            .label(),
            "cancelling"
        );
        assert_eq!(
            Stopping {
                id: "req-1".into(),
                answer: Some(queued)
            }
            .label(),
            "cancelled"
        );

        // A 200 THAT SAYS NOTHING reads as the flag being set, because
        // claiming a request ended when it did not is the error that leaves a
        // card billing unwatched.
        assert_eq!(Cancelled::default().label(), "cancelling");
    }

    /// A cancel marks the job it reached and says how many never started.
    ///
    /// The drop count is the half an operator pressed the button for: the
    /// jobs that will not run have no rows, because no `unit_started` ever
    /// named them, so a watcher that only marked the in-flight job would show
    /// one cancelled row and nothing about the other seven.
    #[test]
    fn a_cancel_marks_the_job_it_reached_and_counts_what_was_dropped() {
        let mut p = Progress::default();

        let mut started = ev(1, "unit_started");
        started.unit = "job".into();
        started.index = 3;
        started.total = 8;
        p.apply(&started);

        let mut cancelled = ev(2, "job_cancelled");
        cancelled.unit = "job".into();
        cancelled.index = 3;
        cancelled.total = 8;
        p.apply(&cancelled);

        assert_eq!(p.jobs.len(), 1);
        assert!(p.jobs[0].cancelled);
        assert!(p.jobs[0].cancel_error.is_empty());
        assert!(!p.jobs[0].done, "a stopped job produced no images");
        assert_eq!(p.dropped, 5);
    }

    /// A cancel the server could not honour says what it will still cost.
    ///
    /// `error` set means the generation is still running on a card that is
    /// still billing, and the row has to say so rather than reading as
    /// stopped -- which is the whole reason there is one kind with an error
    /// field instead of two kinds a reader could handle one of.
    #[test]
    fn a_cancel_that_could_not_be_honoured_keeps_its_reason() {
        let mut p = Progress::default();

        let mut cancelled = ev(1, "job_cancelled");
        cancelled.unit = "job".into();
        cancelled.index = 1;
        cancelled.total = 2;
        cancelled.error = "this sd-server build does not report features.cancel_generating".into();
        p.apply(&cancelled);

        assert!(p.jobs[0].cancelled);
        assert!(p.jobs[0].cancel_error.contains("cancel_generating"));
        assert_eq!(p.dropped, 1);
    }

    /// A cancel with no unit belongs to no row.
    ///
    /// It lands in the gap between one job finishing and the next starting,
    /// and `index` is then the count that FINISHED. Folding it into a row
    /// would mark the last completed job -- the one whose image is in the
    /// gallery -- as cancelled.
    #[test]
    fn a_cancel_between_jobs_marks_no_row() {
        let mut p = Progress::default();

        let mut images = ev(1, "job_images");
        images.unit = "job".into();
        images.index = 2;
        images.total = 5;
        images.files = 1;
        p.apply(&images);

        let mut cancelled = ev(2, "job_cancelled");
        cancelled.index = 2;
        cancelled.total = 5;
        p.apply(&cancelled);

        assert_eq!(p.jobs.len(), 1);
        assert!(p.jobs[0].done);
        assert!(
            !p.jobs[0].cancelled,
            "job 2 completed; it was not cancelled"
        );
        assert_eq!(p.dropped, 3);
    }

    /// A stopped job has no frame coming, so its last latent leaves the
    /// screen.
    ///
    /// Without the `!cancelled` half of `sampling`, the half-sampled preview
    /// of the job that was cancelled stays up under "in progress" for the
    /// rest of the rental -- a picture of a generation that is not happening,
    /// which is the preview caption defect in a different place.
    #[test]
    fn a_cancelled_job_has_no_frame_coming() {
        let mut p = Progress::default();

        let mut prog = ev(1, "job_progress");
        prog.unit = "job".into();
        prog.index = 1;
        prog.total = 2;
        prog.step = 3;
        prog.total_steps = 20;
        p.apply(&prog);

        // Mid-sampling: the frame is what the operator is watching.
        assert_eq!(p.sampling().len(), 1);

        let mut cancelled = ev(2, "job_cancelled");
        cancelled.unit = "job".into();
        cancelled.index = 1;
        cancelled.total = 2;
        p.apply(&cancelled);

        assert!(
            p.sampling().is_empty(),
            "a cancelled job still offers a progress frame, so a stale latent stays on screen"
        );
    }

    /// "destroy failed; the node may still be running and billing" is the one
    /// line an operator must not miss, so it is kept where the UI can shout.
    #[test]
    fn keeps_a_destroy_that_failed() {
        let mut p = Progress::default();
        let mut e = ev(1, "destroy_failed");
        e.error = "provider unreachable".into();
        e.node = "local/n1".into();
        p.apply(&e);
        assert_eq!(p.failure, "provider unreachable");
    }

    /// A failed request's reason is readable, and it does not travel through
    /// `Progress::failure`.
    ///
    /// The field had no reader at all: deleting it left the crate compiling
    /// and every test green, because serde drops what nothing asks for. What
    /// this fixes in place is the two halves staying apart -- a request
    /// refused by a money ceiling never had a node, so the destroy sentence
    /// `Progress::failure` is rendered with must not be what carries it.
    #[test]
    fn a_requests_reason_is_its_own_and_not_the_destroy_sentence() {
        let refused = Request {
            status: "failed".into(),
            error: "max_month_usd would be exceeded: $47.80 spent of $50 \
                    and this rental projects $3.58"
                .into(),
            ..Default::default()
        };
        assert!(refused.reason().unwrap().contains("max_month_usd"));

        // Nothing in the event stream put it there, which is the point: a
        // request the planner refused has no sink and emits nothing, so a
        // watcher folding only events has no reason to show.
        let p = Progress::default();
        assert!(
            p.failure.is_empty(),
            "the request's reason reached the field whose view says the node may still be billing"
        );
    }

    /// Not gated on `failed`, because a cancelled request's reason is the one
    /// with money in it.
    ///
    /// `queue.finish` writes this same column for a cancel, and the sentence
    /// it writes is "the job in flight cannot be stopped, so it will finish
    /// and be paid for". A status check here would hide exactly that.
    #[test]
    fn a_cancelled_request_still_has_a_reason() {
        let stopped = Request {
            status: "cancelled".into(),
            error: "job 3/8: cannot be stopped, so it will finish and be paid for".into(),
            ..Default::default()
        };
        assert!(stopped.reason().unwrap().contains("paid for"));

        // And a request that ended with nothing to explain says nothing. An
        // empty line under a `done` row would be the money row of zeros one
        // panel over: a reader assumes a reason was withheld.
        let done = Request {
            status: "done".into(),
            ..Default::default()
        };
        assert!(done.reason().is_none());
    }

    /// An unbounded log is a tab that grows until the browser stops: a `-v`
    /// sd-server prints a line per tensor.
    #[test]
    fn the_log_is_bounded() {
        let mut p = Progress::default();
        for i in 0..(MAX_LOG * 2) {
            let mut e = ev(i as i64 + 1, "log");
            e.text = format!("line {i}\n");
            p.apply(&e);
        }
        assert_eq!(p.log.len(), MAX_LOG);
        // The newest are kept: the last thing the node said is the thing that
        // explains a failure.
        assert!(p.log_text().contains(&format!("line {}", MAX_LOG * 2 - 1)));
        assert!(!p.log_text().contains("line 0\n"));
    }

    /// A kind this build has never heard of is ignored, not a panic.
    ///
    /// Events are persisted, so a database written by a newer ember can hand
    /// this one a kind that did not exist when it was compiled -- and reading
    /// old history is exactly when an operator least wants a crash.
    #[test]
    fn ignores_a_kind_from_a_newer_ember() {
        let mut p = Progress::default();
        p.apply(&ev(1, "something_a_later_ember_added"));
        assert_eq!(p.cursor, 1);
    }

    /// Money is never formatted here. `node.MicroUSD.String` is `"$%.6g"`,
    /// which nothing in Rust reproduces by accident.
    #[test]
    fn money_is_displayed_as_go_rendered_it() {
        let m = Money {
            micro_usd: 289_999,
            display: "$0.289999".into(),
        };
        assert_eq!(m.text(), "$0.289999");
        assert!(!m.is_free());
        assert_eq!(Money::default().text(), "$0");
        assert!(Money::default().is_free());
    }

    #[test]
    fn describes_a_job_the_way_ember_does() {
        let j = Job {
            prompt: "a cat".into(),
            width: 512,
            height: 256,
            steps: 4,
            seed: 9,
            ..Default::default()
        };
        let got = j.describe();
        for want in ["\"a cat\"", "512x256", "4 steps", "seed 9"] {
            assert!(got.contains(want), "{got:?} is missing {want:?}");
        }

        // Only one side set: the other would come from the server's default,
        // and "512x" would be a guess printed as a fact.
        let half = Job {
            prompt: "a".into(),
            width: 512,
            ..Default::default()
        };
        assert!(!half.describe().contains("512"));

        // A FLUX.2 prompt is a thousand characters and the first words are
        // what identifies it.
        let long = Job {
            prompt: "x".repeat(200),
            ..Default::default()
        };
        assert!(long.describe().len() < 80);
        assert!(long.describe().contains("..."));
    }

    /// A UI must not offer a paid card on a server that will refuse it: the
    /// refusal would arrive after the operator chose.
    #[test]
    fn knows_which_providers_can_be_rented_from() {
        let cfg = Config {
            providers: vec![
                Provider {
                    name: "local".into(),
                    free: true,
                    stores: true,
                },
                Provider {
                    name: "runpod".into(),
                    free: false,
                    stores: true,
                },
            ],
            allow_paid: false,
            ..Default::default()
        };
        assert!(cfg.can_rent("local"));
        assert!(!cfg.can_rent("runpod"));
        assert!(!cfg.can_rent("nowhere"));

        let paid = Config {
            allow_paid: true,
            ..cfg.clone()
        };
        assert!(paid.can_rent("runpod"));
    }

    #[test]
    fn a_request_knows_what_can_be_done_to_it() {
        let queued = Request {
            status: "queued".into(),
            ..Default::default()
        };
        assert!(queued.is_cancellable());
        assert!(!queued.is_terminal());

        // A RUNNING ONE IS CANCELLABLE TOO, which this test asserted the
        // opposite of. The old reason -- that it would destroy the node and
        // take every other request on the rental -- was true of cancelling a
        // rental and false of cancelling a job, which is what
        // `POST /sdcpp/v1/jobs/{id}/cancel` does.
        let running = Request {
            status: "running".into(),
            ..Default::default()
        };
        assert!(running.is_cancellable());
        assert!(!running.is_terminal());

        for s in ["done", "failed", "cancelled", "interrupted"] {
            let r = Request {
                status: s.into(),
                ..Default::default()
            };
            assert!(r.is_terminal(), "{s} is not terminal");
            assert!(!r.is_cancellable());
        }
    }

    /// A gallery must not open on a latent: a `proj` preview of a 64x64
    /// generation is an 8x8 PNG of about 250 bytes.
    #[test]
    fn results_leave_out_the_preview_frames() {
        let r = Request {
            images: vec![
                ImageRef {
                    job: 1,
                    index: 0,
                    path: "/a".into(),
                    media: "image/png".into(),
                    preview: false,
                    bytes: 2048,
                },
                ImageRef {
                    job: 1,
                    index: 0,
                    path: "/p".into(),
                    media: "image/png".into(),
                    preview: true,
                    bytes: 250,
                },
            ],
            ..Default::default()
        };
        let got: Vec<_> = r.results().map(|i| i.path.clone()).collect();
        assert_eq!(got, vec!["/a".to_string()]);
    }

    /// A posted job leaves out what the operator did not set, so the server's
    /// own defaults apply rather than a zero this UI invented.
    #[test]
    fn a_posted_job_omits_what_was_not_set() {
        let body = serde_json::to_string(&NewRequest {
            provider: "local".into(),
            jobs: vec![Job {
                prompt: "a cat".into(),
                steps: 4,
                ..Default::default()
            }],
            ..Default::default()
        })
        .unwrap();

        assert!(body.contains("\"prompt\":\"a cat\""));
        assert!(body.contains("\"steps\":4"));
        // Not sent: the server would read a zero as a choice.
        assert!(!body.contains("width"));
        assert!(!body.contains("seed"));
        assert!(!body.contains("sampler"));
        // And nothing that could carry a file path on the node.
        assert!(!body.contains("extra"));
    }

    /// The optional parameters are sent when set and absent when not.
    ///
    /// `Some(0)` is the case worth pinning: the server's own default for
    /// clip_skip is -1 and for eta is null, so a zero the operator typed has
    /// to reach the wire while an untouched field does not. An `i32` with a
    /// zero-is-unset rule -- which is right for every field above -- would be
    /// wrong here and would look identical in the struct.
    #[test]
    fn a_zero_the_operator_typed_is_sent_and_an_empty_box_is_not() {
        let body = serde_json::to_string(&Job {
            prompt: "a cat".into(),
            clip_skip: Some(0),
            eta: Some(0.0),
            ..Default::default()
        })
        .unwrap();
        assert!(body.contains("\"clip_skip\":0"), "{body}");
        assert!(body.contains("\"eta\":0.0"), "{body}");

        let bare = serde_json::to_string(&Job {
            prompt: "a cat".into(),
            ..Default::default()
        })
        .unwrap();
        assert!(!bare.contains("clip_skip"), "{bare}");
        assert!(!bare.contains("eta"), "{bare}");
        // And the three blocks are absent rather than empty objects, so an
        // ordinary job posts the body it posted before they existed.
        assert!(!bare.contains("slg"), "{bare}");
        assert!(!bare.contains("hires"), "{bare}");
        assert!(!bare.contains("vae_tiling"), "{bare}");
    }

    /// An unset parameter reads as unset, a missing block reads as off, and
    /// an explicit zero reads as zero.
    ///
    /// THE THREE ARE SPELLED DIFFERENTLY BECAUSE THEY ARE DIFFERENT CLAIMS,
    /// and the whole value of this page is that it says what a generation ran
    /// with rather than what a struct's zero value is. `clip_skip: Some(0)`
    /// is a model being told zero; `None` is the server's own -1; a `hires`
    /// of `None` is a highres pass that was off. A page that printed `0` for
    /// all three would state two values nobody chose.
    #[test]
    fn an_unset_parameter_is_not_a_zero_one() {
        let bare = Job {
            prompt: "a paper boat".into(),
            ..Default::default()
        }
        .settings();
        let find = |label: &str| -> String {
            bare.iter()
                .find(|s| s.label == label)
                .unwrap_or_else(|| panic!("no {label} row in {bare:?}"))
                .value
                .clone()
        };
        assert_eq!(find("clip skip"), UNSET);
        assert_eq!(find("eta"), UNSET);
        assert_eq!(find("flow shift"), UNSET);
        assert_eq!(find("skip-layer guidance"), OFF);
        assert_eq!(find("highres pass"), OFF);
        assert_eq!(find("vae tiling"), OFF);
        // A seed of zero is not a seed: omitempty means the browser sent
        // none, so a reproduction of this request is not available and the
        // row says so instead of printing a number nobody chose.
        assert!(find("seed").contains("not pinned"), "{bare:?}");
        // And a parameter a text-to-image job cannot have is not asked
        // about, because `unset` there is a question with no answer.
        assert!(!bare.iter().any(|s| s.label == "strength"), "{bare:?}");

        let set = Job {
            prompt: "a paper boat".into(),
            seed: 42,
            sampler: "euler_a".into(),
            scheduler: "karras".into(),
            clip_skip: Some(0),
            eta: Some(0.0),
            flow_shift: Some(3.5),
            hires: Some(Hires {
                enabled: true,
                upscaler: "Latent".into(),
                steps: 10,
                ..Default::default()
            }),
            slg: Some(Slg {
                layers: vec![7, 8],
                scale: 2.5,
                ..Default::default()
            }),
            ..Default::default()
        }
        .settings();
        let find = |label: &str| -> String {
            set.iter()
                .find(|s| s.label == label)
                .unwrap_or_else(|| panic!("no {label} row in {set:?}"))
                .value
                .clone()
        };
        assert_eq!(find("seed"), "42");
        assert_eq!(find("sampler"), "euler_a");
        assert_eq!(find("scheduler"), "karras");
        // The two that would be indistinguishable from unset if zero meant
        // unset, which is the entire reason both are pointers.
        assert_eq!(find("clip skip"), "0");
        assert_eq!(find("eta"), "0");
        assert_eq!(find("flow shift"), "3.5");
        assert!(find("highres pass").contains("Latent"), "{set:?}");
        assert!(find("skip-layer guidance").contains("7, 8"), "{set:?}");
        // Still off, because the one block that was not sent is the one
        // whose absence must not read as the two that were.
        assert_eq!(find("vae tiling"), OFF);
        // An `enabled: false` block is off too: ember sends the block for
        // the other fields, and upstream's own switch is `enabled`.
        let disabled = Job {
            hires: Some(Hires {
                enabled: false,
                upscaler: "Latent".into(),
                ..Default::default()
            }),
            ..Default::default()
        }
        .settings();
        assert_eq!(
            disabled
                .iter()
                .find(|s| s.label == "highres pass")
                .unwrap()
                .value,
            OFF
        );
    }

    /// A block that is set travels whole, under the names ember accepts.
    ///
    /// The names here are ember's own and not upstream's -- `vae_tiling`
    /// rather than `vae_tiling_params`, `slg` at the top of the job rather
    /// than inside `sample_params.guidance` -- so a rename on either side
    /// would make the page post fields the server silently ignores. The Go
    /// side has its own test that each one lands where upstream names it;
    /// this is the half that checks the page and the handler agree.
    #[test]
    fn a_block_that_is_set_travels_whole() {
        let body = serde_json::to_string(&Job {
            prompt: "a cat".into(),
            hires: Some(Hires {
                enabled: true,
                upscaler: "Latent".into(),
                scale: 2.0,
                steps: 10,
                denoising_strength: 0.7,
                ..Default::default()
            }),
            vae_tiling: Some(VaeTiling {
                enabled: true,
                tile_size_w: 512,
                ..Default::default()
            }),
            slg: Some(Slg {
                layers: vec![7, 8, 9],
                layer_start: 0.01,
                layer_end: 0.2,
                scale: 2.5,
            }),
            ..Default::default()
        })
        .unwrap();
        for want in [
            "\"hires\":{",
            "\"enabled\":true",
            "\"upscaler\":\"Latent\"",
            "\"denoising_strength\":0.7",
            "\"vae_tiling\":{",
            "\"tile_size_w\":512",
            "\"slg\":{",
            "\"layers\":[7,8,9]",
            "\"scale\":2.5",
        ] {
            assert!(body.contains(want), "{want} missing from {body}");
        }

        // And it decodes back, because the same type is what a request view
        // returns: a job on screen has to be the job that was queued.
        let back: Job = serde_json::from_str(&body).unwrap();
        assert_eq!(back.hires.unwrap().steps, 10);
        assert_eq!(back.slg.unwrap().layers, vec![7, 8, 9]);
        assert_eq!(back.vae_tiling.unwrap().tile_size_w, 512);
    }

    /// The server sends fields this build may not know; decoding must not
    /// fail on them, or a newer ember breaks an older page.
    /// `null` where a list is expected must not drop the whole message.
    ///
    /// THIS WAS A REAL BUG AND ONLY A BROWSER FOUND IT. Go marshals a nil
    /// slice as `null`, and `#[serde(default)]` covers a MISSING field rather
    /// than a null one -- so the first envelope of every websocket
    /// connection, which carries no events by design, failed to decode
    /// entirely and took the request state with it. The UI showed a job stuck
    /// at "starting" while the image was already on disk.
    ///
    /// The server sends `[]` now. This is the other half, because the rule
    /// has to hold for a field somebody adds later too.
    #[test]
    fn a_null_list_is_an_empty_list() {
        // `next` is in the body and is not a field of this type: the server
        // sends it and this page does not decode it, so serde has to ignore
        // it rather than refuse the envelope that carries it.
        let env: Envelope =
            serde_json::from_str(r#"{"next":4,"events":null,"request":null}"#).unwrap();
        assert!(env.events.is_empty());
        assert!(env.request.is_none());

        let r: Request = serde_json::from_str(r#"{"id":"a","jobs":null,"images":null}"#).unwrap();
        assert!(r.jobs.is_empty());
        assert!(r.images.is_empty());

        let c: Config = serde_json::from_str(r#"{"providers":null,"caps":null}"#).unwrap();
        assert!(c.providers.is_empty());
        assert!(c.caps.is_none());

        let o: Offers = serde_json::from_str(r#"{"listings":null}"#).unwrap();
        assert!(o.listings.is_empty());

        let l: Listing = serde_json::from_str(r#"{"provider":"local","offers":null}"#).unwrap();
        assert!(l.offers.is_empty());
    }

    /// The bytes Go sends, not bytes somebody typed here.
    ///
    /// NOTHING ANYWHERE TOOK GO'S OUTPUT AND ASKED WHETHER RUST COULD READ
    /// IT. The wire tests in this module feed JSON written by hand, whose
    /// numbers are all small; the Go suite marshals and asserts on Go types,
    /// where a width of three billion is fine; and `TestServe_UI` drives the
    /// page, where that value cannot be entered, because the control is an
    /// `<input type=number>` read into this same `i32`. So a Go `int` against
    /// a Rust `i32` -- the same literal in JSON and different types on either
    /// side -- survived every test in two languages, which is the closed loop
    /// `capabilities.model` sat in with an API on the other end instead of a
    /// compiler.
    ///
    /// This file is produced by
    /// `TestRequests_AListingWithARowTheBrowserCannotRead` in
    /// `internal/webapi`, from the handler, and that test fails if the
    /// handler stops sending what is in it. `include_str!` is the other half:
    /// a cargo build cannot miss the file.
    #[test]
    fn a_row_go_sent_and_this_page_cannot_read_does_not_take_the_list_with_it() {
        const LIST: &str =
            include_str!("../../internal/webapi/testdata/request_list_unrepresentable.json");

        // THE NEGATIVE CONTROL FIRST: the whole envelope is what used to go,
        // so an assertion that the healthy row survived means nothing unless
        // the oversized one really is unreadable.
        let one: Result<Request, _> = serde_json::from_value(
            serde_json::from_str::<serde_json::Value>(LIST).unwrap()["requests"][1].clone(),
        );
        assert!(
            one.is_err(),
            "the fixture no longer carries a value this page cannot hold, so every \
             assertion below would pass against a listing with nothing wrong with it"
        );

        let l: RequestList = serde_json::from_str(LIST).expect("the envelope still decodes");
        assert_eq!(
            l.requests.items.len(),
            1,
            "the healthy request was dropped too"
        );
        assert_eq!(l.requests.items[0].id, "req-healthy");
        assert_eq!(l.requests.dropped, 1);

        // AND IT IS SAID RATHER THAN SWALLOWED. A row that vanished silently
        // is the operator's own request disappearing out of the queue, which
        // is worse than the table being short.
        let note = l.requests.note();
        assert!(note.contains('1'), "note = {note:?}");
        assert!(!note.is_empty());
        assert!(
            Rows::default().note().is_empty(),
            "a clean listing says nothing"
        );

        // AND "nothing queued" IS NOT SAID OVER IT. A listing whose every
        // row was dropped is the state that used to print it, and those
        // requests are queued and about to rent a card.
        assert!(!l.requests.nothing_queued());
        let all_bad = Rows {
            items: Vec::new(),
            dropped: 1,
            trouble: "x".into(),
        };
        assert!(
            !all_bad.nothing_queued(),
            "every row dropped is not an empty queue"
        );
        assert!(
            Rows::default().nothing_queued(),
            "an empty listing is an empty queue"
        );
    }

    #[test]
    fn decodes_a_response_with_fields_it_does_not_know() {
        let json = r#"{"id":"abc","status":"done","cost":{"micro_usd":5,"display":"$5e-06"},
            "something_new":{"nested":true},"jobs":[{"prompt":"a"}]}"#;
        let r: Request = serde_json::from_str(json).unwrap();
        assert_eq!(r.id, "abc");
        assert_eq!(r.cost.text(), "$5e-06");
        assert_eq!(r.jobs.len(), 1);
    }

    /// The three states of a feature flag are three answers, not two.
    ///
    /// `caps` absent is before the first rental, when there is nothing to
    /// check against and every control has to stay usable -- otherwise the
    /// operator's first request is the one that can say least. A flag the
    /// server did not publish is the same answer arrived at differently: some
    /// build, or some cached `caps` row written before ember read these maps,
    /// simply has nothing to say about it. Only a published `false` takes a
    /// control away.
    ///
    /// Written as one test over all three because the bug is picking the
    /// wrong default for the absent cases, and a test that only covered
    /// `Some(false)` would pass against a page that greyed everything.
    #[test]
    fn only_a_published_no_takes_a_control_away() {
        let before_any_rental = Config::default();
        assert!(before_any_rental.caps.is_none());
        assert!(
            before_any_rental.offers(|f| f.clip_skip),
            "a control was greyed out before anything had asked a server what it can do"
        );

        let silent: Config = serde_json::from_str(r#"{"caps":{"model":"sd15.gguf"}}"#).unwrap();
        assert!(
            silent.offers(|f| f.clip_skip),
            "a server that published no feature map greyed a control anyway"
        );

        let yes: Config =
            serde_json::from_str(r#"{"caps":{"features":{"clip_skip":true}}}"#).unwrap();
        assert!(yes.offers(|f| f.clip_skip));

        let no: Config =
            serde_json::from_str(r#"{"caps":{"features":{"clip_skip":false}}}"#).unwrap();
        assert!(
            !no.offers(|f| f.clip_skip),
            "the server said clip_skip is unsupported and the control stayed available"
        );
        // And the answer is per flag: one denial must not take the rest with
        // it, which is what reading the map as "all or nothing" would do.
        assert!(
            no.offers(|f| f.slg),
            "one denial greyed an unrelated control"
        );
    }

    /// A greyed control must not post what was typed before it greyed.
    ///
    /// `/api/config` is fetched after the page mounts, so there is a real
    /// window in which the advanced section is open, typeable and ungated.
    /// Values from that window live in the draft and in any job already added
    /// to the batch, and greying the control afterwards does not remove them
    /// -- the request would then carry a parameter whose control is visibly
    /// unavailable. ember's own enqueue refuses it, which means the page would
    /// be posting a request it knows will be refused.
    ///
    /// The set cleared is the set `Body` would send, which is why an
    /// slg block with scale 0 and a `shifted_timestep` of 0 are not here:
    /// they are already absent from the wire.
    #[test]
    fn a_denied_parameter_is_dropped_from_what_was_already_typed() {
        let f: Features = serde_json::from_str(
            r#"{"clip_skip":false,"flow_shift":false,"shifted_timestep":false,
                "slg":false,"vae_tiling":false,"hires":false}"#,
        )
        .unwrap();

        let mut j = Job {
            prompt: "a paper boat".into(),
            clip_skip: Some(2),
            eta: Some(0.0),
            flow_shift: Some(3.0),
            shifted_timestep: 250,
            slg: Some(Slg {
                scale: 2.5,
                ..Default::default()
            }),
            vae_tiling: Some(VaeTiling {
                enabled: true,
                ..Default::default()
            }),
            hires: Some(Hires {
                enabled: true,
                ..Default::default()
            }),
            ..Default::default()
        };
        j.drop_denied(&f);

        let body = serde_json::to_string(&j).unwrap();
        for gone in [
            "clip_skip",
            "flow_shift",
            "shifted_timestep",
            "slg",
            "vae_tiling",
            "hires",
        ] {
            assert!(!body.contains(gone), "{gone} survived in {body}");
        }
        // The prompt and the ungated parameter are untouched. A clear that
        // emptied the job would pass the assertions above and lose the
        // request.
        assert_eq!(j.prompt, "a paper boat");
        assert_eq!(j.eta, Some(0.0), "eta has no flag and must not be cleared");

        // And nothing is dropped when the server said yes, or said nothing.
        let mut kept = j.clone();
        kept.clip_skip = Some(2);
        kept.hires = Some(Hires {
            enabled: true,
            ..Default::default()
        });
        let before = kept.clone();
        kept.drop_denied(&Features::default());
        assert_eq!(kept, before, "a silent server had a parameter dropped");
    }

    /// The image reference and its strength travel together, both ways.
    ///
    /// THE PAIR IS THE POINT. ember refuses a strength with no image -- a
    /// live server accepts that combination, generates and reports the
    /// strength nowhere -- so a page that could post one without the other
    /// would be posting a request it knows will be refused. Checked in both
    /// directions: set, they are both on the wire; unset, neither is; denied,
    /// both go.
    #[test]
    fn an_image_and_its_strength_are_sent_together_or_not_at_all() {
        let bare = serde_json::to_string(&Job {
            prompt: "a paper boat".into(),
            ..Default::default()
        })
        .unwrap();
        assert!(!bare.contains("init_image"), "{bare}");
        assert!(!bare.contains("strength"), "{bare}");

        let set = serde_json::to_string(&Job {
            prompt: "a paper boat".into(),
            init_image: "sha256:abc".into(),
            // Zero, which is the value a zero-is-unset rule would lose: the
            // server's own default is 0.75 and it echoes an explicit 0 back.
            strength: Some(0.0),
            ..Default::default()
        })
        .unwrap();
        assert!(set.contains(r#""init_image":"sha256:abc""#), "{set}");
        assert!(set.contains(r#""strength":0.0"#), "{set}");

        // And a server that says no to an image takes the strength with it.
        let no: Features = serde_json::from_str(r#"{"init_image":false}"#).unwrap();
        let mut j = Job {
            prompt: "a paper boat".into(),
            init_image: "sha256:abc".into(),
            strength: Some(0.4),
            ..Default::default()
        };
        j.drop_denied(&no);
        assert_eq!(j.init_image, "");
        assert_eq!(
            j.strength, None,
            "the image was dropped and the strength was left, which ember refuses"
        );
        assert_eq!(j.prompt, "a paper boat");

        // A server that published nothing about it keeps both: absent is not
        // a denial, and treating it as one would make the picker unusable
        // against every build that does not publish the map.
        let mut kept = Job {
            init_image: "sha256:abc".into(),
            strength: Some(0.4),
            ..Default::default()
        };
        let before = kept.clone();
        kept.drop_denied(&Features::default());
        assert_eq!(kept, before, "a silent server had the image dropped");
    }

    /// The picker's own line, which is the only way to tell staged from
    /// refused.
    #[test]
    fn a_staged_image_describes_itself() {
        let i = Input {
            id: "sha256:abc".into(),
            format: "jpeg".into(),
            width: 1024,
            height: 768,
            bytes: 2 * 1024 * 1024,
        };
        assert_eq!(i.describe(), "jpeg 1024x768, 2.0 MiB");
    }

    /// Mebibytes, because the bound the server enforces is written in them.
    #[test]
    fn sizes_are_in_the_units_the_bound_is() {
        assert_eq!(human_bytes(0), "0 B");
        assert_eq!(human_bytes(344), "344 B");
        assert_eq!(human_bytes(2048), "2 KiB");
        assert_eq!(human_bytes(8 * 1024 * 1024), "8.0 MiB");
    }

    #[test]
    fn durations_read_like_gos() {
        assert_eq!(human_duration(0), "0s");
        assert_eq!(human_duration(551_000_000), "551ms");
        assert_eq!(human_duration(6_040_000_000), "6.0s");
        assert_eq!(human_duration(140_800_000_000), "2m21s");
    }

    fn finding(kind: &str, provider: &str, id: &str) -> Finding {
        Finding {
            kind: kind.into(),
            provider: provider.into(),
            id: id.into(),
            reason: "because".into(),
            ..Default::default()
        }
    }

    /// A sweep that found nothing says so, and it has to be ALLOWED to.
    ///
    /// The negative control for every assertion below: without it, a verdict
    /// that said "not the whole picture" unconditionally would pass them all.
    #[test]
    fn a_clean_sweep_says_nothing_is_running() {
        let n = Nodes::default();
        assert!(n.nothing_running());
        assert_eq!(
            n.verdict(),
            "nothing is running and nothing was left behind"
        );
        assert!(n.unreachable().is_empty());
    }

    /// "I could not check RunPod" must never render as a clean bill of health.
    ///
    /// The rule `ember ls` exits non-zero for, in the panel that is now the
    /// only path a browser has to it. The offers panel has the same rule one
    /// panel over and it was defeated there by a `<For>` key, which is why
    /// the decision is a method with a test rather than a branch in a view.
    #[test]
    fn an_unreachable_provider_is_not_a_clean_bill_of_health() {
        let n = Nodes {
            findings: vec![finding("unreachable", "runpod", "")],
            attention: 1,
            ..Default::default()
        };
        assert!(
            !n.nothing_running(),
            "a sweep that could not ask a provider claimed nothing is running"
        );
        assert_eq!(n.unreachable(), vec!["runpod".to_string()]);
        let v = n.verdict();
        assert!(v.contains("runpod"), "the verdict does not name it: {v}");
        assert!(
            v.contains("not the whole picture"),
            "the verdict reads as complete: {v}"
        );

        // AND IT IS NAMED IN THE ROW TOO. An unreachable finding has no node,
        // no record, no store and no leftover, so its id is empty -- a table
        // built from the id alone would say "could not be asked" without
        // saying whom.
        assert_eq!(n.findings[0].names(), "runpod");
    }

    /// A provider can fail one listing and answer another, and the store
    /// listing is the one that holds the monthly charge.
    ///
    /// THE CASE THE FIRST VERSION OF `nothing_running` WOULD HAVE MISSED: a
    /// sweep whose node listing came back empty and whose STORE listing
    /// failed has nothing in `findings` and nothing in `leftovers`, so a
    /// check over those two alone prints the clean sentence over a provider
    /// ember never reached. `reconcile.ScanLeftovers` names this case in as
    /// many words.
    #[test]
    fn a_failure_in_one_listing_is_not_silence_in_the_others() {
        let n = Nodes {
            stores: vec![finding("unreachable", "runpod", "")],
            attention: 1,
            ..Default::default()
        };
        assert!(n.findings.is_empty() && n.leftovers.is_empty());
        assert!(
            !n.nothing_running(),
            "a failed store listing read as a clean sweep"
        );
        assert_eq!(n.unreachable(), vec!["runpod".to_string()]);
    }

    /// Both facts, because the operator needs both.
    ///
    /// An expired node here and an unreachable provider there are two things
    /// to act on. An enum would have forced one of them off the screen, and
    /// the count alone would have read as the whole picture.
    #[test]
    fn the_count_and_the_gap_are_both_said() {
        let n = Nodes {
            findings: vec![
                finding("expired", "local", "local/n1"),
                finding("unreachable", "runpod", ""),
            ],
            attention: 2,
            ..Default::default()
        };
        let v = n.verdict();
        assert!(v.contains('2'), "the count is missing: {v}");
        assert!(v.contains("runpod"), "the gap is missing: {v}");
    }

    /// A provider that failed two listings is named once.
    #[test]
    fn a_provider_is_named_once_however_many_listings_it_failed() {
        let n = Nodes {
            findings: vec![finding("unreachable", "runpod", "")],
            leftovers: vec![finding("unreachable", "runpod", "")],
            stores: vec![finding("unreachable", "runpod", "")],
            attention: 3,
            ..Default::default()
        };
        assert_eq!(n.unreachable(), vec!["runpod".to_string()]);
    }

    /// A node that is behaving and a store that exists are rows with nothing
    /// to act on.
    ///
    /// `reconcile.Kind.NeedsAttention` exempts exactly those two, because
    /// making every store exit non-zero would teach the operator to ignore
    /// the count. The panel must not then print the clean sentence, because
    /// there IS a node running -- it is simply fine.
    #[test]
    fn a_healthy_node_is_listed_without_being_a_finding() {
        let n = Nodes {
            findings: vec![finding("ok", "local", "local/n1")],
            stores: vec![finding("store", "runpod", "runpod/vol1")],
            attention: 0,
            ..Default::default()
        };
        assert!(!n.nothing_running());
        assert_eq!(n.verdict(), "nothing needs attention");
    }

    /// `null` is not an empty list, and three slices here means three chances.
    ///
    /// Go marshals a nil slice as `null`, `#[serde(default)]` covers a field
    /// that is MISSING rather than one that is present and null, and this
    /// page has already lost a whole websocket envelope to exactly that. The
    /// server sends `[]`; this is the other half, because the rule has to
    /// hold for a field somebody adds later.
    #[test]
    fn a_null_list_decodes_as_an_empty_one() {
        let n: Nodes = serde_json::from_str(
            r#"{"findings":null,"leftovers":null,"stores":null,
                "monthly_storage":{"micro_usd":0,"display":"$0"},"attention":0}"#,
        )
        .expect("an envelope of nulls decodes");
        assert!(n.findings.is_empty() && n.leftovers.is_empty() && n.stores.is_empty());
        assert!(n.nothing_running());
    }

    /// The money the panel exists for, read and never formatted here.
    #[test]
    fn the_standing_charge_is_the_servers_own_rendering() {
        let n: Nodes = serde_json::from_str(
            r#"{"findings":[],"leftovers":[],
                "stores":[{"kind":"store","provider":"runpod","id":"runpod/vol1",
                           "reason":"a store","monthly":{"micro_usd":7000000,"display":"$7"}}],
                "monthly_storage":{"micro_usd":7000000,"display":"$7"},"attention":0}"#,
        )
        .expect("a priced store decodes");
        assert_eq!(n.monthly_storage.text(), "$7");
        assert_eq!(n.stores[0].monthly.as_ref().expect("priced").text(), "$7");
        // Accrued and monthly are different claims. A store has the second
        // and not the first.
        assert!(n.stores[0].accrued.is_none());
    }

    // --- speech ---

    /// A config publishing voices, as `/api/config` sends one.
    fn speaking() -> Config {
        serde_json::from_str(
            r#"{"providers":[],"kinds":["sd","tts"],
                "voices":[{"id":"lessac","name":"Lessac (en_US, medium)"},
                          {"id":"thorsten","name":"Thorsten (de_DE, high)"}]}"#,
        )
        .expect("a config with a catalogue decodes")
    }

    /// Every arm of the control-set decision.
    #[test]
    fn a_kind_decides_which_controls_the_panel_shows() {
        let c = speaking();

        // The image controls, and an absent kind is an image generation --
        // which is `genstore.KindOf`'s rule and is what every body this page
        // posted before speech existed means.
        assert_eq!(c.composing(""), Composing::Image);
        assert_eq!(c.composing("sd"), Composing::Image);

        // The speech controls, carrying the list the chooser needs. The list
        // is in the value so that a view cannot reach the chooser without
        // something to put in it.
        match c.composing("tts") {
            Composing::Speech(v) => {
                assert_eq!(v.len(), 2);
                assert_eq!(v[0].id, "lessac");
            }
            other => panic!("a tts kind with voices published gave {other:?}"),
        }
    }

    /// A process that publishes no voices refuses speech and names the key.
    ///
    /// NOT AN EMPTY CHOOSER, which is the defect this arm exists for: a
    /// `<select>` with no options posts an empty `voice_id`, the server
    /// refuses it after the operator has typed their text, and the control
    /// looked like it worked the whole time.
    /// THE FIXTURE CARRIES A PROVIDER, and that is the whole of what changed
    /// here. "ember has not answered" is keyed on `providers` rather than on
    /// `kinds`, because the server now narrows `kinds` to what the
    /// configuration can run and `[]` became a real answer. A fixture with no
    /// providers is an unanswered config, so without this it was asserting
    /// about the wait and not about the refusal.
    ///
    /// The arm itself is now reachable only by a client that did not take its
    /// kind from `kinds` -- a `?kind=` in a URL, or a page older than the
    /// server -- because the selector no longer offers `tts` to a process
    /// publishing no voices. It stays because that is the second mechanism,
    /// and because a refusal naming the key is what an operator needs in the
    /// one case where they typed the kind themselves.
    #[test]
    fn a_process_with_no_voices_says_so_rather_than_offering_an_empty_chooser() {
        let c: Config =
            serde_json::from_str(r#"{"providers":[{"name":"local"}],"kinds":["sd"],"voices":[]}"#)
                .expect("a config publishing none decodes");
        match c.composing("tts") {
            Composing::Refused(why) => {
                assert!(
                    why.contains("tts.voices"),
                    "the refusal does not name the key: {why}"
                );
                assert!(
                    why.contains("restart"),
                    "the refusal names no remedy: {why}"
                );
            }
            other => panic!("a tts kind with no voices gave {other:?}"),
        }
    }

    /// A CONFIG THAT HAS NOT ANSWERED IS A WAIT, AND AN ANSWER OF `[]` IS NOT.
    ///
    /// This is the pair the discriminator exists for, and it is the thing that
    /// had to be true before the server could narrow `kinds` at all. While
    /// "unanswered" meant `kinds.is_empty()`, a configuration able to run
    /// neither kind published `[]` and this page would have told the operator
    /// to keep waiting for a server that had already said everything it had
    /// to say -- for ever, since nothing was going to arrive.
    ///
    /// `providers` carries it because an empty registry is not a state a real
    /// answer can be in: it is built from the config, and `ember serve` cannot
    /// start without a provider. So empty means "no answer yet" and nothing
    /// else, which is the property `kinds` had right up until it stopped being
    /// a constant.
    #[test]
    fn an_unanswered_config_waits_and_a_config_that_runs_nothing_does_not() {
        // Nothing has arrived. The remedy is to wait, so the sentence must
        // not tell anybody to go and edit a file.
        let waiting = Config::default();
        match waiting.composing("tts") {
            Composing::Refused(why) => {
                assert!(
                    why.contains("not said"),
                    "an unanswered config does not read as a wait: {why}"
                );
                assert!(
                    !why.contains("ember.yaml"),
                    "an unanswered config sends the operator to edit a file: {why}"
                );
            }
            other => panic!("an unanswered config gave {other:?}"),
        }

        // ANSWERED, AND THE ANSWER IS THAT IT RUNS NOTHING. `kinds` is empty
        // and that is now information rather than silence, so the remedy is
        // the file and not patience.
        let nothing: Config =
            serde_json::from_str(r#"{"providers":[{"name":"local"}],"kinds":[],"voices":[]}"#)
                .expect("a config that runs nothing decodes");
        match nothing.composing("tts") {
            Composing::Refused(why) => {
                assert!(
                    why.contains("ember.yaml"),
                    "a config that runs nothing reads as a wait: {why}"
                );
            }
            other => panic!("a config that runs nothing gave {other:?}"),
        }
    }

    /// And a config nothing has answered yet is a WAIT and not that refusal.
    ///
    /// THE TWO REFUSALS ARE SEPARATE ASSERTIONS AND NOT ONE `is_refused`,
    /// because the remedies differ: this one is a wait and the other is an
    /// edit to `ember.yaml` and a restart. A test that only checked "speech is
    /// refused" would pass against a page telling an operator whose file is
    /// correct to go and change it.
    ///
    /// `Config::default()` is what the page holds between mounting and
    /// `/api/config` answering. `kinds` cannot be empty in a real response --
    /// `genstore.Kinds()` is derived from nothing, and
    /// `TestConfig_PublishesAtLeastOneKind` is the exit code behind it -- so an
    /// empty list is the unanswered state and nothing else.
    #[test]
    fn an_unanswered_config_is_a_wait_and_not_a_refusal() {
        let c = Config::default();
        match c.composing("tts") {
            Composing::Refused(why) => {
                assert!(why.contains("has not said"), "{why}");
                // AND NOT THE OTHER SENTENCE. An operator whose ember.yaml is
                // correct must not be sent to edit it.
                assert!(
                    !why.contains("tts.voices"),
                    "a wait reads as a misconfiguration: {why}"
                );
            }
            other => panic!("an unanswered config gave {other:?}"),
        }
        // The image controls still show, so the panel is usable in the window
        // -- which is what it did before speech existed.
        assert_eq!(c.composing(""), Composing::Image);
    }

    /// What cannot run is said once per state, and a speech-only config is
    /// not told it runs nothing.
    ///
    /// THE SECOND ARM IS THE CORRECTION AND THE ONE WITH A DEFECT BEHIND IT.
    /// This line was `!servable` and said "a queued request would have nowhere
    /// to run" -- in red, above a compose panel that was about to work, for
    /// the one configuration speech is for. Found by a browser lab, whose own
    /// wait-for-a-refusal helper read it and stopped.
    #[test]
    fn a_speech_only_config_is_not_told_it_runs_nothing() {
        let one = |servable: bool, voices: &str| -> Config {
            serde_json::from_str(&format!(
                r#"{{"providers":[{{"name":"local","free":true}}],
                    "servable":{servable},"kinds":["sd","tts"],"voices":{voices}}}"#
            ))
            .expect("decodes")
        };
        let voiced = r#"[{"id":"lessac","name":"Lessac"}]"#;

        // A servable config says nothing, whether or not it also speaks.
        assert_eq!(one(true, "[]").unrunnable(), None);
        assert_eq!(one(true, voiced).unrunnable(), None);

        // Speech only: the sd half is named and the speech half is said to
        // work, because the whole point of the line is what to expect.
        let why = one(false, voiced)
            .unrunnable()
            .expect("an unservable config says so");
        assert!(why.contains("image request"), "{why}");
        assert!(why.contains("Speech still works"), "{why}");

        // Neither: the original sentence, and it names both keys so the
        // remedy is readable.
        let why = one(false, "[]")
            .unrunnable()
            .expect("a config running nothing says so");
        assert!(why.contains("sd:"), "{why}");
        assert!(why.contains("tts.voices"), "{why}");
        assert!(!why.contains("Speech still works"), "{why}");

        // AND NOTHING BEFORE /api/config HAS ANSWERED. A default Config is
        // not servable and publishes no voices, so an unguarded version
        // shouts "this runs nothing" at every page load.
        assert_eq!(Config::default().unrunnable(), None);
    }

    /// A kind from a newer ember is named rather than rendered as a blank
    /// panel.
    #[test]
    fn a_kind_this_page_has_no_controls_for_is_named() {
        match speaking().composing("video") {
            Composing::Refused(why) => assert!(why.contains("video"), "{why}"),
            other => panic!("an unknown kind gave {other:?}"),
        }
    }

    /// A chooser option always has text to click on.
    #[test]
    fn a_voice_with_no_name_is_offered_by_its_id() {
        assert_eq!(
            Voice {
                id: "lessac".into(),
                name: "Lessac".into()
            }
            .label(),
            "Lessac"
        );
        assert_eq!(
            Voice {
                id: "lessac".into(),
                name: String::new()
            }
            .label(),
            "lessac"
        );
    }

    /// A speech request posts the utterance and NOTHING belonging to the
    /// other kind.
    ///
    /// THIS IS THE ASSERTION THE SERVER'S `crossKind` WOULD OTHERWISE MAKE
    /// FOR US, at the cost of a 400 the operator reads after typing. It
    /// compares a speech job's remaining fields against the zero value, so
    /// one geometry left over from the image draft refuses the whole request
    /// naming an index -- and the panel's draft defaults are 512, 512 and 20,
    /// which are exactly the values that would be left over.
    #[test]
    fn a_speech_request_names_its_kind_and_carries_no_image_parameters() {
        let body = serde_json::to_string(&NewRequest {
            kind: "tts".into(),
            provider: "local".into(),
            jobs: vec![Job {
                tts: Some(Utterance {
                    text: "Hello from ember.".into(),
                    voice_id: "lessac".into(),
                    output: "greeting".into(),
                }),
                ..Default::default()
            }],
            ..Default::default()
        })
        .unwrap();

        assert!(body.contains(r#""kind":"tts""#), "{body}");
        assert!(body.contains(r#""text":"Hello from ember.""#), "{body}");
        assert!(body.contains(r#""voice_id":"lessac""#), "{body}");
        assert!(body.contains(r#""output":"greeting""#), "{body}");

        // The image half, in the spellings crossKind compares against a zero.
        // `prompt` IS sent, as `""`, which is the zero value Go reads -- so it
        // is asserted as present-and-empty rather than absent, because
        // asserting its absence would be asserting a thing that is not true.
        assert!(body.contains(r#""prompt":"""#), "{body}");
        for leftover in [
            "width",
            "height",
            "steps",
            "seed",
            "sampler",
            "scheduler",
            "cfg",
            "guidance",
            "batch_count",
            "init_image",
            "strength",
            "clip_skip",
            "slg",
            "hires",
            "vae_tiling",
        ] {
            assert!(
                !body.contains(leftover),
                "a speech job carries {leftover}, which crossKind refuses: {body}"
            );
        }
        // And no path to a voice, ever: the type has no field for one.
        assert!(!body.contains(".onnx"), "{body}");
    }

    /// An image request is byte-for-byte what it was before `kind` existed.
    ///
    /// The server resolves an absent kind to `sd`, so omitting it is what
    /// keeps every older body valid -- and sending `"kind":"sd"` on every
    /// generation would be a change to a wire nobody asked to change.
    #[test]
    fn an_image_request_still_names_no_kind() {
        let body = serde_json::to_string(&NewRequest {
            provider: "local".into(),
            jobs: vec![Job {
                prompt: "a cat".into(),
                ..Default::default()
            }],
            ..Default::default()
        })
        .unwrap();
        assert!(!body.contains("kind"), "{body}");
        assert!(!body.contains("tts"), "{body}");
    }

    /// An utterance nobody can say is refused here and not after a rental.
    ///
    /// The blank-voice case is the one with a defect behind it: an empty
    /// `<select>` posts `""`, and the server's refusal for that arrives after
    /// the operator has typed and pressed the button.
    #[test]
    fn an_utterance_with_no_text_or_no_voice_is_refused_before_anything_is_rented() {
        let ok = Utterance {
            text: "Hello from ember.".into(),
            voice_id: "lessac".into(),
            output: String::new(),
        };
        assert_eq!(ok.refusal(), None);

        // Whitespace is not text. piper refuses it too, which is the same
        // answer reached after a card has been paid for.
        for blank in ["", "   ", "\n\t "] {
            let u = Utterance {
                text: blank.into(),
                ..ok.clone()
            };
            let why = u.refusal().expect("blank text is refused");
            assert!(why.contains("nothing to say"), "{why}");
        }

        let no_voice = Utterance {
            voice_id: String::new(),
            ..ok.clone()
        };
        let why = no_voice.refusal().expect("a blank voice id is refused");
        assert!(why.contains("voice"), "{why}");
        // And it names where a voice comes from, because the remedy differs
        // from "you forgot to pick one": a process publishing none has an
        // empty list and nothing to pick.
        assert!(why.contains("tts.voices"), "{why}");
    }

    /// A batch row describes an utterance by what was said.
    ///
    /// A speech job is built from `Job::default()`, so the image spelling
    /// would describe every row in the batch as `""` -- a list of identical
    /// rows with a remove button each.
    #[test]
    fn a_batch_row_describes_an_utterance_and_not_an_empty_prompt() {
        let j = Job {
            tts: Some(Utterance {
                text: "Hello from ember.".into(),
                voice_id: "lessac".into(),
                output: String::new(),
            }),
            ..Default::default()
        };
        let d = j.describe();
        assert!(d.contains("Hello from ember."), "{d}");
        assert!(d.contains("lessac"), "{d}");
        assert!(!d.contains("0x0"), "{d}");
    }

    /// A job the server said nothing about is not described in dashes.
    ///
    /// The all-default case is every speech job today, because `viewRequest`
    /// projects a job in the image shape and the view carries no `kind`
    /// either. `settings` pushes seven rows unconditionally, so the block
    /// this guards would be a summary of two dashes over an empty prompt and
    /// seven more below it.
    #[test]
    fn a_job_the_server_said_nothing_about_has_no_parameters_to_show() {
        assert!(!Job::default().has_parameters());

        // Everything an ordinary generation sets, one at a time, because the
        // guard has to stop applying for each of them on its own.
        for j in [
            Job {
                prompt: "a cat".into(),
                ..Default::default()
            },
            Job {
                width: 512,
                ..Default::default()
            },
            Job {
                seed: 4242,
                ..Default::default()
            },
            Job {
                clip_skip: Some(0),
                ..Default::default()
            },
        ] {
            assert!(j.has_parameters(), "{j:?} reads as a job nobody described");
        }

        // AND IT STOPS APPLYING THE MOMENT AN UTTERANCE ARRIVES, which is the
        // coupling: the day `newJob` carries a `tts` on the way out, this
        // guard gets out of the way and whoever added the field has to add
        // its rows. Asserted now so that change goes red here rather than
        // shipping a block that says the parameters were not sent over
        // parameters that were.
        let spoken = Job {
            tts: Some(Utterance {
                text: "Hello".into(),
                voice_id: "lessac".into(),
                output: String::new(),
            }),
            ..Default::default()
        };
        assert!(spoken.has_parameters());
    }

    // --- which element an artifact wants ---

    /// The download link names the format the server declared.
    #[test]
    fn a_download_link_names_the_declared_subtype() {
        assert_eq!(subtype("audio/wav"), "wav");
        assert_eq!(subtype("image/png"), "png");
        // Neither of these is a type ember declares; both answer something
        // rather than panicking, which is all this has to do.
        assert_eq!(subtype("wav"), "wav");
        assert_eq!(subtype(""), "");
    }

    /// The element is chosen from what the server will declare, each way.
    #[test]
    fn an_artifact_chooses_its_element_from_its_declared_type() {
        let art = |media: &str| {
            ImageRef {
                media: media.into(),
                ..Default::default()
            }
            .artifact()
        };

        assert_eq!(art("image/png"), Artifact::Image);
        assert_eq!(art("audio/wav"), Artifact::Audio);
        // A second image format needs no change here, which is why the match
        // is on the top-level type: `output_format` can already ask sd-server
        // for a jpeg.
        assert_eq!(art("image/jpeg"), Artifact::Image);
        // And a parameter the server one day appends does not fall through.
        assert_eq!(art("audio/wav; rate=22050"), Artifact::Audio);
    }

    /// An artifact ember will not name is SAID and never guessed at.
    ///
    /// An empty `media` is the row `GET` on that path answers 500 for, so
    /// rendering an `<img>` over it produces a gallery that looks loaded and
    /// is empty -- which is the one failure mode `<img>` and `<audio>` share.
    #[test]
    fn an_artifact_ember_cannot_name_is_not_rendered_as_an_image() {
        for media in ["", "application/octet-stream", "text/html"] {
            let got = ImageRef {
                media: media.into(),
                ..Default::default()
            }
            .artifact();
            assert_eq!(
                got,
                Artifact::Unknown(media.into()),
                "media {media:?} must not be rendered as an image or played"
            );
        }
    }

    /// And the wire field is read, which is what the Go side sends.
    #[test]
    fn an_artifact_decodes_the_type_the_server_declared() {
        let r: Request = serde_json::from_str(
            r#"{"id":"req-1","images":[
                {"job":1,"index":0,"bytes":44,
                 "path":"/api/requests/req-1/images/1/0","media":"audio/wav"}]}"#,
        )
        .expect("a wav artifact decodes");
        assert_eq!(r.results().count(), 1);
        assert_eq!(r.images[0].artifact(), Artifact::Audio);
    }

    /// THE FOURTH SLICE DECODES, which is this envelope's own history: for
    /// the life of `GET /api/nodes` there was no Rust type for the answer at
    /// all, and the route answered 501 in every shipped `ember serve`.
    #[test]
    fn a_fleet_decodes_what_the_queue_reported() {
        let n: Nodes = serde_json::from_str(
            r#"{"findings":[],"leftovers":[],"stores":[],"monthly_storage":{"micro_usd":0,
                "display":"$0.00"},"attention":0,"workers":[
                {"node":"local/n1","provider":"local","card":"rtx2080ti","image":"ember:sd",
                 "opener":"req-a","request":"req-b","state":"live","beat":"2s","up":"9m0s",
                 "accrued":{"micro_usd":11333,"display":"$0.01"},"remaining":"42m0s"}]}"#,
        )
        .expect("a fleet decodes");
        assert_eq!(n.workers.len(), 1);
        let w = &n.workers[0];
        assert_eq!(w.node, "local/n1");
        assert_eq!(w.opener, "req-a");
        assert_eq!(w.request, "req-b");
        assert_eq!(w.beat, "2s");
        assert_eq!(w.remaining.as_deref(), Some("42m0s"));
        assert_eq!(w.accrued.as_ref().map(|m| m.text()), Some("$0.01"));
        assert!(!w.needs_attention());
    }

    /// A NULL IS A `null` AND NOT A MISSING FIELD, which is the defect this
    /// repository has already paid for once: Go marshals a nil slice as
    /// `null`, serde's `default` covers a field that is ABSENT, and a whole
    /// websocket envelope -- with it a request's state -- was dropped on a
    /// type error nobody could see. Go sends `[]` now; this is the belt.
    #[test]
    fn a_null_fleet_is_an_empty_fleet_and_not_a_dropped_envelope() {
        let n: Nodes = serde_json::from_str(r#"{"workers":null,"attention":3}"#)
            .expect("a null workers list must not drop the whole answer");
        assert!(n.workers.is_empty());
        assert_eq!(n.attention, 3, "the rest of the envelope survived");
    }

    fn worker(state: &str) -> Worker {
        Worker {
            node: "local/n1".into(),
            provider: "local".into(),
            state: state.into(),
            ..Default::default()
        }
    }

    /// ONLY `stale` IS RED, and the three exclusions are each a decision.
    ///
    /// `starting` is a create call in flight -- money committed and nothing
    /// wrong -- so a red row there would be red on every rental this server
    /// ever opens, for its first minute. `quiet` is a speech rental, which
    /// has no worker to beat at all: there being nobody to answer is a
    /// different fact from not answering, and painting it red would teach an
    /// operator to ignore the colour, which is `reconcile.KindStore`'s
    /// argument about exit 2 reached through a stylesheet.
    ///
    /// THE NEGATIVE CONTROL IS `stale` ITSELF: without it these assertions
    /// are all satisfied by a method that answers false for everything.
    #[test]
    fn only_a_worker_that_stopped_answering_is_painted_as_wrong() {
        assert!(worker("stale").needs_attention());
        for ok in ["live", "starting", "quiet", "something-go-added-later"] {
            assert!(
                !worker(ok).needs_attention(),
                "a {ok} rental was painted as needing a person"
            );
        }
    }

    /// The sentence and not the word.
    ///
    /// "stale" alone reads as a cache that needs refreshing, which is the
    /// opposite of what it means here: the card is billing and nobody on it
    /// is answering.
    #[test]
    fn a_wedged_row_says_what_is_wrong_with_it() {
        assert_eq!(worker("stale").note(), "not answering, and still billing");
        assert_eq!(
            worker("starting").note(),
            "the create call has not returned"
        );
        assert_eq!(worker("live").note(), "");
        assert_eq!(worker("quiet").note(), "");
    }

    /// A ROW WITH A BLANK NAME COLUMN IS A ROW AN OPERATOR CANNOT SCAN, which
    /// is `Finding::names`'s own reason. A rental being created has no node,
    /// and what that row is about is precisely that.
    #[test]
    fn a_rental_being_created_still_names_itself() {
        let mut w = worker("starting");
        w.node = String::new();
        w.provider = "runpod".into();
        assert_eq!(w.names(), "runpod (being created)");
        assert_eq!(worker("live").names(), "local/n1");
    }

    /// THE FLEET'S COUNT IS NOT `attention`, and that is the whole point of
    /// it being a separate method.
    ///
    /// `attention` is `reconcile`'s count of what `ember ls` exits 2 for, and
    /// no `ember ls` has ever seen a wedged worker -- it is this process's own
    /// observation. Adding it in would change what that number means depending
    /// on whether an `ember serve` happened to be running when it was taken.
    #[test]
    fn a_wedged_worker_is_counted_apart_from_the_sweeps_own_findings() {
        let n = Nodes {
            workers: vec![
                worker("stale"),
                worker("live"),
                worker("stale"),
                worker("quiet"),
            ],
            ..Default::default()
        };
        assert_eq!(n.wedged(), 2);
        assert_eq!(n.attention, 0, "the sweep found nothing, and still has not");
        assert!(
            n.verdict().contains("nothing is running"),
            "a wedged worker changed reconcile's own sentence: {}",
            n.verdict()
        );
    }

    fn dated(created: &str) -> Finding {
        Finding {
            created: created.into(),
            ..Default::default()
        }
    }

    /// The shape Go sends, as a person reads it.
    #[test]
    fn a_creation_time_is_a_date_a_time_and_the_zone_it_is_in() {
        assert_eq!(
            dated("2026-11-10T11:26:44Z").created_text(),
            "2026-11-10 11:26 UTC"
        );
    }

    /// THE ZONE IS NOT DECORATION. Go sends UTC and the operator is on the
    /// machine serving the page, so dropping the marker would show a Moscow
    /// reader 11:26 for something that happened at 14:26 their time -- three
    /// hours of understatement about how long a card has been billing, in the
    /// direction that makes a leak look fresher than it is.
    #[test]
    fn the_zone_is_said_and_never_dropped() {
        let t = dated("2026-11-10T11:26:44Z").created_text();
        assert!(t.ends_with(" UTC"), "the zone went missing from {t:?}");
    }

    /// AN OFFSET INSTANT IS NOT A UTC ONE, and this is the case that would be
    /// a silent three-hour lie. A `replace('T', ' ')` would render
    /// `2026-11-10T14:26:44+03:00` as `2026-11-10 14:26` and the column would
    /// read as UTC like every row beside it. Parse, don't grep: an unknown
    /// shape is shown whole, which is wrong-looking rather than wrong.
    #[test]
    fn an_instant_that_is_not_utc_is_not_dressed_up_as_one() {
        let raw = "2026-11-10T14:26:44+03:00";
        assert_eq!(dated(raw).created_text(), raw);
    }

    /// A record ember has no creation time for sends nothing, and nothing is
    /// what the cell shows. The one spelling of "ember cannot say".
    #[test]
    fn no_creation_time_is_an_empty_cell() {
        assert_eq!(dated("").created_text(), "");
    }

    /// Every rejected shape falls back to the raw string rather than to a
    /// guess. The last is the one that would panic a slice taken on faith:
    /// twenty BYTES that are nineteen characters, so `&s[..10]` is fine and
    /// a careless `&s[..19]` would land inside one and abort the whole page.
    #[test]
    fn a_shape_this_does_not_know_is_shown_whole() {
        for raw in [
            "2026-11-10 11:26:44Z",       // no T
            "2026-11-10T11:26:44",        // no zone at all
            "2026-11-10T11:26:4Z",        // nineteen bytes
            "26-11-10T11:26:44Z",         // two-digit year
            "2026-11-10TXX:26:44Z",       // not digits
            "2026-11-10T11:26:4\u{042F}", // twenty bytes, nineteen characters
        ] {
            assert_eq!(dated(raw).created_text(), raw, "{raw:?} was reshaped");
        }
    }
}
