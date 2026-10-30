package assets

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Legion112/ember/internal/node"
)

func digest(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// serve returns a server handing out body, and a count of how many times it
// was asked. The count is the assertion for "did not re-download".
func serve(t *testing.T, body []byte) (string, *int) {
	t.Helper()
	hits := 0
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		_, _ = w.Write(body)
	}))
	t.Cleanup(s.Close)
	return s.URL + "/weights.gguf", &hits
}

func TestEnsure_FetchesVerifiesAndPlaces(t *testing.T) {
	body := []byte("the weights")
	url, hits := serve(t, body)
	dir := t.TempDir()

	var out strings.Builder
	res, err := Ensure(t.Context(), nil, dir,
		node.Asset{URL: url, SHA256: digest(body), As: "sd15.gguf"}, VerifyAlways, &out)
	if err != nil {
		t.Fatalf("Ensure = %v", err)
	}
	if !res.Fetched || !res.Verified || res.Bytes != int64(len(body)) {
		t.Errorf("result = %+v", res)
	}

	got, err := os.ReadFile(filepath.Join(dir, "sd15.gguf"))
	if err != nil || string(got) != string(body) {
		t.Fatalf("on the store: %q, %v", got, err)
	}
	if *hits != 1 {
		t.Errorf("fetched %d times", *hits)
	}

	// Nothing partial is left behind. A partial on the store is a file
	// nothing cleans up and the operator pays to keep.
	assertOnlyFiles(t, dir, "sd15.gguf")
}

func TestEnsure_DoesNotRefetchWhatIsAlreadyThere(t *testing.T) {
	body := []byte("the weights")
	url, hits := serve(t, body)
	dir := t.TempDir()
	a := node.Asset{URL: url, SHA256: digest(body), As: "sd15.gguf"}

	for i := range 3 {
		res, err := Ensure(t.Context(), nil, dir, a, VerifyAlways, nil)
		if err != nil {
			t.Fatalf("run %d: %v", i+1, err)
		}
		if i > 0 && res.Fetched {
			t.Errorf("run %d fetched again", i+1)
		}
		if !res.Verified {
			t.Errorf("run %d did not verify", i+1)
		}
	}
	if *hits != 1 {
		t.Errorf("the server was asked %d times; the whole point is that it is asked once", *hits)
	}
}

