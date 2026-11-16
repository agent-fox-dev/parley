package awareness_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/awareness"
	"github.com/agent-fox-dev/parley/internal/change"
	"github.com/agent-fox-dev/parley/internal/event"
	"github.com/agent-fox-dev/parley/internal/gitrepo"
	"github.com/agent-fox-dev/parley/internal/gitrepo/gitrepotest"
	"github.com/agent-fox-dev/parley/internal/hub"
	"github.com/agent-fox-dev/parley/internal/hubtest"
	"github.com/agent-fox-dev/parley/internal/land"
	"github.com/agent-fox-dev/parley/internal/store"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

// fixture is an awareness component on a real store and a real repository,
// with an event source and a change reader the test feeds: each event it
// appends names a change whose record the reader then returns.
type fixture struct {
	t       testing.TB
	dir     string
	repo    string
	store   *store.Store
	comp    *awareness.Component
	events  *eventFeed
	changes *changeBook
	log     *hubtest.LogBuffer
	// hubEvents is the real event log the component writes its events to,
	// on the fixture's store.
	hubEvents *event.Component
	// base is the root commit the heads of head start from, made at its
	// first call; heads counts the heads made.
	base  string
	heads int
}

// newFixture is a fixture on a fresh store, with an empty bare repository
// that every workspace's slug opens.
func newFixture(t testing.TB) *fixture {
	t.Helper()
	dir := t.TempDir()
	repo := filepath.Join(dir, "demo.git")
	gitrepotest.Git(t, dir, "init", "--quiet", "--bare", "--initial-branch=main", repo)
	return newFixtureOn(t, repo)
}

// newFixtureOn is a fixture on a fresh store and the repository repo.
func newFixtureOn(t testing.TB, repo string) *fixture {
	t.Helper()
	f := &fixture{t: t, dir: t.TempDir(), repo: repo, events: &eventFeed{}, changes: &changeBook{records: map[string]awareness.Change{}},
		log: &hubtest.LogBuffer{}}
	f.open()
	return f
}

// open opens the fixture's store and binds a new component to it, as a
// hub does at every start.
func (f *fixture) open() {
	f.t.Helper()
	st, err := store.Open(context.Background(), store.Options{
		DatabasePath:  filepath.Join(f.dir, "parley.db"),
		WorkspaceRoot: filepath.Join(f.dir, "workspaces"),
	}, []store.Step{event.SchemaStep, awareness.SchemaStep})
	if err != nil {
		f.t.Fatalf("open store: %v", err)
	}
	f.t.Cleanup(func() { st.Close() })
	f.store = st
	f.hubEvents, err = event.New(event.Deps{Store: st, Clock: event.SystemClock(nil)})
	if err != nil {
		f.t.Fatalf("bind the event component: %v", err)
	}
	f.comp = &awareness.Component{}
	f.comp.Bind(awareness.Deps{
		Store: st, Events: f.events, Changes: f.changes, Emitter: f.hubEvents,
		Repos: func(ctx context.Context, _ string) (*gitrepo.Repo, error) {
			return gitrepo.Git{}.Open(ctx, f.repo)
		},
		Logger: slog.New(slog.NewTextHandler(f.log, nil)),
	})
}

// served is a fixture whose component is a running hub's: the hub serves
// the awareness routes on the workspace demo over the index the fixture's
// events build, and the worker runs only when the test calls pass. The
// fixture's event log is not the hub's, so overlapEvents does not apply.
type served struct {
	*fixture
	hub *hubtest.Hub
}

// newServed is a served fixture with an empty bare repository, in which
// the hub's admin token reaches demo.
func newServed(t testing.TB) *served {
	t.Helper()
	dir := t.TempDir()
	repo := filepath.Join(dir, "demo.git")
	gitrepotest.Git(t, dir, "init", "--quiet", "--bare", "--initial-branch=main", repo)
	f := &fixture{t: t, dir: dir, repo: repo, events: &eventFeed{}, changes: &changeBook{records: map[string]awareness.Change{}},
		log: &hubtest.LogBuffer{}}
	h := hubtest.Start(t, func(c *hub.Composition) {
		f.comp = c.Awareness()
		c.AdjustAwareness(func(d *awareness.Deps) {
			d.Events, d.Changes, d.IndexInterval = f.events, f.changes, time.Hour
			d.Repos = func(ctx context.Context, _ string) (*gitrepo.Repo, error) { return gitrepo.Git{}.Open(ctx, f.repo) }
			d.Logger = slog.New(slog.NewTextHandler(f.log, nil))
		})
	})
	f.store = h.Store
	owner := h.CreateUser(t, "owner")
	h.AddWorkspace(t, access.Record{Slug: "demo", OwnerID: owner, OrgID: h.CreateOrg(t, "acme"), Kind: access.KindRepository,
		Status: access.StatusReady})
	return &served{fixture: f, hub: h}
}

