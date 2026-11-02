package cli

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Legion112/ember/internal/awssig"
	"github.com/Legion112/ember/internal/config"
	"github.com/Legion112/ember/internal/node"
)

// fakeStore is a RunPod-shaped store reachable over both of its APIs.
//
// Two endpoints, through the two seams that exist for exactly this:
// CloudBaseURL puts a store in the registry without reaching RunPod, and
// S3BaseURL catches the object calls. A test that could reach either service
// would behave differently on a machine that has credentials, which is the
// reason both seams exist.
type fakeStore struct {
	// objects maps key to size.
	objects map[string]int64

	// uploads are the incomplete multipart uploads, by id.
	uploads map[string]fakeUpload

	// refuseAbort names an upload id the endpoint will not let go of, which
	// is what an endpoint with a half-removed upload looks like.
	refuseAbort string

	aborted []string
}

type fakeUpload struct {
	key       string
	parts     int
	size      int64
	initiated time.Time
}

// deps wires both fakes into a deps and returns a config path.
func (f *fakeStore) deps(t *testing.T, extra string) (deps, string) {
	t.Helper()
	if f.objects == nil {
		f.objects = map[string]int64{}
	}
	if f.uploads == nil {
		f.uploads = map[string]fakeUpload{}
	}

	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if strings.Contains(req.URL.Path, "network-volumes") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprint(w, `{"networkVolumes":[{"id":"vol_abc","name":"ember-store-models",`+
				`"size":40,"dataCenter":"EU-RO-1","type":"STANDARD"}],`+
				`"pagination":{"hasNextPage":false}}`)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(api.Close)

	s3srv := httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(s3srv.Close)

	d := labDeps(t)
	d.CloudBaseURL = api.URL
	d.S3BaseURL = s3srv.URL
	d.Creds = &config.Credentials{
		RunPod: node.NewSecret([]byte("rp-token")),
		S3: awssig.Credentials{
			AccessKey: "user_test", Secret: node.NewSecret([]byte("rps_secret")),
		},
	}
	return d, storeConfigFor(t, d, "runpod", extra)
}

func (f *fakeStore) handle(w http.ResponseWriter, req *http.Request) {
	q := req.URL.Query()
	w.Header().Set("Content-Type", "application/xml")

	switch {
	case req.Method == http.MethodHead:
		key := strings.TrimPrefix(req.URL.Path, "/vol_abc/")
		size, ok := f.objects[key]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Length", fmt.Sprint(size))
		w.Header().Set("Last-Modified", time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC).Format(http.TimeFormat))
		w.WriteHeader(http.StatusOK)

	case req.Method == http.MethodGet && q.Get("list-type") == "2":
		var b strings.Builder
		b.WriteString("<ListBucketResult><IsTruncated>false</IsTruncated>")
		for key, size := range f.objects {
			fmt.Fprintf(&b, "<Contents><Key>%s</Key><Size>%d</Size>"+
				"<LastModified>2026-11-01T09:00:00Z</LastModified></Contents>", key, size)
		}
		b.WriteString("</ListBucketResult>")
		_, _ = w.Write([]byte(b.String()))

	case req.Method == http.MethodGet && q.Has("uploads"):
		var b strings.Builder
		b.WriteString("<ListMultipartUploadsResult><IsTruncated>false</IsTruncated>")
		for id, u := range f.uploads {
			fmt.Fprintf(&b, "<Upload><Key>%s</Key><UploadId>%s</UploadId><Initiated>%s</Initiated></Upload>",
				u.key, id, u.initiated.UTC().Format(time.RFC3339))
		}
		b.WriteString("</ListMultipartUploadsResult>")
		_, _ = w.Write([]byte(b.String()))

	case req.Method == http.MethodGet && q.Has("uploadId"):
		u := f.uploads[q.Get("uploadId")]
		var b strings.Builder
		b.WriteString("<ListPartsResult><IsTruncated>false</IsTruncated>")
		each := u.size / int64(max(u.parts, 1))
		for i := range u.parts {
			n := each
			if i == u.parts-1 {
				n = u.size - each*int64(u.parts-1)
			}
			fmt.Fprintf(&b, "<Part><PartNumber>%d</PartNumber><Size>%d</Size></Part>", i+1, n)
		}
		b.WriteString("</ListPartsResult>")
		_, _ = w.Write([]byte(b.String()))

	case req.Method == http.MethodDelete && q.Has("uploadId"):
		id := q.Get("uploadId")
		if id == f.refuseAbort {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`<Error><Code>AccessDenied</Code></Error>`))
			return
		}
		f.aborted = append(f.aborted, id)
		delete(f.uploads, id)
		w.WriteHeader(http.StatusNoContent)

	default:
		w.WriteHeader(http.StatusBadRequest)
	}
}

