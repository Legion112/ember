package config

import (
	"strings"
	"testing"
	"time"
)

// load puts a config on disk and reads it back, which is the only path that
// runs the cross-key refusal and both catalogues.
//
// THROUGH Load AND NOT BY BUILDING THE STRUCT, which is #105's lesson applied
// to a validator: every refusal this file is about is wired into `Load`, so a
// test that called `WorkloadCatalogue` directly could not tell "the refusal
// works" from "the refusal is wired".
func load(t *testing.T, body string) (Config, error) {
	t.Helper()
	return Load(write(t, body))
}

// ref is a digest-ref image, which is what every entry's own has to be.
var ref = pinned("")

// TestWorkloads_AFileThatDeclaresNoneDescribesOnePerDriver.
//
// THE BACK-COMPATIBLE HALF, and it is the half that matters most: every
// configuration committed here and every `ember up` ever taken declares no
// `workloads:`, so the implicit derivation is what keeps all of them working.
//
// THE IDS ARE THE DRIVERS' OWN NAMES, which is load-bearing rather than
// convenient: `genstore.WorkloadOf` resolves a row written before the column
// existed to `KindOf(kind)`, so a legacy row names the implicit entry exactly
// and a restart after an upgrade claims the queue it already had.
func TestWorkloads_AFileThatDeclaresNoneDescribesOnePerDriver(t *testing.T) {
	cfg, err := load(t, "image: "+ref+"\nsd:\n  serve: [\"sd-server\", \"--model\", \"/m.gguf\"]\n")
	if err != nil {
		t.Fatal(err)
	}
	ws, err := cfg.WorkloadCatalogue()
	if err != nil {
		t.Fatal(err)
	}
	if len(ws) != 2 {
		t.Fatalf("%d entries, want one per driver: %+v", len(ws), ws)
	}
	if ws[0].ID != DriverSD || ws[1].ID != DriverTTS {
		t.Errorf("ids = %q and %q, want the drivers' own names", ws[0].ID, ws[1].ID)
	}
	// THE IMAGE IS SHARED, which is exactly today's behaviour: one file names
	// one image and ember cannot tell which binary it carries.
	for _, w := range ws {
		if w.Image != ref {
			t.Errorf("entry %q has image %q, want the file's own", w.ID, w.Image)
		}
	}
	// AND ONLY THE ONE THE FILE DESCRIBES CAN BE SERVED, which is the whole
	// fix: the speech entry exists so a refusal can name `tts.voices`, and it
	// is never offered.
	if ws[0].Unservable() != "" {
		t.Errorf("the image entry cannot be served: %s", ws[0].Unservable())
	}
	if why := ws[1].Unservable(); !strings.Contains(why, "tts.voices") {
		t.Errorf("the speech entry's reason does not name the key: %q", why)
	}
}

// TestWorkloads_SelectingOnAFileThatDeclaresNoneSubstitutesNothing.
//
// THE ONE LINE THAT MAKES THIS SAFE FOR EVERY FILE THAT EXISTS. The implicit
// entries are derived FROM the top-level keys, so writing them back would be a
// no-op at best -- and at worst, for the file that describes both drivers, a
// choice between two entries that `ember up` has never had to make. So it is
// not made.
func TestWorkloads_SelectingOnAFileThatDeclaresNoneSubstitutesNothing(t *testing.T) {
	cfg, err := load(t, "image: "+ref+"\n"+
		"sd:\n  serve: [\"sd-server\"]\n"+
		"tts:\n  voices:\n    - id: lessac\n      voice: /v.onnx\n")
	if err != nil {
		t.Fatal(err)
	}
	got, w, err := cfg.Select("")
	if err != nil {
		t.Fatalf("an empty id against a file declaring none was refused: %v", err)
	}
	if w.Driver != DriverSD {
		t.Errorf("driver = %q, want the first entry's", w.Driver)
	}
	if got.Image != cfg.Image || len(got.SD.Serve) != 1 || len(got.TTS.Voices) != 1 {
		t.Errorf("the config was rewritten: %+v", got)
	}

	// AND NAMING EITHER IMPLICIT ID WORKS, because that is what a stored row
	// does: `WorkloadOf` resolves a legacy speech row to `tts`.
	if _, w, err := cfg.Select(DriverTTS); err != nil || w.Driver != DriverTTS {
		t.Errorf("selecting the implicit speech entry: %v, %+v", err, w)
	}
}

