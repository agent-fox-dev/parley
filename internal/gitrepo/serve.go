package gitrepo

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

// ServeRequest is one run of git upload-pack or git receive-pack for a
// client of the hub, in git's stateless mode, which smart HTTP needs: each
// request is answered by a process of its own that reads the request from
// Stdin and writes the whole answer to Stdout.
type ServeRequest struct {
	// Dir is the git directory of the repository served.
	Dir string
	// AdvertiseRefs runs the ref advertisement alone, the answer to ref
	// discovery; Stdin is not read.
	AdvertiseRefs bool
	// Protocol2 serves the request under git protocol version 2. Without
	// it git serves version 0, whatever the environment of the hub says.
	// receive-pack speaks no version 2 and is always run without it.
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
	// MaxRequest bounds the bytes of a version 2 request kept before
	// upload-pack runs; zero is DefaultMaxRequest.
	MaxRequest int64
}

// DefaultMaxRequest is how much of a version 2 request UploadPack keeps
// before git runs: 16 MiB. A fetch request is a pkt-line of about 50 bytes
// for each want and each have, and stock git sends a few thousand at most in
// one round of negotiation, so a real request stays far below it, a fetch
// of 100,000 refs included; it bounds what a client can make the hub write
// to disk for one request.
const DefaultMaxRequest = 16 << 20

// ErrRequestTooLarge is the failure of a version 2 request that holds more
// than its MaxRequest bytes; git did not run.
var ErrRequestTooLarge = errors.New("request too large")

// ServeError is the failure of a git process that served a client, or a
// request refused before git ran as git would have refused it.
type ServeError struct {
	// Command is the git command that served: upload-pack or receive-pack.
	Command string
	// Stderr is what git wrote to its standard error, trimmed: git's own
	// text of what it refused.
	Stderr string
	// Err is the failure of the process.
	Err error
}

func (e *ServeError) Error() string {
	return fmt.Sprintf("git %s: %v: %s", e.Command, e.Err, e.Stderr)
}

func (e *ServeError) Unwrap() error { return e.Err }

// The git commands that serve a client.
const (
	uploadPack  = "upload-pack"
	receivePack = "receive-pack"
)

// The failures of a fetch refused before git runs: a want of an object no
// ref reaches, and a version 2 want that names no object id git can read.
var (
	errNotOurRef = errors.New("a want names an object no ref reaches")
	errBadWant   = errors.New("a want names no object id")
)

// receivePackConfig is the configuration of every run of receive-pack:
// the push options and atomic pushes are advertised, which the push rules
// of the hub take, whatever a repository's own configuration says.
var receivePackConfig = []string{"receive.advertisePushOptions=true", "receive.advertiseAtomic=true"}

// UploadPack runs git upload-pack --stateless-rpc on req.Dir under ctx,
// with the minimal environment and Env, so that neither the system nor the
// global configuration of the host takes part, and with
// GIT_PROTOCOL=version=2 only when req asks for protocol version 2. It
// returns nil when git exited with success, an error wrapping ctx's error
// when ctx ended, git killed with every process it started, and otherwise a
// *ServeError.
//
// A fetch is served only the objects the repository's refs reach. git
// itself refuses, under version 0 alone, a want of a commit no ref reaches,
// and under either version serves a tree, a blob or a tag by its id
// whether a ref reaches it or not. So every want of a fetch is checked here,
// before git runs, and a want of an object no ref reaches (a commit whose
// ref was deleted, a blob of rewritten history, or an object a task fork
// holds only through its parent's objects) is refused with git's own text
// of version 0. A commit is judged by its ancestry; a tree, a blob or a tag
// that no ref points at by a walk of every object the refs reach, which
// stops once it has met every one wanted. A partial clone's fetch of what
// its filter left out wants objects its refs reach, and is served. A
// version 2 want whose id git could not read is refused as well. For the
// check a version 2 request is kept whole, and a version 0 request up to
// the end of its wants, up to req.MaxRequest bytes; a larger one is refused
// with ErrRequestTooLarge.
func (g Git) UploadPack(ctx context.Context, req ServeRequest) error {
	var filter func(dir, repo string, in io.Reader) (io.ReadCloser, error)
	if !req.AdvertiseRefs && req.Stdin != nil {
		limit := req.MaxRequest
		if limit <= 0 {
			limit = DefaultMaxRequest
		}
		v2 := req.Protocol2
		filter = func(dir, repo string, in io.Reader) (io.ReadCloser, error) {
			return g.checkWants(ctx, dir, repo, limit, v2, in)
		}
	}
	return g.serve(ctx, uploadPack, req, filter)
}

