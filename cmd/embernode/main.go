// Command embernode is the entrypoint inside an ember node.
//
// It installs the host key and authorized key ember injected, starts sshd, and
// ends the node at its deadline. It is a Go binary rather than a shell script
// for the reason in AGENTS.md — and for one specific to this file: the
// environment it reads holds a private key, and clearing a variable reliably
// before anything else can read /proc/1/environ is not something a shell
// entrypoint does well.
package main

import (
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	envHostKey    = "EMBER_HOST_KEY"
	envAuthorized = "EMBER_AUTHORIZED_KEY"
	envTTL        = "EMBER_TTL_SECONDS"

	hostKeyPath    = "/etc/ssh/ssh_host_ed25519_key"
	authorizedPath = "/root/.ssh/authorized_keys"
)

func main() {
	// THE LOGGER IS FIRST, before the dispatch and before anything can fail.
	//
	// The fatal line is the one worth having: a node that could not install
	// its host key is a node whose handshake failure ember reads as a
	// possible attack, and the sentence explaining it was previously written
	// to a stream nobody collects, on a machine ember then destroys. Both
	// exits below therefore log rather than print -- see log.go on why a
	// logger that cannot open its file still returns one.
	log := logger()

	// Two subcommands, and everything else is the node entrypoint.
	//
	// Dispatching on os.Args rather than growing a flag package at the top:
	// embernode is PID 1 in the container and is started by the image's
	// ENTRYPOINT with no arguments at all. A parser that could fail on the
	// empty case would be a node that does not come up.
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "fetch":
			if err := runFetch(os.Args[2:]); err != nil {
				// Logged AND printed in the old spelling. `embernode fetch`
				// runs over ember's SSH connection, so this stream is the
				// operator's own terminal and a timestamped record is not
				// what they are reading there; the log copy is for the node's
				// file, which is the only one that survives the node.
				log.Error("asset fetch failed; the workload will not start", "err", err)
				fmt.Fprintf(os.Stderr, "embernode fetch: %v\n", err)
				os.Exit(1)
			}
			return
		case "worker":
			// NOT PRINTED TO STDERR AS WELL, unlike fetch: nobody is watching
			// this stream. The worker is started by the entrypoint above and
			// its output is the node's own log, which ember brings home --
			// which is the one place a failure here can be read, because the
			// operator's terminal is not attached to it.
			if err := runWorker(log, os.Args[2:]); err != nil {
				log.Error("the worker stopped", "err", err)
				os.Exit(1)
			}
			return
		}
	}

	code, err := run(log)
	if err != nil {
		log.Error("the node could not be brought up", "err", err)
		os.Exit(1)
	}
	os.Exit(code)
}

func run(log *slog.Logger) (int, error) {
	if err := installHostKey(); err != nil {
		return 0, err
	}
	if err := installAuthorizedKey(); err != nil {
		return 0, err
	}
	// Fingerprints and never the material; see keys.go, which exists as a
	// function so that keys_test.go can refuse the shorter spelling of that
	// mistake with no node and no Docker.
	logKeys(log)

	// Clear the key material before anything else can read /proc/1/environ.
	// A container's init process keeps its environment for its whole life,
	// and every later `docker exec` inherits it.
	for _, k := range []string{envHostKey, envAuthorized} {
		if err := os.Unsetenv(k); err != nil {
			return 0, fmt.Errorf("clear %s: %w", k, err)
		}
	}

	sshd, err := exec.LookPath("sshd")
	if err != nil {
		return 0, fmt.Errorf("find sshd: %w", err)
	}

	// sshd runs as a child and embernode stays PID 1.
	//
	// The obvious implementation is syscall.Exec, and it is wrong here: exec
	// replaces the whole process image, so the deadline timer below is
	// destroyed along with it and the node never stops itself. That is a
	// safety property quietly doing nothing — on a rented machine it is what
	// ends the bill when the operator's laptop dies — and nothing about the
	// running container would have looked wrong.
	//
	// The cost of staying PID 1 is forwarding signals by hand, below.
	cmd := exec.Command(sshd, "-D", "-e") //nolint:gosec // an absolute path from LookPath
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Start(); err != nil {
		return 0, fmt.Errorf("start sshd: %w", err)
	}

	g := newGroup(log)
	g.add(childSSHD, cmdProc{cmd: cmd})
	log.Info("sshd started", "pid", cmd.Process.Pid)

	// THE WORKER IS STARTED BESIDE sshd AND NOT INSTEAD OF IT, and a worker
	// that could not start is NOT fatal.
	//
	// The choice is between a node that cannot work and a node that cannot be
	// asked why. Returning here would exit PID 1, the container would stop,
	// and ember would report a handshake failure with the real reason -- a
	// port collision in the workload image, a read-only /tmp -- sitting in a
	// log file on a machine that no longer exists and was never reachable.
	// That is the failure log.go was written against, one layer out. Keeping
	// sshd means ember's first job push is refused, that request fails, the
	// rental ends and the destroy runs, so the loss is bounded by one request
	// rather than by the TTL -- and the node's log comes home with the
	// sentence in it.
	if err := startWorker(log, g); err != nil {
		log.Error("the worker did not start; this node can accept no work and ember will say so on its first push", "err", err)
	}

	stopDeadline := armDeadline(log, g)
	defer stopDeadline()

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT)
	defer signal.Stop(sigs)

	return supervise(log, g, sigs)
}

