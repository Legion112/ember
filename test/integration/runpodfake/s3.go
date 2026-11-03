package runpodfake

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/Legion112/ember/internal/awssig"
	"github.com/Legion112/ember/internal/node"
	"github.com/Legion112/ember/internal/s3"
)

// The S3-compatible half, over the same directories the volume route made.
//
// WHY IT IS HERE AND NOT A ONE-OFF HANDLER. `internal/cli` and
// `internal/queue` already stand a store up with an httptest handler that
// answers a size for a key it was handed in a map. That is enough to test a
// decision and cannot test the thing the store pre-flight turns on, which is a
// JOIN: `ember store sync` fills the store by running `embernode fetch` on a
// rented machine, over SSH, into a mounted directory -- and the next plan asks
// the store's own endpoint whether the weights are there. Two maps agree with
// each other whatever either half does. One directory, written by a real
// container and read by this endpoint, is one fact.
//
// SIGV4 IS VERIFIED AND NOT WAVED THROUGH, and the verification re-signs
// rather than re-implementing. `awssig.Sign` is called on a reconstruction of
// the incoming request and the whole Authorization header is compared, which
// checks the access key, the scope, the signed-header list and the signature in
// one string equality -- and means this endpoint cannot come to disagree with
// the signer about any of the four steps. A fake that accepted an unsigned
// request could not catch a signing regression, and the only other feedback
// SigV4 has is a 403 with no detail.
//
// THE RECONSTRUCTION IS FILTERED BY SignedHeaders, which is what a real
// server does and the one asymmetry worth naming: Go's transport adds
// `Accept-Encoding: gzip` after the client has signed, so a verifier that
// hashed every header it received would reject every correct request.
//
// WHAT IS NOT SERVED. There is no object GET, because `internal/s3` has no
// download -- a node fetches its own assets over plain HTTP, verified by
// digest. There is no multipart, because ember only multiparts above 400 MB
// and moving that through an httptest server to exercise a sequence the
// package's own tests already drive with PartSize overridden is a bad trade.
// `ListMultipartUploads` is served and answers an empty listing, which is the
// truthful answer for a store nothing has ever multiparted and is what
// `ember store ls` reads. Anything else is a 501 naming the verb, rather than
// a 404 that would reach a caller as "the object is not there".

// S3 credentials the endpoint accepts by default.
//
// RunPod issues these separately from the API key, on a different page of the
// console, which is why `CheckStoreAssets` treats their absence as a skip
// rather than a refusal -- and why a lab that wants the pre-flight to run has
// to supply a pair. These are the pair, so a lab and this endpoint cannot
// disagree about them by each writing their own literal.
//
//nolint:gosec // G101: a lab's own stand-in key pair, accepted only by this in-process httptest server.
const (
	S3AccessKey = "user_runpodfake"
	S3Secret    = "rps_runpodfake_secret"
)

// S3Credentials is the pair above, in the shape the signer and the config take.
func S3Credentials() awssig.Credentials {
	return awssig.Credentials{AccessKey: S3AccessKey, Secret: node.NewSecret([]byte(S3Secret))}
}

// maxObject bounds what this endpoint will accept in one PUT.
//
// Generous for a lab and far below anything that would matter: every asset a
// lab here pushes is a sentence. A bound at all, because a fake that buffered
// whatever arrived would turn a client bug into an out-of-memory kill rather
// than an error naming the size.
const maxObject = 64 << 20

// routeS3 is the S3 endpoint's whole dispatch.
func (f *Fake) routeS3(w http.ResponseWriter, r *http.Request) {
	bucket, key := splitBucketKey(r.URL.Path)
	if bucket == "" {
		f.s3Error(w, http.StatusBadRequest, "InvalidRequest", "no bucket in the path")
		return
	}

	f.mu.Lock()
	v := f.volumes[bucket]
	f.mu.Unlock()
	if v == nil {
		// A bucket is a volume id here, as it is on RunPod: the endpoint is
		// per datacentre and the store's own id names the bucket.
		f.s3Error(w, http.StatusNotFound, "NoSuchBucket", "no network volume "+bucket)
		return
	}

	body, err := f.verify(r, v)
	if err != nil {
		// SignatureDoesNotMatch and AccessDenied are both 403 and the reason
		// is in the body, which is why internal/s3's StatusError carries it.
		f.s3Error(w, http.StatusForbidden, "SignatureDoesNotMatch", err.Error())
		return
	}

	q := r.URL.Query()
	switch {
	case r.Method == http.MethodHead && key != "":
		f.headObject(w, v, key)
	case r.Method == http.MethodPut && key != "":
		f.putObject(w, v, key, body)
	case r.Method == http.MethodGet && key == "" && q.Get("list-type") == "2":
		f.listObjects(w, v, q)
	case r.Method == http.MethodGet && key == "" && q.Has("uploads"):
		f.listUploads(w, v)
	default:
		f.s3Error(w, http.StatusNotImplemented, "NotImplemented",
			fmt.Sprintf("%s %s is not a call ember makes, so this fake does not serve it", r.Method, r.URL.RequestURI()))
	}
}

