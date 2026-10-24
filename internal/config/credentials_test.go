package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// clearEnv keeps a test from reading the developer's own tokens.
func clearEnv(t *testing.T) {
	t.Helper()
	t.Setenv(EnvRunPod, "")
	t.Setenv(EnvVast, "")
}

func writeCreds(t *testing.T, body string, mode os.FileMode) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, CredentialsFile), []byte(body), mode); err != nil {
		t.Fatal(err)
	}
	return dir
}

// TestLoadCredentials_AMissingFileIsFine. The local provider needs no
// credential, and refusing to start without one would make
// `ember up --provider local` depend on a cloud account it never contacts.
func TestLoadCredentials_AMissingFileIsFine(t *testing.T) {
	clearEnv(t)
	c, err := LoadCredentials(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if !c.RunPod.IsZero() || !c.Vast.IsZero() {
		t.Errorf("tokens appeared out of nowhere: %+v", c)
	}
}

func TestLoadCredentials_ReadsTheFile(t *testing.T) {
	clearEnv(t)
	dir := writeCreds(t, "runpod_api_key: rp-from-file\nvast_api_key: vast-from-file\n", 0o600)

	c, err := LoadCredentials(dir)
	if err != nil {
		t.Fatal(err)
	}
	if string(c.RunPod.Bytes()) != "rp-from-file" {
		t.Errorf("runpod = %q", c.RunPod.Bytes())
	}
	if string(c.Vast.Bytes()) != "vast-from-file" {
		t.Errorf("vast = %q", c.Vast.Bytes())
	}
}

// TestLoadCredentials_TheEnvironmentWins, so a one-off run can use a different
// account without editing a file and CI can supply a token with nothing on
// disk at all.
func TestLoadCredentials_TheEnvironmentWins(t *testing.T) {
	clearEnv(t)
	dir := writeCreds(t, "runpod_api_key: rp-from-file\n", 0o600)
	t.Setenv(EnvRunPod, "rp-from-env")

	c, err := LoadCredentials(dir)
	if err != nil {
		t.Fatal(err)
	}
	if string(c.RunPod.Bytes()) != "rp-from-env" {
		t.Errorf("runpod = %q", c.RunPod.Bytes())
	}
}

func TestLoadCredentials_AnEmptyEnvVarDoesNotOverrideTheFile(t *testing.T) {
	clearEnv(t)
	dir := writeCreds(t, "runpod_api_key: rp-from-file\n", 0o600)
	t.Setenv(EnvRunPod, "   ")

	c, err := LoadCredentials(dir)
	if err != nil {
		t.Fatal(err)
	}
	// An exported-but-blank variable is how a shell profile looks when
	// somebody has commented out their key; it must not silently blank a
	// working credential.
	if string(c.RunPod.Bytes()) != "rp-from-file" {
		t.Errorf("runpod = %q", c.RunPod.Bytes())
	}
}

func TestLoadCredentials_TrimsSurroundingSpace(t *testing.T) {
	clearEnv(t)
	// A token pasted from a browser often carries a trailing newline, and a
	// bearer header with one in it fails with an unhelpful 401.
	dir := writeCreds(t, "runpod_api_key: \"  rp-padded\\n\"\n", 0o600)
	c, err := LoadCredentials(dir)
	if err != nil {
		t.Fatal(err)
	}
	if string(c.RunPod.Bytes()) != "rp-padded" {
		t.Errorf("runpod = %q", c.RunPod.Bytes())
	}
}

// TestLoadCredentials_RefusesAWorldReadableFile. Checked on every load rather
// than assumed from however it was created: a file chmodded later is the
// common case, and a token that rents GPUs is worth as much as the key that
// reaches them.
func TestLoadCredentials_RefusesAWorldReadableFile(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Fatal("this test must not run as root: root ignores file permissions, so it would pass without checking anything")
	}
	clearEnv(t)
	dir := writeCreds(t, "runpod_api_key: rp\n", 0o644)

	_, err := LoadCredentials(dir)
	if err == nil {
		t.Fatal("a world-readable credentials file was read")
	}
	// The message has to carry the fix, or the operator guesses at chmod
	// arguments while a provision is blocked.
	if !strings.Contains(err.Error(), "chmod 600") {
		t.Errorf("the error does not say how to fix it: %v", err)
	}
}

func TestLoadCredentials_RefusesAGroupReadableFile(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Fatal("this test must not run as root")
	}
	clearEnv(t)
	dir := writeCreds(t, "runpod_api_key: rp\n", 0o640)
	if _, err := LoadCredentials(dir); err == nil {
		t.Fatal("a group-readable credentials file was read")
	}
}

func TestLoadCredentials_ReportsMalformedYAML(t *testing.T) {
	clearEnv(t)
	dir := writeCreds(t, "runpod_api_key: [unterminated\n", 0o600)
	if _, err := LoadCredentials(dir); err == nil {
		t.Fatal("a credentials file that does not parse was accepted")
	}
}

// TestLoadCredentials_ReportsAnUnreadablePath. A directory where the file
// should be is not "no credentials": it is a misconfiguration, and silently
// continuing would make every cloud run fail later with a 401.
func TestLoadCredentials_ReportsAnUnreadablePath(t *testing.T) {
	clearEnv(t)
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, CredentialsFile), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadCredentials(dir); err == nil {
		t.Fatal("a directory in place of the credentials file was accepted")
	}
}

// TestCredentials_AreRedactedWhenPrinted. These end up in error messages and
// log lines, and a token that can rent GPUs on somebody's card must not be
// one %v away from a logfile.
func TestCredentials_AreRedactedWhenPrinted(t *testing.T) {
	clearEnv(t)
	dir := writeCreds(t, "runpod_api_key: rp-very-secret\nvast_api_key: vast-very-secret\n", 0o600)
	c, err := LoadCredentials(dir)
	if err != nil {
		t.Fatal(err)
	}

	for _, s := range []string{
		c.RunPod.String(),
		c.Vast.String(),
	} {
		if strings.Contains(s, "secret") {
			t.Errorf("a token printed itself: %q", s)
		}
	}
}

func TestLoadCredentials_TheVastEnvVarIsReadToo(t *testing.T) {
	clearEnv(t)
	t.Setenv(EnvVast, "vast-from-env")

	c, err := LoadCredentials(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if string(c.Vast.Bytes()) != "vast-from-env" {
		t.Errorf("vast = %q", c.Vast.Bytes())
	}
}

// TestLoadCredentials_ReportsAConfigDirThatIsNotADirectory. Distinct from
// "there is no credentials file", which is fine: this is a misconfigured
// config_dir, and treating it as "no credentials" would have every cloud run
// fail later with a 401 that blames the token.
func TestLoadCredentials_ReportsAConfigDirThatIsNotADirectory(t *testing.T) {
	clearEnv(t)
	notADir := filepath.Join(t.TempDir(), "config")
	if err := os.WriteFile(notADir, []byte("this is a file"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := LoadCredentials(notADir)
	if err == nil {
		t.Fatal("a config dir that is a file was accepted as having no credentials")
	}
	if !strings.Contains(err.Error(), "stat") {
		t.Errorf("got %v", err)
	}
}