// get sends GET /api/v1/workspaces/demo/<path> with the admin token and
// returns the answer's status and body.
func (s *served) get(path string) (int, []byte) {
	s.t.Helper()
	return s.hub.Call(s.t, http.MethodGet, "/api/v1/workspaces/demo/"+path, s.hub.Token, nil)
}

// changeHub is a hub whose awareness worker runs only when the test calls
// Pass, with the ready workspace demo, owned by a member of acme, whose
// repository holds main with api/a.go.
type changeHub struct {
	*hubtest.Hub
	comp *awareness.Component
	// key is the owner's API key.
	key string
	// repo is demo's repository; base is main's head.
	repo, base string
}

func newChangeHub(t testing.TB) *changeHub {
	t.Helper()
	var comp *awareness.Component
	var workspaces *workspace.Service
	h := hubtest.StartWith(t, hubtest.Config{Compose: func(c *hub.Composition) {
		comp, workspaces = c.Awareness(), c.Workspaces()
		c.AdjustAwareness(func(d *awareness.Deps) { d.IndexInterval = time.Hour })
	}})
	owner := h.CreateUser(t, "owner")
	key := h.IssueKey(t, owner)
	org := h.CreateOrg(t, "acme")
	h.AddMember(t, org, owner)
	dir := workspaces.RepoPath("demo")
	gitrepotest.Git(t, dir, "init", "--quiet", "--bare", "--initial-branch=main", dir)
	base := commit(t, dir, "main", "", map[string]string{"api/a.go": "a\n"})
	now := time.Now().UTC()
	rec := workspace.Workspace{
		Slug: "demo", Kind: access.KindRepository, Mode: workspace.ModeStandard,
		GitURL: new("https://origin.test/demo.git"), DefaultBranch: new("main"), OwnerID: owner, OrgID: org,
		Status: workspace.StatusReady, DisplayName: "demo", HeadSHA: &base,
		CreatedAt: now, UpdatedAt: now, InstanceID: workspace.NewInstanceID(),
	}
	txOf(t, h.Store, func(tx *sql.Tx) error { return workspace.Insert(context.Background(), tx, rec) })
	return &changeHub{Hub: h, comp: comp, key: key, repo: dir, base: base}
}

// open opens a change of demo from a new branch whose commit on main's
// head sets each of files, and returns its number.
func (h *changeHub) open(t testing.TB, branch string, files map[string]string) int64 {
	t.Helper()
	commit(t, h.repo, branch, h.base, files)
	status, raw := h.Call(t, http.MethodPost, "/api/v1/workspaces/demo/changes", h.key,
		map[string]any{"source": map[string]any{"branch": branch}, "title": "Rework " + branch, "intent": "Make it better."})
	if status != http.StatusCreated {
		t.Fatalf("POST changes from %s = %d %s, want 201", branch, status, raw)
	}
	var opened struct{ Number int64 }
	if err := json.Unmarshal(raw, &opened); err != nil {
		t.Fatal(err)
	}
	return opened.Number
}

// restart closes the store and opens it again under a new component.
func (f *fixture) restart() {
	f.t.Helper()
	if err := f.store.Close(); err != nil {
		f.t.Fatal(err)
	}
	f.open()
}

// pass runs one worker pass and fails the test on its error.
func (f *fixture) pass() {
	f.t.Helper()
	if err := f.comp.Pass(context.Background()); err != nil {
		f.t.Fatalf("pass: %v", err)
	}
}

// emit makes ch the change's record and appends an event of type typ
// naming it, and returns the event's seq.
func (f *fixture) emit(typ string, ch awareness.Change) int64 {
	if ch.Workspace == "" {
		ch.Workspace = "demo"
	}
	if ch.TargetBranch == "" {
		ch.TargetBranch = "main"
	}
	f.changes.set(ch)
	return f.events.add(typ, ch.Workspace, ch.Number)
}

// version emits a parley.change.version event of ch, in flight.
func (f *fixture) version(ch awareness.Change) int64 {
	if ch.State == "" {
		ch.State = awareness.StateOpen
	}
	return f.emit(change.EventVersion, ch)
}

