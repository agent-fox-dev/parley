package land_test

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
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
	"uuid"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/change"
	"github.com/agent-fox-dev/parley/internal/event"
	"github.com/agent-fox-dev/parley/internal/fork"
	"github.com/agent-fox-dev/parley/internal/gitrepo"
	"github.com/agent-fox-dev/parley/internal/gitrepo/gitrepotest"
	"github.com/agent-fox-dev/parley/internal/hub"
	"github.com/agent-fox-dev/parley/internal/hubtest"
	"github.com/agent-fox-dev/parley/internal/land"
	"github.com/agent-fox-dev/parley/internal/store"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

// world is a hub with the landing composed: the user owner, a member of
// the org acme, with the API key ownerKey; the owner's ready workspace
// api, whose main holds one commit (base) with a.txt ("l1\nl2\nl3\n") and
// whose changes are never restacked automatically, so that nothing but
// the test moves a change's branch. Its mergeability checks are not
// spaced.
type world struct {
	*hubtest.Hub
	land       *land.Component
	changes    *change.Component
	forks      *fork.Component
	events     *event.Component
	workspaces *workspace.Service
	// sessions is the session linker registered on the land component.
	sessions *sessionStub

	owner, ownerKey, org string
	// api is the git directory of api's repository, and base the commit
	// its main is at.
	api, base string
}

// newWorld starts the world, each of adjust changing the land component's
// dependencies before they are bound.
func newWorld(t *testing.T, adjust ...func(*land.Deps)) *world {
	t.Helper()
	w := &world{sessions: &sessionStub{known: map[string]session{}}}
	w.Hub = hubtest.StartWith(t, hubtest.Config{
		TOML: "[changes]\nmergeable_debounce = \"0s\"\n",
		Compose: func(c *hub.Composition) {
			w.land, w.changes, w.forks, w.events, w.workspaces = c.Land(), c.Changes(), c.Forks(), c.Events(), c.Workspaces()
			w.land.RegisterSessionLinker(w.sessions)
			for _, fn := range adjust {
				c.AdjustLand(fn)
			}
		},
	})
	w.owner = w.CreateUser(t, "owner")
	w.ownerKey = w.IssueKey(t, w.owner)
	w.org = w.CreateOrg(t, "acme")
	w.AddMember(t, w.org, w.owner)
	w.api = w.addWorkspace(t, "api")
	w.base = gitrepotest.Refs(t, w.api, "refs/heads/")["main"]
	return w
}

// addWorkspace adds the owner's ready workspace slug of the org acme,
// whose main holds one commit with a.txt, with changes.auto_restack off,
// and returns its git directory.
func (w *world) addWorkspace(t *testing.T, slug string) string {
	t.Helper()
	ctx, now, dir := context.Background(), time.Now().UTC(), w.workspaces.RepoPath(slug)
	gitrepotest.Git(t, dir, "init", "--quiet", "--bare", "--initial-branch=main", dir)
	commit(t, dir, "main", "", "base", map[string]string{"a.txt": "l1\nl2\nl3\n"})
	head := gitrepotest.Refs(t, dir, "refs/heads/")["main"]
	rec := workspace.Workspace{
		Slug: slug, Kind: access.KindRepository, Mode: workspace.ModeStandard,
		GitURL: new("https://origin.test/" + slug + ".git"), DefaultBranch: new("main"), OwnerID: w.owner, OrgID: w.org,
		Status: workspace.StatusReady, DisplayName: slug, HeadSHA: &head,
		Settings:  map[string]json.RawMessage{"changes.auto_restack": json.RawMessage("false")},
		CreatedAt: now, UpdatedAt: now, InstanceID: workspace.NewInstanceID(),
	}
	if err := w.Store.Tx(ctx, func(tx *sql.Tx) error { return workspace.Insert(ctx, tx, rec) }); err != nil {
		t.Fatal(err)
	}
	return dir
}

// member adds the user name as a member of the org acme, which reads the
// world's workspaces, and returns their id and an API key of theirs.
func (w *world) member(t *testing.T, name string) (id, key string) {
	t.Helper()
	id = w.CreateUser(t, name)
	w.AddMember(t, w.org, id)
	return id, w.IssueKey(t, id)
}

