package fork_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"uuid"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/event"
	"github.com/agent-fox-dev/parley/internal/fork"
	"github.com/agent-fox-dev/parley/internal/gitrepo"
	"github.com/agent-fox-dev/parley/internal/gitrepo/gitrepotest"
	"github.com/agent-fox-dev/parley/internal/hub"
	"github.com/agent-fox-dev/parley/internal/hubtest"
	"github.com/agent-fox-dev/parley/internal/identity"
	"github.com/agent-fox-dev/parley/internal/secret"
	"github.com/agent-fox-dev/parley/internal/server/timefmt"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

// forksPath is the fork routes of demo, forkPath the workspace demo--f.
const (
	forksPath = "/workspaces/demo/forks"
	forkPath  = "/workspaces/demo--f"
)

// parentSlug is the repository workspace every world holds, the parent the
// fork tests fork.
const parentSlug = "demo"

// world is a hub the fork tests ask, on a clock that stands still unless a
// test moves it: the developer dev, who owns the ready repository workspace
// demo in the org acme, and mia, a member of acme who can only read it, each
// with an API key, and demo's repository at its place on disk, a bare
// repository whose default branch main has two commits, with the branch
// release one commit off main's first and the annotated tag v1 on main's
// first.
type world struct {
	*hubtest.Hub
	forks      *fork.Component
	workspaces *workspace.Service
	events     *event.Component
	secrets    *secret.Component

	// dev and mia are the users' ids, devKey and miaKey their API keys,
	// acme the org's id.
	dev, mia       string
	devKey, miaKey string
	acme           string
	// parent is the git directory of demo's repository.
	parent string

	// mu guards now, the hub's time.
	mu  sync.Mutex
	now time.Time
}

// newWorld starts a hub with a clock in the middle of a second, the hub's
// own composition and compose run after it (nil for nothing), and builds
// the world in it.
func newWorld(t *testing.T, compose func(*hub.Composition)) *world {
	t.Helper()
	return newWorldWith(t, hubtest.Config{Compose: compose})
}

// newWorldWith is newWorld with what cfg adds to the hub's configuration;
// the world sets cfg.Now.
func newWorldWith(t *testing.T, cfg hubtest.Config) *world {
	t.Helper()
	w := &world{now: time.Now().UTC().Truncate(time.Second).Add(time.Second / 2)}
	compose := cfg.Compose
	cfg.Now, cfg.Compose = w.clock, func(c *hub.Composition) {
		w.forks, w.workspaces, w.events, w.secrets = c.Forks(), c.Workspaces(), c.Events(), c.Secrets()
		if compose != nil {
			compose(c)
		}
	}
	w.Hub = hubtest.StartWith(t, cfg)
	w.dev, w.mia = w.CreateUser(t, "dev"), w.CreateUser(t, "mia")
	w.devKey, w.miaKey = w.IssueKey(t, w.dev), w.IssueKey(t, w.mia)
	w.acme = w.CreateOrg(t, "acme")
	w.AddMember(t, w.acme, w.dev)
	w.AddMember(t, w.acme, w.mia)
	w.parent = w.workspaces.RepoPath(parentSlug)
	gitrepotest.Git(t, w.parent, "init", "--quiet", "--bare", "--initial-branch=main", w.parent)
	stream := "commit refs/heads/main\nmark :1\ncommitter P <p@example.invalid> now\ndata 5\nfirst\nM 100644 inline README\ndata 2\n1\n" +
		"commit refs/heads/main\nmark :2\ncommitter P <p@example.invalid> now\ndata 6\nsecond\nfrom :1\nM 100644 inline README\ndata 2\n2\n" +
		"commit refs/heads/release\nmark :3\ncommitter P <p@example.invalid> now\ndata 7\nrelease\nfrom :1\nM 100644 inline README\ndata 2\n3\n" +
		"tag v1\nfrom :1\ntagger P <p@example.invalid> now\ndata 2\nv1\n"
	if _, err := gitrepotest.Run(w.parent, stream, "fast-import", "--quiet", "--date-format=now"); err != nil {
		t.Fatal(err)
	}
	w.storeParent(t, parentSlug)
	return w
}

