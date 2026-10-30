// Package awssig signs HTTP requests with AWS Signature Version 4.
//
// It exists because RunPod's S3-compatible endpoint speaks SigV4 and nothing
// else, and because the alternative is the AWS SDK -- which would be the
// fifth dependency in this module, for one signing algorithm whose whole
// specification is four hashes and a sorted list.
//
// Every step is a pure function of its inputs, which is the only reason this
// is testable at all: a signature is correct or it is a 403 with no detail,
// and "the server said no" cannot tell you which of the four steps was wrong.
// The pieces are therefore checked individually against AWS's own published
// values rather than end to end against a server.
package awssig

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/Legion112/ember/internal/node"
)

// Algorithm names the scheme, and appears verbatim in the string to sign.
const Algorithm = "AWS4-HMAC-SHA256"

// Terminator ends every credential scope.
const Terminator = "aws4_request"

// EmptyPayloadHash is SHA-256 of nothing, which a GET or DELETE signs.
const EmptyPayloadHash = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

// Credentials are an S3 key pair.
//
// RunPod issues these separately from the API key, on a different page of the
// console, and the secret is shown exactly once. Secret is a node.Secret so a
// stray %v in an error prints a fingerprint rather than the key -- the same
// reason the SSH private half is one.
type Credentials struct {
	AccessKey string
	Secret    node.Secret
}

// IsZero reports whether these name nothing.
func (c Credentials) IsZero() bool { return c.AccessKey == "" || c.Secret.IsZero() }

