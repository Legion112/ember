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
	code, err := run()
	if err != nil {
		fmt.Fprintf(os.Stderr, "embernode: %v\n", err)
		os.Exit(1)
	}
	os.Exit(code)
}

func run() (int, error) {
	if err := installHostKey(); err != nil {
		return 0, err
	}
	if err := installAuthorizedKey(); err != nil {
		return 0, err
	}

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

	stopDeadline := armDeadline(cmd)
	defer stopDeadline()

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT)
	defer signal.Stop(sigs)

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	select {
	case sig := <-sigs:
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
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
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
func armDeadline(cmd *exec.Cmd) func() {
	raw := os.Getenv(envTTL)
	if raw == "" {
		return func() {}
	}
	secs, err := strconv.Atoi(raw)
	if err != nil || secs <= 0 {
		fmt.Fprintf(os.Stderr, "embernode: ignoring unusable %s=%q\n", envTTL, raw)
		return func() {}
	}

	timer := time.AfterFunc(time.Duration(secs)*time.Second, func() {
		fmt.Fprintf(os.Stderr, "embernode: deadline reached after %ds, stopping\n", secs)
		if cmd.Process != nil {
			_ = cmd.Process.Signal(syscall.SIGTERM)
		}
	})
	return func() { timer.Stop() }
}
