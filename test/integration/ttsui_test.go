//go:build integration && imagelab

// The browser half of speech: an operator types text and gets back audio that
// plays.
//
// Behind the imagelab tag with the rest of the speech labs, for their reason --
// it needs ember:tts, which `make docker-build` does not produce. It also needs
// the wasm, because it drives the real page; `make test-tts-lab` depends on
// `web` and a missing image is a hard failure naming the command.
//
//	make workload-tts
//	make test-tts-lab
package integration

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/chromedp/chromedp"

	"github.com/Legion112/ember/test/integration/harness"
)

// TestTTS_ServeUISpeaksWhatTheOperatorTyped is the user's own sentence: type
// text, choose speech, submit, and get back audio that plays.
//
// WHY A BROWSER, AND WHY THIS IS NOT THE cargo TEST BESIDE IT. Every decision
// it drives has one: `Config::composing` decides which controls a kind shows,
// `Utterance::refusal` decides whether an utterance can be sent, and
// `ImageRef::artifact` decides which element an artifact wants -- three
// methods, twelve assertions, all green with no browser anywhere. What no Rust
// test can see is whether the three are JOINED to the page and to the server:
// whether `/api/config`'s voices reach a chooser, whether a `<select>` change
// reaches the draft, whether the body the button posts is one `crossKind`
// accepts, and whether the element the gallery renders can play the bytes the
// route sends. Four of this page's defects were exactly that shape, and the
// one this is nearest to is the preview frame -- written end to end, covered,
// and inserted by nothing.
//
// AND THE DURATION IS THE ASSERTION THAT MAKES IT WORTH RUNNING. An `<audio>`
// element whose source 404s renders perfectly and plays nothing, exactly as an
// `<img>` does -- so "the page has an audio tag" is satisfied by a page that
// is completely broken. `duration > 0` is the browser saying it demuxed a
// stream out of what the route sent, which is a claim that cannot be made
// without the whole chain: the id the page posted resolved against the
// catalogue ember was started with, piper ran, the artifact row was keyed
// `.wav`, the route declared `audio/wav` rather than the `image/png` it
// declared unconditionally until a second kind existed, and `nosniff` beside
// that declaration did not stop Chrome playing it.
func TestTTS_ServeUISpeaksWhatTheOperatorTyped(t *testing.T) {
	requireChrome(t)
	l := harness.New(t)
	image := ttsImage(t, l)

	l.WriteConfig(map[string]string{"image": image, "ttl": "20m"})
	// TWO VOICES AND NOT ONE, which is what makes the chooser a chooser: a
	// `<select>` with one entry cannot show that the list came from the server
	// rather than from a literal in the page, and the panel hides the kind
	// selector below two kinds for the same reason. The second entry points at
	// the same file on purpose -- this lab is about which ids reach a browser
	// and which one comes back, and a second voice model would be a second
	// download for nothing.
	l.AppendConfig(`
tts:
  voices:
    - id: lessac
      name: Lessac (en_US, medium)
      voice: /opt/voices/en_US-lessac-medium.onnx
    - id: lessac-again
      name: Lessac, a second entry
      voice: /opt/voices/en_US-lessac-medium.onnx
`)
	cmd, srv, wait := startServe(t, l)
	pageURL := fmt.Sprintf("http://127.0.0.1:%d/", srv.Port)

	// SIGINT and then wait, which is `ember serve`'s documented shutdown: a
	// request that finished does not destroy the node, because the drain loop
	// destroys it when nothing compatible is left -- so the test exiting would
	// leave a container and a bridge network behind. Nothing bills for the
	// network, which is why that class went unnoticed until Docker had
	// subnetted its whole address pool.
	defer func() {
		if err := cmd.Process.Signal(syscall.SIGINT); err != nil {
			t.Errorf("signalling ember: %v", err)
			return
		}
		out := wait()
		if nodes := l.Nodes(t.Context()); len(nodes) != 0 {
			t.Errorf("%d node(s) survived the shutdown; ember said:\n%s", len(nodes), out)
		}
	}()

	opts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.Headless,
		chromedp.NoSandbox,
		chromedp.DisableGPU,
		// The one flag this lab adds, and it is about not depending on the
		// machine: a headless container has no audio device, and a Chrome that
		// tried to open one would make the verdict depend on whether the
		// runner has a sound server. Muted, Chrome still demuxes -- which is
		// where `duration` comes from.
		chromedp.Flag("mute-audio", true),
	)
	alloc, cancelAlloc := chromedp.NewExecAllocator(t.Context(), opts...)
	defer cancelAlloc()

	ctx, cancel := chromedp.NewContext(alloc)
	defer cancel()
	ctx, cancelTimeout := context.WithTimeout(ctx, 12*time.Minute)
	defer cancelTimeout()

	// The one-time URL, which the server exchanges for the cookie and
	// redirects away from -- so everything after this is the cookie doing the
	// work, including the `<audio>` element's own fetch.
	tokenURL := fmt.Sprintf("%s?t=%s", pageURL, srv.Token)
	if err := chromedp.Do(ctx, chromedp.Navigate(tokenURL)); err != nil {
		t.Fatalf("navigating to the one-time URL: %v", err)
	}
	// After the first navigation: chromedp.Console subscribes to a target, and
	// a context whose target does not exist yet has nothing to subscribe to --
	// so starting it first deadlocks the first action.
	console := watchConsole(t, ctx)

	if err := chromedp.Do(ctx,
		chromedp.WaitVisible(`h1`),
		// The kind selector, which is the control this feature adds to the
		// panel. It is built from `/api/config`'s `kinds` and from nothing
		// else, so it being here at all is the first half of the join.
		chromedp.WaitVisible(kindSelect),
	); err != nil {
		t.Fatalf("loading the page: %v\n%s", err, console.text())
	}

	// AND THE HEADER DOES NOT SAY THIS CONFIGURATION RUNS NOTHING, which is a
	// defect this lab found rather than one it was written for. The warning
	// was `!servable` and said "a queued request would have nowhere to run" --
	// true while an image generation was the only workload, and wrong for the
	// one configuration speech is for: a `tts.voices` list with no `sd:` block
	// runs speech perfectly. In red, above a panel that was about to work.
	//
	// Both directions, because a check for the new wording alone would pass
	// against a header printing both.
	header, herr := chromedp.Run(ctx, chromedp.Text(`header`))
	if herr != nil {
		t.Fatalf("reading the header: %v\n%s", herr, console.text())
	}
	if !strings.Contains(header, "Speech still works") {
		t.Errorf("the header does not say speech runs on a config with no sd: block; "+
			"it says:\n%s", header)
	}
	if strings.Contains(header, "nothing for a queued request to run") {
		t.Errorf("the header says this configuration runs nothing, over a published "+
			"voice catalogue:\n%s", header)
	}

	// BEFORE: the image controls, which is "exactly what is there today".
	// Asserted so the absence below means the panel SWITCHED rather than that
	// this lab cannot find a width box.
	if !visible(t, ctx, labelled("width")) {
		t.Fatalf("the panel does not open on the image controls\n%s", console.text())
	}

	selectKind(t, ctx, "tts", console)

	// AFTER: the speech controls, and the image ones gone.
	//
	// BOTH DIRECTIONS, because each is a different defect. A voice chooser
	// that never appeared is a feature that is not there; a width box left
	// beside it is a control that means nothing for audio, accepts a value,
	// and makes `crossKind` refuse the whole request naming an index the
	// operator cannot see.
	if err := chromedp.Do(ctx, chromedp.WaitVisible(labelled("voice")+"/select")); err != nil {
		t.Fatalf("the voice chooser never appeared: %v\n%s", err, console.text())
	}
	// THE LABEL AND NOT THE INPUT, AND THE FIRST VERSION OF THIS LOOP COULD
	// NOT FAIL. It asked for `/input` under each label, and not every control
	// here is one: `preview` is a `<select>`, and so is `sampler` once a
	// rental has cached a capability list. So the case this loop exists for --
	// the preview row left rendering over a speech request -- was GREEN
	// against a patch that put it back, measured. "What would this assertion
	// say if the bug were present?" answered "nothing, it cannot see that
	// element". The label carries the help text and the control sits inside
	// it, so asking about the label is tag-agnostic and cannot go stale when
	// a control's element changes, which is the same reason these selectors
	// read the page's own structure rather than an attribute added for a test.
	for _, gone := range []string{
		"width", "height", "steps", "seed", "sampler", "preview", "every N steps",
	} {
		if visible(t, ctx, labelled(gone)) {
			t.Errorf("the %q control is still on screen over a speech request; it means "+
				"nothing for audio and crossKind refuses a job that sets it", gone)
		}
	}

	// AND THE CHOOSER'S OPTIONS ARE THE SERVER'S, which is the whole reason
	// the list is on the wire: a request names a voice by id and has no field
	// for a path, so a page that guessed would offer a choice that is refused
	// after a card has been rented. Read as the DISPLAY NAMES, because those
	// are the half a page could not have derived from an id.
	voices, verr := chromedp.Run(ctx, chromedp.Text(labelled("voice")+"/select"))
	if verr != nil {
		t.Fatalf("reading the voice chooser: %v\n%s", verr, console.text())
	}
	for _, want := range []string{"Lessac (en_US, medium)", "Lessac, a second entry"} {
		if !strings.Contains(voices, want) {
			t.Errorf("the chooser does not offer %q; it says %q", want, voices)
		}
	}
	if strings.Contains(voices, "/opt/voices") {
		t.Errorf("the chooser shows a path on the node: %q", voices)
	}

	// THE BUTTON'S OWN VERB, read while the panel is still on screen:
	// "generate" over a sentence of speech is a label describing the other
	// workload, and the label is built from the same `composing` the controls
	// are.
	if label, lerr := chromedp.Run(ctx, chromedp.Text(`button.primary`)); lerr != nil {
		t.Errorf("reading the submit button: %v", lerr)
	} else if !strings.Contains(label, "speak") {
		t.Errorf("the submit button says %q over a speech request", label)
	}

	// TYPE, AND SUBMIT.
	//
	// A WHOLE SENTENCE AND NOT A WORD, because the duration below is most of
	// what can say the text reached the request: piper speaks what it was
	// given, so a page posting an empty utterance is refused by
	// `Utterance::refusal` and a page posting a hardcoded one would produce
	// audio of the wrong length. The floor is set under what this sentence
	// takes rather than at zero.
	const said = "Hello from ember. This sentence is long enough to measure."
	if err := chromedp.Do(ctx,
		chromedp.SendKeys(`textarea`, said),
		chromedp.Click(`button.primary`),
	); err != nil {
		t.Fatalf("queueing an utterance: %v\n%s", err, console.text())
	}

	// AND THE TEXT REACHED THE REQUEST, which is the gap `TestServe_UI` went
	// most of its life without an assertion across: a control driven here and
	// a request queued there, with nothing joining them, so a page that took a
	// value into the element and not into the draft would have gone on passing
	// while running the panel's default.
	//
	// READ OFF `ember serve`'s OWN OUTPUT and not off the request view, and
	// that is the stated gap rather than a choice: `viewRequest` projects a
	// job's image shape and carries no utterance, so neither the page nor this
	// lab can read the text back from the API. What it can read is the line
	// `report.Text` prints for a started unit -- `tts.Job.Describe`, which
	// quotes the text -- and both halves of that line have a unit test.
	// Raced against the page's own refusal rather than waited out, for the
	// reason `waitForAudioElement` is: a body `crossKind` would not take
	// leaves the panel showing the server's sentence and this loop with
	// nothing coming, and six minutes of that reports a timeout for a failure
	// that is on screen.
	deadline := time.Now().Add(6 * time.Minute)
	for !strings.Contains(l.Printed(), said) {
		if why := pageRefusal(t, ctx); why != "" {
			t.Fatalf("the page is showing a refusal rather than a queued utterance: %s\n%s",
				why, l.Printed())
		}
		if time.Now().After(deadline) {
			t.Fatalf("ember never started an utterance carrying the text this lab typed; "+
				"it said:\n%s\n%s", l.Printed(), console.text())
		}
		time.Sleep(500 * time.Millisecond)
	}

	// THE AUDIO, AND THAT IT PLAYS.
	waitForAudioElement(t, ctx, 6*time.Minute, console, l)

	dur, src := audioDuration(t, ctx, 2*time.Minute, console)
	t.Logf("the browser demuxed %.3fs of audio from %s", dur, src)
	if dur <= 0 {
		t.Fatalf("the audio element reports %.3fs; an <audio> whose source does not resolve "+
			"renders perfectly and plays nothing", dur)
	}
	// Under what this sentence takes and over what a truncated or empty
	// artifact would give: the server-side lab asserts 0.5s for a four-word
	// utterance, and this is eleven words.
	if dur < 1.0 {
		t.Errorf("%.3fs of audio for %q is shorter than the sentence; either the text did "+
			"not reach the request or the artifact is truncated", dur, said)
	}
	if !strings.Contains(src, "/api/requests/") || !strings.Contains(src, "/images/") {
		t.Errorf("the audio source is %q, want the artifact route", src)
	}
	if errs := console.errors(); errs != "" {
		t.Errorf("the page reported errors:\n%s", errs)
	}
}

