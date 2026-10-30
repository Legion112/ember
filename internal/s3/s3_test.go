package s3

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Legion112/ember/internal/awssig"
	"github.com/Legion112/ember/internal/node"
)

// recorder is a stand-in S3 endpoint that remembers every request.
//
// Stateful rather than a canned response, for the reason runpodfake is: what
// breaks a multipart upload is the sequence -- an ETag that never came back,
// a completion that names a part nobody sent, an abort that is not made --
// and a stateless stub cannot express a sequence.
type recorder struct {
	mu sync.Mutex

	calls []string
	parts map[int64][]byte
	body  []byte

	noETag     bool
	failPart   int64
	completeAs string
}

func newRecorder() *recorder { return &recorder{parts: map[int64][]byte{}} }

func (r *recorder) Calls() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.calls...)
}

func (r *recorder) serve(t *testing.T) *Client {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(r.handle))
	t.Cleanup(s.Close)
	return &Client{
		Endpoint: s.URL,
		Region:   "EU-RO-1",
		Creds: awssig.Credentials{
			AccessKey: "user_test",
			Secret:    node.NewSecret([]byte("rps_test_secret")),
		},
		HTTP: s.Client(),
		Now:  func() time.Time { return time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC) },
		// Small enough that the multipart path runs in milliseconds. The
		// arithmetic under test is the same at any size; moving half a
		// gigabyte through an httptest server on every run is not.
		PartSize:       4 << 10,
		SinglePutLimit: 8 << 10,
	}
}

func (r *recorder) handle(w http.ResponseWriter, req *http.Request) {
	r.mu.Lock()
	defer r.mu.Unlock()

	q := req.URL.Query()
	switch {
	case req.Method == http.MethodPost && q.Has("uploads"):
		r.calls = append(r.calls, "create")
		w.Header().Set("Content-Type", "application/xml")
		_, _ = fmt.Fprint(w, `<InitiateMultipartUploadResult><UploadId>up-1</UploadId></InitiateMultipartUploadResult>`)

	case req.Method == http.MethodPut && q.Has("partNumber"):
		n := int64(0)
		_, _ = fmt.Sscanf(q.Get("partNumber"), "%d", &n)
		r.calls = append(r.calls, fmt.Sprintf("part %d", n))
		if n == r.failPart {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = fmt.Fprint(w, `<Error><Code>InternalError</Code></Error>`)
			return
		}
		b, _ := io.ReadAll(req.Body)
		r.parts[n] = b
		if !r.noETag {
			w.Header().Set("ETag", fmt.Sprintf("\"etag-%d\"", n))
		}

	case req.Method == http.MethodPost && q.Has("uploadId"):
		r.calls = append(r.calls, "complete")
		if r.completeAs != "" {
			_, _ = fmt.Fprint(w, r.completeAs)
			return
		}
		// Assembled in part order, which is what S3 does.
		for i := int64(1); ; i++ {
			p, ok := r.parts[i]
			if !ok {
				break
			}
			r.body = append(r.body, p...)
		}
		_, _ = fmt.Fprint(w, `<CompleteMultipartUploadResult><ETag>"final"</ETag></CompleteMultipartUploadResult>`)

	case req.Method == http.MethodDelete && q.Has("uploadId"):
		r.calls = append(r.calls, "abort")
		w.WriteHeader(http.StatusNoContent)

	case req.Method == http.MethodPut:
		r.calls = append(r.calls, "put")
		r.body, _ = io.ReadAll(req.Body)

	default:
		r.calls = append(r.calls, "unexpected "+req.Method+" "+req.URL.RequestURI())
		w.WriteHeader(http.StatusBadRequest)
	}
}

