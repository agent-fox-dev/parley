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
	"sync"
	"sync/atomic"
	"time"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/gitrepo"
	"github.com/agent-fox-dev/parley/internal/gitserver/pushrule"
	"github.com/agent-fox-dev/parley/internal/server"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

// OptionsInput is what the options stage is told of a push, before any of
// its pack is read: the workspace record and the caller as the checks
// established them, the ref updates in command order and the push options
// as the client sent them.
type OptionsInput struct {
	Workspace workspace.Workspace
	Caller    PushCaller
	Updates   []pushrule.Update
	Options   []string
}

// OptionsAnswer is the options stage's answer. A Refusal that is not ""
// refuses the whole push with that message. SessionID, nil for none, is the
// session the push is written for, which the ref-updated stages and the
// push's events receive. OpensChange marks every update of the push as one
// that opens a change, which decides what a path-scoped token is held to.
type OptionsAnswer struct {
	Refusal     string
	SessionID   *string
	OpensChange bool
}

// OptionsStage judges the push options of a push before its pack is read.
// An error refuses the push as internal.
type OptionsStage interface {
	CheckOptions(ctx context.Context, in OptionsInput) (OptionsAnswer, error)
}

// MappingInput is what the mapping stage is told of an accepted push, once
// its refs hold their new values: the workspace record, the caller, the
// push options, the session the options stage set, and what the ref writer
// made of each update, in command order, with its direct flag and the
// version a ref-updated stage recorded for it.
type MappingInput struct {
	Workspace workspace.Workspace
	Caller    PushCaller
	Options   []string
	SessionID *string
	Updates   []pushrule.UpdateResult
}

// MappingStage maps an accepted push to what it means beyond its refs and
// returns the lines to show the client, each sent on the side band before
// the status report. It runs after the refs moved, so it cannot refuse the
// push; it words its own failures as lines.
type MappingStage interface {
	MapPush(ctx context.Context, in MappingInput) []string
}

// ReceiverDeps are what the push processor works with: the push rule
// engine, the ref writer, the hub's git, which opens a workspace's
// repository and runs git index-pack, the place of each workspace's
// repository on disk, the largest pack a push may send in bytes
// ([git].max_push_size, 0 for no limit) and the logger of the failures
// that are not the client's doing.
type ReceiverDeps struct {
	Engine      *pushrule.Engine
	Writer      *pushrule.Writer
	Git         gitrepo.Git
	RepoPath    func(slug string) string
	MaxPushSize int64
	Logger      *slog.Logger
}

// Receiver is the push processor behind git-receive-pack: it reads the
// commands and push options of a push that passed the git server's checks,
// asks the options stage, receives the pack into a quarantine, has the push
// rules decide every ref update, has the ref writer write all of them or
// none, asks the mapping stage of an accepted push, has the ref writer
// write its events and tell its post-push listeners, and answers with
// git's status report, carrying the rules' message for each ref. git
// receive-pack is not run: its report cannot carry those messages.
//
// The stages are registered on the instance while the hub is composed
// (docs/adr/08-register-the-push-collaborators-on-the-ref-writer.md); with
// none registered every push option is accepted and no line is added.
type Receiver struct {
	mu      sync.RWMutex
	deps    ReceiverDeps
	options OptionsStage
	mapping MappingStage
}

// Bind sets what the processor works with from then on.
func (p *Receiver) Bind(d ReceiverDeps) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.deps = d
}

// RegisterOptionsStage makes s the options stage, replacing the one
// registered before; nil removes it.
func (p *Receiver) RegisterOptionsStage(s OptionsStage) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.options = s
}

// RegisterMappingStage makes s the mapping stage, replacing the one
// registered before; nil removes it.
func (p *Receiver) RegisterMappingStage(s MappingStage) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.mapping = s
}

// msgInvalidRequest answers a push whose command list git would not have
// sent.
const msgInvalidRequest = "invalid receive-pack request"

