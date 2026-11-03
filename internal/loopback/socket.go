package loopback

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
)

// THE SECOND KIND OF ADDRESS THIS PACKAGE KNOWS, AND WHY IT IS IN HERE.
//
// A unix socket publishes a PATH where a TCP bind publishes a PORT, and the
// failure is the same shape: a socket in a directory another account can
// write to is a socket another account can unlink and replace with their
// own, so the CLI client would hand the operator's requests -- which can rent
// GPUs -- to whoever got there first. That is loopback.Bind's bug with a
// filesystem in place of a network, which is why it is sealed the same way
// and in the same package rather than beside the thing that wants it.
//
// SO Socket IS Bind's SHAPE. Its fields are unexported, this package exports
// no struct-literal path, and StateSocket is the only constructor -- so the
// only socket paths that exist are the ones it made from a state directory
// plus a basename of this package's own choosing. A caller names a directory
// the way a caller of Port names a port; it cannot name a path.
//
// WHAT THE MODE IS AND WHY IT IS NOT A CHMOD. bind(2) creates the socket file
// with 0777 &^ umask and takes no mode argument, so there is no call that
// creates one at 0600 directly. Measured on this machine, whose umask is
// 0002: a socket bound with no intervention is mode 0775 -- group-writable,
// and connect(2) on a unix socket needs WRITE permission, so every member of
// the operator's primary group could have reached it. The obvious repair is
// os.Chmod after the bind, and between those two calls the socket is already
// connectable. So ListenSocket holds the umask for the length of the bind
// instead, and the socket is 0600 from the moment it exists. See socket_unix.go.
//
// AND THE DIRECTORY IS THE SECOND MECHANISM. Containment twice, by two
// different mechanisms, as the tar extractor is: the socket is 0600, and it
// lives in a directory ember creates at 0700 whose parent is refused if group
// or others can write in it. The mode holds if the directory is ever widened;
// the directory holds if a platform ignores the mode.

// socketDirName is the one name for the directory the socket lives in.
//
// A SUBDIRECTORY OF THE STATE DIRECTORY AND NOT THE STATE DIRECTORY ITSELF,
// because ember creates this one and therefore owns its mode. The state
// directory may predate this feature, may have been made by hand, and is
// named by the operator's own `state_dir`; a socket whose containment depends
// on whatever mode that happens to carry is containment ember cannot promise.
const socketDirName = "socket"

// socketFileName is the socket `ember serve` answers on.
//
// Named for the command rather than for the program, because the path is part
// of what a later client has to find and "which ember is this" is answered by
// the state directory it sits in.
const socketFileName = "serve.sock"

// socketMode is who may connect.
//
// connect(2) on a unix socket requires write permission on the node, so this
// is the whole of the operating system's answer to "which account is asking":
// the operator's own, and nobody else's. Measured: a socket chmod-ed to 0000
// refuses connect even to its owner, so the mode is a live control rather
// than advisory.
const socketMode os.FileMode = 0o600

// socketDirMode is the directory's.
//
// 0700 and not 0711: nothing needs to traverse it but ember, and a directory
// another account cannot list is one whose contents it cannot name.
const socketDirMode os.FileMode = 0o700

// maxSocketPath is the longest path bind(2) accepted here.
//
// MEASURED RATHER THAN READ OUT OF A HEADER, which is this repository's rule
// after `allowedCudaVersions`: on this kernel 107 characters bind and 108
// answer `bind: invalid argument` -- an error that names neither the limit
// nor the length, which is the whole reason for refusing earlier with one
// that does. sun_path is smaller on some platforms and this repository has
// not measured one, so no second number is invented: there, the kernel's own
// refusal is what an operator would see, and it would reach them through the
// same wrapped error.
const maxSocketPath = 107

// Socket is a local socket path proved to be inside a directory ember owns.
//
// The zero value is invalid and ListenSocket refuses it, so a Socket nobody
// set cannot be mistaken for one somebody chose -- Bind's rule, for the same
// reason.
type Socket struct {
	// dir is the directory the socket lives in, kept rather than recomputed
	// so that the thing whose mode is checked and the thing the socket is
	// created in are one value and not two derivations that could differ.
	dir string

	path string
}

// StateSocket is the only way to name a socket: `<stateDir>/socket/serve.sock`.
//
// The caller supplies a directory and this package supplies both the
// subdirectory and the basename, which is what makes "a caller cannot name an
// arbitrary path" true rather than discouraged.
func StateSocket(stateDir string) (Socket, error) {
	d := strings.TrimSpace(stateDir)
	if d == "" {
		return Socket{}, errors.New("refusing to place a socket with no state directory: " +
			"the directory is what bounds who can reach it, so an empty one is not a location")
	}
	dir := filepath.Join(d, socketDirName)
	p := filepath.Join(dir, socketFileName)
	if len(p) > maxSocketPath {
		return Socket{}, fmt.Errorf("the socket path %s is %d characters and bind(2) here takes at most %d; "+
			"set state_dir in ember.yaml to something shorter", p, len(p), maxSocketPath)
	}
	return Socket{dir: dir, path: p}, nil
}

// Path is where the socket is, for an operator to be told and a client to find.
func (s Socket) Path() string { return s.path }

// IsValid reports whether s was produced by a constructor.
func (s Socket) IsValid() bool { return s.dir != "" && s.path != "" }

// SocketListener is a listener ListenSocket made, and nothing else.
//
// A CONCRETE TYPE AND NOT net.Listener, which is the point of it. Whoever
// serves this listener serves requests that the operating system has already
// authenticated, so they get no credential check -- and handing that
// treatment to a TCP listener would replace a token with nothing. The
// argument type is what refuses it: there is no way to produce a valid
// SocketListener except through ListenSocket, so `ServeSocket(ctx, tcpLn)` is
// a compile error rather than a thing a test has to notice. That is the shape
// hostkey.Pin and exposure.PortRequest already use.
type SocketListener struct {
	ln net.Listener
}

// IsValid reports whether l came from ListenSocket.
//
// A SocketListener{} is writable by anyone who imports this package -- the
// type has to be exported for ServeSocket to name it -- so the zero value is
// refused rather than left to panic in Accept, which is Listen's answer for a
// Bind nobody set.
func (l *SocketListener) IsValid() bool { return l != nil && l.ln != nil }

// Accept, Close and Addr are net.Listener, which is what an http.Server takes.
//
// They delegate and add nothing. There is no Socket() accessor beside them on
// purpose: Addr already answers which path this is bound to, and a second
// carrier for one fact is the mistake queue.Cost was.
func (l *SocketListener) Accept() (net.Conn, error) { return l.ln.Accept() }

// Close closes the listener, which also unlinks the socket.
//
// net's own unlink-on-close, which is set for a socket Listen created: a
// `ember serve` that shuts down leaves nothing behind, and only a process
// killed outright leaves a path for the next one to clear.
func (l *SocketListener) Close() error { return l.ln.Close() }

func (l *SocketListener) Addr() net.Addr { return l.ln.Addr() }
