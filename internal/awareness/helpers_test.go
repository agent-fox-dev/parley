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
	"github.com/agent-fox-dev/parley/internal/identity"
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
	// now is the component's clock; nil for time.Now.
	now func() time.Time
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
	return newFixtureAt(t, nil)
}

// newFixtureAt is newFixture with now as the component's clock.
func newFixtureAt(t testing.TB, now func() time.Time) *fixture {
	t.Helper()
	dir := t.TempDir()
	repo := filepath.Join(dir, "demo.git")
	gitrepotest.Git(t, dir, "init", "--quiet", "--bare", "--initial-branch=main", repo)
	return fixtureOn(t, repo, now)
}

// newFixtureOn is a fixture on a fresh store and the repository repo.
func newFixtureOn(t testing.TB, repo string) *fixture {
	t.Helper()
	return fixtureOn(t, repo, nil)
}

// fixtureOn is a fixture on a fresh store and the repository repo, with now
// as the component's clock (nil for time.Now).
func fixtureOn(t testing.TB, repo string, now func() time.Time) *fixture {
	t.Helper()
	f := &fixture{t: t, dir: t.TempDir(), repo: repo, events: &eventFeed{}, changes: &changeBook{records: map[string]awareness.Change{}},
		log: &hubtest.LogBuffer{}, now: now}
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
	}, []store.Step{event.SchemaStep, awareness.SchemaStep, awareness.ClaimSchemaStep})
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
		Now:    f.now,
	})
}

// served is a fixture whose component is a running hub's: the hub serves
// the awareness routes on the workspace demo over the index the fixture's
// events build, and the worker runs only when the test calls pass. The
// fixture's event log is not the hub's, so overlapEvents does not apply.
type served struct {
	*fixture
	hub *hubtest.Hub
	// owner is the user who owns demo and acme; key is the owner's API key.
	owner, key string
	// org is acme's id.
	org string
}

// servedConfig is what newServedWith starts a served fixture's hub with.
type servedConfig struct {
	// TOML is added to the hub's config.toml, such as an [awareness]
	// section.
	TOML string
	// Now is the awareness component's clock; nil for the hub's.
	Now func() time.Time
	// Compose is run on the hub's composition after the fixture's own
	// changes; nil for nothing.
	Compose func(*hub.Composition)
}

// newServed is a served fixture with an empty bare repository, in which
// the hub's admin token reaches demo.
func newServed(t testing.TB) *served {
	t.Helper()
	return newServedWith(t, servedConfig{})
}

// newServedWith is newServed with the hub started as cfg says.
func newServedWith(t testing.TB, cfg servedConfig) *served {
	t.Helper()
	dir := t.TempDir()
	repo := filepath.Join(dir, "demo.git")
	gitrepotest.Git(t, dir, "init", "--quiet", "--bare", "--initial-branch=main", repo)
	f := &fixture{t: t, dir: dir, repo: repo, events: &eventFeed{}, changes: &changeBook{records: map[string]awareness.Change{}},
		log: &hubtest.LogBuffer{}}
	h := hubtest.StartWith(t, hubtest.Config{TOML: cfg.TOML, Compose: func(c *hub.Composition) {
		f.comp = c.Awareness()
		c.AdjustAwareness(func(d *awareness.Deps) {
			d.Events, d.Changes, d.IndexInterval, d.ExpiryInterval = f.events, f.changes, time.Hour, time.Hour
			d.Repos = func(ctx context.Context, _ string) (*gitrepo.Repo, error) { return gitrepo.Git{}.Open(ctx, f.repo) }
			d.Logger = slog.New(slog.NewTextHandler(f.log, nil))
			if cfg.Now != nil {
				d.Now = cfg.Now
			}
		})
		if cfg.Compose != nil {
			cfg.Compose(c)
		}
	}})
	f.store = h.Store
	owner := h.CreateUser(t, "owner")
	org := h.CreateOrg(t, "acme")
	h.AddWorkspace(t, access.Record{Slug: "demo", OwnerID: owner, OrgID: org, Kind: access.KindRepository,
		Status: access.StatusReady})
	txOf(t, h.Store, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(context.Background(), `UPDATE workspaces SET default_branch = 'main' WHERE slug = 'demo'`)
		return err
	})
	return &served{fixture: f, hub: h, owner: owner, key: h.IssueKey(t, owner), org: org}
}