// ReceivePack runs git receive-pack --stateless-rpc on req.Dir under ctx,
// as UploadPack runs upload-pack, with the push options and atomic pushes
// advertised ahead of req.Config, and always under protocol version 0,
// whatever req.Protocol2 says: receive-pack speaks no version 2, and a
// version it does not know would only be ignored by it.
func (g Git) ReceivePack(ctx context.Context, req ServeRequest) error {
	req.Protocol2 = false
	req.Config = append(slices.Clone(receivePackConfig), req.Config...)
	return g.serve(ctx, receivePack, req, nil)
}

// serve runs git command --stateless-rpc for req. filter, when not nil, is
// given git's private directory, the repository and the request, and
// returns what git reads in the request's place, or a failure that stops
// git before it runs.
func (g Git) serve(ctx context.Context, command string, req ServeRequest, filter func(dir, repo string, in io.Reader) (io.ReadCloser, error)) error {
	repo, err := filepath.Abs(req.Dir)
	if err != nil {
		return fmt.Errorf("serve %s: %w", req.Dir, err)
	}
	args := make([]string, 0, 2*len(req.Config)+4)
	for _, c := range req.Config {
		args = append(args, "-c", c)
	}
	args = append(args, command, "--stateless-rpc")
	if req.AdvertiseRefs {
		args = append(args, "--advertise-refs")
	}
	args = append(args, repo)
	dir, err := privateDir(false)
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	if filter != nil {
		request, err := filter(dir, repo, req.Stdin)
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
			return fmt.Errorf("git %s: %w", command, ctxErr)
		}
		return &ServeError{Command: command, Stderr: strings.TrimSpace(errOut.String()), Err: err}
	}
	return nil
}

// checkWants reads the request in into a file of the private directory
// dir, the whole of a version 2 request, the wants of a version 0 one, and
// returns what git is to read: the file, read from its start, followed for
// version 0 by the rest of in. It returns a *ServeError when the request is
// a fetch that wants an object no ref of the repository repo reaches, or a
// version 2 want git cannot read, and an error wrapping ErrRequestTooLarge
// when what it keeps holds more than limit bytes. A request git cannot read
// otherwise is passed on for git to refuse in its own words. A client that
// stops sending holds the request no longer than ctx.
func (g Git) checkWants(ctx context.Context, dir, repo string, limit int64, v2 bool, in io.Reader) (io.ReadCloser, error) {
	f, err := os.Create(filepath.Join(dir, "request"))
	if err != nil {
		return nil, fmt.Errorf("keep the request: %w", err)
	}
	wants, err := keepWants(ctx, f, in, limit, v2)
	if err == nil {
		_, err = f.Seek(0, io.SeekStart)
	}
	if err == nil && len(wants) > 0 {
		err = g.reached(ctx, repo, wants)
	}
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	if v2 {
		return f, nil
	}
	return struct {
		io.Reader
		io.Closer
	}{io.MultiReader(f, in), f}, nil
}

// reached returns a *ServeError naming a want of wants that is, or leads
// to, an object no ref of the repository repo reaches, nil when there is
// none.
func (g Git) reached(ctx context.Context, repo string, wants []want) error {
	var ids []string
	for _, w := range wants {
		ids = append(ids, w.ids...)
	}
	// rev-list lists the wanted commits that no ref reaches, an annotated
	// tag's commit among them: a ref's tip and its ancestors are left out,
	// and a tree or a blob is no commit. A name it cannot resolve, an
	// object the repository does not hold or an id of the other object
	// format, is left out too: git refuses a missing object itself, and
	// reads an id of its own format alone.
	out, err := g.runInput(ctx, access{}, []byte(strings.Join(ids, "\n")+"\n"), "--git-dir="+repo, "rev-list", "--ignore-missing", "--stdin", "--not", "--all")
	if ctxErr := ctx.Err(); ctxErr != nil {
		return fmt.Errorf("git upload-pack: %w", ctxErr)
	}
	if err != nil {
		return fmt.Errorf("check the wants: %w", err)
	}
	unreached := strings.Fields(string(out))
	if len(unreached) == 0 {
		if unreached, err = g.unreachedObjects(ctx, repo, ids); err != nil {
			return err
		}
	}
	if len(unreached) == 0 {
		return nil
	}
	// The want named is the first listed, or, when rev-list lists an
	// annotated tag's commit, the first of all.
	named := wants[0].ids[0]
	for _, w := range wants {
		if i := slices.IndexFunc(w.ids, func(id string) bool { return slices.Contains(unreached, id) }); i >= 0 {
			named = w.ids[i]
			break
		}
	}
	return &ServeError{Command: uploadPack, Stderr: "upload-pack: not our ref " + named, Err: errNotOurRef}
}

