//go:build integration

package integration

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Legion112/ember/internal/awssig"
	"github.com/Legion112/ember/internal/node"
	"github.com/Legion112/ember/internal/provider"
	"github.com/Legion112/ember/internal/s3"
	"github.com/Legion112/ember/test/integration/runpodfake"
)

// The checks on the fake itself, in the shape TestRunPodFake_RefusesAnOversizedCPUDisk
// already uses.
//
// The lab above proves something only while the service it runs against would
// have said no. A fake that accepted the other API's field name, or a size the
// server refuses, or an unsigned request would turn every one of those
// assertions into an assertion about a value ember chose for reasons nothing
// enforces -- which is the defect the DELETE of an already-deleted pod had,
// answering 200 where the live API 404s and letting destroy-is-idempotent pass
// with the not-found path never taken.
//
// They run against a fake with no data plane, because a regression that made
// one of these succeed must not start a container from a lab that is already
// failing.

// TestRunPodFake_RefusesAVolumeSpelledTheOtherAPIsWay.
//
// RunPod serves network volumes at two hosts and the same field is
// `dataCenter` on one and `dataCenterId` on the other -- and the published
// documentation describes only the second. A create against v2 carrying the v1
// spelling is refused with a 422 naming both halves of the mistake, which is
// what a client written from the docs discovers on its first create, after the
// operator has decided to spend money. Reproduced because that disagreement
// stayed invisible for as long as the only account of the shape was a fixture.
//
// The acceptance is asserted beside the refusal on purpose: without it, a fake
// that refused every create there is would satisfy the first half.
func TestRunPodFake_RefusesAVolumeSpelledTheOtherAPIsWay(t *testing.T) {
	f := runpodfake.New(t, nil, runpodfake.Options{})

	code, body := fakeCall(t, http.MethodPost, f.URL+"/v2/network-volumes",
		`{"name":"ember-store-models","size":40,"dataCenterId":"EU-RO-1","type":"STANDARD"}`)
	if code != http.StatusUnprocessableEntity {
		t.Errorf("a create with the v1 field name got status %d, want 422", code)
	}
	for _, want := range []string{
		"missing property 'dataCenter'",
		"additional properties 'dataCenterId' not allowed",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the refusal does not carry the API's own %q: %s", want, body)
		}
	}

	// And the v2 spelling is accepted, and comes back spelled the same way.
	code, body = fakeCall(t, http.MethodPost, f.URL+"/v2/network-volumes",
		`{"name":"ember-store-models","size":40,"dataCenter":"EU-RO-1","type":"STANDARD"}`)
	if code != http.StatusOK {
		t.Fatalf("a create with the v2 field name got status %d (%s), want 200", code, body)
	}
	if !strings.Contains(body, `"dataCenter"`) {
		t.Errorf("the created volume does not report dataCenter: %s", body)
	}
	if strings.Contains(body, `"dataCenterId"`) {
		t.Errorf("the created volume reports the other API's dataCenterId: %s", body)
	}
}

// TestRunPodFake_RefusesAVolumeBelowTheMeasuredMinimum.
//
// The documented minimum is 0 and the server's is 10: `size: 1` is refused
// with `"$.size: minimum: got 1, want 10"`. `node.MinStoreGiB` holds the
// measured pair so that a size ember will refuse locally costs nothing instead
// of a round trip -- and a fake that accepted 1 would let that local refusal
// be removed with nothing going red.
func TestRunPodFake_RefusesAVolumeBelowTheMeasuredMinimum(t *testing.T) {
	f := runpodfake.New(t, nil, runpodfake.Options{})

	post := func(t *testing.T, size int) (int, string) {
		t.Helper()
		return fakeCall(t, http.MethodPost, f.URL+"/v2/network-volumes",
			`{"name":"ember-store-models","size":`+strconv.Itoa(size)+`,"dataCenter":"EU-RO-1","type":"STANDARD"}`)
	}

	code, body := post(t, 1)
	if code != http.StatusUnprocessableEntity {
		t.Errorf("a 1 GiB volume got status %d, want 422", code)
	}
	if !strings.Contains(body, "minimum: got 1, want 10") {
		t.Errorf("the refusal does not carry the API's own message: %s", body)
	}

	// At the bound it is accepted, or ember's own refusal would be tested
	// against a service that rejects the smallest store it will sell.
	if code, body := post(t, node.MinStoreGiB); code != http.StatusOK {
		t.Errorf("a %d GiB volume got status %d (%s), want 200", node.MinStoreGiB, code, body)
	}
	// And the other end of the same measured pair.
	if code, body := post(t, node.MaxStoreGiB+1); code != http.StatusUnprocessableEntity {
		t.Errorf("a %d GiB volume got status %d (%s), want 422", node.MaxStoreGiB+1, code, body)
	}
}

