package s3

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Legion112/ember/internal/awssig"
	"github.com/Legion112/ember/internal/node"
)

// store is a stand-in endpoint that answers the read calls.
//
// Stateful and paginated, for the reason recorder is: what breaks a listing is
// the paging, and a stateless stub that answers every request with the same
// single page cannot express a tail that is never asked for. pageSize is
// deliberately tiny so the loop runs more than once without a thousand
// entries.
type store struct {
	objects  []Object
	uploads  []IncompleteUpload
	pageSize int

	// truncatedWithNoCursor reproduces the half of the pagination bug that
	// drops a tail: a page that says there is more and gives nothing to ask
	// with.
	truncatedWithNoCursor bool

	// cursorOnFinalPage reproduces the other half. It is not an error — real
	// implementations do this — and following it would re-read the last page
	// forever.
	cursorOnFinalPage bool

	calls []string
}

func (s *store) serve(t *testing.T) *Client {
	t.Helper()
	if s.pageSize == 0 {
		s.pageSize = 2
	}
	srv := httptest.NewServer(http.HandlerFunc(s.handle))
	t.Cleanup(srv.Close)
	return &Client{
		Endpoint: srv.URL,
		Region:   "EU-RO-1",
		Creds: awssig.Credentials{
			AccessKey: "user_test",
			Secret:    node.NewSecret([]byte("rps_test_secret")),
		},
		HTTP: srv.Client(),
		Now:  func() time.Time { return time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC) },
	}
}

func (s *store) handle(w http.ResponseWriter, req *http.Request) {
	q := req.URL.Query()
	w.Header().Set("Content-Type", "application/xml")

	switch {
	case req.Method == http.MethodHead:
		key := strings.TrimPrefix(req.URL.Path, "/vol-1/")
		s.calls = append(s.calls, "head "+key)
		for _, o := range s.objects {
			if o.Key == key {
				w.Header().Set("Content-Length", strconv.FormatInt(o.Size, 10))
				w.Header().Set("ETag", `"`+o.ETag+`"`)
				w.Header().Set("Last-Modified", o.LastModified.UTC().Format(http.TimeFormat))
				// No body: Go's client fills Response.Header for a HEAD and
				// the size has to be read from there.
				w.WriteHeader(http.StatusOK)
				return
			}
		}
		w.WriteHeader(http.StatusNotFound)

	case req.Method == http.MethodGet && q.Get("list-type") == "2":
		from := atoiOr(q.Get("continuation-token"), 0)
		s.calls = append(s.calls, fmt.Sprintf("list-objects from=%d", from))
		page, next := s.page(len(s.objects), from)
		var b strings.Builder
		b.WriteString("<ListBucketResult>")
		fmt.Fprintf(&b, "<IsTruncated>%t</IsTruncated>", next > 0)
		for _, o := range s.objects[from:page] {
			fmt.Fprintf(&b, "<Contents><Key>%s</Key><Size>%d</Size>"+
				"<LastModified>%s</LastModified><ETag>&#34;%s&#34;</ETag></Contents>",
				o.Key, o.Size, o.LastModified.UTC().Format(time.RFC3339), o.ETag)
		}
		b.WriteString(s.cursor("NextContinuationToken", next))
		b.WriteString("</ListBucketResult>")
		_, _ = w.Write([]byte(b.String()))

	case req.Method == http.MethodGet && q.Has("uploads"):
		from := atoiOr(q.Get("key-marker"), 0)
		s.calls = append(s.calls, fmt.Sprintf("list-uploads from=%d", from))
		page, next := s.page(len(s.uploads), from)
		var b strings.Builder
		b.WriteString("<ListMultipartUploadsResult>")
		fmt.Fprintf(&b, "<IsTruncated>%t</IsTruncated>", next > 0)
		for _, u := range s.uploads[from:page] {
			fmt.Fprintf(&b, "<Upload><Key>%s</Key><UploadId>%s</UploadId>"+
				"<Initiated>%s</Initiated></Upload>",
				u.Key, u.UploadID, u.Initiated.UTC().Format(time.RFC3339))
		}
		b.WriteString(s.cursor("NextKeyMarker", next))
		b.WriteString("</ListMultipartUploadsResult>")
		_, _ = w.Write([]byte(b.String()))

	case req.Method == http.MethodGet && q.Has("uploadId"):
		id := q.Get("uploadId")
		s.calls = append(s.calls, "list-parts "+id)
		var b strings.Builder
		b.WriteString("<ListPartsResult><IsTruncated>false</IsTruncated>")
		for _, u := range s.uploads {
			if u.UploadID != id {
				continue
			}
			// The parts are synthesised to sum to the upload's size, so a
			// test states one number and the fake splits it the way a real
			// interrupted upload would have.
			each := u.Size / int64(max(u.Parts, 1))
			for i := range u.Parts {
				n := each
				if i == u.Parts-1 {
					n = u.Size - each*int64(u.Parts-1)
				}
				fmt.Fprintf(&b, "<Part><PartNumber>%d</PartNumber><Size>%d</Size></Part>", i+1, n)
			}
		}
		b.WriteString("</ListPartsResult>")
		_, _ = w.Write([]byte(b.String()))

	case req.Method == http.MethodDelete && q.Has("uploadId"):
		s.calls = append(s.calls, "abort "+q.Get("uploadId"))
		w.WriteHeader(http.StatusNoContent)

	default:
		s.calls = append(s.calls, "unexpected "+req.Method+" "+req.URL.RequestURI())
		w.WriteHeader(http.StatusBadRequest)
	}
}