func TestUpload_SmallObjectGoesInOneRequest(t *testing.T) {
	r := newRecorder()
	c := r.serve(t)
	body := []byte("the weights")

	if err := c.Upload(t.Context(), "vol-1", "sd15.gguf", bytes.NewReader(body), int64(len(body)), nil); err != nil {
		t.Fatalf("Upload = %v", err)
	}
	if got := r.Calls(); len(got) != 1 || got[0] != "put" {
		t.Errorf("calls = %v, want one put", got)
	}
	if !bytes.Equal(r.body, body) {
		t.Errorf("uploaded %q", r.body)
	}
}

func TestUpload_EverySignatureIsWellFormed(t *testing.T) {
	// Checked at the server, because a signature is the one thing whose only
	// other feedback is a 403 with no detail.
	var seen []string
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		seen = append(seen, req.Header.Get("Authorization"))
		if req.Header.Get("X-Amz-Content-Sha256") == "" {
			t.Error("a request went out with no X-Amz-Content-Sha256")
		}
		w.Header().Set("ETag", `"e"`)
	}))
	t.Cleanup(s.Close)

	c := &Client{Endpoint: s.URL, Region: "EU-RO-1", HTTP: s.Client(),
		Creds: awssig.Credentials{AccessKey: "user_x", Secret: node.NewSecret([]byte("k"))}}
	if err := c.Upload(t.Context(), "vol-1", "a.gguf", bytes.NewReader([]byte("x")), 1, nil); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 1 {
		t.Fatalf("saw %d requests", len(seen))
	}
	for _, a := range seen {
		for _, want := range []string{awssig.Algorithm, "Credential=user_x/", "/EU-RO-1/s3/aws4_request",
			"SignedHeaders=", "Signature="} {
			if !strings.Contains(a, want) {
				t.Errorf("Authorization %q is missing %q", a, want)
			}
		}
		// The secret must never travel.
		if strings.Contains(a, "k") && strings.Contains(a, "Secret") {
			t.Errorf("the secret appears in the header: %q", a)
		}
	}
}

// bigReader is a file-sized source that costs nothing to hold.
//
// A real 400 MiB buffer in a unit test is a test that cannot run on a small
// machine; this gives the same bytes at the same offsets without the memory.
type bigReader struct{ size int64 }

func (b bigReader) ReadAt(p []byte, off int64) (int, error) {
	n := 0
	for i := range p {
		if off+int64(i) >= b.size {
			return n, io.EOF
		}
		p[i] = byte((off + int64(i)) % 251)
		n++
	}
	return n, nil
}

func TestUpload_LargeObjectGoesMultipart(t *testing.T) {
	r := newRecorder()
	c := r.serve(t)
	// Two parts and a bit, so the last one is short -- the case an
	// off-by-one in the offset arithmetic gets wrong.
	size := c.singleLimit() + c.partSize() + 1024

	if err := c.Upload(t.Context(), "vol-1", "wan.gguf", bigReader{size}, size, nil); err != nil {
		t.Fatalf("Upload = %v", err)
	}

	calls := r.Calls()
	if calls[0] != "create" || calls[len(calls)-1] != "complete" {
		t.Fatalf("calls = %v", calls)
	}
	// Every byte arrived, in order, exactly once.
	if int64(len(r.body)) != size {
		t.Fatalf("assembled %d bytes, want %d", len(r.body), size)
	}
	for i := int64(0); i < size; i += 7919 {
		if want := byte(i % 251); r.body[i] != want {
			t.Fatalf("byte %d = %d, want %d: the parts were assembled wrong", i, r.body[i], want)
		}
	}
	// And the last byte, which is where a short final part shows up.
	if want := byte((size - 1) % 251); r.body[size-1] != want {
		t.Errorf("the final byte is %d, want %d", r.body[size-1], want)
	}
}