// addParent adds the ready repository workspace slug of dev in acme, whose
// repository is a copy of demo's; edit, when given, changes the record
// before it is stored.
func (w *world) addParent(t *testing.T, slug string, edit ...func(*workspace.Workspace)) {
	t.Helper()
	gitrepotest.Git(t, w.parent, "clone", "--quiet", "--bare", w.parent, w.workspaces.RepoPath(slug))
	w.storeParent(t, slug, edit...)
}

// storeParent stores the record of the ready repository workspace slug of
// dev in acme, whose repository is at its place already.
func (w *world) storeParent(t *testing.T, slug string, edit ...func(*workspace.Workspace)) {
	t.Helper()
	head := w.head(t, w.workspaces.RepoPath(slug), "main")
	rec := workspace.Workspace{
		Slug: slug, Kind: access.KindRepository, Mode: workspace.ModeStandard,
		GitURL: new("https://origin.test/" + slug + ".git"), DefaultBranch: new("main"), OwnerID: w.dev, OrgID: w.acme,
		Status: workspace.StatusReady, DisplayName: slug, HeadSHA: &head,
		CreatedAt: w.clock(), UpdatedAt: w.clock(), InstanceID: workspace.NewInstanceID(),
	}
	for _, fn := range edit {
		fn(&rec)
	}
	w.insert(t, func(tx *sql.Tx) error { return workspace.Insert(context.Background(), tx, rec) })
}

// clock is the hub's time.
func (w *world) clock() time.Time {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.now
}

// advance moves the hub's time on by d.
func (w *world) advance(d time.Duration) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.now = w.now.Add(d)
}

// insert runs fn in a transaction of the hub's store and fails the test on
// its error.
func (w *world) insert(t *testing.T, fn func(*sql.Tx) error) {
	t.Helper()
	if err := w.Store.Tx(context.Background(), fn); err != nil {
		t.Fatal(err)
	}
}

// exec runs the statement query with args on the hub's store and fails the
// test on its error.
func (w *world) exec(t *testing.T, query string, args ...any) {
	t.Helper()
	if _, err := w.Store.Main().ExecContext(context.Background(), query, args...); err != nil {
		t.Fatal(err)
	}
}

// seedFork stores a ready fork of demo named slug, as a create would, with
// the fork's repository at its place: owned by dev in acme, taken from main
// at main's head, tracking every branch and tag demo has now, created at the
// hub's time and expiring a week later. edit, when given, changes the record
// before it is stored. It returns the stored record.
func (w *world) seedFork(t *testing.T, slug string, edit ...func(*fork.Fork)) fork.Fork {
	t.Helper()
	now := w.clock()
	f := fork.Fork{
		Workspace: workspace.Workspace{
			Slug: slug, Kind: access.KindFork, Mode: workspace.ModeStandard, DefaultBranch: new("main"),
			OwnerID: w.dev, OrgID: w.acme, Status: workspace.StatusReady, DisplayName: slug,
			ParentSlug: new(parentSlug), ForkPoint: &workspace.ForkPoint{Ref: "main", SHA: w.head(t, w.parent, "main")},
			ExpiresAt: new(now.Add(7 * 24 * time.Hour)), AutoChange: new(false),
			CreatedAt: now, UpdatedAt: now, InstanceID: workspace.NewInstanceID(),
		},
		TrackingBranches: slices.Sorted(maps.Keys(gitrepotest.Refs(t, w.parent, "refs/heads/"))),
		TrackingTags:     slices.Sorted(maps.Keys(gitrepotest.Refs(t, w.parent, "refs/tags/"))),
	}
	for _, fn := range edit {
		fn(&f)
	}
	if err := (gitrepo.Git{}).CreateFork(context.Background(), w.parent, w.workspaces.RepoPath(slug)); err != nil {
		t.Fatal(err)
	}
	w.insert(t, func(tx *sql.Tx) error { return fork.Insert(context.Background(), tx, f) })
	return f
}

