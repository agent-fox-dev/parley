// Command parley is the command-line client of the Parley hub.
package main

import (
	"context"
	"os"

	"github.com/agent-fox-dev/parley/internal/cli"
)

func main() {
	os.Exit(cli.Run(context.Background(), os.Args[1:], os.Stdout, os.Stderr))
}
