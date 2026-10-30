// Package s3 uploads a file to an S3-compatible endpoint.
//
// Enough of S3 to put weights on a store and no more: one PUT for a small
// object, multipart for a large one, and an abort that runs when anything
// goes wrong. There is no download here -- a node fetches its own assets over
// plain HTTP, verified by digest -- and no listing, because what is on a
// store is a question for the node that mounts it.
//
// It does not go through internal/httpx, which sets a bearer token and JSON
// headers on every request. S3 authenticates per request with a signature
// over the headers, so the two have nothing in common but the word "client".
package s3

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/Legion112/ember/internal/awssig"
)

// Service is the name S3 signs under.
const Service = "s3"

// PartSize is how much of a large file goes in one part.
//
// RunPod refuses a single PUT above 500 MB, so anything larger has to be
// multipart; 128 MiB parts put a 14 GB expert in 110 of them and leave room
// for a 1.2 TiB object before the 10,000-part ceiling. Smaller parts would
// mean more requests for no gain, larger ones more to re-send when one fails.
const PartSize int64 = 128 << 20

// SinglePutLimit is the largest object sent in one request.
//
// Below RunPod's documented 500 MB cap rather than at it, because the cap is
// theirs to change and a request that is refused has already spent the upload.
const SinglePutLimit int64 = 400 << 20

// MaxParts is S3's ceiling on parts in one upload.
const MaxParts = 10000

// Client talks to one datacentre's S3 endpoint.
type Client struct {
	// Endpoint is the scheme and host, as https://s3api-EU-RO-1.runpod.io.
	Endpoint string

	// Region is the datacentre id, which is also the signing region. RunPod
	// uses the same string for both, which is why there is one field.
	Region string

	Creds awssig.Credentials

	HTTP *http.Client

	// Now is injected only so a test can pin a signature. Production leaves
	// it nil.
	Now func() time.Time

	// PartSize and SinglePutLimit override the defaults.
	//
	// Fields rather than constants because the right part size depends on
	// the link: 128 MiB is sensible over a datacentre connection and painful
	// over a home one, where a failed part is twenty minutes to re-send.
	// They also let the tests exercise the multipart path without moving
	// half a gigabyte through an httptest server on every run.
	PartSize       int64
	SinglePutLimit int64
}

func (c *Client) partSize() int64 {
	if c.PartSize > 0 {
		return c.PartSize
	}
	return PartSize
}

func (c *Client) singleLimit() int64 {
	if c.SinglePutLimit > 0 {
		return c.SinglePutLimit
	}
	return SinglePutLimit
}

// EndpointFor is where a store in this datacentre is reached.
//
// Not every datacentre has one: RunPod serves the S3 API from 15 of the 23
// that accept a volume, so a store created outside those can only be filled
// by renting something to download into it. The caller checks; this only
// builds the name.
func EndpointFor(datacentre string) string {
	return "https://s3api-" + strings.ToLower(datacentre) + ".runpod.io"
}

func (c *Client) client() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	// No overall timeout: a part is up to 128 MiB over a home upload link.
	// The bounds that matter are on the parts that hang without
	// transferring, and the caller's context ends a stalled one.
	return &http.Client{Transport: &http.Transport{
		TLSHandshakeTimeout:   30 * time.Second,
		ResponseHeaderTimeout: 2 * time.Minute,
		IdleConnTimeout:       90 * time.Second,
	}}
}

func (c *Client) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

// Upload puts a file on the store, choosing single or multipart by size.
//
// src is read more than once -- once per part, and once more if a part is
// retried -- so it is a ReaderAt rather than a Reader. A stream would have to
// be buffered, and buffering a 14 GB expert to upload it is how a laptop runs
// out of memory doing something that should only need a file handle.
func (c *Client) Upload(ctx context.Context, bucket, key string, src io.ReaderAt, size int64, out io.Writer) error {
	if size < 0 {
		return fmt.Errorf("negative size %d", size)
	}
	if size <= c.singleLimit() {
		return c.put(ctx, bucket, key, src, size, out)
	}
	return c.multipart(ctx, bucket, key, src, size, out)
}

