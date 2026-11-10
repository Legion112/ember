package main

import (
	"bytes"
	"errors"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// fakeProc is a supervised process with no process in it.
//
// AGENTS.md: "external binaries at runtime are fine, invoked from Go through
// an interface that has an in-process fake". sshd and the worker are exactly
// that, and until the `proc` interface existed the supervision was four lines
// of select inlined in run() -- so the one thing it decides, what each child's
// exit MEANS, had no exit code anywhere and could not be given one without a
// node and a real sshd.
type fakeProc struct {
	mu      sync.Mutex
	signals []os.Signal
	exit    chan error
	waited  bool
}

func newFakeProc() *fakeProc { return &fakeProc{exit: make(chan error, 1)} }

func (p *fakeProc) Signal(sig os.Signal) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.signals = append(p.signals, sig)
	return nil
}

func (p *fakeProc) Wait() error {
	err := <-p.exit
	p.mu.Lock()
	p.waited = true
	p.mu.Unlock()
	return err
}

func (p *fakeProc) Pid() int { return 4242 }

// got is the signals this child was sent.
func (p *fakeProc) got() []os.Signal {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]os.Signal(nil), p.signals...)
}

// ends makes this child exit with err.
func (p *fakeProc) ends(err error) { p.exit <- err }

// syncBuf is a log buffer two goroutines may write while a third reads it.
//
// NOT A bytes.Buffer, and the race detector is what says so: the deadline
// timer signals from its own goroutine and logs there, supervise logs from
// the run loop, and the assertions read the lines while both are live. A
// plain buffer here is a data race in the test rather than in the program,
// which is the kind that gets a suite marked flaky instead of fixed.
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// nodeLogger is the node's logger over that buffer, at debug so the lines
// this file is about are all present. A _test.go file is exempt from
// internal/logx's one-construction-site walk, which is why keys_test.go
// builds its own too.
func nodeLogger(w io.Writer) *slog.Logger {
	return slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// TestSupervise_SshdExitingOnItsOwnKeepsItsMeaning.
//
// THIS IS THE CASE THAT MAKES A NODE OBSERVABLE AT ALL, and the one a second
// child could most easily have broken. "no hostkeys available -- exiting" is
// what a private key one byte short produces, it happens in the second before
// ember's first dial, and what ember then reports is a connection refused on a
// node that looks perfectly healthy. The sentence and the exit code are the
// whole of the diagnosis.
func TestSupervise_SshdExitingOnItsOwnKeepsItsMeaning(t *testing.T) {
	var b syncBuf
	log := nodeLogger(&b)
	g := newGroup(log)
	sshd := newFakeProc()
	g.add(childSSHD, sshd)

	// An *exec.ExitError with no ProcessState reports -1, which is what this
	// test is about: the routing and the sentence, not the number. A real
	// status needs a real process, and what has to be true here is that
	// whatever sshd's status was reaches the caller.
	sshd.ends(&exec.ExitError{})

	code, err := supervise(log, g, make(chan os.Signal))
	if err != nil {
		t.Fatalf("supervise returned an error for a child that merely exited: %v", err)
	}
	if code != -1 {
		t.Errorf("exit code = %d, want sshd's own", code)
	}
	if !strings.Contains(b.String(), "nothing can connect to this node") {
		t.Errorf("the one sentence that explains a healthy-looking node is missing:\n%s", b.String())
	}
}

// TestSupervise_SshdThatCannotBeWaitedForIsAnError, as opposed to one that
// exited: the first is a bug in embernode and the second is a fact about the
// node.
func TestSupervise_SshdThatCannotBeWaitedForIsAnError(t *testing.T) {
	var b syncBuf
	log := nodeLogger(&b)
	g := newGroup(log)
	sshd := newFakeProc()
	g.add(childSSHD, sshd)
	sshd.ends(errors.New("waitid: no child processes"))

	if _, err := supervise(log, g, make(chan os.Signal)); err == nil {
		t.Fatal("a wait that failed was reported as an ordinary exit")
	}
}

// TestSupervise_TheWorkerExitingDoesNotEndTheNode.
//
// THE OBVIOUS REFACTOR IS WRONG HERE AND THIS IS WHAT CATCHES IT. A
// multiplexer that returned on whichever child exited first would turn a
// worker that finished its queue into a node that tears down its own sshd --
// while ember is still fetching the results the worker just named and the
// node's own log, over that connection, and is about to run the destroy.
//
// So a worker's exit is reported and supervision carries on, and what ends
// the node is ember's destroy or the deadline. Asserted both ways: the worker
// going does not return, and sshd going afterwards still does.
func TestSupervise_TheWorkerExitingDoesNotEndTheNode(t *testing.T) {
	var b syncBuf
	log := nodeLogger(&b)
	g := newGroup(log)
	sshd, w := newFakeProc(), newFakeProc()
	g.add(childSSHD, sshd)
	g.add(childWorker, w)

	done := make(chan int, 1)
	go func() {
		code, err := supervise(log, g, make(chan os.Signal))
		if err != nil {
			t.Error(err)
		}
		done <- code
	}()

	// The worker finished its queue.
	w.ends(nil)
	select {
	case code := <-done:
		t.Fatalf("supervise returned %d when the worker exited; sshd is what ember is "+
			"still using to fetch results and the node's own log", code)
	case <-time.After(200 * time.Millisecond):
	}
	if !strings.Contains(b.String(), "a child exited cleanly") {
		t.Errorf("the worker's exit was not reported, so a rental that did nothing has no record:\n%s", b.String())
	}
	// AND IT DOES NOT SAY WHY. A run of the real node image with a
	// five-second TTL put "the worker's queue is empty and it has exited" one
	// line under the worker's own "stopped before its queue was empty": a
	// supervisor sees a status and nothing else, so a line that asserts the
	// reason is wrong on exactly the path that matters -- a deadline ending a
	// rental nobody is watching.
	if strings.Contains(b.String(), "queue is empty") {
		t.Errorf("the supervisor claims to know why the worker exited, which it cannot:\n%s", b.String())
	}

	// And sshd going still ends it.
	sshd.ends(nil)
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("WEDGED: sshd exited and supervise did not return, so the node would bill " +
			"until its deadline with nothing able to connect")
	}
}

