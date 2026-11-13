// Command parleyd is the Parley hub. Its work is done by internal/hub: main
// hands it the process's arguments, environment and working directory and
// exits with the status it returns.
package main

import (
	"context"
	"io"
	"os"

	"github.com/agent-fox-dev/parley/internal/config"
	"github.com/agent-fox-dev/parley/internal/hub"
)

func main() {
	wd, err := os.Getwd()
	if err != nil {
		// The hub resolves "." itself and reports the failure as its one
		// start error line.
		wd = "."
	}
	os.Exit(run(context.Background(), os.Args[1:], os.LookupEnv, wd, os.Stdout, os.Stderr))
}

// run is parleyd: hub.Run with the process's inputs. tune lets tests change
// the hub's test options, such as its time limits; main passes none.
func run(ctx context.Context, args []string, env config.Env, wd string, stdout, stderr io.Writer, tune ...func(*hub.Options)) int {
	opts := hub.Options{Args: args, Env: env, WorkDir: wd, Stdout: stdout, Stderr: stderr}
	for _, f := range tune {
		f(&opts)
	}
	return hub.Run(ctx, opts)
}
