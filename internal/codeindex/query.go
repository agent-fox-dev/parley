package codeindex

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"os"
	pathpkg "path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/agent-fox-dev/agentkit-go/outline"

	"github.com/agent-fox-dev/parley/internal/jobs"
	"github.com/agent-fox-dev/parley/internal/metrics"
	"github.com/agent-fox-dev/parley/internal/scm"
	"github.com/agent-fox-dev/parley/internal/server"
)

// The ops the two query routes are counted under.
const (
	opSearch  = "code_search"
	opSymbols = "symbols"
)

// The bounds of a code search: its query, its files, and the matches and
// the text of a match it answers.
const (
	maxQueryChars     = 500
	maxQueryBytes     = 1024
	defaultSearchSize = 20
	maxFileMatches    = 20
	maxMatchChars     = 500
)

// retryAfterBuild is the Retry-After, in seconds, of a request refused
// while a build of the index runs or waits.
const retryAfterBuild = 5

// errReadTimeout is the cause of the end of a query's context at the read
// timeout, which tells it from a client that went away.
var errReadTimeout = errors.New("the read timed out")

// query is h bounded by the read timeout and counted under op, as the
// source control reads are: a query that outlasts the timeout without an
// answer of its own is answered 503 scm_timeout.
func (c *Component) query(op string, h server.HandlerFunc) server.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) error {
		ctx, cancel := context.WithTimeoutCause(r.Context(), c.deps.ReadTimeout, errReadTimeout)
		defer cancel()
		// A refusal carries none of the caching headers its handler set
		// before it failed.
		uncached := scm.Uncached(w.Header())
		err := h(w, r.WithContext(ctx))
		if err != nil {
			uncached()
		}
		if _, answered := errors.AsType[*server.Error](err); err != nil && !answered && context.Cause(ctx) == errReadTimeout {
			err = server.NewError("scm_timeout", http.StatusServiceUnavailable,
				fmt.Sprintf("the read did not finish within the read timeout of %s", c.deps.ReadTimeout))
		}
		result := "ok"
		if err != nil {
			result = "error"
		}
		_ = c.deps.Metrics.Add(metrics.SCMRequestsTotal, map[string]string{"op": op, "result": result}, 1)
		return err
	}
}

// target is what a query is answered from: the workspace whose index it
// is, the head of its default branch when the request arrived, and its
// record once the waiting rule is satisfied.
type target struct {
	ws   Workspace
	head string
	rec  Record
}

// prepare runs the checks the two query routes share, in their order: the
// feature, the request's parameters (params), the workspace's setting, its
// head, and the waiting rule (await). A fork is answered from its parent,
// whose setting, head and index these are.
func (c *Component) prepare(w http.ResponseWriter, r *http.Request, params func(url.Values) error) (target, error) {
	if !c.Enabled() {
		return target{}, server.NewError("code_index_disabled", http.StatusNotFound,
			"this hub keeps no code index: its configuration has no ["+Section+"]")
	}
	if err := params(r.URL.Query()); err != nil {
		return target{}, err
	}
	ctx := r.Context()
	ws, found, err := c.deps.Workspace(ctx, server.Param(r, "slug"))
	if err == nil && found && ws.Parent != "" {
		ws, found, err = c.deps.Workspace(ctx, ws.Parent)
	}
	if err != nil {
		return target{}, err
	}
	if !found {
		// Deleted since the access check.
		return target{}, server.NotFound("workspace not found")
	}
	if !ws.Indexed {
		return target{}, server.NewError("code_index_off", http.StatusConflict,
			fmt.Sprintf("workspace %q has its code index off; set %s to true to search it", ws.Slug, SettingName))
	}
	head, err := c.headOf(ctx, ws)
	if err != nil {
		return target{}, err
	}
	rec, err := c.await(w, r, ws, head)
	if err != nil {
		return target{}, err
	}
	return target{ws: ws, head: head, rec: rec}, nil
}