func TestEnsure_RefusesTamperedBytesBeforeAnythingExpensive(t *testing.T) {
	// The control that replaces a read-only mount. The store is writable by
	// every node that mounts it, so one workload can leave something behind
	// that the next one would load as its model.
	body := []byte("the weights")
	url, hits := serve(t, body)
	dir := t.TempDir()
	a := node.Asset{URL: url, SHA256: digest(body), As: "sd15.gguf"}

	if _, err := Ensure(t.Context(), nil, dir, a, VerifyAlways, nil); err != nil {
		t.Fatal(err)
	}

	// Something else writes to the store.
	full := filepath.Join(dir, "sd15.gguf")
	if err := os.WriteFile(full, []byte("not the weights"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := Ensure(t.Context(), nil, dir, a, VerifyAlways, nil)
	if err == nil {
		t.Fatal("tampered bytes were accepted")
	}
	if !strings.Contains(err.Error(), "does not match its digest") {
		t.Errorf("err = %q", err)
	}
	// Not repaired by re-downloading. Something wrote to the store and what
	// did is not knowable from here; what is knowable is that this is the
	// moment to stop, not the moment to guess.
	if *hits != 1 {
		t.Errorf("a tampered file triggered a re-download (%d hits); it must refuse instead", *hits)
	}
	if got, _ := os.ReadFile(full); string(got) != "not the weights" {
		t.Error("the tampered file was overwritten rather than reported")
	}
}

func TestEnsure_VerifyPresentSkipsTheReRead(t *testing.T) {
	// The documented trade: about forty seconds on a forty-gigabyte set,
	// against detecting a store somebody else wrote to.
	body := []byte("the weights")
	url, _ := serve(t, body)
	dir := t.TempDir()
	a := node.Asset{URL: url, SHA256: digest(body), As: "sd15.gguf"}

	if _, err := Ensure(t.Context(), nil, dir, a, VerifyAlways, nil); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sd15.gguf"), []byte("tampered"), 0o600); err != nil {
		t.Fatal(err)
	}

	res, err := Ensure(t.Context(), nil, dir, a, VerifyPresent, nil)
	if err != nil {
		t.Fatalf("VerifyPresent refused a present file: %v", err)
	}
	if res.Verified {
		t.Error("VerifyPresent reported the file as verified")
	}
}

func TestEnsure_RefusesABodyThatDoesNotMatch(t *testing.T) {
	// The server serves something other than what was asked for. Nothing is
	// written to the store: a short or wrong download left at the final name
	// is what the next run would call "present".
	url, _ := serve(t, []byte("something else entirely"))
	dir := t.TempDir()

	_, err := Ensure(t.Context(), nil, dir,
		node.Asset{URL: url, SHA256: digest([]byte("the weights")), As: "sd15.gguf"}, VerifyAlways, nil)
	if err == nil {
		t.Fatal("a mismatched download was accepted")
	}
	if !strings.Contains(err.Error(), "Nothing was written to the store") {
		t.Errorf("err = %q, want it to say the store is untouched", err)
	}
	// Nothing at all: not the file, and not a partial either.
	assertOnlyFiles(t, dir)
}

func TestEnsure_ReportsAFailedRequest(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(s.Close)
	dir := t.TempDir()

	_, err := Ensure(t.Context(), nil, dir,
		node.Asset{URL: s.URL + "/x", SHA256: digest([]byte("x")), As: "a.gguf"}, VerifyAlways, nil)
	if err == nil {
		t.Fatal("a 404 was accepted")
	}
	if !strings.Contains(err.Error(), "404") {
		t.Errorf("err = %q, want the server's own status", err)
	}
}

func TestEnsure_RefusesAnInvalidAsset(t *testing.T) {
	dir := t.TempDir()
	for _, tc := range []struct {
		name  string
		asset node.Asset
	}{
		{"no digest", node.Asset{URL: "https://h/x", As: "a.gguf"}},
		{"escaping name", node.Asset{URL: "https://h/x", SHA256: digest(nil), As: "../a.gguf"}},
		{"bad scheme", node.Asset{URL: "file:///etc/passwd", SHA256: digest(nil), As: "a.gguf"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Ensure(t.Context(), nil, dir, tc.asset, VerifyAlways, nil); err == nil {
				t.Error("accepted")
			}
		})
	}
}

func TestEnsure_RefusesToReplaceADirectory(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "sd15.gguf"), 0o700); err != nil {
		t.Fatal(err)
	}
	_, err := Ensure(t.Context(), nil, dir,
		node.Asset{URL: "https://h/x", SHA256: digest(nil), As: "sd15.gguf"}, VerifyAlways, nil)
	if err == nil {
		t.Fatal("a directory in the way was accepted")
	}
	if !strings.Contains(err.Error(), "directory") {
		t.Errorf("err = %q", err)
	}
}

