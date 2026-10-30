package awssig

import (
	"encoding/hex"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/Legion112/ember/internal/node"
)

// The credentials AWS uses throughout its own documentation and test suite.
// They are published examples, not anybody's keys.
const (
	exampleAccess = "AKIDEXAMPLE"
	exampleSecret = "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY"
)

func creds() Credentials {
	return Credentials{AccessKey: exampleAccess, Secret: node.NewSecret([]byte(exampleSecret))}
}

// TestSigningKey_MatchesAWSPublishedDerivation.
//
// From AWS's "deriving a signing key" worked example. This is the step with
// no visible output of its own -- a wrong signing key and a wrong string to
// sign both produce "403" -- so it is pinned against a published value rather
// than against whatever this code happens to compute.
func TestSigningKey_MatchesAWSPublishedDerivation(t *testing.T) {
	got := hex.EncodeToString(SigningKey([]byte(exampleSecret), "20150830", "us-east-1", "iam"))
	const want = "c4afb1cc5771d871763a393e44b703571b55cc28424d1a5e86da6ed3c154a4b9"
	if got != want {
		t.Errorf("signing key = %s\nwant          %s", got, want)
	}
}

// vanilla builds the request from AWS's "get-vanilla" test case.
func vanilla(t *testing.T) *http.Request {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, "https://example.amazonaws.com/", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Amz-Date", "20150830T123600Z")
	return req
}

func TestCanonicalRequest_MatchesGetVanilla(t *testing.T) {
	canonical, signed := CanonicalRequest(vanilla(t), EmptyPayloadHash)

	want := strings.Join([]string{
		"GET",
		"/",
		"",
		"host:example.amazonaws.com",
		"x-amz-date:20150830T123600Z",
		"",
		"host;x-amz-date",
		EmptyPayloadHash,
	}, "\n")
	if canonical != want {
		t.Errorf("canonical request:\n%q\nwant:\n%q", canonical, want)
	}
	if signed != "host;x-amz-date" {
		t.Errorf("signed headers = %q", signed)
	}
}

func TestStringToSign_MatchesGetVanilla(t *testing.T) {
	canonical, _ := CanonicalRequest(vanilla(t), EmptyPayloadHash)
	got := StringToSign("20150830T123600Z", "20150830/us-east-1/service/aws4_request", canonical)

	want := strings.Join([]string{
		"AWS4-HMAC-SHA256",
		"20150830T123600Z",
		"20150830/us-east-1/service/aws4_request",
		"bb579772317eb040ac9ed261061d46c1f17a8133879d6129b6e1c25292927e63",
	}, "\n")
	if got != want {
		t.Errorf("string to sign:\n%q\nwant:\n%q", got, want)
	}
}

func TestSign_MatchesGetVanilla(t *testing.T) {
	req := vanilla(t)
	when, err := time.Parse("20060102T150405Z", "20150830T123600Z")
	if err != nil {
		t.Fatal(err)
	}
	if err := Sign(req, creds(), "us-east-1", "service", EmptyPayloadHash, when); err != nil {
		t.Fatalf("Sign = %v", err)
	}

	const want = "AWS4-HMAC-SHA256 " +
		"Credential=AKIDEXAMPLE/20150830/us-east-1/service/aws4_request, " +
		"SignedHeaders=host;x-amz-date, " +
		"Signature=5fa00fa31553b73ebf1942676e86291e8372ff2a2260956d9b8aae1d763fbf31"
	if got := req.Header.Get("Authorization"); got != want {
		t.Errorf("Authorization:\n%s\nwant:\n%s", got, want)
	}
}