func TestUpload_AbortsAnUploadItCannotFinish(t *testing.T) {
	// The leak this prevents. An incomplete multipart upload keeps every
	// part that arrived, and S3-compatible stores bill for them -- without
	// showing them, because an incomplete upload is not an object and does
	// not appear in a listing. Same shape as a leaked pod.
	r := newRecorder()
	r.failPart = 2
	c := r.serve(t)
	size := c.singleLimit() + c.partSize() + 1024

	err := c.Upload(t.Context(), "vol-1", "wan.gguf", bigReader{size}, size, nil)
	if err == nil {
		t.Fatal("a failed part was reported as success")
	}

	var aborted bool
	for _, call := range r.Calls() {
		if call == "abort" {
			aborted = true
		}
		if call == "complete" {
			t.Error("a failed upload was completed anyway")
		}
	}
	if !aborted {
		t.Errorf("the incomplete upload was not aborted: %v", r.Calls())
	}
}

func TestUpload_SaysSoWhenItCannotAbort(t *testing.T) {
	// Both failures, because the second one costs money. An error that
	// mentioned only the upload would leave the operator with parts they do
	// not know about.
	hits := 0
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		hits++
		if req.Method == http.MethodPost && req.URL.Query().Has("uploads") {
			_, _ = fmt.Fprint(w, `<InitiateMultipartUploadResult><UploadId>up-1</UploadId></InitiateMultipartUploadResult>`)
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(s.Close)

	c := &Client{Endpoint: s.URL, Region: "EU-RO-1", HTTP: s.Client(),
		Creds: awssig.Credentials{AccessKey: "u", Secret: node.NewSecret([]byte("k"))}}
	size := c.singleLimit() + c.partSize()

	err := c.Upload(t.Context(), "vol-1", "wan.gguf", bigReader{size}, size, nil)
	if err == nil {
		t.Fatal("expected a failure")
	}
	if !strings.Contains(err.Error(), "still billing") {
		t.Errorf("err = %q, want it to say the parts are still stored", err)
	}
}

func TestUpload_RefusesAPartWithNoETag(t *testing.T) {
	// Without it the completion cannot name the part, and S3 rejects the
	// whole upload after every byte has been sent. Caught at the first part
	// instead.
	r := newRecorder()
	r.noETag = true
	c := r.serve(t)
	size := c.singleLimit() + c.partSize()

	err := c.Upload(t.Context(), "vol-1", "wan.gguf", bigReader{size}, size, nil)
	if err == nil {
		t.Fatal("a part with no ETag was accepted")
	}
	if !strings.Contains(err.Error(), "ETag") {
		t.Errorf("err = %q", err)
	}
}

func TestUpload_RefusesAnErrorInsideA200(t *testing.T) {
	// S3 answers 200 and then reports the failure in the body, because the
	// connection is held open while it assembles the object. A client that
	// trusted the status reports success for an upload that did not finish.
	r := newRecorder()
	r.completeAs = `<Error><Code>InternalError</Code><Message>try again</Message></Error>`
	c := r.serve(t)
	size := c.singleLimit() + c.partSize()

	err := c.Upload(t.Context(), "vol-1", "wan.gguf", bigReader{size}, size, nil)
	if err == nil {
		t.Fatal("an error inside a 200 was read as success")
	}
	if !strings.Contains(err.Error(), "try again") {
		t.Errorf("err = %q, want the endpoint's own message", err)
	}
}

func TestUpload_RefusesTooManyParts(t *testing.T) {
	r := newRecorder()
	c := r.serve(t)
	size := int64(MaxParts+1) * c.partSize()

	err := c.Upload(t.Context(), "vol-1", "huge.gguf", bigReader{0}, size, nil)
	if err == nil {
		t.Fatal("an object above the part ceiling was accepted")
	}
	if !strings.Contains(err.Error(), "ceiling") {
		t.Errorf("err = %q", err)
	}
	if n := len(r.Calls()); n != 0 {
		t.Errorf("made %d calls before refusing", n)
	}
}

func TestStatusError_CarriesTheEndpointsOwnMessage(t *testing.T) {
	// SignatureDoesNotMatch and AccessDenied are both 403. Reporting only the
	// code would hand back the one detail that cannot be guessed.
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = fmt.Fprint(w, `<Error><Code>SignatureDoesNotMatch</Code></Error>`)
	}))
	t.Cleanup(s.Close)

	c := &Client{Endpoint: s.URL, Region: "EU-RO-1", HTTP: s.Client(),
		Creds: awssig.Credentials{AccessKey: "u", Secret: node.NewSecret([]byte("k"))}}
	err := c.Upload(t.Context(), "vol-1", "a.gguf", bytes.NewReader([]byte("x")), 1, nil)
	if err == nil {
		t.Fatal("a 403 was accepted")
	}
	if !strings.Contains(err.Error(), "SignatureDoesNotMatch") {
		t.Errorf("err = %q", err)
	}
	// The query carries the upload id and, on a presigned URL, the
	// signature. It is not repeated into an error.
	if strings.Contains(err.Error(), "?") {
		t.Errorf("err = %q, want the query stripped", err)
	}
}

