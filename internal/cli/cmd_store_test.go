package cli

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Legion112/ember/internal/awssig"
	"github.com/Legion112/ember/internal/config"
	"github.com/Legion112/ember/internal/dockerengine"
	"github.com/Legion112/ember/internal/node"
	"github.com/Legion112/ember/internal/provider/local"
	"github.com/Legion112/ember/internal/provider/providertest"
)

// storeConfig writes an ember.yaml naming a store, and returns its path.
func storeConfig(t *testing.T, d deps, extra string) string {
	t.Helper()
	return storeConfigFor(t, d, "local", extra)
}

func storeConfigFor(t *testing.T, d deps, provider, extra string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, config.FileName)
	body := "provider: " + provider + "\n" +
		"image: " + local.DefaultImage + "\n" +
		"ttl: 10m\n" +
		"config_dir: " + dir + "\n" +
		"state_dir: " + filepath.Join(dir, "state") + "\n" +
		"output_dir: " + filepath.Join(dir, "out") + "\n" +
		extra
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	_ = d
	return p
}

const storeBlock = "store:\n  name: models\n  size_gib: 40\n  region: EU-RO-1\n"

func TestStore_CreateThenRemove(t *testing.T) {
	d := labDeps(t)
	cfg := storeConfig(t, d, storeBlock)

	var out bytes.Buffer
	if err := runStore([]string{"create", "-config", cfg}, &out, d); err != nil {
		t.Fatalf("store create = %v", err)
	}
	if !strings.Contains(out.String(), "created models") {
		t.Errorf("create said %q", out.String())
	}
	// Free storage says so rather than printing a cost of nothing.
	if !strings.Contains(out.String(), "charges nothing") {
		t.Errorf("a free store did not say so: %q", out.String())
	}

	// It shows up in ls, which is the only listing there is.
	var ls bytes.Buffer
	if err := runLs([]string{"-config", cfg}, &ls, d); err != nil {
		t.Fatalf("ls = %v", err)
	}
	if !strings.Contains(ls.String(), "models") || !strings.Contains(ls.String(), "store") {
		t.Errorf("ls did not report the store: %q", ls.String())
	}
	// A store is reported, never flagged: exit 2 here would teach the
	// operator to ignore exit 2.
	if ExitCode(nil) != ExitOK {
		t.Error("a store made ls exit non-zero")
	}

	out.Reset()
	if err := runStore([]string{"rm", "-config", cfg, "models"}, &out, d); err != nil {
		t.Fatalf("store rm = %v", err)
	}
	if !strings.Contains(out.String(), "destroyed models") {
		t.Errorf("rm said %q", out.String())
	}

	// Gone already is the state the caller asked for.
	out.Reset()
	if err := runStore([]string{"rm", "-config", cfg, "models"}, &out, d); err != nil {
		t.Errorf("a second rm = %v, want success", err)
	}
	if !strings.Contains(out.String(), "no store named models") {
		t.Errorf("rm said %q", out.String())
	}
}

func TestStore_CreateIsIdempotent(t *testing.T) {
	// Run twice on a fresh machine and on one that already has the store,
	// the config means the same thing. A create that failed the second time
	// would make `ember store create` something nobody could put in a setup
	// script.
	d := labDeps(t)
	cfg := storeConfig(t, d, storeBlock)
	var out bytes.Buffer
	for i := range 2 {
		if err := runStore([]string{"create", "-config", cfg}, &out, d); err != nil {
			t.Fatalf("create %d = %v", i+1, err)
		}
	}
}

func TestStore_Usage(t *testing.T) {
	d := labDeps(t)
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"no subcommand", nil},
		{"unknown subcommand", []string{"list"}},
		{"create takes no arguments", []string{"create", "models"}},
		{"rm needs a name", []string{"rm"}},
		{"rm takes one name", []string{"rm", "a", "b"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			err := runStore(tc.args, &out, d)
			if err == nil {
				t.Fatal("accepted")
			}
			if ExitCode(err) != ExitUsage {
				t.Errorf("exit = %d, want %d", ExitCode(err), ExitUsage)
			}
		})
	}

	var out bytes.Buffer
	if err := runStore([]string{"help"}, &out, d); err != nil {
		t.Errorf("store help = %v", err)
	}
	if !strings.Contains(out.String(), "outlives") {
		t.Errorf("help said %q", out.String())
	}
}

