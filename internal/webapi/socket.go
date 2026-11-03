package webapi

import (
	"context"
	"errors"
	"net"
	"net/http"

	"github.com/Legion112/ember/internal/loopback"
)

// THE SECOND CHANNEL, AND WHAT AUTHENTICATES IT.
//
// The loopback port is reachable by every uid on the machine, which is why
// every request on it has to carry a token -- auth.go is the whole argument.
// A unix socket at 0600 answers the same threat with no secret at all:
// connect(2) needs write permission on the socket node, so the kernel refuses
// the account next door before ember sees a byte. AGENTS.md records that as
// the alternative that was NOT taken for #29, for two reasons that both still
// hold: it answers nothing about `ssh -L`, where the client end is a TCP port
// on loopback again, and a browser cannot speak to it. Neither is a reason
// not to have it as well -- the token is still mandatory and still the only
// answer on TCP -- and #146 needs exactly the thing a browser cannot reach:
// a channel for the CLI, on a machine where the browser's token is already
// working.
//
// WHAT MARKS A REQUEST AS HAVING ARRIVED HERE, which is the only part of this
// that could be a hole. It is a value on the request's CONTEXT, put there by
// http.Server.ConnContext on the server that serves the socket and by nothing
// else. A client cannot set a context value; there is no header, no query
// parameter and no trusted-proxy list anywhere in this file, so there is no
// spelling of "I came in on the socket" that a request can claim. If it were
// a header, the TCP listener would accept the header too and the credential
// would have been replaced by a string anyone can type -- which is worse than
// no change at all.
//
// AND THE TCP SERVER CANNOT ACQUIRE IT. The two listeners are served by two
// http.Servers whose only difference is this one field, rather than by one
// server that inspects the connection's type -- because a type switch on a
// net.Conn re-derives the fact from the connection, and the listener is what
// loopback proved. ServeSocket takes a *loopback.SocketListener, a type whose
// only constructor is loopback.ListenSocket, so handing it a TCP listener is
// a compile error.
//
// THE COOKIE AND THE ?t= EXCHANGE ARE LEFT ALONE HERE. Both are browser
// mechanisms, and a browser cannot reach this socket; a request on it carrying
// either is the operator's own curl, and the exchange does nothing to a client
// that keeps no cookie jar. Refusing them would be a branch only the account
// that is already authenticated could reach.

// socketChannel is the context key AND its own value type.
//
// An unexported zero-size type used as both means nothing outside this package
// can construct the key, so nothing outside this package can put the value on
// a context -- which is the property the paragraph above rests on. A string
// key would be reachable by anybody who guessed the string.
type socketChannel struct{}

// fromSocket marks ctx as a connection that arrived on the unix socket.
//
// Called from exactly one place, ServeSocket's ConnContext. That is worth
// stating because the whole access decision turns on nobody else calling it.
func fromSocket(ctx context.Context) context.Context {
	return context.WithValue(ctx, socketChannel{}, socketChannel{})
}

// arrivedOnSocket reports whether the operating system has already said who
// is asking.
func arrivedOnSocket(ctx context.Context) bool {
	_, ok := ctx.Value(socketChannel{}).(socketChannel)
	return ok
}

// SocketPath is where the local client channel is, or "" for a server with none.
//
// `ember serve` prints it, because a channel nothing can find is a channel
// nobody uses.
func (s *Server) SocketPath() string { return s.opts.Socket.Path() }

// ListenSocket binds the local client channel.
//
// SEPARATE FROM ServeSocket FOR Listen's REASON: the path is bound before
// anything is announced, so an operator is never told about a socket that is
// not accepting yet, and "is another ember serve running?" is answered before
// this one claims to have started.
func (s *Server) ListenSocket(ctx context.Context) (*loopback.SocketListener, error) {
	if !s.opts.Socket.IsValid() {
		return nil, errors.New("this server was built with no socket path; cmd_serve resolves one " +
			"from the state directory, and a handler test that does not need the channel passes none")
	}
	return loopback.ListenSocket(ctx, s.opts.Socket)
}

// ServeSocket accepts on the socket until ctx ends, then drains and closes it.
//
// EVERYTHING IT SERVES IS AUTHENTICATED BY THE OPERATING SYSTEM, which is the
// one line of difference from Serve below it: ConnContext marks the
// connection, and authenticate reads that mark instead of looking for a
// token. The Host check is dropped for a request that arrives here and the
// Origin check is not -- see hostOK.
func (s *Server) ServeSocket(ctx context.Context, ln *loopback.SocketListener) error {
	if !ln.IsValid() {
		return errors.New("refusing to serve a socket listener nobody opened: " +
			"only loopback.ListenSocket produces one whose mode and directory have been checked")
	}
	return serveOn(ctx, &http.Server{
		Handler:           s.mux,
		ReadHeaderTimeout: readHeaderTimeout,
		ConnContext: func(ctx context.Context, _ net.Conn) context.Context {
			return fromSocket(ctx)
		},
	}, ln)
}
