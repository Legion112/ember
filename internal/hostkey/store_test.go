package hostkey

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func storePath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "state", "pins.json")
}

// TestTOFU_PinsThenRefusesAChange is the test that proves the control works.
// Everything else in this package is structure; this is the behaviour.
func TestTOFU_PinsThenRefusesAChange(t *testing.T) {
	s, err := OpenStore(storePath(t))
	if err != nil {
		t.Fatal(err)
	}
	first, second := pub(t), pub(t)

	if err := Callback(s.TrustOnFirstUse("n1"), "n1")("h", remote, first); err != nil {
		t.Fatalf("the first key should be accepted and recorded: %v", err)
	}
	// Same key again is fine.
	if err := Callback(s.TrustOnFirstUse("n1"), "n1")("h", remote, first); err != nil {
		t.Fatalf("the pinned key was refused on reconnect: %v", err)
	}
	// A different key is not.
	err = Callback(s.TrustOnFirstUse("n1"), "n1")("h", remote, second)
	var mm *MismatchError
	if !errors.As(err, &mm) {
		t.Fatalf("a rotated host key was accepted: %v", err)
	}
}

// TestTOFU_PinsAreKeyedByNodeNotAddress: two nodes reached at the same address
// are two different pins. An address-keyed pin would match whoever is given
// that IP next, who on a provider recycling addresses is a stranger.
func TestTOFU_PinsAreKeyedByNodeNotAddress(t *testing.T) {
	s, err := OpenStore(storePath(t))
	if err != nil {
		t.Fatal(err)
	}
	a, b := pub(t), pub(t)

	if err := Callback(s.TrustOnFirstUse("node-a"), "node-a")("h", remote, a); err != nil {
		t.Fatal(err)
	}
	// Same remote address, different node: must pin independently, not clash.
	if err := Callback(s.TrustOnFirstUse("node-b"), "node-b")("h", remote, b); err != nil {
		t.Fatalf("a second node at the same address was refused: %v", err)
	}
}

func TestStore_SurvivesReload(t *testing.T) {
	path := storePath(t)
	s, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	key := pub(t)
	if err := Callback(s.TrustOnFirstUse("n1"), "n1")("h", remote, key); err != nil {
		t.Fatal(err)
	}

	reopened, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := reopened.Get("n1")
	if !ok {
		t.Fatal("the pin did not survive a reload; trust-on-first-use would silently re-arm")
	}
	if !got.Equal(Of(key)) {
		t.Fatal("the reloaded pin is a different key")
	}
	// And it must still refuse a change after the reload.
	if err := Callback(reopened.TrustOnFirstUse("n1"), "n1")("h", remote, pub(t)); err == nil {
		t.Fatal("the reloaded store accepted a rotated key")
	}
}

func TestStore_WritesRestrictivePermissions(t *testing.T) {
	path := storePath(t)
	s, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := Callback(s.TrustOnFirstUse("n1"), "n1")("h", remote, pub(t)); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if m := fi.Mode().Perm(); m != 0o600 {
		t.Errorf("pin file mode is %04o, want 0600", m)
	}
}

func TestStore_ForgetRemovesThePin(t *testing.T) {
	path := storePath(t)
	s, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := Callback(s.TrustOnFirstUse("n1"), "n1")("h", remote, pub(t)); err != nil {
		t.Fatal(err)
	}
	if err := s.Forget("n1"); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Get("n1"); ok {
		t.Fatal("Forget left the pin behind")
	}
	// Forgetting something absent is not an error: destroy is idempotent and
	// so is the cleanup that follows it.
	if err := s.Forget("never-existed"); err != nil {
		t.Fatalf("Forget on an absent pin: %v", err)
	}
	// After Forget the node is a stranger again.
	if err := Callback(s.TrustOnFirstUse("n1"), "n1")("h", remote, pub(t)); err != nil {
		t.Fatalf("a forgotten node should pin afresh: %v", err)
	}
}

func TestOpenStore_MissingFileIsEmptyNotAnError(t *testing.T) {
	s, err := OpenStore(storePath(t))
	if err != nil {
		t.Fatalf("a missing pin file should just be an empty store: %v", err)
	}
	if _, ok := s.Get("anything"); ok {
		t.Error("an empty store returned a pin")
	}
}

func TestOpenStore_RefusesAFutureVersion(t *testing.T) {
	path := storePath(t)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"version":99,"pins":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := OpenStore(path)
	if err == nil {
		t.Fatal("a newer on-disk version was read as if it were this one; dropping pins re-arms trust-on-first-use silently")
	}
	if !strings.Contains(err.Error(), "newer ember") {
		t.Errorf("got %v", err)
	}
}

func TestOpenStore_RefusesCorruptContent(t *testing.T) {
	for name, body := range map[string]string{
		"not json":        "{{{",
		"bad fingerprint": `{"version":1,"pins":{"n1":"nonsense"}}`,
	} {
		path := storePath(t)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := OpenStore(path); err == nil {
			t.Errorf("%s: OpenStore accepted it", name)
		}
	}
}

func TestOpenStore_ReportsAnUnreadableFile(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores file permissions")
	}
	path := storePath(t)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"version":1,"pins":{}}`), 0o000); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenStore(path); err == nil {
		t.Fatal("an unreadable pin file was treated as empty")
	}
}

func TestStore_ReportsAnUnwritableDirectory(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	parent := filepath.Join(t.TempDir(), "ro")
	if err := os.Mkdir(parent, 0o500); err != nil {
		t.Fatal(err)
	}
	s, err := OpenStore(filepath.Join(parent, "sub", "pins.json"))
	if err != nil {
		t.Fatal(err)
	}
	// Pinning has to fail loudly: a pin that silently fails to persist means
	// the next run trusts a new key on sight.
	if err := Callback(s.TrustOnFirstUse("n1"), "n1")("h", remote, pub(t)); err == nil {
		t.Fatal("a pin that could not be written reported success")
	}
}

func TestStore_ReportsAnUnwritableExistingDirectory(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	dir := filepath.Join(t.TempDir(), "state")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	s, err := OpenStore(filepath.Join(dir, "pins.json"))
	if err != nil {
		t.Fatal(err)
	}
	// The directory exists, so MkdirAll succeeds and the failure lands on
	// creating the temporary file instead — a different branch from
	// TestStore_ReportsAnUnwritableDirectory, and the one a full or
	// read-only state directory actually hits in the field.
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	if err := Callback(s.TrustOnFirstUse("n1"), "n1")("h", remote, pub(t)); err == nil {
		t.Fatal("a pin that could not be written reported success")
	}
}
