package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/Legion112/ember/internal/awssig"
	"github.com/Legion112/ember/internal/node"
)

// CredentialsFile is where tokens live when they are not in the environment.
const CredentialsFile = "credentials.yaml"

// EnvRunPod and EnvVast are the environment variables ember reads.
const (
	EnvRunPod = "RUNPOD_API_KEY"
	EnvVast   = "VAST_API_KEY"

	// EnvServeToken is `ember serve`'s credential.
	//
	//nolint:gosec // G101: this is the name of a variable, not its value.
	//
	// AN ENVIRONMENT VARIABLE OR THIS FILE, NEVER A FLAG. /proc/<pid>/cmdline
	// is world-readable, so a token passed as a flag value is readable by
	// every uid on the box for the life of the process -- which is precisely
	// the uid the credential exists to keep out. /proc/<pid>/environ is
	// readable only by the process's own owner, and this file is refused
	// unless it is 0600.
	EnvServeToken = "EMBER_SERVE_TOKEN"
)

// Credentials are the provider tokens for this run.
//
// Kept out of ember.yaml, which is meant to be committed. The tokens are
// node.Secret so that a stray slog.Any or %v prints a redaction rather than a
// key that can rent GPUs on somebody's card.
type Credentials struct {
	RunPod node.Secret
	Vast   node.Secret

	// ServeToken is the credential `ember serve` requires on every request.
	//
	// OPTIONAL HERE AND MANDATORY THERE. `ember serve` mints one per process
	// when this is empty, so there is no unauthenticated configuration to
	// reach -- what setting it buys is a STABLE credential, which is what
	// makes a bookmark survive a restart and what a script against the local
	// API needs. It lives beside the provider tokens because it is the same
	// kind of thing with the same handling: 0600, node.Secret, never argv.
	ServeToken node.Secret

	// S3 is the key pair for RunPod's S3-compatible endpoint, which uploads
	// to a store without renting anything.
	//
	// Separate from RunPod above because they are separate credentials:
	// RunPod issues them on a different page of the console, the secret is
	// shown exactly once, and the API key cannot sign an S3 request. An
	// account can have one and not the other, which is why having no S3 key
	// is not an error until something asks to upload.
	S3 awssig.Credentials
}

// credentialsFile is the on-disk shape.
type credentialsFile struct {
	RunPod      string `yaml:"runpod_api_key"`
	Vast        string `yaml:"vast_api_key"`
	S3AccessKey string `yaml:"runpod_s3_access_key"`
	S3Secret    string `yaml:"runpod_s3_secret"`
	ServeToken  string `yaml:"serve_token"`
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
		c.S3 = awssig.Credentials{
			AccessKey: strings.TrimSpace(f.S3AccessKey),
			Secret:    secretOrZero(f.S3Secret),
		}
		c.ServeToken = secretOrZero(f.ServeToken)
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
	if v := strings.TrimSpace(os.Getenv(EnvServeToken)); v != "" {
		c.ServeToken = node.NewSecret([]byte(v))
	}
	// Both halves move together. Overriding one from the environment and
	// leaving the other from the file would pair an access key with somebody
	// else's secret, and the only symptom is a 403.
	ak := strings.TrimSpace(os.Getenv(awssig.EnvAccessKey))
	sk := strings.TrimSpace(os.Getenv(awssig.EnvSecret))
	if ak != "" || sk != "" {
		c.S3 = awssig.Credentials{AccessKey: ak, Secret: secretOrZero(sk)}
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