// activity gets GET …/activity?query, which must answer 200 with exactly
// the five members of the activity and three arrays.
func (s *served) activity(t *testing.T, query string) activityAnswer {
	t.Helper()
	status, raw := s.get("activity?" + query)
	if status != http.StatusOK {
		t.Fatalf("GET activity?%s = %d %s, want 200", query, status, raw)
	}
	if !jsonHasKeys(t, raw, "target_branch", "target_sha", "changes", "claims", "landed") {
		t.Errorf("GET activity?%s = %s, want exactly target_branch, target_sha, changes, claims and landed", query, raw)
	}
	var lists map[string]json.RawMessage
	if err := json.Unmarshal(raw, &lists); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"changes", "claims", "landed"} {
		if !strings.HasPrefix(string(lists[key]), "[") {
			t.Errorf("%s = %s, want an array", key, lists[key])
		}
	}
	var a activityAnswer
	if err := json.Unmarshal(raw, &a); err != nil {
		t.Fatal(err)
	}
	return a
}

// activityAnswer is the body of an activity answer.
type activityAnswer struct {
	TargetBranch string  `json:"target_branch"`
	TargetSHA    *string `json:"target_sha"`
	Changes      []struct {
		Change struct {
			Number int64 `json:"number"`
		} `json:"change"`
		Paths []string `json:"paths"`
		Count int      `json:"count"`
	} `json:"changes"`
	Claims []struct {
		ID string `json:"id"`
	} `json:"claims"`
	Landed []struct {
		Number int64 `json:"number"`
	} `json:"landed"`
}

// changeCounts are the changes of the answer, each as "number:count", in
// order.
func (a activityAnswer) changeCounts() []string {
	out := []string{}
	for _, e := range a.Changes {
		out = append(out, fmt.Sprintf("%d:%d", e.Change.Number, e.Count))
	}
	return out
}

// claimIDs are the ids of the claims of the answer, in order.
func (a activityAnswer) claimIDs() []string {
	out := []string{}
	for _, c := range a.Claims {
		out = append(out, c.ID)
	}
	return out
}

// landedNumbers are the numbers of the landed changes of the answer, in
// order.
func (a activityAnswer) landedNumbers() []int64 {
	out := []int64{}
	for _, l := range a.Landed {
		out = append(out, l.Number)
	}
	return out
}

// The ids of the tests' claims.
const (
	claimA = "aaaaaaaa-0000-4000-8000-00000000000a"
	claimB = "aaaaaaaa-0000-4000-8000-00000000000b"
	claimC = "aaaaaaaa-0000-4000-8000-00000000000c"
	claimD = "aaaaaaaa-0000-4000-8000-00000000000d"
	claimE = "aaaaaaaa-0000-4000-8000-00000000000e"
	claimF = "aaaaaaaa-0000-4000-8000-00000000000f"
)

// seedClaims stores each of claims as it is.
func (f *fixture) seedClaims(claims ...awareness.Claim) {
	f.t.Helper()
	txOf(f.t, f.store, func(tx *sql.Tx) error {
		for _, cl := range claims {
			if err := awareness.InsertClaim(context.Background(), tx, cl); err != nil {
				return err
			}
		}
		return nil
	})
}

// landed emits a version event of ch, in flight, and then a land event of
// ch landed as sha at at.
func (f *fixture) landed(ch awareness.Change, sha string, at time.Time) {
	f.version(ch)
	ch.State, ch.Landed = awareness.StateLanded, &awareness.Landed{SHA: sha, At: at}
	f.emit(eventLand, ch)
}

// firstOf is the first element of the array member key of the JSON object
// raw.
func firstOf(t testing.TB, raw []byte, key string) json.RawMessage {
	t.Helper()
	var obj map[string]json.RawMessage
	var list []json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		t.Fatalf("%s: %v", raw, err)
	}
	if err := json.Unmarshal(obj[key], &list); err != nil || len(list) == 0 {
		t.Fatalf("%s has no %s", raw, key)
	}
	return list[0]
}