func TestEncodePath(t *testing.T) {
	// The classic SigV4 failure: every key without a space signs correctly,
	// and the first key with one returns 403. url.QueryEscape writes a space
	// as "+", which signs as a literal plus.
	for _, tc := range []struct{ in, want string }{
		{"/", "/"},
		{"", "/"},
		{"/models/sd15.gguf", "/models/sd15.gguf"},
		{"/a%20b", "/a%20b"},
		{"/wan-2.2/high_noise~v1", "/wan-2.2/high_noise~v1"},
		{"/a+b", "/a%2Bb"},
		{"/a:b", "/a%3Ab"},
		{"/%E6%A8%A1%E5%9E%8B", "/%E6%A8%A1%E5%9E%8B"},
	} {
		t.Run(tc.in, func(t *testing.T) {
			if got := EncodePath(tc.in); got != tc.want {
				t.Errorf("EncodePath(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestEncodePath_UsesUppercaseHex(t *testing.T) {
	// Lowercase hex is a different string and therefore a different
	// signature. The server computes uppercase.
	if got := EncodePath("/a b"); !strings.Contains(got, "%20") {
		t.Errorf("EncodePath = %q", got)
	}
	if got := EncodePath("/\x1f"); got != "/%1F" {
		t.Errorf("EncodePath = %q, want uppercase hex", got)
	}
}

func TestEncodeQuery_SortsAndEncodes(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   url.Values
		want string
	}{
		{"empty", url.Values{}, ""},
		{"nil", nil, ""},
		{"one", url.Values{"a": {"1"}}, "a=1"},
		// Sorted by name, which is what "canonical" means here.
		{"sorted by name", url.Values{"b": {"2"}, "a": {"1"}}, "a=1&b=2"},
		// Then by value, for a name that repeats.
		{"sorted by value", url.Values{"a": {"2", "1"}}, "a=1&a=2"},
		{"encoded", url.Values{"key": {"a b"}}, "key=a%20b"},
		// A slash is a separator in a path and a value in a query.
		{"slash in a value", url.Values{"prefix": {"a/b"}}, "prefix=a%2Fb"},
		{"empty value", url.Values{"uploads": {""}}, "uploads="},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := EncodeQuery(tc.in); got != tc.want {
				t.Errorf("EncodeQuery = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestCanonicalRequest_CollapsesHeaderWhitespace(t *testing.T) {
	// Required by the specification, and what a proxy may do to a header in
	// flight. A signer that kept the original spacing signs something the
	// server will not reproduce.
	req := vanilla(t)
	req.Header.Set("X-Amz-Meta-Note", "  two   spaces  ")
	canonical, _ := CanonicalRequest(req, EmptyPayloadHash)
	if !strings.Contains(canonical, "x-amz-meta-note:two spaces\n") {
		t.Errorf("canonical request did not collapse whitespace:\n%q", canonical)
	}
}

func TestCanonicalRequest_NeverSignsAuthorization(t *testing.T) {
	// It cannot sign itself, and a retry that re-signed a request would
	// otherwise include the previous signature in the new one.
	req := vanilla(t)
	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 stale")
	canonical, signed := CanonicalRequest(req, EmptyPayloadHash)
	if strings.Contains(signed, "authorization") || strings.Contains(canonical, "stale") {
		t.Errorf("Authorization was signed:\n%q", canonical)
	}
}

func TestSign_IsSafeToRepeat(t *testing.T) {
	// Signing twice must give the same answer rather than compounding, since
	// a retry re-signs with a fresh timestamp through the same path.
	when := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	a, b := vanilla(t), vanilla(t)
	for _, r := range []*http.Request{a, b} {
		if err := Sign(r, creds(), "EU-RO-1", "s3", EmptyPayloadHash, when); err != nil {
			t.Fatal(err)
		}
	}
	if a.Header.Get("Authorization") != b.Header.Get("Authorization") {
		t.Error("two identical requests signed differently")
	}

	if err := Sign(a, creds(), "EU-RO-1", "s3", EmptyPayloadHash, when); err != nil {
		t.Fatal(err)
	}
	if a.Header.Get("Authorization") != b.Header.Get("Authorization") {
		t.Error("re-signing a request changed the signature")
	}
}

func TestSign_Refusals(t *testing.T) {
	when := time.Now()
	t.Run("no credentials", func(t *testing.T) {
		err := Sign(vanilla(t), Credentials{}, "r", "s3", EmptyPayloadHash, when)
		if err == nil {
			t.Fatal("signed with no credentials")
		}
		// The message names where they come from, because the usual cause is
		// that the key was never created.
		if !strings.Contains(err.Error(), EnvAccessKey) {
			t.Errorf("err = %q", err)
		}
	})
	t.Run("no secret", func(t *testing.T) {
		c := Credentials{AccessKey: exampleAccess}
		if err := Sign(vanilla(t), c, "r", "s3", EmptyPayloadHash, when); err == nil {
			t.Error("signed with an access key and no secret")
		}
	})
	t.Run("no payload hash", func(t *testing.T) {
		err := Sign(vanilla(t), creds(), "r", "s3", "", when)
		if err == nil {
			t.Fatal("signed with no payload hash")
		}
		if !strings.Contains(err.Error(), EmptyPayloadHash) {
			t.Errorf("err = %q, want it to name the digest of an empty body", err)
		}
	})
	t.Run("no url", func(t *testing.T) {
		if err := Sign(&http.Request{Header: http.Header{}}, creds(), "r", "s3", EmptyPayloadHash, when); err == nil {
			t.Error("signed a request with no URL")
		}
	})
}

func TestCredentials_SecretIsRedacted(t *testing.T) {
	// A stray %v in an error must print a fingerprint and not the key. The
	// secret is shown once by RunPod and is the only copy.
	c := creds()
	if s := fmt.Sprintf("%v", c); strings.Contains(s, exampleSecret) {
		t.Errorf("the secret printed in full: %s", s)
	}
}

func TestCanonicalRequest_TakesHostFromTheURLWhenNotSet(t *testing.T) {
	// Go keeps the host on the request rather than in Header, and a request
	// built by hand may not set it at all. A signer that read only the map
	// would omit the one header S3 always signs.
	req := &http.Request{
		Method: http.MethodGet,
		URL:    &url.URL{Scheme: "https", Host: "s3api-eu-ro-1.runpod.io", Path: "/vol/key"},
		Header: http.Header{},
	}
	canonical, signed := CanonicalRequest(req, EmptyPayloadHash)
	if !strings.Contains(canonical, "host:s3api-eu-ro-1.runpod.io\n") {
		t.Errorf("host was not signed:\n%q", canonical)
	}
	if !strings.Contains(signed, "host") {
		t.Errorf("signed headers = %q", signed)
	}
}