// page returns the end of this page and the next cursor, 0 when there is none.
func (s *store) page(n, from int) (end, next int) {
	end = min(from+s.pageSize, n)
	if end < n {
		return end, end
	}
	return end, 0
}

func (s *store) cursor(tag string, next int) string {
	if s.truncatedWithNoCursor {
		return ""
	}
	if next == 0 {
		if s.cursorOnFinalPage {
			return fmt.Sprintf("<%s>999</%s>", tag, tag)
		}
		return ""
	}
	return fmt.Sprintf("<%s>%d</%s>", tag, next, tag)
}

func atoiOr(s string, def int) int {
	if n, err := strconv.Atoi(s); err == nil {
		return n
	}
	return def
}

func obj(key string, size int64) Object {
	return Object{Key: key, Size: size, ETag: "e-" + key,
		LastModified: time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)}
}

func TestHead_ReportsSizeAndAbsence(t *testing.T) {
	s := &store{objects: []Object{obj("sd15.gguf", 2<<30)}}
	c := s.serve(t)

	got, found, err := c.Head(t.Context(), "vol-1", "sd15.gguf")
	if err != nil || !found {
		t.Fatalf("Head = %+v, %t, %v", got, found, err)
	}
	if got.Size != 2<<30 {
		t.Errorf("size = %d, want %d", got.Size, int64(2)<<30)
	}
	if got.ETag != "e-sd15.gguf" {
		t.Errorf("etag = %q", got.ETag)
	}
	if got.LastModified.IsZero() {
		t.Error("no last-modified was read")
	}

	// Absent is an answer, not a failure: it is the answer `ember store
	// verify` exists to get, and an error here would make a missing asset
	// indistinguishable from an unreachable store.
	_, found, err = c.Head(t.Context(), "vol-1", "flux2.gguf")
	if err != nil {
		t.Fatalf("Head on a missing object = %v, want no error", err)
	}
	if found {
		t.Error("an object that is not there was reported as present")
	}
}

// TestHead_AnythingButA404IsAnError. A 403 means the credentials cannot see
// the store; reporting that as "not there" would send an operator to
// re-upload forty gigabytes over a permissions problem.
func TestHead_AnythingButA404IsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	t.Cleanup(srv.Close)
	c := &Client{Endpoint: srv.URL, Region: "EU-RO-1", HTTP: srv.Client(),
		Creds: awssig.Credentials{AccessKey: "u", Secret: node.NewSecret([]byte("k"))}}

	if _, found, err := c.Head(t.Context(), "vol-1", "sd15.gguf"); err == nil || found {
		t.Errorf("Head on a 403 = %t, %v; a permissions failure must not read as absence", found, err)
	}
}

