package tunnel

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
)

// RecordingDialer records what was dialled and hands back one end of a
// net.Pipe, so the forwarder can be tested end to end with no SSH server.
//
// The field names match go-tun's linux.RecordingRunner — Calls, Errors,
// FailOn — so anyone who has read one fake reads this one without learning
// anything new.
type RecordingDialer struct {
	mu sync.Mutex

	// Calls records every dial as "tcp 127.0.0.1:8188".
	Calls []string

	// Errors fails a specific address.
	Errors map[string]error

	// FailOn fails any address containing this substring. Set it to make the
	// next dial fail, clear it to let the one after succeed — that pair is
	// how "the workload was not listening yet" is reproduced.
	FailOn string

	// Echo makes the far end echo what it receives, so a test can assert the
	// copy ran in both directions rather than merely that a dial happened.
	Echo bool

	conns []net.Conn
}

// NewRecordingDialer returns a dialer that succeeds and echoes.
func NewRecordingDialer() *RecordingDialer {
	return &RecordingDialer{Errors: map[string]error{}, Echo: true}
}

// DialContext satisfies Dialer.
func (d *RecordingDialer) DialContext(_ context.Context, network, addr string) (net.Conn, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	d.Calls = append(d.Calls, network+" "+addr)

	if err, ok := d.Errors[addr]; ok {
		return nil, err
	}
	if d.FailOn != "" && strings.Contains(addr, d.FailOn) {
		return nil, fmt.Errorf("recording dialer refusing %s (FailOn %q)", addr, d.FailOn)
	}

	near, far := net.Pipe()
	d.conns = append(d.conns, near, far)
	if d.Echo {
		go func() { _, _ = io.Copy(far, far) }()
	}
	return near, nil
}

// Dialled reports every address dialled so far.
func (d *RecordingDialer) Dialled() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.Calls...)
}

// Close releases every pipe handed out.
func (d *RecordingDialer) Close() {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, c := range d.conns {
		_ = c.Close()
	}
	d.conns = nil
}

// ErrRefused is a convenient stand-in for "nothing is listening there yet".
var ErrRefused = errors.New("connection refused")