// head makes a commit that touches each of paths, on top of a root commit
// all heads share, and returns the root and the head; a later head that
// touches a path again gives it other content.
func (f *fixture) head(paths ...string) (base, head string) {
	f.t.Helper()
	if f.base == "" {
		f.base = commit(f.t, f.repo, "main", "", map[string]string{"README": "demo\n"})
	}
	f.heads++
	files := map[string]string{}
	for _, p := range paths {
		files[p] = fmt.Sprintf("%s %d\n", p, f.heads)
	}
	return f.base, commit(f.t, f.repo, fmt.Sprintf("h%d", f.heads), f.base, files)
}

// touching is version v of change number, in flight on main, its version
// touching paths and its session session ("" for none).
func (f *fixture) touching(number, v int64, session string, paths ...string) awareness.Change {
	f.t.Helper()
	base, head := f.head(paths...)
	ch := awareness.Change{Number: number, Version: v, BaseSHA: &base, HeadSHA: head}
	if session != "" {
		ch.SessionID = &session
	}
	return ch
}

// overlapEvents are the parley.change.overlap events of the event log, in
// seq order.
func (f *fixture) overlapEvents() []event.Record {
	f.t.Helper()
	recs, _, err := f.hubEvents.ReadAfter(context.Background(), 0, 10000)
	if err != nil {
		f.t.Fatal(err)
	}
	var out []event.Record
	for _, r := range recs {
		if r.Type == awareness.EventOverlap {
			out = append(out, r)
		}
	}
	return out
}

// overlapMeta is the metadata of a parley.change.overlap event.
type overlapMeta struct {
	Number int64 `json:"number"`
	With   struct {
		Number int64  `json:"number"`
		Ref    string `json:"ref"`
	} `json:"with"`
	WithSessionID *string  `json:"with_session_id"`
	Paths         []string `json:"paths"`
	Truncated     bool     `json:"truncated"`
	Count         int      `json:"count"`
}

// metaOf decodes the metadata of the overlap event r.
func metaOf(t testing.TB, r event.Record) overlapMeta {
	t.Helper()
	var m overlapMeta
	if err := json.Unmarshal(r.Metadata, &m); err != nil {
		t.Fatalf("metadata %s: %v", r.Metadata, err)
	}
	return m
}

// announced are the pairs the overlap events name, each as "low-high" and
// once per event, sorted.
func (f *fixture) announced() []string {
	f.t.Helper()
	var out []string
	for _, r := range f.overlapEvents() {
		m := metaOf(f.t, r)
		out = append(out, fmt.Sprintf("%d-%d", min(m.Number, m.With.Number), max(m.Number, m.With.Number)))
	}
	slices.Sort(out)
	return out
}

// pairs are the pairs the index records as announced, each as "low-high",
// sorted.
func (f *fixture) pairs() []string {
	f.t.Helper()
	rows, err := f.store.Main().QueryContext(context.Background(), `SELECT low, high FROM awareness_pairs ORDER BY low, high`)
	if err != nil {
		f.t.Fatal(err)
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var low, high int64
		if err := rows.Scan(&low, &high); err != nil {
			f.t.Fatal(err)
		}
		out = append(out, fmt.Sprintf("%d-%d", low, high))
	}
	return out
}

// partners are the numbers of the changes that overlap change number of
// demo, in the order Partners gives them.
func (f *fixture) partners(number int64) []int64 {
	f.t.Helper()
	got, err := f.comp.Partners(context.Background(), nil, "demo", number)
	if err != nil {
		f.t.Fatal(err)
	}
	out := []int64{}
	for _, p := range got {
		out = append(out, p.Number)
	}
	return out
}

// The sessions of the tests' changes; the event log takes a session id only
// in UUID form.
const (
	session1 = "11111111-1111-4111-8111-111111111111"
	session2 = "22222222-2222-4222-8222-222222222222"
)

// files are the files the index holds for change number of demo.
func (f *fixture) files(number int64) []string {
	f.t.Helper()
	got, err := f.comp.Files(context.Background(), nil, "demo", number)
	if err != nil {
		f.t.Fatal(err)
	}
	return got
}

// inFlight are the in-flight changes of demo on target.
func (f *fixture) inFlight(target string) []int64 {
	f.t.Helper()
	got, err := f.comp.InFlight(context.Background(), nil, "demo", target)
	if err != nil {
		f.t.Fatal(err)
	}
	return got
}

// position is the worker's stored position.
func (f *fixture) position() int64 {
	f.t.Helper()
	got, err := f.comp.Position(context.Background())
	if err != nil {
		f.t.Fatal(err)
	}
	return got
}