// TestHead_RefusesAReplyWithNoSize. A zero size reported as fact reads as a
// file that uploaded as nothing, which is one of the states this call exists
// to detect — so it must not also be what a missing header looks like.
//
// Only the absent case is tested, and that is not an omission: Go's transport
// parses and validates Content-Length before a caller sees the response, so a
// malformed one cannot reach Head. That is why the size is read from the
// parsed ContentLength rather than re-parsed out of the header — a strconv
// there would be a branch no server could reach, and an httptest server
// refuses to send an invalid value at all.
func TestHead_RefusesAReplyWithNoSize(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	c := &Client{Endpoint: srv.URL, Region: "EU-RO-1", HTTP: srv.Client(),
		Creds: awssig.Credentials{AccessKey: "u", Secret: node.NewSecret([]byte("k"))}}

	_, _, err := c.Head(t.Context(), "vol-1", "sd15.gguf")
	if err == nil {
		t.Fatal("Head reported a size it did not learn")
	}
	if !strings.Contains(err.Error(), "no Content-Length") {
		t.Errorf("the error does not say what was missing: %v", err)
	}
}

func TestListObjects_FollowsEveryPage(t *testing.T) {
	s := &store{pageSize: 2, objects: []Object{
		obj("a.gguf", 1), obj("b.gguf", 2), obj("c.gguf", 3), obj("d.gguf", 4), obj("e.gguf", 5),
	}}
	c := s.serve(t)

	got, err := c.ListObjects(t.Context(), "vol-1", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 5 {
		t.Fatalf("ListObjects returned %d of 5 objects: %+v", len(got), got)
	}
	for i, want := range []string{"a.gguf", "b.gguf", "c.gguf", "d.gguf", "e.gguf"} {
		if got[i].Key != want {
			t.Errorf("object %d = %q, want %q", i, got[i].Key, want)
		}
	}
	if got[4].Size != 5 || got[4].LastModified.IsZero() {
		t.Errorf("the last object came back as %+v", got[4])
	}
	// Three pages for five objects at two a page, which is what proves the
	// loop ran rather than the first page happening to hold everything.
	if n := len(s.calls); n != 3 {
		t.Errorf("made %d requests for 3 pages: %v", n, s.calls)
	}
}

// TestListObjects_RefusesATruncatedPageWithNoCursor is the half of the
// pagination rule that drops a tail.
//
// A page that says there is more and gives nothing to ask with is a page ember
// cannot request, so the result would be short — and a listing silently
// missing its tail is exactly the leak these calls exist to find.
func TestListObjects_RefusesATruncatedPageWithNoCursor(t *testing.T) {
	s := &store{pageSize: 2, truncatedWithNoCursor: true, objects: []Object{
		obj("a.gguf", 1), obj("b.gguf", 2), obj("c.gguf", 3),
	}}
	c := s.serve(t)

	got, err := c.ListObjects(t.Context(), "vol-1", "")
	if err == nil {
		t.Fatalf("ListObjects returned %d objects and no error over a listing it could not finish", len(got))
	}
	if !strings.Contains(err.Error(), "missing its tail") {
		t.Errorf("the error does not say what is wrong: %v", err)
	}
}

// TestListObjects_ToleratesACursorOnTheFinalPage is the other half. Real
// implementations populate a next marker on a final page; following it would
// re-read that page until the loop bound.
func TestListObjects_ToleratesACursorOnTheFinalPage(t *testing.T) {
	s := &store{pageSize: 10, cursorOnFinalPage: true, objects: []Object{obj("a.gguf", 1)}}
	c := s.serve(t)

	got, err := c.ListObjects(t.Context(), "vol-1", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d objects", len(got))
	}
	if n := len(s.calls); n != 1 {
		t.Errorf("made %d requests; a cursor on a final page was followed: %v", n, s.calls)
	}
}

func TestListObjects_PassesThePrefix(t *testing.T) {
	var seen string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		seen = req.URL.Query().Get("prefix")
		_, _ = w.Write([]byte(`<ListBucketResult><IsTruncated>false</IsTruncated></ListBucketResult>`))
	}))
	t.Cleanup(srv.Close)
	c := &Client{Endpoint: srv.URL, Region: "EU-RO-1", HTTP: srv.Client(),
		Creds: awssig.Credentials{AccessKey: "u", Secret: node.NewSecret([]byte("k"))}}

	if _, err := c.ListObjects(t.Context(), "vol-1", "loras/"); err != nil {
		t.Fatal(err)
	}
	if seen != "loras/" {
		t.Errorf("prefix = %q, want loras/", seen)
	}
}

