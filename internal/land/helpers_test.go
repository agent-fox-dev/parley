package land_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/change"
	"github.com/agent-fox-dev/parley/internal/event"
	"github.com/agent-fox-dev/parley/internal/fork"
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

	owner, ownerKey, org string
	// api is the git directory of api's repository, and base the commit
	// its main is at.
	api, base string
}

// newWorld starts the world, each of adjust changing the land component's
// dependencies before they are bound.
func newWorld(t *testing.T, adjust ...func(*land.Deps)) *world {
	t.Helper()
	w := &world{}
	w.Hub = hubtest.StartWith(t, hubtest.Config{
		TOML: "[changes]\nmergeable_debounce = \"0s\"\n",
		Compose: func(c *hub.Composition) {
			w.land, w.changes, w.forks, w.events, w.workspaces = c.Land(), c.Changes(), c.Forks(), c.Events(), c.Workspaces()
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
	ctx := context.Background()
	a := attemptOf(ch, by, session, time.Now().UTC())
	if err := land.InsertAttempt(ctx, w.Store, a); err != nil {
		t.Fatal(err)
	}
	runErr := land.RunAttempt(ctx, w.land, a.Workspace, a.ID)
	return w.attempt(t, a.ID), runErr
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