// TestWorkloads_ADeclaredEntrySubstitutesItsOwnImageAndBlock.
//
// WHAT THE WHOLE CHANGE IS FOR: the selection hands back a Config whose
// `image:`, `images:`, `sd:` and `tts:` are the entry's, so everything
// downstream -- `app.NewPlan`, `node.NewSpec`, `provision` -- reads the keys it
// always read and nothing in those layers learns what a workload is.
func TestWorkloads_ADeclaredEntrySubstitutesItsOwnImageAndBlock(t *testing.T) {
	cfg, err := load(t, `
provider: local
workloads:
  - id: image
    label: Images
    driver: sd
    image: `+ref+`
    sd:
      serve: ["sd-server", "--model", "/m.gguf"]
      preview: tae
      preview_interval: 3
  - id: speech
    driver: tts
    image: ghcr.io/example/tts:v1
    tts:
      voices:
        - id: lessac
          voice: /opt/voices/lessac.onnx
`)
	if err != nil {
		t.Fatal(err)
	}

	got, w, err := cfg.Select("speech")
	if err != nil {
		t.Fatal(err)
	}
	if w.LabelOrID() != "speech" {
		t.Errorf("label = %q, want the id where there is none", w.LabelOrID())
	}
	if got.Image != "ghcr.io/example/tts:v1" {
		t.Errorf("image = %q, want the entry's", got.Image)
	}
	// THE OTHER ENTRY'S BLOCK IS GONE, which is what stops a speech rental
	// starting an sd-server: the substituted config has no `sd.serve` at all.
	if len(got.SD.Serve) != 0 {
		t.Errorf("the image entry's serve argv survived into the speech config: %v", got.SD.Serve)
	}
	if len(got.TTS.Voices) != 1 {
		t.Errorf("the entry's voices did not reach the config: %+v", got.TTS)
	}
	// AND THE LIST IS CLEARED, so nothing downstream can read it again and
	// reach a different answer.
	if len(got.Workloads) != 0 {
		t.Errorf("the selected config still lists %d workload(s)", len(got.Workloads))
	}

	// The other direction, because one substitution is consistent with a
	// Select that always answers the same entry.
	img, _, err := cfg.Select("image")
	if err != nil {
		t.Fatal(err)
	}
	if img.Image != ref || img.SD.Preview != "tae" || img.SD.PreviewInterval != 3 {
		t.Errorf("the image entry resolved to %+v", img)
	}
}

// TestWorkloads_AnEmptyIdAgainstSeveralDeclaredIsRefused, naming them.
//
// `ember up` AND `ember store sync` RENT ONE MACHINE WITH ONE IMAGE. Picking
// silently would mean an operator who declared images then speech and expected
// speech watches the wrong binary start, and for the store sync it would mean
// pulling a 2.87 GB image where a 286 MB one would do.
func TestWorkloads_AnEmptyIdAgainstSeveralDeclaredIsRefused(t *testing.T) {
	cfg, err := load(t, `
workloads:
  - id: image
    driver: sd
    image: `+ref+`
    sd: {serve: ["sd-server"]}
  - id: speech
    driver: tts
    image: ghcr.io/example/tts:v1
    tts: {voices: [{id: lessac, voice: /v.onnx}]}
`)
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = cfg.Select("")
	if err == nil {
		t.Fatal("a file declaring two workloads picked one for an empty id")
	}
	for _, want := range []string{"image", "speech", "--workload"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not name %q: %v", want, err)
		}
	}

	// AND ONE DECLARED ENTRY NEEDS NO FLAG, which is the half that keeps the
	// common case usable: a refusal that fired for every declared file would
	// make `--workload` mandatory rather than a tie-break.
	one, err := load(t, "workloads:\n  - id: image\n    driver: sd\n    image: "+ref+
		"\n    sd: {serve: [\"sd-server\"]}\n")
	if err != nil {
		t.Fatal(err)
	}
	if _, w, err := one.Select(""); err != nil || w.ID != "image" {
		t.Errorf("a file declaring one workload refused an empty id: %v, %+v", err, w)
	}

	// AND AN ID NOBODY DECLARED NAMES WHAT IS THERE.
	if _, _, err := cfg.Select("video"); err == nil ||
		!strings.Contains(err.Error(), "image, speech") {
		t.Errorf("an undeclared id gave %v, want it to list the declared ones", err)
	}
}