// TestListIncompleteUploads_MeasuresWhatEachOneIsStoring.
//
// The listing does not carry a size, so each upload costs one more request to
// measure. It is fetched rather than left out because an orphan with no size
// cannot be priced, and an orphan with no price is a line an operator reads
// and does nothing about.
func TestListIncompleteUploads_MeasuresWhatEachOneIsStoring(t *testing.T) {
	started := time.Date(2026, 10, 30, 8, 0, 0, 0, time.UTC)
	s := &store{pageSize: 1, uploads: []IncompleteUpload{
		{Key: "flux2.gguf", UploadID: "up-1", Initiated: started, Parts: 3, Size: 384 << 20},
		{Key: "wan22.gguf", UploadID: "up-2", Initiated: started, Parts: 1, Size: 128 << 20},
	}}
	c := s.serve(t)

	got, err := c.ListIncompleteUploads(t.Context(), "vol-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d incomplete uploads: %+v", len(got), got)
	}
	if got[0].Key != "flux2.gguf" || got[0].UploadID != "up-1" {
		t.Errorf("first upload = %+v", got[0])
	}
	if got[0].Parts != 3 || got[0].Size != 384<<20 {
		t.Errorf("first upload measured %d parts / %d bytes, want 3 / %d",
			got[0].Parts, got[0].Size, int64(384)<<20)
	}
	if got[1].Size != 128<<20 {
		t.Errorf("second upload measured %d bytes", got[1].Size)
	}

	// Age is what makes an abort safe, so it has to be readable.
	now := started.Add(26 * time.Hour)
	if age := got[0].Age(now); age != 26*time.Hour {
		t.Errorf("Age = %s, want 26h", age)
	}
}

// TestListIncompleteUploads_RefusesAnEntryWithNoUploadID. Without an id it can
// be neither priced nor aborted, which is a hole in the listing rather than an
// entry in it.
func TestListIncompleteUploads_RefusesAnEntryWithNoUploadID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`<ListMultipartUploadsResult><IsTruncated>false</IsTruncated>` +
			`<Upload><Key>flux2.gguf</Key><Initiated>2026-10-30T08:00:00Z</Initiated></Upload>` +
			`</ListMultipartUploadsResult>`))
	}))
	t.Cleanup(srv.Close)
	c := &Client{Endpoint: srv.URL, Region: "EU-RO-1", HTTP: srv.Client(),
		Creds: awssig.Credentials{AccessKey: "u", Secret: node.NewSecret([]byte("k"))}}

	if _, err := c.ListIncompleteUploads(t.Context(), "vol-1"); err == nil {
		t.Error("an upload with no id was accepted; nothing could ever abort it")
	}
}

func TestListIncompleteUploads_FollowsEveryPage(t *testing.T) {
	started := time.Date(2026, 10, 30, 8, 0, 0, 0, time.UTC)
	s := &store{pageSize: 1, uploads: []IncompleteUpload{
		{Key: "a", UploadID: "up-1", Initiated: started, Parts: 1, Size: 1 << 20},
		{Key: "b", UploadID: "up-2", Initiated: started, Parts: 1, Size: 1 << 20},
		{Key: "c", UploadID: "up-3", Initiated: started, Parts: 1, Size: 1 << 20},
	}}
	c := s.serve(t)

	got, err := c.ListIncompleteUploads(t.Context(), "vol-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("ListIncompleteUploads returned %d of 3: %+v", len(got), got)
	}
}

func TestListIncompleteUploads_RefusesATruncatedPageWithNoCursor(t *testing.T) {
	started := time.Date(2026, 10, 30, 8, 0, 0, 0, time.UTC)
	s := &store{pageSize: 1, truncatedWithNoCursor: true, uploads: []IncompleteUpload{
		{Key: "a", UploadID: "up-1", Initiated: started, Parts: 1, Size: 1 << 20},
		{Key: "b", UploadID: "up-2", Initiated: started, Parts: 1, Size: 1 << 20},
	}}
	c := s.serve(t)

	if _, err := c.ListIncompleteUploads(t.Context(), "vol-1"); err == nil {
		t.Error("a truncated listing of incomplete uploads read as complete")
	}
}