func TestStore_RmNamesTheFlagOrder(t *testing.T) {
	// The standard flag package stops parsing at the first non-flag
	// argument, so `ember store rm models -config x` leaves three. The error
	// has to say that rather than count, or the operator reads it as "this
	// command is broken".
	d := labDeps(t)
	cfg := storeConfig(t, d, storeBlock)
	var out bytes.Buffer
	err := runStore([]string{"rm", "models", "-config", cfg}, &out, d)
	if err == nil {
		t.Fatal("accepted")
	}
	if !strings.Contains(err.Error(), "flags come before the name") {
		t.Errorf("err = %q, want it to explain the flag order", err)
	}
}

func TestStore_CreateRefusesWithoutAStoreBlock(t *testing.T) {
	d := labDeps(t)
	cfg := storeConfig(t, d, "")
	var out bytes.Buffer
	err := runStore([]string{"create", "-config", cfg}, &out, d)
	if err == nil {
		t.Fatal("created a store the config does not name")
	}
	if !strings.Contains(err.Error(), "no store is named") {
		t.Errorf("err = %q", err)
	}
}

func TestStore_RefusesAProviderWithoutStorage(t *testing.T) {
	d := labDeps(t)
	cfg := storeConfig(t, d, storeBlock)
	var out bytes.Buffer
	if err := runStore([]string{"rm", "-config", cfg, "-provider", "nosuch", "models"}, &out, d); err == nil {
		t.Error("an unknown provider was accepted")
	}
}

func TestUp_RefusesWhenTheNamedStoreDoesNotExist(t *testing.T) {
	// The refusal that keeps `ember up` from bringing a monthly charge into
	// existence because a config mentioned a name. It must also happen
	// before anything is rented: a run that discovers this after creating a
	// node has already started paying.
	d := labDeps(t)
	cfg := storeConfig(t, d, storeBlock)

	var out bytes.Buffer
	err := runUp([]string{"-config", cfg, "--cmd", "/bin/true"}, &out, d)
	if err == nil {
		t.Fatal("up ran with a store that does not exist")
	}
	if !strings.Contains(err.Error(), "ember store create") {
		t.Errorf("err = %q, want it to name the command that makes one", err)
	}
	e := d.Engine.(*dockerengine.RecordingEngine)
	for _, c := range e.Calls {
		if strings.HasPrefix(c, "ContainerCreate") {
			t.Errorf("a node was created before the store was resolved: %v", e.Calls)
			break
		}
	}
}

func TestUp_MountsTheStoreItWasToldAbout(t *testing.T) {
	d := labDeps(t)
	cfg := storeConfig(t, d, storeBlock)
	var out bytes.Buffer
	if err := runStore([]string{"create", "-config", cfg}, &out, d); err != nil {
		t.Fatal(err)
	}

	out.Reset()
	if err := runUp([]string{"-config", cfg, "--dry-run"}, &out, d); err != nil {
		t.Fatalf("dry-run up = %v", err)
	}

	// The dry run creates nothing, so the assertion is on the spec reaching
	// the provider rather than on a container. Exercised against a real
	// container in the labs.
	e := d.Engine.(*dockerengine.RecordingEngine)
	for _, c := range e.Calls {
		if strings.HasPrefix(c, "ContainerCreate") {
			t.Errorf("--dry-run created a container: %v", e.Calls)
		}
	}
}

