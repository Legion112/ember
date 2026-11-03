//go:build unix

package loopback

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// stateDir is a state directory of the mode ember's own store makes.
func stateDir(t *testing.T) string {
	t.Helper()
	d := filepath.Join(t.TempDir(), "state")
	if err := os.MkdirAll(d, 0o700); err != nil {
		t.Fatal(err)
	}
	return d
}

// TestStateSocket_NamesThePathItself, which is the seal rather than a
// convenience.
//
// A caller supplies a DIRECTORY the way a caller of Port supplies a port, and
// this package supplies the subdirectory and the basename -- so there is no
// argument to StateSocket that puts a socket where ember did not mean to put
// one, and no struct literal that gets around it.
func TestStateSocket_NamesThePathItself(t *testing.T) {
	s, err := StateSocket("/var/lib/ember")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := s.Path(), "/var/lib/ember/socket/serve.sock"; got != want {
		t.Errorf("Path() = %q, want %q", got, want)
	}
	if !s.IsValid() {
		t.Error("a Socket a constructor made reports itself invalid")
	}
	// And the zero value is not one somebody chose.
	if (Socket{}).IsValid() {
		t.Error("the zero Socket reports itself valid")
	}
	if p := (Socket{}).Path(); p != "" {
		t.Errorf("the zero Socket has a path: %q", p)
	}
}

// TestStateSocket_RefusesAnEmptyDirectory.
//
// The directory is what bounds who can reach the socket, so a socket with no
// directory is not a stricter location -- it is a relative path against
// whatever directory ember happened to be started in.
func TestStateSocket_RefusesAnEmptyDirectory(t *testing.T) {
	for _, in := range []string{"", "   "} {
		if _, err := StateSocket(in); err == nil {
			t.Errorf("StateSocket(%q) was accepted", in)
		}
	}
}

// TestStateSocket_RefusesAPathLongerThanBindTakes.
//
// bind(2) answers `invalid argument` over a path longer than sun_path, which
// names neither the limit nor the length -- so an operator with a long
// state_dir would read a message about an argument with no argument in it.
// Measured on this kernel: 107 characters bind and 108 do not.
func TestStateSocket_RefusesAPathLongerThanBindTakes(t *testing.T) {
	long := "/" + strings.Repeat("d", maxSocketPath)
	_, err := StateSocket(long)
	if err == nil {
		t.Fatal("a path longer than bind takes was accepted")
	}
	for _, want := range []string{"bind(2)", "state_dir"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not mention %q: %v", want, err)
		}
	}

	// And the boundary itself is accepted, so the refusal is a limit rather
	// than a blanket. Without this the check would pass against a guard that
	// refused every path there is.
	dir := strings.Repeat("d", maxSocketPath-len("/socket/serve.sock")-1)
	s, err := StateSocket("/" + dir)
	if err != nil {
		t.Fatalf("a path of exactly the limit was refused: %v", err)
	}
	if len(s.Path()) != maxSocketPath {
		t.Fatalf("this test is measuring the wrong length: %d", len(s.Path()))
	}
}