// verify checks the signature and returns the body it read.
//
// The body is read here because the signature covers its digest: a verifier
// that let the handler read it would have to be trusted to check the hash, and
// there would then be two places that know what X-Amz-Content-Sha256 means.
func (f *Fake) verify(r *http.Request, v *volume) ([]byte, error) {
	auth := r.Header.Get("Authorization")
	if auth == "" {
		return nil, fmt.Errorf("the request carries no Authorization header")
	}
	signed, err := signedHeadersOf(auth)
	if err != nil {
		return nil, err
	}

	stamp := r.Header.Get("X-Amz-Date")
	at, err := time.Parse("20060102T150405Z", stamp)
	if err != nil {
		return nil, fmt.Errorf("X-Amz-Date %q is not a SigV4 timestamp: %w", stamp, err)
	}

	claimed := r.Header.Get("X-Amz-Content-Sha256")
	if claimed == "" {
		// S3 requires the header and the client sets it. It is not part of
		// SigV4 -- awssig deliberately does not set it, so that package can be
		// checked against AWS's published vectors -- so the refusal is the
		// service's rather than the signer's.
		return nil, fmt.Errorf("the request carries no X-Amz-Content-Sha256")
	}

	var body []byte
	if r.Body != nil {
		// One byte past the bound, so "too large" and "exactly the bound" are
		// different answers, which is the rule internal/s3's own `do` follows
		// in the other direction.
		body, err = io.ReadAll(io.LimitReader(r.Body, maxObject+1))
		if err != nil {
			return nil, fmt.Errorf("read the body: %w", err)
		}
		if int64(len(body)) > maxObject {
			return nil, fmt.Errorf("the body is larger than %d bytes", int64(maxObject))
		}
	}
	if len(body) > 0 {
		sum := sha256.Sum256(body)
		if got := hex.EncodeToString(sum[:]); got != claimed {
			// AWS's own code for this. Not observed against RunPod -- nothing
			// here has sent a body that disagreed with its own digest -- and
			// it is the check that would catch a truncated upload, which is
			// the failure a store that can be written and not read hides.
			return nil, fmt.Errorf("XAmzContentSHA256Mismatch: the body hashes to %s and the header claims %s", got, claimed)
		}
	}

	// Rebuilt with only the headers the client said it signed, and with Host
	// kept where Go keeps it: awssig.canonicalHeaders reads req.Host rather
	// than the map, because that is where the standard library puts it.
	probe := &http.Request{
		Method: r.Method,
		URL:    r.URL,
		Host:   r.Host,
		Header: http.Header{},
	}
	for _, name := range signed {
		if name == "host" {
			continue
		}
		for _, val := range r.Header.Values(name) {
			probe.Header.Add(name, val)
		}
	}

	// The region is the VOLUME's datacentre and never the scope the caller
	// claimed. Reading it out of the Credential would make a signature valid
	// for whatever region it was computed in, which is the one thing the scope
	// exists to prevent: a signing key that leaks is useless in another region
	// only if somebody checks.
	creds := f.opts.S3
	if creds.IsZero() {
		creds = S3Credentials()
	}
	if err := awssig.Sign(probe, creds, v.DataCenter, s3.Service, claimed, at); err != nil {
		return nil, fmt.Errorf("re-sign the request to compare: %w", err)
	}
	if probe.Header.Get("Authorization") != auth {
		return nil, fmt.Errorf("the signature does not match what this endpoint computes over the request")
	}
	return body, nil
}

// signedHeadersOf reads the SignedHeaders list out of an Authorization header.
//
// Parsed rather than grepped: the header is three comma-separated fields and
// taking the one named SignedHeaders is not the same as taking the text after
// the second comma, which is what a split would do the day AWS adds a fourth.
func signedHeadersOf(auth string) ([]string, error) {
	if !strings.HasPrefix(auth, awssig.Algorithm+" ") {
		return nil, fmt.Errorf("the Authorization header does not name %s", awssig.Algorithm)
	}
	for _, field := range strings.Split(strings.TrimPrefix(auth, awssig.Algorithm+" "), ",") {
		field = strings.TrimSpace(field)
		if rest, ok := strings.CutPrefix(field, "SignedHeaders="); ok {
			names := strings.Split(rest, ";")
			if len(names) == 0 || names[0] == "" {
				return nil, errors.New("the Authorization header's SignedHeaders list is empty")
			}
			return names, nil
		}
	}
	return nil, errors.New("the Authorization header names no SignedHeaders")
}

