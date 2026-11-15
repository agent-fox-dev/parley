package change_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/change"
	"github.com/agent-fox-dev/parley/internal/event"
	"github.com/agent-fox-dev/parley/internal/fork"
	"github.com/agent-fox-dev/parley/internal/gitrepo"
	"github.com/agent-fox-dev/parley/internal/gitrepo/gitrepotest"
	"github.com/agent-fox-dev/parley/internal/hub"
	"github.com/agent-fox-dev/parley/internal/hubtest"
	"github.com/agent-fox-dev/parley/internal/identity"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

// Session ids, which the event log takes only as UUIDs: sessionOne and
// sessionTwo are sessions the world's session linker accepts, sessionOther
// one it refuses.
const (
	sessionOne   = "11111111-1111-4111-8111-111111111111"
	sessionTwo   = "22222222-2222-4222-8222-222222222222"
	sessionOther = "33333333-3333-4333-8333-333333333333"
)

// world is a hub with the change component composed: the user owner, a
// member of the org acme, with the API key ownerKey; the owner's ready
// workspace demo, whose main and release are at one commit (base) holding
// a.txt ("l1\nl2\nl3\n"); a session linker that accepts sessionOne and
// sessionTwo, registered on the change component. Its mergeability checks
// are not spaced, unless the configuration has a [changes] table.
type world struct {
	*hubtest.Hub
	workspaces *workspace.Service
	events     *event.Component
	changes    *change.Component
	forks      *fork.Component
	sessions   *sessions

	owner, ownerKey, org string
	// demo is the git directory of demo's repository, and base the commit
	// its main and release are at.
	demo, base string
	// held is set once hold keeps the mergeability checks from starting.
	held bool
}

func newWorld(t *testing.T) *world {
	t.Helper()
	return newWorldWith(t, hubtest.Config{})
}

// newWorldWith is newWorld on a hub started with cfg, whose Compose runs
// after the world's own registrations.
func newWorldWith(t *testing.T, cfg hubtest.Config) *world {
	t.Helper()
	w := &world{sessions: &sessions{accept: map[string]bool{sessionOne: true, sessionTwo: true}}}
	if !strings.Contains(cfg.TOML, "[changes]") {
		cfg.TOML = zeroDebounce + cfg.TOML
	}
	compose := cfg.Compose
	cfg.Compose = func(c *hub.Composition) {
		w.workspaces, w.events, w.changes, w.forks = c.Workspaces(), c.Events(), c.Changes(), c.Forks()
		w.changes.RegisterSessionLinker(w.sessions)
		if compose != nil {
			compose(c)
		}
	}
	w.Hub = hubtest.StartWith(t, cfg)
	w.owner = w.CreateUser(t, "owner")
	w.ownerKey = w.IssueKey(t, w.owner)
	w.org = w.CreateOrg(t, "acme")
	w.AddMember(t, w.org, w.owner)
	w.demo = w.addWorkspace(t, "demo")
	w.base = gitrepotest.Refs(t, w.demo, "refs/heads/")["main"]
	return w
}

// addWorkspace adds the owner's ready workspace slug, whose main and
// release hold one commit with a.txt, and returns its git directory.
func (w *world) addWorkspace(t *testing.T, slug string) string {
	t.Helper()
	ctx, now, dir := context.Background(), time.Now().UTC(), w.workspaces.RepoPath(slug)
	gitrepotest.Git(t, dir, "init", "--quiet", "--bare", "--initial-branch=main", dir)
	if _, err := gitrepotest.Run(dir, "commit refs/heads/main\n"+committer+data("base")+
		"M 100644 inline a.txt\n"+data("l1\nl2\nl3\n")+"\nreset refs/heads/release\nfrom refs/heads/main\n\n",
		"fast-import", "--quiet"); err != nil {
		t.Fatal(err)
	}
	head := gitrepotest.Refs(t, dir, "refs/heads/")["main"]
	rec := workspace.Workspace{
		Slug: slug, Kind: access.KindRepository, Mode: workspace.ModeStandard,
		GitURL: new("https://origin.test/" + slug + ".git"), DefaultBranch: new("main"), OwnerID: w.owner, OrgID: w.org,
		Status: workspace.StatusReady, DisplayName: slug, HeadSHA: &head,
		CreatedAt: now, UpdatedAt: now, InstanceID: workspace.NewInstanceID(),
	}
	if err := w.Store.Tx(ctx, func(tx *sql.Tx) error { return workspace.Insert(ctx, tx, rec) }); err != nil {
		t.Fatal(err)
	}
	return dir
}

