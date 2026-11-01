package sdserver

import (
	"strings"
	"testing"
)

// TestDescribe_SaysWhichImageThisIs.
//
// Describe is what identifies a job in a line of output and in a dry run, so
// what it includes is a decision rather than a formatting detail.
func TestDescribe(t *testing.T) {
	long := strings.Repeat("x", 100)
	for _, tc := range []struct {
		name string
		job  Job
		want []string
	}{
		{"everything", Job{Prompt: "a cat", Width: 512, Height: 256, Steps: 4, Seed: 9},
			[]string{`"a cat"`, "512x256", "4 steps", "seed 9"}},
		{"a prompt from extra", Job{Extra: map[string]any{"prompt": "from extra"}},
			[]string{`"from extra"`}},
		{"nothing at all", Job{}, []string{`""`}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.job.Describe()
			for _, w := range tc.want {
				if !strings.Contains(got, w) {
					t.Errorf("Describe() = %q, want it to contain %q", got, w)
				}
			}
		})
	}

	// A FLUX.2 prompt is a thousand characters; the first words are what
	// identifies it in a log.
	got := Job{Prompt: long}.Describe()
	if len(got) > 70 {
		t.Errorf("Describe did not truncate a long prompt: %d chars", len(got))
	}
	if !strings.Contains(got, "...") {
		t.Errorf("a truncated prompt must say so: %q", got)
	}
}

// A geometry with only one dimension set is not printed, because the other
// comes from the server's own default and "512x" would be a guess.
func TestDescribe_OmitsAHalfSpecifiedGeometry(t *testing.T) {
	if got := (Job{Prompt: "a", Width: 512}).Describe(); strings.Contains(got, "512") {
		t.Errorf("Describe() = %q, want no geometry when only one side is set", got)
	}
}
