package node

import "testing"

// TestSpec_WorkloadNamesWhatThisNodeRuns covers both answers and, with them,
// the reason this method exists: app.NewPlan refuses a speech batch beside
// another kind and has only a finished Spec to name the other one with.
//
// THE EMPTY ANSWER IS THE HALF THAT MATTERS, because it is what a speech run
// and a run with no batch at all both give -- so a method that answered
// something for every spec would make the refusal one layer up fire on a
// configuration naming one kind.
func TestSpec_WorkloadNamesWhatThisNodeRuns(t *testing.T) {
	cases := []struct {
		name string
		spec Spec
		want string
	}{
		{"nothing", Spec{}, ""},
		{"a serve command", Spec{Serve: []string{"sd-server"}}, "sd.serve"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.spec.Workload(); got != c.want {
				t.Errorf("Workload() = %q, want %q", got, c.want)
			}
		})
	}
}

// TestSpec_WorkloadReadsAFinishedSpecAndNotAConfig: the answer has to survive
// NewSpec, because the caller that needs it -- app.NewPlan, finding out a
// speech batch was configured beside a generation server -- holds a Spec and
// not the options it was built from.
func TestSpec_WorkloadReadsAFinishedSpecAndNotAConfig(t *testing.T) {
	o := validSpecOptions()
	o.Serve = []string{"sd-server", "--model", "/models/x.gguf"}
	s, err := NewSpec(o)
	if err != nil {
		t.Fatal(err)
	}
	if got := s.Workload(); got != "sd.serve" {
		t.Errorf("Workload() = %q, want sd.serve", got)
	}

	bare := validSpecOptions()
	bare.Serve = nil
	b, err := NewSpec(bare)
	if err != nil {
		t.Fatal(err)
	}
	if got := b.Workload(); got != "" {
		t.Errorf("Workload() = %q for a spec naming no workload, want empty", got)
	}
}
