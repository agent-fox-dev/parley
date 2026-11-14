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

// Register registers [git].timeout with r: how long one run of upload-pack
// may take, a duration with a unit of s, m, h or d above zero (default
// 30m). A value that breaks the rule fails the load with an error naming
// the key, which stops the start.
func Register(r *config.Registry) error {
	return r.Register(config.Key{Section: SectionName, Name: "timeout", Type: config.String, Default: "30m", Validate: validTimeout})
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

// Runner runs git upload-pack for a client: gitrepo.Git is one.
type Runner interface {
	UploadPack(ctx context.Context, req gitrepo.ServeRequest) error
}

// The content types of the two answers of a fetch.
const (
	typeAdvertisement = "application/x-git-upload-pack-advertisement"
	typeResult        = "application/x-git-upload-pack-result"
)

// The texts of the ERR packets the hub writes itself, in the answer of an
// exchange that has already begun, where git expects them.
const (
	errEmptyRequest = "empty request"
	errInvalidGzip  = "invalid gzip body"
	errTimedOut     = "request timed out"
	// errGitFailed stands for git's text when git failed without one.
	errGitFailed = "upload-pack failed"
)

var (
	errNoRunner      = errors.New("no git runner is installed")
	errNoPushService = errors.New("no push service is installed")
)

// exchange serves a request that passed every check with git. A fetch is
// served by upload-pack; a push has no service yet and is answered 500.
func (c *Component) exchange(w http.ResponseWriter, r *http.Request, x Exchange) {
	switch {
	case x.Service != ServiceUploadPack:
		c.fail(w, r, errNoPushService)
	case c.deps.Runner == nil:
		c.fail(w, r, errNoRunner)
	case x.Route == RouteInfoRefs:
		c.advertise(w, r, x)
	default:
		c.fetch(w, r, x)
	}
}

// advertise answers ref discovery for a fetch with upload-pack's ref
// advertisement. Under protocol version 0 the advertisement is preceded by
// the service line smart HTTP adds and a flush packet; under version 2
// git's capability advertisement is the whole answer.
func (c *Component) advertise(w http.ResponseWriter, r *http.Request, x Exchange) {
	v2 := protocol2(r.Header)
	out := begin(w, typeAdvertisement)
	if !v2 {
		_, _ = out.Write(append(pktLine("# service="+ServiceUploadPack+"\n"), flushPkt...))
	}
	c.run(r, x, out, gitrepo.ServeRequest{Dir: x.Repo, AdvertiseRefs: true, Protocol2: v2})
}

// fetch runs the fetch exchange: git reads the request body, decompressed
// when it came gzipped, and its answer goes to the client as git writes
// it. A body that is empty or is not valid gzip is answered in band, as git
// would answer it, without running git.
func (c *Component) fetch(w http.ResponseWriter, r *http.Request, x Exchange) {
	// Git reads the request while its answer is being sent; without full
	// duplex the server would read the whole body before the first byte
	// of the answer.
	_ = http.NewResponseController(w).EnableFullDuplex()
	body, refusal := requestBody(r)
	out := begin(w, typeResult)
	if refusal != "" {
		out.writeErr(refusal)
		return
	}
	c.run(r, x, out, gitrepo.ServeRequest{Dir: x.Repo, Protocol2: protocol2(r.Header), Stdin: body})
}

// run runs upload-pack for req, its answer written to out, bounded by the
// timeout. When git fails or runs out of time before any pack data went
// out, and git wrote no error packet of its own, the answer ends with an ERR
// packet that says why: the hub's text for the timeout, git's own for a
// request git refused. A client that went away is told nothing.
func (c *Component) run(r *http.Request, x Exchange, out *stream, req gitrepo.ServeRequest) {
	ctx, cancel := context.WithTimeout(r.Context(), c.timeout())
	defer cancel()
	req.Stdout = out
	err := c.deps.Runner.UploadPack(ctx, req)
	if err == nil || r.Context().Err() != nil {
		return
	}
	text := gitText(err, x.Repo)
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
}

// timeout is how long one run of upload-pack may take.
func (c *Component) timeout() time.Duration {
	if c.deps.Timeout <= 0 {
		return DefaultTimeout
	}
	return c.deps.Timeout
}

// gitText is the text of the ERR packet for err, a failure of git: the
// last line git wrote to its standard error, without git's fatal: or
// error: prefix and with the repository's place on disk left out, which is
// no business of the client's.
func gitText(err error, repo string) string {
	var se *gitrepo.ServeError
	if !errors.As(err, &se) || se.Stderr == "" {
		return errGitFailed
	}
	line := se.Stderr[strings.LastIndexByte(se.Stderr, '\n')+1:]
	for _, prefix := range []string{"fatal: ", "error: "} {
		line = strings.TrimPrefix(line, prefix)
	}
	if repo != "" {
		line = strings.ReplaceAll(line, repo, "repository")
	}
	return line
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
