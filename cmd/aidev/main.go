// Command aidev is the control plane for delegating implementation tasks to an
// agent, verifying the result independently, and recording what happened.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"aidev/internal/cli"
)

// version is overridden at build time with -ldflags "-X main.version=...".
var version = "0.1.0-dev"

func main() {
	// Ctrl-C and SIGTERM cancel the root context rather than killing the
	// process outright, so an in-flight task can record that it was cancelled
	// instead of leaving a row stuck in RUNNING.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := cli.Run(ctx, version, os.Args[1:], os.Stdout, os.Stderr); err != nil {
		if errors.Is(err, context.Canceled) {
			fmt.Fprintln(os.Stderr, "aidev: interrupted")
			os.Exit(130) // conventional exit status for SIGINT
		}
		var usage *cli.UsageError
		if errors.As(err, &usage) {
			fmt.Fprintf(os.Stderr, "aidev: %v\n", usage)
			os.Exit(2)
		}
		fmt.Fprintf(os.Stderr, "aidev: %v\n", err)
		os.Exit(1)
	}
}
