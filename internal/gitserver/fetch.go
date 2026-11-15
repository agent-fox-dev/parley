package gitserver

import (
	"bufio"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/agent-fox-dev/parley/internal/config"
	"github.com/agent-fox-dev/parley/internal/gitrepo"
	"github.com/agent-fox-dev/parley/internal/server"
	"github.com/agent-fox-dev/parley/internal/server/timefmt"
)

// The [git] section of the configuration file.
const (
	// SectionName is the section the git server's keys live in.
	SectionName = "git"
	// DefaultTimeout is [git].timeout when the file does not set it.
	DefaultTimeout = 30 * time.Minute
)

// Register registers the keys of [git] with r: timeout, how long one git
// request may take, a task fork's ref copy and the run of upload-pack, or
// the push processor's work up to its ref transaction, together, a
// duration with a unit of s, m, h or d above zero (default 30m); and
// max_push_size, the largest pack a push may send, 0 for no limit (the
// default), a whole number of bytes or a whole number followed by KB, MB or
// GB. A value that breaks a rule fails the load with an error naming the
// key, which stops the start.
func Register(r *config.Registry) error {
	return errors.Join(
		r.Register(config.Key{Section: SectionName, Name: "timeout", Type: config.String, Default: "30m", Validate: validTimeout}),
		r.Register(config.Key{Section: SectionName, Name: "max_push_size", Type: config.String, Default: "0", Validate: validPushSize}),
	)
}

func validTimeout(v any) error {
	d, err := timefmt.ParseDuration(v.(string))
	if err != nil {
		return fmt.Errorf("must be a duration with a unit of s, m, h or d, such as 30m: %w", err)
	}
	if d <= 0 {
		return errors.New("must be more than 0")
	}
	return nil
}

// TimeoutFrom reads [git].timeout from cfg, loaded with the key Register
// registers; a key that was not loaded has its default.
func TimeoutFrom(cfg *config.Config) time.Duration {
	if d, err := timefmt.ParseDuration(cfg.String(SectionName, "timeout")); err == nil && d > 0 {
		return d
	}
	return DefaultTimeout
}

func validPushSize(v any) error {
	_, err := parsePushSize(v.(string))
	return err
}

// parsePushSize reads a value of [git].max_push_size: a whole number of
// bytes, 0 among them, or a size as [server].max_body_size takes one.
func parsePushSize(s string) (int64, error) {
	if s != "" && strings.Trim(s, "0123456789") == "" {
		if n, err := strconv.ParseInt(s, 10, 64); err == nil {
			return n, nil
		}
		return 0, errors.New("is too large")
	}
	n, err := config.ParseSize(s)
	if err != nil {
		return 0, errors.New("must be 0, a whole number of bytes, or a whole number followed by KB, MB or GB, such as 100MB")
	}
	return n, nil
}

// MaxPushSizeFrom reads [git].max_push_size from cfg, loaded with the key
// Register registers, in bytes; 0, also for a key that was not loaded, is
// no limit.
func MaxPushSizeFrom(cfg *config.Config) int64 {
	n, _ := parsePushSize(cfg.String(SectionName, "max_push_size"))
	return n
}

// Runner runs git for a client: upload-pack for ref discovery and the
// exchange of a fetch, receive-pack for ref discovery of a push.
// gitrepo.Git is one.
type Runner interface {
	UploadPack(ctx context.Context, req gitrepo.ServeRequest) error
	ReceivePack(ctx context.Context, req gitrepo.ServeRequest) error
}

// typeResult is the content type of the answer of a fetch exchange; an
// advertisement's is application/x-<service>-advertisement.
const typeResult = "application/x-git-upload-pack-result"

// The texts of the ERR packets the hub writes itself, in the answer of an
// exchange that has already begun, where git expects them.
const (
	errEmptyRequest = "empty request"
	errInvalidGzip  = "invalid gzip body"
	errTimedOut     = "request timed out"
	// errTooLarge answers a version 2 request larger than the hub keeps
	// for its want check (gitrepo.DefaultMaxRequest).
	errTooLarge = "request too large"
	// errGitFailed, after the name of git's command, upload-pack or
	// receive-pack, stands for git's text when git failed without one.
	errGitFailed = " failed"
)