// TestRunPodFake_AStoreRoundTripsThroughTheProviderAndTheLabelSurvives.
//
// `ember store create`, `ls` and `rm` against RunPod had never run in a lab,
// because this package served no volume route: a request for it 404'd and
// seven subcommands were exercised only by one-off httptest handlers -- the
// canned handlers this package's own doc comment was written to replace.
//
// The label is the assertion with money in it. `StoreCreate` refuses a volume
// whose name does not come back as ember's own, because a store ember cannot
// recognise is one `ember ls` will not report and `ember store rm` will not
// remove: a leak created by the command meant to be the careful one.
func TestRunPodFake_AStoreRoundTripsThroughTheProviderAndTheLabelSurvives(t *testing.T) {
	p, f, _ := runpodLab(t, runpodfake.Options{})
	ctx := t.Context()

	// Statically, because the provider declares the interface: a type
	// assertion here would be a runtime check for something the compiler
	// already refuses to let drift.
	var sa provider.StoreAPI = p

	st, err := sa.StoreCreate(ctx, "models", 40, runpodfake.VolumeDataCenter)
	if err != nil {
		t.Fatalf("store create: %v", err)
	}
	if st.Name != "models" {
		t.Errorf("the created store is named %q; the label did not round-trip", st.Name)
	}
	if st.Region != runpodfake.VolumeDataCenter {
		t.Errorf("the created store reports region %q, want %q -- a store with the wrong site narrows the offer filter to the wrong place",
			st.Region, runpodfake.VolumeDataCenter)
	}
	if st.SizeGiB != 40 {
		t.Errorf("the created store is %d GiB, want 40", st.SizeGiB)
	}

	ls, err := sa.StoreList(ctx)
	if err != nil {
		t.Fatalf("store list: %v", err)
	}
	if len(ls) != 1 || ls[0].Handle != st.Handle {
		t.Fatalf("the listing is %+v, want exactly the store just created (%s)", ls, st.Handle)
	}

	if err := sa.StoreDestroy(ctx, st.Handle); err != nil {
		t.Fatalf("store rm: %v", err)
	}
	if ls, err := sa.StoreList(ctx); err != nil || len(ls) != 0 {
		t.Fatalf("after removal the listing is %+v (err %v)", ls, err)
	}
	// Removing it twice is success, as destroying a node twice is: the caller
	// asked for the store to be gone and it is. The fake 404s the second
	// DELETE, so this is the not-found path being taken rather than a handler
	// that answers 200 whatever it is handed.
	if err := sa.StoreDestroy(ctx, st.Handle); err != nil {
		t.Errorf("removing an already-removed store is an error: %v", err)
	}
	if vols := f.Volumes(); len(vols) != 0 {
		t.Errorf("%d volume(s) survived their own removal: %v", len(vols), vols)
	}
}