const committer = "committer T <t@example.invalid> 1700000000 +0000\n"

// data is a fast-import data command for s.
func data(s string) string { return "data " + strconv.Itoa(len(s)) + "\n" + s + "\n" }

// commit writes to the repository at dir a commit with message on top of
// from (a commit id or a branch), or a root commit for from "", setting
// each file of files to its content, and points branch at it; it returns
// the commit's id. Its author is its committer.
func commit(t *testing.T, dir, branch, from, message string, files map[string]string) string {
	t.Helper()
	return commitAs(t, dir, branch, from, "", message, files)
}

// commitAs is commit with author, "Name <email> <unix time> <zone>", as
// the commit's author; "" makes the committer the author.
func commitAs(t *testing.T, dir, branch, from, author, message string, files map[string]string) string {
	t.Helper()
	var s strings.Builder
	s.WriteString("commit refs/heads/" + branch + "\n")
	if author != "" {
		s.WriteString("author " + author + "\n")
	}
	s.WriteString(committer + data(message))
	if from != "" {
		s.WriteString("from " + from + "\n")
	}
	for _, path := range slices.Sorted(maps.Keys(files)) {
		s.WriteString("M 100644 inline " + path + "\n" + data(files[path]))
	}
	if _, err := gitrepotest.Run(dir, s.String()+"\n", "fast-import", "--quiet", "--force"); err != nil {
		t.Fatal(err)
	}
	return gitrepotest.Refs(t, dir, "refs/heads/")[branch]
}

// open opens, through the change component, a change of api by the owner
// from a new branch of base that adds the file <branch>.txt, and returns
// it.
func (w *world) open(t *testing.T, branch string) change.Change {
	t.Helper()
	commit(t, w.api, branch, w.base, branch, map[string]string{branch + ".txt": branch + "\n"})
	return w.openBranch(t, branch, w.caller())
}

// caller is the owner with their API key k1, as a change and a land name
// them.
func (w *world) caller() change.Caller {
	return change.Caller{UserID: w.owner, Username: "owner", Credential: change.Credential{Type: change.CredentialAPIKey, ID: "k1"}}
}

// openBranch opens, through the change component, a change of api by the
// caller from its branch, whose commits the test made, and returns it.
func (w *world) openBranch(t *testing.T, branch string, by change.Caller) change.Change {
	t.Helper()
	in := change.OpenInput{Workspace: "api", SourceBranch: branch, Intent: new("why"), Kind: change.KindRef, Caller: by}
	var ch change.Change
	ctx := context.Background()
	if err := w.Store.Tx(ctx, func(tx *sql.Tx) (err error) {
		ch, err = w.changes.Open(ctx, tx, in)
		return err
	}); err != nil {
		t.Fatalf("open %s: %v", branch, err)
	}
	return ch
}

// get sends GET path under /api/v1 with the credential and returns the
// status and the decoded answer.
func (w *world) get(t *testing.T, path, credential string) (int, map[string]any) {
	t.Helper()
	status, raw := w.Call(t, http.MethodGet, "/api/v1"+path, credential, nil)
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("GET %s answered %d %q: %v", path, status, raw, err)
	}
	return status, out
}

// attemptID is the n-th of a series of attempt ids, which sort as n does.
func attemptID(n int) string { return fmt.Sprintf("00000000-0000-4000-8000-%012d", n) }

// running is a running manual rebase attempt of the workspace on branch,
// by the owner, created at created and covering the changes numbers at
// version 1.
func running(id, workspace, branch string, created time.Time, numbers ...int64) land.Attempt {
	a := land.Attempt{
		ID: id, Workspace: workspace, TargetBranch: branch, Status: land.StatusRunning, Trigger: land.TriggerManual,
		Strategy: land.StrategyRebase, Attempts: 1, RequestedBy: change.Actor{ActorType: change.CredentialAPIKey},
		CredentialID: "k1", CreatedAt: created,
	}
	for _, n := range numbers {
		a.Covered = append(a.Covered, land.CoveredChange{Number: n, Version: 1})
	}
	return a
}

