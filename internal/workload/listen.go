package workload

import (
	"net/netip"
	"strings"
)

// NonLoopbackBind is one place in a workload's argv that asks a server to
// listen somewhere other than the node's own loopback interface.
type NonLoopbackBind struct {
	// Index is where in the argv it was found, so the operator can go
	// straight to the line rather than re-reading a forty-word command.
	Index int

	// Flag is the spelling that was used, without its dashes: "host".
	Flag string

	// Addr is the address as it was written: "0.0.0.0", "::", "10.0.0.4".
	Addr string
}

// listenFlags are the argv spellings that name a server's listen address.
//
// An allow-list, and a short one, because every entry is a claim about what
// some program's flag means. These are the spellings the servers an operator
// would point a coding agent at actually use -- llama.cpp and vLLM and
// SGLang spell it `--host`, TGI `--hostname`, sd-server `--listen-ip` -- plus
// the handful of synonyms that cost nothing to recognise. Dashes are trimmed
// before the lookup, so `-host` and `--host` are the same key.
//
// Deliberately NOT here: single-letter flags. `-b` is a bind address to one
// program and a batch size to llama.cpp, and a finding about a batch size is
// the kind of false positive that teaches an operator to stop reading these.
var listenFlags = map[string]bool{
	"host": true, "hostname": true, "http-host": true,
	"listen": true, "listen-ip": true, "listen-address": true, "listen-host": true,
	"bind": true, "bind-address": true, "bind-addr": true,
	"address": true, "addr": true,
}

// NonLoopbackBinds reports the places in cmd that ask for a non-loopback bind.
//
// WHY THIS IS A REPORT AND NOT A REFUSAL. `cmd` is opaque argv on purpose:
// ember does not know llama-server's flags and must not learn them, which is
// the same position gpu_probe takes. sdserver.ServeCommand can refuse a
// listen flag outright because it knows it is building sd-server's argv and
// appends the address itself; this function is reading somebody else's
// program, so a refusal would be ember claiming to know what `--host` means
// to a binary it has never heard of -- and the first config whose `--host`
// names the server to *talk to* would be a config ember will not run.
//
// WHAT IT IS FOR, which is not a security control. AGENTS.md is explicit that
// a warning is not one, and the control here is structural and already in
// place: exposure.PortRequest's `public` field is unexported, only SSHPort()
// sets it, and a workload port goes through exposure.Internal -- so on a
// rented pod a process bound to 0.0.0.0 has no published mapping, and the
// post-create exposure check destroys the node if the provider published
// anything else. What a `--host 0.0.0.0` reveals is therefore not a hole but
// a *belief*: somebody concluded that the bind address is what makes their
// endpoint reachable, and on the local provider that belief is partly true --
// the container sits on a Docker bridge, so a 0.0.0.0 bind is reachable from
// this machine at the container's address. Saying so before anything is
// rented costs nothing and is the only moment it is useful.
//
// WHY netip DECIDES. A value is reported only when it parses as an IP address
// that is not loopback; the string is never pattern-matched. That is
// ParseBind's rule applied one layer out, and it is what keeps this from
// repeating rejectSecrets' mistake in reverse -- that function deliberately
// does not scan `cmd`, because a prompt is arbitrary English and "a token of
// affection" is a legitimate prompt. A prompt is one argv element, so it can
// never be mistaken for a flag name, and a flag's value is only ever looked
// at after its name matched the list above.
//
// THE LIMIT, written down rather than hidden: a value that is not an IP is
// not reported, so `--host localhost` passes. A name is never proof of
// loopback -- ParseBind refuses "localhost" for exactly that reason, since
// /etc/hosts and NSS decide what it means -- but it is the node's resolver
// that would answer, not this machine's, and warning about every `--host`
// whose value is not an IP would fire on every program whose `--host` names
// something else entirely.
func NonLoopbackBinds(cmd []string) []NonLoopbackBind {
	var out []NonLoopbackBind
	for i, arg := range cmd {
		if !strings.HasPrefix(arg, "-") {
			continue
		}
		name, value, joined := strings.Cut(strings.TrimLeft(arg, "-"), "=")
		if !listenFlags[strings.ToLower(name)] {
			continue
		}
		at := i
		if !joined {
			// The value is the next word, and a flag at the end of the argv
			// has none.
			if i+1 >= len(cmd) {
				continue
			}
			value, at = cmd[i+1], i+1
		}
		addr, ok := parseBindAddr(value)
		if !ok || addr.IsLoopback() {
			continue
		}
		out = append(out, NonLoopbackBind{Index: at, Flag: strings.ToLower(name), Addr: value})
	}
	return out
}

// parseBindAddr reads a bind value as an address, with or without a port.
//
// Both forms, because both are written: `--host 0.0.0.0` and
// `--listen 0.0.0.0:8080`. Unmapped first, so that ::ffff:127.0.0.1 answers
// the loopback question the way a kernel would rather than by its family.
func parseBindAddr(s string) (netip.Addr, bool) {
	if a, err := netip.ParseAddr(s); err == nil {
		return a.Unmap(), true
	}
	if ap, err := netip.ParseAddrPort(s); err == nil {
		return ap.Addr().Unmap(), true
	}
	return netip.Addr{}, false
}