func TestAbort_RemovesAnUploadAndIsIdempotent(t *testing.T) {
	s := &store{}
	c := s.serve(t)

	if err := c.Abort(t.Context(), "vol-1", "flux2.gguf", "up-1"); err != nil {
		t.Fatal(err)
	}
	if len(s.calls) != 1 || s.calls[0] != "abort up-1" {
		t.Errorf("calls = %v", s.calls)
	}

	// An id is required: without one the DELETE would be a request to remove
	// the object itself.
	if err := c.Abort(t.Context(), "vol-1", "flux2.gguf", ""); err == nil {
		t.Error("an abort with no upload id was sent")
	}
}

// TestAbort_NotFoundIsSuccess, as destroying a node or a store is: the goal is
// that the upload is gone, and it is.
func TestAbort_NotFoundIsSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	c := &Client{Endpoint: srv.URL, Region: "EU-RO-1", HTTP: srv.Client(),
		Creds: awssig.Credentials{AccessKey: "u", Secret: node.NewSecret([]byte("k"))}}

	if err := c.Abort(t.Context(), "vol-1", "flux2.gguf", "up-1"); err != nil {
		t.Errorf("aborting an upload that is already gone = %v", err)
	}
}

func TestAbort_SurfacesARealFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`<Error><Code>AccessDenied</Code></Error>`))
	}))
	t.Cleanup(srv.Close)
	c := &Client{Endpoint: srv.URL, Region: "EU-RO-1", HTTP: srv.Client(),
		Creds: awssig.Credentials{AccessKey: "u", Secret: node.NewSecret([]byte("k"))}}

	err := c.Abort(t.Context(), "vol-1", "flux2.gguf", "up-1")
	if err == nil {
		t.Fatal("a refused abort reported success; the parts are still billing")
	}
	// The reason is in the body and not the status — SignatureDoesNotMatch and
	// AccessDenied are both 403 — so it has to reach the operator.
	if !strings.Contains(err.Error(), "AccessDenied") {
		t.Errorf("the error drops the endpoint's own explanation: %v", err)
	}
}

// TestRead_EverySignatureIsWellFormed. A signature's only other feedback is a
// 403 with no detail, so it is checked at the server — and these are the first
// GETs and the first HEAD this package has ever signed.
func TestRead_EverySignatureIsWellFormed(t *testing.T) {
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		seen = append(seen, req.Header.Get("Authorization"))
		if got := req.Header.Get("X-Amz-Content-Sha256"); got != awssig.EmptyPayloadHash {
			t.Errorf("a bodiless request signed payload hash %q, want the empty-payload hash", got)
		}
		if req.Method == http.MethodHead {
			// A HEAD carries the size in the header and no body, so the two
			// cases cannot share one reply.
			w.Header().Set("Content-Length", "1")
			w.WriteHeader(http.StatusOK)
			return
		}
		_, _ = w.Write([]byte(`<ListBucketResult><IsTruncated>false</IsTruncated></ListBucketResult>`))
	}))
	t.Cleanup(srv.Close)
	c := &Client{Endpoint: srv.URL, Region: "EU-RO-1", HTTP: srv.Client(),
		Creds: awssig.Credentials{AccessKey: "user_x", Secret: node.NewSecret([]byte("k"))}}

	if _, err := c.ListObjects(t.Context(), "vol-1", ""); err != nil {
		t.Fatal(err)
	}
	if _, _, err := c.Head(t.Context(), "vol-1", "a.gguf"); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 2 {
		t.Fatalf("saw %d requests", len(seen))
	}
	for _, a := range seen {
		for _, want := range []string{awssig.Algorithm, "Credential=user_x/", "/EU-RO-1/s3/aws4_request",
			"SignedHeaders=", "Signature="} {
			if !strings.Contains(a, want) {
				t.Errorf("Authorization %q is missing %q", a, want)
			}
		}
	}
}

