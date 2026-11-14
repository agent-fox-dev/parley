package gitrepo

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// ServeRequest is one run of git upload-pack for a client of the hub, in
// git's stateless mode, which smart HTTP needs: each request is answered
// by a process of its own that reads the request from Stdin and writes the
// whole answer to Stdout.
type ServeRequest struct {
	// Dir is the git directory of the repository served.
	Dir string
	// AdvertiseRefs runs the ref advertisement alone, the answer to ref
	// discovery; Stdin is not read.
	AdvertiseRefs bool
	// Protocol2 serves the request under git protocol version 2. Without
	// it git serves version 0, whatever the environment of the hub says.
	Protocol2 bool
	// Config are configuration settings, as name=value, given to this one
	// run on git's command line, so that what the hub serves does not
	// depend on a repository's own configuration being changed.
	Config []string
	// Stdin is the client's request; nil for none.
	Stdin io.Reader
	// Stdout receives git's answer as git writes it, so that the client
	// gets the first bytes of a large pack while git still writes the rest.
	Stdout io.Writer
}

// ServeError is the failure of a git process that served a client.
type ServeError struct {
	// Stderr is what git wrote to its standard error, trimmed: git's own
	// text of what it refused.
	Stderr string
	// Err is the failure of the process.
	Err error
}

func (e *ServeError) Error() string {
	return fmt.Sprintf("git upload-pack: %v: %s", e.Err, e.Stderr)
}

func (e *ServeError) Unwrap() error { return e.Err }

// UploadPack runs git upload-pack --stateless-rpc on req.Dir under ctx,
// with the minimal environment and Env, so that neither the system nor the
// global configuration of the host takes part, and with
// GIT_PROTOCOL=version=2 only when req asks for protocol version 2. It
// returns nil when git exited with success, an error wrapping ctx's error
// when ctx ended, git killed with every process it started, and otherwise a
// *ServeError.
func (g Git) UploadPack(ctx context.Context, req ServeRequest) error {
	repo, err := filepath.Abs(req.Dir)
	if err != nil {
		return fmt.Errorf("serve %s: %w", req.Dir, err)
	}
	args := make([]string, 0, 2*len(req.Config)+4)
	for _, c := range req.Config {
		args = append(args, "-c", c)
	}
	args = append(args, "upload-pack", "--stateless-rpc")
	if req.AdvertiseRefs {
		args = append(args, "--advertise-refs")
	}
	args = append(args, repo)
	dir, err := privateDir(false)
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	cmd := g.command(ctx, dir, access{}, args)
	if req.Protocol2 {
		cmd.Env = append(cmd.Env, "GIT_PROTOCOL=version=2")
	}
	errOut := &limitedBuffer{limit: stderrLimit}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = req.Stdin, req.Stdout, errOut
	if err := cmd.Run(); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return fmt.Errorf("git upload-pack: %w", ctxErr)
		}
		return &ServeError{Stderr: strings.TrimSpace(errOut.String()), Err: err}
	}
	return nil
}
