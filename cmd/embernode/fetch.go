package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/Legion112/ember/internal/assets"
	"github.com/Legion112/ember/internal/node"
)

// runFetch puts one asset on the store and proves it is the right bytes.
//
// It runs on the node, over the same SSH connection the workload uses, so its
// output streams to the operator's terminal and its exit status is known when
// it happens. A failure here means the workload never starts -- which is the
// point: the cheapest moment to discover that the weights are wrong is before
// the GPU has been asked to do anything with them.
func runFetch(args []string) error {
	fs := flag.NewFlagSet("embernode fetch", flag.ContinueOnError)
	var (
		dir    = fs.String("dir", "", "the store's mount path on this node")
		url    = fs.String("url", "", "where to fetch it from if it is not already there")
		sha    = fs.String("sha256", "", "the digest the file must have")
		as     = fs.String("as", "", "what to call it on the store")
		verify = fs.String("verify", string(assets.DefaultPolicy), "always | present")
	)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *dir == "" {
		return fmt.Errorf("--dir is required: an asset needs a store to land on")
	}

	policy, err := assets.ParsePolicy(*verify)
	if err != nil {
		return err
	}

	// Interruptible, because this is the long step. A forty-gigabyte
	// checkpoint takes minutes, and an operator who presses Ctrl-C is asking
	// for the node to be destroyed -- which cannot happen while this blocks
	// forever on a stalled transfer.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	_, err = assets.Ensure(ctx, nil, *dir, node.Asset{URL: *url, SHA256: *sha, As: *as}, policy, os.Stdout)
	return err
}