// TestWorkloads_RefusedBesideTheTopLevelKeysTheyReplace, naming each key.
//
// A REFUSAL AND NOT A PRECEDENCE. The top-level keys ARE the one unnamed
// workload a file without `workloads:` describes, so honouring both would mean
// one file declaring an unnamed workload and some named ones, with the unnamed
// one reachable by `ember up` and invisible to a browser.
func TestWorkloads_RefusedBesideTheTopLevelKeysTheyReplace(t *testing.T) {
	entry := "workloads:\n  - id: image\n    driver: sd\n    image: " + ref +
		"\n    sd: {serve: [\"sd-server\"]}\n"
	cases := []struct {
		name, extra, key string
	}{
		{"an image", "image: " + ref + "\n", "image"},
		{"an images map", "images:\n  sm_89: " + ref + "\n", "images"},
		{"an sd block", "sd:\n  serve: [\"sd-server\"]\n", "sd"},
		{"a tts block", "tts:\n  voices:\n    - id: a\n      voice: /v.onnx\n", "tts"},
		// NO `cmd` OR `cmds` CASE. They were in this table when it was
		// written and the keys are gone from the config now, so
		// `rejectRemovedKeys` answers the spelling first with a message
		// naming the removal -- a file carrying one never reaches the
		// workloads check, and asserting the second refusal here would be
		// asserting a message nothing can produce.
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := load(t, entry+c.extra)
			if err == nil {
				t.Fatalf("workloads: beside a top-level %s was accepted", c.key)
			}
			if !strings.Contains(err.Error(), c.key) {
				t.Errorf("the refusal does not name %q: %v", c.key, err)
			}
			if !strings.Contains(err.Error(), "workloads:") {
				t.Errorf("the refusal does not name workloads: %v", err)
			}
		})
	}

	// AND THE KEYS THAT ARE NOT A WORKLOAD ARE UNTOUCHED, which is the line
	// this refusal draws: a ttl, a provider, a store and the budgets are
	// settings of the whole file and stay where they are.
	if _, err := load(t, entry+"provider: local\nttl: 2h\nmax_hourly_usd: \"1.00\"\n"+
		"store:\n  name: weights\n"); err != nil {
		t.Errorf("a shared setting beside workloads: was refused: %v", err)
	}
}

// TestWorkloads_RefusedForAMistakeInTheEntryItself, at load.
//
// AT LOAD AND NOT AT THE FIRST REQUEST, which is `TTSCatalogue`'s rule: a
// duplicate id or an unpinned image is a mistake in a file the operator just
// edited, and its symptom at lookup time is a nav entry that cannot be used --
// read as ember being broken rather than as their own typo.
func TestWorkloads_RefusedForAMistakeInTheEntryItself(t *testing.T) {
	cases := []struct {
		name, body, want string
	}{
		{
			"a duplicate id",
			"workloads:\n" +
				"  - {id: w, driver: sd, image: " + ref + ", sd: {serve: [s]}}\n" +
				"  - {id: w, driver: tts, image: " + ref + "}\n",
			"used twice",
		},
		{
			"an id that is nothing but dots",
			"workloads:\n  - {id: \"..\", driver: sd, image: " + ref + "}\n",
			"nothing but dots",
		},
		{
			"an id with a slash in it",
			"workloads:\n  - {id: \"a/b\", driver: sd, image: " + ref + "}\n",
			"lowercase letters",
		},
		{
			"no driver",
			"workloads:\n  - {id: w, image: " + ref + "}\n",
			"driver is empty",
		},
		{
			"a driver this build does not have",
			"workloads:\n  - {id: w, driver: video, image: " + ref + "}\n",
			"not one this build has",
		},
		{
			"no image at all",
			"workloads:\n  - {id: w, driver: sd}\n",
			"names no image",
		},
		{
			"an unpinned image",
			"workloads:\n  - {id: w, driver: sd, image: \"ghcr.io/x/sd:latest\"}\n",
			"latest",
		},
		{
			// A BLOCK THE DRIVER DOES NOT READ, refused rather than ignored:
			// it would sit in the file looking exactly like a setting that
			// applies, and the first thing to notice would be a generation
			// running with a sampler nobody chose.
			"an sd block under a tts driver",
			"workloads:\n  - {id: w, driver: tts, image: " + ref +
				", sd: {serve: [s]}, tts: {voices: [{id: a, voice: /v.onnx}]}}\n",
			"does not read one",
		},
		{
			"a per-entry images key whose tag disagrees with its arch",
			"workloads:\n  - {id: w, driver: sd, images: {sm_89: \"ghcr.io/x/sd:v1-sm75@sha256:" +
				strings.Repeat("a", 64) + "\"}}\n",
			"sm_89",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := load(t, c.body)
			if err == nil {
				t.Fatalf("%s was accepted", c.name)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("the refusal does not say %q: %v", c.want, err)
			}
		})
	}
}

