//go:build unix

package loopback

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

// socketUmask is socketMode said the only way bind(2) accepts it.
//
// Derived rather than written, so the two cannot disagree: bind creates the
// socket with 0777 &^ umask, so the mask is the complement of the mode within
// the permission bits. 0o777 &^ 0o600 is 0o177.
const socketUmask = 0o777 &^ int(socketMode)

// umaskMu serializes the one window in this program where a process-global is
// changed.
//
// WHAT THE WINDOW COSTS, measured rather than waved at. umask is per-process,
// so for the length of one bind(2) any other goroutine creating a file gets
// 0600 and any creating a directory gets 0700. Every mode literal in ember's
// own packages is already 0600 or 0700 -- there are 28 of them and not one is
// wider -- so nothing ember creates can come out different, and anything that
// did would come out STRICTER, which is never the direction that costs
// security. The alternative is os.Chmod after the bind, which leaves the
// socket at 0775 on this machine's umask for the length of two syscalls, and
// a window in which another account can connect is the thing the mode exists
// to close.
var umaskMu sync.Mutex

// staleDial bounds the "is somebody already listening" probe.
//
// connect(2) on a unix socket does not go near a network, so this is not a
// round trip -- it is the bound on a socket whose owner has stopped accepting
// and whose backlog is full, which is the one way the call can block.
const staleDial = 2 * time.Second

// ListenSocket binds s at socketMode, inside a directory ember owns.
//
// THE ORDER IS THE WHOLE OF IT.
//
//  1. The state directory is refused if group or others can write in it,
//     before anything is created: a parent anyone can write to is a parent in
//     which the socket directory can be renamed away and replaced, so the
//     mode on what is inside proves nothing. That is keys.LoadSigner's rule
//     about a private key's directory, reached through a socket.
//  2. The socket directory is refused on the same test if it already exists.
//     One ember created is 0700; one that is wider is one ember did not make.
//  3. It is created at 0700. MkdirAll applies the umask, which can only
//     remove bits, so a directory asked for at 0700 is never created wider.
//  4. A path somebody is already listening on is a refusal and not a
//     cleanup. Unlinking it would let a second `ember serve` take the socket
//     out from under the first, which is worse than refusing to start.
//  5. Only then the bind, under the umask that makes the socket 0600 at
//     creation. See socket.go for why this is not a chmod.
//
// IT IS STRICTER THAN keys.LoadSigner's 0o002 ON PURPOSE. That one guards a
// file an operator placed, where a group-writable directory is a
// configuration somebody may have meant; this one guards a directory ember
// creates for itself, so there is no wider mode to tolerate -- and what a
// group member can do with it, unlink the socket and leave their own listener
// at the same path, is the same impersonation world-writability buys.
func ListenSocket(ctx context.Context, s Socket) (*SocketListener, error) {
	if !s.IsValid() {
		return nil, errors.New("refusing to listen on an unset socket path: " +
			"a Socket nobody set is not a Socket somebody chose")
	}

	if err := refuseUnsafeDir(filepath.Dir(s.dir), false); err != nil {
		return nil, err
	}
	if err := refuseUnsafeDir(s.dir, true); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(s.dir, socketDirMode); err != nil {
		return nil, fmt.Errorf("create the socket directory %s: %w", s.dir, err)
	}

	if err := refuseLiveSocket(ctx, s.path); err != nil {
		return nil, err
	}
	// A socket left behind by a process that was killed outright -- the
	// labs do that on purpose and so does an OOM kill. Its error is dropped
	// because the only thing it can mean is that the bind below will fail,
	// and the bind's message names the path and asks whether another ember
	// serve is running, which is the sentence an operator needs either way. A
	// second error return for it would be a branch reachable only with a
	// directory this process owns and cannot write to.
	_ = os.Remove(s.path)

	ln, err := bindAt(ctx, s.path)
	if err != nil {
		return nil, fmt.Errorf("listen on %s: %w; is another ember serve running?", s.path, err)
	}
	return &SocketListener{ln: ln}, nil
}

// refuseUnsafeDir refuses a directory group or others can write in.
//
// mayBeMissing is for the socket directory, which is about to be created and
// whose absence is the ordinary first run. The state directory is not allowed
// to be missing: ember's own store made it before this is reached, so a
// missing one means the paths were resolved against something else.
func refuseUnsafeDir(dir string, mayBeMissing bool) error {
	fi, err := os.Stat(dir)
	switch {
	case mayBeMissing && errors.Is(err, fs.ErrNotExist):
		return nil
	case err != nil:
		return fmt.Errorf("stat %s, which is where ember's socket goes: %w", dir, err)
	}
	if m := fi.Mode().Perm(); m&0o022 != 0 {
		return fmt.Errorf("directory %s is mode %04o, so group or others can write in it: "+
			"a socket there can be unlinked and replaced with somebody else's listener, "+
			"which would hand ember's requests to them (chmod 700 %s)", dir, m, dir)
	}
	return nil
}

// refuseLiveSocket refuses a path something is already accepting on.
//
// The answer that matters is the negative one: a dial that fails means
// nothing holds the path, so whatever is there is a leftover and may be
// removed. A dial that succeeds means an `ember serve` is up, and a second
// one must not take its socket -- the first would go on serving a path
// nothing can reach, which is a running server an operator cannot talk to.
func refuseLiveSocket(ctx context.Context, path string) error {
	d := net.Dialer{Timeout: staleDial}
	c, err := d.DialContext(ctx, "unix", path)
	if err != nil {
		return nil
	}
	_ = c.Close()
	return fmt.Errorf("%s is a live socket: another ember serve is already running, "+
		"and taking its socket would leave it serving a path nothing can reach", path)
}

// bindAt is the one bind(2) in ember that creates a filesystem node.
//
// The umask is restored before the lock is released, so no other caller of
// this function can observe it, and the deferred restore runs even if Listen
// panics. See umaskMu for what the window costs.
func bindAt(ctx context.Context, path string) (net.Listener, error) {
	umaskMu.Lock()
	defer umaskMu.Unlock()
	old := syscall.Umask(socketUmask)
	defer syscall.Umask(old)

	var lc net.ListenConfig
	return lc.Listen(ctx, "unix", path)
}