// uploadPackConfig is the configuration of every run of upload-pack, given
// on its command line, so that every repository is served alike whatever
// its own configuration says: partial clone with the filters blob:none,
// blob:limit and tree alone, so that git refuses every other filter in
// band; and a want of a commit that is no ref's tip but that a ref
// reaches, which a client that fetches by object id needs under protocol
// version 0, where it asks only when the advertisement allows it. No ref is
// hidden: the hub's repositories hide none, and the configuration of the
// host takes no part.
var uploadPackConfig = []string{
	"uploadpack.allowFilter=true",
	"uploadpackfilter.allow=false",
	"uploadpackfilter.blob:none.allow=true",
	"uploadpackfilter.blob:limit.allow=true",
	"uploadpackfilter.tree.allow=true",
	"uploadpack.allowReachableSHA1InWant=true",
}

var errNoRunner = errors.New("no git runner is installed")

// exchange serves a request that passed every check and returns its
// result. Ref discovery is answered by git, upload-pack's or receive-pack's
// advertisement, and a fetch by upload-pack; a push is handed to the push
// processor, whose result it is.
func (c *Component) exchange(w http.ResponseWriter, r *http.Request, x Exchange) Result {
	switch {
	case x.Route == RouteReceivePack:
		return c.push(w, r, x)
	case c.deps.Runner == nil:
		c.fail(w, r, errNoRunner)
		return ResultError
	case x.Route == RouteInfoRefs:
		return c.advertise(w, r, x)
	default:
		return c.fetch(w, r, x)
	}
}

// advertise answers ref discovery with the ref advertisement of the
// service asked for: upload-pack's for a fetch, receive-pack's for a push.
// Under protocol version 0 the advertisement is preceded by the service
// line smart HTTP adds and a flush packet; under version 2 git's
// capability advertisement is the whole answer. A push is always served
// under version 0, whatever Git-Protocol says, since receive-pack speaks
// no version 2.
func (c *Component) advertise(w http.ResponseWriter, r *http.Request, x Exchange) Result {
	run, req := c.deps.Runner.UploadPack, gitrepo.ServeRequest{Dir: x.Repo, AdvertiseRefs: true, Config: uploadPackConfig}
	if x.Service == ServiceReceivePack {
		run, req.Config = c.deps.Runner.ReceivePack, nil
	} else {
		req.Protocol2 = protocol2(r.Header)
	}
	out := begin(w, "application/x-"+x.Service+"-advertisement")
	if !req.Protocol2 {
		_, _ = out.Write(append(pktLine("# service="+x.Service+"\n"), flushPkt...))
	}
	return c.run(r, x, out, run, req)
}

// fetch runs the fetch exchange: git reads the request body, decompressed
// when it came gzipped, and its answer goes to the client as git writes
// it. A body that is empty or is not valid gzip is answered in band, as git
// would answer it, without running git. The request is scanned as git
// reads it, and a served fetch that sent a pack is recorded as an event
// when the workspace audits its fetches.
func (c *Component) fetch(w http.ResponseWriter, r *http.Request, x Exchange) Result {
	// Git reads the request while its answer is being sent; without full
	// duplex the server would read the whole body before the first byte
	// of the answer.
	_ = http.NewResponseController(w).EnableFullDuplex()
	body, refusal := requestBody(r)
	out := begin(w, typeResult)
	if refusal != "" {
		out.writeErr(refusal)
		return ResultError
	}
	scan := &requestScanner{}
	res := c.run(r, x, out, c.deps.Runner.UploadPack, gitrepo.ServeRequest{
		Dir: x.Repo, Protocol2: protocol2(r.Header), Config: uploadPackConfig, Stdin: scanningReader{body, scan},
	})
	if res == ResultOK && out.scan.packSent {
		c.recordFetch(r, x, scan)
	}
	return res
}

// run runs git with serve, the runner's upload-pack or receive-pack, for
// req, its answer written to out, bounded by the request's deadline, which
// a fork's ref copy has already used of, and returns ok
// when git succeeded and error when it did not. When git fails or runs out
// of time before any pack data went out, and git wrote no error packet of
// its own, the answer ends with an ERR packet that says why: the hub's text
// for the timeout, git's own for a request git refused. A client that went
// away is told nothing.
func (c *Component) run(r *http.Request, x Exchange, out *stream, serve func(context.Context, gitrepo.ServeRequest) error, req gitrepo.ServeRequest) Result {
	ctx, cancel := context.WithDeadline(r.Context(), x.deadline)
	defer cancel()
	req.Stdout = out
	err := serve(ctx, req)
	if err == nil {
		return ResultOK
	}
	if r.Context().Err() != nil {
		return ResultError
	}
	text := gitText(err, x.Service, x.Repo)
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		text = errTimedOut
	}
	c.deps.Logger.LogAttrs(r.Context(), slog.LevelWarn, "git exchange failed",
		slog.String("request_id", server.RequestID(r.Context())),
		slog.String("route", r.Pattern),
		slog.String("repository", r.PathValue("repo")),
		slog.String("error", err.Error()),
	)
	if !out.scan.packSent && !out.scan.errSent {
		out.writeErr(text)
	}
	return ResultError
}

