package gitrepo

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
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

// ServeError is the failure of a git process that served a client, or a
// request refused before git ran as git would have refused it.
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

// errNotOurRef is the failure of a request that wants a commit no ref
// reaches.
var errNotOurRef = errors.New("a want names a commit no ref reaches")

// UploadPack runs git upload-pack --stateless-rpc on req.Dir under ctx,
// with the minimal environment and Env, so that neither the system nor the
// global configuration of the host takes part, and with
// GIT_PROTOCOL=version=2 only when req asks for protocol version 2. It
// returns nil when git exited with success, an error wrapping ctx's error
// when ctx ended, git killed with every process it started, and otherwise a
// *ServeError.
//
// Under version 0 git refuses a want of a commit that no ref reaches; under
// version 2 git serves any object it holds by its id. A version 2 fetch
// that wants such a commit is refused here, before git runs, with git's own
// text of version 0, so that a commit no ref reaches (one whose ref was
// deleted, or one a task fork holds only through its parent's objects) is
// served under neither version. A want of a tree or a blob is left to git,
// as git does under version 0: a partial clone fetches those by id, and
// whether a ref reaches one is a walk of every tree of every ref.
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
	if req.Protocol2 && !req.AdvertiseRefs && req.Stdin != nil {
		request, err := g.checkWants(ctx, dir, repo, req.Stdin)
		if err != nil {
			return err
		}
		defer func() { _ = request.Close() }()
		req.Stdin = request
	}
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

// checkWants reads the version 2 request in into a file of the private
// directory dir and returns the file, read from its start, for git to
// read; or a *ServeError when the request is a fetch that wants a commit
// no ref of the repository repo reaches. A request git cannot read is
// passed on for git to refuse in its own words. A client that stops
// sending holds the request no longer than ctx.
func (g Git) checkWants(ctx context.Context, dir, repo string, in io.Reader) (*os.File, error) {
	f, err := os.Create(filepath.Join(dir, "request"))
	if err != nil {
		return nil, fmt.Errorf("keep the request: %w", err)
	}
	ids, err := keepWants(ctx, f, in)
	if err == nil {
		_, err = f.Seek(0, io.SeekStart)
	}
	if err == nil && len(ids) > 0 {
		err = g.reached(ctx, repo, ids)
	}
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}

// reached returns a *ServeError naming the first of the object ids ids
// that is a commit no ref of the repository repo reaches, nil when there is
// none.
func (g Git) reached(ctx context.Context, repo string, ids []string) error {
	// rev-list lists the wanted commits that no ref reaches: a ref's tip
	// and its ancestors are left out, and a tree or a blob is no commit.
	out, err := g.runInput(ctx, access{}, []byte(strings.Join(ids, "\n")+"\n"), "--git-dir="+repo, "rev-list", "--stdin", "--not", "--all")
	if ctxErr := ctx.Err(); ctxErr != nil {
		return fmt.Errorf("git upload-pack: %w", ctxErr)
	}
	if err != nil || len(out) == 0 {
		// An error is an object the repository does not hold, which git
		// refuses itself.
		return nil
	}
	unreached := map[string]bool{}
	for _, id := range strings.Fields(string(out)) {
		unreached[id] = true
	}
	for _, id := range ids {
		if unreached[id] {
			return &ServeError{Stderr: "upload-pack: not our ref " + id, Err: errNotOurRef}
		}
	}
	return nil
}

// keepWants copies the request in to w and returns the object ids its want
// lines name when it is a version 2 fetch command, nil otherwise, or an
// error wrapping ctx's when ctx ends first. The copy runs on while in
// blocks, until in gives up.
func keepWants(ctx context.Context, w io.Writer, in io.Reader) ([]string, error) {
	type kept struct {
		ids []string
		err error
	}
	done := make(chan kept, 1)
	go func() {
		ids, err := scanWants(w, in)
		done <- kept{ids, err}
	}()
	select {
	case k := <-done:
		if k.err != nil {
			return nil, fmt.Errorf("keep the request: %w", k.err)
		}
		return k.ids, nil
	case <-ctx.Done():
		return nil, fmt.Errorf("git upload-pack: %w", ctx.Err())
	}
}

// scanWants is keepWants without the context. It stops looking at the end
// of the request, its flush packet, or at bytes that are not pkt-lines, and
// copies them all the same. An id that is not hexadecimal is left out: git
// refuses the line.
func scanWants(w io.Writer, in io.Reader) ([]string, error) {
	r := bufio.NewReader(io.TeeReader(in, w))
	var ids []string
	for first := true; ; first = false {
		var head [4]byte
		if _, err := io.ReadFull(r, head[:]); err != nil {
			break
		}
		n, err := strconv.ParseUint(string(head[:]), 16, 16)
		if err != nil || n == 0 {
			break
		}
		if n < 4 {
			continue
		}
		payload := make([]byte, n-4)
		if _, err := io.ReadFull(r, payload); err != nil {
			break
		}
		line := strings.TrimSuffix(string(payload), "\n")
		if first && line != "command=fetch" {
			break
		}
		if id, ok := strings.CutPrefix(line, "want "); ok && isObjectID(id) {
			ids = append(ids, id)
		}
	}
	if _, err := io.Copy(io.Discard, r); err != nil {
		return nil, err
	}
	return ids, nil
}

// isObjectID reports whether s is a full object id: 40 or 64 lowercase
// hexadecimal digits.
func isObjectID(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