// startWorker starts this same binary's worker subcommand as a child.
//
// os.Executable AND NOT os.Args[0], which would be whatever the container's
// ENTRYPOINT was spelled as. The image spells it absolutely today; a relative
// argv0 with a working directory that is not where the binary lives would make
// this fail at exec time, on a node, with nothing in the image looking wrong.
func startWorker(log *slog.Logger, g *group) error {
	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("find this binary to start the worker: %w", err)
	}
	cmd := exec.Command(self, "worker") //nolint:gosec // this binary's own path
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start the worker: %w", err)
	}
	// Its environment is this process's, which by here has had the host key
	// and the authorized key unset. The child therefore never holds either,
	// and `cat /proc/<worker>/environ` carries no key material -- the same
	// property the unset above was written for, extended to the one process
	// that did not exist when it was.
	g.add(childWorker, cmdProc{cmd: cmd})
	log.Info("the worker started", "pid", cmd.Process.Pid)
	return nil
}

func installHostKey() error {
	pem := os.Getenv(envHostKey)
	if pem == "" {
		return fmt.Errorf("%s is empty: without the key ember minted, the first connection could not be authenticated", envHostKey)
	}
	// OpenSSH refuses a private key whose final line has no newline, and what
	// it prints is "sshd: no hostkeys available -- exiting", which says
	// nothing about newlines. The byte is easy to lose: shell command
	// substitution strips trailing newlines, so any path that carries this
	// value through a shell arrives one byte short. Restoring it here means
	// the node does not depend on every layer above having preserved it.
	if !strings.HasSuffix(pem, "\n") {
		pem += "\n"
	}
	// hostKeyPath is a constant in this file, not input; gosec's taint
	// analysis follows the env value into the contents, not the path.
	if err := os.WriteFile(hostKeyPath, []byte(pem), 0o600); err != nil { //nolint:gosec // constant path
		return fmt.Errorf("write %s: %w", hostKeyPath, err)
	}
	// sshd derives the .pub itself when it is missing, so only the private
	// half has to be installed.
	return nil
}

func installAuthorizedKey() error {
	key := os.Getenv(envAuthorized)
	if key == "" {
		return fmt.Errorf("%s is empty: a node nobody can log into still bills", envAuthorized)
	}
	dir := filepath.Dir(authorizedPath)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	if err := os.WriteFile(authorizedPath, []byte(strings.TrimSpace(key)+"\n"), 0o600); err != nil { //nolint:gosec // constant path
		return fmt.Errorf("write %s: %w", authorizedPath, err)
	}
	return nil
}

// armDeadline ends the node at its deadline even if ember never gets to.
//
// On a rented machine this is what stops the bill when the operator's laptop
// dies mid-run. Here it exercises the same path, so the lab proves the timer
// works rather than assuming it.
//
// IT SIGNALS EVERY CHILD AND NOT sshd ALONE, which is the change a second
// child forced. It took an *exec.Cmd -- sshd's -- and the arrival of a worker
// would have left a deadline that stopped the one process ember uses to reach
// the node while the one spending the card carried on. A deadline that stops
// half of a node is advisory, and the half it left running is the expensive
// half.
//
// A missing or unreadable TTL is not fatal: the node still runs and ember's
// own watchdog still destroys it. Refusing to start would turn a bad number
// into an outage.
func armDeadline(log *slog.Logger, g *group) func() {
	raw := os.Getenv(envTTL)
	if raw == "" {
		// Previously silent, and it is the state worth naming: a node with no
		// injected deadline cannot stop its own bill, so the only thing left
		// is ember's watchdog on a laptop that may be shut.
		log.Warn("no deadline was injected; only ember's own watchdog will stop this node",
			"env", envTTL)
		return func() {}
	}
	secs, err := strconv.Atoi(raw)
	if err != nil || secs <= 0 {
		log.Warn("ignoring an unusable deadline; only ember's own watchdog will stop this node",
			"env", envTTL, "value", raw)
		return func() {}
	}
	log.Info("deadline armed", "seconds", secs)

	timer := time.AfterFunc(time.Duration(secs)*time.Second, func() {
		log.Warn("deadline reached, stopping", "seconds", secs, "children", g.names())
		g.signalAll(syscall.SIGTERM)
	})
	return func() { timer.Stop() }
}