func TestStoreLs_NamesTheObjectsAndTheirSizes(t *testing.T) {
	f := &fakeStore{objects: map[string]int64{"sd15.gguf": 2 << 30, "loras/anime.safetensors": 144 << 20}}
	d, cfg := f.deps(t, storeBlock)

	var out bytes.Buffer
	if err := runStore([]string{"ls", "-config", cfg}, &out, d); err != nil {
		t.Fatalf("store ls = %v\n%s", err, out.String())
	}
	got := out.String()
	for _, want := range []string{"sd15.gguf", "2.0 GiB", "loras/anime.safetensors", "144.0 MiB", "in 2 object(s)"} {
		if !strings.Contains(got, want) {
			t.Errorf("ls does not mention %q:\n%s", want, got)
		}
	}
	// The clean case is said out loud. Silence would read as "this command
	// does not look at that", which is exactly what was wrong before.
	if !strings.Contains(got, "no incomplete uploads") {
		t.Errorf("ls did not say there are no incomplete uploads:\n%s", got)
	}
}

func TestStoreLs_AnEmptyStoreSaysSo(t *testing.T) {
	f := &fakeStore{}
	d, cfg := f.deps(t, storeBlock)

	var out bytes.Buffer
	if err := runStore([]string{"ls", "-config", cfg}, &out, d); err != nil {
		t.Fatalf("store ls = %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "holds no objects") {
		t.Errorf("got %q", out.String())
	}
}

// TestStoreLs_ReportsAnAbandonedUploadAndExitsTwo is the money leak.
//
// An interrupted multipart upload keeps every part that arrived and bills for
// them by the gigabyte-month. It is not an object, so it is in no object
// listing and `ember ls` cannot see it either — before this command there was
// nothing in ember that could find one.
func TestStoreLs_ReportsAnAbandonedUploadAndExitsTwo(t *testing.T) {
	f := &fakeStore{uploads: map[string]fakeUpload{
		"up-1": {key: "flux2.gguf", parts: 3, size: 384 << 20, initiated: time.Now().Add(-48 * time.Hour)},
	}}
	d, cfg := f.deps(t, storeBlock)

	var out bytes.Buffer
	err := runStore([]string{"ls", "-config", cfg}, &out, d)
	if err == nil {
		t.Fatalf("ls exited 0 over an upload that is billing for nothing:\n%s", out.String())
	}
	if ExitCode(err) != ExitFindings {
		t.Errorf("exit = %d, want %d", ExitCode(err), ExitFindings)
	}
	got := out.String()
	for _, want := range []string{"incomplete uploads", "flux2.gguf", "384.0 MiB", "3 part(s)", "abandoned", "ember store abort"} {
		if !strings.Contains(got, want) {
			t.Errorf("ls does not mention %q:\n%s", want, got)
		}
	}
	// Priced, because an orphan with no price is a line an operator reads and
	// does nothing about. 384 MiB is under a gibibyte, which is exactly the
	// case whole-GiB arithmetic prices at nothing.
	if !strings.Contains(got, "/month") {
		t.Errorf("ls did not say what the orphan costs:\n%s", got)
	}
}

// TestStoreLs_DoesNotFlagAnUploadThatMayStillBeRunning.
//
// An upload in progress and an abandoned one are the same thing to S3 — both
// are started and not completed. Exiting 2 over the first would be a finding
// that is sometimes false, which teaches an operator to ignore exit 2; it is
// still listed, because an operator checking on their own push wants to see
// it.
func TestStoreLs_DoesNotFlagAnUploadThatMayStillBeRunning(t *testing.T) {
	f := &fakeStore{uploads: map[string]fakeUpload{
		"up-1": {key: "flux2.gguf", parts: 1, size: 128 << 20, initiated: time.Now().Add(-5 * time.Minute)},
	}}
	d, cfg := f.deps(t, storeBlock)

	var out bytes.Buffer
	if err := runStore([]string{"ls", "-config", cfg}, &out, d); err != nil {
		t.Fatalf("ls exited non-zero over an upload that may be in flight: %v\n%s", err, out.String())
	}
	got := out.String()
	if !strings.Contains(got, "flux2.gguf") {
		t.Errorf("the upload was not listed at all:\n%s", got)
	}
	if strings.Contains(got, "abandoned") {
		t.Errorf("a five-minute-old upload was called abandoned:\n%s", got)
	}
	if !strings.Contains(got, "in progress") {
		t.Errorf("ls does not explain why it left it alone:\n%s", got)
	}
}

func TestStoreLs_TakesAStoreNameOrTheConfiguredOne(t *testing.T) {
	f := &fakeStore{objects: map[string]int64{"sd15.gguf": 1 << 20}}
	d, cfg := f.deps(t, storeBlock)

	// Named explicitly, so a second store can be inspected without editing a
	// config.
	var out bytes.Buffer
	if err := runStore([]string{"ls", "-config", cfg, "models"}, &out, d); err != nil {
		t.Fatalf("store ls models = %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "sd15.gguf") {
		t.Errorf("got %q", out.String())
	}

	// A name that is not there is a refusal naming the command that makes
	// one, never a silently empty listing.
	out.Reset()
	err := runStore([]string{"ls", "-config", cfg, "nosuchstore"}, &out, d)
	if err == nil {
		t.Fatal("an unknown store listed as though it existed")
	}
	if !strings.Contains(err.Error(), "ember store create") {
		t.Errorf("the error does not name the fix: %v", err)
	}
}

func TestStoreLs_RefusalsBeforeAnythingIsAsked(t *testing.T) {
	f := &fakeStore{}
	d, _ := f.deps(t, storeBlock)
	noStore := storeConfigFor(t, d, "runpod", "")

	// No store anywhere and no argument: a usage error rather than a guess,
	// because there is no "the store" when nothing has named one.
	var out bytes.Buffer
	if err := runStore([]string{"ls", "-config", noStore}, &out, d); ExitCode(err) != ExitUsage {
		t.Errorf("ls with no store configured = %v, want a usage error", err)
	}

	// The same flag-order trap as push and rm: the flag package stops at the
	// first non-flag argument.
	out.Reset()
	cfg := storeConfigFor(t, d, "runpod", storeBlock)
	if err := runStore([]string{"ls", "models", "-config", cfg}, &out, d); ExitCode(err) != ExitUsage {
		t.Errorf("ls with flags after the name = %v, want a usage error", err)
	}
}

func TestStoreLs_RefusesAProviderWithNoS3(t *testing.T) {
	// The local provider has stores but no S3 endpoint: its volumes are on
	// the daemon, not in a datacentre.
	d := labDeps(t)
	cfg := storeConfig(t, d, storeBlock)
	var out bytes.Buffer
	if err := runStore([]string{"create", "-config", cfg}, &out, d); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	err := runStore([]string{"ls", "-config", cfg}, &out, d)
	if err == nil {
		t.Fatal("ls tried to reach an S3 endpoint the local provider does not have")
	}
	if !strings.Contains(err.Error(), "does not expose its stores over S3") {
		t.Errorf("err = %v", err)
	}
}

func TestStoreAbort_RemovesWhatLsFlagged(t *testing.T) {
	old := time.Now().Add(-48 * time.Hour)
	f := &fakeStore{uploads: map[string]fakeUpload{
		"up-1": {key: "flux2.gguf", parts: 3, size: 384 << 20, initiated: old},
		"up-2": {key: "wan22.gguf", parts: 1, size: 128 << 20, initiated: old},
	}}
	d, cfg := f.deps(t, storeBlock)

	var out bytes.Buffer
	if err := runStore([]string{"abort", "-config", cfg}, &out, d); err != nil {
		t.Fatalf("store abort = %v\n%s", err, out.String())
	}
	if len(f.aborted) != 2 {
		t.Errorf("aborted %v, want both uploads", f.aborted)
	}
	if !strings.Contains(out.String(), "aborted 2 of 2") {
		t.Errorf("got %q", out.String())
	}
	// And it says what it freed, because that is the number the charge was
	// being made against.
	if !strings.Contains(out.String(), "512.0 MiB") {
		t.Errorf("abort did not say what it freed: %q", out.String())
	}

	// ls now agrees, and exits 0.
	out.Reset()
	if err := runStore([]string{"ls", "-config", cfg}, &out, d); err != nil {
		t.Errorf("ls still reports findings after abort: %v\n%s", err, out.String())
	}
}

// TestStoreAbort_LeavesAnUploadThatMayStillBeRunning is the restraint, and it
// matters more than the removal: aborting an upload in progress destroys an
// hour of somebody's home link and reports success.
func TestStoreAbort_LeavesAnUploadThatMayStillBeRunning(t *testing.T) {
	f := &fakeStore{uploads: map[string]fakeUpload{
		"up-1": {key: "flux2.gguf", parts: 1, size: 128 << 20, initiated: time.Now().Add(-10 * time.Minute)},
	}}
	d, cfg := f.deps(t, storeBlock)

	var out bytes.Buffer
	if err := runStore([]string{"abort", "-config", cfg}, &out, d); err != nil {
		t.Fatalf("store abort = %v\n%s", err, out.String())
	}
	if len(f.aborted) != 0 {
		t.Errorf("aborted %v; an upload ten minutes old may still be running", f.aborted)
	}
	got := out.String()
	if !strings.Contains(got, "nothing to abort") {
		t.Errorf("got %q", got)
	}
	if !strings.Contains(got, "may still be running") {
		t.Errorf("abort does not explain what it left alone: %q", got)
	}
}

// TestStoreAbort_RefusesANegativeBound. A negative minimum age reads as "no
// bound" and would abort an upload that is still running, which is the one
// outcome the flag exists to prevent — the same reason a negative preview
// interval is refused rather than treated as "off".
func TestStoreAbort_RefusesANegativeBound(t *testing.T) {
	f := &fakeStore{}
	d, cfg := f.deps(t, storeBlock)

	var out bytes.Buffer
	err := runStore([]string{"abort", "-config", cfg, "-older-than", "-1h"}, &out, d)
	if ExitCode(err) != ExitUsage {
		t.Errorf("a negative --older-than = %v, want a usage error", err)
	}
}

func TestStoreAbort_DryRunTouchesNothing(t *testing.T) {
	f := &fakeStore{uploads: map[string]fakeUpload{
		"up-1": {key: "flux2.gguf", parts: 3, size: 384 << 20, initiated: time.Now().Add(-48 * time.Hour)},
	}}
	d, cfg := f.deps(t, storeBlock)

	var out bytes.Buffer
	if err := runStore([]string{"abort", "-config", cfg, "-dry-run"}, &out, d); err != nil {
		t.Fatalf("store abort --dry-run = %v", err)
	}
	if len(f.aborted) != 0 {
		t.Errorf("--dry-run aborted %v", f.aborted)
	}
	if !strings.Contains(out.String(), "would abort 1 upload(s)") {
		t.Errorf("got %q", out.String())
	}
}

// TestStoreAbort_OneRefusalDoesNotStopTheRest. The whole failure is that these
// accumulate, so one upload the endpoint will not let go of must not leave the
// others billing.
func TestStoreAbort_OneRefusalDoesNotStopTheRest(t *testing.T) {
	old := time.Now().Add(-48 * time.Hour)
	f := &fakeStore{
		refuseAbort: "up-1",
		uploads: map[string]fakeUpload{
			"up-1": {key: "flux2.gguf", parts: 1, size: 128 << 20, initiated: old},
			"up-2": {key: "wan22.gguf", parts: 1, size: 128 << 20, initiated: old},
		},
	}
	d, cfg := f.deps(t, storeBlock)

	var out bytes.Buffer
	err := runStore([]string{"abort", "-config", cfg}, &out, d)
	if err == nil {
		t.Fatalf("abort reported success over an upload the endpoint refused:\n%s", out.String())
	}
	if len(f.aborted) != 1 || f.aborted[0] != "up-2" {
		t.Errorf("aborted %v, want the one that could go", f.aborted)
	}
	if !strings.Contains(out.String(), "aborted 1 of 2") {
		t.Errorf("got %q", out.String())
	}
	// The endpoint's own explanation reaches the operator: AccessDenied and
	// SignatureDoesNotMatch are both 403, so the code alone says nothing.
	if !strings.Contains(err.Error(), "AccessDenied") {
		t.Errorf("the error drops the reason: %v", err)
	}
}

// twoAssets names one asset that the fake store has and one it does not, which
// is what makes a verify run report both answers in one pass. Distinct from
// cmd_store_test.go's assetBlock, which names one.
const twoAssets = "assets:\n" +
	"  - url: https://example.invalid/sd15.gguf\n" +
	"    sha256: d44a17e278412bd45c1847a91d30a69146b9bb9144c91a61fa0e357d7e2466af\n" +
	"    as: sd15.gguf\n" +
	"  - url: https://example.invalid/flux2.gguf\n" +
	"    sha256: 0000000000000000000000000000000000000000000000000000000000000000\n" +
	"    as: flux2.gguf\n"

// TestStoreVerify_FindsAMissingAssetBeforeAnythingIsRented.
//
// Today this is discovered on a node that has already been provisioned, had
// its image pulled and its store mounted, all of it billed. One HEAD per asset
// over the store's own endpoint answers it for nothing.
func TestStoreVerify_FindsAMissingAssetBeforeAnythingIsRented(t *testing.T) {
	f := &fakeStore{objects: map[string]int64{"sd15.gguf": 2 << 30}}
	d, cfg := f.deps(t, storeBlock+twoAssets)

	var out bytes.Buffer
	err := runStore([]string{"verify", "-config", cfg}, &out, d)
	if err == nil {
		t.Fatalf("verify exited 0 with an asset missing:\n%s", out.String())
	}
	if ExitCode(err) != ExitFindings {
		t.Errorf("exit = %d, want %d", ExitCode(err), ExitFindings)
	}
	got := out.String()
	if !strings.Contains(got, "present   sd15.gguf") {
		t.Errorf("verify did not report the asset that is there:\n%s", got)
	}
	if !strings.Contains(got, "missing   flux2.gguf") {
		t.Errorf("verify did not report the asset that is not:\n%s", got)
	}
	if !strings.Contains(got, "after paying for it") {
		t.Errorf("verify does not say what the consequence is:\n%s", got)
	}
}

// TestStoreVerify_AnEmptyObjectIsNotAPresentAsset.
//
// A PUT that failed after creating the key leaves a zero-byte object. Under
// `verify: present` the node finds it, calls it there, and loads nothing.
func TestStoreVerify_AnEmptyObjectIsNotAPresentAsset(t *testing.T) {
	f := &fakeStore{objects: map[string]int64{"sd15.gguf": 0, "flux2.gguf": 1 << 30}}
	d, cfg := f.deps(t, storeBlock+twoAssets)

	var out bytes.Buffer
	err := runStore([]string{"verify", "-config", cfg}, &out, d)
	if err == nil {
		t.Fatalf("verify exited 0 over a zero-byte asset:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "empty     sd15.gguf") {
		t.Errorf("verify did not call the empty object empty:\n%s", out.String())
	}
}

func TestStoreVerify_AllPresentExitsZeroAndSaysWhatItDidNotCheck(t *testing.T) {
	f := &fakeStore{objects: map[string]int64{"sd15.gguf": 2 << 30, "flux2.gguf": 14 << 30}}
	d, cfg := f.deps(t, storeBlock+twoAssets)

	var out bytes.Buffer
	if err := runStore([]string{"verify", "-config", cfg}, &out, d); err != nil {
		t.Fatalf("verify = %v\n%s", err, out.String())
	}
	got := out.String()
	if !strings.Contains(got, "all 2 asset(s) are on models") {
		t.Errorf("got %q", got)
	}
	// It must not read as a digest check it did not do. The node still
	// verifies the bytes before the workload, and saying so is what stops
	// this being mistaken for that.
	if !strings.Contains(got, "digests are checked on the node") {
		t.Errorf("verify overstates what it proved:\n%s", got)
	}
}

func TestStoreVerify_RefusesWithNoAssets(t *testing.T) {
	f := &fakeStore{}
	d, cfg := f.deps(t, storeBlock)

	var out bytes.Buffer
	if err := runStore([]string{"verify", "-config", cfg}, &out, d); ExitCode(err) != ExitUsage {
		t.Errorf("verify with no assets = %v, want a usage error", err)
	}
	out.Reset()
	if err := runStore([]string{"verify", "-config", cfg, "extra"}, &out, d); ExitCode(err) != ExitUsage {
		t.Errorf("verify with an argument = %v, want a usage error", err)
	}
}

// TestStoreVerify_CannotAskIsNotTheSameAsNotThere. A 403 means the credentials
// cannot see the store; reporting it as a missing asset would send an operator
// to re-upload forty gigabytes over a permissions problem.
func TestStoreVerify_CannotAskIsNotTheSameAsNotThere(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if strings.Contains(req.URL.Path, "network-volumes") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprint(w, `{"networkVolumes":[{"id":"vol_abc","name":"ember-store-models",`+
				`"size":40,"dataCenter":"EU-RO-1","type":"STANDARD"}],`+
				`"pagination":{"hasNextPage":false}}`)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(api.Close)
	s3srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	t.Cleanup(s3srv.Close)

	d := labDeps(t)
	d.CloudBaseURL = api.URL
	d.S3BaseURL = s3srv.URL
	d.Creds = &config.Credentials{
		RunPod: node.NewSecret([]byte("rp-token")),
		S3:     awssig.Credentials{AccessKey: "u", Secret: node.NewSecret([]byte("k"))},
	}
	cfg := storeConfigFor(t, d, "runpod", storeBlock+twoAssets)

	var out bytes.Buffer
	err := runStore([]string{"verify", "-config", cfg}, &out, d)
	if err == nil {
		t.Fatal("a store ember could not read reported its assets as missing")
	}
	if ExitCode(err) == ExitFindings {
		t.Errorf("exit = %d; an unreachable store must not read as a count of missing assets", ExitCode(err))
	}
	if strings.Contains(out.String(), "missing") {
		t.Errorf("verify called an asset missing over a 403:\n%s", out.String())
	}
}

func TestStoreRead_HelpExplainsWhatEachOneIsFor(t *testing.T) {
	for _, tc := range []struct{ sub, want string }{
		{"ls", "appears in no object listing"},
		{"abort", "bills for them"},
		{"verify", "does not check digests"},
	} {
		var out bytes.Buffer
		_ = runStore([]string{tc.sub, "-h"}, &out, labDeps(t))
		if !strings.Contains(out.String(), tc.want) {
			t.Errorf("`store %s -h` does not mention %q:\n%s", tc.sub, tc.want, out.String())
		}
	}
}

func TestStoreRead_HumanAndStamp(t *testing.T) {
	for n, want := range map[int64]string{
		0:              "0 B",
		512:            "512 B",
		2 << 10:        "2.0 KiB",
		144 << 20:      "144.0 MiB",
		int64(2) << 30: "2.0 GiB",
	} {
		if got := human(n); got != want {
			t.Errorf("human(%d) = %q, want %q", n, got, want)
		}
	}
	// A zero time prints as a dash, not Go's year 1 — which in a column of
	// real dates reads as a file from before the epoch.
	if got := stamp(time.Time{}); got != "-" {
		t.Errorf("stamp(zero) = %q", got)
	}
	if got := stamp(time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)); got != "2026-11-01T09:00:00Z" {
		t.Errorf("stamp = %q", got)
	}
}

// TestStoreLs_AgreesWithTheNumberItPrints.
//
// The single-orphan case is the common one, and "1 have been there" is what a
// reader notices instead of the charge. Checked because the first version got
// it wrong and only printing the real output showed it — the same reason the
// README example is a transcript rather than something typed from memory.
func TestStoreLs_AgreesWithTheNumberItPrints(t *testing.T) {
	old := time.Now().Add(-48 * time.Hour)
	for _, tc := range []struct {
		n    int
		want string
	}{
		{1, "1 has been there longer"},
		{2, "2 have been there longer"},
	} {
		f := &fakeStore{uploads: map[string]fakeUpload{}}
		for i := range tc.n {
			f.uploads[fmt.Sprintf("up-%d", i)] = fakeUpload{
				key: fmt.Sprintf("m%d.gguf", i), parts: 1, size: 1 << 20, initiated: old,
			}
		}
		d, cfg := f.deps(t, storeBlock)

		var out bytes.Buffer
		_ = runStore([]string{"ls", "-config", cfg}, &out, d)
		if !strings.Contains(out.String(), tc.want) {
			t.Errorf("with %d orphans, ls does not say %q:\n%s", tc.n, tc.want, out.String())
		}
	}
	if got := plural(1, "it", "them"); got != "it" {
		t.Errorf("plural(1) = %q", got)
	}
	if got := plural(2, "it", "them"); got != "them" {
		t.Errorf("plural(2) = %q", got)
	}
}
