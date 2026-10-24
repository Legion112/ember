// Command emberwl is a stand-in workload for the integration labs.
//
// It is go-tun's labhttp for this project: a server whose answers let a test
// distinguish "the tunnel carried a byte" from "the tunnel carried a megabyte
// to the right container", which are the two failures that otherwise both
// look like success.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/Legion112/ember/internal/loopback"
)

func main() {
	var (
		listen    = flag.String("listen", "127.0.0.1:8188", "address to serve on")
		id        = flag.String("id", "emberwl", "string returned by /id")
		outPath   = flag.String("write-output", "", "write a deterministic file here at startup")
		outSize   = flag.Int("size", 4096, "size of the file written by -write-output")
		exitAfter = flag.Duration("exit-after", 0, "exit after this long")
		exitCode  = flag.Int("exit-code", 0, "status to exit with")
	)
	flag.Parse()

	if *outPath != "" {
		if err := writeOutput(*outPath, *outSize); err != nil {
			fmt.Fprintf(os.Stderr, "emberwl: %v\n", err)
			os.Exit(1)
		}
	}
	if *exitAfter > 0 {
		go func() {
			time.Sleep(*exitAfter)
			os.Exit(*exitCode)
		}()
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})
	// /id proves the forward reached *this* container rather than something
	// else that happened to be bound locally.
	mux.HandleFunc("/id", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(*id))
	})
	// /bulk is go-tun's MTU lesson: a tunnel that passes /id and drops a
	// megabyte passes every small test and fails in production.
	mux.HandleFunc("/bulk", func(w http.ResponseWriter, r *http.Request) {
		n, err := strconv.Atoi(r.URL.Query().Get("bytes"))
		if err != nil || n < 0 {
			http.Error(w, "bytes must be a non-negative integer", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Length", strconv.Itoa(n))
		buf := make([]byte, 32*1024)
		for i := range buf {
			buf[i] = byte(i % 251)
		}
		for n > 0 {
			chunk := min(n, len(buf))
			if _, err := w.Write(buf[:chunk]); err != nil {
				return
			}
			n -= chunk
		}
	})

	// Loopback only, and through the same guard the rest of ember uses. The
	// lab asserts that nothing is reachable except through the tunnel, and a
	// workload bound to 0.0.0.0 would make that assertion pass for the wrong
	// reason -- the port would be reachable on the container's network too.
	//
	// Routing through loopback.ParseBind rather than net.Listen also keeps
	// TestNoListenOutsideLoopback true of the whole module, with no
	// exemption for "it is only the lab".
	bind, err := loopback.ParseBind(*listen)
	if err != nil {
		fmt.Fprintf(os.Stderr, "emberwl: %v\n", err)
		os.Exit(1)
	}
	ln, err := loopback.Listen(context.Background(), bind)
	if err != nil {
		fmt.Fprintf(os.Stderr, "emberwl: %v\n", err)
		os.Exit(1)
	}
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	if err := srv.Serve(ln); err != nil {
		fmt.Fprintf(os.Stderr, "emberwl: %v\n", err)
		os.Exit(1)
	}
}

// writeOutput writes a file whose content is a pure function of its size, so a
// test can assert a SHA-256 it computed itself rather than one it copied from
// a previous run.
func writeOutput(path string, size int) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil { //nolint:gosec // lab output directory
		return fmt.Errorf("create %s: %w", filepath.Dir(path), err)
	}
	buf := make([]byte, size)
	for i := range buf {
		buf[i] = byte(i % 251)
	}
	if err := os.WriteFile(path, buf, 0o644); err != nil { //nolint:gosec // lab output, deliberately readable
		return fmt.Errorf("write %s: %w", path, err)
	}
	sum := sha256.Sum256(buf)
	fmt.Printf("wrote %s (%d bytes, sha256 %s)\n", path, size, hex.EncodeToString(sum[:]))
	return nil
}