// addFork adds the owner's ready task fork slug of demo, based on base,
// with purpose and session (nil for none), and returns its git directory.
func (w *world) addFork(t *testing.T, slug, base string, purpose, session *string) string {
	t.Helper()
	ctx, now, dir := context.Background(), time.Now().UTC(), w.workspaces.RepoPath(slug)
	f := fork.Fork{Workspace: workspace.Workspace{
		Slug: slug, Kind: access.KindFork, Mode: workspace.ModeStandard, DefaultBranch: new(base),
		OwnerID: w.owner, OrgID: w.org, Status: workspace.StatusReady, DisplayName: slug,
		ParentSlug: new("demo"), ForkPoint: &workspace.ForkPoint{Ref: base, SHA: w.base},
		ExpiresAt: new(now.Add(7 * 24 * time.Hour)), AutoChange: new(false), Purpose: purpose, SessionID: session,
		CreatedAt: now, UpdatedAt: now, InstanceID: workspace.NewInstanceID(),
	}}
	if err := (gitrepo.Git{}).CreateFork(ctx, w.demo, dir); err != nil {
		t.Fatal(err)
	}
	if err := w.Store.Tx(ctx, func(tx *sql.Tx) error { return fork.Insert(ctx, tx, f) }); err != nil {
		t.Fatal(err)
	}
	return dir
}

const committer = "committer T <t@example.invalid> 1700000000 +0000\n"

// data is a fast-import data command for s.
func data(s string) string { return "data " + strconv.Itoa(len(s)) + "\n" + s + "\n" }

// deref is the string p points to, "" for nil.
func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// gone is the content commit gives a file to delete it.
const gone = "\x00gone"

// commit writes to the repository at dir a commit with message on top of
// from (a commit id or a branch), or a root commit for from "", setting
// each file of files to its content, or deleting it for gone, and points
// branch at it, also when that rewrites the branch; it returns the
// commit's id.
func commit(t *testing.T, dir, branch, from, message string, files map[string]string) string {
	t.Helper()
	var s strings.Builder
	s.WriteString("commit refs/heads/" + branch + "\n" + committer + data(message))
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

// branch is commit on top of the world's base in demo.
func (w *world) branch(t *testing.T, name, message string) string {
	t.Helper()
	return commit(t, w.demo, name, w.base, message, map[string]string{name + ".txt": name + "\n"})
}

// post opens a change in the workspace slug through the route, with the
// credential and the body, and returns the status and the decoded answer.
func (w *world) post(t *testing.T, slug, credential string, body any) (int, map[string]any) {
	t.Helper()
	status, raw := w.Call(t, http.MethodPost, "/api/v1/workspaces/"+slug+"/changes", credential, body)
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("POST changes answered %d %q: %v", status, raw, err)
	}
	return status, out
}

// get reads path, relative to /api/v1, with the credential, and returns
// the status, the decoded answer and the answer as received.
func (w *world) get(t *testing.T, path, credential string) (int, map[string]any, []byte) {
	t.Helper()
	status, raw := w.Call(t, http.MethodGet, "/api/v1"+path, credential, nil)
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("GET %s answered %d %q: %v", path, status, raw, err)
	}
	return status, out, raw
}

// send sends a request with the method, path (relative to /api/v1), the
// credential and body (nil for none), and returns the status and the
// decoded answer, nil for an empty one.
func (w *world) send(t *testing.T, method, path, credential string, body any) (int, map[string]any) {
	t.Helper()
	status, raw := w.Call(t, method, "/api/v1"+path, credential, body)
	var out map[string]any
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatalf("%s %s answered %d %q: %v", method, path, status, raw, err)
		}
	}
	return status, out
}

// remote is the git URL of the workspace slug of the org acme on the hub,
// with the owner's API key as its password, as a git client is given it.
func (w *world) remote(slug string) string {
	u, err := url.Parse(w.URL("/git/acme/" + slug + ".git"))
	if err != nil {
		panic(err)
	}
	u.User = url.UserPassword("x", w.ownerKey)
	return u.String()
}

