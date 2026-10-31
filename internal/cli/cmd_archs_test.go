package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/Legion112/ember/internal/node"
)

// TestArchs_PrintsEveryArchitectureACardNeeds.
//
// The build matrix reads this, so the list it prints has to be the list in
// internal/node and not a copy. Asserted against node.KnownCaps directly: a
// second source of truth here would be the defect this command exists to
// prevent, implemented by the command itself.
func TestArchs_PrintsEveryArchitectureACardNeeds(t *testing.T) {
	var out bytes.Buffer
	if err := runArchs(nil, &out); err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, l := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		got = append(got, strings.TrimSpace(l))
	}
	if len(got) != len(node.KnownCaps()) {
		t.Fatalf("printed %d architectures, node knows %d: %q", len(got), len(node.KnownCaps()), out.String())
	}
	for i, c := range node.KnownCaps() {
		if got[i] != strconv.Itoa(int(c)) {
			t.Errorf("line %d = %q, want %d", i, got[i], int(c))
		}
	}
}

// TestArchs_JSONIsAMatrixLiteral: what fromJSON() in the workflow consumes.
func TestArchs_JSONIsAMatrixLiteral(t *testing.T) {
	var out bytes.Buffer
	if err := runArchs([]string{"--json"}, &out); err != nil {
		t.Fatal(err)
	}
	var got []int
	if err := json.Unmarshal([]byte(strings.TrimSpace(out.String())), &got); err != nil {
		t.Fatalf("not JSON: %v (%q)", err, out.String())
	}
	if len(got) != len(node.KnownCaps()) {
		t.Errorf("got %v, want %v", got, node.KnownCaps())
	}
	for i := 1; i < len(got); i++ {
		if got[i-1] >= got[i] {
			t.Errorf("not ascending: %v", got)
			break
		}
	}
}

// TestArchs_RefusesAnArchitectureNoCardNeeds.
//
// A typo in a workflow's dispatch box would otherwise buy twenty minutes of
// nvcc and an image nothing will ever run.
func TestArchs_RefusesAnArchitectureNoCardNeeds(t *testing.T) {
	// "" is not in this list: an empty --only means "every one", which is
	// what the workflow passes when its dispatch box is left blank. "," is,
	// because that is a list with nothing in it rather than no list. Whitespace
	// is trimmed to blank for the same reason: a dispatch box can hold a space.
	for _, bad := range []string{"sm_103", "103", "90;100", "ninety", ","} {
		var out bytes.Buffer
		err := runArchs([]string{"--only", bad}, &out)
		if err == nil {
			t.Errorf("--only %q was accepted: %q", bad, out.String())
			continue
		}
		if ExitCode(err) != ExitUsage {
			t.Errorf("--only %q: exit %d, want usage", bad, ExitCode(err))
		}
	}
}