// unreachedObjects returns those of ids that name a tree, a blob or a tag
// of the repository repo that no ref reaches. A commit is left out, and so
// is an id the repository does not resolve. An object a ref points at is
// reached; the others are looked for among every object the refs reach.
func (g Git) unreachedObjects(ctx context.Context, repo string, ids []string) ([]string, error) {
	out, err := g.runInput(ctx, access{}, []byte(strings.Join(ids, "\n")+"\n"), "--git-dir="+repo, "cat-file", "--batch-check=%(objectname) %(objecttype)")
	if err != nil {
		return nil, g.checkFailed(ctx, err)
	}
	lines := strings.Split(strings.TrimSuffix(string(out), "\n"), "\n")
	if len(lines) != len(ids) {
		return nil, fmt.Errorf("check the wants: cat-file answered %d lines for %d ids", len(lines), len(ids))
	}
	// pending holds each object yet to be met, by its full name, with the
	// ids that name it.
	pending := map[string][]string{}
	for i, line := range lines {
		name, typ, _ := strings.Cut(line, " ")
		if typ == "tree" || typ == "blob" || typ == "tag" {
			pending[name] = append(pending[name], ids[i])
		}
	}
	if len(pending) > 0 {
		tips, err := g.run(ctx, access{}, "--git-dir="+repo, "for-each-ref", "--format=%(objectname)")
		if err != nil {
			return nil, g.checkFailed(ctx, err)
		}
		for _, tip := range strings.Fields(string(tips)) {
			delete(pending, tip)
		}
	}
	if len(pending) > 0 {
		if err := g.walkReached(ctx, repo, func(name string) bool {
			delete(pending, name)
			return len(pending) > 0
		}); err != nil {
			return nil, err
		}
	}
	var unreached []string
	for _, named := range pending {
		unreached = append(unreached, named...)
	}
	return unreached, nil
}

// walkReached gives more the name of every object the refs of the
// repository repo reach, HEAD's included, the trees and blobs of each
// commit after it, newest first, until more returns false, and then stops
// the walk. Reachability bitmaps, where the repository has them, answer in
// place of the walk.
func (g Git) walkReached(ctx context.Context, repo string, more func(name string) bool) error {
	dir, err := privateDir(false)
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	walk, stop := context.WithCancel(ctx)
	defer stop()
	cmd := g.command(walk, dir, access{}, []string{"--git-dir=" + repo, "rev-list", "--objects", "--no-object-names", "--in-commit-order", "--use-bitmap-index", "--all"})
	errOut := &limitedBuffer{limit: stderrLimit}
	cmd.Stderr = errOut
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("check the wants: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("check the wants: %w", err)
	}
	met := false
	for lines := bufio.NewScanner(stdout); lines.Scan(); {
		if !more(lines.Text()) {
			met = true
			break
		}
	}
	if met {
		stop()
	}
	err = cmd.Wait()
	switch {
	case met:
		return nil
	case err != nil:
		return g.checkFailed(ctx, fmt.Errorf("git rev-list: %w: %s", err, strings.TrimSpace(errOut.String())))
	}
	return ctx.Err()
}

// checkFailed is the error of a git command of the want check that failed
// with err: one wrapping ctx's error when ctx ended.
func (g Git) checkFailed(ctx context.Context, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return fmt.Errorf("git upload-pack: %w", ctxErr)
	}
	return fmt.Errorf("check the wants: %w", err)
}