// kindSelect is the panel's kind selector.
const kindSelect = `//label[span[@class="help" and text()="kind"]]/select`

// labelled is one control's LABEL, named by the help text inside it.
//
// The page's own structure and not an attribute added for a test: these labels
// are what an operator reads, so markup that changes breaks this, which is the
// correct direction. One spelling for both directions of the assertion above,
// because two inlined copies are how two labs came to disagree about what
// drives a control.
//
// THE LABEL AND NOT THE CONTROL INSIDE IT, so that an assertion about a
// control being GONE does not also depend on which element it is -- see the
// loop above, whose first version asked for an `<input>` and therefore could
// not see a `<select>` left on screen. A caller that needs the control itself
// appends the tag.
func labelled(label string) string {
	return fmt.Sprintf(`//label[span[@class="help" and text()=%q]]`, label)
}

// visible answers whether one selector resolves to something on screen.
//
// `Evaluate` AND NOT `WaitVisible`, because what this has to report is an
// element that is NOT there: `chromedp.WaitVisible` blocks until the context
// ends, so the absent case would spend the lab's whole budget and report
// "context deadline exceeded" -- true, slow, and naming nothing. The same
// correction `waitForParams` carries, in a predicate instead of a poll.
//
// A box with width and height rather than merely a node, because Leptos leaves
// comment markers where a `<Show>` is closed and a hidden control is still a
// control an operator cannot see.
func visible(t *testing.T, ctx context.Context, xpath string) bool {
	t.Helper()
	// document.evaluate rather than querySelector, because these selectors
	// read a label's text -- which CSS cannot express, and which is what keeps
	// them out of attributes added for a test.
	js := fmt.Sprintf(`(function () {
		var r = document.evaluate(%q, document, null,
			XPathResult.FIRST_ORDERED_NODE_TYPE, null);
		var n = r.singleNodeValue;
		if (!n) { return false; }
		var box = n.getBoundingClientRect();
		return box.width > 0 && box.height > 0;
	})()`, xpath)
	got, err := chromedp.Run(ctx, chromedp.Evaluate[bool](js))
	if err != nil {
		t.Fatalf("asking whether %s is on screen: %v", xpath, err)
	}
	return got
}