// TestRead_AListingIsAskedForAtTheBucketAndNotUnderIt.
//
// A bucket-level listing requested at "/vol-1/" is a listing of a store whose
// name ends in a slash, and some endpoints answer that with nothing rather
// than an error — which would read as an empty store.
func TestRead_AListingIsAskedForAtTheBucketAndNotUnderIt(t *testing.T) {
	var path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		path = req.URL.Path
		_, _ = w.Write([]byte(`<ListBucketResult><IsTruncated>false</IsTruncated></ListBucketResult>`))
	}))
	t.Cleanup(srv.Close)
	c := &Client{Endpoint: srv.URL, Region: "EU-RO-1", HTTP: srv.Client(),
		Creds: awssig.Credentials{AccessKey: "u", Secret: node.NewSecret([]byte("k"))}}

	if _, err := c.ListObjects(t.Context(), "vol-1", ""); err != nil {
		t.Fatal(err)
	}
	if path != "/vol-1" {
		t.Errorf("listed at %q, want /vol-1", path)
	}
}

// TestDo_RefusesABodyLargerThanItWillRead.
//
// io.LimitReader truncates silently, and a truncated listing page parses as
// XML right up to where it does not — surfacing as a malformed-document error
// naming neither the size nor the cause.
func TestDo_RefusesABodyLargerThanItWillRead(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(make([]byte, maxBody+1))
	}))
	t.Cleanup(srv.Close)
	c := &Client{Endpoint: srv.URL, Region: "EU-RO-1", HTTP: srv.Client(),
		Creds: awssig.Credentials{AccessKey: "u", Secret: node.NewSecret([]byte("k"))}}

	_, err := c.ListObjects(t.Context(), "vol-1", "")
	if err == nil {
		t.Fatal("an over-long reply was parsed as though it were whole")
	}
	if !strings.Contains(err.Error(), "larger than") {
		t.Errorf("the error does not say the reply was too big: %v", err)
	}
}

func TestParseS3Time_ZeroRatherThanAnError(t *testing.T) {
	// The timestamp is for the operator to read. Refusing the whole listing —
	// the listing that finds a leak — because one entry's clock format is
	// unfamiliar would lose the thing that matters to keep the thing that
	// does not.
	if got := parseS3Time("2026-11-01T09:00:00.123Z"); got.IsZero() {
		t.Error("a fractional-second timestamp did not parse")
	}
	if got := parseS3Time("2026-11-01T09:00:00Z"); got.IsZero() {
		t.Error("a whole-second timestamp did not parse")
	}
	for _, bad := range []string{"", "last tuesday", "30/10/2026"} {
		if got := parseS3Time(bad); !got.IsZero() {
			t.Errorf("parseS3Time(%q) = %s, want the zero time", bad, got)
		}
	}
}

// TestListObjects_StopsAtTheBoundRatherThanReturningAShortList.
//
// Against a store that says every page is truncated, the loop has to end — and
// it has to end with an error. Returning what it had would be a listing
// silently missing its tail, which is the exact failure these calls were added
// to find. The real constant is exercised rather than an injected one, because
// a bound a test can lower is a bound a test is not checking.
func TestListObjects_StopsAtTheBoundRatherThanReturningAShortList(t *testing.T) {
	pages := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		pages++
		_, _ = w.Write([]byte(`<ListBucketResult><IsTruncated>true</IsTruncated>` +
			`<Contents><Key>a.gguf</Key><Size>1</Size></Contents>` +
			`<NextContinuationToken>more</NextContinuationToken></ListBucketResult>`))
	}))
	t.Cleanup(srv.Close)
	c := &Client{Endpoint: srv.URL, Region: "EU-RO-1", HTTP: srv.Client(),
		Creds: awssig.Credentials{AccessKey: "u", Secret: node.NewSecret([]byte("k"))}}

	got, err := c.ListObjects(t.Context(), "vol-1", "")
	if err == nil {
		t.Fatalf("ListObjects returned %d objects and no error over an endless listing", len(got))
	}
	if !strings.Contains(err.Error(), "missing its tail") {
		t.Errorf("the error does not say the result would be incomplete: %v", err)
	}
	if pages != maxPages {
		t.Errorf("made %d requests, want the bound of %d", pages, maxPages)
	}
}

