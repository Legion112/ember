package webapi

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"net/http"
	"net/url"
	"strings"

	"github.com/Legion112/ember/internal/node"
)

// WHICH THREAT THIS IS FOR, AND WHICH IT IS NOT.
//
// guard.go already answers the browser: an Origin that is not this server's
// own is refused, and a Host that is not loopback at this port is refused,
// which is the DNS rebinding case. Those two are not authentication and this
// file does not replace them -- it runs after both, and all three or nothing.
//
// WHAT THEY CANNOT SEE is a client that is not a browser. A loopback port is
// reachable by every uid on the machine, and a request with no Origin at all
// is deliberately allowed -- curl sends none -- so `curl -XPOST
// http://127.0.0.1:7777/api/requests` from a second account on the same box
// passes every check in guard.go and queues a generation. On `ember serve
// --allow-paid` that is somebody else's shell renting an H100 on the
// operator's card. The same shape one hop further out is the deployment ember
// is otherwise exactly the tool for: `ssh -L 7777:localhost:7777 homebox`
// puts the UI on the laptop's loopback, where every uid on the laptop can
// reach it too.
//
// So the credential is for A CLIENT THAT IS NOT THE OPERATOR'S OWN PAGE,
// reaching a loopback socket directly or through a forward.
//
// WHAT IT DOES NOT DEFEND AGAINST, said plainly because a control whose
// limits are not written down gets believed past them:
//
//   - An attacker on the network. Nothing listens off this machine;
//     internal/loopback is what keeps that true and this file does not make a
//     public listener any more acceptable than it was. That seal was NOT
//     widened for this: a unix socket at 0600 would answer the second-uid
//     case with no secret at all, and it is the one answer that requires
//     teaching loopback.Bind a second kind of address -- and it answers
//     nothing about the forward, because the client end of `ssh -L` is a TCP
//     port on loopback again.
//   - The operator's own uid, or root. Either can read EMBER_SERVE_TOKEN out
//     of /proc/<pid>/environ, read credentials.yaml, ptrace this process or
//     read the browser profile holding the cookie. A secret held by a uid
//     cannot authenticate against that uid. What it does do is hold against a
//     DIFFERENT uid, which is the whole point: 0600 on the credentials file
//     and 0400-by-owner on /proc/<pid>/environ are what make the token
//     unreadable from the next account over -- and are exactly why AGENTS.md
//     bans a secret in argv, where /proc/<pid>/cmdline is world-readable.
//   - Rotation and expiry. This is a bearer token with neither. Anyone who
//     learns it holds it until the process restarts.
//
// WHY A TOKEN AND NOT THE OS. SO_PEERCRED is a unix-socket mechanism, and the
// TCP equivalent is matching a source port against /proc/net/tcp -- racy,
// Linux-only, and wrong through a forward, where the connection is made by
// sshd as the logged-in user and would therefore pass. The token is one
// mechanism that answers both the second uid and the forward, and it is the
// seam AGENTS.md already named.

// cookieName is where a browser keeps the token.
//
// A COOKIE AND NOT sessionStorage WITH A HEADER, and the reason is the three
// places a page cannot set a header. coder/websocket cannot set request
// headers from a browser at all, so a header scheme has to put the token in
// the socket's query string; <img src> in the gallery cannot carry one
// either, so the image route needs it in a URL as well; and sessionStorage is
// per-tab, so a second tab is unauthenticated. A cookie is carried on fetch,
// on the websocket handshake and on <img src> alike, with no plumbing in the
// page -- which is why web/ needed no change for this.
//
// HttpOnly, so a wasm panic or a stray console cannot read it back out.
// SameSite=Strict, which is the part that is not merely convenience: a
// cross-site request does not carry the cookie at all, so the one browser
// hole guard.go leaves open -- a no-Origin GET, which is what <img src> and a
// top-level navigation are -- stops reaching the operator's data. Path=/
// because the UI, the API and the socket are all under it.
//
// The cookie is host-scoped and not port-scoped, which is a real limit worth
// knowing: another server on localhost could overwrite ember's cookie for the
// name "localhost". That can only break the UI for the operator, never
// authenticate anything, because the attacker would have to know the token to
// set a working one. The printed URL names 127.0.0.1 for that reason.
const cookieName = "ember_serve"