// headObject answers HEAD, which is the one call the store pre-flight makes.
//
// A missing object is a 404 and nothing else is: `s3.Head` reports found=false
// only for that status, because a 403 means the credentials cannot see the
// store and reporting it as an absence would send a run off to re-download
// forty gigabytes over a permissions problem.
func (f *Fake) headObject(w http.ResponseWriter, v *volume, key string) {
	fi, err := statIn(v.Dir, key)
	if err != nil {
		f.s3Error(w, http.StatusNotFound, "NoSuchKey", "no object "+key)
		return
	}
	w.Header().Set("Content-Length", strconv.FormatInt(fi.Size(), 10))
	w.Header().Set("Last-Modified", fi.ModTime().UTC().Format(http.TimeFormat))
	// OMITTED rather than sent empty when there is no tag to compute, because
	// `""` is a value that looks like a fact: `normalizeETag` strips the
	// quotes and the caller cannot tell it from a store that reports none.
	if tag := etagOf(v.Dir, key); tag != "" {
		w.Header().Set("ETag", `"`+tag+`"`)
	}
	w.WriteHeader(http.StatusOK)
}

// putObject answers a single PUT, which is what `ember store push` sends for
// anything under 400 MB.
func (f *Fake) putObject(w http.ResponseWriter, v *volume, key string, body []byte) {
	root, err := os.OpenRoot(v.Dir)
	if err != nil {
		f.s3Error(w, http.StatusInternalServerError, "InternalError", err.Error())
		return
	}
	defer func() { _ = root.Close() }()

	// Containment by the one mechanism that cannot be spelled wrong. The key
	// comes from ember rather than from a stranger, so this is not the tar
	// extractor's two-mechanism case -- but a fake that wrote outside its own
	// directory would be a lab writing over the checkout.
	if dir := pathDir(key); dir != "" {
		if err := root.MkdirAll(dir, 0o755); err != nil { //nolint:gosec // a lab's own scratch directory
			f.s3Error(w, http.StatusBadRequest, "InvalidKey", err.Error())
			return
		}
	}
	fh, err := root.Create(key)
	if err != nil {
		f.s3Error(w, http.StatusBadRequest, "InvalidKey", err.Error())
		return
	}
	if _, err := fh.Write(body); err != nil {
		_ = fh.Close()
		f.s3Error(w, http.StatusInternalServerError, "InternalError", err.Error())
		return
	}
	if err := fh.Close(); err != nil {
		f.s3Error(w, http.StatusInternalServerError, "InternalError", err.Error())
		return
	}

	// The ETag goes in the header and the body stays empty, which is what AWS
	// does for a single PUT. internal/s3 reads the body first and the header
	// second for a *part*, because RunPod sits behind a proxy that rewrites
	// the header into a weak validator -- that is a multipart concern and
	// there is no multipart here.
	w.Header().Set("ETag", `"`+etagOf(v.Dir, key)+`"`)
	w.WriteHeader(http.StatusOK)
}

// listObjects answers ListObjectsV2, with the prefix, the page size and the
// continuation token all honoured.
//
// The cursor halves are the point of honouring max-keys: `IsTruncated` with no
// `NextContinuationToken` is a page ember cannot ask for, and a token on a
// final page is one it would re-read forever. `internal/s3` refuses the first
// and ignores the second, and a fake that answered one page whatever was asked
// would leave both readings untested.
func (f *Fake) listObjects(w http.ResponseWriter, v *volume, q url.Values) {
	keys, err := v.objectKeys()
	if err != nil {
		f.s3Error(w, http.StatusInternalServerError, "InternalError", err.Error())
		return
	}

	prefix := q.Get("prefix")
	after := q.Get("continuation-token")
	limit := 1000
	if n, err := strconv.Atoi(q.Get("max-keys")); err == nil && n > 0 {
		limit = n
	}

	res := listBucketResult{Name: v.ID, Prefix: prefix, MaxKeys: limit}
	for _, k := range keys {
		if prefix != "" && !strings.HasPrefix(k, prefix) {
			continue
		}
		if after != "" && k <= after {
			continue
		}
		if len(res.Contents) == limit {
			res.IsTruncated = true
			res.NextContinuationToken = res.Contents[len(res.Contents)-1].Key
			break
		}
		fi, err := statIn(v.Dir, k)
		if err != nil {
			continue
		}
		e := listEntry{Key: k, Size: fi.Size(), LastModified: s3Time(fi.ModTime())}
		if tag := etagOf(v.Dir, k); tag != "" {
			e.ETag = `"` + tag + `"`
		}
		res.Contents = append(res.Contents, e)
	}
	res.KeyCount = len(res.Contents)
	f.s3XML(w, res)
}

