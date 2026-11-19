package sandbox

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/server"
)

// errorTypeNotFound answers a read of a sandbox the workspace does not
// have, with 404.
const errorTypeNotFound = "sandbox_not_found"

// msgNotFound is the message of every sandbox_not_found answer. It names no
// id, so that the answer is the same whether the id is malformed, unknown
// or of another workspace.
const msgNotFound = "the workspace has no such sandbox"

// idPattern is the form of a sandbox id.
var idPattern = regexp.MustCompile(`^sbx_[0-9a-f]{32}$`)

// The values of wait_for, and the bounds of wait in seconds.
const (
	waitReady = "ready"
	waitEnded = "ended"
	maxWait   = 60
)

// waitPoll is how often a waiting read reads the record again although no
// transition was notified: a fallback, should a notification be missed.
const waitPoll = time.Second

func sandboxNotFound() *server.Error {
	return server.NewError(errorTypeNotFound, http.StatusNotFound, msgNotFound)
}

// waitCond is what a read of one sandbox waits for: the condition of
// wait_for and the wait, zero for a read that does not wait.
type waitCond struct {
	waitFor string
	wait    time.Duration
}

// holds reports whether a sandbox in state meets the condition: ready holds
// once it is not provisioning, since an ended sandbox does not become ready
// without a start, and ended holds once it has ended.
func (c waitCond) holds(state string) bool {
	if c.waitFor == waitEnded {
		return slices.Contains(endedStates, state)
	}
	return state != StateProvisioning
}

// readWait reads wait_for and wait: both or neither, wait_for ready or
// ended, wait a whole number of seconds from 1 to 60.
func readWait(q url.Values) (waitCond, error) {
	hasFor, hasWait := q.Has("wait_for"), q.Has("wait")
	switch {
	case !hasFor && !hasWait:
		return waitCond{}, nil
	case hasFor != hasWait:
		return waitCond{}, server.InvalidRequest("wait_for and wait are given together")
	}
	c := waitCond{waitFor: q.Get("wait_for")}
	if c.waitFor != waitReady && c.waitFor != waitEnded {
		return waitCond{}, server.InvalidRequest("wait_for must be ready or ended")
	}
	n, err := strconv.Atoi(q.Get("wait"))
	if err != nil || n < 1 || n > maxWait {
		return waitCond{}, server.InvalidRequest(fmt.Sprintf("wait must be a whole number of seconds from 1 to %d", maxWait))
	}
	c.wait = time.Duration(n) * time.Second
	return c, nil
}

// getSandbox answers GET /workspaces/:slug/sandboxes/:id. With wait_for and
// wait, a sandbox that does not meet the condition holds the request until
// it does, the wait has passed on the hub's clock or the hub begins to stop,
// and is then answered as it is; a request whose client goes away ends
// without an answer. The read learns of a change from the notification of
// every applied transition, and reads the record every second besides.
func (s *Service) getSandbox(w http.ResponseWriter, r *http.Request) error {
	cond, err := readWait(r.URL.Query())
	if err != nil {
		return err
	}
	d, ok := access.DecisionOf(r)
	if !ok {
		return server.Internal("the sandbox read found no access decision")
	}
	ctx, slug, id := r.Context(), d.Record.Slug, server.Param(r, "id")
	// Taken before the read, so that a transition after it ends the wait.
	var changed <-chan struct{}
	if cond.wait > 0 {
		changed = s.changes.next()
	}
	rec, err := s.readSandbox(ctx, slug, id, false)
	if err != nil {
		return err
	}
	if cond.wait > 0 && !cond.holds(rec.State) {
		var answer bool
		rec, answer, err = s.await(ctx, slug, id, cond, changed)
		if ctx.Err() != nil || (err == nil && !answer) {
			// Nobody is left to answer.
			return nil
		}
		if err != nil {
			return err
		}
	}
	server.WriteJSON(w, http.StatusOK, s.object(rec))
	return nil
}

