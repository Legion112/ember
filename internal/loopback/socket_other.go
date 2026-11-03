//go:build !unix

package loopback

import (
	"context"
	"fmt"
	"runtime"
)

// ListenSocket refuses, because the mode cannot be made true here.
//
// A REFUSAL AND NOT AN UNMODED SOCKET. Windows has AF_UNIX and net.Listen
// will bind one, and os.Chmod there honours only the read-only bit -- so the
// socket would exist, `ember serve` would announce a channel it describes as
// authenticated by the operating system, and the one thing that authenticates
// it would not be set. That is the "reads as working" defect this repository
// keeps recording, on a security control. The loopback TCP listener and its
// mandatory token are untouched and are what `ember serve` runs on; a
// platform with no file modes simply has no second channel.
//
// A BUILD TAG AND NOT A RUNTIME CHECK, so there is no branch anywhere that
// could conclude at run time that this machine cannot have a socket when it
// can.
func ListenSocket(_ context.Context, _ Socket) (*SocketListener, error) {
	return nil, fmt.Errorf("ember has no local client socket on %s: a unix socket there carries no "+
		"mode, so the %04o that authenticates it cannot be set and a socket without it would be "+
		"reachable by every account on the machine. Use the loopback port and its token",
		runtime.GOOS, socketMode.Perm())
}
