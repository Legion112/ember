package sshx

import (
	"bytes"
	"context"
	"io"
	"sync"
)

// RecordingRunner is a Runner that answers from a script.
//
// Field names match dockerengine.RecordingEngine and linux.RecordingRunner so
// they read the same way across the repository: Calls for what happened,
// Outputs for what to return, Errors and FailOn for what to break.
type RecordingRunner struct {
	mu sync.Mutex

	// Outputs maps a rendered command to the stdout it produces.
	Outputs map[string][]byte

	// Errors maps a rendered command to the error Run returns before it
	// produces any output -- a transport failure, not a non-zero exit.
	Errors map[string]error

	// Waits maps a rendered command to what its wait function returns, for
	// reproducing a command that streams and then fails.
	Waits map[string]error

	// FailOn makes any command containing this substring fail at Run.
	FailOn string

	calls []string
}

// NewRecordingRunner returns an empty runner.
func NewRecordingRunner() *RecordingRunner {
	return &RecordingRunner{
		Outputs: map[string][]byte{},
		Errors:  map[string]error{},
		Waits:   map[string]error{},
	}
}

// Run records the command and returns the scripted answer.
func (r *RecordingRunner) Run(ctx context.Context, cmd Command) (io.ReadCloser, func() error, error) {
	if err := cmd.Validate(); err != nil {
		// The fake refuses what the real one would refuse. A fake that is
		// more permissive than production is a fake that hides the bug.
		return nil, nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}

	line := cmd.String()

	r.mu.Lock()
	r.calls = append(r.calls, line)
	out, hasOut := r.Outputs[line]
	err, hasErr := r.Errors[line]
	wait, hasWait := r.Waits[line]
	failOn := r.FailOn
	r.mu.Unlock()

	if failOn != "" && contains(line, failOn) {
		return nil, nil, &ExitError{Cmd: line, Code: 1, Stderr: "recording runner was told to fail"}
	}
	if hasErr {
		return nil, nil, err
	}
	if !hasOut {
		out = nil
	}

	rc := io.NopCloser(bytes.NewReader(out))
	return rc, func() error {
		if hasWait {
			return wait
		}
		return nil
	}, nil
}

// Calls returns the commands run so far.
func (r *RecordingRunner) Calls() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.calls...)
}

func contains(s, sub string) bool {
	return len(sub) > 0 && len(s) >= len(sub) && bytes.Contains([]byte(s), []byte(sub))
}