// TestWorkloads_APerEntryCredentialIsRefusedLikeAnyOther.
//
// `rejectSecrets` builds its field map rather than writing it as a literal
// precisely so that a new free-text scalar is covered, and a declared entry's
// `image:` is one: the one place in a committed file that is not scanned is
// the one somewhere a reference gets pasted.
func TestWorkloads_APerEntryCredentialIsRefusedLikeAnyOther(t *testing.T) {
	_, err := load(t, "workloads:\n  - {id: w, driver: sd, image: \"ghcr.io/x/sd:api_key\"}\n")
	if err == nil || !strings.Contains(err.Error(), "credential") {
		t.Errorf("a credential in a declared entry's image gave %v", err)
	}
	_, err = load(t, "workloads:\n  - {id: w, driver: sd, images: {sm_89: \"x:secret\"}}\n")
	if err == nil || !strings.Contains(err.Error(), "credential") {
		t.Errorf("a credential in a declared entry's images map gave %v", err)
	}
}

// TestWorkloads_AnUnservableEntryIsNotAnUnloadableOne.
//
// THE TWO ARE DIFFERENT ANSWERS AND THE REMEDIES DIFFER. A mistake in the
// entry is a file that will not load; an entry that loads and cannot be served
// is a file that is missing a block, which `/api/config` leaves out and
// `POST /api/requests` refuses by name. A load refusal for the second would
// mean one typo in a second workload stopped the first one working.
func TestWorkloads_AnUnservableEntryIsNotAnUnloadableOne(t *testing.T) {
	cfg, err := load(t, "workloads:\n"+
		"  - {id: image, driver: sd, image: "+ref+", sd: {serve: [s]}}\n"+
		"  - {id: speech, driver: tts, image: "+ref+"}\n")
	if err != nil {
		t.Fatalf("an entry with no voices made the file unloadable: %v", err)
	}
	ws, err := cfg.WorkloadCatalogue()
	if err != nil {
		t.Fatal(err)
	}
	if ws[0].Unservable() != "" {
		t.Errorf("the image entry cannot be served: %s", ws[0].Unservable())
	}
	if why := ws[1].Unservable(); !strings.Contains(why, "tts.voices") {
		t.Errorf("the speech entry's reason does not name the key: %q", why)
	}
}

// TestWorkload_AnUnknownDriverIsUnservableRatherThanUnloadable.
//
// NOT REACHABLE THROUGH Load, which is why this builds the value: `validate`
// refuses an unknown `driver:` at load, naming the drivers this build has. The
// state this arm is for is a newer ember's config read by an older one -- a
// file whose other entries may be perfectly runnable -- which is the same
// reason the page names an unknown driver rather than rendering a blank panel.
func TestWorkload_AnUnknownDriverIsUnservableRatherThanUnloadable(t *testing.T) {
	w := Workload{ID: "clips", Driver: "video", Image: ref}
	why := w.Unservable()
	if !strings.Contains(why, "video") {
		t.Errorf("the reason does not name the driver: %q", why)
	}
	for _, d := range Drivers() {
		if !strings.Contains(why, d) {
			t.Errorf("the reason does not say this build runs %q: %q", d, why)
		}
	}
}

