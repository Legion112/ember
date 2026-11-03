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

/// What the server will let this UI offer.
#[derive(Clone, Debug, Default, Deserialize)]
pub struct Config {
    #[serde(default, deserialize_with = "nullable")]
    pub providers: Vec<Provider>,
    #[serde(default)]
    pub ceilings: Ceilings,
    #[serde(default)]
    pub allow_paid: bool,
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

    /// One line identifying this job, the way ember's own output does.
    ///
    /// Deliberately the same shape as `sdserver.Job.Describe` in Go: a prompt
    /// in quotes, truncated, then the geometry only when both sides are set --
    /// because the other would come from the server's default and "512x" would
    /// be a guess printed as a fact.
    pub fn describe(&self) -> String {
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
    #[serde(default)]
    pub preview: bool,
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
    #[serde(default)]
    pub running: String,
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
    #[serde(default)]
    pub next: i64,
    #[serde(default, deserialize_with = "nullable")]
    pub events: Vec<Event>,
    #[serde(default, deserialize_with = "nullable")]
    pub request: Option<Request>,
    #[serde(default)]
    pub running: String,
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
            "running": "req-1"
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
                    preview: false,
                    bytes: 2048,
                },
                ImageRef {
                    job: 1,
                    index: 0,
                    path: "/p".into(),
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
        let env: Envelope =
            serde_json::from_str(r#"{"next":0,"events":null,"request":null}"#).unwrap();
        assert_eq!(env.next, 0);
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
}
