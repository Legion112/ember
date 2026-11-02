package main

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

// TestKeyAttrs_LogAFingerprintAndNeverTheKey.
//
// THIS IS THE SEAL ON THE HAZARD THIS CHANGE CREATES. Making --log-level
// debug reachable makes a secret-leaking log line reachable, and this program
// is the worst place for one: it holds a private key in a plain string,
// because it reads it out of its own environment, and the file it now writes
// is copied onto the operator's disk and kept.
//
// internal/logx's walk refuses `.Bytes()` inside a log call, which is the
// spelling that defeats node.Secret. It cannot refuse `os.Getenv(envHostKey)`
// passed straight to a log call -- the shorter spelling, and the one somebody
// reaching for "just print it while I debug this" would write. That is what
// this test refuses, which is why it asserts on the OUTPUT rather than on the
// type: a value's type is what the walk can see, and what has to be true is
// about the bytes that reach the file.
//
// The planted key is a real PEM header rather than a word, because what makes
// this dangerous is the whole block and a test on a short string would pass
// against a line that printed only the first ten characters of one.
func TestKeyAttrs_LogAFingerprintAndNeverTheKey(t *testing.T) {
	const pem = "-----BEGIN OPENSSH PRIVATE KEY-----\n" +
		"b3BlbnNzaC1rZXktdjEAAAAABG5vbmUAAAAEbm9uZQAAAAAAAAABAAAAMwAAAAtz\n" +
		"-----END OPENSSH PRIVATE KEY-----\n"
	const authorized = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIB0h7mHLg9D5 ember"

	t.Setenv(envHostKey, pem)
	t.Setenv(envAuthorized, authorized)

	var b bytes.Buffer
	logKeys(bufLogger(&b))
	out := b.String()

	for _, leaked := range []string{"PRIVATE KEY", "b3BlbnNzaC1rZXktdjEA", authorized} {
		if strings.Contains(out, leaked) {
			t.Errorf("the node's log carries key material (%q):\n%s", leaked, out)
		}
	}
	// And it says something, because a line that logged nothing would satisfy
	// every assertion above: two fingerprints, which is what makes "the node
	// installed the key ember injected" answerable after the node is gone.
	if n := strings.Count(out, "SHA256:"); n != 2 {
		t.Errorf("want a fingerprint for each key, got %d:\n%s", n, out)
	}
}

// bufLogger builds a logger over a buffer.
//
// Built here rather than taken from internal/logx, because this test is about
// what reaches a writer and the handler is not the subject. A _test.go file is
// exempt from the one-construction-site walk for exactly this reason.
func bufLogger(w *bytes.Buffer) *slog.Logger {
	return slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: slog.LevelInfo}))
}
