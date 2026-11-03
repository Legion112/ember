//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/Legion112/ember/test/integration/harness"
)

// probeFailsConfig is a rental that comes all the way up and then refuses.
//
// A GPU probe that exits non-zero is the cheapest honest post-create failure
// there is: the container is created, sshd is up, ember has dialled it and
// pinned its host key, and only then does prepare return an error. Every
// other failure in that span -- waiting for sshd, the asset verify, the model
// load -- reaches `once` by the same path and leaves the same row, so this
// stands in for all of them and takes seconds rather than a timeout.
//
// It is also the real shape of the thing: a probe that refuses is what
// `gpu_probe` is FOR, and AGENTS.md records a live rental where it fired
// after a 1.6 GB weight fetch.
//
// THE TIMEOUT IS SHORT ON PURPOSE AND THAT IS NOT A SHORTCUT. gpucheck.Wait
// RETRIES until the timeout, because a card that is not up yet and a card
// that will never be up look the same on the first attempt -- so a probe
// which always exits 1 is a wait of exactly gpu_probe_timeout, and the
// probe's own output is buffered and printed only when it finally gives up.
// Five seconds makes the lab fast; the path it drives is identical.
const probeFailsConfig = `
gpu_probe: [sh, -c, "echo this card is not the one the image was built for; exit 1"]
gpu_probe_timeout: 5s
sd:
  serve: [sd-server, --model, /opt/models/sd15.gguf]
  jobs:
    - prompt: placeholder
`

// TestServe_ARentalThatFailsAfterTheNodeExistsDoesNotLeaveTheRequestRunning.
//
// ClaimNext writes `running` and the terminal status is written by drain,
// inside WithServed's closure -- so before this lab, every failure between
// those two points returned with the row still reading `running`, with no
// reason in requests.err, and nothing corrected it: Interrupt sweeps only at
// startup. The operator was left with a request that ran for the life of the
// process against a rental that no longer existed, whose only remedy was
// restarting `ember serve` -- which destroys any OTHER rental in flight.
//
// NO UNIT TEST CAN REACH THIS. It needs a node that really comes up and then
// really refuses, which is a provider, a container and an sshd. The unit
// tests beside it in internal/queue drive the same path through a fake whose
// `rentErr` had never been set by anything; this is the one that proves the
// path is the same one a real rental takes.
func TestServe_ARentalThatFailsAfterTheNodeExistsDoesNotLeaveTheRequestRunning(t *testing.T) {
	l := harness.New(t)
	l.AppendConfig(probeFailsConfig)
	_, srv, _ := startServe(t, l)

	resp := ask(t, "POST", srv, "/api/requests",
		`{"provider":"local","jobs":[{"prompt":"a paper boat","steps":1}]}`)
	if resp.Status != 202 {
		t.Fatalf("queueing failed: %d %s", resp.Status, resp.Body)
	}

	// The node exists by the time the probe speaks, which is what makes this
	// a post-create failure rather than a planning one.
	ctx, cancel := context.WithTimeout(t.Context(), 120*time.Second)
	defer cancel()
	l.WaitForLine(ctx, "node ready")
	// gpucheck's own refusal, which carries the probe's output with it.
	l.WaitForLine(ctx, "the GPU never became usable")

	// Then the row. Polled rather than read once, because the status is
	// written as the rental unwinds and the deferred destroy runs first.
	var got request
	deadline := time.Now().Add(60 * time.Second)
	for {
		got = oneRequest(t, srv)
		if got.Status != "running" && got.Status != "queued" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the request still reads %q a minute after its rental failed; "+
				"nothing but a restart of ember serve will clear it", got.Status)
		}
		time.Sleep(500 * time.Millisecond)
	}

	if got.Status != "failed" {
		t.Errorf("the request ended as %q, want failed", got.Status)
	}
	// AND IT SAYS WHY. This is the half an operator needs: the failures in
	// this span are the expensive ones -- a create refused, a card that will
	// not initialise, weights whose digest did not match -- and all of them
	// happen after money has been committed.
	if got.Error == "" {
		t.Error("the request carries no reason, so the most expensive failures ember has " +
			"are the ones it explains least")
	}

	// The node is gone either way: a rental that fails still unwinds through
	// the deferred destroy. Asserted here because a terminal row over a node
	// that is still billing would be worse than the bug being fixed.
	if nodes := l.Nodes(t.Context()); len(nodes) != 0 {
		t.Errorf("%d node(s) survived the failed rental", len(nodes))
	}
}

// request is the part of the API's listing this lab reads.
type request struct {
	ID     string `json:"id"`
	Status string `json:"status"`
	Error  string `json:"error"`
}

func oneRequest(t *testing.T, srv served) request {
	t.Helper()
	resp := ask(t, "GET", srv, "/api/requests", "")
	if resp.Status != 200 {
		t.Fatalf("listing requests: %d %s", resp.Status, resp.Body)
	}
	var list struct {
		Requests []request `json:"requests"`
	}
	if err := json.Unmarshal(resp.Body, &list); err != nil {
		t.Fatalf("decode %s: %v", strings.TrimSpace(string(resp.Body)), err)
	}
	if len(list.Requests) != 1 {
		t.Fatalf("want exactly one request, got %d: %s", len(list.Requests), resp.Body)
	}
	return list.Requests[0]
}
