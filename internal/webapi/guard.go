package webapi

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// THESE TWO CHECKS ARE NOT AUTHENTICATION, AND NOW THEY DO NOT HAVE TO BE.
//
// They used to be the whole control, and this comment used to open by saying
// so. auth.go is the credential that runs after them, for the threat these
// two cannot see at all: a client that is not a browser, where no Origin is
// sent and none is required. What follows is still the reason these exist and
// still the reason they are not enough on their own.
//
// WHAT THE THREAT ACTUALLY IS. Nothing is listening off this machine, so it is
// not an attacker on the network. It is any web page the operator has open:
// evil.example can `fetch('http://127.0.0.1:7777/api/requests', {method:
// 'POST', ...})` and queue a generation. The browser hides the *response*
// cross-origin -- which is all CORS does -- but the side effect has already
// happened. So CORS is not a control, and a server that relied on it would be
// relying on a thing that does not work.
//
// WHAT BOUNDS THE DAMAGE is --allow-paid, which is off by default: a
// cross-site POST to a default `ember serve` can start a free local container
// and spend nothing. That is the reason the flag exists and the reason it
// defaults off.
//
// The two checks below are not authentication and cost nothing, so they are
// here anyway:
//
//   - Origin must be this server's own. An attacker's page cannot forge it;
//     the browser sets it.
//   - Host must be a loopback address and this port. This is the DNS
//     rebinding case: an attacker's domain resolving to 127.0.0.1 makes their
//     page same-origin with *their own* name, which passes the Origin check
//     and fails this one.
//
// A REQUEST WITH NO Origin IS ALLOWED, deliberately. curl sends none, and so
// does every script an operator writes against their own machine; refusing
// would break the command line without stopping a browser, which always sends
// one on a cross-origin fetch. The Host check applies regardless, so a
// rebinding attack does not get in through that door.
//
// THAT ALLOWANCE IS EXACTLY THE HOLE auth.go CLOSES. A second uid on this
// machine sends no Origin either, and is indistinguishable here from the
// operator's own curl. The credential is what tells them apart; these two
// checks are kept because they are free and because a browser is still a
// different threat from a shell.

// guard refuses a request that a page on another site could have made.
//
// port is this server's own, so a Host naming a different one is not this
// server being addressed -- it is a proxy, or a rebinding attempt.
func guard(r *http.Request, port string) error {
	if err := hostOK(r, port); err != nil {
		return err
	}
	return checkOrigin(r.Header.Get("Origin"), port)
}

// hostOK is checkHost unless the request arrived on the unix socket.
//
// WHAT THE TWO CHECKS DO ON THE SOCKET IS A DECISION AND NOT A CONSEQUENCE,
// so it is written here rather than left to fall out of the code. Both were
// written against a browser, and a browser cannot open a unix socket -- there
// is no fetch, no XHR and no <img src> that names one. What can still put a
// browser's request on it is something the operator built in front of it, a
// proxy with `proxy_pass http://unix:...`, and that is what decides the split:
//
//   - checkOrigin STILL RUNS, on the socket as on the port. It costs nothing,
//     it is the half that is about who COMPOSED the request rather than where
//     it arrived, and through such a proxy it is the only thing left that
//     refuses a page on another site. A request with no Origin is allowed
//     here exactly as it is on TCP, because that is what a shell sends.
//   - checkHost CANNOT RUN, because it is a statement about a TCP port.
//     It compares the Host header against the port this server listens on,
//     and a request that arrived on a socket did not come through any port:
//     `curl --unix-socket` sends `Host: localhost`, which has no port at all.
//     Requiring the TCP port in it would be asserting a fact that is not true
//     of the channel, and every socket request would be refused -- which is
//     not a stricter check, it is a channel that does not work.
//
// ONE FUNCTION FOR BOTH CALL SITES -- guard above and serveWS, which checks
// the Host and lets coder/websocket answer the Origin -- because two copies of
// "unless it came in on the socket" is two places for one of them to be
// forgotten, which is this repository's journal-subdirectory rule.
func hostOK(r *http.Request, port string) error {
	if arrivedOnSocket(r.Context()) {
		return nil
	}
	return checkHost(r.Host, port)
}

// checkHost refuses anything but loopback at this port.
func checkHost(host, port string) error {
	if host == "" {
		// HTTP/1.1 requires it and Go's server rejects a request without
		// one, so this is unreachable from the network. Refused rather than
		// waved through, because "unreachable" is a claim about today's
		// server and the cost of being wrong is the rebinding attack.
		return errors.New("the request carries no Host header")
	}
	h, p, ok := splitHostPort(host)
	if !ok {
		return fmt.Errorf("the Host header %q is not host:port", host)
	}
	if p != port {
		return fmt.Errorf("the Host header %q names port %s and this server is on %s; "+
			"ember serve answers only for its own address", host, p, port)
	}
	if !isLoopbackName(h) {
		return fmt.Errorf("the Host header %q is not a loopback address. ember serve is reachable only from "+
			"this machine, so a request addressed to anything else reached it through a proxy or "+
			"through a name that resolves here -- which is how a page on another site talks to a "+
			"local server", host)
	}
	return nil
}

// checkOrigin refuses an Origin that is not this server.
func checkOrigin(origin, port string) error {
	if origin == "" {
		// See the header comment: curl sends none, a browser always does on a
		// cross-origin fetch.
		return nil
	}
	u, err := url.Parse(origin)
	if err != nil {
		return fmt.Errorf("the Origin header %q is not a URL", origin)
	}
	if u.Scheme != "http" {
		return fmt.Errorf("the Origin header %q is not http; ember serve is plain http on loopback", origin)
	}
	h, p, ok := splitHostPort(u.Host)
	if !ok || p != port || !isLoopbackName(h) {
		return fmt.Errorf("the Origin header %q is not this server. A page served from somewhere else is "+
			"asking ember to rent hardware on your account; it is refused whatever it claims to be",
			origin)
	}
	return nil
}

// isLoopbackName is the set of hosts this server answers for.
//
// "localhost" IS accepted here, and that is not the same decision as
// loopback.ParseBind's refusal of it. There, a name was going to be handed to
// net.Listen and resolved at listen time -- so a hosts file mapping it to a
// LAN address would publish the port, and nothing in the config would look
// wrong. Here the name has already been resolved, by the browser, to an
// address that reached a socket bound to loopback and nothing else. What is
// being checked is which name the operator typed, not where a packet will go.
//
// It is still a closed set rather than a suffix match: "localhost.evil.test"
// must not pass, and `strings.HasSuffix(h, "localhost")` would let it.
func isLoopbackName(h string) bool {
	switch strings.ToLower(strings.Trim(h, "[]")) {
	case "localhost", "127.0.0.1", "::1":
		return true
	}
	return false
}

// splitHostPort is net.SplitHostPort without the error type, and tolerant of
// the bracketed IPv6 form a browser sends.
func splitHostPort(hp string) (host, port string, ok bool) {
	if hp == "" {
		return "", "", false
	}
	if strings.HasPrefix(hp, "[") {
		end := strings.Index(hp, "]")
		if end < 0 {
			return "", "", false
		}
		host = hp[1:end]
		rest := hp[end+1:]
		if !strings.HasPrefix(rest, ":") {
			return "", "", false
		}
		return host, rest[1:], true
	}
	i := strings.LastIndex(hp, ":")
	if i < 0 {
		return "", "", false
	}
	return hp[:i], hp[i+1:], true
}