// errorType is the error_type of the error answer raw.
func errorType(t testing.TB, raw []byte) string {
	t.Helper()
	var body struct {
		Error struct {
			ErrorType string `json:"error_type"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("body %s: %v", raw, err)
	}
	return body.Error.ErrorType
}

// deref is *p, or "" for nil.
func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
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
	// owner is the id of demo's owner; key is the owner's API key.
	owner, key string
	// repo is demo's repository; base is main's head.
	repo, base string
}

func newChangeHub(t testing.TB) *changeHub {
	t.Helper()
	return newChangeHubWith(t, nil, nil)
}

// newChangeHubWith is newChangeHub with clock as the hub's clock (nil for
// its own) and adjust run on the awareness component's dependencies after
// the fixture's own changes (nil for nothing), such as to give the worker
// another interval.
func newChangeHubWith(t testing.TB, clock func() time.Time, adjust func(*awareness.Deps)) *changeHub {
	t.Helper()
	var comp *awareness.Component
	var workspaces *workspace.Service
	h := hubtest.StartWith(t, hubtest.Config{Now: clock, Compose: func(c *hub.Composition) {
		comp, workspaces = c.Awareness(), c.Workspaces()
		c.AdjustAwareness(func(d *awareness.Deps) { d.IndexInterval = time.Hour })
		if adjust != nil {
			c.AdjustAwareness(adjust)
		}
	}})
	owner := h.CreateUser(t, "owner")
	key := h.IssueKey(t, owner)
	org := h.CreateOrg(t, "acme")
	h.AddMember(t, org, owner)
	dir := workspaces.RepoPath("demo")
	gitrepotest.InitBare(t, dir, "main")
	base := commit(t, dir, "main", "", map[string]string{"api/a.go": "a\n"})
	now := time.Now().UTC()
	rec := workspace.Workspace{
		Slug: "demo", Kind: access.KindRepository, Mode: workspace.ModeStandard,
		GitURL: new("https://origin.test/demo.git"), DefaultBranch: new("main"), OwnerID: owner, OrgID: org,
		Status: workspace.StatusReady, DisplayName: "demo", HeadSHA: &base,
		CreatedAt: now, UpdatedAt: now, InstanceID: workspace.NewInstanceID(),
	}
	txOf(t, h.Store, func(tx *sql.Tx) error { return workspace.Insert(context.Background(), tx, rec) })
	return &changeHub{Hub: h, comp: comp, owner: owner, key: key, repo: dir, base: base}
}

// open opens a change of demo from a new branch whose commit on main's
// head sets each of files, and returns its number.
func (h *changeHub) open(t testing.TB, branch string, files map[string]string) int64 {
	t.Helper()
	var opened struct{ Number int64 }
	if err := json.Unmarshal(h.opened(t, branch, files), &opened); err != nil {
		t.Fatal(err)
	}
	return opened.Number
}

// opened is open's answer, the change as the open created it.
func (h *changeHub) opened(t testing.TB, branch string, files map[string]string) []byte {
	t.Helper()
	commit(t, h.repo, branch, h.base, files)
	status, raw := h.Call(t, http.MethodPost, "/api/v1/workspaces/demo/changes", h.key,
		map[string]any{"source": map[string]any{"branch": branch}, "title": "Rework " + branch, "intent": "Make it better."})
	if status != http.StatusCreated {
		t.Fatalf("POST changes from %s = %d %s, want 201", branch, status, raw)
	}
	return raw
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
// naming it, with ch's version in its metadata as the change events carry
// it, and returns the event's seq.
func (f *fixture) emit(typ string, ch awareness.Change) int64 {
	if ch.Workspace == "" {
		ch.Workspace = "demo"
	}
	if ch.TargetBranch == "" {
		ch.TargetBranch = "main"
	}
	f.changes.set(ch)
	return f.events.add(typ, ch.Workspace, ch.Number, fmt.Sprintf(`{"version":%d}`, ch.Version))
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

func (e *eventFeed) add(typ, workspace string, number int64, meta string) int64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	seq := int64(len(e.records) + 1)
	e.records = append(e.records, event.Record{
		Seq: seq, ID: strconv.FormatInt(seq, 10), Type: typ, Time: time.Unix(1700000000+seq, 0).UTC(), Workspace: &workspace,
		ActorType: event.ActorSystem, ResourceType: "change", ResourceID: strconv.FormatInt(number, 10),
		Metadata: []byte(meta),
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
// its reads; while failing is set, every read fails with it.
type changeBook struct {
	mu      sync.Mutex
	records map[string]awareness.Change
	reads   int
	failing error
}

// fail makes every read from now on fail with err.
func (b *changeBook) fail(err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.failing = err
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
	if b.failing != nil {
		return awareness.Change{}, b.failing
	}
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

// clock is a clock that moves only when the test moves it.
type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// advance moves the clock d forward.
func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// postClaim sends POST /api/v1/workspaces/<slug>/claims with the owner's
// API key and body, and returns the answer's status and body.
func (s *served) postClaim(t testing.TB, slug string, body any) (int, []byte) {
	t.Helper()
	return s.hub.Call(t, http.MethodPost, "/api/v1/workspaces/"+slug+"/claims", s.key, body)
}

// createClaim posts body to demo's claims, which must answer 201, and
// returns the new claim's id.
func (s *served) createClaim(t testing.TB, slug string, body any) string {
	t.Helper()
	status, raw := s.postClaim(t, slug, body)
	if status != http.StatusCreated {
		t.Fatalf("POST %s/claims %v = %d %s, want 201", slug, body, status, raw)
	}
	var created struct {
		Claim struct {
			ID string `json:"id"`
		} `json:"claim"`
	}
	if err := json.Unmarshal(raw, &created); err != nil {
		t.Fatal(err)
	}
	return created.Claim.ID
}

// count is the number of rows of table in the hub's store.
func (s *served) count(t testing.TB, table string) int {
	t.Helper()
	var n int
	if err := s.hub.Store.Main().QueryRowContext(context.Background(), `SELECT count(*) FROM `+table).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// sessions is a session interface that accepts the session ids of
// accepted and records each claim link it is told of.
type sessions struct {
	mu       sync.Mutex
	accepted []string
	records  []string
}

func (s *sessions) Check(_ context.Context, _ awareness.Actor, id, _ string) (string, error) {
	if !slices.Contains(s.accepted, strings.ToLower(id)) {
		return "", awareness.ErrUnknownSession
	}
	return strings.ToLower(id), nil
}

func (s *sessions) Record(_ context.Context, _ *sql.Tx, id, workspace, claim string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.records = append(s.records, id+" "+workspace+" "+claim)
	return nil
}

// claimAnswer is the body of a claim creation's answer.
type claimAnswer struct {
	Claim    claimBody      `json:"claim"`
	Activity activityAnswer `json:"activity"`
}

// claimBody is a claim object as an answer shows it.
type claimBody struct {
	ID           string   `json:"id"`
	Workspace    string   `json:"workspace_slug"`
	Paths        []string `json:"paths"`
	TargetBranch string   `json:"target_branch"`
	Intent       string   `json:"intent"`
	By           struct {
		ActorType string  `json:"actor_type"`
		ActorID   *string `json:"actor_id"`
		Username  *string `json:"username"`
	} `json:"by"`
	SessionID *string `json:"session_id"`
	Change    *int64  `json:"change"`
	ExpiresAt string  `json:"expires_at"`
	State     string  `json:"state"`
	CreatedAt string  `json:"created_at"`
}

// storedEvent is a row of the hub's event log.
type storedEvent struct {
	Type, ResourceType, ResourceID string
	Workspace, ActorID, SessionID  *string
	ActorType                      string
	Metadata                       map[string]any
}

// lastEvent is the newest event of the hub's event log.
func (s *served) lastEvent(t *testing.T) storedEvent {
	t.Helper()
	var ev storedEvent
	var meta string
	if err := s.hub.Store.Main().QueryRowContext(context.Background(), `SELECT type, resource_type, resource_id, workspace,
	actor_type, actor_id, session_id, metadata FROM hub_events ORDER BY seq DESC LIMIT 1`).Scan(&ev.Type, &ev.ResourceType,
		&ev.ResourceID, &ev.Workspace, &ev.ActorType, &ev.ActorID, &ev.SessionID, &meta); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(meta), &ev.Metadata); err != nil {
		t.Fatal(err)
	}
	return ev
}

// firstMember is the member key of the JSON object raw.
func firstMember(t testing.TB, raw []byte, key string) []byte {
	t.Helper()
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		t.Fatalf("%s: %v", raw, err)
	}
	return obj[key]
}

// member makes a new user name a member of acme and returns the user's id
// and an API key of theirs.
func (s *served) member(t testing.TB, name string) (id, key string) {
	t.Helper()
	id = s.hub.CreateUser(t, name)
	s.hub.AddMember(t, s.org, id)
	return id, s.hub.IssueKey(t, id)
}

// pat issues a PAT of the user that holds perms.
func (s *served) pat(t testing.TB, user string, perms ...string) string {
	t.Helper()
	return s.hub.IssuePAT(t, identity.PATSpec{UserID: user, Permissions: perms})
}

// call sends method /api/v1/workspaces/<path> with credential and body and
// returns the answer's status and body.
func (s *served) call(t testing.TB, method, path, credential string, body any) (int, []byte) {
	t.Helper()
	return s.hub.Call(t, method, "/api/v1/workspaces/"+path, credential, body)
}

// claim gets demo's claim id with the admin token, which must answer 200,
// and returns it decoded and as it was sent.
func (s *served) claim(t testing.TB, id string) (claimBody, []byte) {
	t.Helper()
	return s.claimOn(t, "demo", id)
}

// claimOn is claim for the claim id of the workspace slug.
func (s *served) claimOn(t testing.TB, slug, id string) (claimBody, []byte) {
	t.Helper()
	status, raw := s.call(t, http.MethodGet, slug+"/claims/"+id, s.hub.Token, nil)
	if status != http.StatusOK {
		t.Fatalf("GET %s/claims/%s = %d %s, want 200", slug, id, status, raw)
	}
	var cl claimBody
	if err := json.Unmarshal(raw, &cl); err != nil {
		t.Fatal(err)
	}
	return cl, raw
}

// claimRows are the stored claims, every column, one line each, by id.
func (s *served) claimRows(t testing.TB) []string {
	t.Helper()
	rows, err := s.hub.Store.Main().QueryContext(context.Background(), `SELECT id, workspace_slug, target_branch, paths, intent,
	actor_type, actor_id, username, session_id, change_number, state, created_at, expires_at, ended_at
	FROM awareness_claims ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		vals := make([]any, 14)
		ptrs := make([]any, len(vals))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			t.Fatal(err)
		}
		out = append(out, fmt.Sprint(vals...))
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// eventsOf is the number of events of type typ in the hub's event log.
func (s *served) eventsOf(t testing.TB, typ string) int {
	t.Helper()
	var n int
	if err := s.hub.Store.Main().QueryRowContext(context.Background(), `SELECT count(*) FROM hub_events WHERE type = ?`,
		typ).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// ended makes cl a claim that left the state active for state at at.
func ended(state string, at time.Time) func(*awareness.Claim) {
	return func(cl *awareness.Claim) { cl.State, cl.EndedAt = state, &at }
}

// claimPage is the body of a page of the claim list.
type claimPage struct {
	Items      []claimBody `json:"items"`
	NextCursor *string     `json:"next_cursor"`
}

// ids are the ids of the page's claims, in order.
func (p claimPage) ids() []string {
	out := []string{}
	for _, c := range p.Items {
		out = append(out, c.ID)
	}
	return out
}

// newClaim is an active claim id of demo on main for api/**, made with an
// API key at created and expiring an hour later, as edit (nil for none)
// changes it.
func newClaim(id string, created time.Time, edit func(*awareness.Claim)) awareness.Claim {
	cl := awareness.Claim{
		ID: id, Workspace: "demo", TargetBranch: "main", Paths: []string{"api/**"}, Intent: "rework",
		By:    awareness.Actor{ActorType: "api_key", ActorID: new("u1"), Username: new("dev")},
		State: awareness.ClaimActive, CreatedAt: created, ExpiresAt: created.Add(time.Hour),
	}
	if edit != nil {
		edit(&cl)
	}
	return cl
}

// stored is the claim id of demo as the fixture's store holds it.
func (f *fixture) stored(id string) awareness.Claim {
	f.t.Helper()
	return storedOn(f.t, f.store, "demo", id)
}

// storedOn is the claim id of workspace as st holds it, which must exist.
func storedOn(t testing.TB, st *store.Store, workspace, id string) awareness.Claim {
	t.Helper()
	cl, found, err := awareness.ReadClaim(context.Background(), st.Main(), workspace, id)
	if err != nil || !found {
		t.Fatalf("the claim %s of %s: found %v, %v", id, workspace, found, err)
	}
	return cl
}

// claimIDsIn are the ids of every claim st holds, sorted.
func claimIDsIn(t testing.TB, st *store.Store) []string {
	t.Helper()
	rows, err := st.Main().QueryContext(context.Background(), `SELECT id FROM awareness_claims ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		out = append(out, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// logged are the events of the fixture's event log whose type starts with
// prefix, in seq order.
func (f *fixture) logged(prefix string) []event.Record {
	f.t.Helper()
	recs, _, err := f.hubEvents.ReadAfter(context.Background(), 0, 10000)
	if err != nil {
		f.t.Fatal(err)
	}
	var out []event.Record
	for _, r := range recs {
		if strings.HasPrefix(r.Type, prefix) {
			out = append(out, r)
		}
	}
	return out
}