// git runs git on the repository of the workspace slug and returns its
// trimmed output.
func (w *world) git(t *testing.T, slug string, args ...string) string {
	t.Helper()
	return gitrepotest.Git(t, w.workspaces.RepoPath(slug), args...)
}

// head is the commit branch points at in the repository dir, "" when it has
// no such branch.
func (w *world) head(t *testing.T, dir, branch string) string {
	t.Helper()
	return gitrepotest.Refs(t, dir, "refs/heads/")[branch]
}

// commit adds a commit on top of from (a commit id or a ref) to branch of
// the repository of the workspace slug, creating or moving the branch, and
// returns its id. The commit's objects are written into that repository.
func (w *world) commit(t *testing.T, slug, branch, from string) string {
	t.Helper()
	dir := w.workspaces.RepoPath(slug)
	msg := fmt.Sprintf("on %s %d", branch, time.Now().UnixNano())
	stream := fmt.Sprintf("commit refs/heads/%s\ncommitter T <t@example.invalid> now\ndata %d\n%s\nfrom %s\nM 100644 inline README\ndata %d\n%s\n",
		branch, len(msg), msg, from, len(msg), msg)
	if _, err := gitrepotest.Run(dir, stream, "fast-import", "--quiet", "--force", "--date-format=now"); err != nil {
		t.Fatal(err)
	}
	return w.head(t, dir, branch)
}

// refs is the ref view of the fork slug, failing the test on an error.
func (w *world) refs(t *testing.T, slug string) fork.RefView {
	t.Helper()
	v, err := w.forks.Refs(context.Background(), slug)
	if err != nil {
		t.Fatalf("Refs(%s): %v", slug, err)
	}
	return v
}

// answer is the status and the body of a hub's answer.
type answer struct {
	status int
	body   []byte
}

// object decodes a, which must have status want, as a JSON object.
func (a answer) object(t *testing.T, want int) map[string]any {
	t.Helper()
	var m map[string]any
	if a.status != want || json.Unmarshal(a.body, &m) != nil {
		t.Fatalf("answer = %d %s, want %d with a JSON object", a.status, a.body, want)
	}
	return m
}

// call sends method path, under /api/v1, to the hub with cred as the bearer
// credential and body (nil for none; see hubtest.Served.Call).
func (w *world) call(t *testing.T, method, path, cred string, body any) answer {
	t.Helper()
	status, b := w.Call(t, method, "/api/v1"+path, cred, body)
	return answer{status, b}
}

// get asks GET path with cred.
func (w *world) get(t *testing.T, cred, path string) answer {
	t.Helper()
	return w.call(t, http.MethodGet, path, cred, nil)
}

// post sends POST path with cred and body.
func (w *world) post(t *testing.T, cred, path string, body any) answer {
	t.Helper()
	return w.call(t, http.MethodPost, path, cred, body)
}

// postKey sends POST path with cred, body and the Idempotency-Key key.
func (w *world) postKey(t *testing.T, cred, path, key string, body any) answer {
	t.Helper()
	status, b := w.CallWith(t, http.MethodPost, "/api/v1"+path, cred, body, http.Header{"Idempotency-Key": {key}})
	return answer{status, b}
}

// forkEvents are the parley.workspace.fork events of the log, in its order.
func (w *world) forkEvents(t *testing.T) []event.Record {
	t.Helper()
	return w.eventsOf(t, "parley.workspace.fork")
}

