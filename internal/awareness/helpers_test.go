package awareness_test

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"maps"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/awareness"
	"github.com/agent-fox-dev/parley/internal/change"
	"github.com/agent-fox-dev/parley/internal/event"
	"github.com/agent-fox-dev/parley/internal/gitrepo"
	"github.com/agent-fox-dev/parley/internal/gitrepo/gitrepotest"
	"github.com/agent-fox-dev/parley/internal/hubtest"
	"github.com/agent-fox-dev/parley/internal/land"
	"github.com/agent-fox-dev/parley/internal/store"
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
	}, []store.Step{awareness.SchemaStep})
	if err != nil {
		f.t.Fatalf("open store: %v", err)
	}
	f.t.Cleanup(func() { st.Close() })
	f.store = st
	f.comp = &awareness.Component{}
	f.comp.Bind(awareness.Deps{
		Store: st, Events: f.events, Changes: f.changes,
		Repos: func(ctx context.Context, _ string) (*gitrepo.Repo, error) {
			return gitrepo.Git{}.Open(ctx, f.repo)
		},
		Logger: slog.New(slog.NewTextHandler(f.log, nil)),
	})
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
