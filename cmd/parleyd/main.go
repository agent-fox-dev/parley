// Command parleyd is the Parley hub: it serves the hub's HTTP routes until
// SIGTERM or SIGINT stops it.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/agent-fox-dev/parley/internal/server"
)

const (
	// defaultAddr is where parleyd listens until it has a configuration.
	defaultAddr = "0.0.0.0:8080"
	// defaultShutdownGrace is how long in-flight requests get on a stop.
	defaultShutdownGrace = 15 * time.Second
)

func main() {
	os.Exit(run(context.Background(), os.Args[1:], defaultAddr, defaultShutdownGrace, os.Stdout, os.Stderr))
}

const usageFormat = `Usage: parleyd [flags]

parleyd is the Parley hub. It takes no arguments, listens for HTTP on %s
and runs until it receives SIGTERM or SIGINT; in-flight requests then get %v
to finish.

Flags:
  -h, --help   print this help and exit
`

// run is parleyd: it parses args, serves on addr until ctx is cancelled or a
// SIGTERM or SIGINT arrives, gives in-flight requests up to grace, and
// returns the exit status: 0 after a stop, 1 when addr cannot be bound and 2
// for a usage error.
func run(ctx context.Context, args []string, addr string, grace time.Duration, stdout, stderr io.Writer) int {
	usage := fmt.Sprintf(usageFormat, addr, grace)
	fs := flag.NewFlagSet("parleyd", flag.ContinueOnError)
	// The flag package's own messages name a flag without the dashes it was
	// given with, so run writes every message itself.
	fs.SetOutput(io.Discard)
	fs.Usage = func() {}
	err := fs.Parse(args)
	switch {
	case errors.Is(err, flag.ErrHelp):
		fmt.Fprint(stdout, usage)
		return 0
	case err != nil:
		fmt.Fprintf(stderr, "parleyd: argument %q: %v\n\n%s", rejectedArgument(args, fs.Args()), err, usage)
		return 2
	case fs.NArg() > 0:
		fmt.Fprintf(stderr, "parleyd: unexpected argument %q\n\n%s", fs.Arg(0), usage)
		return 2
	}

	// Registered before the bind, so a signal that arrives once the listening
	// line is out always stops the server rather than the process.
	ctx, stop := signal.NotifyContext(ctx, syscall.SIGTERM, os.Interrupt)
	defer stop()

	logger := slog.New(slog.NewJSONHandler(stderr, nil))
	srv := server.New(server.Options{
		Addr:          addr,
		ShutdownGrace: grace,
		Logger:        logger,
		OnListen: func(bound net.Addr) {
			logger.Info("listening", "addr", bound.String())
		},
	})
	if err := srv.Serve(ctx); err != nil {
		msg := "server failed"
		if opErr := (*net.OpError)(nil); errors.As(err, &opErr) && opErr.Op == "listen" {
			msg = "cannot listen"
		}
		logger.Error(msg, "addr", addr, "error", err.Error())
		return 1
	}
	return 0
}

// rejectedArgument is the argument a failed fs.Parse stopped at. Parse drops
// each argument it reads, the rejected flag included, so it is the last one
// read; when Parse read none, it is the first.
func rejectedArgument(args, rest []string) string {
	if n := len(args) - len(rest); n > 0 {
		return args[n-1]
	}
	return args[0]
}
