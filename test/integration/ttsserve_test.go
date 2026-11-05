//go:build integration && imagelab

// Speech through `ember serve`: the browser half of the feature, server-side.
//
// Behind the imagelab tag for the reason tts_test.go is -- it needs ember:tts.
//
//	make workload-tts
//	make test-tts-lab
package integration

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/Legion112/ember/test/integration/harness"
)

// TestTTS_ServeQueuesAnUtteranceAndReturnsAPlayableWav is the whole browser
// half of the feature, server-side, on the free provider.
//
// WHAT NO UNIT TEST CROSSES. Six things are each correct in isolation and the
// question is only whether they are joined: `GET /api/config` publishes a
// voice id, `POST /api/requests` stores an ask carrying that id and no path,
// the claim honours the kind, the drain loop opens a speech rental rather than
// an sd one, the runner resolves the id against the catalogue the process was
// started with, and the artifact row is keyed `.wav` so the route declares
// audio. Every one of those is a seam, and this repository's record is that a
// feature built across seams and never joined passes every test it has.
//
// IT GOES THROUGH `ember serve` AND NOT THROUGH THE QUEUE IN PROCESS, which is
// the opposite of the store lab's choice and is right here: what is being
// proved includes `cmd_serve` wiring the catalogue into BOTH readers, which is
// exactly the join #105's 501 was missing -- a field the constructor never
// assigned, invisible to every test that set it itself.
func TestTTS_ServeQueuesAnUtteranceAndReturnsAPlayableWav(t *testing.T) {
	l := harness.New(t)
	image := ttsImage(t, l)

	l.WriteConfig(map[string]string{"image": image})
	l.AppendConfig(`
tts:
  voices:
    - id: lessac
      name: Lessac (en_US, medium)
      voice: /opt/voices/en_US-lessac-medium.onnx
`)
	cmd, srv, wait := startServe(t, l)

	// THE LIST A BROWSER WOULD READ. A request names an id and nothing else,
	// so this is the only way a page could know what to send -- and the paths
	// must not be in it, which is the projection's own rule.
	cfg := ask(t, "GET", srv, "/api/config", "")
	if cfg.Status != http.StatusOK {
		t.Fatalf("GET /api/config: %d %s", cfg.Status, cfg.Body)
	}
	var cv struct {
		Workloads []struct {
			ID     string `json:"id"`
			Driver string `json:"driver"`
			Voices []struct {
				ID   string `json:"id"`
				Name string `json:"name"`
			} `json:"voices"`
		} `json:"workloads"`
	}
	if err := json.Unmarshal(cfg.Body, &cv); err != nil {
		t.Fatalf("decode /api/config: %v\n%s", err, cfg.Body)
	}
	// WHAT THIS SERVER DECLARES AND NOT WHAT THIS BUILD KNOWS (#200). This
	// config has `tts.voices` and no `sd:` block, so it describes one
	// servable workload -- and the image one is absent rather than offered
	// and then explained in a red banner, which is what `genstore.Kinds()`
	// published here before.
	var drivers []string
	for _, w := range cv.Workloads {
		drivers = append(drivers, w.Driver)
		if w.Driver != "tts" {
			continue
		}
		if len(w.Voices) != 1 || w.Voices[0].ID != "lessac" {
			t.Fatalf("voices = %+v, want the one this config publishes", w.Voices)
		}
	}
	if !slices.Contains(drivers, "tts") {
		t.Errorf("workloads = %+v, want one with the tts driver", cv.Workloads)
	}
	if slices.Contains(drivers, "sd") {
		t.Errorf("workloads = %+v; this config has no sd: block and it is offered anyway",
			cv.Workloads)
	}
	if strings.Contains(string(cfg.Body), "/opt/voices/") {
		t.Errorf("/api/config publishes a path on the node:\n%s", cfg.Body)
	}

	// AND AN ID NOBODY PUBLISHED IS REFUSED BEFORE ANYTHING IS RENTED, which
	// is the cheap half of the seal: the same mistake found at the node costs
	// the create call, the wait for sshd and the image pull.
	bad := ask(t, "POST", srv, "/api/requests",
		`{"workload":"tts","provider":"local","jobs":[{"tts":{"text":"x","voice_id":"nope"}}]}`)
	if bad.Status != http.StatusBadRequest {
		t.Errorf("an unpublished voice got %d, want 400: %s", bad.Status, bad.Body)
	}
	if !strings.Contains(string(bad.Body), "lessac") {
		t.Errorf("the refusal does not list what is published: %s", bad.Body)
	}

	resp := ask(t, "POST", srv, "/api/requests",
		`{"workload":"tts","provider":"local","jobs":[
			{"tts":{"text":"Hello from the browser.","voice_id":"lessac","output":"greeting"}}]}`)
	if resp.Status != http.StatusAccepted {
		t.Fatalf("queueing an utterance failed: %d %s", resp.Status, resp.Body)
	}
	var acc struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(resp.Body, &acc); err != nil {
		t.Fatalf("decode the acceptance: %v\n%s", err, resp.Body)
	}

	// Waits for the row to END rather than only for the one state it wants,
	// which is `orExited`'s rule as the browser labs spell it: a lab that
	// waits for success alone reports a timeout for a refusal that is already
	// recorded, which cost one run fourteen minutes.
	deadline := time.Now().Add(4 * time.Minute)
	var done bool
	for time.Now().Before(deadline) && !done {
		one := ask(t, "GET", srv, "/api/requests/"+acc.ID, "")
		if one.Status == http.StatusOK {
			var rv struct {
				Status string `json:"status"`
				Error  string `json:"error"`
			}
			if err := json.Unmarshal(one.Body, &rv); err == nil {
				switch rv.Status {
				case "done":
					done = true
				case "failed", "cancelled", "interrupted":
					t.Fatalf("the request ended %s: %s\n%s", rv.Status, rv.Error, l.Printed())
				}
			}
		}
		if !done {
			time.Sleep(500 * time.Millisecond)
		}
	}
	if !done {
		t.Fatalf("the utterance never finished:\n%s", l.Printed())
	}

	// THE ARTIFACT, THROUGH THE ROUTE A BROWSER WOULD USE, declared as audio.
	// This is the Phase-3 half: the same route served `image/png`
	// unconditionally until a second kind made a wav, and with `nosniff`
	// beside it a browser will not play that file and cannot be talked into
	// playing it.
	one := ask(t, "GET", srv, "/api/requests/"+acc.ID, "")
	var rv struct {
		Images []struct {
			Path string `json:"path"`
		} `json:"images"`
	}
	if err := json.Unmarshal(one.Body, &rv); err != nil {
		t.Fatal(err)
	}
	if len(rv.Images) != 1 {
		t.Fatalf("%d artifact(s) on the request view, want 1:\n%s", len(rv.Images), one.Body)
	}

	got := askRaw(t, srv, rv.Images[0].Path)
	if got.Status != http.StatusOK {
		t.Fatalf("GET %s: %d", rv.Images[0].Path, got.Status)
	}
	if ct := got.Header.Get("Content-Type"); ct != "audio/wav" {
		t.Errorf("content-type = %q, want audio/wav: a wav declared as a PNG is a file a "+
			"browser will not play", ct)
	}
	if got.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Error("the artifact was served without nosniff, so a browser may second-guess what " +
			"a rented machine produced")
	}
	// AND THE BYTES ARE A WAV, not a file with the right name: a tar that
	// failed halfway produces a valid prefix of an archive, so "a file
	// arrived" is satisfied by two different failures.
	p := filepath.Join(t.TempDir(), "got.wav")
	if err := os.WriteFile(p, got.Body, 0o600); err != nil {
		t.Fatal(err)
	}
	dur := wavSeconds(t, p)
	t.Logf("the browser would have received %.3fs of audio, %d bytes", dur, len(got.Body))
	if dur < 0.5 {
		t.Errorf("the artifact is %.3fs of audio: truncated or empty", dur)
	}

	// AND THE SERVER IS STOPPED RATHER THAN LEFT TO THE TEST ENDING, which
	// the lab's own stray check reported on its first green run: a request
	// that finished does not destroy the node, because the drain loop
	// destroys it when nothing COMPATIBLE is left -- and the test exiting
	// killed `ember serve` before that teardown ran, leaving a container and
	// a bridge network behind. Nothing bills for the network, which is why
	// AGENTS.md records that class going unnoticed until Docker had subnetted
	// its whole address pool.
	//
	// SIGINT and then wait, which is `ember serve`'s documented shutdown:
	// it waits for queue.Run, which does not return until
	// provision.WithNode's deferred destroy has.
	if err := cmd.Process.Signal(syscall.SIGINT); err != nil {
		t.Fatalf("signal: %v", err)
	}
	out := wait()
	if nodes := l.Nodes(t.Context()); len(nodes) != 0 {
		t.Errorf("%d node(s) survived the shutdown; ember said:\n%s", len(nodes), out)
	}
}

// askRaw fetches one artifact and keeps the headers.
//
// `ask` decodes a body into a string and drops the response, which is right
// for every JSON route and wrong for the one assertion here that is about a
// header -- so this is a second helper rather than a widened first one, which
// is the line AGENTS.md draws for a production seam applied to a harness.
func askRaw(t *testing.T, s served, path string) rawAnswer {
	t.Helper()
	url := fmt.Sprintf("http://127.0.0.1:%d%s", s.Port, path)
	req, err := http.NewRequestWithContext(t.Context(), "GET", url, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Origin", fmt.Sprintf("http://127.0.0.1:%d", s.Port))
	req.Header.Set("Authorization", "Bearer "+s.Token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return rawAnswer{Status: resp.StatusCode, Header: resp.Header, Body: b}
}

type rawAnswer struct {
	Status int
	Header http.Header
	Body   []byte
}
