package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/Legion112/ember/internal/node"
)

// TestNoWorkflowPublishesAMovingLatestTag keeps a `:latest` tag out of the
// publishing workflows.
//
// node-image.yml pushed `${{ env.IMAGE }}:latest` for the life of the node
// image, six lines beneath a comment arguing against doing exactly that. The
// argument was right and understated: ember runs one container that is both
// the node and the workload, so the node image is the `image:` key and goes
// through node.NewSpec like every other reference -- and validateImage
// refuses `:latest` by name. So the tag was never reachable from ember at
// all. It was a published reference whose only possible use was to be
// refused, which is why nothing in the repository named it and every example
// config names a digest.
//
// Deleting the tag is the fix for the tag. This is the fix for the *class*,
// which is the distinction TestEveryBuildTagReachesTheGates is written about
// one axis over: a workflow edit leaves the next paste a silent gap in the
// same place. And the paste is the likely failure here rather than an
// argument -- `:latest` is the tag every registry example uses, so it comes
// back by habit, and it comes back in a file nobody runs locally.
//
// The prohibition is on the literal tag `latest` and not on moving tags in
// general, because ember's refusal is on the *name* and not on the behaviour:
// `:main` moves exactly as much and validateImage accepts it. A guard
// claiming "no workflow publishes a moving tag" would also be false --
// workload-image.yml publishes `<tag>-sm<arch>` on purpose, beside a pinned
// sha tag, because the reference it hands an operator carries the digest as
// well and the digest is what decides. What is actually true, and what this
// asserts, is that nothing this repository publishes is tagged with the one
// name ember will not provision from.
func TestNoWorkflowPublishesAMovingLatestTag(t *testing.T) {
	// The premise, asked of the production rule rather than assumed. This
	// guard's whole justification is that ember refuses the tag; if that
	// stopped being true the message below would be wrong, and a guard that
	// cannot find its source of truth must not report that the truth agrees
	// with it -- the same reason makeVetTags fails hard on a renamed
	// variable.
	if err := node.ValidateImage("ghcr.io/x/ember-node:" + movingTag); err == nil {
		t.Fatalf("node.ValidateImage accepts :%s; this guard exists because it refuses it", movingTag)
	}

	dir := filepath.Join("..", "..", ".github", "workflows")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read the workflow directory: %v", err)
	}

	checked, strs := 0, 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || (!strings.HasSuffix(name, ".yml") && !strings.HasSuffix(name, ".yaml")) {
			continue
		}
		checked++
		raw, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		var doc any
		if err := yaml.Unmarshal(raw, &doc); err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, s := range yamlStrings(doc) {
			strs++
			for _, line := range strings.Split(s, "\n") {
				// A `#` comment inside a run: script is part of the string
				// value, and the comment this test was written about says
				// `:latest` three times while arguing against it. Cut at the
				// first `#`, as TestNoWorkflowSpellsItsOwnVetTags does.
				// Comments outside a run: block are dropped by the decoder
				// and never reach here at all.
				code, _, _ := strings.Cut(line, "#")
				if hasLatestTag(code) {
					t.Errorf("%s tags an image :%s (%q); ember refuses such a reference (node.ValidateImage), so "+
						"it could only ever be pulled by hand -- push the commit sha and let the digest be what a "+
						"config names",
						name, movingTag, strings.TrimSpace(line))
				}
			}
		}
	}
	if checked == 0 {
		t.Fatal("read no workflow files; the walk is broken, not the workflows")
	}
	// A walk that inspected nothing passes for the wrong reason, which is the
	// failure mode every source-walking test has. Both map shapes are handled
	// below and this is the assertion that says so: yaml.v3 hands back
	// map[any]any for a mapping with one non-string key, and `on:` in a
	// GitHub workflow is exactly such a key.
	if strs == 0 {
		t.Fatalf("parsed %d workflow files and found no strings; the walk is broken, not the workflows", checked)
	}
}