// TestWorkload_PublishesItsOwnVoicesAndNotTheProcessWide.
//
// THE IMAGE BEING PER ENTRY FORCES IT: two declared speech workloads are two
// images with two voice sets on them, and one catalogue for the process would
// offer each one's ids for the other's node. One derivation for both, so the
// id rules and the espeak fallback exist once.
func TestWorkload_PublishesItsOwnVoicesAndNotTheProcessWide(t *testing.T) {
	w := Workload{ID: "speech", Driver: DriverTTS, Image: ref, TTS: TTSConfig{
		Voices: []TTSVoice{{ID: "lessac", Voice: "/opt/voices/lessac.onnx"}},
	}}
	cat, err := w.TTSCatalogue()
	if err != nil {
		t.Fatal(err)
	}
	if len(cat) != 1 || cat[0].ID != "lessac" {
		t.Fatalf("catalogue = %+v", cat)
	}
	// The image's own espeak data, filled in by the one function that knows
	// what ember's speech image bakes in.
	if cat[0].EspeakData != DefaultEspeakData {
		t.Errorf("espeak_data = %q, want the image's default", cat[0].EspeakData)
	}
	// AND AN ENTRY WITH A MISTAKE IN ITS LIST REPORTS IT, which is what makes
	// this a call rather than a copy: every refusal about an id lives once.
	bad := Workload{ID: "speech", Driver: DriverTTS, Image: ref, TTS: TTSConfig{
		Voices: []TTSVoice{{ID: "A", Voice: "/v.onnx"}},
	}}
	if _, err := bad.TTSCatalogue(); err == nil {
		t.Error("an uppercase voice id was accepted")
	}
}

// TestWorkload_ATTSJobsBlockUnderAnSDDriverIsRefused, which is the other half
// of the block check and the one the table above does not reach.
func TestWorkload_ATTSJobsBlockUnderAnSDDriverIsRefused(t *testing.T) {
	_, err := load(t, "workloads:\n  - {id: w, driver: sd, image: "+ref+
		", sd: {serve: [s]}, tts: {jobs: [{text: hi}]}}\n")
	if err == nil {
		t.Fatal("a tts: block under an sd driver was accepted")
	}
	if !strings.Contains(err.Error(), "does not read one") {
		t.Errorf("the refusal does not say the driver does not read it: %v", err)
	}
}

// TestWorkload_AnSDEntryWithNoServeSaysSoNamingTheKey.
//
// THE OTHER ARM OF `Unservable`, and the state it is for is the one every
// `ember up`-only configuration is in: a file with `image:` and no `sd:` block
// describes an image entry that cannot be SERVED, which is why no tab appears
// for it and why `ember serve` refuses to start from such a file at all.
//
// THE FIXTURE WAS `image:` PLUS `cmds:` and is `image:` alone now, because the
// key that used to make this file an `ember up` one has been deleted from the
// config. The state it describes is unchanged and is in fact more common: a
// file naming an image and no driver block.
func TestWorkload_AnSDEntryWithNoServeSaysSoNamingTheKey(t *testing.T) {
	cfg, err := load(t, "image: "+ref+"\n")
	if err != nil {
		t.Fatal(err)
	}
	ws, err := cfg.WorkloadCatalogue()
	if err != nil {
		t.Fatal(err)
	}
	why := ws[0].Unservable()
	if !strings.Contains(why, "sd.serve") {
		t.Errorf("the reason does not name the key: %q", why)
	}
	// AND THE RUN ITSELF IS UNAFFECTED, which is the distinction: `ember up`
	// with `cmds` needs an image and no server, so `Select` still answers.
	if got, _, serr := cfg.Select(""); serr != nil || got.Image != ref {
		t.Errorf("a cmds configuration could not be selected: %v, %q", serr, got.Image)
	}
}

// TestSelect_ReportsACatalogueItCannotBuild rather than planning against half
// of one.
//
// NOT REACHABLE THROUGH Load, which refuses the file first -- so this builds
// the value, as the unknown-driver arm does. What it holds is the ORDER: a
// mistake in the list is reported before an id is matched against it, because
// matching against a list that is missing an entry would resolve an id to the
// wrong workload.
func TestSelect_ReportsACatalogueItCannotBuild(t *testing.T) {
	cfg := Config{Workloads: []Workload{{ID: "A", Driver: DriverSD, Image: ref}}}
	if _, _, err := cfg.Select(""); err == nil {
		t.Fatal("a catalogue with an invalid id was selected from")
	} else if !strings.Contains(err.Error(), "lowercase") {
		t.Errorf("the refusal is not the catalogue's: %v", err)
	}
}