// dump is every row of the index, one line each, in a fixed order.
func (f *fixture) dump() []string {
	f.t.Helper()
	var out []string
	for _, query := range []string{
		`SELECT workspace_slug, number, target_branch, phase, landed_sha, landed_at, session_id FROM awareness_changes ORDER BY 1, 2`,
		`SELECT workspace_slug, number, path FROM awareness_files ORDER BY 1, 2, 3`,
		`SELECT workspace_slug, low, high FROM awareness_pairs ORDER BY 1, 2, 3`,
	} {
		rows, err := f.store.Main().QueryContext(context.Background(), query)
		if err != nil {
			f.t.Fatal(err)
		}
		cols, _ := rows.Columns()
		for rows.Next() {
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				f.t.Fatal(err)
			}
			out = append(out, fmt.Sprint(vals...))
		}
		if err := rows.Close(); err != nil {
			f.t.Fatal(err)
		}
	}
	return out
}

// eventFeed is an event source over the events a test appends.
type eventFeed struct {
	mu      sync.Mutex
	records []event.Record
}

func (e *eventFeed) add(typ, workspace string, number int64) int64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	seq := int64(len(e.records) + 1)
	e.records = append(e.records, event.Record{
		Seq: seq, ID: strconv.FormatInt(seq, 10), Type: typ, Time: time.Unix(1700000000+seq, 0).UTC(), Workspace: &workspace,
		ActorType: event.ActorSystem, ResourceType: "change", ResourceID: strconv.FormatInt(number, 10),
		Metadata: []byte(`{}`),
	})
	return seq
}

func (e *eventFeed) ReadAfter(_ context.Context, after int64, limit int) ([]event.Record, int64, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []event.Record
	for _, r := range e.records {
		if r.Seq <= after {
			continue
		}
		if limit > 0 && len(out) == limit {
			return out, out[len(out)-1].Seq, nil
		}
		out = append(out, r)
	}
	return out, int64(len(e.records)), nil
}

// changeBook is a change reader over the records a test sets, which counts
// its reads.
type changeBook struct {
	mu      sync.Mutex
	records map[string]awareness.Change
	reads   int
}

func (b *changeBook) set(ch awareness.Change) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.records[ch.Workspace+"#"+strconv.FormatInt(ch.Number, 10)] = ch
}

func (b *changeBook) ReadChange(_ context.Context, _ awareness.Querier, workspace string, number int64) (awareness.Change, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.reads++
	ch, ok := b.records[workspace+"#"+strconv.FormatInt(number, 10)]
	if !ok {
		return awareness.Change{}, awareness.ErrChangeNotFound
	}
	return ch, nil
}

func (b *changeBook) readCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.reads
}

const committer = "committer T <t@example.invalid> 1700000000 +0000\n"

// data is a fast-import data command for s.
func data(s string) string { return "data " + strconv.Itoa(len(s)) + "\n" + s + "\n" }

// gone is the content commit gives a file to delete it.
const gone = "\x00gone"

// commit writes to the repository at dir a commit on top of from (a
// commit id or a branch), or a root commit for from "", setting each file
// of files to its content, or deleting it for gone, and points branch at
// it; it returns the commit's id.
func commit(t testing.TB, dir, branch, from string, files map[string]string) string {
	t.Helper()
	var s strings.Builder
	s.WriteString("commit refs/heads/" + branch + "\n" + committer + data(branch))
	if from != "" {
		s.WriteString("from " + from + "\n")
	}
	for _, path := range slices.Sorted(maps.Keys(files)) {
		if files[path] == gone {
			s.WriteString("D " + path + "\n")
			continue
		}
		s.WriteString("M 100644 inline " + path + "\n" + data(files[path]))
	}
	if _, err := gitrepotest.Run(dir, s.String()+"\n", "fast-import", "--quiet", "--force"); err != nil {
		t.Fatal(err)
	}
	return gitrepotest.Refs(t, dir, "refs/heads/")[branch]
}

// The other event types the index follows.
const (
	eventLand    = land.EventLand
	eventAbandon = change.EventAbandon
	eventRestore = change.EventRestore
)

// txOf runs fn in a transaction of st.
func txOf(t testing.TB, st *store.Store, fn func(*sql.Tx) error) {
	t.Helper()
	if err := st.Tx(context.Background(), fn); err != nil {
		t.Fatal(err)
	}
}

// jsonHasKeys reports whether raw is a JSON object with exactly the
// members keys.
func jsonHasKeys(t testing.TB, raw []byte, keys ...string) bool {
	t.Helper()
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		t.Fatalf("%s is not a JSON object: %v", raw, err)
	}
	got := slices.Sorted(maps.Keys(obj))
	return slices.Equal(got, slices.Sorted(slices.Values(keys)))
}