// Sign adds the Authorization header to req, in place.
//
// payloadHash is the hex SHA-256 of the body and is required rather than
// computed here: the body may be a file far too large to buffer, and the
// caller has to read it once anyway to know its digest. Passing it in is what
// lets one read serve both the signature and the asset manifest's checksum.
func Sign(req *http.Request, c Credentials, region, service, payloadHash string, now time.Time) error {
	if c.IsZero() {
		return fmt.Errorf("no S3 credentials: set runpod_s3_access_key and runpod_s3_secret, "+
			"or %s and %s", EnvAccessKey, EnvSecret)
	}
	if req.URL == nil {
		return fmt.Errorf("request has no URL")
	}
	if payloadHash == "" {
		return fmt.Errorf("payload hash is empty: sign the digest of the body, or %s for no body", EmptyPayloadHash)
	}

	stamp := now.UTC().Format("20060102T150405Z")
	day := stamp[:8]

	// Set before the canonical request is built, because it is part of what
	// gets signed. host is not in req.Header -- Go keeps it on the request --
	// so it is added explicitly in canonicalHeaders.
	//
	// X-Amz-Content-Sha256 is deliberately *not* set here. S3 requires it and
	// the S3 client sets it, but it is not part of SigV4: signing it here
	// would mean this package could never be checked against AWS's own
	// published test vectors, which do not carry that header. A signer whose
	// only test is "the server accepted it" has no way to tell which of the
	// four steps was wrong when the server does not.
	req.Header.Set("X-Amz-Date", stamp)

	canonical, signed := CanonicalRequest(req, payloadHash)
	scope := strings.Join([]string{day, region, service, Terminator}, "/")
	toSign := StringToSign(stamp, scope, canonical)

	key := SigningKey(c.Secret.Bytes(), day, region, service)
	sig := hex.EncodeToString(hmacSHA256(key, []byte(toSign)))

	req.Header.Set("Authorization", fmt.Sprintf("%s Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		Algorithm, c.AccessKey, scope, signed, sig))
	return nil
}

// Environment variables the credentials may come from instead of the file.
//
//nolint:gosec // G101: these are the names of variables, not their values.
const (
	EnvAccessKey = "RUNPOD_S3_ACCESS_KEY"
	EnvSecret    = "RUNPOD_S3_SECRET"
)

// CanonicalRequest renders the request in the form SigV4 hashes, and returns
// the signed-header list alongside it.
//
// The two are returned together because they must describe the same set. A
// SignedHeaders string that names a header the canonical request did not
// include -- or the reverse -- produces a signature the server computes
// differently, and the only symptom is a 403.
func CanonicalRequest(req *http.Request, payloadHash string) (canonical, signedHeaders string) {
	names, headers := canonicalHeaders(req)

	return strings.Join([]string{
		req.Method,
		EncodePath(req.URL.EscapedPath()),
		EncodeQuery(req.URL.Query()),
		headers,
		strings.Join(names, ";"),
		payloadHash,
	}, "\n"), strings.Join(names, ";")
}

// canonicalHeaders returns the sorted header names and the rendered block.
//
// host is included from req.Host or req.URL.Host rather than from the header
// map, because Go stores it on the request and not in Header -- a signer that
// read only the map would omit the one header S3 always signs.
func canonicalHeaders(req *http.Request) ([]string, string) {
	vals := map[string]string{}

	host := req.Host
	if host == "" {
		host = req.URL.Host
	}
	vals["host"] = host

	for k, v := range req.Header {
		name := strings.ToLower(k)
		if name == "authorization" {
			// Never signs itself.
			continue
		}
		// Sequential whitespace collapses and the value is trimmed, which is
		// what the specification requires and what a proxy may do to the
		// header in flight.
		joined := make([]string, 0, len(v))
		for _, one := range v {
			joined = append(joined, strings.Join(strings.Fields(one), " "))
		}
		vals[name] = strings.Join(joined, ",")
	}

	names := make([]string, 0, len(vals))
	for n := range vals {
		names = append(names, n)
	}
	sort.Strings(names)

	var b strings.Builder
	for _, n := range names {
		b.WriteString(n)
		b.WriteByte(':')
		b.WriteString(vals[n])
		b.WriteByte('\n')
	}
	return names, b.String()
}

// StringToSign is the second of the four steps.
func StringToSign(stamp, scope, canonicalRequest string) string {
	sum := sha256.Sum256([]byte(canonicalRequest))
	return strings.Join([]string{
		Algorithm,
		stamp,
		scope,
		hex.EncodeToString(sum[:]),
	}, "\n")
}

// SigningKey derives the key for one day, region and service.
//
// Chained deliberately rather than derived from the secret directly: the
// result is scoped, so a signing key that leaks is useless tomorrow, in
// another region, or against another service.
func SigningKey(secret []byte, day, region, service string) []byte {
	k := hmacSHA256(append([]byte("AWS4"), secret...), []byte(day))
	k = hmacSHA256(k, []byte(region))
	k = hmacSHA256(k, []byte(service))
	return hmacSHA256(k, []byte(Terminator))
}

func hmacSHA256(key, data []byte) []byte {
	m := hmac.New(sha256.New, key)
	m.Write(data)
	return m.Sum(nil)
}

// EncodePath encodes a URI path the way SigV4 requires.
//
// Single-encoded, with the separators left alone. S3 is the service that does
// not double-encode its path, and getting that wrong is the classic SigV4
// failure: every request with a plain key signs correctly and every request
// with a space or a plus in the name returns 403.
func EncodePath(p string) string {
	if p == "" {
		return "/"
	}
	parts := strings.Split(p, "/")
	for i, part := range parts {
		// The path arrives escaped from URL.EscapedPath, so it is unescaped
		// first and then re-encoded by AWS's rules -- which are not Go's.
		if un, err := url.PathUnescape(part); err == nil {
			part = un
		}
		parts[i] = encode(part)
	}
	return strings.Join(parts, "/")
}

// EncodeQuery renders the query string in canonical form: sorted by name,
// then by value, with both encoded.
func EncodeQuery(q url.Values) string {
	if len(q) == 0 {
		return ""
	}
	keys := make([]string, 0, len(q))
	for k := range q {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var pairs []string
	for _, k := range keys {
		vs := append([]string(nil), q[k]...)
		sort.Strings(vs)
		for _, v := range vs {
			pairs = append(pairs, encode(k)+"="+encode(v))
		}
	}
	return strings.Join(pairs, "&")
}

// encode percent-encodes everything outside AWS's unreserved set.
//
// Hand-written because the standard library has no function with these
// rules. url.QueryEscape writes a space as "+", which signs as a literal plus
// and fails; url.PathEscape leaves several characters AWS expects encoded.
// Either produces a signature that is right until a key contains a space.
func encode(s string) string {
	const upperhex = "0123456789ABCDEF"
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9',
			c == '-', c == '_', c == '.', c == '~':
			b.WriteByte(c)
		default:
			b.WriteByte('%')
			b.WriteByte(upperhex[c>>4])
			b.WriteByte(upperhex[c&0x0f])
		}
	}
	return b.String()
}