// TestSupervise_AWorkerThatCrashedSaysTheNodeCanTakeNoWork.
//
// Two different sentences on purpose: a clean exit means the node is finished
// with, and a crash means the node is still reachable and can no longer be
// given work -- a state ember discovers on its next push and an operator can
// otherwise only infer from a rental that produced nothing.
func TestSupervise_AWorkerThatCrashedSaysTheNodeCanTakeNoWork(t *testing.T) {
	var b syncBuf
	log := nodeLogger(&b)
	g := newGroup(log)
	sshd, w := newFakeProc(), newFakeProc()
	g.add(childSSHD, sshd)
	g.add(childWorker, w)

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = supervise(log, g, make(chan os.Signal))
	}()
	w.ends(&exec.ExitError{})
	// Waited for by reading the log rather than by sleeping: the line is the
	// assertion.
	deadline := time.Now().Add(10 * time.Second)
	for !strings.Contains(b.String(), "can no longer be given work") {
		if time.Now().After(deadline) {
			t.Fatalf("a crashed worker was not reported:\n%s", b.String())
		}
		time.Sleep(time.Millisecond)
	}
	sshd.ends(nil)
	<-done
}

// TestSupervise_ASignalReachesEveryChildAndWaitsForAllOfThem.
//
// Forward and wait, so `docker stop` is quick rather than taking the full
// ten-second grace period before a kill -- and EVERY child, because a signal
// that stopped sshd and left the worker generating would be a container the
// daemon has to kill with a card still working inside it.
func TestSupervise_ASignalReachesEveryChildAndWaitsForAllOfThem(t *testing.T) {
	var b syncBuf
	log := nodeLogger(&b)
	g := newGroup(log)
	sshd, w := newFakeProc(), newFakeProc()
	g.add(childSSHD, sshd)
	g.add(childWorker, w)

	sigs := make(chan os.Signal, 1)
	done := make(chan int, 1)
	go func() {
		code, err := supervise(log, g, sigs)
		if err != nil {
			t.Error(err)
		}
		done <- code
	}()
	sigs <- syscall.SIGTERM

	// It must not return before both children have been reaped.
	select {
	case code := <-done:
		t.Fatalf("supervise returned %d before its children had exited", code)
	case <-time.After(100 * time.Millisecond):
	}
	for _, p := range []*fakeProc{sshd, w} {
		if got := p.got(); len(got) != 1 || got[0] != syscall.SIGTERM {
			t.Errorf("a child was sent %v, want one SIGTERM", got)
		}
	}
	sshd.ends(nil)
	w.ends(nil)
	select {
	case code := <-done:
		// Zero, because the node is being stopped on purpose: reporting a
		// child's reaction to a signal we sent would turn an orderly
		// shutdown into a non-zero container exit.
		if code != 0 {
			t.Errorf("exit code = %d, want 0 for a deliberate stop", code)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("WEDGED: supervise did not return after both children exited")
	}
}

// TestArmDeadline_StopsEveryChildAndNotJustSshd.
//
// THE MONEY ASSERTION IN THIS FILE. On a rented machine this timer is what
// ends the bill when the operator's laptop dies mid-run, and it took an
// *exec.Cmd -- sshd's. With a second child beside it, a deadline that signals
// sshd alone stops the one process ember uses to REACH the node and leaves the
// one SPENDING the card running: the connection goes, the generation carries
// on, and nothing is watching. A deadline that stops half a node is advisory,
// and the half it leaves running is the expensive half.
//
// One second, because that is the smallest deadline armDeadline accepts and
// what is being distinguished is "both" from "one".
func TestArmDeadline_StopsEveryChildAndNotJustSshd(t *testing.T) {
	t.Setenv(envTTL, "1")

	var b syncBuf
	log := nodeLogger(&b)
	g := newGroup(log)
	sshd, w := newFakeProc(), newFakeProc()
	g.add(childSSHD, sshd)
	g.add(childWorker, w)

	stop := armDeadline(log, g)
	defer stop()

	deadline := time.Now().Add(20 * time.Second)
	for {
		s, ww := sshd.got(), w.got()
		if len(s) > 0 && len(ww) > 0 {
			if s[0] != syscall.SIGTERM || ww[0] != syscall.SIGTERM {
				t.Errorf("signals were %v and %v, want SIGTERM to both", s, ww)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("after the deadline: sshd got %v and the worker got %v -- "+
				"a deadline that stops one child leaves the other billing", s, ww)
		}
		time.Sleep(time.Millisecond)
	}
	if !strings.Contains(b.String(), "deadline reached") {
		t.Errorf("the deadline was not reported:\n%s", b.String())
	}
	// Both children named on the line, because "deadline reached" over a node
	// where only one of them stopped is the log agreeing with a bug.
	for _, name := range []string{childSSHD, childWorker} {
		if !strings.Contains(b.String(), name) {
			t.Errorf("the deadline line does not name %s:\n%s", name, b.String())
		}
	}

	// Drained so the fakes' Wait goroutines do not outlive the test.
	sshd.ends(nil)
	w.ends(nil)
}

// TestArmDeadline_ANodeWithNoUsableDeadlineSaysSoAndStillRuns.
//
// A missing or unreadable TTL is not fatal: refusing to start would turn a bad
// number into an outage, and ember's own watchdog still destroys the node. It
// was previously SILENT, and that is the state worth naming -- a node with no
// injected deadline cannot stop its own bill, so the only thing left is a
// watchdog on a laptop that may be shut.
func TestArmDeadline_ANodeWithNoUsableDeadlineSaysSoAndStillRuns(t *testing.T) {
	for _, tc := range []struct{ name, ttl, want string }{
		{"none injected", "", "no deadline was injected"},
		{"not a number", "soon", "unusable deadline"},
		{"zero", "0", "unusable deadline"},
		{"negative", "-5", "unusable deadline"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(envTTL, tc.ttl)
			var b syncBuf
			log := nodeLogger(&b)
			g := newGroup(log)
			p := newFakeProc()
			g.add(childSSHD, p)

			stop := armDeadline(log, g)
			stop()

			if !strings.Contains(b.String(), tc.want) {
				t.Errorf("log = %q, want it to mention %q", b.String(), tc.want)
			}
			if got := p.got(); len(got) != 0 {
				t.Errorf("a node with no usable deadline signalled %v", got)
			}
			p.ends(nil)
		})
	}
}

// TestArmDeadline_StopCancelsTheTimer, so a node that was torn down in good
// order does not fire a SIGTERM at children that are already gone -- and, more
// to the point, so the timer does not hold a process that is trying to exit.
func TestArmDeadline_StopCancelsTheTimer(t *testing.T) {
	t.Setenv(envTTL, "1")
	var b syncBuf
	log := nodeLogger(&b)
	g := newGroup(log)
	p := newFakeProc()
	g.add(childSSHD, p)

	armDeadline(log, g)()
	time.Sleep(1200 * time.Millisecond)
	if got := p.got(); len(got) != 0 {
		t.Errorf("a stopped deadline fired anyway: %v", got)
	}
	p.ends(nil)
}

// TestGroup_SignallingAChildThatHasGoneIsNotFatal.
//
// The ordinary case: the deadline fires after the worker has already finished
// its queue. os.Process refuses to signal a process it has waited for rather
// than signalling a pid the kernel may have reused, so this is at debug and
// never an error -- and a group that gave up on the first refusal would leave
// the other child running.
func TestGroup_SignallingAChildThatHasGoneIsNotFatal(t *testing.T) {
	var b syncBuf
	log := nodeLogger(&b)
	g := newGroup(log)
	gone, live := newRefusingProc(), newFakeProc()
	g.add(childWorker, gone)
	g.add(childSSHD, live)

	g.signalAll(syscall.SIGTERM)
	if got := live.got(); len(got) != 1 {
		t.Errorf("the live child was sent %v; a refusal from the other one stopped the loop", got)
	}
	live.ends(nil)
	gone.exit <- nil
}

// refusingProc is a child that has already been waited for.
type refusingProc struct{ exit chan error }

func newRefusingProc() *refusingProc { return &refusingProc{exit: make(chan error, 1)} }

func (p *refusingProc) Signal(os.Signal) error { return os.ErrProcessDone }
func (p *refusingProc) Wait() error            { return <-p.exit }
func (p *refusingProc) Pid() int               { return 1 }