// TestListenSocket_CreatesItAtSixHundredAndNotTheUmask.
//
// THE ASSERTION THIS WHOLE FILE IS FOR. bind(2) takes no mode and creates the
// socket at 0777 &^ umask: measured on this machine, whose umask is 0002, a
// socket bound with no intervention is 0775 -- and connect(2) on a unix
// socket needs WRITE permission, so every member of the operator's primary
// group could reach it. The mode is therefore the authentication, and it has
// to be right at creation rather than after an os.Chmod, because between a
// bind and a chmod the socket is already connectable.
//
// The directory is asserted beside it: containment twice, by two mechanisms,
// so the mode holds if the directory is widened and the directory holds if a
// filesystem ignores the mode.
func TestListenSocket_CreatesItAtSixHundredAndNotTheUmask(t *testing.T) {
	s, err := StateSocket(stateDir(t))
	if err != nil {
		t.Fatal(err)
	}
	ln, err := ListenSocket(t.Context(), s)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()

	// THE MODES ARE SPELLED HERE AS LITERALS AND NOT AS THE CONSTANTS, which
	// is the difference between an assertion and a tautology. The first
	// version of this test compared against socketMode and socketDirMode, so
	// a change to either constant moved the expected value with it: a
	// socketMode of 0o666 would have been asserted to be 0o666 and passed.
	// Measured by reinstating exactly that -- green against the constants,
	// red against these. The expected value must not be what the bug
	// produces.
	//
	// The constants are checked too, from the other side, because socketUmask
	// is derived from socketMode and a derivation has to be shown to work.
	if socketMode != 0o600 || socketDirMode != 0o700 || socketUmask != 0o177 {
		t.Errorf("the modes are %04o/%04o with umask %04o, want 0600/0700/0177",
			socketMode, socketDirMode, socketUmask)
	}

	fi, err := os.Stat(s.Path())
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode().Perm(); got != 0o600 {
		t.Errorf("the socket is mode %04o, want 0600: anything wider is an account next door that can connect", got)
	}
	if fi.Mode().Type()&os.ModeSocket == 0 {
		t.Errorf("%s is not a socket: %v", s.Path(), fi.Mode())
	}

	di, err := os.Stat(filepath.Dir(s.Path()))
	if err != nil {
		t.Fatal(err)
	}
	if got := di.Mode().Perm(); got != 0o700 {
		t.Errorf("the socket directory is mode %04o, want 0700: another account must not be able "+
			"to traverse to the socket or to replace it", got)
	}

	// It is a listener, and it answers. Asserting the mode alone would pass
	// against a socket nothing is accepting on.
	if !ln.IsValid() {
		t.Error("a listener ListenSocket made reports itself invalid")
	}
	if got := ln.Addr().String(); got != s.Path() {
		t.Errorf("Addr() = %q, want %q", got, s.Path())
	}
	accepted := make(chan error, 1)
	go func() {
		c, aerr := ln.Accept()
		if aerr == nil {
			_ = c.Close()
		}
		accepted <- aerr
	}()
	c, err := net.Dial("unix", s.Path())
	if err != nil {
		t.Fatalf("dialling the socket ember just made: %v", err)
	}
	_ = c.Close()
	if aerr := <-accepted; aerr != nil {
		t.Errorf("Accept: %v", aerr)
	}

	// Close unlinks it, which is why a clean shutdown leaves nothing for the
	// next run to clear.
	if err := ln.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := os.Stat(s.Path()); !os.IsNotExist(err) {
		t.Errorf("the socket survived Close: %v", err)
	}
}

// TestListenSocket_RefusesAnUnsetSocket, which is Listen's rule for a Bind
// nobody set.
func TestListenSocket_RefusesAnUnsetSocket(t *testing.T) {
	if _, err := ListenSocket(t.Context(), Socket{}); err == nil {
		t.Error("ListenSocket accepted a Socket nobody constructed")
	}
	// And the listener type's zero value says so too, rather than panicking
	// in Accept once an http.Server has it.
	var l *SocketListener
	if l.IsValid() {
		t.Error("a nil *SocketListener reports itself valid")
	}
	if (&SocketListener{}).IsValid() {
		t.Error("a SocketListener nobody opened reports itself valid")
	}
}