func (c *Client) put(ctx context.Context, bucket, key string, src io.ReaderAt, size int64, out io.Writer) error {
	body := make([]byte, size)
	if _, err := src.ReadAt(body, 0); err != nil && err != io.EOF {
		return fmt.Errorf("read %s: %w", key, err)
	}
	sum := sha256.Sum256(body)

	say(out, "uploading %s (%s) in one request\n", key, human(size))
	_, err := c.do(ctx, http.MethodPut, c.path(bucket, key), nil,
		bytes.NewReader(body), int64(len(body)), hex.EncodeToString(sum[:]))
	return err
}

// multipart uploads in parts, and aborts if any of them fails.
//
// The abort is the part worth reading. An upload left incomplete keeps every
// part that did arrive, and S3-compatible stores bill for them -- invisibly,
// because an incomplete upload is not an object and does not appear in a
// listing. It is the same shape as a leaked pod: nothing shows it, and it
// charges by the month.
func (c *Client) multipart(ctx context.Context, bucket, key string, src io.ReaderAt, size int64, out io.Writer) (err error) {
	ps := c.partSize()
	parts := (size + ps - 1) / ps
	if parts > MaxParts {
		return fmt.Errorf("%s needs %d parts of %s, above S3's ceiling of %d",
			key, parts, human(ps), MaxParts)
	}

	uploadID, err := c.create(ctx, bucket, key)
	if err != nil {
		return err
	}
	say(out, "uploading %s (%s) in %d parts\n", key, human(size), parts)

	defer func() {
		if err == nil {
			return
		}
		// On a context the cancellation cannot reach, for the reason
		// provision's destroy uses one: Ctrl-C is the most likely way to end
		// up here, and an abort on the cancelled context fails instantly --
		// leaving the parts, and the charge, behind.
		actx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
		defer cancel()
		if aerr := c.abort(actx, bucket, key, uploadID); aerr != nil {
			err = fmt.Errorf("%w (and the incomplete upload could not be aborted: %v; "+
				"its parts are still stored and still billing)", err, aerr)
		}
	}()

	tags := make([]completedPart, 0, parts)
	for i := range parts {
		offset := i * ps
		n := min(ps, size-offset)

		buf := make([]byte, n)
		if _, rerr := src.ReadAt(buf, offset); rerr != nil && rerr != io.EOF {
			return fmt.Errorf("read part %d of %s: %w", i+1, key, rerr)
		}
		sum := sha256.Sum256(buf)

		q := url.Values{
			"partNumber": {strconv.FormatInt(i+1, 10)},
			"uploadId":   {uploadID},
		}
		resp, perr := c.do(ctx, http.MethodPut, c.path(bucket, key), q,
			bytes.NewReader(buf), n, hex.EncodeToString(sum[:]))
		if perr != nil {
			return fmt.Errorf("part %d of %d: %w", i+1, parts, perr)
		}
		etag := partETag(resp)
		if etag == "" {
			// Without it the completion call cannot name the part, and S3
			// would reject the whole upload after every byte had been sent.
			return fmt.Errorf("part %d of %s came back with no ETag", i+1, key)
		}
		tags = append(tags, completedPart{PartNumber: i + 1, ETag: etag})
		say(out, "  part %d/%d (%s)\n", i+1, parts, human(n))
	}

	return c.complete(ctx, bucket, key, uploadID, tags)
}