// ProcessPush processes one push, writes the whole answer and returns its
// result: ok for an accepted push and for one without commands, refused
// for one the options stage, a rule or the compare-and-swap refused, error
// for a pack that cannot be read or is larger than MaxPushSize, for a push
// whose request context ended before its ref transaction started, and for
// a failure of the hub's. A command list that cannot be read is answered
// 400, since it has no ref to report on.
//
// The request's context bounds every stage before the ref transaction: the
// git server ends it at [git].timeout. When it ends there, the push's git
// processes are killed with it, its quarantine is discarded, nothing
// changes and every ref is reported as the push timed out. Once the ref
// transaction has started, the push is finished whatever the context says.
func (p *Receiver) ProcessPush(w http.ResponseWriter, r *http.Request, caller PushCaller, ws workspace.Workspace) Result {
	p.mu.RLock()
	run := &push{deps: p.deps, options: p.options, mapping: p.mapping, caller: caller, ws: ws}
	p.mu.RUnlock()
	if run.deps.Logger == nil {
		run.deps.Logger = slog.New(slog.DiscardHandler)
	}
	if deadline, ok := r.Context().Deadline(); ok {
		// A read of the body waits for the client however the context
		// ends; a deadline on the connection ends the read with it.
		_ = http.NewResponseController(w).SetReadDeadline(deadline)
	}
	body, err := pushBody(r)
	if err == nil {
		err = run.readCommands(body)
	}
	if err != nil {
		refuse(w, http.StatusBadRequest, msgInvalidRequest)
		return ResultError
	}
	if len(run.updates) == 0 {
		begin(w, receivePackResult)
		return ResultOK
	}
	rep, res := run.process(r.Context(), &packReader{r: body, max: run.deps.MaxPushSize})
	run.caps.write(w, rep)
	return res
}

// push is one push being processed.
type push struct {
	deps    ReceiverDeps
	options OptionsStage
	mapping MappingStage
	caller  PushCaller
	ws      workspace.Workspace

	caps     capabilities
	updates  []pushrule.Update
	pushOpts []string
}

// process runs the stages of a push with at least one command, its
// commands and options read, its pack to be read from body, and returns its
// report and result.
func (run *push) process(ctx context.Context, body *packReader) (report, Result) {
	answer := OptionsAnswer{}
	if run.options != nil {
		var err error
		answer, err = run.options.CheckOptions(ctx, OptionsInput{run.ws, run.caller, run.updates, run.pushOpts})
		switch {
		case expired(ctx):
			return run.timedOut()
		case err != nil:
			_, _ = io.Copy(io.Discard, body)
			return run.internal(ctx, "options stage", err)
		case answer.Refusal != "":
			// The client sends its whole pack before it reads the answer.
			_, _ = io.Copy(io.Discard, body)
			return refuseAll(run.updates, answer.Refusal), ResultRefused
		}
	}
	for i := range run.updates {
		run.updates[i].OpensChange = answer.OpensChange
	}
	repo, err := run.deps.Git.Open(ctx, run.repoPath(run.ws.Slug))
	if err != nil {
		return run.failed(ctx, "open the repository", err)
	}
	view := repo
	var beforeCommit, afterAbort func(context.Context) error
	if afters := run.afters(); len(afters) > 0 {
		q, err := repo.NewQuarantine()
		if err != nil {
			return run.failed(ctx, "quarantine", err)
		}
		// An accepted push's objects were moved out first, so this removes
		// an empty directory; a refused push's objects go with it.
		defer func() { _ = q.Discard() }()
		if ok := run.unpack(ctx, q, body, afters); !ok {
			switch {
			case body.over():
				return failAll(run.updates, unpackTooLarge, msgUnpackerErr), ResultError
			case expired(ctx):
				return run.timedOut()
			}
			return failAll(run.updates, unpackInvalid, msgUnpackerErr), ResultError
		}
		// A push stopped once its objects moved in takes them out again,
		// so a refused push leaves none in the repository.
		view = q.View()
		beforeCommit = func(context.Context) error { return q.Migrate() }
		afterAbort = func(context.Context) error { return q.Unmigrate() }
	} else {
		_, _ = io.Copy(io.Discard, body)
	}
	pw, pc := run.ruleFacts()
	req := pushrule.Request{Workspace: pw, Caller: pc, Updates: run.updates, Repo: view}
	if run.ws.Kind == access.KindFork && run.ws.ParentSlug != nil {
		req.ParentRepo = run.repoPath(*run.ws.ParentSlug)
	}
	verdict := run.engine().Evaluate(ctx, req)
	if expired(ctx) {
		return run.timedOut()
	}
	// The ref writer stops at ctx's end only up to its ref transaction.
	res, err := run.writer().Apply(ctx, pushrule.ApplyRequest{
		Verdict: verdict, Repo: view, Workspace: pw, Caller: pc, SessionID: answer.SessionID,
		Via: pushrule.ViaGit, VersionKind: pushrule.VersionKindPush, BeforeCommit: beforeCommit, AfterAbort: afterAbort,
	})
	if res.Outcome == pushrule.OutcomeInternal && expired(ctx) {
		return run.timedOut()
	}
	if err != nil {
		run.logFailure(ctx, "write the refs", err)
	}
	rep := reportOf(res)
	if res.Refused() {
		if res.Outcome == pushrule.OutcomeInternal {
			return rep, ResultError
		}
		return rep, ResultRefused
	}
	// The refs have moved: the push is finished whatever ctx says.
	ctx = context.WithoutCancel(ctx)
	if run.mapping != nil {
		rep.band = append(rep.band, run.mapping.MapPush(ctx, MappingInput{run.ws, run.caller, run.pushOpts, answer.SessionID, res.Updates})...)
	}
	// The events come after the mapping stage, which may open changes, and
	// before the answer, so a client that has read it finds them.
	run.writer().Finish(ctx, res)
	return rep, ResultOK
}

