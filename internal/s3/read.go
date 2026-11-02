package s3

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/Legion112/ember/internal/awssig"
)

// The read half of the client, and why it took a second pass to exist.
//
// This package could only write. `ember store push` and `ember store sync`
// could put forty gigabytes on a store and nothing in the program could
// afterwards say what was on it -- not the names, not the sizes, not whether
// an asset a run is about to need is there at all. The store was reachable
// over the S3 endpoint with no node rented and no GPU billing, and ember
// asked it nothing.
//
// Two things follow from that, and the second is money.
//
// A store's contents were knowable only from a rented node: assets are
// verified over the same SSH connection the workload uses, so the cheapest
// moment to learn that a 14 GB expert is half a file was after provisioning,
// the image pull and the mount had all been paid for. One HEAD per asset
// answers "will this run get the weights it asked for" for nothing.
//
// And an interrupted multipart upload keeps every part that arrived and bills
// for them. It is not an object, so it is in no listing; `multipart`'s own
// comment has said so since it was written, and the abort it describes runs
// from a defer on a context Ctrl-C cannot cancel -- which covers Ctrl-C and
// covers neither SIGKILL, nor a panic, nor a laptop closing, nor an OOM kill.
// In every one of those the parts of a 40 GB upload stay on the store and
// charge by the gigabyte-month, and until these calls existed there was no
// command in ember that could find them. That was the one money leak in the
// repository with no detection path at all.
//
// Everything here is a GET or a HEAD. None of it can spend money, which is
// the distinction DryProvider already draws one layer up.

// Object is one object on a store.
type Object struct {
	Key          string
	Size         int64
	LastModified time.Time

	// ETag is the store's own validator. It is deliberately **not** treated
	// as a digest anywhere: for a single-part object it is the MD5 of the
	// content, for a multipart one it is an MD5 of the parts' MD5s with a
	// part count stuck on the end, and ember's `assets:` digests are
	// SHA-256. Comparing the two would be a check that always fails, and
	// comparing them loosely would be one that always passes.
	ETag string
}

// IncompleteUpload is a multipart upload that was started and never finished.
//
// Not an object: it appears in no object listing, and the only way to see one
// is to ask for it by name. Size is the sum of the parts that did arrive,
// which is what it is being billed for -- and it takes a second call per
// upload to learn, because ListMultipartUploads does not report it.
type IncompleteUpload struct {
	Key       string
	UploadID  string
	Initiated time.Time
	Parts     int
	Size      int64
}

// Age is how long this upload has been sitting there.
//
// It is the field that makes an abort safe. A multipart upload in progress is
// indistinguishable from an abandoned one -- both are "started, not
// completed" -- so anything that removes them has to be bounded by age, or a
// push running in another terminal is killed by the command meant to tidy up
// after a crashed one. Measured over a 67 Mbps home link, a 40 GB model set
// takes eighty-one minutes to upload, so the bound has to be generous.
func (u IncompleteUpload) Age(now time.Time) time.Duration { return now.Sub(u.Initiated) }

// Head reports what the store says about one object.
//
// found is false for a 404 and no error, because "the asset is not there" is
// an answer rather than a failure -- it is the answer `ember store verify`
// exists to get. Any other status is an error: a 403 means the credentials
// cannot see the store, and reporting that as "not there" would send an
// operator to re-upload forty gigabytes over a permissions problem.
func (c *Client) Head(ctx context.Context, bucket, key string) (Object, bool, error) {
	resp, err := c.do(ctx, http.MethodHead, c.path(bucket, key), nil, nil, 0, awssig.EmptyPayloadHash)
	if err != nil {
		var se *StatusError
		if errors.As(err, &se) && se.Code == http.StatusNotFound {
			return Object{}, false, nil
		}
		return Object{}, false, err
	}

	// A size that was never reported is an error and not a zero. A store with
	// no Content-Length on a HEAD has stopped making sense, and zero reported
	// as fact reads as a file that uploaded as nothing -- which is one of the
	// states this call exists to detect, so it must not also be what a
	// missing header looks like.
	if resp.ContentLength < 0 {
		return Object{}, false, fmt.Errorf("the store answered HEAD %s with no Content-Length, so its size is unknown; "+
			"ember will not report a size it did not learn", key)
	}
	o := Object{
		Key:  key,
		Size: resp.ContentLength,
		ETag: normalizeETag(resp.Header.Get("ETag")),
	}
	if t, err := http.ParseTime(resp.Header.Get("Last-Modified")); err == nil {
		o.LastModified = t
	}
	return o, true, nil
}