// TestArchs_AnEmptyOnlyIsEveryArchitecture, which is what the workflow passes
// when nobody typed anything in the dispatch box.
func TestArchs_AnEmptyOnlyIsEveryArchitecture(t *testing.T) {
	var out bytes.Buffer
	if err := runArchs([]string{"--json", "--only", ""}, &out); err != nil {
		t.Fatal(err)
	}
	var got []int
	if err := json.Unmarshal([]byte(strings.TrimSpace(out.String())), &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != len(node.KnownCaps()) {
		t.Errorf("--only \"\" printed %v, want every architecture", got)
	}
}

func TestArchs_OnlyTakesBothSpellings(t *testing.T) {
	for _, in := range []string{"90", "sm_90", " sm_90 ", "90,90"} {
		var out bytes.Buffer
		if err := runArchs([]string{"--json", "--only", in}, &out); err != nil {
			t.Errorf("--only %q: %v", in, err)
			continue
		}
		if strings.TrimSpace(out.String()) != "[90]" {
			t.Errorf("--only %q printed %q, want [90]", in, strings.TrimSpace(out.String()))
		}
	}
}

func TestArchs_CardsNamesWhatEachIsFor(t *testing.T) {
	var out bytes.Buffer
	if err := runArchs([]string{"--cards", "--only", "89"}, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "rtx4090") || !strings.Contains(out.String(), "sm_89") {
		t.Errorf("--cards does not name the cards: %q", out.String())
	}
}

func TestArchs_TakesNoArguments(t *testing.T) {
	var out bytes.Buffer
	if err := runArchs([]string{"90"}, &out); err == nil {
		t.Error("a positional argument was accepted")
	}
}

func frag(t *testing.T, dir, key, ref string) {
	t.Helper()
	p := filepath.Join(dir, key+".yaml")
	if err := os.WriteFile(p, []byte("  "+key+": "+ref+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestImages_AssemblesAPasteableBlock(t *testing.T) {
	dir := t.TempDir()
	frag(t, dir, "sm_89", pinnedRef("sd-cuda-sm89"))
	frag(t, dir, "sm_75", pinnedRef("sd-cuda-sm75"))

	var out bytes.Buffer
	if err := runImages([]string{"--from", dir, "--archs", "75,89"}, &out); err != nil {
		t.Fatalf("runImages = %v\n%s", err, out.String())
	}
	// Ascending, whatever order the files were read in: the block is meant to
	// be read as well as pasted.
	want := "images:\n  sm_75: " + pinnedRef("sd-cuda-sm75") + "\n  sm_89: " + pinnedRef("sd-cuda-sm89") + "\n"
	if out.String() != want {
		t.Errorf("block =\n%s\nwant\n%s", out.String(), want)
	}
}

// TestImages_RefusesAnIncompleteSetButStillPrintsIt.
//
// Both halves matter. The digests that were published are worth having, and
// the operator must not mistake the block for a complete one -- so it is
// printed and the command still fails, naming the gap. A map missing a key is
// a card ember refuses to rent.
func TestImages_RefusesAnIncompleteSetButStillPrintsIt(t *testing.T) {
	dir := t.TempDir()
	frag(t, dir, "sm_75", pinnedRef("sd-cuda-sm75"))

	var out bytes.Buffer
	err := runImages([]string{"--from", dir}, &out)
	if err == nil {
		t.Fatal("an incomplete set was reported as complete")
	}
	if !strings.Contains(out.String(), "sm_75: ") {
		t.Errorf("the digests that were published were not printed: %q", out.String())
	}
	for _, want := range []string{"sm_90", "sm_120"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not name %s: %v", want, err)
		}
	}
}

func TestImages_RefusesABadFragment(t *testing.T) {
	for name, line := range map[string]string{
		"a tag naming another architecture": "  sm_90: " + pinnedRef("sd-cuda-sm89"),
		"an unpinned reference":             "  sm_90: ghcr.io/legion112/ember-sd",
		"a latest reference":                "  sm_90: ghcr.io/legion112/ember-sd:latest",
		"a key that is not an architecture": "  sm_9x: " + pinnedRef(""),
	} {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "f.yaml"), []byte(line+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		var out bytes.Buffer
		if err := runImages([]string{"--from", dir, "--archs", "90"}, &out); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// TestImages_RefusesTwoFragmentsThatDisagree, because two jobs claiming one
// architecture with different digests means one of them is not what shipped.
func TestImages_RefusesTwoFragmentsThatDisagree(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.yaml"),
		[]byte("  sm_90: "+pinnedRef("sd-cuda-sm90")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	other := "ghcr.io/legion112/ember-sd:sd-cuda-sm90@sha256:" + strings.Repeat("b", 64)
	if err := os.WriteFile(filepath.Join(dir, "b.yaml"), []byte("  sm_90: "+other+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := runImages([]string{"--from", dir, "--archs", "90"}, &out); err == nil {
		t.Error("two disagreeing fragments were accepted")
	}
}

func TestImages_RefusesAMissingDirectoryAndNoDirectory(t *testing.T) {
	var out bytes.Buffer
	if err := runImages([]string{"--from", filepath.Join(t.TempDir(), "nope")}, &out); err == nil {
		t.Error("a missing fragment directory was accepted")
	}
	out.Reset()
	if err := runImages(nil, &out); err == nil {
		t.Error("no -from was accepted")
	}
	out.Reset()
	if err := runImages([]string{"--from", t.TempDir(), "x"}, &out); err == nil {
		t.Error("a positional argument was accepted")
	}
	out.Reset()
	if err := runImages([]string{"--from", t.TempDir(), "--archs", "sm_103"}, &out); err == nil {
		t.Error("a bad --archs was accepted")
	}
}

// TestImages_IgnoresEverythingThatIsNotAFragmentLine, so a step summary or a
// stray README in the directory does not break a publish.
func TestImages_IgnoresEverythingThatIsNotAFragmentLine(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "sub"), 0o750); err != nil {
		t.Fatal(err)
	}
	body := "### workload image published\n```yaml\n  sm_90: " + pinnedRef("sd-cuda-sm90") + "\n```\n"
	if err := os.WriteFile(filepath.Join(dir, "summary.md"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := runImages([]string{"--from", dir, "--archs", "90"}, &out); err != nil {
		t.Fatalf("runImages = %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "sm_90: ") {
		t.Errorf("the fragment line was not found: %q", out.String())
	}
}

func pinnedRef(tag string) string {
	ref := "ghcr.io/legion112/ember-sd"
	if tag != "" {
		ref += ":" + tag
	}
	return ref + "@sha256:" + strings.Repeat("a", 64)
}
