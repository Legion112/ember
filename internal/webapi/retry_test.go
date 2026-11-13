package webapi

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/Legion112/ember/internal/genstore"
	"github.com/Legion112/ember/internal/report"
)

// TestRetry_RequeuesAFailedRequestAndRecordsThatItDid.
//
// THE ROW IS REUSED, WHICH IS WHY THE EVENT IS NOT OPTIONAL. A retry could
// have been a new request, leaving the failure in the ledger untouched; one
// row was chosen so the queue does not fill with retries of the same work, and
// that makes the row's status, error and node things this overwrites. The
// event stream only appends and is the truth about what happened -- without
// the KindRequeued, a request that failed, was retried and then succeeded
// reads as one that simply succeeded.
func TestRetry_RequeuesAFailedRequestAndRecordsThatItDid(t *testing.T) {
	h := newHarness(t)

	id := genstore.ReqID(decode[map[string]string](t,
		h.do(t, "POST", "/api/requests", oneJob))["id"])
	if err := h.store.Update(t.Context(), id, func(q *genstore.Request) {
		q.Status = genstore.StatusFailed
		q.Err = "no store named models at local"
	}); err != nil {
		t.Fatal(err)
	}

	res := h.do(t, "POST", "/api/requests/"+string(id)+"/retry", "")
	if res.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", res.Code, res.Body.String())
	}

	got, err := h.store.Get(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != genstore.StatusQueued {
		t.Errorf("status = %q, want queued", got.Status)
	}
	if got.Err != "" {
		t.Errorf("err = %q, want cleared: the row reads queued now", got.Err)
	}

	evs, err := h.store.Events(t.Context(), id, 0)
	if err != nil {
		t.Fatal(err)
	}
	var requeued *report.Event
	for i := range evs {
		if evs[i].Kind == report.KindRequeued {
			requeued = &evs[i]
		}
	}
	if requeued == nil {
		t.Fatalf("no requeued event, so this request's history will read as one that never "+
			"failed: %+v", evs)
	}
	// THE STATUS IT REPLACED IS IN THE TEXT, because "requeued" alone does not
	// say what was there, and a failure, a cancellation and an interruption
	// are three different reasons to be reading that line.
	if requeued.Text != string(genstore.StatusFailed) {
		t.Errorf("the event says it was requeued from %q, want failed", requeued.Text)
	}
}

// TestRetry_RefusesARequestThatIsNotTerminal.
//
// 409 AND NOT 400, which is the distinction the status code carries: the
// request is well formed and names a real row, and what is wrong is the row's
// state. `done` is the case worth naming -- it is the row an operator is most
// likely to press by accident, and a store rule of "not queued or running"
// rather than the three terminal names would have let it through.
func TestRetry_RefusesARequestThatIsNotTerminal(t *testing.T) {
	h := newHarness(t)

	id := genstore.ReqID(decode[map[string]string](t,
		h.do(t, "POST", "/api/requests", oneJob))["id"])
	if err := h.store.Update(t.Context(), id, func(q *genstore.Request) {
		q.Status = genstore.StatusDone
	}); err != nil {
		t.Fatal(err)
	}

	res := h.do(t, "POST", "/api/requests/"+string(id)+"/retry", "")
	if res.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409: %s", res.Code, res.Body.String())
	}
	var e struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal(res.Body.Bytes(), &e)
	if e.Error == "" {
		t.Error("the refusal carries no sentence, so the page can say only that it failed")
	}

	got, err := h.store.Get(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != genstore.StatusDone {
		t.Errorf("status became %q, so the row was written anyway", got.Status)
	}
}