// await waits for the sandbox id of the workspace slug to meet cond, as
// getSandbox says, and returns the record to answer with; answer is false
// when the request's context ended first. It returns with its timers
// stopped and no goroutine left.
func (s *Service) await(ctx context.Context, slug, id string, cond waitCond, changed <-chan struct{}) (rec Record, answer bool, err error) {
	deadline := s.newTimer(cond.wait)
	defer deadline.Stop()
	poll := time.NewTicker(waitPoll)
	defer poll.Stop()
	for {
		// A stop releases a held request before anything else that is ready.
		select {
		case <-s.deps.Shutdown:
			rec, err = s.readSandbox(ctx, slug, id, false)
			return rec, true, err
		default:
		}
		notified := false
		select {
		case <-s.deps.Shutdown:
			rec, err = s.readSandbox(ctx, slug, id, false)
			return rec, true, err
		case <-ctx.Done():
			return Record{}, false, nil
		case <-deadline.C():
			rec, err = s.readSandbox(ctx, slug, id, false)
			return rec, true, err
		case <-changed:
			notified = true
		case <-poll.C:
		}
		changed = s.changes.next()
		if rec, err = s.readSandbox(ctx, slug, id, notified); err != nil || cond.holds(rec.State) {
			return rec, err == nil, err
		}
	}
}

// readSandbox reads the sandbox id of the workspace slug; a malformed id, or
// one the workspace has no sandbox of, is sandbox_not_found. afterChange
// reads in the store's turn of writes: a transition notifies its waiters
// before the caller's transaction commits, and the turn comes once it has.
func (s *Service) readSandbox(ctx context.Context, slug, id string, afterChange bool) (Record, error) {
	if !idPattern.MatchString(id) {
		return Record{}, sandboxNotFound()
	}
	var (
		rec Record
		err error
	)
	if afterChange {
		err = s.deps.Store.Tx(ctx, func(tx *sql.Tx) (err error) {
			rec, err = GetIn(ctx, tx, slug, id)
			return err
		})
	} else {
		rec, err = GetIn(ctx, s.deps.Store.Main(), slug, id)
	}
	if errors.Is(err, ErrNotFound) {
		return Record{}, sandboxNotFound()
	}
	return rec, err
}

// listQuery is the filter of a workspace's sandbox list.
type listQuery struct {
	states       []string
	session      *string
	labels       [][2]string
	includeForks bool
}

// defaultStates are the states a list shows when the query names none:
// every state but deleted.
var defaultStates = []string{StateProvisioning, StateReady, StateStopped, StateCompleted, StateFailed}

// readListQuery reads the filters of GET /workspaces/:slug/sandboxes: state,
// repeatable, each one of the six states (every state but deleted when
// none is given); session_id; label, repeatable, each key=value with a key;
// include_forks, true or false.
func readListQuery(q url.Values) (listQuery, error) {
	lq := listQuery{states: defaultStates}
	if given := q["state"]; len(given) > 0 {
		for _, st := range given {
			if !slices.Contains(states, st) {
				return listQuery{}, server.InvalidRequest("state must be one of " + strings.Join(states, ", "))
			}
		}
		lq.states = given
	}
	if q.Has("session_id") {
		v := q.Get("session_id")
		lq.session = &v
	}
	for _, l := range q["label"] {
		k, v, ok := strings.Cut(l, "=")
		if !ok || k == "" {
			return listQuery{}, server.InvalidRequest("label must be key=value")
		}
		lq.labels = append(lq.labels, [2]string{k, v})
	}
	if q.Has("include_forks") {
		switch q.Get("include_forks") {
		case "true":
			lq.includeForks = true
		case "false":
		default:
			return listQuery{}, server.InvalidRequest("include_forks must be true or false")
		}
	}
	return lq, nil
}

// listWorkspaceSandboxes answers GET /workspaces/:slug/sandboxes: the
// sandboxes of the workspace in one of the states asked for, of the session
// asked for and with every label asked for, by created_at and then id, both
// descending, a page at a time. With include_forks=true on a workspace that
// is not a fork, the list also holds the sandboxes of those of its forks
// that the caller can read and its binding covers.
func (s *Service) listWorkspaceSandboxes(w http.ResponseWriter, r *http.Request) error {
	page, err := server.ReadPage(r)
	if err != nil {
		return err
	}
	lq, err := readListQuery(r.URL.Query())
	if err != nil {
		return err
	}
	d, ok := access.DecisionOf(r)
	if !ok {
		return server.Internal("the sandbox list found no access decision")
	}
	ctx := r.Context()
	workspaces := []string{d.Record.Slug}
	if lq.includeForks && d.Record.Kind != access.KindFork {
		forks, err := s.readableForks(ctx, d.Record.Slug)
		if err != nil {
			return err
		}
		workspaces = append(workspaces, forks...)
	}
	var pos Position
	after := &pos
	if ok, err := page.After(&pos); err != nil {
		return err
	} else if !ok {
		after = nil
	}
	recs, err := listRecords(ctx, s.deps.Store.Main(), recordQuery{
		workspaces: workspaces, states: lq.states, session: lq.session, labels: lq.labels, after: after, limit: page.Limit + 1,
	})
	if err != nil {
		return err
	}
	return server.WritePage(w, page, s.items(recs), func(i Item) any { return Position{CreatedAt: i.CreatedAt, ID: i.ID} })
}

