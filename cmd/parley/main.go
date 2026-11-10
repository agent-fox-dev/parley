// Command parley is the Parley command-line client.
package main

import (
	"context"
	"os"

	"github.com/agent-fox-dev/parley/internal/cli"
)

func main() {
	os.Exit(cli.Run(context.Background(), os.Args[1:], os.Stdout, os.Stderr))
}