// tokenParam is the ONE place a token may appear in a URL.
//
// Honoured on the app's own route only -- see exchange -- never on an API
// route and never on /ws, so there is no path on which an operator's token
// lands in a query string that something might log. The app route's answer to
// it is a Set-Cookie and a redirect to the same path without it, so it leaves
// the address bar and the history entry immediately.
//
// A FRAGMENT WOULD NOT REACH THE SERVER AT ALL, which is the candidate the
// issue described, and it is better on exactly that point. It is worse on
// every other: the page has to be served unauthenticated for its script to
// read the fragment, so the one route a browser navigates to would be the one
// route with no credential on it. A query parameter the server consumes keeps
// the whole surface gated, with one request carrying the token and nothing
// logging it -- the refusal log below records the path and never the query.
const tokenParam = "t"

// NewToken mints the credential for one process.
//
// crypto/rand.Text is 128 bits in base32, which is URL-safe and
// cookie-safe without encoding, and it cannot fail -- so there is no error
// return and no unreachable branch standing in for one.
func NewToken() node.Secret { return node.NewSecret([]byte(rand.Text())) }

// errNoCredential is the only answer a failed check gives, whatever failed.
//
// ONE MESSAGE FOR NO TOKEN, A WRONG TOKEN AND A MALFORMED ONE. A caller
// learning which of the three it was learns something about the secret, and
// the operator does not need to be told -- the remediation is the same line
// either way. It names where the token comes from because an error that
// refuses without saying how to proceed is how an operator concludes the
// server is broken.
var errNoCredential = errors.New("this ember serve is authenticated and the request carried no " +
	"credential it accepts. Open the URL ember serve printed -- it ends in ?" + tokenParam +
	"=<token>, which this server exchanges for a cookie -- or send the token as " +
	"`Authorization: Bearer <token>`. The token is EMBER_SERVE_TOKEN in the environment, " +
	"serve_token in credentials.yaml, or the one this process minted and printed at startup")

// authenticate is the third check, after Host and after Origin.
//
// Both carriers are tried and either is enough: there is one secret, and a
// precedence rule between two spellings of it would be a rule that can be
// wrong. A browser sends the cookie; curl and the labs send the header.
func (s *Server) authenticate(r *http.Request) error {
	// THE OPERATING SYSTEM ALREADY ANSWERED, AND IT ANSWERED BETTER.
	//
	// A request that arrived on the unix socket got there through connect(2)
	// on a node at 0600 in a directory at 0700, so the kernel refused every
	// account but the operator's own before ember saw a byte -- which is the
	// question this whole file exists to ask, settled by a mechanism that has
	// no secret to leak, no rotation to lack and nothing to put in a URL.
	//
	// THIS IS THE ONE LINE THAT COULD WIDEN THE PORT, so what reaches it
	// matters more than what it does: arrivedOnSocket reads a value that only
	// socket.go's ConnContext puts on a context, on the one http.Server that
	// serves the one listener type loopback.ListenSocket can produce. There
	// is no header, no query parameter and no proxy header that spells it, so
	// a request on the loopback port cannot reach this return however it is
	// composed.
	if arrivedOnSocket(r.Context()) {
		return nil
	}
	if tok, ok := bearer(r.Header.Get("Authorization")); ok && s.tokenMatches(tok) {
		return nil
	}
	if c, err := r.Cookie(cookieName); err == nil && s.tokenMatches([]byte(c.Value)) {
		return nil
	}
	return errNoCredential
}

// tokenMatches compares what arrived against what this server holds.
//
// THE DIGESTS ARE COMPARED AND NOT THE TOKENS. subtle.ConstantTimeCompare is
// constant time in the length of its arguments and returns 0 immediately when
// they differ, so comparing the raw values leaks the token's length to a
// caller who can time the answer -- and `==` leaks the matching prefix one
// byte at a time, which is the whole reason this repository does not use it on
// a secret. Hashing both sides first makes every comparison the same
// thirty-two bytes, so neither the length nor a prefix is observable.
func (s *Server) tokenMatches(presented []byte) bool {
	got := sha256.Sum256(presented)
	return subtle.ConstantTimeCompare(got[:], s.tokenDigest[:]) == 1
}

// bearer pulls the token out of an Authorization header.
//
// The scheme is matched case-insensitively because RFC 7235 says it is, and a
// client spelling it "bearer" is not a client to refuse over a capital letter.
func bearer(h string) ([]byte, bool) {
	const scheme = "bearer "
	if len(h) <= len(scheme) || !strings.EqualFold(h[:len(scheme)], scheme) {
		return nil, false
	}
	return []byte(strings.TrimSpace(h[len(scheme):])), true
}