func TestEnsure_LandsInASubdirectory(t *testing.T) {
	// A model set is several files, and naming them wan/high-noise.gguf is
	// how they stay apart on one store.
	body := []byte("expert")
	url, _ := serve(t, body)
	dir := t.TempDir()

	if _, err := Ensure(t.Context(), nil, dir,
		node.Asset{URL: url, SHA256: digest(body), As: "wan/high-noise.gguf"}, VerifyAlways, nil); err != nil {
		t.Fatalf("Ensure = %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "wan", "high-noise.gguf")); err != nil {
		t.Errorf("not where it was asked for: %v", err)
	}
}

func TestParsePolicy(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want Policy
		ok   bool
	}{
		{"", DefaultPolicy, true},
		{"always", VerifyAlways, true},
		{"present", VerifyPresent, true},
		// A misspelling must not quietly become the weaker setting, which is
		// the direction a typo would go unnoticed in.
		{"alwyas", "", false},
		{"never", "", false},
		{"ALWAYS", "", false},
	} {
		t.Run(tc.in, func(t *testing.T) {
			got, err := ParsePolicy(tc.in)
			if tc.ok != (err == nil) {
				t.Fatalf("ParsePolicy(%q) = %v, %v", tc.in, got, err)
			}
			if tc.ok && got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
	// The default is the strict one. The run it protects costs dollars and
	// the check costs cents.
	if DefaultPolicy != VerifyAlways {
		t.Errorf("DefaultPolicy = %q", DefaultPolicy)
	}
}

func TestCommand_MatchesWhatFetchParses(t *testing.T) {
	mount, err := node.MountAt("/models")
	if err != nil {
		t.Fatal(err)
	}
	a := node.Asset{URL: "https://h/x.gguf", SHA256: digest([]byte("x")), As: "sd15.gguf"}

	cmd, err := Command(mount, a, VerifyPresent)
	if err != nil {
		t.Fatalf("Command = %v", err)
	}
	if cmd.Path != NodeBinary {
		t.Errorf("path = %q", cmd.Path)
	}

	// The flags this writes are the flags cmd/embernode reads. They are
	// asserted by name because a rename on one side and not the other is a
	// node that fetches nothing and a workload that finds an empty directory.
	want := map[string]string{
		"--dir":    "/models",
		"--url":    a.URL,
		"--sha256": a.SHA256,
		"--as":     a.As,
		"--verify": string(VerifyPresent),
	}
	got := map[string]string{}
	for i := 0; i+1 < len(cmd.Args); i += 2 {
		got[cmd.Args[i]] = cmd.Args[i+1]
	}
	if cmd.Args[0] != "fetch" {
		t.Fatalf("args = %v, want fetch first", cmd.Args)
	}
	for i := 1; i+1 < len(cmd.Args); i += 2 {
		got[cmd.Args[i]] = cmd.Args[i+1]
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q (args: %v)", k, got[k], v, cmd.Args)
		}
	}

	// It must survive quoting into a real shell, which a prompt-bearing URL
	// is the usual way to break.
	if s := cmd.String(); !strings.Contains(s, "embernode fetch") {
		t.Errorf("String() = %q", s)
	}
}

func TestCommand_Refusals(t *testing.T) {
	mount, err := node.MountAt("/models")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Command(node.Mount{}, node.Asset{URL: "https://h/x", SHA256: digest(nil), As: "a"}, VerifyAlways); err == nil {
		t.Error("a command with no mount was built")
	}
	if _, err := Command(mount, node.Asset{URL: "https://h/x", As: "a"}, VerifyAlways); err == nil {
		t.Error("a command for an asset with no digest was built")
	}
	// An empty policy is the default rather than an error, so a caller that
	// never set one gets the strict check.
	cmd, err := Command(mount, node.Asset{URL: "https://h/x", SHA256: digest(nil), As: "a"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(cmd.String(), string(DefaultPolicy)) {
		t.Errorf("an unset policy did not become the default: %q", cmd.String())
	}
}

func TestDefaultClient_HasNoOverallTimeout(t *testing.T) {
	// A forty-gigabyte checkpoint legitimately takes minutes, and a client
	// timeout would turn a working download into a failure nobody could
	// explain. The bounds are on the parts that hang without transferring.
	c := DefaultClient()
	if c.Timeout != 0 {
		t.Errorf("Timeout = %v, want none", c.Timeout)
	}
	tr, ok := c.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport = %T", c.Transport)
	}
	if tr.ResponseHeaderTimeout == 0 {
		t.Error("no ResponseHeaderTimeout: a server that accepts and never answers would hang the run")
	}
}

// assertOnlyFiles fails if dir holds anything other than the named files.
//
// It exists for the partials. A download is written to a temp name beside its
// destination and renamed only once the digest matches, so every path out of
// download must remove it -- and a leftover partial on a store is a file
// nothing cleans up and the operator pays by the month to keep.
func assertOnlyFiles(t *testing.T, dir string, names ...string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range entries {
		got = append(got, e.Name())
	}
	want := map[string]bool{}
	for _, n := range names {
		want[n] = true
	}
	for _, g := range got {
		if !want[g] {
			t.Errorf("%s holds %q, which nothing put there on purpose (all: %v)", dir, g, got)
		}
	}
	if len(got) != len(names) {
		t.Errorf("%s holds %v, want exactly %v", dir, got, names)
	}
}

func TestEnsure_DefaultsToTheStrictPolicy(t *testing.T) {
	// An empty policy is the strict one, not the weak one. The caller that
	// forgot to set it gets the check, which is the direction a mistake
	// should go.
	body := []byte("the weights")
	url, _ := serve(t, body)
	dir := t.TempDir()
	a := node.Asset{URL: url, SHA256: digest(body), As: "sd15.gguf"}

	if _, err := Ensure(t.Context(), nil, dir, a, "", nil); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sd15.gguf"), []byte("tampered"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Ensure(t.Context(), nil, dir, a, "", nil); err == nil {
		t.Error("an unset policy skipped the check")
	}
}

func TestEnsure_ReportsAStoreItCannotRead(t *testing.T) {
	// The store is a mount that may not be there, may be full, or may be
	// read-only. Each of those has to be reported rather than fallen through,
	// because a fetch that silently did nothing is a workload that starts
	// against weights which are not there.
	dir := filepath.Join(t.TempDir(), "notadir")
	if err := os.WriteFile(dir, []byte("this is a file"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Ensure(t.Context(), nil, dir,
		node.Asset{URL: "https://h/x", SHA256: digest(nil), As: "a.gguf"}, VerifyAlways, nil)
	if err == nil {
		t.Fatal("a store that is a file was accepted")
	}
	if !strings.Contains(err.Error(), "make room") {
		t.Errorf("err = %q", err)
	}
}

func TestEnsure_ReportsAFileItCannotHash(t *testing.T) {
	// Present but unreadable. VerifyAlways has to read it, and "I could not
	// check" is not "it is fine".
	dir := t.TempDir()
	full := filepath.Join(dir, "sd15.gguf")
	if err := os.WriteFile(full, []byte("x"), 0o000); err != nil {
		t.Fatal(err)
	}
	if os.Geteuid() == 0 {
		t.Skip("running as root, which can read a 0000 file")
	}
	_, err := Ensure(t.Context(), nil, dir,
		node.Asset{URL: "https://h/x", SHA256: digest([]byte("x")), As: "sd15.gguf"}, VerifyAlways, nil)
	if err == nil {
		t.Fatal("an unreadable file was accepted")
	}
	if !strings.Contains(err.Error(), "read sd15.gguf") {
		t.Errorf("err = %q", err)
	}
}

func TestEnsure_ReportsAnUnreachableServer(t *testing.T) {
	dir := t.TempDir()
	// Port 9 is discard; nothing answers HTTP there.
	_, err := Ensure(t.Context(), nil, dir,
		node.Asset{URL: "http://127.0.0.1:9/never", SHA256: digest(nil), As: "a.gguf"}, VerifyAlways, nil)
	if err == nil {
		t.Fatal("an unreachable server was accepted")
	}
	if !strings.Contains(err.Error(), "fetch a.gguf") {
		t.Errorf("err = %q, want it to name the asset", err)
	}
	// And nothing was left on the store.
	assertOnlyFiles(t, dir)
}

func TestEnsure_ReportsABadRequestURL(t *testing.T) {
	// A URL that validates but cannot become a request. Reported rather than
	// panicking somewhere lower down.
	dir := t.TempDir()
	_, err := Ensure(t.Context(), nil, dir,
		node.Asset{URL: "http://host\x7f/x", SHA256: digest(nil), As: "a.gguf"}, VerifyAlways, nil)
	if err == nil {
		t.Error("a malformed request URL was accepted")
	}
}

func TestEnsure_RefusesATruncatedResponse(t *testing.T) {
	// The server promises a length and delivers less. This is what a
	// connection dropped mid-download looks like, and the file must not be
	// left where the next run would call it "present" -- under VerifyPresent
	// that is a short file loaded as a model.
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "10000")
		_, _ = w.Write([]byte("only a little"))
	}))
	t.Cleanup(s.Close)
	dir := t.TempDir()

	_, err := Ensure(t.Context(), nil, dir,
		node.Asset{URL: s.URL + "/x", SHA256: digest([]byte("whatever")), As: "sd15.gguf"}, VerifyAlways, nil)
	if err == nil {
		t.Fatal("a truncated download was accepted")
	}
	assertOnlyFiles(t, dir)
}

func TestEnsure_ReportsAStoreItCannotWriteTo(t *testing.T) {
	// A full or read-only volume. The partial cannot be created, and that
	// has to be an error rather than a fetch that silently did nothing.
	if os.Geteuid() == 0 {
		t.Skip("running as root, which can write to a read-only directory")
	}
	body := []byte("the weights")
	url, _ := serve(t, body)
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	_, err := Ensure(t.Context(), nil, dir,
		node.Asset{URL: url, SHA256: digest(body), As: "sd15.gguf"}, VerifyAlways, nil)
	if err == nil {
		t.Fatal("a read-only store was accepted")
	}
	if !strings.Contains(err.Error(), "partial file") {
		t.Errorf("err = %q", err)
	}
}