// TestRead_AReplyThatIsNotTheDocumentAskedFor is one error path per listing.
//
// Every one of these is a GET that answered 200 with something that is not the
// XML it should be, which is what a proxy serving an error page looks like.
func TestRead_AReplyThatIsNotTheDocumentAskedFor(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`<html><body>502 Bad Gateway`))
	}))
	t.Cleanup(srv.Close)
	c := &Client{Endpoint: srv.URL, Region: "EU-RO-1", HTTP: srv.Client(),
		Creds: awssig.Credentials{AccessKey: "u", Secret: node.NewSecret([]byte("k"))}}

	if _, err := c.ListObjects(t.Context(), "vol-1", ""); err == nil {
		t.Error("ListObjects accepted a reply that is not a listing")
	}
	if _, err := c.ListIncompleteUploads(t.Context(), "vol-1"); err == nil {
		t.Error("ListIncompleteUploads accepted a reply that is not a listing")
	}
}

// TestListIncompleteUploads_AFailureMeasuringOneIsAFailure.
//
// An upload whose parts could not be listed has an unknown size, and an orphan
// with no size cannot be priced. Reporting it as zero bytes would be the
// leak-prices-as-free failure MonthlyBytes exists to prevent, arrived at from
// the other side.
func TestListIncompleteUploads_AFailureMeasuringOneIsAFailure(t *testing.T) {
	for name, parts := range map[string]string{
		"the parts listing is refused": "",
		"the parts listing has no cursor and says there is more": `<ListPartsResult>` +
			`<IsTruncated>true</IsTruncated><Part><PartNumber>1</PartNumber><Size>1</Size></Part>` +
			`</ListPartsResult>`,
	} {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				if req.URL.Query().Has("uploads") {
					_, _ = w.Write([]byte(`<ListMultipartUploadsResult><IsTruncated>false</IsTruncated>` +
						`<Upload><Key>flux2.gguf</Key><UploadId>up-1</UploadId>` +
						`<Initiated>2026-10-30T08:00:00Z</Initiated></Upload>` +
						`</ListMultipartUploadsResult>`))
					return
				}
				if parts == "" {
					w.WriteHeader(http.StatusForbidden)
					_, _ = w.Write([]byte(`<Error><Code>AccessDenied</Code></Error>`))
					return
				}
				_, _ = w.Write([]byte(parts))
			}))
			t.Cleanup(srv.Close)
			c := &Client{Endpoint: srv.URL, Region: "EU-RO-1", HTTP: srv.Client(),
				Creds: awssig.Credentials{AccessKey: "u", Secret: node.NewSecret([]byte("k"))}}

			got, err := c.ListIncompleteUploads(t.Context(), "vol-1")
			if err == nil {
				t.Fatalf("ListIncompleteUploads returned %+v and no error", got)
			}
		})
	}
}

// TestUploadSize_FollowsEveryPageOfParts. A 40 GB upload in 128 MiB parts is
// 320 of them, which is one page — but a smaller part size is a field, and an
// upload measured from only its first page is an orphan priced at a fraction
// of what it costs.
func TestUploadSize_FollowsEveryPageOfParts(t *testing.T) {
	pages := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Query().Has("uploads") {
			_, _ = w.Write([]byte(`<ListMultipartUploadsResult><IsTruncated>false</IsTruncated>` +
				`<Upload><Key>flux2.gguf</Key><UploadId>up-1</UploadId>` +
				`<Initiated>2026-10-30T08:00:00Z</Initiated></Upload>` +
				`</ListMultipartUploadsResult>`))
			return
		}
		pages++
		if pages == 1 {
			_, _ = w.Write([]byte(`<ListPartsResult><IsTruncated>true</IsTruncated>` +
				`<Part><PartNumber>1</PartNumber><Size>100</Size></Part>` +
				`<NextPartNumberMarker>1</NextPartNumberMarker></ListPartsResult>`))
			return
		}
		_, _ = w.Write([]byte(`<ListPartsResult><IsTruncated>false</IsTruncated>` +
			`<Part><PartNumber>2</PartNumber><Size>23</Size></Part></ListPartsResult>`))
	}))
	t.Cleanup(srv.Close)
	c := &Client{Endpoint: srv.URL, Region: "EU-RO-1", HTTP: srv.Client(),
		Creds: awssig.Credentials{AccessKey: "u", Secret: node.NewSecret([]byte("k"))}}

	got, err := c.ListIncompleteUploads(t.Context(), "vol-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("got %+v", got)
	}
	if got[0].Parts != 2 || got[0].Size != 123 {
		t.Errorf("measured %d parts / %d bytes, want 2 / 123 across both pages", got[0].Parts, got[0].Size)
	}
}