// The outcomes a query that reached the waiting rule is observed under.
const (
	waitReady   = "ready"
	waitTimeout = "timeout"
	waitFailed  = "failed"
)

// await is the waiting rule: it returns the record of ws once its index
// holds arrival, the head of the default branch when the request arrived,
// or a head the branch reached after it, and never one of an older commit.
// It waits for the build, which it queues when no one else did, for as long
// as the request's context lasts, woken by the settlement of each build.
// When the context ends at the read timeout the request is refused, 503
// code_index_not_ready with the state building; when the last build of the
// head failed and waits for its next attempt, it is refused at once with
// the state failed and the time to that attempt.
func (c *Component) await(w http.ResponseWriter, r *http.Request, ws Workspace, arrival string) (Record, error) {
	ctx, start := r.Context(), c.deps.Now()
	observe := func(outcome string) {
		_ = c.deps.Metrics.Observe(metrics.CodeIndexWaitSeconds, map[string]string{"outcome": outcome}, c.deps.Now().Sub(start).Seconds())
	}
	for {
		// Taken before the record is read: a settlement after this wakes.
		settled := c.settlements()
		rec, _, err := c.Record(ctx, ws.Slug)
		if err != nil {
			return Record{}, err
		}
		head, err := c.headOf(ctx, ws)
		if err != nil {
			return Record{}, err
		}
		if rec.IndexedSHA != "" && (rec.IndexedSHA == arrival || rec.IndexedSHA == head) && c.stored(ws.Slug, rec.IndexedSHA) {
			observe(waitReady)
			return rec, nil
		}
		pending, err := c.deps.Jobs.List(ctx, JobType, ws.Slug, "", nil, 4)
		if err != nil {
			return Record{}, err
		}
		var failed *jobs.Job
		building := false
		for i, j := range pending {
			switch j.Status {
			case jobs.StatusQueued, jobs.StatusRunning:
				building = true
			case jobs.StatusFailed:
				failed = &pending[i]
			}
		}
		switch {
		case building:
		case failed != nil && rec.FailedSHA == head:
			retry := 1
			if failed.NextAttemptAt != nil {
				retry = max(1, int(math.Ceil(failed.NextAttemptAt.Sub(c.deps.Now()).Seconds())))
			}
			observe(waitFailed)
			return Record{}, c.notReady(w, StateFailed, retry, head, rec)
		default:
			// No build is on its way: the notice of this head was lost, or
			// the failed build is of an older one.
			if err := c.Queue(ctx, ws.Slug); err != nil {
				return Record{}, err
			}
		}
		select {
		case <-settled:
		case <-ctx.Done():
			if context.Cause(ctx) != errReadTimeout {
				return Record{}, context.Cause(ctx)
			}
			observe(waitTimeout)
			return Record{}, c.notReady(w, StateBuilding, retryAfterBuild, head, rec)
		}
	}
}

// headOf is the head of the default branch of ws. A workspace without one,
// whose repository is not there yet or holds no commit, is answered as the
// source control reads answer a missing revision.
func (c *Component) headOf(ctx context.Context, ws Workspace) (string, error) {
	noHead := server.NewError("ref_not_found", http.StatusNotFound, `revision "" names no commit`)
	if ws.DefaultBranch == "" {
		return "", noHead
	}
	repo, err := c.deps.Repos(ctx, ws.Slug)
	if err != nil {
		return "", noHead
	}
	head, found, err := repo.ResolveBranch(ctx, ws.DefaultBranch)
	if err != nil {
		return "", err
	}
	if !found {
		return "", noHead
	}
	return head, nil
}

// notReady is the refusal of a query whose index does not hold the head:
// 503 code_index_not_ready with the state of the index, the head and the
// commit the index there is holds, and when to ask again.
func (c *Component) notReady(w http.ResponseWriter, state string, retryAfter int, head string, rec Record) error {
	w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
	var indexed any
	if rec.IndexedSHA != "" {
		indexed = rec.IndexedSHA
	}
	return server.NewError("code_index_not_ready", http.StatusServiceUnavailable,
		fmt.Sprintf("the code index does not hold %s yet", head)).
		With("state", state).With("head_sha", head).With("indexed_sha", indexed)
}