// clone clones the workspace slug over the hub's git URL with stock git
// and returns the working tree, in which commits are made as P.
func (w *world) clone(t *testing.T, slug string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), slug)
	clientGit(t, "", "clone", "--quiet", w.remote(slug), dir)
	clientGit(t, dir, "config", "user.name", "P")
	clientGit(t, dir, "config", "user.email", "p@example.invalid")
	return dir
}

// clientGit runs stock git with args in dir as a client of the hub, with
// none of the host's configuration and no terminal prompt, fails the test
// when git fails, and returns its output with surrounding space trimmed.
func clientGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + t.TempDir(), "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0", "LC_ALL=C"}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// errorType is the error_type of a decoded error answer, "" for another.
func errorType(out map[string]any) any {
	e, _ := out["error"].(map[string]any)
	return e["error_type"]
}

// mustPost is post that fails the test unless the answer is 201.
func (w *world) mustPost(t *testing.T, body any) map[string]any {
	t.Helper()
	status, out := w.post(t, "demo", w.ownerKey, body)
	if status != http.StatusCreated {
		t.Fatalf("POST changes = %d %v, want 201", status, out)
	}
	w.quiet(t)
	return out
}

// open calls Open with in in a transaction of its own, which it commits
// when Open succeeds, and waits until the change's mergeability check has
// ended.
func (w *world) open(t *testing.T, in change.OpenInput) (change.Change, error) {
	t.Helper()
	var ch change.Change
	err := w.Store.Tx(context.Background(), func(tx *sql.Tx) error {
		var err error
		ch, err = w.changes.Open(context.Background(), tx, in)
		return err
	})
	w.quiet(t)
	return ch, err
}

// versions are the stored versions of the change of demo, as JSON objects.
func (w *world) versions(t *testing.T, workspace string, number int64) []map[string]any {
	t.Helper()
	vs, err := change.ReadVersions(context.Background(), w.Store.Main(), workspace, number)
	if err != nil {
		t.Fatal(err)
	}
	var out []map[string]any
	for _, v := range vs {
		out = append(out, asJSON(t, v))
	}
	return out
}

// seq is the seq of the last event in the log.
func (w *world) seq(t *testing.T) int64 {
	t.Helper()
	var seq int64
	if err := w.Store.Main().QueryRow(`SELECT last_seq FROM hub_event_seq`).Scan(&seq); err != nil {
		t.Fatal(err)
	}
	return seq
}

// eventsAfter are the events the log holds after seq, oldest first.
func (w *world) eventsAfter(t *testing.T, seq int64) []event.Record {
	t.Helper()
	recs, _, err := w.events.ReadAfter(context.Background(), seq, 1000)
	if err != nil {
		t.Fatal(err)
	}
	return recs
}

// rows counts the rows of each change table.
func (w *world) rows(t *testing.T) map[string]int {
	t.Helper()
	out := map[string]int{}
	for _, table := range []string{"changes", "change_versions", "change_sessions"} {
		var n int
		if err := w.Store.Main().QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&n); err != nil {
			t.Fatal(err)
		}
		out[table] = n
	}
	return out
}

