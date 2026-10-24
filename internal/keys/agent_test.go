package keys

import (
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
)

// TestAgentSigners_NoAgentIsNotAnError: most runs have no agent, and ember
// only needs one when the operator's key is passphrase-protected.
func TestAgentSigners_NoAgentIsNotAnError(t *testing.T) {
	signers, err := agentSigners("", failDial(t))
	if err != nil {
		t.Fatalf("an unset %s should not be an error: %v", AgentEnv, err)
	}
	if len(signers) != 0 {
		t.Errorf("got %d signers with no agent", len(signers))
	}
}

func TestAgentSigners_ReturnsWhatTheAgentHolds(t *testing.T) {
	want, dial := inProcessAgent(t, 2)

	signers, err := agentSigners("ignored", dial)
	if err != nil {
		t.Fatal(err)
	}
	if len(signers) != len(want) {
		t.Fatalf("got %d signers, want %d", len(signers), len(want))
	}

	got := map[string]bool{}
	for _, s := range signers {
		got[ssh.FingerprintSHA256(s.PublicKey())] = true
	}
	for _, fp := range want {
		if !got[fp] {
			t.Errorf("the agent held %s but AgentSigners did not return it", fp)
		}
	}
}

func TestAgentSigners_ReportsAnUnreachableAgent(t *testing.T) {
	_, err := agentSigners("/nonexistent/agent.sock", func(string) (net.Conn, error) {
		return nil, errors.New("no such socket")
	})
	if err == nil {
		t.Fatal("an unreachable agent was not reported")
	}
	// The socket path belongs in the message: SSH_AUTH_SOCK pointing at a
	// stale socket from a dead session is the ordinary cause.
	if !strings.Contains(err.Error(), "/nonexistent/agent.sock") {
		t.Errorf("error should name the socket, got %v", err)
	}
}

func TestAgentSigners_ReportsAProtocolFailure(t *testing.T) {
	// A socket that accepts and then says nothing useful: a stale socket
	// belonging to something that is not an agent at all.
	_, err := agentSigners("weird", func(string) (net.Conn, error) {
		client, server := net.Pipe()
		go func() { _ = server.Close() }()
		return client, nil
	})
	if err == nil {
		t.Fatal("a non-agent on the socket was not reported")
	}
}

func TestAgentSigners_UsesTheEnvironment(t *testing.T) {
	t.Setenv(AgentEnv, "")
	signers, err := AgentSigners()
	if err != nil || len(signers) != 0 {
		t.Fatalf("AgentSigners with no agent = %v, %v", signers, err)
	}
}

// inProcessAgent starts a real ssh-agent in this process holding n keys, and
// returns their fingerprints plus a Dialer reaching it.
//
// Testing against the machine's own agent would be testing whatever the
// developer happens to have loaded, and on CI there is no agent at all.
func inProcessAgent(t *testing.T, n int) ([]string, Dialer) {
	t.Helper()
	keyring := agent.NewKeyring()

	var fps []string
	for range n {
		_, priv, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		if err := keyring.Add(agent.AddedKey{PrivateKey: priv, Comment: "test"}); err != nil {
			t.Fatal(err)
		}
		signer, err := ssh.NewSignerFromKey(priv)
		if err != nil {
			t.Fatal(err)
		}
		fps = append(fps, ssh.FingerprintSHA256(signer.PublicKey()))
	}

	return fps, func(string) (net.Conn, error) {
		client, server := net.Pipe()
		go func() { _ = agent.ServeAgent(keyring, server) }()
		return client, nil
	}
}

func failDial(t *testing.T) Dialer {
	t.Helper()
	return func(addr string) (net.Conn, error) {
		t.Errorf("dialled %q when no agent was configured", addr)
		return nil, errors.New("should not be called")
	}
}

// TestNetDial_ReachesARealUnixSocket covers the production Dialer. It is one
// line of code and the one line every other test in this file replaces, so
// without this nothing would notice it being changed to "tcp".
func TestNetDial_ReachesARealUnixSocket(t *testing.T) {
	// Unix socket paths are limited to about 100 bytes, which t.TempDir() can
	// exceed on some CI runners; os.MkdirTemp in the system temp dir stays short.
	dir, err := os.MkdirTemp("", "ag")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	sock := filepath.Join(dir, "s")
	ln, err := net.Listen("unix", sock) //nolint:gosec // a unix socket in a private temp dir, not a network listener
	if err != nil {
		t.Skipf("unix sockets unavailable: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	keyring := agent.NewKeyring()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if err := keyring.Add(agent.AddedKey{PrivateKey: priv, Comment: "t"}); err != nil {
		t.Fatal(err)
	}
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		_ = agent.ServeAgent(keyring, c)
	}()

	signers, err := agentSigners(sock, netDial)
	if err != nil {
		t.Fatal(err)
	}
	if len(signers) != 1 {
		t.Fatalf("got %d signers over a real socket, want 1", len(signers))
	}
}
