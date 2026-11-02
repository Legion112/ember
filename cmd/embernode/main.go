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
	"errors"
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

	// One subcommand, and everything else is the node entrypoint.
	//
	// Dispatching on os.Args rather than growing a flag package at the top:
	// embernode is PID 1 in the container and is started by the image's
	// ENTRYPOINT with no arguments at all. A parser that could fail on the
	// empty case would be a node that does not come up.
	if len(os.Args) > 1 && os.Args[1] == "fetch" {
		if err := runFetch(os.Args[2:]); err != nil {
			// Logged AND printed in the old spelling. `embernode fetch` runs
			// over ember's SSH connection, so this stream is the operator's
			// own terminal and a timestamped record is not what they are
			// reading there; the log copy is for the node's file, which is
			// the only one that survives the node.
			log.Error("asset fetch failed; the workload will not start", "err", err)
			fmt.Fprintf(os.Stderr, "embernode fetch: %v\n", err)
			os.Exit(1)
		}
		return
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

	log.Info("sshd started", "pid", cmd.Process.Pid)

	stopDeadline := armDeadline(log, cmd)
	defer stopDeadline()

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT)
	defer signal.Stop(sigs)

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	select {
	case sig := <-sigs:
		log.Info("signal received, forwarding it to sshd and stopping", "signal", sig.String())
		// Forward and wait, so `docker stop` is quick rather than taking the
		// full ten-second grace period before a kill.
		_ = cmd.Process.Signal(sig)
		// Wait for sshd to finish exiting so docker stop is quick. Its exit
		// status is not interesting here: the node is being stopped on
		// purpose, and reporting sshd's reaction to a signal we sent would
		// turn an orderly shutdown into a non-zero container exit.
		<-done //nolint:errcheck // deliberate: see above
		return 0, nil
	case err := <-done:
		// sshd exiting on its own is the interesting case, and the one that
		// had no record at all: "no hostkeys available -- exiting" is what a
		// private key one byte short produces, it happens in the second
		// before ember's first dial, and what ember then reports is a
		// connection refused on a node that looks perfectly healthy.
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			log.Error("sshd exited on its own; nothing can connect to this node",
				"exit", exitErr.ExitCode())
			return exitErr.ExitCode(), nil
		}
		if err != nil {
			return 0, fmt.Errorf("sshd: %w", err)
		}
		return 0, nil
	}
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
// A missing or unreadable TTL is not fatal: the node still runs and ember's
// own watchdog still destroys it. Refusing to start would turn a bad number
// into an outage.
func armDeadline(log *slog.Logger, cmd *exec.Cmd) func() {
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
		log.Warn("deadline reached, stopping", "seconds", secs)
		if cmd.Process != nil {
			_ = cmd.Process.Signal(syscall.SIGTERM)
		}
	})
	return func() { timer.Stop() }
}