// ListObjects lists every object under prefix.
//
// An empty prefix is the whole store, which is what `ember store ls` asks
// for.
func (c *Client) ListObjects(ctx context.Context, bucket, prefix string) ([]Object, error) {
	var out []Object
	token := ""

	for page := 0; ; page++ {
		if page >= maxPages {
			return nil, incompleteListing("objects", bucket, len(out))
		}
		q := url.Values{"list-type": {"2"}, "max-keys": {strconv.Itoa(maxKeys)}}
		if prefix != "" {
			q.Set("prefix", prefix)
		}
		if token != "" {
			q.Set("continuation-token", token)
		}

		resp, err := c.do(ctx, http.MethodGet, c.bucketPath(bucket), q, nil, 0, awssig.EmptyPayloadHash)
		if err != nil {
			return nil, fmt.Errorf("list what is on the store: %w", err)
		}
		var r listBucketResult
		if err := xml.Unmarshal(resp.Body, &r); err != nil {
			return nil, fmt.Errorf("list what is on the store: %w", err)
		}

		for _, e := range r.Contents {
			out = append(out, Object{
				Key:          e.Key,
				Size:         e.Size,
				LastModified: parseS3Time(e.LastModified),
				ETag:         normalizeETag(e.ETag),
			})
		}

		next, err := nextCursor("objects", r.IsTruncated, r.NextContinuationToken)
		if err != nil {
			return nil, err
		}
		if next == "" {
			return out, nil
		}
		token = next
	}
}

// ListIncompleteUploads lists every multipart upload that was never
// completed, with what each one is storing.
//
// The size costs one extra request per upload, because the listing does not
// carry it -- and it is fetched rather than left out, because an orphan with
// no size cannot be priced, and an orphan with no price is a line an operator
// reads and does nothing about. There are normally none of these at all, so
// the extra calls are paid only in the case that matters.
func (c *Client) ListIncompleteUploads(ctx context.Context, bucket string) ([]IncompleteUpload, error) {
	var out []IncompleteUpload
	keyMarker, idMarker := "", ""

	for page := 0; ; page++ {
		if page >= maxPages {
			return nil, incompleteListing("incomplete uploads", bucket, len(out))
		}
		q := url.Values{"uploads": {""}, "max-uploads": {strconv.Itoa(maxKeys)}}
		if keyMarker != "" {
			q.Set("key-marker", keyMarker)
		}
		if idMarker != "" {
			q.Set("upload-id-marker", idMarker)
		}

		resp, err := c.do(ctx, http.MethodGet, c.bucketPath(bucket), q, nil, 0, awssig.EmptyPayloadHash)
		if err != nil {
			return nil, fmt.Errorf("list incomplete uploads: %w", err)
		}
		var r listUploadsResult
		if err := xml.Unmarshal(resp.Body, &r); err != nil {
			return nil, fmt.Errorf("list incomplete uploads: %w", err)
		}

		for _, e := range r.Uploads {
			if e.UploadID == "" {
				// Without an id it can be neither priced nor aborted, so it
				// is a hole in the listing rather than an entry in it -- the
				// same reading as a truncated page with no cursor.
				return nil, fmt.Errorf("the store reported an incomplete upload of %q with no upload id, "+
					"so it can be neither measured nor aborted", e.Key)
			}
			u := IncompleteUpload{
				Key:       e.Key,
				UploadID:  e.UploadID,
				Initiated: parseS3Time(e.Initiated),
			}
			n, bytes, err := c.uploadSize(ctx, bucket, e.Key, e.UploadID)
			if err != nil {
				return nil, err
			}
			u.Parts, u.Size = n, bytes
			out = append(out, u)
		}

		// Both markers, and the key marker decides. S3 pages this listing by
		// a pair, and a next page asked for with only one of them re-reads
		// from the top of that key.
		next, err := nextCursor("incomplete uploads", r.IsTruncated, r.NextKeyMarker)
		if err != nil {
			return nil, err
		}
		if next == "" {
			return out, nil
		}
		keyMarker, idMarker = next, r.NextUploadIDMarker
	}
}

// uploadSize sums the parts that arrived.
func (c *Client) uploadSize(ctx context.Context, bucket, key, uploadID string) (parts int, size int64, err error) {
	marker := ""

	for page := 0; ; page++ {
		if page >= maxPages {
			return 0, 0, incompleteListing("the parts of "+key, bucket, parts)
		}
		q := url.Values{"uploadId": {uploadID}, "max-parts": {strconv.Itoa(maxKeys)}}
		if marker != "" {
			q.Set("part-number-marker", marker)
		}

		resp, err := c.do(ctx, http.MethodGet, c.path(bucket, key), q, nil, 0, awssig.EmptyPayloadHash)
		if err != nil {
			return 0, 0, fmt.Errorf("measure the incomplete upload of %s: %w", key, err)
		}
		var r listPartsResult
		if err := xml.Unmarshal(resp.Body, &r); err != nil {
			return 0, 0, fmt.Errorf("measure the incomplete upload of %s: %w", key, err)
		}

		for _, p := range r.Parts {
			parts++
			size += p.Size
		}

		next, err := nextCursor("the parts of "+key, r.IsTruncated, r.NextPartNumberMarker)
		if err != nil {
			return 0, 0, err
		}
		if next == "" {
			return parts, size, nil
		}
		marker = next
	}
}

