package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/Legion112/ember/internal/node"
)

// CredentialsFile is where tokens live when they are not in the environment.
const CredentialsFile = "credentials.yaml"

// EnvRunPod and EnvVast are the environment variables ember reads.
const (
	EnvRunPod = "RUNPOD_API_KEY"
	EnvVast   = "VAST_API_KEY"
)

// Credentials are the provider tokens for this run.
//
// Kept out of ember.yaml, which is meant to be committed. The tokens are
// node.Secret so that a stray slog.Any or %v prints a redaction rather than a
// key that can rent GPUs on somebody's card.
type Credentials struct {
	RunPod node.Secret
	Vast   node.Secret
}

// credentialsFile is the on-disk shape.
type credentialsFile struct {
	RunPod string `yaml:"runpod_api_key"`
	Vast   string `yaml:"vast_api_key"`
}

// LoadCredentials reads tokens from the environment, then from dir.
//
// The environment wins, so a one-off run can use a different account without
// editing a file, and CI can supply a token with nothing on disk at all.
//
// A missing file is not an error: the local provider needs no credential, and
// refusing to start without one would make `ember up --provider local` depend
// on a cloud account it never contacts.
func LoadCredentials(dir string) (Credentials, error) {
	var c Credentials

	path := filepath.Join(dir, CredentialsFile)

	// Stat first, so the permission check is the gate rather than a second
	// look at a file already in memory. The earlier spelling read the file
	// and then stat-ed it, which meant the stat could not fail in any
	// reachable way -- a branch that existed only to be uncovered.
	fi, err := os.Stat(path)
	switch {
	case err == nil:
		if m := fi.Mode().Perm(); m&0o077 != 0 {
			return Credentials{}, fmt.Errorf("%s is mode %04o; it holds API tokens that can rent GPUs, so ember refuses to read it until it is 0600:\n  chmod 600 %s", path, m, path)
		}
		raw, err := os.ReadFile(path) //nolint:gosec // the operator's own config dir, whose mode was just checked
		if err != nil {
			return Credentials{}, fmt.Errorf("read %s: %w", path, err)
		}
		var f credentialsFile
		if err := yaml.Unmarshal(raw, &f); err != nil {
			return Credentials{}, fmt.Errorf("parse %s: %w", path, err)
		}
		c.RunPod = secretOrZero(f.RunPod)
		c.Vast = secretOrZero(f.Vast)
	case os.IsNotExist(err):
		// Fine. Nothing requires a credentials file.
	default:
		return Credentials{}, fmt.Errorf("stat %s: %w", path, err)
	}

	// The environment last, so it overrides the file.
	if v := strings.TrimSpace(os.Getenv(EnvRunPod)); v != "" {
		c.RunPod = node.NewSecret([]byte(v))
	}
	if v := strings.TrimSpace(os.Getenv(EnvVast)); v != "" {
		c.Vast = node.NewSecret([]byte(v))
	}
	return c, nil
}

func secretOrZero(s string) node.Secret {
	s = strings.TrimSpace(s)
	if s == "" {
		return node.Secret{}
	}
	return node.NewSecret([]byte(s))
}