// eventsOf are the events of type typ in the log, in its order.
func (w *world) eventsOf(t *testing.T, typ string) []event.Record {
	t.Helper()
	recs, _, err := w.events.ReadAfter(context.Background(), 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	var out []event.Record
	for _, r := range recs {
		if r.Type == typ {
			out = append(out, r)
		}
	}
	return out
}

// setParent stores status and marker on demo's record until the test ends,
// when demo is ready again with no maintenance running.
func (w *world) setParent(t *testing.T, status workspace.Status, marker workspace.Marker) {
	t.Helper()
	const set = `UPDATE workspaces SET status = ?, maintenance = ? WHERE slug = ?`
	w.exec(t, set, status, marker, parentSlug)
	t.Cleanup(func() { w.exec(t, set, workspace.StatusReady, workspace.MarkerNone, parentSlug) })
}

// record is the stored fork slug.
func (w *world) record(t *testing.T, slug string) fork.Fork {
	t.Helper()
	f, found, err := fork.BySlug(context.Background(), w.Store.Main(), slug)
	if err != nil || !found {
		t.Fatalf("fork %s: found %v, %v", slug, found, err)
	}
	return f
}

// footprint is what a refused or failed create must leave as it found it:
// demo's forks, the entries of the directory that holds the workspaces'
// repositories, the tokens and the events.
type footprint struct {
	forks, tokens, events int
	entries               string
}

// footprint is the world's footprint now.
func (w *world) footprint(t *testing.T) footprint {
	t.Helper()
	ctx := context.Background()
	var f footprint
	var err error
	if f.forks, err = fork.CountOf(ctx, w.Store.Main(), parentSlug); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		query string
		n     *int
	}{{"SELECT count(*) FROM pats", &f.tokens}, {"SELECT count(*) FROM hub_events", &f.events}} {
		if err := w.Store.Main().QueryRowContext(ctx, c.query).Scan(c.n); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := os.ReadDir(filepath.Dir(w.parent))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		f.entries += e.Name() + " "
	}
	return f
}

// unchanged fails the test when the world's footprint is no longer before.
func (w *world) unchanged(t *testing.T, before footprint) {
	t.Helper()
	if got := w.footprint(t); got != before {
		t.Errorf("footprint = %+v, want it unchanged from %+v", got, before)
	}
}

// patch sends PATCH path with cred and body.
func (w *world) patch(t *testing.T, cred, path string, body any) answer {
	t.Helper()
	return w.call(t, http.MethodPatch, path, cred, body)
}

// pat stores a PAT of dev with perms, paths (nil for none) and bound to
// workspace ("" for none), and returns its secret and its id.
func (w *world) pat(t *testing.T, workspace string, paths []string, perms ...string) (secret, id string) {
	t.Helper()
	return w.IssuePATID(t, identity.PATSpec{UserID: w.dev, Permissions: perms, Paths: paths, Workspace: workspace, CreatedAt: w.clock()})
}

// linker is a session linker that accepts every id, in lower case, unless
// refuse or fail is set; its record writes a row of the table session_marks
// through the transaction it is given, unless recordErr is set. It keeps
// what it was asked.
type linker struct {
	mu              sync.Mutex
	refuse          bool
	fail, recordErr error
	checks, records [][3]string
}

// set changes the linker under its lock.
func (l *linker) set(fn func(*linker)) {
	l.mu.Lock()
	defer l.mu.Unlock()
	fn(l)
}

// asked is what the linker's check and record were asked, in order: the
// checks as (id, caller's user, parent), the records as (id, parent, fork).
func (l *linker) asked() (checks, records [][3]string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.checks), slices.Clone(l.records)
}

func (l *linker) Check(_ context.Context, caller identity.Principal, id, parent string) (string, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.checks = append(l.checks, [3]string{id, caller.User.ID, parent})
	switch {
	case l.fail != nil:
		return "", l.fail
	case l.refuse:
		return "", fmt.Errorf("no session %s: %w", id, fork.ErrUnknownSession)
	}
	return strings.ToLower(id), nil
}

