package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"
)

// outDir is where the upgrade test finds the archives, relative to the
// repository root the command is run from.
const outDir = "test/upgrade/testdata"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := Run(ctx, os.Args[1:], outDir, os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}