func (c *Client) create(ctx context.Context, bucket, key string) (string, error) {
	resp, err := c.do(ctx, http.MethodPost, c.path(bucket, key),
		url.Values{"uploads": {""}}, nil, 0, awssig.EmptyPayloadHash)
	if err != nil {
		return "", fmt.Errorf("start upload of %s: %w", key, err)
	}

	var r initiateResult
	if err := xml.Unmarshal(resp.Body, &r); err != nil {
		return "", fmt.Errorf("start upload of %s: %w", key, err)
	}
	if r.UploadID == "" {
		return "", fmt.Errorf("start upload of %s: no upload id in the response", key)
	}
	return r.UploadID, nil
}

func (c *Client) complete(ctx context.Context, bucket, key, uploadID string, parts []completedPart) error {
	body, err := xml.Marshal(completeUpload{Parts: parts})
	if err != nil {
		return err
	}
	sum := sha256.Sum256(body)

	resp, err := c.do(ctx, http.MethodPost, c.path(bucket, key),
		url.Values{"uploadId": {uploadID}}, bytes.NewReader(body), int64(len(body)), hex.EncodeToString(sum[:]))
	if err != nil {
		return fmt.Errorf("finish upload of %s: %w", key, err)
	}

	// S3 answers 200 and *then* reports a failure in the body, because the
	// connection is held open while it assembles the object. A client that
	// trusted the status would report success for an upload that did not
	// complete.
	if bytes.Contains(resp.Body, []byte("<Error")) {
		return fmt.Errorf("finish upload of %s: %s", key, strings.TrimSpace(string(resp.Body)))
	}
	return nil
}

func (c *Client) abort(ctx context.Context, bucket, key, uploadID string) error {
	_, err := c.do(ctx, http.MethodDelete, c.path(bucket, key),
		url.Values{"uploadId": {uploadID}}, nil, 0, awssig.EmptyPayloadHash)
	return err
}

// response is a reply whose body has already been read and closed.
//
// Returned instead of an *http.Response because every body here is small --
// an XML document or nothing -- and because a client that hands back an open
// body has three call sites that must each remember to close it. One of them
// forgetting is a leaked connection per part of a multipart upload.
type response struct {
	Header http.Header
	Body   []byte
}

// maxBody bounds what a reply may be. The largest real one is a few hundred
// bytes of XML; anything near this is a server that has stopped making sense.
const maxBody = 1 << 20

// do signs and sends one request, reading and closing the reply.
func (c *Client) do(ctx context.Context, method, path string, q url.Values, body io.Reader, size int64, payloadHash string) (response, error) {
	if c.Endpoint == "" {
		return response{}, fmt.Errorf("no S3 endpoint")
	}
	u, err := url.Parse(c.Endpoint)
	if err != nil {
		return response{}, fmt.Errorf("endpoint %q: %w", c.Endpoint, err)
	}
	u.Path = path
	if q != nil {
		u.RawQuery = q.Encode()
	}

	req, err := http.NewRequestWithContext(ctx, method, u.String(), body)
	if err != nil {
		return response{}, err
	}
	if body != nil {
		req.ContentLength = size
	}
	// Signed before sending, over the headers as they will go out. Anything
	// added afterwards is not covered and makes the signature wrong.
	req.Header.Set("X-Amz-Content-Sha256", payloadHash)
	if err := awssig.Sign(req, c.Creds, c.Region, Service, payloadHash, c.now()); err != nil {
		return response{}, err
	}

	resp, err := c.client().Do(req)
	if err != nil {
		return response{}, fmt.Errorf("%s %s: %w", method, redact(u), err)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return response{}, &StatusError{Method: method, URL: redact(u), Code: resp.StatusCode,
			Body: strings.TrimSpace(string(raw))}
	}
	if readErr != nil {
		return response{}, fmt.Errorf("%s %s: %w", method, redact(u), readErr)
	}
	return response{Header: resp.Header, Body: raw}, nil
}

// StatusError is a non-2xx answer, carrying the endpoint's own explanation.
//
// S3 puts the reason in the body and not the status -- SignatureDoesNotMatch
// and AccessDenied are both 403 -- so a client that reported only the code
// would hand back the one detail that cannot be guessed.
type StatusError struct {
	Method string
	URL    string
	Code   int
	Body   string
}