// readableForks returns the forks of the repository slug that the caller of
// ctx can read and that its binding covers.
func (s *Service) readableForks(ctx context.Context, slug string) ([]string, error) {
	if s.deps.Readable == nil {
		return nil, nil
	}
	c, _ := server.CallerOf(ctx)
	set, err := s.deps.Readable(ctx, access.CallerFrom(c))
	if err != nil {
		return nil, fmt.Errorf("read the workspaces the caller can read: %w", err)
	}
	forks, err := access.ForksOf(ctx, s.deps.Store.Main(), []string{slug})
	if err != nil {
		return nil, fmt.Errorf("read the forks of %s: %w", slug, err)
	}
	var out []string
	for _, f := range forks {
		if set.Contains(f.Slug) {
			out = append(out, f.Slug)
		}
	}
	return out, nil
}

// items are the list items of recs, each object with runtime_reachable as
// the runtime client reports it.
func (s *Service) items(recs []Record) []Item {
	out := make([]Item, len(recs))
	for i, rec := range recs {
		out[i] = Item{CreatedAt: rec.CreatedAt, ID: rec.ID, Object: s.object(rec)}
	}
	return out
}

// recordQuery selects stored sandboxes; a nil or empty member selects every
// value.
type recordQuery struct {
	workspaces []string
	states     []string
	session    *string
	// labels are key and value pairs, each of which a sandbox must have.
	labels [][2]string
	// user is the id of the creating user.
	user string
	// after is the position the list continues after; nil for its start.
	after *Position
	limit int
}

// listRecords reads through q the sandboxes rq selects, by created_at and
// then id, both descending, at most rq.limit of them.
func listRecords(ctx context.Context, q access.Querier, rq recordQuery) ([]Record, error) {
	var (
		where []string
		args  []any
	)
	in := func(column string, values []string) {
		where = append(where, column+` IN (`+strings.TrimSuffix(strings.Repeat("?, ", len(values)), ", ")+`)`)
		for _, v := range values {
			args = append(args, v)
		}
	}
	if len(rq.workspaces) > 0 {
		in("workspace", rq.workspaces)
	}
	if len(rq.states) > 0 {
		in("state", rq.states)
	}
	if rq.session != nil {
		where = append(where, `session_id = ?`)
		args = append(args, *rq.session)
	}
	for _, l := range rq.labels {
		where = append(where, `EXISTS (SELECT 1 FROM json_each(sandboxes.labels) AS l WHERE l.key = ? AND l.value = ?)`)
		args = append(args, l[0], l[1])
	}
	if rq.user != "" {
		where = append(where, `user_id = ?`)
		args = append(args, rq.user)
	}
	if rq.after != nil {
		at := rq.after.CreatedAt.Unix()
		where = append(where, `(created_at < ? OR (created_at = ? AND id < ?))`)
		args = append(args, at, at, rq.after.ID)
	}
	query := `SELECT ` + recordColumns + ` FROM sandboxes`
	if len(where) > 0 {
		query += ` WHERE ` + strings.Join(where, ` AND `)
	}
	query += ` ORDER BY created_at DESC, id DESC LIMIT ?`
	rows, err := q.QueryContext(ctx, query, append(args, rq.limit)...)
	if err != nil {
		return nil, fmt.Errorf("list sandboxes: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []Record
	for rows.Next() {
		rec, err := scanRecord(rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("list sandboxes: %w", err)
		}
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list sandboxes: %w", err)
	}
	return out, nil
}

// StoredSource returns the source of the hub-wide sandbox list over the
// stored sandboxes, for RegisterSource: their objects with
// runtime_reachable as the runtime client reports it.
func (s *Service) StoredSource() Source { return storedSource{s: s} }

type storedSource struct{ s *Service }

func (src storedSource) List(ctx context.Context, f Filter, after *Position, limit int) ([]Item, error) {
	rq := recordQuery{user: f.User, after: after, limit: limit}
	if f.State != "" {
		rq.states = []string{f.State}
	}
	recs, err := listRecords(ctx, src.s.deps.Store.Main(), rq)
	if err != nil {
		return nil, err
	}
	return src.s.items(recs), nil
}
