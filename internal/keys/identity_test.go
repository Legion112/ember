package keys

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
)

func TestEnsureIdentity_GeneratesOnceAndReloads(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "ember")

	first, err := EnsureIdentity(dir)
	if err != nil {
		t.Fatal(err)
	}
	if first.Path == "" || first.AuthorizedKey == "" {
		t.Fatalf("incomplete identity: %+v", first)
	}

	second, err := EnsureIdentity(dir)
	if err != nil {
		t.Fatal(err)
	}
	if first.Fingerprint() != second.Fingerprint() {
		t.Fatal("EnsureIdentity generated a second key instead of reloading the first; every node authorised with the old one would stop being reachable")
	}
}

// TestEnsureIdentity_WritesRestrictivePermissions. sshd refuses a key readable
// by anyone else, and so does ember — but only if it was written that way to
// begin with.
func TestEnsureIdentity_WritesRestrictivePermissions(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "ember")
	id, err := EnsureIdentity(dir)
	if err != nil {
		t.Fatal(err)
	}

	fi, err := os.Stat(id.Path)
	if err != nil {
		t.Fatal(err)
	}
	if m := fi.Mode().Perm(); m != 0o600 {
		t.Errorf("key mode is %04o, want 0600", m)
	}
	di, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if m := di.Mode().Perm(); m != 0o700 {
		t.Errorf("config dir mode is %04o, want 0700", m)
	}
}

// TestEnsureIdentity_TightensALooseExistingDirectory: MkdirAll leaves an
// existing directory's mode alone, so a config directory created earlier by
// something more permissive would keep that mode and the key inside it would
// be readable.
func TestEnsureIdentity_TightensALooseExistingDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "ember")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := EnsureIdentity(dir); err != nil {
		t.Fatal(err)
	}
	di, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if m := di.Mode().Perm(); m != 0o700 {
		t.Errorf("an existing loose directory was left at %04o", m)
	}
}

func TestLoadSigner_RefusesAGroupOrWorldReadableKey(t *testing.T) {
	dir := t.TempDir()
	id, err := EnsureIdentity(dir)
	if err != nil {
		t.Fatal(err)
	}

	for _, mode := range []os.FileMode{0o640, 0o644, 0o604, 0o660} {
		if err := os.Chmod(id.Path, mode); err != nil {
			t.Fatal(err)
		}
		_, err := LoadSigner(id.Path)
		if err == nil {
			t.Errorf("mode %04o was accepted", mode)
			continue
		}
		if !strings.Contains(err.Error(), "chmod 600") {
			t.Errorf("mode %04o: error should say how to fix it, got %v", mode, err)
		}
	}
}

// TestLoadSigner_RefusesAWorldWritableParent: when a directory is
// world-writable the mode on the file inside it proves nothing, because anyone
// can replace the file with their own key and ember would authenticate to the
// operator's nodes with it.
func TestLoadSigner_RefusesAWorldWritableParent(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "loose")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	id, err := EnsureIdentity(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o777); err != nil {
		t.Fatal(err)
	}
	_, err = LoadSigner(id.Path)
	if err == nil {
		t.Fatal("a world-writable parent directory was accepted")
	}
	if !strings.Contains(err.Error(), "world-writable") {
		t.Errorf("error should name the problem, got %v", err)
	}
}

// TestLoadSigner_RefusesAnEncryptedKeyAndPointsAtTheAgent. ember never
// decrypts: the passphrase and the decrypted key then never enter this
// program's address space.
func TestLoadSigner_RefusesAnEncryptedKeyAndPointsAtTheAgent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "enc")
	if err := os.WriteFile(path, encryptedKey(t), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := LoadSigner(path)
	if err == nil {
		t.Fatal("an encrypted key was accepted")
	}
	if !strings.Contains(err.Error(), "ssh-add") || !strings.Contains(err.Error(), AgentEnv) {
		t.Errorf("error should point at the agent, got %v", err)
	}
}

func TestLoadSigner_ReportsMissingAndMalformed(t *testing.T) {
	dir := t.TempDir()

	if _, err := LoadSigner(filepath.Join(dir, "nope")); err == nil {
		t.Error("a missing key was accepted")
	}

	bad := filepath.Join(dir, "bad")
	if err := os.WriteFile(bad, []byte("not a key"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadSigner(bad); err == nil {
		t.Error("a malformed key was accepted")
	}
}

func TestEnsureIdentity_ReportsAnUnusableDirectory(t *testing.T) {
	// A path whose parent is a regular file cannot become a directory.
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := EnsureIdentity(filepath.Join(file, "ember")); err == nil {
		t.Fatal("EnsureIdentity succeeded under a regular file")
	}
}

func TestAuthorizedKey_IsOneLineWithAComment(t *testing.T) {
	id, err := EnsureIdentity(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	line := id.AuthorizedKey
	if strings.Count(line, "\n") != 0 {
		t.Errorf("authorized key spans lines: %q", line)
	}
	if !strings.HasPrefix(line, "ssh-ed25519 ") || !strings.HasSuffix(line, " ember") {
		t.Errorf("unexpected shape: %q", line)
	}
	// It must be parseable by the thing that will actually consume it.
	pub, _, _, _, err := ssh.ParseAuthorizedKey([]byte(line))
	if err != nil {
		t.Fatalf("sshd would reject this line: %v", err)
	}
	if ssh.FingerprintSHA256(pub) != id.Fingerprint() {
		t.Error("the authorized key is not the identity's own public key")
	}
}

// encryptedKey builds a passphrase-protected private key, so the refusal path
// is exercised against a real one rather than a committed fixture that would
// have to be regenerated whenever the format moves.
func encryptedKey(t *testing.T) []byte {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	block, err := ssh.MarshalPrivateKeyWithPassphrase(priv, "test", []byte("hunter2"))
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(block)
}

func TestLoadSigner_ReportsAMissingDirectory(t *testing.T) {
	_, err := LoadSigner(filepath.Join(t.TempDir(), "absent", "key"))
	if err == nil {
		t.Fatal("a key under a missing directory was accepted")
	}
	if !strings.Contains(err.Error(), "absent") {
		t.Errorf("error should name the directory, got %v", err)
	}
}

func TestLoadIdentity_PropagatesLoadSignerFailures(t *testing.T) {
	bad := filepath.Join(t.TempDir(), "bad")
	if err := os.WriteFile(bad, []byte("not a key"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadIdentity(bad); err == nil {
		t.Fatal("LoadIdentity accepted a malformed key")
	}
}

func TestEnsureIdentity_ReportsAnUndirectoryableParent(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions, so this cannot be provoked")
	}
	parent := filepath.Join(t.TempDir(), "ro")
	if err := os.Mkdir(parent, 0o500); err != nil {
		t.Fatal(err)
	}
	if _, err := EnsureIdentity(filepath.Join(parent, "ember")); err == nil {
		t.Fatal("EnsureIdentity succeeded under a read-only parent")
	}
}