func TestUpload_Refusals(t *testing.T) {
	r := newRecorder()
	c := r.serve(t)

	if err := c.Upload(t.Context(), "v", "k", bytes.NewReader(nil), -1, nil); err == nil {
		t.Error("a negative size was accepted")
	}

	noEndpoint := &Client{Region: "EU-RO-1",
		Creds: awssig.Credentials{AccessKey: "u", Secret: node.NewSecret([]byte("k"))}}
	if err := noEndpoint.Upload(t.Context(), "v", "k", bytes.NewReader([]byte("x")), 1, nil); err == nil {
		t.Error("an upload with no endpoint was accepted")
	}

	noCreds := &Client{Endpoint: c.Endpoint, Region: "EU-RO-1", HTTP: c.HTTP}
	if err := noCreds.Upload(t.Context(), "v", "k", bytes.NewReader([]byte("x")), 1, nil); err == nil {
		t.Error("an upload with no credentials was accepted")
	}
}

func TestUpload_HonoursCancellation(t *testing.T) {
	r := newRecorder()
	c := r.serve(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := c.Upload(ctx, "v", "k", bytes.NewReader([]byte("x")), 1, nil); err == nil {
		t.Error("a cancelled upload reported success")
	}
}

func TestEndpointFor(t *testing.T) {
	// Lowercased, because a host is case-insensitive but the signature over
	// it is not: "s3api-EU-RO-1" and "s3api-eu-ro-1" are different strings to
	// sign and only one of them matches what the server computes.
	if got, want := EndpointFor("EU-RO-1"), "https://s3api-eu-ro-1.runpod.io"; got != want {
		t.Errorf("EndpointFor = %q, want %q", got, want)
	}
}

func TestHuman(t *testing.T) {
	for _, tc := range []struct {
		in   int64
		want string
	}{
		{512, "512 B"},
		{2 << 20, "2.0 MiB"},
		{3 << 30, "3.0 GiB"},
	} {
		if got := human(tc.in); got != tc.want {
			t.Errorf("human(%d) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestUpload_ReportsAStartThatGoesWrong(t *testing.T) {
	for _, tc := range []struct {
		name    string
		body    string
		status  int
		wantMsg string
	}{
		{"refused", "", http.StatusForbidden, "start upload"},
		{"unreadable xml", "not xml at all", http.StatusOK, "start upload"},
		{"no upload id", `<InitiateMultipartUploadResult></InitiateMultipartUploadResult>`, http.StatusOK, "no upload id"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if tc.status != http.StatusOK {
					w.WriteHeader(tc.status)
				}
				_, _ = fmt.Fprint(w, tc.body)
			}))
			t.Cleanup(s.Close)
			c := &Client{Endpoint: s.URL, Region: "EU-RO-1", HTTP: s.Client(),
				PartSize: 4 << 10, SinglePutLimit: 8 << 10,
				Creds: awssig.Credentials{AccessKey: "u", Secret: node.NewSecret([]byte("k"))}}

			size := c.singleLimit() + c.partSize()
			err := c.Upload(t.Context(), "v", "k.gguf", bigReader{size}, size, nil)
			if err == nil {
				t.Fatal("accepted")
			}
			if !strings.Contains(err.Error(), tc.wantMsg) {
				t.Errorf("err = %q, want it to mention %q", err, tc.wantMsg)
			}
		})
	}
}