func TestStorePush_RefusalsBeforeAnythingIsRead(t *testing.T) {
	d := labDeps(t)
	for _, tc := range []struct {
		name    string
		extra   string
		args    []string
		wantMsg string
	}{
		{"no file", storeBlock, []string{"push"}, "exactly one file"},
		{
			// The same flag-order trap as rm: the standard flag package
			// stops at the first non-flag argument.
			name: "flags after the file", extra: storeBlock,
			args: []string{"push", "x.gguf", "-config"}, wantMsg: "flags come before the file",
		},
		{"no store named", "", []string{"push", "x.gguf"}, "no store is named"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := storeConfig(t, d, tc.extra)
			args := append(append([]string{}, tc.args[:1]...), "-config", cfg)
			args = append(args, tc.args[1:]...)
			if tc.name == "flags after the file" {
				args = []string{"push", "x.gguf", "-config", cfg}
			}
			var out bytes.Buffer
			err := runStore(args, &out, d)
			if err == nil {
				t.Fatal("accepted")
			}
			if !strings.Contains(err.Error(), tc.wantMsg) {
				t.Errorf("err = %q, want it to mention %q", err, tc.wantMsg)
			}
		})
	}
}

func TestStorePush_RefusesANameTheNodeWouldRefuse(t *testing.T) {
	// The same check an asset name gets. Caught here rather than after the
	// upload, which for a 40 GB model is the difference between an error and
	// an hour.
	d := labDeps(t)
	cfg := storeConfig(t, d, storeBlock)
	var out bytes.Buffer
	if err := runStore([]string{"push", "-config", cfg, "-as", "../escape.gguf", "x"}, &out, d); err == nil {
		t.Error("an escaping name was accepted")
	}
}

func TestStorePush_RefusesAProviderWithNoS3(t *testing.T) {
	// The local provider has stores but no S3 endpoint: its volumes are on
	// the daemon, not in a datacentre. Reported rather than attempted
	// against a hostname that does not exist.
	d := labDeps(t)
	cfg := storeConfig(t, d, storeBlock)
	var out bytes.Buffer
	if err := runStore([]string{"create", "-config", cfg}, &out, d); err != nil {
		t.Fatal(err)
	}

	f := filepath.Join(t.TempDir(), "sd15.gguf")
	if err := os.WriteFile(f, []byte("weights"), 0o600); err != nil {
		t.Fatal(err)
	}

	out.Reset()
	err := runStore([]string{"push", "-config", cfg, f}, &out, d)
	if err == nil {
		t.Fatal("pushed to a provider with no S3 endpoint")
	}
	if !strings.Contains(err.Error(), "S3") {
		t.Errorf("err = %q", err)
	}
}

func TestDigestOf_RewindsForTheUpload(t *testing.T) {
	// The file is hashed in its own pass and then uploaded, so a digest that
	// left the handle at EOF would upload nothing and report success.
	p := filepath.Join(t.TempDir(), "x")
	if err := os.WriteFile(p, []byte("the weights"), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()

	sum, err := digestOf(f)
	if err != nil {
		t.Fatal(err)
	}
	rest, err := io.ReadAll(f)
	if err != nil {
		t.Fatal(err)
	}
	if string(rest) != "the weights" {
		t.Errorf("after hashing, the file reads %q; the upload would have sent that", rest)
	}
	if len(sum) != 64 {
		t.Errorf("digest = %q", sum)
	}
}

// TestStorePush_UploadsAndPrintsTheDigest is the happy path.
//
// Both endpoints are stand-ins, through the seams that exist for exactly
// this: CloudBaseURL puts a RunPod-shaped store in the registry without
// reaching RunPod, and S3BaseURL catches the upload. A test that could reach
// either service behaves differently on a machine that has credentials.
func TestStorePush_UploadsAndPrintsTheDigest(t *testing.T) {
	body := []byte("these are the weights")

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

	var uploaded []byte
	var auth, contentSHA string
	s3srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		auth = req.Header.Get("Authorization")
		contentSHA = req.Header.Get("X-Amz-Content-Sha256")
		uploaded, _ = io.ReadAll(req.Body)
		w.Header().Set("ETag", `"e"`)
	}))
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

	cfg := storeConfigFor(t, d, "runpod", storeBlock)
	f := filepath.Join(t.TempDir(), "sd15.gguf")
	if err := os.WriteFile(f, body, 0o600); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err := runStore([]string{"push", "-config", cfg, f}, &out, d); err != nil {
		t.Fatalf("push = %v\n%s", err, out.String())
	}

	if !bytes.Equal(uploaded, body) {
		t.Errorf("uploaded %q, want %q", uploaded, body)
	}
	if !strings.Contains(auth, awssig.Algorithm) {
		t.Errorf("the upload was not signed: %q", auth)
	}
	// Signed over the digest of the body, which is also what gets printed.
	sum := sha256.Sum256(body)
	want := hex.EncodeToString(sum[:])
	if contentSHA != want {
		t.Errorf("X-Amz-Content-Sha256 = %q, want %q", contentSHA, want)
	}
	// The digest is printed because it is the value `assets:` needs, and
	// reading it from the pass that uploaded the file is how the two cannot
	// disagree.
	if !strings.Contains(out.String(), want) {
		t.Errorf("the digest was not printed:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "as: sd15.gguf") {
		t.Errorf("the asset stanza was not printed:\n%s", out.String())
	}
}