// searchFile is a file of a code search's answer.
type searchFile struct {
	Path     string        `json:"path"`
	Language string        `json:"language"`
	Matches  []searchMatch `json:"matches"`
}

// searchMatch is a line that matches.
type searchMatch struct {
	Line int    `json:"line"`
	Text string `json:"text"`
}

// searchAnswer is the answer of the code search route.
type searchAnswer struct {
	Workspace  string       `json:"workspace"`
	IndexedSHA string       `json:"indexed_sha"`
	Partial    bool         `json:"partial"`
	Truncated  bool         `json:"truncated"`
	Files      []searchFile `json:"files"`
}

// getSearch answers GET …/scm/code-search: the files of the default
// branch that match q, a query in the index's syntax, best first, each with
// its language and its matching lines, from the index of the head.
func (c *Component) getSearch(w http.ResponseWriter, r *http.Request) error {
	var q string
	limit := defaultSearchSize
	t, err := c.prepare(w, r, func(v url.Values) (err error) {
		q = v.Get("q")
		switch {
		case strings.TrimSpace(q) == "":
			return server.InvalidRequest("q is required: a query of 1 to 500 characters in the index's syntax")
		case utf8.RuneCountInString(q) > maxQueryChars || len(q) > maxQueryBytes:
			return server.InvalidRequest("q must be at most 500 characters and 1,024 bytes")
		}
		limit, err = limitParam(v, defaultSearchSize, maxSearchFiles)
		return err
	})
	if err != nil {
		return err
	}
	ctx := r.Context()
	// The index is held for the length of the query: a build that replaces
	// it meanwhile closes it only afterwards.
	idx, indexed, done, err := c.acquire(ctx, t.ws.Slug)
	if err != nil {
		return err
	}
	defer done()
	// The answer is the index's of this commit to this query: a caller who
	// holds it is told so before the index is asked.
	if scm.Revalidated(w, r, indexed) {
		return nil
	}
	res, err := idx.Search(ctx, q, limit)
	var bad *QueryError
	switch {
	case errors.As(err, &bad):
		return server.NewError("invalid_query", http.StatusBadRequest, bad.Message)
	case errors.Is(err, ErrSearchTimeout):
		return server.NewError("scm_timeout", http.StatusServiceUnavailable,
			"the index did not finish the search in its own time: narrow the query or add a path constraint")
	case err != nil:
		return err
	}
	answer := searchAnswer{Workspace: t.ws.Slug, IndexedSHA: indexed, Partial: res.Partial, Truncated: res.Truncated, Files: []searchFile{}}
	for _, f := range res.Files {
		file := searchFile{Path: f.Path, Language: outline.LangFor(f.Path, nil), Matches: []searchMatch{}}
		for _, l := range f.Lines[:min(len(f.Lines), maxFileMatches)] {
			file.Matches = append(file.Matches, searchMatch{Line: l.Line, Text: cutChars(l.Text, maxMatchChars)})
		}
		answer.Files = append(answer.Files, file)
	}
	server.WriteJSON(w, http.StatusOK, answer)
	return nil
}

// The bounds of a symbol lookup: its name and its answer.
const (
	maxNameChars       = 200
	defaultSymbolsSize = 20
	maxSymbols         = 50
)

// symbolKinds are the kinds of declaration a lookup may ask for.
var symbolKinds = []string{"func", "method", "type", "class", "interface", "enum", "trait", "const", "var", "module", "macro"}

// symbolsAnswer is the answer of the symbols route.
type symbolsAnswer struct {
	Workspace  string   `json:"workspace"`
	IndexedSHA string   `json:"indexed_sha"`
	Partial    bool     `json:"partial"`
	Truncated  bool     `json:"truncated"`
	Symbols    []Symbol `json:"symbols"`
}