// movingTag is the one tag name ember has a rule about.
const movingTag = "latest"

// TestHasLatestTag checks the matcher in both directions.
//
// A guard whose match is too lenient fails open, which is worse than no
// guard: it reports that the workflows agree with a rule it is not actually
// applying. `runs-on: ubuntu-latest` appears in all three workflow files, so
// the false positive is not hypothetical -- the first version of the walk
// would have failed on every one of them.
func TestHasLatestTag(t *testing.T) {
	for _, tc := range []struct {
		line string
		want bool
	}{
		{"${{ env.IMAGE }}:latest", true},
		{"            ${{ env.IMAGE }}:latest", true},
		{`docker push "$IMAGE:latest"`, true},
		{"tags: ghcr.io/x/ember-node:latest", true},
		{"runs-on: ubuntu-latest", false},
		{"      - uses: actions/checkout@v5", false},
		{"ghcr.io/x/ember-sd:latest-sm89", false},
		{"image: ghcr.io/x/ember-node:${{ github.sha }}", false},
		{"echo use :latest at your peril", false},
		{"", false},
		{":latest", false},
	} {
		if got := hasLatestTag(tc.line); got != tc.want {
			t.Errorf("hasLatestTag(%q) = %v, want %v", tc.line, got, tc.want)
		}
	}
}

// hasLatestTag reports whether a line of workflow text tags an image :latest.
//
// Matched as a tag rather than as a substring, in both directions. `runs-on:
// ubuntu-latest` must not fire, so the word has to follow a colon that itself
// follows a reference rather than whitespace; `:latest-sm89` must not fire
// either, so the word has to end the reference. Substring matching with
// neither boundary is how NormalizeGPU once turned "A100 SXM4 80GB" into
// `a1004-80g` -- a model name no provider had -- which is the entry in
// AGENTS.md this function is written against.
//
// Over-matching is the safe direction and a one-line fix: the failure guarded
// against is a paste, not an evasion, which is the argument
// TestNoWorkflowSpellsItsOwnVetTags makes for the same shape of match.
func hasLatestTag(line string) bool {
	const want = ":" + movingTag
	for i := 0; i < len(line); {
		at := strings.Index(line[i:], want)
		if at < 0 {
			return false
		}
		colon := i + at
		end := colon + len(want)
		i = end

		// Something has to be on the repository side of the colon, or this is
		// the word with punctuation in front of it rather than a reference.
		if colon == 0 || isBreak(line[colon-1]) {
			continue
		}
		// And the tag has to end here. A longer tag that merely starts with
		// the word is a different tag.
		if end < len(line) && !isBreak(line[end]) {
			continue
		}
		return true
	}
	return false
}

// isBreak reports whether c ends an image reference in workflow text.
func isBreak(c byte) bool {
	switch c {
	case ' ', '\t', '"', '\'', '(', ')', ',', ';':
		return true
	}
	return false
}

// yamlStrings collects every string value in a parsed workflow document.
//
// Walked generically rather than against a typed workflow schema, for
// runScripts' reason: the shape of a GitHub workflow is somebody else's and
// changes without asking. A tag can be written under `tags:` on a
// build-push-action, in a `docker push` line of a run: script, or in an env
// block three steps earlier, and all three are strings in this document.
func yamlStrings(n any) []string {
	var out []string
	switch v := n.(type) {
	case string:
		return []string{v}
	case map[string]any:
		for _, val := range v {
			out = append(out, yamlStrings(val)...)
		}
	case map[any]any:
		// yaml.v3 decodes a mapping into map[string]any only when every key
		// is a string, and `on:` is the key that can come back as a bool --
		// one such key makes the whole file this shape, so a walk that knew
		// only the first would skip every string in it and report success.
		for _, val := range v {
			out = append(out, yamlStrings(val)...)
		}
	case []any:
		for _, item := range v {
			out = append(out, yamlStrings(item)...)
		}
	}
	return out
}
