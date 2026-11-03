package queue

import (
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/Legion112/ember/internal/genstore"
)

// TestDropPending_NamesTheRunItThrewAwayAndSaysNothingIsBilling.
//
// WHY THE LINE IS TESTED DIRECTLY AND NOT THROUGH THE RACE. The state it
// reports is reachable only when a shutdown and a parked submission are both
// ready in `Run`'s idle select, where Go picks uniformly -- measured with the
// two fired from a shared barrier, and reported as unreachable without one.
// A test that raced for it would be the flake this change has already been
// corrected for once; what is worth an exit code is WHAT THE LINE SAYS,
// because the whole point of it is that a reader can confirm the row's fate
// instead of taking it on trust. Submit's own comment carries the numbers.
//
// The facts are asserted separately because they answer different questions
// and a reader needs all of them: the request, so it can be found; that
// nothing is billing, which is the one an operator reads this for; and what
// to do, since the client was told the run had started.
func TestDropPending_NamesTheRunItThrewAwayAndSaysNothingIsBilling(t *testing.T) {
	var sb strings.Builder
	q := &Queue{
		Store: genstore.NewRecordingStore(),
		Log:   slog.New(slog.NewTextHandler(&sb, &slog.HandlerOptions{Level: slog.LevelWarn})),
	}

	q.dropPending(&Direct{Req: "req_abc", Run: func(context.Context) error { return nil }})

	got := sb.String()
	for _, want := range []string{
		"req_abc",
		"nothing was rented and nothing is billing",
		"marks the request interrupted",
		"submit it again",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the drop line does not say %q:\n%s", want, got)
		}
	}
	if !strings.Contains(got, "level=WARN") {
		t.Errorf("the drop is not reported at warn: %s", got)
	}

	// AND AN ORDINARY SHUTDOWN IS SILENT. `Run` calls this unconditionally, so
	// the nil answer is taken on every clean exit -- a line there would be one
	// per stop, about nothing, which is how an operator learns to stop reading
	// these.
	sb.Reset()
	q.dropPending(nil)
	if sb.Len() != 0 {
		t.Errorf("a shutdown with no submission in hand logged %q", sb.String())
	}
}