// finished is a with the final status, finished a minute after it was
// created, with the result members that status has.
func finished(a land.Attempt, status string) land.Attempt {
	a.Status, a.FinishedAt = status, new(a.CreatedAt.Add(time.Minute))
	switch status {
	case land.StatusLanded:
		a.OntoSHA, a.LandedSHA = new(strings.Repeat("a", 40)), new(strings.Repeat("b", 40))
		for _, n := range a.Numbers() {
			a.Commits = append(a.Commits, land.Commit{Change: n, VersionSHA: strings.Repeat("c", 40), LandedSHA: strings.Repeat("b", 40)})
		}
		a.PolicyRevision = new(int64(0))
	case land.StatusFailed:
		a.Error = new(land.ErrorInternal)
	}
	return a
}

// insert stores the attempts in the world's store.
func (w *world) insert(t *testing.T, attempts ...land.Attempt) {
	t.Helper()
	insertInto(t, w.Store, attempts...)
}

// insertInto stores the attempts in s, each as it is: one that is final
// is created running and then finished.
func insertInto(t *testing.T, s *store.Store, attempts ...land.Attempt) {
	t.Helper()
	ctx := context.Background()
	for _, a := range attempts {
		created := a
		created.Status, created.FinishedAt = land.StatusRunning, nil
		if err := land.InsertAttempt(ctx, s, created); err != nil {
			t.Fatalf("insert attempt %s: %v", a.ID, err)
		}
		if a.Status != land.StatusRunning {
			if err := land.FinishAttempt(ctx, s, a); err != nil {
				t.Fatalf("finish attempt %s: %v", a.ID, err)
			}
		}
	}
}

