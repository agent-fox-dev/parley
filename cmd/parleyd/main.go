// Command parleyd is the Parley hub server. Its whole life is composed in
// internal/hub; this file only hands it the process's arguments,
// environment, working directory, output and signals.
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/agent-fox-dev/parley/internal/config"
	"github.com/agent-fox-dev/parley/internal/hub"
)

func main() {
	wd, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(os.Stderr, "parleyd: cannot read the working directory: %v\n", err)
		os.Exit(1)
	}
	os.Exit(run(context.Background(), os.Args[1:], os.LookupEnv, wd, os.Stdout, os.Stderr))
}

// run runs the hub with the arguments, an environment lookup and a working
// directory until it ends by itself or SIGTERM or SIGINT arrives, and
// returns the exit status. While the hub shuts down, further signals are
// still caught here, so they change nothing.
func run(ctx context.Context, args []string, env config.LookupEnv, workDir string, stdout, stderr io.Writer, options ...hub.Option) int {
	ctx, stop := signal.NotifyContext(ctx, syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	opts := hub.Options{Args: args, Env: env, WorkDir: workDir, Stdout: stdout, Stderr: stderr}
	for _, o := range options {
		o(&opts)
	}
	return hub.Run(ctx, opts)
}