// selectKind drives the panel's kind selector.
//
// A REAL `change` EVENT, DISPATCHED EXPLICITLY, and not `chromedp.SetValue`.
// AGENTS.md records measuring that SetValue does dispatch a bubbling `input`
// and `change` when the attribute is named `value` -- which is true, and is a
// property of the pinned v0.21.0's embedded JavaScript rather than of anything
// here. This lab does not rest on it. What proves the change reached the
// signal is the panel switching, which is what the two directions of assertion
// above report: a change that went nowhere leaves the width box on screen and
// the voice chooser absent.
//
// IT REPORTS A MISSING OPTION RATHER THAN SUCCEEDING QUIETLY. Assigning a
// value a `<select>` does not have leaves the element's value unchanged, so a
// page whose kinds list lost `tts` would otherwise dispatch a change that
// meant nothing and the lab would blame the panel.
func selectKind(t *testing.T, ctx context.Context, kind string, console *consoleLog) {
	t.Helper()
	js := fmt.Sprintf(`(function () {
		var r = document.evaluate(%q, document, null,
			XPathResult.FIRST_ORDERED_NODE_TYPE, null);
		var n = r.singleNodeValue;
		if (!n) { return "there is no kind selector"; }
		n.value = %q;
		if (n.value !== %q) { return "the selector has no such option"; }
		n.dispatchEvent(new Event("change", {bubbles: true}));
		return "";
	})()`, kindSelect, kind, kind)
	why, err := chromedp.Run(ctx, chromedp.Evaluate[string](js))
	if err != nil {
		t.Fatalf("choosing the %s kind: %v\n%s", kind, err, console.text())
	}
	if why != "" {
		t.Fatalf("choosing the %s kind: %s\n%s", kind, why, console.text())
	}
}