// TestListenSocket_RefusesADirectoryOthersCanWriteIn.
//
// keys.LoadSigner's rule about a private key's directory, reached through a
// socket: the mode on the thing inside proves nothing when anyone can replace
// the thing. Here the replacement is worse than a substituted key, because
// another account's listener at ember's path receives the operator's own
// requests -- which can rent GPUs -- and answers them.
//
// BOTH DIRECTORIES, because either one is enough. A writable parent means the
// socket directory can be renamed away and a new one put in its place, which
// defeats the mode on the directory ember made.
func TestListenSocket_RefusesADirectoryOthersCanWriteIn(t *testing.T) {
	for _, c := range []struct {
		name string
		mode os.FileMode
		// which of the two directories is widened
		child bool
	}{
		{"a world-writable state directory", 0o777, false},
		{"a group-writable state directory", 0o770, false},
		{"a world-writable socket directory", 0o777, true},
		{"a group-writable socket directory", 0o720, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			st := stateDir(t)
			s, err := StateSocket(st)
			if err != nil {
				t.Fatal(err)
			}
			target := st
			if c.child {
				target = filepath.Dir(s.Path())
				if err := os.Mkdir(target, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Chmod(target, c.mode); err != nil {
				t.Fatal(err)
			}
			ln, err := ListenSocket(t.Context(), s)
			if err == nil {
				_ = ln.Close()
				t.Fatalf("%s was accepted", c.name)
			}
			if !strings.Contains(err.Error(), "chmod 700") {
				t.Errorf("the refusal does not say how to fix it: %v", err)
			}
		})
	}
}

// TestListenSocket_ReportsAStateDirectoryThatIsNotThere.
//
// The parent is not allowed to be missing: ember's own store makes it before
// this is reached, so an absent one means the paths were resolved against
// something else -- and creating it here would be this package deciding where
// ember's state lives.
func TestListenSocket_ReportsAStateDirectoryThatIsNotThere(t *testing.T) {
	s, err := StateSocket(filepath.Join(t.TempDir(), "never-made"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ListenSocket(t.Context(), s); err == nil {
		t.Error("ListenSocket invented a state directory")
	}
}

// TestListenSocket_ReportsADirectoryItCannotCreate.
func TestListenSocket_ReportsADirectoryItCannotCreate(t *testing.T) {
	st := stateDir(t)
	s, err := StateSocket(st)
	if err != nil {
		t.Fatal(err)
	}
	// A regular file where the socket directory has to go.
	if err := os.WriteFile(filepath.Dir(s.Path()), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ListenSocket(t.Context(), s); err == nil {
		t.Error("ListenSocket started with a file where its directory goes")
	}
}

// TestListenSocket_RefusesToTakeALiveSocketAndClearsADeadOne.
//
// THE TWO HALVES ARE THE SAME MEASUREMENT AND MUST BOTH HOLD. A socket left
// by a process that was killed outright -- which three labs here do on
// purpose, and so does an OOM kill -- must not stop the next `ember serve`
// from starting; and a socket a running one is accepting on must not be taken
// out from under it, because the first would then go on serving a path
// nothing can reach. A guard that only did the first would unlink a live
// server's channel, and one that only did the second would need an operator
// to clean up after a crash.
func TestListenSocket_RefusesToTakeALiveSocketAndClearsADeadOne(t *testing.T) {
	t.Run("a live one is refused", func(t *testing.T) {
		s, err := StateSocket(stateDir(t))
		if err != nil {
			t.Fatal(err)
		}
		first, err := ListenSocket(t.Context(), s)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = first.Close() }()

		second, err := ListenSocket(t.Context(), s)
		if err == nil {
			_ = second.Close()
			t.Fatal("a second ember serve took the first one's socket")
		}
		if !strings.Contains(err.Error(), "already running") {
			t.Errorf("the refusal does not name the cause: %v", err)
		}
		// And the first one still answers, which is the property the refusal
		// exists for rather than the refusal itself.
		c, derr := net.Dial("unix", s.Path())
		if derr != nil {
			t.Fatalf("the first server's socket stopped answering: %v", derr)
		}
		_ = c.Close()
	})

	t.Run("a dead one is cleared", func(t *testing.T) {
		s, err := StateSocket(stateDir(t))
		if err != nil {
			t.Fatal(err)
		}
		// What a SIGKILL leaves: the node is there and nothing is accepting.
		// Reproduced by binding and then closing the listener's file
		// descriptor without letting net unlink it.
		if err := os.MkdirAll(filepath.Dir(s.Path()), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(s.Path(), nil, 0o600); err != nil {
			t.Fatal(err)
		}
		ln, err := ListenSocket(t.Context(), s)
		if err != nil {
			t.Fatalf("a leftover from a killed process stopped ember starting: %v", err)
		}
		defer func() { _ = ln.Close() }()
		fi, err := os.Stat(s.Path())
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Type()&os.ModeSocket == 0 {
			t.Errorf("the leftover was not replaced by a socket: %v", fi.Mode())
		}
		if got := fi.Mode().Perm(); got != 0o600 {
			t.Errorf("the replacement is mode %04o, want 0600", got)
		}
	})
}

// TestListenSocket_ReportsABindItCannotMake.
//
// The path is refused earlier when it is too long, so what is left that bind
// can fail on is a path that is already something it cannot replace. A
// non-empty directory at the socket's own name is that: the dial finds
// nothing listening, the unlink cannot remove it, and the bind says so --
// which is why the unlink's own error is dropped rather than carried.
func TestListenSocket_ReportsABindItCannotMake(t *testing.T) {
	st := stateDir(t)
	s, err := StateSocket(st)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(s.Path(), "inside"), 0o700); err != nil {
		t.Fatal(err)
	}
	ln, err := ListenSocket(t.Context(), s)
	if err == nil {
		_ = ln.Close()
		t.Fatal("ListenSocket reported success over a path it cannot have bound")
	}
	if !strings.Contains(err.Error(), "another ember serve") {
		t.Errorf("the refusal does not ask the one question worth asking: %v", err)
	}
}