// TestRunPodFake_TheStoreEndpointVerifiesEverySignature.
//
// A fake that accepted an unsigned request cannot catch a signing regression,
// and SigV4's only other feedback is a 403 with no detail -- which is why
// every step of `internal/awssig` is checked against AWS's published values
// rather than against a server. This is the other half: the one place in this
// tree where ember's signature is checked by something that recomputes it.
//
// Four cases, and the acceptance is one of them: a refusal alone is consistent
// with an endpoint that refuses everything, which is exactly what a verifier
// with a bug in its canonical request would be.
func TestRunPodFake_TheStoreEndpointVerifiesEverySignature(t *testing.T) {
	f := runpodfake.New(t, nil, runpodfake.Options{})
	ctx := t.Context()

	code, body := fakeCall(t, http.MethodPost, f.URL+"/v2/network-volumes",
		`{"name":"ember-store-models","size":40,"dataCenter":"`+runpodfake.VolumeDataCenter+`","type":"STANDARD"}`)
	if code != http.StatusOK {
		t.Fatalf("create a volume to sign against: %d %s", code, body)
	}
	bucket := f.Volumes()[0]

	// 1. Correctly signed, and the object is simply not there: found=false and
	// NO error, because "the asset is not on the store" is the answer
	// `ember store verify` exists to get.
	ok := &s3.Client{Endpoint: f.S3URL, Region: runpodfake.VolumeDataCenter, Creds: runpodfake.S3Credentials()}
	if _, found, err := ok.Head(ctx, bucket, "absent.gguf"); err != nil || found {
		t.Fatalf("HEAD of an absent object: found=%v err=%v, want false and no error", found, err)
	}

	// 2. And a PUT lands, read back by a HEAD through the same endpoint. The
	// acceptance, without which every refusal below proves nothing.
	payload := []byte("these are the weights")
	if err := ok.Upload(ctx, bucket, "sd15.gguf", bytes.NewReader(payload), int64(len(payload)), nil); err != nil {
		t.Fatalf("upload: %v", err)
	}
	o, found, err := ok.Head(ctx, bucket, "sd15.gguf")
	if err != nil || !found {
		t.Fatalf("HEAD after a PUT: found=%v err=%v", found, err)
	}
	if o.Size != int64(len(payload)) {
		t.Errorf("the endpoint reports %d bytes, want %d", o.Size, len(payload))
	}

	// 3. The wrong secret is a 403. A fake that waved this through would let
	// the signing key, the scope or the header set change with nothing going
	// red.
	wrong := &s3.Client{
		Endpoint: f.S3URL, Region: runpodfake.VolumeDataCenter,
		Creds: awssig.Credentials{AccessKey: runpodfake.S3AccessKey, Secret: node.NewSecret([]byte("not-the-secret"))},
	}
	if _, _, err := wrong.Head(ctx, bucket, "sd15.gguf"); !is403(err) {
		t.Errorf("a request signed with the wrong secret got %v, want a 403", err)
	}

	// 4. The wrong REGION is a 403 as well, which is the point of the scope: a
	// signing key that leaks is useless in another region only if somebody
	// checks. The endpoint reads the region off the volume's own datacentre
	// and never off the caller's credential scope.
	elsewhere := &s3.Client{Endpoint: f.S3URL, Region: "US-IL-1", Creds: runpodfake.S3Credentials()}
	if _, _, err := elsewhere.Head(ctx, bucket, "sd15.gguf"); !is403(err) {
		t.Errorf("a request signed for another datacentre got %v, want a 403", err)
	}

	// 5. Unsigned entirely. There is no ember client that can send this, which
	// is why it is a raw request: the refusal has to be the endpoint's and not
	// the signer's.
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, f.S3URL+"/"+bucket+"/sd15.gguf", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("an unsigned request got status %d, want 403", resp.StatusCode)
	}
}

// TestRunPodFake_TheStoreEndpointRefusesABodyThatDisagreesWithItsOwnDigest.
//
// The signature covers `X-Amz-Content-Sha256` and not the bytes, so a
// correctly signed request can still carry a body that is not the one it
// claims -- which is what a truncated upload is. The endpoint hashes what
// arrived, because a store that can be written and not read back hides exactly
// that: an object whose key exists and whose bytes are short loads as a model
// under `verify: present`.
//
// Signed by hand, because no ember client can produce this: `internal/s3`
// computes the digest from the body it is about to send, so the disagreement
// has to be constructed.
func TestRunPodFake_TheStoreEndpointRefusesABodyThatDisagreesWithItsOwnDigest(t *testing.T) {
	f := runpodfake.New(t, nil, runpodfake.Options{})
	ctx := t.Context()

	if code, body := fakeCall(t, http.MethodPost, f.URL+"/v2/network-volumes",
		`{"name":"ember-store-models","size":40,"dataCenter":"`+runpodfake.VolumeDataCenter+`","type":"STANDARD"}`); code != http.StatusOK {
		t.Fatalf("create a volume to sign against: %d %s", code, body)
	}
	bucket := f.Volumes()[0]

	claimed := sha256.Sum256([]byte("what the request says it carries"))
	hash := hex.EncodeToString(claimed[:])
	sent := []byte("what it actually carries")

	req, err := http.NewRequestWithContext(ctx, http.MethodPut,
		f.S3URL+"/"+bucket+"/sd15.gguf", bytes.NewReader(sent))
	if err != nil {
		t.Fatal(err)
	}
	req.ContentLength = int64(len(sent))
	req.Header.Set("X-Amz-Content-Sha256", hash)
	if err := awssig.Sign(req, runpodfake.S3Credentials(), runpodfake.VolumeDataCenter, s3.Service, hash, time.Now()); err != nil {
		t.Fatal(err)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("a body that disagrees with its own digest got status %d, want 403", resp.StatusCode)
	}

	// And nothing was written, which is the half that matters: a store holding
	// a short file reports it as present.
	ok := &s3.Client{Endpoint: f.S3URL, Region: runpodfake.VolumeDataCenter, Creds: runpodfake.S3Credentials()}
	if _, found, err := ok.Head(ctx, bucket, "sd15.gguf"); err != nil || found {
		t.Errorf("after a refused PUT the object is found=%v (err %v); a refusal that still wrote is worse than none", found, err)
	}
}

// is403 reports whether err is the endpoint refusing.
func is403(err error) bool {
	var se *s3.StatusError
	return errors.As(err, &se) && se.Code == http.StatusForbidden
}