func TestUpload_ReportsACompletionThatIsRefused(t *testing.T) {
	r := newRecorder()
	c := r.serve(t)
	// A 403 on the completion call itself, as distinct from an <Error> body
	// inside a 200.
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		q := req.URL.Query()
		switch {
		case req.Method == http.MethodPost && q.Has("uploads"):
			_, _ = fmt.Fprint(w, `<InitiateMultipartUploadResult><UploadId>up-1</UploadId></InitiateMultipartUploadResult>`)
		case req.Method == http.MethodPut:
			w.Header().Set("ETag", `"e"`)
		case req.Method == http.MethodPost:
			w.WriteHeader(http.StatusForbidden)
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	t.Cleanup(s.Close)
	c.Endpoint, c.HTTP = s.URL, s.Client()

	size := c.singleLimit() + c.partSize()
	err := c.Upload(t.Context(), "v", "k.gguf", bigReader{size}, size, nil)
	if err == nil {
		t.Fatal("a refused completion was accepted")
	}
	if !strings.Contains(err.Error(), "finish upload") {
		t.Errorf("err = %q", err)
	}
}

func TestUpload_ReportsAnUnreadableSource(t *testing.T) {
	// A file that cannot be read at an offset. The upload must not send a
	// part of zeroes and call it done.
	r := newRecorder()
	c := r.serve(t)

	small := failingReader{}
	if err := c.Upload(t.Context(), "v", "k", small, 16, nil); err == nil {
		t.Error("an unreadable source was accepted in a single put")
	}

	size := c.singleLimit() + c.partSize()
	if err := c.Upload(t.Context(), "v", "k", failingReader{}, size, nil); err == nil {
		t.Error("an unreadable source was accepted in a multipart upload")
	}
}

// failingReader fails every read, as a file on a disconnected volume would.
type failingReader struct{}

func (failingReader) ReadAt([]byte, int64) (int, error) {
	return 0, fmt.Errorf("input/output error")
}

func TestClient_DefaultsAreUsable(t *testing.T) {
	// The zero client must produce a real HTTP client and real sizes, since
	// production builds one with neither set.
	c := &Client{}
	if c.client() == nil {
		t.Error("no HTTP client")
	}
	if c.partSize() != PartSize || c.singleLimit() != SinglePutLimit {
		t.Errorf("defaults = %d / %d", c.partSize(), c.singleLimit())
	}
	if _, err := c.do(t.Context(), http.MethodGet, "/", nil, nil, 0, awssig.EmptyPayloadHash); err == nil {
		t.Error("a request with no endpoint was sent")
	}
	bad := &Client{Endpoint: "://nope"}
	if _, err := bad.do(t.Context(), http.MethodGet, "/", nil, nil, 0, awssig.EmptyPayloadHash); err == nil {
		t.Error("an unparseable endpoint was accepted")
	}
}

func TestUpload_WritesProgress(t *testing.T) {
	// The operator is watching a 40 GB upload. Silence for twenty minutes is
	// indistinguishable from a hang.
	r := newRecorder()
	c := r.serve(t)
	var out bytes.Buffer
	size := c.singleLimit() + c.partSize()
	if err := c.Upload(t.Context(), "v", "k.gguf", bigReader{size}, size, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "part 1/") {
		t.Errorf("no per-part progress:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "k.gguf") {
		t.Errorf("progress does not name the file:\n%s", out.String())
	}
}

func TestUpload_SendsTheETagWithoutXMLEscaping(t *testing.T) {
	// The bug this exists for cost a full upload to find. S3 returns an ETag
	// wrapped in quotes; Go's XML encoder escapes a quote in character data,
	// so echoing the header verbatim emits `&#34;abc&#34;` -- and RunPod's
	// gateway compares that without un-escaping, rejecting the completion
	// after every part has been sent.
	//
	// Asserted on the bytes that go out, not on the round trip: a server
	// that echoes back whatever it was given agrees with any encoding, which
	// is exactly why the first version of this test passed.
	var completion []byte
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		q := req.URL.Query()
		switch {
		case req.Method == http.MethodPost && q.Has("uploads"):
			_, _ = fmt.Fprint(w, `<InitiateMultipartUploadResult><UploadId>up-1</UploadId></InitiateMultipartUploadResult>`)
		case req.Method == http.MethodPut:
			// Quoted, as a real S3 returns it.
			w.Header().Set("ETag", `"d41d8cd98f00b204e9800998ecf8427e"`)
		case req.Method == http.MethodPost:
			completion, _ = io.ReadAll(req.Body)
			_, _ = fmt.Fprint(w, `<CompleteMultipartUploadResult/>`)
		}
	}))
	t.Cleanup(s.Close)

	c := &Client{Endpoint: s.URL, Region: "EU-RO-1", HTTP: s.Client(),
		PartSize: 4 << 10, SinglePutLimit: 8 << 10,
		Creds: awssig.Credentials{AccessKey: "u", Secret: node.NewSecret([]byte("k"))}}

	size := c.singleLimit() + c.partSize()
	if err := c.Upload(t.Context(), "v", "k.gguf", bigReader{size}, size, nil); err != nil {
		t.Fatalf("Upload = %v", err)
	}

	body := string(completion)
	if strings.Contains(body, "&#34;") {
		t.Errorf("the completion carries an XML-escaped quote, which RunPod rejects:\n%s", body)
	}
	if !strings.Contains(body, "<ETag>d41d8cd98f00b204e9800998ecf8427e</ETag>") {
		t.Errorf("the ETag is not the bare digest:\n%s", body)
	}
}

