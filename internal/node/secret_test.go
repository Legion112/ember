package node

import (
	"log/slog"
	"strings"
	"testing"
)

const fakeKey = "-----BEGIN OPENSSH PRIVATE KEY-----\nb3BlbnNzaC1rZXktdjEAAAAA\n-----END OPENSSH PRIVATE KEY-----\n"

// TestSecret_NeverPrintsItself is the whole purpose of the type. One
// slog.String("hostkey", string(pem)) at debug level writes a private key into
// the journal permanently, and nothing about that line looks wrong in review.
func TestSecret_NeverPrintsItself(t *testing.T) {
	s := NewSecret([]byte(fakeKey))

	for name, got := range map[string]string{
		"String":    s.String(),
		"LogValue":  s.LogValue().String(),
		"Sprintf":   sprintf("%v", s),
		"Sprintf%s": sprintf("%s", s),
	} {
		if strings.Contains(got, "OPENSSH") || strings.Contains(got, "b3Blb") {
			t.Errorf("%s leaked key material: %q", name, got)
		}
		if !strings.HasPrefix(got, "SHA256:") {
			t.Errorf("%s = %q, want a SHA256 fingerprint", name, got)
		}
	}
}

// TestSecret_SurvivesSlogAttr checks the real path: a Secret handed to slog as
// a value, not as a pre-formatted string.
func TestSecret_SurvivesSlogAttr(t *testing.T) {
	var sb strings.Builder
	log := slog.New(slog.NewTextHandler(&sb, nil))
	log.Info("creating node", "hostkey", NewSecret([]byte(fakeKey)))

	out := sb.String()
	if strings.Contains(out, "OPENSSH") {
		t.Fatalf("slog printed the key: %s", out)
	}
	if !strings.Contains(out, "SHA256:") {
		t.Fatalf("slog did not print a fingerprint: %s", out)
	}
}

func TestSecret_FingerprintIsStableAndDistinguishing(t *testing.T) {
	a := NewSecret([]byte("one"))
	b := NewSecret([]byte("two"))
	if a.Fingerprint() != NewSecret([]byte("one")).Fingerprint() {
		t.Error("the same material must fingerprint the same")
	}
	if a.Fingerprint() == b.Fingerprint() {
		t.Error("different material must fingerprint differently")
	}
}

func TestSecret_Empty(t *testing.T) {
	var s Secret
	if !s.IsZero() {
		t.Error("the zero Secret must report IsZero")
	}
	if got := s.Fingerprint(); got != "SHA256:<empty>" {
		t.Errorf("empty Fingerprint() = %q", got)
	}
	if len(s.Bytes()) != 0 {
		t.Error("the zero Secret must carry nothing")
	}
}

func TestSecret_BytesRoundTrips(t *testing.T) {
	s := NewSecret([]byte(fakeKey))
	if string(s.Bytes()) != fakeKey {
		t.Error("Bytes() must return what was wrapped")
	}
	if s.IsZero() {
		t.Error("a populated Secret must not report IsZero")
	}
}