// openStore opens a store with nothing but the land attempts' schema.
func openStore(t *testing.T) *store.Store {
	t.Helper()
	dir := t.TempDir()
	s, err := store.Open(context.Background(), store.Options{
		DatabasePath: filepath.Join(dir, "parley.db"), WorkspaceRoot: filepath.Join(dir, "workspaces"),
	}, []store.Step{land.SchemaStep})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// asJSON is v encoded and decoded as a JSON object.
func asJSON(t *testing.T, v any) map[string]any {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	return out
}

// landNow lands the change ch of api at its version on its target, by the
// caller by with the session (nil for none): it stores a running manual
// rebase attempt covering ch, runs it to its end and returns it as it is
// stored then.
func (w *world) landNow(t *testing.T, ch change.Change, by change.Caller, session *string) land.Attempt {
	t.Helper()
	a, err := w.runLand(t, ch, by, session)
	if err != nil {
		t.Fatalf("run attempt of %s: %v", ch.Ref, err)
	}
	return a
}

// runLand is landNow returning the engine's error in place of failing the
// test on it, with the attempt as it is stored after the run.
func (w *world) runLand(t *testing.T, ch change.Change, by change.Caller, session *string) (land.Attempt, error) {
	t.Helper()
	return w.run(t, attemptOf(ch, by, session, time.Now().UTC()))
}

// attemptOf is a running manual rebase attempt landing the change ch at
// its version on its target, by the caller by in the session (nil for
// none), created at created.
func attemptOf(ch change.Change, by change.Caller, session *string, created time.Time) land.Attempt {
	a := land.Attempt{
		ID: uuid.New().String(), Workspace: ch.WorkspaceSlug, TargetBranch: ch.TargetBranch, Status: land.StatusRunning,
		Trigger: land.TriggerManual, Strategy: land.StrategyRebase,
		RequestedBy:  change.Actor{ActorType: by.Credential.Type, ActorID: new(by.UserID), Username: new(by.Username)},
		CredentialID: by.Credential.ID, SessionID: session, CreatedAt: created,
		Covered: []land.CoveredChange{{Number: ch.Number, Version: ch.Version, Head: ch.HeadSHA,
			SourceWorkspace: ch.Source.WorkspaceSlug, SourceBranch: ch.Source.Branch}},
	}
	if ch.BaseSHA != nil {
		a.Covered[0].Base = *ch.BaseSHA
	}
	return a
}

// submit stores an attempt of the owner landing the change ch, created at
// created, and hands it to the land pool, which counts its deadline from
// then; it returns the attempt as it was created.
func (w *world) submit(t *testing.T, ch change.Change, created time.Time) land.Attempt {
	t.Helper()
	a := attemptOf(ch, w.caller(), nil, created)
	if err := land.InsertAttempt(context.Background(), w.Store, a); err != nil {
		t.Fatal(err)
	}
	land.Submit(w.land, a)
	return a
}

// ended waits until the pool is done with the attempt id, at most 30 s,
// and returns the attempt as it is stored then.
func (w *world) ended(t *testing.T, id string) land.Attempt {
	t.Helper()
	select {
	case <-land.Ended(w.land, id):
	case <-time.After(30 * time.Second):
		t.Fatalf("land attempt %s did not end within 30 s", id)
	}
	return w.attempt(t, id)
}

// attempt is the attempt id of api as it is stored.
func (w *world) attempt(t *testing.T, id string) land.Attempt {
	t.Helper()
	a, found, err := land.ReadAttempt(context.Background(), w.Store, "api", id)
	if err != nil || !found {
		t.Fatalf("read attempt %s: %v, %v", id, found, err)
	}
	return a
}

// readChange is the change number of api as it is stored.
func (w *world) readChange(t *testing.T, number int64) change.Change {
	t.Helper()
	ch, err := w.changes.ReadChange(context.Background(), nil, "api", number)
	if err != nil {
		t.Fatalf("read change %d: %v", number, err)
	}
	return ch
}

// eventsOf are the hub events of the type typ, oldest first.
func (w *world) eventsOf(t *testing.T, typ string) []event.Record {
	t.Helper()
	recs, _, err := w.events.ReadAfter(context.Background(), 0, 1000)
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

// metadata is the decoded metadata of the event rec.
func metadata(t *testing.T, rec event.Record) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(rec.Metadata, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// head is the commit the branch of the repository at dir points at, ""
// for none.
func head(t *testing.T, dir, branch string) string {
	t.Helper()
	return gitrepotest.Refs(t, dir, "refs/heads/")[branch]
}

// landPushes are the parley.git.push events of api's main written for a
// land, oldest first.
func (w *world) landPushes(t *testing.T) []event.Record {
	t.Helper()
	var out []event.Record
	for _, e := range w.eventsOf(t, "parley.git.push") {
		if m := metadata(t, e); m["ref"] == "refs/heads/main" && m["via"] == "land" {
			out = append(out, e)
		}
	}
	return out
}

// fork adds a ready task fork slug of api, owned by the user owner and
// made from base, and returns its git directory.
func (w *world) fork(t *testing.T, slug, owner string) string {
	t.Helper()
	ctx, now, dir := context.Background(), time.Now().UTC(), w.workspaces.RepoPath(slug)
	f := fork.Fork{Workspace: workspace.Workspace{
		Slug: slug, Kind: access.KindFork, Mode: workspace.ModeStandard, DefaultBranch: new("main"),
		OwnerID: owner, OrgID: w.org, Status: workspace.StatusReady, DisplayName: slug,
		ParentSlug: new("api"), ForkPoint: &workspace.ForkPoint{Ref: "main", SHA: w.base},
		ExpiresAt: new(now.Add(7 * 24 * time.Hour)), AutoChange: new(false),
		CreatedAt: now, UpdatedAt: now, InstanceID: workspace.NewInstanceID(),
	}}
	if err := (gitrepo.Git{}).CreateFork(ctx, w.api, dir); err != nil {
		t.Fatal(err)
	}
	if err := w.Store.Tx(ctx, func(tx *sql.Tx) error { return fork.Insert(ctx, tx, f) }); err != nil {
		t.Fatal(err)
	}
	return dir
}

// openFrom opens, through the change component, a change of api by the
// caller by from a new branch of base in the fork at dir, slug, that adds
// the file <branch>.txt, and returns it.
func (w *world) openFrom(t *testing.T, slug, dir, branch string, by change.Caller) change.Change {
	t.Helper()
	commit(t, dir, branch, w.base, branch, map[string]string{branch + ".txt": branch + "\n"})
	in := change.OpenInput{Workspace: "api", SourceWorkspace: slug, SourceBranch: branch, Intent: new("why"),
		Kind: change.KindRef, Caller: by}
	var ch change.Change
	ctx := context.Background()
	if err := w.Store.Tx(ctx, func(tx *sql.Tx) (err error) {
		ch, err = w.changes.Open(ctx, tx, in)
		return err
	}); err != nil {
		t.Fatalf("open %s from %s: %v", branch, slug, err)
	}
	return ch
}

// configure sets the settings of api to the JSON object settings.
func (w *world) configure(t *testing.T, settings string) {
	t.Helper()
	ctx := context.Background()
	if err := w.Store.Tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE workspaces SET settings = ? WHERE slug = 'api'`, settings)
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

// post sends the land request of the change number of the workspace slug,
// with the query ("" for none), the body (nil for none) and the
// credential, and returns the status and the raw answer.
func (w *world) post(t *testing.T, credential, slug string, number any, query string, body any) (int, []byte) {
	t.Helper()
	path := fmt.Sprintf("/api/v1/workspaces/%s/changes/%v/land", slug, number)
	if query != "" {
		path += "?" + query
	}
	return w.Call(t, http.MethodPost, path, credential, body)
}

// landCall is post on api, with the answer decoded.
func (w *world) landCall(t *testing.T, credential string, number any, query string, body any) (int, map[string]any) {
	t.Helper()
	status, raw := w.post(t, credential, "api", number, query, body)
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("POST land of %v answered %d %q: %v", number, status, raw, err)
	}
	return status, out
}

// errorType is the error_type of an error answer, "" for any other.
func errorType(answer map[string]any) string {
	errObj, _ := answer["error"].(map[string]any)
	s, _ := errObj["error_type"].(string)
	return s
}

// attempts counts the land attempts in the store.
func (w *world) attempts(t *testing.T) (n int) {
	t.Helper()
	if err := w.Store.Main().QueryRowContext(context.Background(), `SELECT COUNT(*) FROM land_attempts`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// session is a session as the session stub knows it: the user who
// created it and the workspace it runs in.
type session struct{ user, workspace string }

// sessionStub is the session linker of the world: it accepts a session it
// knows that the caller created, or any session for the admin token,
// whose workspace is the target, a task fork of api or api for a fork,
// and keeps every link recorded as "<session> <workspace> <land id>".
type sessionStub struct {
	mu    sync.Mutex
	known map[string]session
	links []string
}

func (s *sessionStub) add(id, user, workspace string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.known[id] = session{user, workspace}
}

func (s *sessionStub) Check(_ context.Context, caller change.Caller, id, workspace string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.known[id]
	related := func(a, b string) bool { return a == b || strings.HasPrefix(a, b+"--") || strings.HasPrefix(b, a+"--") }
	if !ok || rec.user != caller.UserID && caller.Credential.Type != change.CredentialAdminToken || !related(rec.workspace, workspace) {
		return "", change.ErrUnknownSession
	}
	return id, nil
}

func (s *sessionStub) Record(_ context.Context, _ *sql.Tx, id, workspace, landID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.links = append(s.links, id+" "+workspace+" "+landID)
	return nil
}

func (s *sessionStub) recorded() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.links)
}

// clock is a clock the test moves: After answers once the test has moved
// it past the duration asked for, and tells asked of every duration.
type clock struct {
	mu     sync.Mutex
	now    time.Duration
	timers []timer
	asked  chan time.Duration
}

type timer struct {
	at time.Duration
	ch chan time.Time
}

func newClock() *clock { return &clock{asked: make(chan time.Duration, 16)} }

func (c *clock) After(d time.Duration) <-chan time.Time {
	c.mu.Lock()
	ch := make(chan time.Time, 1)
	c.timers = append(c.timers, timer{c.now + d, ch})
	c.mu.Unlock()
	c.asked <- d
	return ch
}

// advance moves the clock on by d and fires every timer due.
func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now += d
	c.timers = slices.DeleteFunc(c.timers, func(tm timer) bool {
		if tm.at > c.now {
			return false
		}
		tm.ch <- time.Now()
		return true
	})
}

// waitAsked waits until the clock is asked for a duration, at most 30 s,
// and returns it.
func (c *clock) waitAsked(t *testing.T) time.Duration {
	t.Helper()
	select {
	case d := <-c.asked:
		return d
	case <-time.After(30 * time.Second):
		t.Fatal("nobody waited on the clock within 30 s")
		return 0
	}
}

// inTx runs fn in a store transaction, failing the test on its error.
func (w *world) inTx(t *testing.T, fn func(ctx context.Context, tx *sql.Tx) error) {
	t.Helper()
	ctx := context.Background()
	if err := w.Store.Tx(ctx, func(tx *sql.Tx) error { return fn(ctx, tx) }); err != nil {
		t.Fatal(err)
	}
}

// openDraft is open with the change opened as a draft.
func (w *world) openDraft(t *testing.T, branch string) change.Change {
	t.Helper()
	commit(t, w.api, branch, w.base, branch, map[string]string{branch + ".txt": branch + "\n"})
	var ch change.Change
	w.inTx(t, func(ctx context.Context, tx *sql.Tx) (err error) {
		ch, err = w.changes.Open(ctx, tx, change.OpenInput{Workspace: "api", SourceBranch: branch, Kind: change.KindRef,
			Caller: w.caller(), Draft: true})
		return err
	})
	return ch
}

// nextVersion adds a commit to the source branch of the change ch of api,
// records it as the change's next version and returns the change as it
// is stored then.
func (w *world) nextVersion(t *testing.T, ch change.Change) change.Change {
	t.Helper()
	branch := ch.Source.Branch
	head := commit(t, w.api, branch, ch.HeadSHA, "next", map[string]string{branch + ".txt": "next\n"})
	w.inTx(t, func(ctx context.Context, tx *sql.Tx) error {
		_, err := w.changes.RecordVersion(ctx, tx, change.VersionInput{Workspace: "api", Branch: branch, Head: head, Kind: change.KindRef})
		return err
	})
	return w.readChange(t, ch.Number)
}

// opened opens, through the change component, the change in describes,
// and returns it.
func (w *world) opened(t *testing.T, in change.OpenInput) change.Change {
	t.Helper()
	var ch change.Change
	w.inTx(t, func(ctx context.Context, tx *sql.Tx) (err error) {
		ch, err = w.changes.Open(ctx, tx, in)
		return err
	})
	return ch
}

// stack opens a stack of changes of api by the owner, one per entry of
// files, bottom first, and returns them in that order: change i is the
// branch <prefix><i> with one commit on the head of the branch below it
// (on base for the first) that sets the files of its entry, or
// <branch>.txt for nil, and the stack linker links it to the one below.
// The bottom one is a draft for draft.
func (w *world) stack(t *testing.T, prefix string, draft bool, files ...map[string]string) []change.Change {
	t.Helper()
	var out []change.Change
	from := w.base
	for i, f := range files {
		branch := prefix + strconv.Itoa(i+1)
		if f == nil {
			f = map[string]string{branch + ".txt": branch + "\n"}
		}
		from = commit(t, w.api, branch, from, branch, f)
		ch := w.opened(t, change.OpenInput{Workspace: "api", SourceBranch: branch, Intent: new("why"), Kind: change.KindRef,
			Caller: w.caller(), Draft: draft && i == 0})
		if i > 0 && (ch.DependsOn == nil || *ch.DependsOn != out[i-1].Number) {
			t.Fatalf("%s depends on %v, want %d", ch.Ref, ch.DependsOn, out[i-1].Number)
		}
		out = append(out, ch)
	}
	return out
}

// numbers are the numbers of the changes.
func numbers(changes []change.Change) []any {
	out := make([]any, len(changes))
	for i, ch := range changes {
		out[i] = float64(ch.Number)
	}
	return out
}

// anySession is a session linker of the change component that accepts
// every session.
type anySession struct{}

func (anySession) Check(_ context.Context, _ change.Caller, id, _ string) (string, error) {
	return strings.ToLower(id), nil
}

func (anySession) Record(context.Context, *sql.Tx, string, string, int64, int64) error { return nil }

// run stores the running attempt a and runs it to its end in the
// caller's goroutine, returning the engine's error and the attempt as it
// is stored after the run.
func (w *world) run(t *testing.T, a land.Attempt) (land.Attempt, error) {
	t.Helper()
	ctx := context.Background()
	if err := land.InsertAttempt(ctx, w.Store, a); err != nil {
		t.Fatal(err)
	}
	runErr := land.RunAttempt(ctx, w.land, a.Workspace, a.ID)
	return w.attempt(t, a.ID), runErr
}

// mergeable waits until the change number of api reads the mergeable
// want, at most 30 s.
func (w *world) mergeable(t *testing.T, number int64, want string) {
	t.Helper()
	for deadline := time.Now().Add(30 * time.Second); w.readChange(t, number).Mergeable != want; {
		if time.Now().After(deadline) {
			t.Fatalf("change %d does not read mergeable %s within 30 s", number, want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// landEvents are the events of the type typ written for the land attempt
// id, oldest first.
func (w *world) landEvents(t *testing.T, typ, id string) []event.Record {
	t.Helper()
	var out []event.Record
	for _, e := range w.eventsOf(t, typ) {
		if metadata(t, e)["land_id"] == id {
			out = append(out, e)
		}
	}
	return out
}

// stubGate is a policy gate that gives each change the answer set for
// its number, ready by revision 0 for any other, and keeps every call.
type stubGate struct {
	mu      sync.Mutex
	answers map[int64]land.GateAnswer
	calls   []land.GateInput
}

func (g *stubGate) answer(number int64, a land.GateAnswer) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.answers == nil {
		g.answers = map[int64]land.GateAnswer{}
	}
	g.answers[number] = a
}

func (g *stubGate) Evaluate(_ context.Context, in land.GateInput) (land.GateAnswer, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.calls = append(g.calls, in)
	if a, ok := g.answers[in.Change.Number]; ok {
		return a, nil
	}
	return land.GateAnswer{Status: change.LandingReady}, nil
}

func (g *stubGate) asked() []land.GateInput {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.calls)
}

// points runs what a test sets at each point of the atomic step of the
// world's land component, and before each swap as "before-swap", in the
// pool's goroutine; the errors it returns are kept for the test, not given
// to the engine.
type points struct {
	mu     sync.Mutex
	run    func(point string) error
	failed []error
}

// atPoints sets the seams of the world's land component to the points it
// returns.
func (w *world) atPoints() *points {
	p := &points{}
	land.SetSeams(w.land, land.Seams{
		BeforeSwap: func(context.Context, string, int) { p.at("before-swap") },
		Fault: func(point string) error {
			if point != "before-swap" {
				p.at(point)
			}
			return nil
		},
	})
	return p
}

func (p *points) at(point string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.run == nil {
		return
	}
	if err := p.run(point); err != nil {
		p.failed = append(p.failed, err)
	}
}

// set makes run (nil for nothing) what runs at each point until the test
// ends, and returns what reports the errors it returned.
func (p *points) set(t *testing.T, run func(point string) error) func() error {
	p.mu.Lock()
	p.run, p.failed = run, nil
	p.mu.Unlock()
	t.Cleanup(func() {
		p.mu.Lock()
		defer p.mu.Unlock()
		p.run = nil
	})
	return func() error {
		p.mu.Lock()
		defer p.mu.Unlock()
		return errors.Join(p.failed...)
	}
}

// hangingMerges is a Deps.Repos that opens the repository of a workspace
// at path(slug) so that every content merge runs the merge driver hang,
// which writes its process id to pidFile and sleeps for a minute.
func hangingMerges(t *testing.T, path func(slug string) string) (repos func(context.Context, string) (*gitrepo.Repo, error), pidFile string) {
	t.Helper()
	dir := t.TempDir()
	pidFile, attributes := filepath.Join(dir, "pid"), filepath.Join(dir, "attributes")
	if err := os.WriteFile(attributes, []byte("* merge=hang\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	git := gitrepo.Git{Env: []string{"GIT_CONFIG_COUNT=2",
		"GIT_CONFIG_KEY_0=merge.hang.driver", "GIT_CONFIG_VALUE_0=echo $$ > " + pidFile + "; exec sleep 60",
		"GIT_CONFIG_KEY_1=core.attributesFile", "GIT_CONFIG_VALUE_1=" + attributes}}
	return func(ctx context.Context, slug string) (*gitrepo.Repo, error) { return git.Open(ctx, path(slug)) }, pidFile
}

// waitGone waits until the process whose id pidFile holds is gone, at
// most 10 s.
func waitGone(t *testing.T, pidFile string) {
	t.Helper()
	raw, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatalf("the process never wrote its id: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(10 * time.Second); !errors.Is(syscall.Kill(pid, 0), syscall.ESRCH); {
		if time.Now().After(deadline) {
			t.Fatalf("the process %d outlived its deadline by 10 s", pid)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