// listUploads answers ListMultipartUploads with an empty listing.
//
// Served rather than left to 404 because `ember store ls` asks it on every
// run: the leak it looks for -- an upload that died with its process and is
// billing for parts no listing shows -- is the one money leak in this
// repository with no other detection path. An empty answer is the true one
// here, and a 404 would reach the caller as a failure to look.
func (f *Fake) listUploads(w http.ResponseWriter, v *volume) {
	f.s3XML(w, listUploadsResult{Bucket: v.ID})
}

// s3XML writes one of the listing documents.
func (f *Fake) s3XML(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/xml")
	_, _ = w.Write([]byte(xml.Header))
	if err := xml.NewEncoder(w).Encode(v); err != nil {
		f.t.Errorf("encode an S3 listing: %v", err)
	}
}

// s3Error writes S3's own error document.
//
// XML with a Code and a Message, because that is what `s3.StatusError` carries
// back to the operator: the status alone cannot tell SignatureDoesNotMatch
// from AccessDenied, since both are 403.
func (f *Fake) s3Error(w http.ResponseWriter, code int, s3code, msg string) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(code)
	_, _ = w.Write([]byte(xml.Header))
	_ = xml.NewEncoder(w).Encode(s3ErrorDoc{Code: s3code, Message: msg})
}

type s3ErrorDoc struct {
	XMLName xml.Name `xml:"Error"`
	Code    string   `xml:"Code"`
	Message string   `xml:"Message"`
}

// The listing documents, in the element names internal/s3's parser reads.
//
// A second copy of a field shape, which this package avoids for the catalogue
// by serving the provider's own fixtures -- and there is no captured S3
// listing to serve. What makes that tolerable is the failure mode: a
// misspelled element decodes to an empty listing, so every lab that reads one
// asserts a non-empty result positively rather than only that no error came
// back.
type listBucketResult struct {
	XMLName               xml.Name    `xml:"ListBucketResult"`
	Name                  string      `xml:"Name"`
	Prefix                string      `xml:"Prefix"`
	MaxKeys               int         `xml:"MaxKeys"`
	KeyCount              int         `xml:"KeyCount"`
	IsTruncated           bool        `xml:"IsTruncated"`
	NextContinuationToken string      `xml:"NextContinuationToken,omitempty"`
	Contents              []listEntry `xml:"Contents"`
}

type listEntry struct {
	Key          string `xml:"Key"`
	Size         int64  `xml:"Size"`
	LastModified string `xml:"LastModified"`
	ETag         string `xml:"ETag,omitempty"`
}

type listUploadsResult struct {
	XMLName     xml.Name `xml:"ListMultipartUploadsResult"`
	Bucket      string   `xml:"Bucket"`
	IsTruncated bool     `xml:"IsTruncated"`
}

// statIn stats one key under dir, through an os.Root so a key cannot escape.
func statIn(dir, key string) (os.FileInfo, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()
	return root.Stat(key)
}

// etagOf is the MD5-shaped validator S3 reports.
//
// A SHA-256 prefix rather than MD5, and that is safe precisely because ember
// never treats an ETag as a digest: `s3.Object.ETag`'s own comment says so --
// a single-part object's tag is an MD5 and a multipart one's is an MD5 of
// MD5s with a part count on the end, while ember's `assets:` digests are
// SHA-256, so comparing them would be a check that always fails. What the tag
// has to be is stable for the same bytes, which this is.
func etagOf(dir, key string) string {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return ""
	}
	defer func() { _ = root.Close() }()
	fh, err := root.Open(key)
	if err != nil {
		return ""
	}
	defer func() { _ = fh.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, fh); err != nil {
		return ""
	}
	return hex.EncodeToString(h.Sum(nil))[:32]
}

// pathDir is the directory part of a slash-separated key, or empty.
func pathDir(key string) string {
	if i := strings.LastIndexByte(key, '/'); i > 0 {
		return key[:i]
	}
	return ""
}
