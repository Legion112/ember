// Package tunnel forwards a workload's ports from a node to the operator's
// machine.
//
// Everything here is written against a Dialer rather than against
// *ssh.Client, so the forwarding logic is tested end to end over net.Pipe with
// no SSH server, no Docker and no network — and so a future WireGuard
// transport plugs in without Forwarder changing a line.
package tunnel

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"sync"
	"time"

	"github.com/Legion112/ember/internal/loopback"
)

// Dialer opens a connection to an address on the far side of the tunnel.
//
// *ssh.Client satisfies this through DialContext.
type Dialer interface {
	DialContext(ctx context.Context, network, addr string) (net.Conn, error)
}

// RemoteLoopback names a port on the node's own loopback interface.
//
// It is the only way to name the far end, and it takes a port rather than an
// address on purpose. SSH's direct-tcpip will connect anywhere the node can
// reach — another tenant, the provider's metadata service, the open internet —
// so a free-form remote host turns every ember node into an open relay billed
// to the operator. The workload always listens on the node's loopback, so
// there is nothing else ember needs to name.
func RemoteLoopback(port uint16) netip.AddrPort {
	return netip.AddrPortFrom(netip.AddrFrom4([4]byte{127, 0, 0, 1}), port)
}

// Forward is one workload port, published on one or more local binds.
type Forward struct {
	Name   string          // "comfyui" — for logs and the ready line
	Local  []loopback.Bind // normally Port(p) and Port6(p)
	Remote netip.AddrPort  // from RemoteLoopback only
}

// Forwarder runs a set of forwards over one Dialer.
type Forwarder struct {
	Dialer Dialer
	Log    *slog.Logger
}

func (f *Forwarder) log() *slog.Logger {
	if f.Log != nil {
		return f.Log
	}
	return slog.New(slog.DiscardHandler)
}

// Run opens every listener, then serves until ctx is cancelled or a listener
// fails. Every listener is closed before Run returns, so a cancelled Run
// leaves no port bound.
//
// All listeners are opened before any connection is accepted. If port 8188 is
// already taken, `ember up` fails with nothing bound and nothing half-started:
// a partially forwarded node is a state the operator cannot reason about, and
// the browser tab that works while the other one does not is a worse bug
// report than an outright failure.
func (f *Forwarder) Run(ctx context.Context, fwds []Forward) error {
	if f.Dialer == nil {
		return errors.New("forwarder has no dialer")
	}

	type live struct {
		fwd Forward
		ln  net.Listener
	}
	var opened []live
	closeAll := func() {
		for _, l := range opened {
			_ = l.ln.Close()
		}
	}

	for _, fwd := range fwds {
		if !fwd.Remote.IsValid() {
			closeAll()
			return fmt.Errorf("forward %q has no remote port", fwd.Name)
		}
		if len(fwd.Local) == 0 {
			closeAll()
			return fmt.Errorf("forward %q has no local bind", fwd.Name)
		}
		for _, b := range fwd.Local {
			ln, err := loopback.Listen(ctx, b)
			if err != nil {
				closeAll()
				return fmt.Errorf("forward %q: %w", fwd.Name, err)
			}
			opened = append(opened, live{fwd: fwd, ln: ln})
		}
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var (
		wg      sync.WaitGroup
		errOnce sync.Once
		fatal   error
	)
	for _, l := range opened {
		// Closing the listener is what unblocks a goroutine parked in
		// Accept. A select on ctx.Done() never gets scheduled there.
		stop := context.AfterFunc(ctx, func() { _ = l.ln.Close() })
		defer stop()

		wg.Add(1)
		go func(l live) {
			defer wg.Done()
			if err := f.serve(ctx, l.fwd, l.ln, &wg); err != nil {
				errOnce.Do(func() { fatal = err })
				cancel() // one dead listener tears the node down as a unit
			}
		}(l)
	}

	for _, l := range opened {
		f.log().Info("forwarding", "name", l.fwd.Name, "local", l.ln.Addr().String(), "remote", l.fwd.Remote.String())
	}

	wg.Wait()
	closeAll()
	return fatal
}

func (f *Forwarder) serve(ctx context.Context, fwd Forward, ln net.Listener, wg *sync.WaitGroup) error {
	for {
		conn, err := ln.Accept()
		if err != nil {
			// Our own close during teardown is the ordinary way out, not a
			// failure. Returning an error there would teach the operator to
			// ignore ember's exit code.
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("forward %q stopped accepting: %w", fwd.Name, err)
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			f.pipe(ctx, fwd, conn)
		}()
	}
}

// pipe joins one accepted connection to the far side.
//
// A failed remote dial closes that one connection and is logged at debug: it
// must never kill the forwarder, because ComfyUI takes a minute to bind its
// port and the operator's browser will retry. Treating it as fatal would mean
// ember exits while the node it is forwarding is still coming up.
func (f *Forwarder) pipe(ctx context.Context, fwd Forward, local net.Conn) {
	defer func() { _ = local.Close() }()

	remote, err := f.Dialer.DialContext(ctx, "tcp", fwd.Remote.String())
	if err != nil {
		f.log().Debug("remote not ready", "name", fwd.Name, "remote", fwd.Remote.String(), "err", err)
		return
	}
	defer func() { _ = remote.Close() }()

	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(remote, local); done <- struct{}{} }()
	go func() { _, _ = io.Copy(local, remote); done <- struct{}{} }()

	select {
	case <-done:
	case <-ctx.Done():
	}
}

// WaitReady polls the far side through the tunnel until it accepts.
//
// `ember up` prints "ready at http://127.0.0.1:8188" only once this returns,
// because a refused page on a working tunnel is the single most likely false
// bug report this program can produce.
func WaitReady(ctx context.Context, d Dialer, remote netip.AddrPort, every time.Duration) error {
	if every <= 0 {
		every = 500 * time.Millisecond
	}
	for {
		conn, err := d.DialContext(ctx, "tcp", remote.String())
		if err == nil {
			_ = conn.Close()
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("waiting for %s through the tunnel: %w", remote, ctx.Err())
		case <-time.After(every):
		}
	}
}