// keepWants copies the request in to w, the whole of a version 2 request,
// a version 0 one up to the flush packet that ends its wants, and returns
// the wants it names when it is a version 2 fetch command or a version 0
// request, nil otherwise, or an error wrapping ctx's when ctx ends first,
// ErrRequestTooLarge when what it keeps holds more than limit bytes, and a
// *ServeError when a version 2 want names no id git can read. The copy runs
// on while in blocks, until in gives up.
func keepWants(ctx context.Context, w io.Writer, in io.Reader, limit int64, v2 bool) ([]want, error) {
	type kept struct {
		wants []want
		err   error
	}
	done := make(chan kept, 1)
	go func() {
		limited := &io.LimitedReader{R: in, N: limit + 1}
		wants, ended, err := scanWants(w, limited, v2)
		if err == nil && limited.N == 0 && (v2 || !ended) {
			err = fmt.Errorf("git upload-pack: %w", ErrRequestTooLarge)
		}
		done <- kept{wants, err}
	}()
	select {
	case k := <-done:
		var se *ServeError
		if k.err != nil && !errors.As(k.err, &se) && !errors.Is(k.err, ErrRequestTooLarge) {
			return nil, fmt.Errorf("keep the request: %w", k.err)
		}
		return k.wants, k.err
	case <-ctx.Done():
		return nil, fmt.Errorf("git upload-pack: %w", ctx.Err())
	}
}

// want is one want line of a fetch: the ids rev-list is given for it.
type want struct{ ids []string }

// scanWants is keepWants without the context and the bound. It reads a
// version 2 request as git's protocol version 2 server does: the command
// and the capabilities, in any order, up to the delimiter packet, then, for
// a fetch, its arguments; it stops looking at the end of the request, its
// flush packet, or at bytes that are not pkt-lines, and copies them all the
// same. It reads a version 0 request's want lines up to the flush packet
// that ends them, when it reports ended, or up to bytes that are not
// pkt-lines, and copies no further than it read: a want whose id git cannot
// read is left for git to refuse.
func scanWants(w io.Writer, in io.Reader, v2 bool) (wants []want, ended bool, err error) {
	r := bufio.NewReader(io.TeeReader(in, w))
	var fetch, args bool
	var refused error
	for {
		var head [4]byte
		if _, err := io.ReadFull(r, head[:]); err != nil {
			break
		}
		n, err := strconv.ParseUint(string(head[:]), 16, 16)
		if err != nil || n == 0 {
			ended = err == nil
			break
		}
		if n < 4 {
			if !v2 {
				break
			}
			if n == 1 && !args {
				if !fetch {
					break
				}
				args = true
			}
			continue
		}
		payload := make([]byte, n-4)
		if _, err := io.ReadFull(r, payload); err != nil {
			break
		}
		line := strings.TrimSuffix(string(payload), "\n")
		if v2 && !args {
			if cmd, ok := strings.CutPrefix(line, "command="); ok {
				fetch = cmd == "fetch"
			}
			continue
		}
		arg, ok := strings.CutPrefix(line, "want ")
		if !ok || refused != nil {
			continue
		}
		if ids := wantIDs(arg); ids != nil {
			wants = append(wants, want{ids})
		} else if v2 {
			refused = &ServeError{Command: uploadPack, Stderr: "upload-pack: protocol error, expected to get oid", Err: errBadWant}
		}
	}
	if !v2 {
		return wants, ended, nil
	}
	if _, err := io.Copy(io.Discard, r); err != nil {
		return nil, false, err
	}
	if refused != nil {
		return nil, false, refused
	}
	return wants, true, nil
}

// wantIDs are the ids git may read from the argument of a want line, nil
// when it reads none. git reads the full object id of the repository's
// format from the argument's start, in hexadecimal digits of either case,
// and ignores whatever follows: the leading 40 digits for SHA-1, 64 for
// SHA-256. Both are given, lowercased, where the argument has as many, and
// rev-list resolves the one of the repository's format.
func wantIDs(arg string) []string {
	n := 0
	for n < len(arg) && n < 64 && isHex(arg[n]) {
		n++
	}
	if n < 40 {
		return nil
	}
	ids := []string{strings.ToLower(arg[:40])}
	if n == 64 {
		ids = append(ids, strings.ToLower(arg[:64]))
	}
	return ids
}

func isHex(c byte) bool {
	return '0' <= c && c <= '9' || 'a' <= c && c <= 'f' || 'A' <= c && c <= 'F'
}