// Abort removes an incomplete upload and the parts it is storing.
//
// Exported, unlike the unexported abort the multipart path calls from its
// defer, and that is the whole point: the parts of an upload that died with
// its process were unreachable from anywhere in ember. Same operation, two
// callers -- one automatic and one a person typing a command after the
// automatic one did not get to run.
//
// Not found counts as success, as destroying a node or a store does: the goal
// is that the upload is gone, and it is.
func (c *Client) Abort(ctx context.Context, bucket, key, uploadID string) error {
	if uploadID == "" {
		return fmt.Errorf("no upload id to abort")
	}
	if err := c.abort(ctx, bucket, key, uploadID); err != nil {
		var se *StatusError
		if errors.As(err, &se) && se.Code == http.StatusNotFound {
			return nil
		}
		return fmt.Errorf("abort the incomplete upload of %s: %w", key, err)
	}
	return nil
}

// maxKeys is how many entries one page asks for.
//
// S3's own maximum, so the number of round trips is as small as the protocol
// allows. It also bounds a page's size against do's body limit: a thousand
// entries of a few hundred bytes is a few hundred kilobytes.
const maxKeys = 1000

// maxPages bounds every listing loop.
//
// A million objects is far past what a model store holds, and hitting this is
// reported as an incomplete listing rather than returned as a short one --
// because a listing silently missing its tail is exactly the leak these calls
// exist to find. Same rule as ScanStores, which `AGENTS.md` records: a
// truncated listing that reads as complete is worse than an error.
//
// The bound is exercised against ListObjects only. It is the same three lines
// in each of the three loops, and covering the other two would mean three
// thousand round trips through an httptest server to re-check one comparison.
const maxPages = 1000

func incompleteListing(what, bucket string, got int) error {
	return fmt.Errorf("listing %s on %s did not end after %d pages (%d so far); "+
		"the result would be missing its tail, which is the opposite of what this check is for",
		what, bucket, maxPages, got)
}

// nextCursor reads a paginated reply's two halves and insists they agree.
//
// Both are checked because each disagreement is a different bug and both are
// silent. A truncated page with no cursor is a page ember cannot ask for, so
// the tail would be dropped and the listing would read as complete. A cursor
// handed back with truncated false is a page ember would ask for forever.
// This is the rule `ScanStores` already follows against RunPod's own
// pagination, applied to S3's.
func nextCursor(what string, truncated bool, cursor string) (string, error) {
	if !truncated {
		// A cursor on a final page is not an error, it is simply not
		// followed. S3 implementations do populate NextKeyMarker there, and
		// refusing would make a correct store unusable -- while following it
		// would re-read the last page until maxPages.
		return "", nil
	}
	if cursor == "" {
		return "", fmt.Errorf("the store said its listing of %s has more pages and gave nothing to ask for them with, "+
			"so the result would be missing its tail", what)
	}
	return cursor, nil
}

// parseS3Time reads the ISO8601 timestamp S3 puts in a listing.
//
// A zero time on anything unparseable rather than an error. The timestamp is
// for the operator to read; refusing a whole listing -- the listing that
// finds a leak -- because one entry's clock format is unfamiliar would be
// losing the thing that matters to keep the thing that does not.
func parseS3Time(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
		if t, err := time.Parse(layout, s); err == nil {
			return t
		}
	}
	return time.Time{}
}

// bucketPath is /bucket, with no trailing slash.
//
// Separate from path, which always appends a key: a bucket-level listing
// asked for at "/bucket/" is a listing of a store whose name ends in a
// slash, and some endpoints answer it with nothing rather than an error.
func (c *Client) bucketPath(bucket string) string { return "/" + bucket }

type listBucketResult struct {
	XMLName               xml.Name `xml:"ListBucketResult"`
	IsTruncated           bool     `xml:"IsTruncated"`
	NextContinuationToken string   `xml:"NextContinuationToken"`
	Contents              []struct {
		Key          string `xml:"Key"`
		Size         int64  `xml:"Size"`
		LastModified string `xml:"LastModified"`
		ETag         string `xml:"ETag"`
	} `xml:"Contents"`
}

type listUploadsResult struct {
	XMLName            xml.Name `xml:"ListMultipartUploadsResult"`
	IsTruncated        bool     `xml:"IsTruncated"`
	NextKeyMarker      string   `xml:"NextKeyMarker"`
	NextUploadIDMarker string   `xml:"NextUploadIdMarker"`
	Uploads            []struct {
		Key       string `xml:"Key"`
		UploadID  string `xml:"UploadId"`
		Initiated string `xml:"Initiated"`
	} `xml:"Upload"`
}

type listPartsResult struct {
	XMLName              xml.Name `xml:"ListPartsResult"`
	IsTruncated          bool     `xml:"IsTruncated"`
	NextPartNumberMarker string   `xml:"NextPartNumberMarker"`
	Parts                []struct {
		PartNumber int64 `xml:"PartNumber"`
		Size       int64 `xml:"Size"`
	} `xml:"Part"`
}