// TestSelect_AnEntryReplacesTheKeysThatMakeItAWorkload.
//
// THE DEFECT THIS CLOSES WAS A LIMIT RATHER THAN A BUG, and it was found by
// trying to write the single committed configuration this repository is
// collapsing to. An entry carried `image`, `images`, `sd` and `tts` -- what a
// DRIVER reads -- and `Select` left everything else shared. The four example
// files differed in exactly what it left shared: a free sd15 entry needs no
// store and no assets where a FLUX.2 one needs 56 GiB of both, 11 GiB and
// 80 GiB cards cannot share a VRAM floor, `verify: always` re-hashes a volume
// the local path has no cross-run writer for, and a CPU image fails a probe
// that asks nvidia-smi which architecture it is. So one file could hold two
// entries and only one of them could work, which is the variety the entry
// exists to remove.
func TestSelect_AnEntryReplacesTheKeysThatMakeItAWorkload(t *testing.T) {
	cfg, err := load(t, `provider: local
ttl: 30m
min_vram_gib: 8
verify: present
store:
  name: shared
  size_gib: 1
  mount: /shared
gpu_probe: ["sh", "-c", "the file's"]
gpu_probe_timeout: 1m
workloads:
  - id: free
    driver: sd
    image: `+ref+`
    sd:
      serve: ["sd-server", "--model", "/opt/models/sd15.gguf"]
  - id: flux2
    driver: sd
    image: `+ref+`
    ttl: 2h
    min_vram_gib: 80
    verify: always
    store:
      name: flux2
      size_gib: 70
      mount: /models
    assets:
      - url: https://example.invalid/w.gguf
        sha256: `+strings.Repeat("b", 64)+`
        as: w.gguf
    gpu_probe: ["sh", "-c", "the entry's"]
    gpu_probe_timeout: 2m
    sd:
      serve: ["sd-server", "--diffusion-model", "/models/w.gguf"]
`)
	if err != nil {
		t.Fatal(err)
	}

	// THE ENTRY THAT NAMES NOTHING INHERITS, which is the half that is easy to
	// get wrong: the zero value of every one of these is a dangerous answer --
	// no store, no assets, no floor, no probe, no deadline -- so "the entry
	// did not say" has to mean "the file's" and never "none".
	free, _, err := cfg.Select("free")
	if err != nil {
		t.Fatal(err)
	}
	if free.TTL != 30*time.Minute || free.MinVRAMGiB != 8 || free.Verify != "present" {
		t.Errorf("the inheriting entry got ttl=%v vram=%d verify=%q, want the file's",
			free.TTL, free.MinVRAMGiB, free.Verify)
	}
	if free.Store.Name != "shared" {
		t.Errorf("the inheriting entry got store %q, want the file's", free.Store.Name)
	}
	if len(free.GPUProbe) == 0 || free.GPUProbe[2] != "the file's" {
		t.Errorf("the inheriting entry got probe %v, want the file's", free.GPUProbe)
	}

	// AND THE ENTRY THAT NAMES THEM REPLACES EVERY ONE.
	flux, _, err := cfg.Select("flux2")
	if err != nil {
		t.Fatal(err)
	}
	if flux.TTL != 2*time.Hour || flux.MinVRAMGiB != 80 || flux.Verify != "always" {
		t.Errorf("the replacing entry got ttl=%v vram=%d verify=%q, want its own",
			flux.TTL, flux.MinVRAMGiB, flux.Verify)
	}
	if flux.Store.Name != "flux2" || flux.Store.SizeGiB != 70 {
		t.Errorf("the replacing entry got store %+v, want its own", flux.Store)
	}
	if len(flux.Assets) != 1 || flux.Assets[0].As != "w.gguf" {
		t.Errorf("the replacing entry got assets %+v, want its own", flux.Assets)
	}
	// THE TIMEOUT TRAVELS WITH THE PROBE IT BOUNDS. An entry that replaced the
	// argv and inherited the file's bound would be timing a different command,
	// which is how a local path's one minute comes to be read against a 56 GiB
	// network volume.
	if len(flux.GPUProbe) == 0 || flux.GPUProbe[2] != "the entry's" {
		t.Errorf("the replacing entry got probe %v, want its own", flux.GPUProbe)
	}
	if flux.GPUProbeTimeout != 2*time.Minute {
		t.Errorf("the probe timeout is %v, want the entry's 2m", flux.GPUProbeTimeout)
	}
}