// pageRefusal is whatever sentence the page is showing instead of working.
//
// `main .error` is the compose panel's refusal, `main .fatal` the
// destroy-failed warning, `main .why` the request's own end reason, and
// `body > .fatal` is `ember serve could not be reached`. All four are the
// page's own classes rather than attributes added for a test, and all four
// mean the thing being waited for is not coming.
//
// SCOPED TO `main`, AND THE HEADER IS DELIBERATELY NOT READ. `Posture` prints
// a `.fatal` of its own for a configuration that cannot run something, which
// is a standing fact about the config rather than a refusal of this request --
// and this lab's own config is exactly the one it is about, since a speech
// catalogue with no `sd:` block is not servable. The first version of this
// helper read it and stopped the lab at the first poll, which is how the
// defect under that line was found: it said "a queued request would have
// nowhere to run" over a panel that was about to work.
func pageRefusal(t *testing.T, ctx context.Context) string {
	t.Helper()
	const probe = `(function () {
		var out = "";
		["main .error", "main .fatal", "main .why", "body > .fatal"].forEach(function (s) {
			var n = document.querySelector(s);
			if (n) { out += n.innerText; }
		});
		return out;
	})()`
	got, err := chromedp.Run(ctx, chromedp.Evaluate[string](probe))
	if err != nil {
		t.Fatalf("reading the page's refusals: %v", err)
	}
	return got
}

