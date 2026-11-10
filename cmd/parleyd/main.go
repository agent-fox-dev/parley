// Command parleyd is the Parley hub server.
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
	defaultListenAddr    = "0.0.0.0:8080"
	defaultShutdownGrace = 15 * time.Second
)

func main() {
	os.Exit(run(context.Background(), os.Args[1:], defaultListenAddr, defaultShutdownGrace, os.Stdout, os.Stderr))
}

const usage = `Usage: parleyd [flags]

parleyd is the Parley hub server. It listens for HTTP and serves until it
receives SIGTERM or SIGINT.

Flags:
  -h, --help   print this help and exit
`

// run is parleyd's whole life: it parses the arguments, serves on addr until
// a signal arrives or ctx is cancelled, and returns the exit status. mounts
// register extra routes on the server before it listens.
func run(ctx context.Context, args []string, addr string, grace time.Duration, stdout, stderr io.Writer, mounts ...func(*server.Server)) int {
	fs := flag.NewFlagSet("parleyd", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.Usage = func() {}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprint(stdout, usage)
			return 0
		}
		fmt.Fprintf(stderr, "parleyd: %v\n\n%s", err, usage)
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "parleyd: unexpected argument %q\n\n%s", fs.Arg(0), usage)
		return 2
	}

	ctx, stop := signal.NotifyContext(ctx, syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	logger := slog.New(slog.NewJSONHandler(stderr, nil))
	opts := server.DefaultOptions()
	opts.Addr, opts.ShutdownGrace = addr, grace
	srv := server.New(opts)
	for _, mount := range mounts {
		mount(srv)
	}
	err := srv.Serve(ctx, func(bound net.Addr) {
		logger.Info("listening", "addr", bound.String())
	})
	if err != nil {
		fmt.Fprintf(stderr, "parleyd: cannot listen on %s: %v\n", addr, err)
		return 1
	}
	return 0
}