// unpack receives the pack in body into q and reports whether every new
// value of the push names an object, in the pack or the repository.
func (run *push) unpack(ctx context.Context, q *gitrepo.Quarantine, body io.Reader, afters []string) bool {
	if err := q.Receive(ctx, body); err != nil {
		return false
	}
	missing, err := q.View().Missing(ctx, afters)
	return err == nil && len(missing) == 0
}

// afters are the new values of the push's commands that are not deletes,
// which its pack must provide unless the repository has them.
func (run *push) afters() []string {
	var ids []string
	for _, u := range run.updates {
		if u.After != pushrule.ZeroID {
			ids = append(ids, u.After)
		}
	}
	return ids
}

// failed reports a failure of step before the ref writer was asked: as the
// push timed out when ctx has ended, else as internal.
func (run *push) failed(ctx context.Context, step string, err error) (report, Result) {
	if expired(ctx) {
		return run.timedOut()
	}
	return run.internal(ctx, step, err)
}

// timedOut reports every ref of a push whose context ended before its ref
// transaction started.
func (run *push) timedOut() (report, Result) {
	return failAll(run.updates, unpackTimedOut, msgUnpackerErr), ResultError
}

// expired reports whether ctx, a push's, has ended: its deadline has
// passed, which a read of the body may notice first, or the client is gone.
func expired(ctx context.Context) bool {
	deadline, ok := ctx.Deadline()
	return ctx.Err() != nil || ok && !time.Now().Before(deadline)
}

// packReader is the rest of a push's body once its commands and options
// are read, which holds its pack. With max above 0 it reads at most max
// bytes and one more: the one more tells a pack over the limit, and the
// read stops there.
type packReader struct {
	r    io.Reader
	max  int64
	read atomic.Int64
}

var errPackTooLarge = errors.New("the pack exceeds the maximum push size")

func (p *packReader) Read(b []byte) (int, error) {
	if p.max <= 0 {
		return p.r.Read(b)
	}
	if p.over() {
		return 0, errPackTooLarge
	}
	if rest := p.max + 1 - p.read.Load(); int64(len(b)) > rest {
		b = b[:rest]
	}
	n, err := p.r.Read(b)
	if p.read.Add(int64(n)) > p.max {
		return n, errPackTooLarge
	}
	return n, err
}

// over reports whether more than max bytes were read.
func (p *packReader) over() bool { return p.max > 0 && p.read.Load() > p.max }

// internal reports every ref of the push refused as internal, for a failure
// of the hub's before the ref writer was asked, and logs it.
func (run *push) internal(ctx context.Context, step string, err error) (report, Result) {
	run.logFailure(ctx, step, err)
	rep := failAll(run.updates, unpackOK, msgPushInternal)
	for _, u := range run.updates {
		rep.band = append(rep.band, "error: "+u.Ref+": "+msgPushInternal)
	}
	return rep, ResultError
}

func (run *push) logFailure(ctx context.Context, step string, err error) {
	run.deps.Logger.LogAttrs(ctx, slog.LevelError, "git push failed",
		slog.String("request_id", server.RequestID(ctx)),
		slog.String("workspace", run.ws.Slug),
		slog.String("step", step),
		slog.String("error", err.Error()),
	)
}

