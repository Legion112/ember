package workload

import "testing"

// TestNonLoopbackBinds reads an argv the way an operator wrote it.
//
// The cases that matter are the negative ones. A finding here is advice, and
// advice that is wrong once is advice nobody reads twice -- which is why
// rejectSecrets does not scan `cmd` at all: "a token of affection" is a
// legitimate prompt. So every form a prompt or an unrelated flag can take is
// asserted to produce nothing.
func TestNonLoopbackBinds(t *testing.T) {
	for _, c := range []struct {
		name string
		cmd  []string
		want []NonLoopbackBind
	}{{
		name: "the loopback bind this repository's recipes write",
		cmd:  []string{"llama-server", "--host", "127.0.0.1", "--port", "8080"},
	}, {
		name: "the one an operator writes when they believe the bind is what exposes them",
		cmd:  []string{"llama-server", "--host", "0.0.0.0", "--port", "8080"},
		want: []NonLoopbackBind{{Index: 2, Flag: "host", Addr: "0.0.0.0"}},
	}, {
		name: "joined with an equals sign, which argparse accepts",
		cmd:  []string{"vllm", "serve", "--host=::"},
		want: []NonLoopbackBind{{Index: 2, Flag: "host", Addr: "::"}},
	}, {
		name: "one dash, which Go's own flag package accepts",
		cmd:  []string{"server", "-listen", "10.0.0.4:8080"},
		want: []NonLoopbackBind{{Index: 2, Flag: "listen", Addr: "10.0.0.4:8080"}},
	}, {
		name: "every spelling in one argv, because a config can set more than one",
		cmd:  []string{"s", "--hostname", "0.0.0.0", "--bind-address", "192.168.1.9"},
		want: []NonLoopbackBind{
			{Index: 2, Flag: "hostname", Addr: "0.0.0.0"},
			{Index: 4, Flag: "bind-address", Addr: "192.168.1.9"},
		},
	}, {
		// The whole reason this is netip's decision and not a pattern match.
		name: "a prompt that contains the words",
		cmd:  []string{"sd-cli", "--prompt", "a server with --host 0.0.0.0 painted on it"},
	}, {
		name: "a flag value that is not an address at all",
		cmd:  []string{"llama-server", "--host", "localhost"},
	}, {
		name: "a 4-in-6 loopback, which is loopback to a kernel",
		cmd:  []string{"llama-server", "--host", "::ffff:127.0.0.1"},
	}, {
		name: "another loopback in 127/8, which is all loopback",
		cmd:  []string{"llama-server", "--host", "127.0.0.53"},
	}, {
		// -b is a batch size to llama.cpp. A finding about a batch size is
		// the false positive that would get these ignored.
		name: "a single-letter flag is never a bind",
		cmd:  []string{"llama-server", "-b", "2048"},
	}, {
		name: "a listen flag with nothing after it",
		cmd:  []string{"llama-server", "--host"},
	}, {
		name: "a bare word is not a flag",
		cmd:  []string{"host", "0.0.0.0"},
	}, {
		name: "no command at all",
	}} {
		t.Run(c.name, func(t *testing.T) {
			got := NonLoopbackBinds(c.cmd)
			if len(got) != len(c.want) {
				t.Fatalf("got %+v, want %+v", got, c.want)
			}
			for i := range got {
				if got[i] != c.want[i] {
					t.Errorf("[%d] = %+v, want %+v", i, got[i], c.want[i])
				}
			}
		})
	}
}
