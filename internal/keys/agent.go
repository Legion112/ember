package keys

import (
	"fmt"
	"net"
	"os"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
)

// AgentEnv is the variable every SSH agent sets.
const AgentEnv = "SSH_AUTH_SOCK"

// Dialer opens the agent socket. It is a seam so the agent path can be tested
// against an in-process agent rather than whatever happens to be running on
// the machine under test — which on a developer laptop is a real agent holding
// real keys, and on CI is nothing at all.
type Dialer func(addr string) (net.Conn, error)

// netDial is the production Dialer.
func netDial(addr string) (net.Conn, error) {
	// "unix" is a literal, so addr is a filesystem path and never a host:
	// there is no request to forge. gosec's taint analysis cannot see the
	// network is fixed.
	return net.Dial("unix", addr) //nolint:gosec // unix domain socket, not a network address
}

// AgentSigners returns the signers an SSH agent is holding.
//
// This is ember's answer to an encrypted private key: rather than prompting
// for a passphrase and decrypting in-process, hand the problem to the agent,
// which already solves it and never gives up the key material.
//
// No agent is not an error — most runs do not need one — so the caller gets an
// empty slice and decides.
func AgentSigners() ([]ssh.Signer, error) { return agentSigners(os.Getenv(AgentEnv), netDial) }

func agentSigners(sock string, dial Dialer) ([]ssh.Signer, error) {
	if sock == "" {
		return nil, nil
	}
	conn, err := dial(sock)
	if err != nil {
		return nil, fmt.Errorf("connect to the ssh agent at %s: %w", sock, err)
	}
	// The connection is deliberately not closed here: the signers returned by
	// agent.NewClient talk to the agent every time they sign, so closing it
	// would hand the caller signers that fail on first use. It is released
	// when the process exits, which for a CLI is the end of the run.
	signers, err := agent.NewClient(conn).Signers()
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("list keys from the ssh agent: %w", err)
	}
	return signers, nil
}