func (l *linker) Record(ctx context.Context, tx *sql.Tx, id, parent, forkSlug string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.records = append(l.records, [3]string{id, parent, forkSlug})
	if l.recordErr != nil {
		return l.recordErr
	}
	_, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS session_marks (fork TEXT);
		INSERT INTO session_marks (fork) VALUES (?)`, forkSlug)
	return err
}

// sandboxer is a sandbox component that answers obj, or err when it is set,
// and keeps every call with what it saw in the transaction it was given.
type sandboxer struct {
	workspaces *workspace.Service
	mu         sync.Mutex
	obj        json.RawMessage
	err        error
	calls      []sandboxCall
}

// sandboxCall is one call of a sandboxer: the request, whether the fork's
// record could be read through the transaction, and whether its repository
// existed.
type sandboxCall struct {
	req           fork.SandboxRequest
	visible, repo bool
}

// set changes the sandboxer under its lock.
func (s *sandboxer) set(fn func(*sandboxer)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fn(s)
}

// called is the sandboxer's calls so far.
func (s *sandboxer) called() []sandboxCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.calls)
}

func (s *sandboxer) CreateSandbox(ctx context.Context, tx *sql.Tx, req fork.SandboxRequest) (json.RawMessage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, visible, err := fork.BySlug(ctx, tx, req.Fork.Slug)
	if err != nil {
		return nil, err
	}
	_, statErr := os.Stat(s.workspaces.RepoPath(req.Fork.Slug))
	s.calls = append(s.calls, sandboxCall{req, visible, statErr == nil})
	return s.obj, s.err
}

// collaborate registers l and s, each when not nil, on the fork component
// of the hub being composed.
func collaborate(l *linker, s *sandboxer) func(*hub.Composition) {
	return func(c *hub.Composition) {
		if l != nil {
			c.Forks().RegisterSessionLinker(l)
		}
		if s != nil {
			s.workspaces = c.Workspaces()
			c.Forks().RegisterSandbox(s)
		}
	}
}

// created is the answer a, which must be 201, with its workspace member.
func created(t *testing.T, a answer) (body, ws map[string]any) {
	t.Helper()
	body = a.object(t, http.StatusCreated)
	ws, _ = body["workspace"].(map[string]any)
	return body, ws
}

// wantMembers fails the test for each member of want that obj does not hold
// with the same value.
func wantMembers(t *testing.T, obj, want map[string]any) {
	t.Helper()
	for k, v := range want {
		if !reflect.DeepEqual(obj[k], v) {
			t.Errorf("%s = %#v, want %#v", k, obj[k], v)
		}
	}
}

// wantAbsent fails the test for each of names obj holds.
func wantAbsent(t *testing.T, obj map[string]any, names ...string) {
	t.Helper()
	for _, n := range names {
		if v, ok := obj[n]; ok {
			t.Errorf("%s = %#v, want it absent", n, v)
		}
	}
}

// wantError fails the test unless a has status and the error type typ.
func wantError(t *testing.T, a answer, status int, typ string) {
	t.Helper()
	if e, _ := a.object(t, status)["error"].(map[string]any); e["error_type"] != typ {
		t.Errorf("error = %v, want %d %s", e, status, typ)
	}
}

// postAll sends n creates with body at once and counts the answers by
// status.
func (w *world) postAll(t *testing.T, n int, body any) map[int]int {
	t.Helper()
	var mu sync.Mutex
	counts := map[int]int{}
	var wg sync.WaitGroup
	for range n {
		wg.Go(func() {
			status, _ := w.Call(t, http.MethodPost, "/api/v1"+forksPath, w.devKey, body)
			mu.Lock()
			defer mu.Unlock()
			counts[status]++
		})
	}
	wg.Wait()
	return counts
}

// listed are the forks of the world of newListWorld, as the list orders
// them: newest first, then by slug.
var listed = []string{"demo--c", "demo--d", "demo--b", "demo--a"}

// newListWorld is a world whose demo has four forks: demo--a of dev, the
// oldest, demo--b of mia, created in the session it returns, and demo--c
// and demo--d of dev, created at the same instant, the newest.
func newListWorld(t *testing.T) (w *world, session string) {
	t.Helper()
	w, session = newWorld(t, nil), uuid.New().String()
	w.seedFork(t, "demo--a")
	w.advance(time.Second)
	w.seedFork(t, "demo--b", func(f *fork.Fork) { f.OwnerID, f.SessionID = w.mia, &session })
	w.advance(time.Second)
	w.seedFork(t, "demo--d")
	w.seedFork(t, "demo--c")
	return w, session
}

// page decodes a, which must be 200 with items an array, as a page of the
// fork list: the items' slugs, the items by slug and next_cursor.
func page(t *testing.T, a answer) (slugs []string, items map[string]map[string]any, next *string) {
	t.Helper()
	var p struct {
		Items      *[]map[string]any `json:"items"`
		NextCursor *string           `json:"next_cursor"`
	}
	if a.status != http.StatusOK || json.Unmarshal(a.body, &p) != nil || p.Items == nil {
		t.Fatalf("answer = %d %s, want 200 with an array of items", a.status, a.body)
	}
	slugs, items = []string{}, map[string]map[string]any{}
	for _, it := range *p.Items {
		slug, _ := it["slug"].(string)
		slugs, items[slug] = append(slugs, slug), it
	}
	return slugs, items, p.NextCursor
}

// lastUpdate is the last parley.workspace.update event of the log, with
// its metadata decoded.
func (w *world) lastUpdate(t *testing.T) (sessionID *string, fields []string, parent string) {
	t.Helper()
	evs := w.eventsOf(t, "parley.workspace.update")
	if len(evs) == 0 {
		t.Fatal("no parley.workspace.update event")
	}
	ev := evs[len(evs)-1]
	var meta struct {
		Fields []string `json:"fields"`
		Parent string   `json:"parent_slug"`
	}
	if err := json.Unmarshal(ev.Metadata, &meta); err != nil {
		t.Fatal(err)
	}
	return ev.SessionID, meta.Fields, meta.Parent
}

// named are the branches of bs whose name is name.
func named(bs []fork.Branch, name string) []fork.Branch {
	return slices.DeleteFunc(slices.Clone(bs), func(b fork.Branch) bool { return b.Name != name })
}

// faultyEvents is the hub's event component, failing its writes while
// fault holds "event".
type faultyEvents struct {
	fork.Events
	fault *atomic.Value
}

func (e faultyEvents) Emit(ctx context.Context, tx *sql.Tx, in event.Input) (event.Record, error) {
	if e.fault.Load() == "event" {
		return event.Record{}, errors.New("injected fault")
	}
	return e.Events.Emit(ctx, tx, in)
}

// secretKey is the current secret key of a removal world, so that a fork
// can hold a secret.
const secretKey = "MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY="

// remover keeps what a fork's removal asked of the collaborators it calls:
// registered in the teardown set as a remover of owned data and as the
// session closer, and on the fork component as its sandbox collaborator. A
// step fails while its fail flag is set; the event writer fails while fault
// holds "event", and the discard of a fork's repository while failDiscard
// is set.
type remover struct {
	store                                func() *sql.DB
	mu                                   sync.Mutex
	failRemove, failSandbox, failDiscard bool
	removed                              []string
	closed                               [][2]string
	sandboxes                            []sandboxDelete
	fault                                atomic.Value
}

// sandboxDelete is one call of the sandbox collaborator's delete, and
// whether the fork's record existed when it was made.
type sandboxDelete struct {
	slug, reason       string
	checkpoint, record bool
}

// set changes the remover under its lock.
func (r *remover) set(fn func(*remover)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	fn(r)
}

// take returns what the remover was asked since the last take and forgets
// it: the slugs the remover of owned data was given, the (slug, reason)
// pairs the session closer was, and the sandbox deletes.
func (r *remover) take() (removed []string, closed [][2]string, sandboxes []sandboxDelete) {
	r.mu.Lock()
	defer r.mu.Unlock()
	removed, closed, sandboxes = r.removed, r.closed, r.sandboxes
	r.removed, r.closed, r.sandboxes = nil, nil, nil
	return removed, closed, sandboxes
}

func (r *remover) remove(_ context.Context, _ *sql.Tx, slug string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.removed = append(r.removed, slug)
	if r.failRemove {
		return errors.New("injected remover fault")
	}
	return nil
}

func (r *remover) CloseSessions(_ context.Context, _ *sql.Tx, slugs []string, reason string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, s := range slugs {
		r.closed = append(r.closed, [2]string{s, reason})
	}
	return nil
}

func (r *remover) DeleteSandboxes(ctx context.Context, slug, reason string, checkpoint bool) error {
	_, found, err := fork.BySlug(ctx, r.store(), slug)
	if err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sandboxes = append(r.sandboxes, sandboxDelete{slug, reason, checkpoint, found})
	if r.failSandbox {
		return errors.New("injected sandbox fault")
	}
	return nil
}

func (r *remover) HasLiveSandbox(context.Context, string) (bool, error) { return false, nil }

// newRemovalWorld is a world with a current secret key whose fork removals
// call r as their collaborators.
func newRemovalWorld(t *testing.T) (*world, *remover) {
	t.Helper()
	r := &remover{}
	w := newWorldWith(t, hubtest.Config{Env: map[string]string{secret.EnvCurrentKey: secretKey}, Compose: func(c *hub.Composition) {
		c.Workspaces().Teardown().RegisterRemover(r.remove)
		c.Workspaces().Teardown().RegisterSessionCloser(r)
		c.Forks().RegisterSandboxes(r)
		c.AdjustForks(func(d *fork.Deps) {
			d.Events = faultyEvents{d.Events, &r.fault}
			d.RemoveRepo = func(dir string) error {
				r.mu.Lock()
				fail := r.failDiscard
				r.mu.Unlock()
				if fail {
					return errors.New("injected discard fault")
				}
				return gitrepo.Remove(dir)
			}
		})
	}})
	r.store = w.Store.Main
	return w, r
}

// held is what a fork of a removal world holds: its own branch task at
// task, the PAT of dev bound to it (pat, with the id patID) with
// workspaces:read, and the secret K.
type held struct {
	slug, task, pat, patID string
	expires                time.Time
}

// seedHeld seeds the fork slug as seedFork does, with edit, and gives it
// what held names.
func (w *world) seedHeld(t *testing.T, slug string, edit ...func(*fork.Fork)) held {
	t.Helper()
	f := w.seedFork(t, slug, edit...)
	h := held{slug: slug, task: w.commit(t, slug, "task", f.ForkPoint.SHA), expires: *f.ExpiresAt}
	h.pat, h.patID = w.pat(t, slug, nil, "workspaces:read")
	w.insert(t, func(tx *sql.Tx) error {
		return w.secrets.Put(context.Background(), tx, secret.ScopeWorkspace, slug, "K", "value")
	})
	return h
}

// whole fails the test unless the fork of h is as seedHeld left it: its
// record answered, its token authenticating, its secret stored and its own
// branch at its commit.
func (w *world) whole(t *testing.T, h held) {
	t.Helper()
	if got := w.get(t, w.devKey, "/workspaces/"+h.slug).object(t, http.StatusOK)["expires_at"]; got != timefmt.Format(h.expires) {
		t.Errorf("expires_at of %s = %v, want %s", h.slug, got, timefmt.Format(h.expires))
	}
	if r := w.get(t, h.pat, "/workspaces/"+h.slug); r.status != http.StatusOK {
		t.Errorf("GET %s with the bound token = %d %s, want 200", h.slug, r.status, r.body)
	}
	if n, err := w.secrets.Count(context.Background(), secret.ScopeWorkspace, h.slug); err != nil || n != 1 {
		t.Errorf("secrets of %s = %d, %v, want 1", h.slug, n, err)
	}
	if got := w.head(t, w.workspaces.RepoPath(h.slug), "task"); got != h.task {
		t.Errorf("branch task = %q, want %s", got, h.task)
	}
}

// eventsFor are the events of type typ of the workspace slug.
func (w *world) eventsFor(t *testing.T, typ, slug string) []event.Record {
	t.Helper()
	return slices.DeleteFunc(w.eventsOf(t, typ), func(r event.Record) bool {
		return r.Workspace == nil || *r.Workspace != slug
	})
}