// exchange turns a one-time URL into a cookie, and reports whether it
// answered the request.
//
// It runs AFTER guard and BEFORE authenticate, on the app's own route only.
// A token that does not match is not an error here: nothing is set, nothing is
// redirected, and the credential check below refuses it with the one message
// every other failure gets. That makes this an online guessing oracle in
// principle, which is why the minted token is 128 bits of crypto/rand rather
// than something an operator chose.
func (s *Server) exchange(w http.ResponseWriter, r *http.Request) bool {
	q := r.URL.Query()
	v := q.Get(tokenParam)
	if v == "" || !s.tokenMatches([]byte(v)) {
		return false
	}

	// NO Secure ATTRIBUTE, DELIBERATELY, and gosec's G124 is asking for one.
	//
	// `ember serve` is plain http on loopback -- there is no TLS to require
	// and no network path to eavesdrop on, which is the property the whole of
	// internal/loopback exists to guarantee. Whether a browser sends a Secure
	// cookie to an http origin it considers trustworthy is a per-browser
	// decision, and a browser that does not would never authenticate at all:
	// the symptom is a UI refused with the same message a wrong token gives,
	// which is the one failure mode here that cannot be told from a working
	// control. HttpOnly and SameSite are both set, and they are the two
	// attributes that do something on this origin.
	http.SetCookie(w, &http.Cookie{ //nolint:gosec // G124: see above -- no TLS on loopback, HttpOnly and SameSite are set
		Name:     cookieName,
		Value:    v,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
	})

	// no-referrer on the one response whose own URL holds the token. The
	// redirect target does not, so nothing after this point can carry it in a
	// Referer -- but this is the request that could, and the header costs a
	// line.
	w.Header().Set("Referrer-Policy", "no-referrer")

	q.Del(tokenParam)

	// THE TARGET IS BUILT, NOT COPIED FROM r.URL, AND THEN CHECKED.
	//
	// First the building. A *url.URL from a server request can carry a Scheme
	// and a Host, because an absolute-form request-target is legal, and it
	// can carry an Opaque part -- and `RequestURI()` returns Opaque verbatim
	// when it is set. Rebuilding from the path and the remaining query drops
	// all three, so nothing a client sent in those fields can become part of
	// a Location header.
	//
	// Then the check, which is NOT belt and braces: a path of
	// "//evil.example" survives `(&url.URL{Path: ...}).String()` unchanged --
	// measured, not assumed, because the "./" prefixing String() does for
	// host-less paths turns out not to apply here -- and a Location of
	// "//evil.example" is protocol-relative, so a browser follows it to
	// somebody else's site. That is a real open redirect out of the one route
	// that handles a credential, which is the worst place in this program to
	// have one. http.ServeMux cleans a double slash and 301s before this
	// handler sees it, so there is no path through Handler() that reaches the
	// refusal below -- but "the mux cleans it" is a claim about today's mux,
	// and this is gosec's G710 finding rather than a hypothetical.
	target := (&url.URL{Path: r.URL.Path, RawQuery: q.Encode()}).String()
	if !strings.HasPrefix(target, "/") || strings.HasPrefix(target, "//") {
		s.log().Warn("refusing to redirect a token exchange to a target that is not local",
			"path", r.URL.Path)
		s.fail(w, http.StatusBadRequest, errors.New("the request path cannot be redirected to"))
		return true
	}

	// 303 and not 302: the browser's next request must be a GET of a URL with
	// no token in it, which is what takes the token out of the address bar
	// and out of the session history.
	http.Redirect(w, r, target, http.StatusSeeOther)
	return true
}

// refuseCredential answers a request that could not prove who it is.
//
// THE SAME 403 THE ORIGIN CHECK USES, and the log line records the path and
// the carrier that was present -- never a value and never a digest of one.
// A fingerprint of what was PRESENTED would be a hash of a secret that may
// not be ember's: an operator who pasted the wrong password would have it
// written to the journal permanently, which is the thing node.Secret exists
// to prevent rather than an application of it.
func (s *Server) refuseCredential(w http.ResponseWriter, r *http.Request) {
	carrier := "none"
	switch {
	case r.Header.Get("Authorization") != "":
		carrier = "authorization header"
	default:
		if _, err := r.Cookie(cookieName); err == nil {
			carrier = "cookie"
		}
	}
	s.log().Warn("refused a request that carried no credential this server accepts",
		"path", r.URL.Path, "carrier", carrier)
	s.fail(w, http.StatusForbidden, errNoCredential)
}