// timeout is how long one git request may take.
func (c *Component) timeout() time.Duration {
	if c.deps.Timeout <= 0 {
		return DefaultTimeout
	}
	return c.deps.Timeout
}

// gitText is the text of the ERR packet for err, a failure of git: the
// last line git wrote to its standard error, without git's fatal: or
// error: prefix and with the repository's place on disk left out, which is
// no business of the client's; that the command of service failed when git
// wrote nothing, or when the line names another place on disk, such as the
// object directory of a fork's parent; and the hub's own text for a request
// refused before git ran because it is too large.
func gitText(err error, service, repo string) string {
	failed := strings.TrimPrefix(service, "git-") + errGitFailed
	var se *gitrepo.ServeError
	switch {
	case errors.Is(err, gitrepo.ErrRequestTooLarge):
		return errTooLarge
	case !errors.As(err, &se) || se.Stderr == "":
		return failed
	}
	line := se.Stderr[strings.LastIndexByte(se.Stderr, '\n')+1:]
	for _, prefix := range []string{"fatal: ", "error: "} {
		line = strings.TrimPrefix(line, prefix)
	}
	if repo != "" {
		line = strings.ReplaceAll(line, repo, "repository")
	}
	if namesAbsolutePath(line) {
		return failed
	}
	return line
}

// namesAbsolutePath reports whether line holds an absolute path: a slash
// that starts the line or follows a space, a quote or a parenthesis. A ref
// name, such as refs/heads/main, is no such path.
func namesAbsolutePath(line string) bool {
	for i := range len(line) {
		if line[i] == '/' && (i == 0 || strings.IndexByte(" \t'\"`(", line[i-1]) >= 0) {
			return true
		}
	}
	return false
}

// protocol2 reports whether the Git-Protocol header asks for protocol
// version 2: one of its entries, separated by colons, is version=2.
// Every other value, and no header, is version 0.
func protocol2(h http.Header) bool {
	for _, v := range h.Values("Git-Protocol") {
		for entry := range strings.SplitSeq(v, ":") {
			if strings.TrimSpace(entry) == "version=2" {
				return true
			}
		}
	}
	return false
}

// requestBody is the body of an upload-pack request as git is to read it,
// decompressed when Content-Encoding says gzip, or the text of the ERR
// packet that answers a body that is empty or not valid gzip.
func requestBody(r *http.Request) (io.Reader, string) {
	body := bufio.NewReader(r.Body)
	if _, err := body.Peek(1); err != nil {
		return nil, errEmptyRequest
	}
	if enc := strings.ToLower(strings.TrimSpace(r.Header.Get("Content-Encoding"))); enc != "gzip" && enc != "x-gzip" {
		return body, ""
	}
	zr, err := gzip.NewReader(body)
	if err != nil {
		return nil, errInvalidGzip
	}
	unzipped := bufio.NewReader(zr)
	if _, err := unzipped.Peek(1); errors.Is(err, io.EOF) {
		return nil, errEmptyRequest
	} else if err != nil {
		return nil, errInvalidGzip
	}
	return unzipped, ""
}

// begin starts the answer of an exchange, 200 with contentType and
// Cache-Control: no-cache, since an advertisement and a result are only
// ever true at the moment they are made, and returns the stream its body
// is written to.
func begin(w http.ResponseWriter, contentType string) *stream {
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	return &stream{w: w, rc: http.NewResponseController(w)}
}

// stream is the body of an exchange's answer. Every write goes to the
// client at once, flushed, so that a client sees git's progress and the
// first bytes of a pack while git is still at work, and the scanner sees
// the pkt-lines go by.
type stream struct {
	w    io.Writer
	rc   *http.ResponseController
	scan pktScanner
}

func (s *stream) Write(p []byte) (int, error) {
	n, err := s.w.Write(p)
	s.scan.feed(p[:n])
	if err != nil {
		return n, err
	}
	if err := s.rc.Flush(); err != nil && !errors.Is(err, http.ErrNotSupported) {
		return n, err
	}
	return n, nil
}

// writeErr ends the answer with the ERR packet of text.
func (s *stream) writeErr(text string) {
	_, _ = s.Write(pktLine("ERR " + text))
}