// waitForAudioElement waits for the player, or for anything that means it is
// never coming.
//
// A RACE AND NOT A `WaitVisible`, which is the rule this file's own `orExited`
// comment states and which a browser lab here has already been corrected for
// once: the first run of the file-picker lab spent fourteen minutes waiting
// for a job row that was never going to appear, because the POST had been
// refused and the panel was already showing the server's sentence. The same
// bug was named in 1.35 seconds the next run.
//
// THE THREE THINGS THAT MEAN IT IS NEVER COMING are each a defect this change
// could have shipped, and each is reported by name rather than as a timeout. A
// refusal on screen is a body `crossKind` would not take -- a speech job
// carrying the image draft's geometry, or an utterance with no text. An
// `<img>` in the gallery is the element this page rendered for every artifact
// until the declared type was on the wire, and a wav in one is a
// broken-image icon that reads as a run that failed. And `figure.unknown` is
// the artifact ember would not name, which is a defect in the projection
// rather than in the page.
func waitForAudioElement(
	t *testing.T, ctx context.Context, within time.Duration,
	console *consoleLog, l *harness.Lab,
) {
	t.Helper()
	const probe = `(function () {
		return [
			document.querySelector(".gallery figure.audio audio") ? "yes" : "",
			String(document.querySelectorAll(".gallery figure:not(.preview) img").length),
			String(document.querySelectorAll(".gallery figure.unknown").length)
		];
	})()`
	deadline := time.Now().Add(within)
	for {
		got, err := chromedp.Run(ctx, chromedp.Evaluate[[]string](probe))
		if err != nil {
			t.Fatalf("looking for the player: %v\n%s", err, console.text())
		}
		if len(got) == 3 {
			if got[0] == "yes" {
				return
			}
			if why := pageRefusal(t, ctx); why != "" {
				t.Fatalf("the page is showing a refusal rather than a player: %s\n%s\n%s",
					why, l.Printed(), console.text())
			}
			if got[1] != "0" {
				t.Fatalf("the gallery rendered %s <img> for a speech request; a wav in an "+
					"<img> is a broken-image icon, which reads as a run that failed\n%s",
					got[1], console.text())
			}
			if got[2] != "0" {
				t.Fatalf("the gallery has %s artifact(s) ember would not name, so the "+
					"declared type did not reach the page\n%s", got[2], console.text())
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("the gallery never showed an audio element within %v\n%s\n%s",
				within, l.Printed(), console.text())
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// audioDuration polls the gallery's player until the browser has demuxed
// something, and returns what it found.
//
// WHAT IT IS ASKING is not "does the element exist" -- `WaitVisible` has
// already answered that, and an `<audio>` whose source 404s is visible. It is
// asking whether Chrome got a decodable stream out of the route: a
// `readyState` of HAVE_METADATA means the headers and the format were
// accepted, and `duration` is read out of the wav's own header. A failed fetch
// leaves `readyState` 0, `duration` NaN and `error` set.
//
// A MEDIA ERROR IS CONCLUSIVE AND IS NOT WAITED OUT. A 404, or a type the
// browser refused, is exactly what this lab exists to catch -- and spending
// two minutes on it would report a timeout for a failure that is already
// decided. `orExited`'s rule: race the thing you are waiting for against the
// thing that means it will never happen.
//
// A POLL, because `preload="metadata"` starts the fetch when the element is
// attached and finishes whenever the route answers, and there is no event
// chromedp can wait on from here.
func audioDuration(
	t *testing.T, ctx context.Context, within time.Duration, console *consoleLog,
) (float64, string) {
	t.Helper()
	const probe = `(function () {
		var a = document.querySelector(".gallery figure.audio audio");
		if (!a) { return ["0", "0", "", "there is no audio element"]; }
		var d = isFinite(a.duration) ? a.duration : 0;
		var e = a.error ? ("media error code " + a.error.code) : "";
		return [String(a.readyState), String(d), a.currentSrc || "", e];
	})()`
	deadline := time.Now().Add(within)
	var last []string
	for {
		got, err := chromedp.Run(ctx, chromedp.Evaluate[[]string](probe))
		if err != nil {
			t.Fatalf("reading the audio element: %v\n%s", err, console.text())
		}
		last = got
		if len(got) == 4 {
			if got[3] != "" {
				t.Fatalf("the browser will not play the artifact: %s (src %q)\n%s",
					got[3], got[2], console.text())
			}
			if d, derr := strconv.ParseFloat(got[1], 64); derr == nil && d > 0 {
				return d, got[2]
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("the audio element never reported a duration within %v; it says %v\n%s",
				within, last, console.text())
		}
		time.Sleep(300 * time.Millisecond)
	}
}