// asJSON is v as a decoded JSON object.
func asJSON(t *testing.T, v any) map[string]any {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// metadata is the decoded metadata of the event.
func metadata(t *testing.T, rec event.Record) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(rec.Metadata, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// sessions is a session linker that accepts the sessions of accept and
// records every link it is asked to record.
type sessions struct {
	accept map[string]bool

	mu      sync.Mutex
	records []string
}

func (s *sessions) Check(_ context.Context, _ change.Caller, id, _ string) (string, error) {
	id = strings.ToLower(id)
	if !s.accept[id] {
		return "", fmt.Errorf("session %s: %w", id, change.ErrUnknownSession)
	}
	return id, nil
}

func (s *sessions) Record(_ context.Context, _ *sql.Tx, id, workspace string, number, version int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.records = append(s.records, fmt.Sprintf("%s %s %d/%d", id, workspace, number, version))
	return nil
}

// errRollback is what a test returns from a transaction to undo it.
var errRollback = errors.New("roll back")

// set sets the column of the workspace record slug to value.
func (w *world) set(t *testing.T, slug, column string, value any) {
	t.Helper()
	ctx := context.Background()
	if err := w.Store.Tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE workspaces SET `+column+` = ? WHERE slug = ?`, value, slug)
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

// member adds the user name as a member of the org acme, which reads demo,
// and returns their id and an API key of theirs.
func (w *world) member(t *testing.T, name string) (id, key string) {
	t.Helper()
	id = w.CreateUser(t, name)
	w.AddMember(t, w.org, id)
	return id, w.IssueKey(t, id)
}

// snapshot is what an open writes: the rows of the change tables, the
// version refs of demo's repository and the change events in the log.
type snapshot struct {
	rows   map[string]int
	refs   map[string]string
	events int
}

func (w *world) snapshot(t *testing.T) snapshot {
	t.Helper()
	events := 0
	for _, e := range w.eventsAfter(t, 0) {
		if e.ResourceType == "change" {
			events++
		}
	}
	return snapshot{w.rows(t), gitrepotest.Refs(t, w.demo, "refs/parley/changes/"), events}
}

// refused posts body to the workspace slug with the credential and fails
// the test unless the answer is status with errorType and the open wrote
// nothing; it returns the answer's error object.
func (w *world) refused(t *testing.T, slug, credential string, body any, status int, errorType string) map[string]any {
	t.Helper()
	before := w.snapshot(t)
	got, out := w.post(t, slug, credential, body)
	errObj, _ := out["error"].(map[string]any)
	if got != status || errObj["error_type"] != errorType {
		t.Errorf("POST %v = %d %v, want %d %s", body, got, out, status, errorType)
	}
	if after := w.snapshot(t); !reflect.DeepEqual(after, before) {
		t.Errorf("a refused open wrote %+v, had %+v", after, before)
	}
	return errObj
}

// patFor issues a PAT of the owner with the permissions and path globs
// (nil for none), and returns it with its token id.
func (w *world) patFor(t *testing.T, perms, paths []string) (token, id string) {
	t.Helper()
	return w.IssuePATID(t, identity.PATSpec{UserID: w.owner, Permissions: perms, Paths: paths, CreatedAt: time.Now().UTC()})
}

// ownerCaller is the owner using the API key k1.
func (w *world) ownerCaller() change.Caller {
	return change.Caller{UserID: w.owner, Credential: change.Credential{Type: change.CredentialAPIKey, ID: "k1"}}
}

// openBranch opens, through the component, a change of demo from its
// branch with an intent, by the owner, adjusted by edit (nil for none),
// and fails the test unless it opens.
func (w *world) openBranch(t *testing.T, branch string, edit func(*change.OpenInput)) change.Change {
	t.Helper()
	in := change.OpenInput{Workspace: "demo", SourceBranch: branch, Intent: new("why"), Caller: w.ownerCaller(), Kind: change.KindRef}
	if edit != nil {
		edit(&in)
	}
	ch, err := w.open(t, in)
	if err != nil {
		t.Fatalf("Open %s: %v", branch, err)
	}
	return ch
}

// record calls RecordVersion with in in a transaction of its own, which it
// commits when RecordVersion succeeds, and waits until the mergeability
// checks it scheduled have ended.
func (w *world) record(t *testing.T, in change.VersionInput) (*change.Version, error) {
	t.Helper()
	var v *change.Version
	err := w.Store.Tx(context.Background(), func(tx *sql.Tx) error {
		var err error
		v, err = w.changes.RecordVersion(context.Background(), tx, in)
		return err
	})
	w.quiet(t)
	return v, err
}

// mustRecord is record that fails the test unless a version is recorded.
func (w *world) mustRecord(t *testing.T, in change.VersionInput) *change.Version {
	t.Helper()
	v, err := w.record(t, in)
	if err != nil || v == nil {
		t.Fatalf("RecordVersion %+v = %v, %v; want a version", in, v, err)
	}
	return v
}

// read reads the change number of demo.
func (w *world) read(t *testing.T, number int64) change.Change {
	t.Helper()
	ch, err := w.changes.ReadChange(context.Background(), nil, "demo", number)
	if err != nil {
		t.Fatal(err)
	}
	return ch
}

// write runs fn in a transaction of its own and fails the test unless it
// commits.
func (w *world) write(t *testing.T, fn func(ctx context.Context, tx *sql.Tx) error) {
	t.Helper()
	ctx := context.Background()
	if err := w.Store.Tx(ctx, func(tx *sql.Tx) error { return fn(ctx, tx) }); err != nil {
		t.Fatal(err)
	}
}

// activity is a fork activity recorder that records the slugs it is told
// of, and returns an error for each while fail is set.
type activity struct {
	mu    sync.Mutex
	calls []string
	fail  bool
}

func (a *activity) RecordActivity(_ context.Context, _ *sql.Tx, slug string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.calls = append(a.calls, slug)
	if a.fail {
		return errors.New("the recorder failed")
	}
	return nil
}

func (a *activity) told() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.calls)
}

// changesOfDemo are the commits of the changes changesOfDemo opens in
// demo, by which they are looked up.
type changesOfDemo struct {
	// task1 and task2 are the commits of demo--m1's task: change 1 has
	// versions at task1, task2, then task1 again.
	task1, task2 string
	// b2 is the head of change 2's version 1, which change 4's version 1
	// has too; change 2's version 2 is at landed.
	b2 string
	// b3 is the head of change 3, which landed it as landed, a commit on
	// demo's main.
	b3, landed string
}

// changesOfDemo opens in demo: change 1, open, from demo--m1's task with
// three versions in session sessionOne; change 2, a draft from b2; change
// 3 from b3, landed with one landed-commit record; change 4, open, from
// b4, at the head of change 2's first version; then records version 2 of
// change 2 at the commit change 3 landed as.
func (w *world) changesOfDemo(t *testing.T) changesOfDemo {
	t.Helper()
	var f changesOfDemo
	m1 := w.addFork(t, "demo--m1", "main", nil, nil)
	f.task1 = commit(t, m1, "task", w.base, "Task\n\nWhy.\n", map[string]string{"t.txt": "t1\n"})
	w.openBranch(t, "task", func(in *change.OpenInput) { in.SourceWorkspace, in.SessionID = "demo--m1", new(sessionOne) })
	f.task2 = commit(t, m1, "task", f.task1, "More\n", map[string]string{"t.txt": "t2\n"})
	w.mustRecord(t, change.VersionInput{Workspace: "demo--m1", Branch: "task", Head: f.task2, Kind: change.KindPush,
		Caller: w.ownerCaller(), SessionID: new(sessionOne)})
	gitrepotest.Git(t, m1, "update-ref", "refs/heads/task", f.task1)
	w.mustRecord(t, change.VersionInput{Workspace: "demo--m1", Branch: "task", Head: f.task1, Kind: change.KindPush,
		Caller: w.ownerCaller(), SessionID: new(sessionOne)})
	f.b2 = w.branch(t, "b2", "B2\n\nWhy.\n")
	w.openBranch(t, "b2", func(in *change.OpenInput) { in.Draft = true })
	f.b3 = w.branch(t, "b3", "B3\n\nWhy.\n")
	w.openBranch(t, "b3", nil)
	f.landed = commit(t, w.demo, "main", w.base, "B3 (landed)\n\nWhy.\n", map[string]string{"b3.txt": "b3\n"})
	w.write(t, func(ctx context.Context, tx *sql.Tx) error {
		return w.changes.SetLanded(ctx, tx, "demo", 3, change.Landed{SHA: f.landed, At: time.Now(), LandID: "l1",
			By: change.Actor{ActorType: change.CredentialAPIKey, ActorID: &w.owner}, Commits: []change.LandedCommit{{VersionSHA: f.b3, LandedSHA: f.landed}}})
	})
	gitrepotest.Git(t, w.demo, "update-ref", "refs/heads/b4", f.b2)
	w.openBranch(t, "b4", nil)
	gitrepotest.Git(t, w.demo, "update-ref", "refs/heads/b2", f.landed)
	w.mustRecord(t, change.VersionInput{Workspace: "demo", Branch: "b2", Head: f.landed, Kind: change.KindPush, Caller: w.ownerCaller()})
	return f
}

// stacked opens in demo a change from each of the new branches, in order,
// each branch one commit adding <name>.txt on top of the branch before it
// (the first on base), and returns them as opened.
func (w *world) stacked(t *testing.T, names ...string) []change.Change {
	t.Helper()
	from := w.base
	var out []change.Change
	for _, n := range names {
		from = commit(t, w.demo, n, from, n+"\n", map[string]string{n + ".txt": n + "\n"})
		out = append(out, w.openBranch(t, n, nil))
	}
	return out
}

// versionEvent is the metadata of the last parley.change.version event of
// the change number of demo after seq; it fails the test when there is
// none.
func (w *world) versionEvent(t *testing.T, seq, number int64) map[string]any {
	t.Helper()
	var out map[string]any
	for _, e := range w.eventsAfter(t, seq) {
		if e.Type == change.EventVersion && e.ResourceID == strconv.FormatInt(number, 10) {
			out = metadata(t, e)
		}
	}
	if out == nil {
		t.Fatalf("no parley.change.version of demo#%d after %d", number, seq)
	}
	return out
}

// landedCommits counts the landed-commit records.
func (w *world) landedCommits(t *testing.T) int {
	t.Helper()
	var n int
	if err := w.Store.Main().QueryRow(`SELECT COUNT(*) FROM change_landed_commits`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// zeroDebounce is the configuration of a hub that does not space two
// mergeability checks of one change.
const zeroDebounce = "[changes]\nmergeable_debounce = \"0s\"\n"

// idle waits until no mergeability check of the change number of demo is
// waiting or running.
func (w *world) idle(t *testing.T, number int64) {
	t.Helper()
	for deadline := time.Now().Add(20 * time.Second); !change.MergeableIdle(w.changes, "demo", number); time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("the mergeability check of demo#%d did not end", number)
		}
	}
}

// settled waits until the background check has computed the mergeable of
// the change number of demo, so that it writes nothing more, and returns
// the change then.
func (w *world) settled(t *testing.T, number int64) change.Change {
	t.Helper()
	for deadline := time.Now().Add(20 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		w.idle(t, number)
		if ch := w.read(t, number); ch.Mergeable != change.MergeableUnknown && change.MergeableIdle(w.changes, "demo", number) {
			return ch
		}
		if time.Now().After(deadline) {
			t.Fatalf("the mergeable of demo#%d stayed unknown", number)
		}
	}
}

// quiet waits until the restack worker has nothing to run and no
// mergeability check is waiting or running, so that the workers write
// nothing until the next trigger; it does not wait for the restacks while
// holdRestacks holds them, or for the checks while hold keeps them from
// starting. The restacks are asked first, since they schedule checks.
func (w *world) quiet(t *testing.T) {
	t.Helper()
	for deadline := time.Now().Add(20 * time.Second); !change.RestacksQuiet(w.changes) || !(w.held || change.MergeableQuiet(w.changes)); time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the restacks and the mergeability checks did not end")
		}
	}
}

// hold keeps every mergeability check from starting until the test ends,
// or until the function it returns lets them start, so that mergeable
// stays unknown after a trigger and nothing else is written.
func (w *world) hold(t *testing.T) (release func()) {
	t.Helper()
	g := newGate()
	t.Cleanup(g.release)
	change.SetMergeableSeams(w.changes, change.MergeableSeams{Gate: g.pass})
	w.held = true
	return func() { g.release(); w.held = false }
}

// trigger schedules a mergeability check of the change number of demo.
func (w *world) trigger(t *testing.T, number int64) {
	t.Helper()
	w.write(t, func(ctx context.Context, tx *sql.Tx) error {
		return w.changes.ScheduleMergeable(ctx, tx, "demo", number)
	})
}

// holdRestacks keeps the restack worker from running any pending restack
// until the test ends, so that the test runs them with runPending.
func (w *world) holdRestacks(t *testing.T) {
	t.Helper()
	change.HoldRestacks(w.changes)
}

// runPending runs the pending restacks in the test's goroutine, those they
// record too, until none is left that has not failed, then waits for the
// mergeability checks they scheduled.
func (w *world) runPending(t *testing.T) {
	t.Helper()
	change.RestackPass(context.Background(), w.changes)
	w.quiet(t)
}

// pending are the numbers of the changes of demo with a pending restack,
// in the order the restacks were recorded.
func (w *world) pending(t *testing.T) []int64 {
	t.Helper()
	out, err := w.Store.Main().Query(`SELECT number FROM change_restacks WHERE workspace_slug = 'demo' ORDER BY seq`)
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	numbers := []int64{}
	for out.Next() {
		var n int64
		if err := out.Scan(&n); err != nil {
			t.Fatal(err)
		}
		numbers = append(numbers, n)
	}
	return numbers
}

// head is the commit the branch of demo is at.
func (w *world) head(t *testing.T, branch string) string {
	t.Helper()
	return gitrepotest.Refs(t, w.demo, "refs/heads/")[branch]
}

// amend replaces the tip commit of the branch of demo by one on the same
// parent that sets files, records it as the next version of the branch's
// change, pushed by the owner, and returns it.
func (w *world) amend(t *testing.T, branch string, files map[string]string) string {
	t.Helper()
	head := commit(t, w.demo, branch, gitrepotest.Git(t, w.demo, "rev-parse", branch+"^"), branch+" amended\n", files)
	w.mustRecord(t, change.VersionInput{Workspace: "demo", Branch: branch, Head: head, Kind: change.KindPush, Caller: w.ownerCaller()})
	return head
}

// land moves main of demo past a commit of its own to one that sets files,
// as the landing of the change number with those files would, marks the
// change landed as that commit through the store, and returns the commit.
func (w *world) land(t *testing.T, number int64, files map[string]string) string {
	t.Helper()
	moved := commit(t, w.demo, "main", w.head(t, "main"), "main moved\n", map[string]string{"main.txt": strconv.FormatInt(number, 10) + "\n"})
	landed := commit(t, w.demo, "main", moved, "landed\n", files)
	w.write(t, func(ctx context.Context, tx *sql.Tx) error {
		return w.changes.SetLanded(ctx, tx, "demo", number, change.Landed{SHA: landed, At: time.Now(), By: change.Actor{ActorType: change.ActorSystem}, LandID: "l" + strconv.FormatInt(number, 10)})
	})
	return landed
}

// subjects are the subjects of the commits of demo after base up to head,
// oldest first.
func (w *world) subjects(t *testing.T, base, head string) []string {
	t.Helper()
	out := gitrepotest.Git(t, w.demo, "log", "--reverse", "--format=%s", base+".."+head)
	if out == "" {
		return nil
	}
	return strings.Split(out, "\n")
}

// restackEvents are the parley.change.restack events after seq, by the
// number of their change.
func (w *world) restackEvents(t *testing.T, seq int64) (numbers []string, meta []map[string]any) {
	t.Helper()
	for _, e := range w.eventsAfter(t, seq) {
		if e.Type == change.EventRestack {
			numbers, meta = append(numbers, e.ResourceID), append(meta, metadata(t, e))
		}
	}
	return numbers, meta
}

// restack sends the manual restack of the change number of demo with the
// credential and the body (nil for none), and returns the status and the
// decoded answer.
func (w *world) restack(t *testing.T, number int64, credential string, body any) (int, map[string]any) {
	t.Helper()
	return w.send(t, http.MethodPost, "/workspaces/demo/changes/"+strconv.FormatInt(number, 10)+"/restack", credential, body)
}

// versionGuard is a version guard that records every call, with the head
// the change's source branch in dir was at then, and answers refuse.
type versionGuard struct {
	dir    string
	refuse error

	mu    sync.Mutex
	calls []versionCall
}

// versionCall is one call of a versionGuard.
type versionCall struct {
	caller               change.Caller
	base, head, branchAt string
}

func (g *versionGuard) CheckVersion(_ context.Context, caller change.Caller, ch change.Change, base, head string) error {
	at, _ := gitrepotest.Run(g.dir, "", "rev-parse", "refs/heads/"+ch.Source.Branch)
	g.mu.Lock()
	defer g.mu.Unlock()
	g.calls = append(g.calls, versionCall{caller, base, head, strings.TrimSpace(at)})
	return g.refuse
}

func (g *versionGuard) called() []versionCall {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.calls)
}

// guard registers a version guard of demo that answers refuse until the
// test ends.
func (w *world) guard(t *testing.T, refuse error) *versionGuard {
	t.Helper()
	g := &versionGuard{dir: w.demo, refuse: refuse}
	w.changes.RegisterVersionGuard(g)
	t.Cleanup(func() { w.changes.RegisterVersionGuard(nil) })
	return g
}

// changeEventsOf are the events of the change number of demo written after
// seq, of every type.
func (w *world) changeEventsOf(t *testing.T, seq, number int64) []event.Record {
	t.Helper()
	var out []event.Record
	for _, e := range w.eventsAfter(t, seq) {
		if e.ResourceType == "change" && e.ResourceID == strconv.FormatInt(number, 10) {
			out = append(out, e)
		}
	}
	return out
}