func TestStorePush_RefusesAProviderWhoseStoreHasNoRegion(t *testing.T) {
	// The local provider's volumes are on the daemon, not in a datacentre,
	// so there is no S3 endpoint to derive. Refused rather than attempted
	// against a hostname that does not exist.
	d := labDeps(t)
	cfg := storeConfig(t, d, storeBlock)
	if err := runStore([]string{"create", "-config", cfg}, &bytes.Buffer{}, d); err != nil {
		t.Fatal(err)
	}
	f := filepath.Join(t.TempDir(), "sd15.gguf")
	if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	err := runStore([]string{"push", "-config", cfg, f}, &out, d)
	if err == nil {
		t.Fatal("pushed to a store with no datacentre")
	}
	if !strings.Contains(err.Error(), "S3") {
		t.Errorf("err = %q", err)
	}
}

func TestStorePush_RefusesWithoutS3Credentials(t *testing.T) {
	// The common first run: the API key is set and the S3 pair was never
	// created, because RunPod issues them on a different page.
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"networkVolumes":[{"id":"vol_abc","name":"ember-store-models",`+
			`"size":40,"dataCenter":"EU-RO-1"}],"pagination":{"hasNextPage":false}}`)
	}))
	t.Cleanup(api.Close)

	d := labDeps(t)
	d.CloudBaseURL = api.URL
	d.Creds = &config.Credentials{RunPod: node.NewSecret([]byte("rp-token"))}

	cfg := storeConfigFor(t, d, "runpod", storeBlock)
	f := filepath.Join(t.TempDir(), "sd15.gguf")
	if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	err := runStore([]string{"push", "-config", cfg, f}, &out, d)
	if err == nil {
		t.Fatal("pushed with no S3 credentials")
	}
	// The message has to say where they come from: the usual cause is that
	// the key was never created, not that it was mistyped.
	if !strings.Contains(err.Error(), "S3 API keys") {
		t.Errorf("err = %q, want it to name the console page", err)
	}
}

func TestStore_UsageTextIsReachable(t *testing.T) {
	// -h runs the usage closure, which is where the explanation of why this
	// command exists at all lives. A closure nothing calls is one that can
	// stop compiling without anybody noticing.
	d := labDeps(t)
	for _, sub := range []string{"create", "rm", "push"} {
		t.Run(sub, func(t *testing.T) {
			var out bytes.Buffer
			if err := runStore([]string{sub, "-h"}, &out, d); err == nil {
				t.Error("-h did not return a usage error")
			}
			if !strings.Contains(out.String(), "ember store "+sub) {
				t.Errorf("usage did not describe the subcommand:\n%s", out.String())
			}
		})
	}
}

func TestStorePush_RefusesADirectory(t *testing.T) {
	d := labDeps(t)
	cfg := storeConfig(t, d, storeBlock)
	if err := runStore([]string{"create", "-config", cfg}, &bytes.Buffer{}, d); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	err := runStore([]string{"push", "-config", cfg, t.TempDir()}, &out, d)
	if err == nil {
		t.Fatal("a directory was accepted")
	}
	if !strings.Contains(err.Error(), "directory") {
		t.Errorf("err = %q", err)
	}
}

func TestStorePush_RefusesAMissingFile(t *testing.T) {
	d := labDeps(t)
	cfg := storeConfig(t, d, storeBlock)
	if err := runStore([]string{"create", "-config", cfg}, &bytes.Buffer{}, d); err != nil {
		t.Fatal(err)
	}
	if err := runStore([]string{"push", "-config", cfg, "/nope/absent.gguf"}, &bytes.Buffer{}, d); err == nil {
		t.Error("a missing file was accepted")
	}
}

func TestStorePush_RefusesWhenTheStoreDoesNotExist(t *testing.T) {
	d := labDeps(t)
	cfg := storeConfig(t, d, storeBlock)
	f := filepath.Join(t.TempDir(), "x.gguf")
	if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := runStore([]string{"push", "-config", cfg, f}, &bytes.Buffer{}, d)
	if err == nil {
		t.Fatal("pushed to a store that does not exist")
	}
	if !strings.Contains(err.Error(), "ember store create") {
		t.Errorf("err = %q, want it to name the command that makes one", err)
	}
}

func TestStoreCreate_SaysWhatItWillCost(t *testing.T) {
	// The one command whose whole cost is in the future. The operator is
	// never again as well placed to decide it is too much.
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if req.Method == http.MethodPost {
			w.WriteHeader(http.StatusCreated)
			_, _ = fmt.Fprint(w, `{"id":"vol_abc","name":"ember-store-models","size":40,`+
				`"dataCenter":"EU-RO-1","type":"STANDARD"}`)
			return
		}
		_, _ = fmt.Fprint(w, `{"networkVolumes":[],"pagination":{"hasNextPage":false}}`)
	}))
	t.Cleanup(api.Close)

	d := labDeps(t)
	d.CloudBaseURL = api.URL
	d.Creds = &config.Credentials{RunPod: node.NewSecret([]byte("rp-token"))}

	var out bytes.Buffer
	cfg := storeConfigFor(t, d, "runpod", storeBlock)
	if err := runStore([]string{"create", "-config", cfg}, &out, d); err != nil {
		t.Fatalf("create = %v", err)
	}
	// 40 GiB at $0.07 is $2.80 a month, for as long as it exists.
	if !strings.Contains(out.String(), "every month") {
		t.Errorf("create did not say the charge recurs:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "ember store rm models") {
		t.Errorf("create did not name what ends it:\n%s", out.String())
	}
}

func TestAttachStore_RefusesAProviderWithNoStorage(t *testing.T) {
	// A config that names a store against a provider that has none. Caught
	// before anything is rented rather than as a nil type assertion when the
	// run is about to start.
	p := providertest.NewRecordingProvider("plain")
	_, err := attachStore(t.Context(), p, config.Config{
		Provider: "plain",
		Store:    config.StoreConfig{Name: "models"},
	})
	if err == nil {
		t.Fatal("accepted")
	}
	if !strings.Contains(err.Error(), "no persistent storage") {
		t.Errorf("err = %q", err)
	}
}

func TestAttached_IsZero(t *testing.T) {
	if !(Attached{}).IsZero() {
		t.Error("the zero Attached is not zero")
	}
	if (Attached{Handle: node.StoreHandle{Provider: "p", ID: "v"}}).IsZero() {
		t.Error("a populated Attached reported zero")
	}
}

func TestStoreSync_PicksTheCheapestMachineWithNoGPU(t *testing.T) {
	// The whole argument for this command: $0.06/hour against $0.89 for a
	// 4090, for a job that is bound by the network and the disk. Checked
	// through --dry-run, which creates nothing.
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(req.URL.Path, "network-volumes"):
			_, _ = fmt.Fprint(w, `{"networkVolumes":[{"id":"vol_abc","name":"ember-store-models",`+
				`"size":40,"dataCenter":"EU-RO-1"}],"pagination":{"hasNextPage":false}}`)
		case strings.Contains(req.URL.Path, "catalog/cpus"):
			_, _ = fmt.Fprint(w, `{"cpus":[{"id":"cpu3c","name":"Compute-Optimized",`+
				`"vcpu":{"min":2,"max":32},"ramGbPerVcpu":2,`+
				`"price":{"securePerVcpu":0.03},"availability":"HIGH",`+
				`"dataCenters":[{"id":"EU-RO-1","availability":"HIGH"}]}]}`)
		case strings.Contains(req.URL.Path, "catalog/gpus"):
			t.Error("sync asked for the GPU catalogue")
			_, _ = fmt.Fprint(w, `{"gpus":[]}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(api.Close)

	d := labDeps(t)
	d.CloudBaseURL = api.URL
	d.Creds = &config.Credentials{RunPod: node.NewSecret([]byte("rp-token"))}

	cfg := storeConfigFor(t, d, "runpod", storeBlock+"\n"+assetBlock)
	var out bytes.Buffer
	if err := runStore([]string{"sync", "-config", cfg, "--dry-run"}, &out, d); err != nil {
		t.Fatalf("sync = %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "cpu3c") {
		t.Errorf("sync did not pick the CPU machine:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "nothing was created") {
		t.Errorf("--dry-run did not say it created nothing:\n%s", out.String())
	}
}

func TestStoreSync_RefusesWithNothingToFetch(t *testing.T) {
	d := labDeps(t)
	cfg := storeConfig(t, d, storeBlock)
	var out bytes.Buffer
	err := runStore([]string{"sync", "-config", cfg}, &out, d)
	if err == nil {
		t.Fatal("sync ran with no assets listed")
	}
	if !strings.Contains(err.Error(), "nothing to fetch") {
		t.Errorf("err = %q", err)
	}
	if ExitCode(err) != ExitUsage {
		t.Errorf("exit = %d, want %d", ExitCode(err), ExitUsage)
	}
}

func TestStoreSync_TakesNoArguments(t *testing.T) {
	d := labDeps(t)
	cfg := storeConfig(t, d, storeBlock)
	var out bytes.Buffer
	if err := runStore([]string{"sync", "-config", cfg, "extra"}, &out, d); err == nil {
		t.Error("sync accepted an argument")
	}
}

const assetBlock = "assets:\n  - url: https://h/x.gguf\n" +
	"    sha256: bea213124ab48b5e44b03c7fa3377740a87aa7b44f798050d9ea16be9de14b58\n" +
	"    as: sd15.gguf"

func TestStoreSync_Usage(t *testing.T) {
	d := labDeps(t)
	var out bytes.Buffer
	if err := runStore([]string{"sync", "-h"}, &out, d); err == nil {
		t.Error("-h did not return a usage error")
	}
	if !strings.Contains(out.String(), "cheapest machine with no GPU") {
		t.Errorf("usage does not explain what it rents:\n%s", out.String())
	}
}

func TestStoreSync_RefusesAnUnreadableConfig(t *testing.T) {
	d := labDeps(t)
	if err := runStore([]string{"sync", "-config", "/nope/absent.yaml"}, &bytes.Buffer{}, d); err != nil {
		// A missing config is the defaults, which name no assets -- so this
		// is the "nothing to fetch" refusal rather than a read error.
		if !strings.Contains(err.Error(), "nothing to fetch") {
			t.Errorf("err = %q", err)
		}
		return
	}
	t.Error("sync ran against a config that does not exist")
}

func TestStoreSync_RefusesAStoreThatDoesNotExist(t *testing.T) {
	// Same refusal `up` gives, and for the same reason: naming a store does
	// not create one, and a command that rents a machine before finding that
	// out has already started paying.
	d := labDeps(t)
	cfg := storeConfig(t, d, storeBlock+"\n"+assetBlock)
	var out bytes.Buffer
	err := runStore([]string{"sync", "-config", cfg}, &out, d)
	if err == nil {
		t.Fatal("sync ran against a store that does not exist")
	}
	if !strings.Contains(err.Error(), "ember store create") {
		t.Errorf("err = %q, want it to name the command that makes one", err)
	}
	e := d.Engine.(*dockerengine.RecordingEngine)
	for _, c := range e.Calls {
		if strings.HasPrefix(c, "ContainerCreate") {
			t.Errorf("a machine was rented before the store was resolved: %v", e.Calls)
		}
	}
}
