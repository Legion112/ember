package node

import "testing"

// TestSpec_WorkloadNamesWhatThisNodeRuns covers the three answers and, with
// them, the reason this method exists: app.NewPlan refuses a speech batch
// beside another kind and has only a finished Spec to name the other one
// with.
func TestSpec_WorkloadNamesWhatThisNodeRuns(t *testing.T) {
	cases := []struct {
		name string
		spec Spec
		want string
	}{
		{"nothing", Spec{}, ""},
		{"a serve command", Spec{Serve: []string{"sd-server"}}, "sd.serve"},
		{"one command", Spec{Cmds: [][]string{{"true"}}}, "cmd or cmds"},
		{"several commands", Spec{Cmds: [][]string{{"a"}, {"b"}}}, "cmd or cmds"},
		// Serve wins, which is not a precedence anybody relies on: NewSpec
		// refuses the combination, so this arm only decides which word an
		// unreachable state would use. Asserted so that the ordering is a
		// decision rather than whichever case was written first.
		{"both", Spec{Serve: []string{"sd-server"}, Cmds: [][]string{{"true"}}}, "sd.serve"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.spec.Workload(); got != c.want {
				t.Errorf("Workload() = %q, want %q", got, c.want)
			}
		})
	}
}

// TestSpec_WorkloadSaysCmdOrCmdsBecauseASpecCannotTellThemApart is the one
// thing about this method a reader would call a bug: NewSpec folds `cmd` into
// Cmds as its first entry, so naming one of the two would name the wrong key
// for half of all configs -- and an operator looking for a key that is not in
// their file concludes the message is about somebody else's.
func TestSpec_WorkloadSaysCmdOrCmdsBecauseASpecCannotTellThemApart(t *testing.T) {
	a := validSpecOptions()
	a.Cmd = []string{"true"}
	fromCmd, err := NewSpec(a)
	if err != nil {
		t.Fatal(err)
	}
	b := validSpecOptions()
	b.Cmds = [][]string{{"true"}}
	fromCmds, err := NewSpec(b)
	if err != nil {
		t.Fatal(err)
	}
	if fromCmd.Workload() != fromCmds.Workload() {
		t.Errorf("cmd and cmds produced different answers: %q and %q",
			fromCmd.Workload(), fromCmds.Workload())
	}
}