func TestPartETag_SurvivesAProxyInThePath(t *testing.T) {
	// RunPod serves S3 behind Cloudflare, which rewrites the tag into a weak
	// validator. Echoing `W/"abc"` back is refused with "InvalidPart" after
	// every byte of every part has been sent -- so the body, which no proxy
	// touches, is the authority and the header is normalised.
	const want = "4c28640dc8df1933aaea192100d50ae0"
	for _, tc := range []struct {
		name string
		resp response
	}{
		{"body wins over a mangled header", response{
			Header: http.Header{"Etag": {`W/"` + want + `"`}},
			Body:   []byte(`<UploadPartResult><ETag>"` + want + `"</ETag></UploadPartResult>`),
		}},
		{"header alone, weak", response{Header: http.Header{"Etag": {`W/"` + want + `"`}}}},
		{"header alone, quoted", response{Header: http.Header{"Etag": {`"` + want + `"`}}}},
		{"header alone, bare", response{Header: http.Header{"Etag": {want}}}},
		{"body alone", response{
			Header: http.Header{},
			Body:   []byte(`<UploadPartResult><ETag>` + want + `</ETag></UploadPartResult>`),
		}},
		{"a body that is not an UploadPartResult falls back to the header", response{
			Header: http.Header{"Etag": {`"` + want + `"`}},
			Body:   []byte(`<Something/>`),
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := partETag(tc.resp); got != want {
				t.Errorf("partETag = %q, want %q", got, want)
			}
		})
	}

	if got := partETag(response{Header: http.Header{}}); got != "" {
		t.Errorf("an absent tag gave %q, want empty so the caller refuses", got)
	}
}