func (e *StatusError) Error() string {
	s := fmt.Sprintf("%s %s: %s", e.Method, e.URL, http.StatusText(e.Code))
	if e.Body != "" {
		s += ": " + e.Body
	}
	return s
}

// path is /bucket/key, with the key's separators preserved.
func (c *Client) path(bucket, key string) string {
	return "/" + bucket + "/" + strings.TrimPrefix(key, "/")
}

// redact strips the query, which carries the signature on a presigned URL and
// the upload id on every multipart call.
func redact(u *url.URL) string {
	clean := *u
	clean.RawQuery = ""
	return clean.String()
}

type initiateResult struct {
	XMLName  xml.Name `xml:"InitiateMultipartUploadResult"`
	UploadID string   `xml:"UploadId"`
}

// partETag is the tag to echo back for an uploaded part.
//
// The body first, the header second, and both normalised -- because the
// header cannot be trusted to survive the trip. RunPod serves this endpoint
// behind Cloudflare, which rewrites the tag into a weak validator:
//
//	Etag: W/"4c28640dc8df1933aaea192100d50ae0"
//
// Echo that back and the completion is refused with "InvalidPart: part 1
// ETag mismatch", after every byte of every part has been sent. The response
// body carries the origin's own value and no proxy touches it, so it wins
// where it exists.
//
// Found against the real endpoint. A test server echoes back whatever the
// client sent and agrees with any encoding, so nothing short of a CDN in the
// path could have produced this.
func partETag(resp response) string {
	var r uploadPartResult
	if err := xml.Unmarshal(resp.Body, &r); err == nil && r.ETag != "" {
		return normalizeETag(r.ETag)
	}
	return normalizeETag(resp.Header.Get("ETag"))
}

// normalizeETag strips the weak-validator marker and the quotes around it.
//
// Quotes go because Go's XML encoder escapes them into `&#34;`, which the
// gateway compares without un-escaping; `W/` goes because it is the proxy's
// annotation and not part of the tag.
func normalizeETag(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "W/")
	s = strings.TrimPrefix(s, "w/")
	return strings.Trim(s, `"`)
}

// uploadPartResult is the body RunPod returns from UploadPart.
//
// AWS leaves it empty and puts the tag only in the header; this endpoint
// sends both, which is what makes the body usable as the authority.
type uploadPartResult struct {
	XMLName xml.Name `xml:"UploadPartResult"`
	ETag    string   `xml:"ETag"`
}

// completedPart names one uploaded part in the completion call.
//
// The ETag is stored stripped of the quotes S3 wraps it in, and that is not
// cosmetic. Go's XML encoder escapes a quote in character data, so keeping
// them emits `<ETag>&#34;abc&#34;</ETag>` -- and RunPod's gateway compares
// that string without un-escaping it, answering "InvalidPart: part 1 ETag
// mismatch" after every byte of every part has already been sent.
//
// Bare hex has nothing to escape, so it reaches the server as written
// whatever the implementation does. Found against the real endpoint on the
// first multipart upload: the test server echoed back whatever the client
// sent, which exercises the sequence and can say nothing about the encoding.
type completedPart struct {
	PartNumber int64  `xml:"PartNumber"`
	ETag       string `xml:"ETag"`
}

type completeUpload struct {
	XMLName xml.Name        `xml:"CompleteMultipartUpload"`
	Parts   []completedPart `xml:"Part"`
}

func human(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GiB", float64(n)/float64(int64(1)<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/float64(int64(1)<<20))
	default:
		return fmt.Sprintf("%d B", n)
	}
}

func say(out io.Writer, format string, a ...any) {
	if out == nil {
		return
	}
	_, _ = fmt.Fprintf(out, format, a...)
}