// ruleFacts are the workspace and the caller as the push rules take them.
func (run *push) ruleFacts() (pushrule.Workspace, pushrule.Caller) {
	ws, c := run.ws, run.caller
	pw := pushrule.Workspace{Slug: ws.Slug, Kind: ws.Kind, Mode: ws.Mode}
	if ws.ParentSlug != nil {
		pw.Parent = *ws.ParentSlug
	}
	if ws.DefaultBranch != nil {
		pw.DefaultBranch = *ws.DefaultBranch
	}
	pc := pushrule.Caller{Level: c.Level, CredentialType: c.CredentialType, CredentialID: c.CredentialID, UserID: c.UserID}
	if c.PAT != nil {
		pc.Bound, pc.Paths = c.PAT.Workspace != nil, c.PAT.Paths
	}
	return pw, pc
}

func (run *push) repoPath(slug string) string {
	if run.deps.RepoPath == nil {
		return ""
	}
	return run.deps.RepoPath(slug)
}

func (run *push) engine() *pushrule.Engine {
	if run.deps.Engine == nil {
		return &pushrule.Engine{}
	}
	return run.deps.Engine
}

func (run *push) writer() *pushrule.Writer {
	if run.deps.Writer == nil {
		return &pushrule.Writer{}
	}
	return run.deps.Writer
}

// pushBody is the body of a push as it is to be read, decompressed when
// Content-Encoding says gzip.
func pushBody(r *http.Request) (*bufio.Reader, error) {
	body := bufio.NewReader(r.Body)
	if enc := strings.ToLower(strings.TrimSpace(r.Header.Get("Content-Encoding"))); enc != "gzip" && enc != "x-gzip" {
		return body, nil
	}
	zr, err := gzip.NewReader(body)
	if err != nil {
		return nil, err
	}
	return bufio.NewReader(zr), nil
}

var errBadCommand = errors.New("bad command")

// readCommands reads the command list of a push from body up to its flush
// packet, the capabilities on its first command, and the push options
// that follow when the client announced them. An empty list, a body of
// only a flush packet, has no options.
func (run *push) readCommands(body *bufio.Reader) error {
	for {
		line, flush, err := readPkt(body)
		if err != nil {
			return err
		}
		if flush {
			break
		}
		if len(run.updates) == 0 {
			var list string
			line, list, _ = strings.Cut(line, "\x00")
			run.caps = parseCapabilities(list)
		}
		u, err := parseCommand(strings.TrimSuffix(line, "\n"))
		if err != nil {
			return err
		}
		run.updates = append(run.updates, u)
	}
	if len(run.updates) == 0 || !run.caps.pushOptions {
		return nil
	}
	for {
		line, flush, err := readPkt(body)
		if err != nil || flush {
			return err
		}
		run.pushOpts = append(run.pushOpts, strings.TrimSuffix(line, "\n"))
	}
}

// parseCommand reads one command, "<old> <new> <ref>".
func parseCommand(line string) (pushrule.Update, error) {
	f := strings.Split(line, " ")
	if len(f) != 3 || !isObjectID(f[0]) || !isObjectID(f[1]) || !strings.HasPrefix(f[2], "refs/") ||
		f[0] == pushrule.ZeroID && f[1] == pushrule.ZeroID {
		return pushrule.Update{}, fmt.Errorf("%w: %q", errBadCommand, line)
	}
	return pushrule.Update{Ref: f[2], Before: f[0], After: f[1]}, nil
}

// isObjectID reports whether s is an object id as the hub's repositories
// name them: forty lower-case hexadecimal digits.
func isObjectID(s string) bool {
	if len(s) != len(pushrule.ZeroID) {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// readPkt reads one pkt-line from r: its payload, or flush true for a
// flush packet.
func readPkt(r io.Reader) (payload string, flush bool, err error) {
	var head [4]byte
	if _, err := io.ReadFull(r, head[:]); err != nil {
		return "", false, err
	}
	n, err := strconv.ParseUint(string(head[:]), 16, 16)
	switch {
	case err != nil:
		return "", false, fmt.Errorf("pkt-line length %q: %w", head, err)
	case n == 0:
		return "", true, nil
	case n < 4:
		return "", false, fmt.Errorf("pkt-line length %d", n)
	}
	buf := make([]byte, n-4)
	if _, err := io.ReadFull(r, buf); err != nil {
		return "", false, err
	}
	return string(buf), false, nil
}