// getSymbols answers GET …/scm/symbols: the declarations of the default
// branch whose name starts with name, ignoring case unless it has an
// upper-case letter, or is name with exact; of the kind kind; in the file
// or under the directory path. They are answered by path and line, cut to
// limit. Only a complete index answers a lookup.
func (c *Component) getSymbols(w http.ResponseWriter, r *http.Request) error {
	var q SymbolQuery
	var path string
	limit := defaultSymbolsSize
	t, err := c.prepare(w, r, func(v url.Values) (err error) {
		q.Name = v.Get("name")
		if q.Name == "" || utf8.RuneCountInString(q.Name) > maxNameChars {
			return server.InvalidRequest("name is required: the start of a declaration's name, 1 to 200 characters")
		}
		switch v.Get("exact") {
		case "", "false":
		case "true":
			q.Exact = true
		default:
			return server.InvalidRequest("exact must be true or false")
		}
		if q.Kind = strings.ToLower(v.Get("kind")); q.Kind != "" && !slices.Contains(symbolKinds, q.Kind) {
			return server.InvalidRequest("kind must be one of " + strings.Join(symbolKinds, ", "))
		}
		if path = v.Get("path"); path != "" {
			if err := scm.CheckPath(path); err != nil {
				return err
			}
		}
		limit, err = limitParam(v, defaultSymbolsSize, maxSymbols)
		return err
	})
	if err != nil {
		return err
	}
	if t.rec.Partial {
		return server.NewError("code_index_partial", http.StatusConflict,
			fmt.Sprintf("the code index of %q is partial: symbols need a complete index; scm/code-search answers from a partial one", t.ws.Slug)).
			With("indexed_sha", t.rec.IndexedSHA)
	}
	ctx := r.Context()
	// The index looks under a directory; for a file it is asked for the
	// file's directory and the file's declarations are kept.
	q.Dir = path
	file := ""
	if path != "" {
		if info, err := os.Lstat(filepath.Join(c.TreeDir(t.ws.Slug, t.rec.IndexedSHA), filepath.FromSlash(path))); err == nil && !info.IsDir() {
			file = path
			if q.Dir = pathpkg.Dir(path); q.Dir == "." {
				q.Dir = ""
			}
		}
	}
	idx, indexed, done, err := c.acquire(ctx, t.ws.Slug)
	if err != nil {
		return err
	}
	defer done()
	if scm.Revalidated(w, r, indexed) {
		return nil
	}
	found, answered, err := idx.Symbols(ctx, q)
	if err != nil {
		return err
	}
	if !answered {
		return server.NewError("too_many_matches", http.StatusUnprocessableEntity,
			"more than 1,000 declarations match: give a longer name, a kind or a path")
	}
	if file != "" {
		found = slices.DeleteFunc(found, func(s Symbol) bool { return s.Path != file })
	}
	answer := symbolsAnswer{Workspace: t.ws.Slug, IndexedSHA: indexed, Truncated: len(found) > limit, Symbols: found[:min(len(found), limit)]}
	if answer.Symbols == nil {
		answer.Symbols = []Symbol{}
	}
	server.WriteJSON(w, http.StatusOK, answer)
	return nil
}

// limitParam is the query parameter limit of v, def when v has none; a
// value that is no integer from 1 to highest is 400 invalid_request.
func limitParam(v url.Values, def, highest int) (int, error) {
	if !v.Has("limit") {
		return def, nil
	}
	n, err := strconv.Atoi(v.Get("limit"))
	if err != nil || n < 1 || n > highest {
		return 0, server.InvalidRequest(fmt.Sprintf("limit must be an integer from 1 to %d", highest))
	}
	return n, nil
}

// cutChars is text cut to at most n characters.
func cutChars(text string, n int) string {
	if utf8.RuneCountInString(text) <= n {
		return text
	}
	return string([]rune(text)[:n])
}
