package change_test

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/change"
	"github.com/agent-fox-dev/parley/internal/event"
	"github.com/agent-fox-dev/parley/internal/gitrepo/gitrepotest"
	"github.com/agent-fox-dev/parley/internal/hubtest"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

func TestSourceDeleted_AbandonsTheChangeOfTheBranchAndKeepsItsVersions(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	rec := &activity{}
	w.changes.RegisterForkActivity(rec)
	feat1 := w.branch(t, "feat", "Feat\n\nWhy.\n")
	w.openBranch(t, "feat", nil)
	feat2 := commit(t, w.demo, "feat", feat1, "More\n", map[string]string{"feat.txt": "feat 2\n"})
	w.mustRecord(t, change.VersionInput{Workspace: "demo", Branch: "feat", Head: feat2, Kind: change.KindPush, Caller: w.ownerCaller()})
	sketch := w.branch(t, "sketch", "Sketch\n")
	w.openBranch(t, "sketch", func(in *change.OpenInput) { in.Draft = true })
	t1 := w.addFork(t, "demo--t1", "main", nil, nil)
	work := commit(t, t1, "work", w.base, "Work\n\nWhy.\n", map[string]string{"w.txt": "w\n"})
	w.openBranch(t, "work", func(in *change.OpenInput) { in.SourceWorkspace = "demo--t1" })
	told := len(rec.told())

	// deleted deletes the branch of the git directory dir, tells the
	// component, and returns the events it wrote.
	deleted := func(t *testing.T, dir, workspace, branch string) []event.Record {
		t.Helper()
		gitrepotest.Git(t, dir, "update-ref", "-d", "refs/heads/"+branch)
		seq := w.seq(t)
		w.write(t, func(ctx context.Context, tx *sql.Tx) error {
			return w.changes.SourceDeleted(ctx, tx, workspace, branch, w.ownerCaller())
		})
		return w.eventsAfter(t, seq)
	}
	for _, c := range []struct {
		name, branch string
		number       int64
		heads        []string
	}{
		{"an open change", "feat", 1, []string{feat1, feat2}},
		{"a draft", "sketch", 2, []string{sketch}},
	} {
		t.Run(c.name, func(t *testing.T) {
			events := deleted(t, w.demo, "demo", c.branch)
			ch := w.read(t, c.number)
			if ch.State != change.StateAbandoned || ch.Version != int64(len(c.heads)) {
				t.Errorf("the change reads %s at version %d, want abandoned at %d", ch.State, ch.Version, len(c.heads))
			}
			if got := abandonReason(t, w, c.number); got != "source_deleted" {
				t.Errorf("abandon reason = %q, want source_deleted", got)
			}
			if got := w.announced(t, c.number); got != "" {
				t.Errorf("the change is still announced landable at %s", got)
			}
			if got := w.versions(t, "demo", c.number); len(got) != len(c.heads) {
				t.Errorf("versions = %v, want %d", got, len(c.heads))
			}
			refs := gitrepotest.Refs(t, w.demo, "refs/parley/changes/")
			for i, head := range c.heads {
				if name := strings.TrimPrefix(change.VersionRef(c.number, int64(i+1)), "refs/parley/changes/"); refs[name] != head {
					t.Errorf("version ref %s = %q, want %s", name, refs[name], head)
				}
			}
			if len(events) != 1 {
				t.Fatalf("events = %+v, want one parley.change.abandon", events)
			}
			e := events[0]
			want := map[string]any{"number": float64(c.number), "reason": "source_deleted", "ref": change.Ref("demo", c.number),
				"version": float64(len(c.heads)), "head_sha": c.heads[len(c.heads)-1]}
			if e.Type != change.EventAbandon || e.ResourceType != "change" || e.ResourceID != strconv.FormatInt(c.number, 10) ||
				*e.Workspace != "demo" || e.ActorType != change.CredentialAPIKey || e.ActorID == nil || *e.ActorID != w.owner ||
				!reflect.DeepEqual(metadata(t, e), want) {
				t.Errorf("event = %+v %s, want %s of change %d by the owner with %v", e, e.Metadata, change.EventAbandon, c.number, want)
			}
		})
	}
	t.Run("a branch without a draft or open change", func(t *testing.T) {
		before := w.read(t, 1)
		for _, branch := range []string{"nothing", "feat"} {
			seq := w.seq(t)
			err := w.Store.Tx(context.Background(), func(tx *sql.Tx) error {
				return w.changes.SourceDeleted(context.Background(), tx, "demo", branch, w.ownerCaller())
			})
			if events := w.eventsAfter(t, seq); err != nil || len(events) != 0 {
				t.Errorf("SourceDeleted(%s) = %v and wrote %+v; want nothing", branch, err, events)
			}
		}
		if after := w.read(t, 1); !reflect.DeepEqual(after, before) {
			t.Errorf("the abandoned change reads %+v, had %+v", after, before)
		}
	})
	t.Run("a fork's branch", func(t *testing.T) {
		events := deleted(t, t1, "demo--t1", "work")
		if ch := w.read(t, 3); ch.State != change.StateAbandoned || len(events) != 1 {
			t.Errorf("the fork's change reads %s with events %+v, want abandoned with one event", ch.State, events)
		}
		if got := gitrepotest.Refs(t, w.demo, "refs/parley/changes/")["3/1"]; got != work {
			t.Errorf("version ref 3/1 = %q, want %s", got, work)
		}
		if got := rec.told()[told:]; !reflect.DeepEqual(got, []string{"demo--t1"}) {
			t.Errorf("the fork activity recorder was told %v, want [demo--t1]", got)
		}
	})
	t.Run("the branch exists again", func(t *testing.T) {
		again := commit(t, w.demo, "feat", w.base, "Feat again\n\nWhy.\n", map[string]string{"feat.txt": "again\n"})
		v, err := w.record(t, change.VersionInput{Workspace: "demo", Branch: "feat", Head: again, Kind: change.KindPush, Caller: w.ownerCaller()})
		if err != nil || v != nil {
			t.Errorf("RecordVersion on the abandoned change's branch = %+v, %v; want no version", v, err)
		}
		out := w.mustPost(t, map[string]any{"source": map[string]any{"branch": "feat"}})
		if out["number"] != float64(4) || out["head_sha"] != again {
			t.Errorf("the new open = %v, want number 4 at %s", out, again)
		}
		if ch := w.read(t, 1); ch.State != change.StateAbandoned || ch.Version != 2 || ch.HeadSHA != feat2 {
			t.Errorf("the abandoned change reads %s at version %d %s, want abandoned at 2 %s", ch.State, ch.Version, ch.HeadSHA, feat2)
		}
	})
}

func TestForkDeleted_AbandonsTheForksDraftAndOpenChanges(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	ctx := context.Background()
	rec := &activity{}
	w.changes.RegisterForkActivity(rec)
	f1 := w.addFork(t, "demo--f1", "main", nil, nil)
	f2 := w.addFork(t, "demo--f2", "main", nil, nil)
	from := func(fork string, draft bool) func(*change.OpenInput) {
		return func(in *change.OpenInput) { in.SourceWorkspace, in.Draft = fork, draft }
	}
	for _, b := range []string{"a", "b", "c", "d"} {
		commit(t, f1, b, w.base, b+"\n\nWhy.\n", map[string]string{b + ".txt": b + "\n"})
	}
	commit(t, f2, "e", w.base, "e\n\nWhy.\n", map[string]string{"e.txt": "e\n"})
	w.branch(t, "own", "Own\n\nWhy.\n")
	w.openBranch(t, "a", from("demo--f1", false))
	w.openBranch(t, "b", from("demo--f1", true))
	w.openBranch(t, "c", from("demo--f1", false))
	w.openBranch(t, "e", from("demo--f2", false))
	w.openBranch(t, "own", nil)
	w.openBranch(t, "d", from("demo--f1", false))
	w.write(t, func(ctx context.Context, tx *sql.Tx) error {
		if err := w.changes.SetLanded(ctx, tx, "demo", 3, change.Landed{SHA: w.base, At: time.Now(), By: change.Actor{ActorType: change.ActorSystem}}); err != nil {
			return err
		}
		return w.changes.SetAbandoned(ctx, tx, "demo", 6, "abandoned")
	})
	unchanged := map[int64]change.Change{}
	for _, n := range []int64{3, 4, 5, 6} {
		unchanged[n] = w.read(t, n)
	}
	refs, told, seq := gitrepotest.Refs(t, w.demo, "refs/parley/changes/"), len(rec.told()), w.seq(t)

	// The fork's record goes in the transaction of its delete, which may
	// come before the change component is told.
	w.write(t, func(ctx context.Context, tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM workspaces WHERE slug = 'demo--f1'`); err != nil {
			return err
		}
		return w.changes.ForkDeleted(ctx, tx, "demo--f1", change.Caller{})
	})
	for n, want := range map[int64]string{1: change.StateAbandoned, 2: change.StateAbandoned, 3: change.StateLanded,
		4: change.StateOpen, 5: change.StateOpen, 6: change.StateAbandoned} {
		if got := w.read(t, n).State; got != want {
			t.Errorf("change %d is %s, want %s", n, got, want)
		}
	}
	for n, before := range unchanged {
		if after := w.read(t, n); !reflect.DeepEqual(after, before) {
			t.Errorf("change %d reads %+v, had %+v", n, after, before)
		}
	}
	events := w.eventsAfter(t, seq)
	var ids []string
	for _, e := range events {
		if e.Type != change.EventAbandon || *e.Workspace != "demo" || e.ActorType != change.ActorSystem || e.ActorID != nil ||
			metadata(t, e)["reason"] != "source_workspace_deleted" {
			t.Errorf("event %+v %s, want %s by the hub for source_workspace_deleted", e, e.Metadata, change.EventAbandon)
		}
		ids = append(ids, e.ResourceID)
	}
	if !reflect.DeepEqual(ids, []string{"1", "2"}) {
		t.Errorf("abandon events of changes %v, want [1 2]", ids)
	}
	for _, n := range []int64{1, 2} {
		if got := abandonReason(t, w, n); got != "source_workspace_deleted" {
			t.Errorf("abandon reason of %d = %q, want source_workspace_deleted", n, got)
		}
	}
	if after := gitrepotest.Refs(t, w.demo, "refs/parley/changes/"); !reflect.DeepEqual(after, refs) || len(after) != 6 {
		t.Errorf("version refs of demo = %v, had %v", after, refs)
	}
	if got := rec.told()[told:]; len(got) != 0 {
		t.Errorf("the fork activity recorder was told %v, want nothing", got)
	}

	t.Run("a move of the fork's branch afterwards", func(t *testing.T) {
		head := commit(t, f1, "a", w.base, "a again\n\nWhy.\n", map[string]string{"a.txt": "again\n"})
		v, err := w.record(t, change.VersionInput{Workspace: "demo--f1", Branch: "a", Head: head, Kind: change.KindPush, Caller: w.ownerCaller()})
		if err != nil || v != nil {
			t.Errorf("RecordVersion on the abandoned change's branch = %+v, %v; want no version", v, err)
		}
	})
	t.Run("a repository's slug or a slug of nothing", func(t *testing.T) {
		before, seq := w.read(t, 5), w.seq(t)
		for _, slug := range []string{"demo", "nosuch"} {
			if err := w.Store.Tx(ctx, func(tx *sql.Tx) error { return w.changes.ForkDeleted(ctx, tx, slug, change.Caller{}) }); err != nil {
				t.Errorf("ForkDeleted(%s) = %v", slug, err)
			}
		}
		if after := w.read(t, 5); !reflect.DeepEqual(after, before) {
			t.Errorf("demo's own change reads %+v, had %+v", after, before)
		}
		if events := w.eventsAfter(t, seq); len(events) != 0 {
			t.Errorf("events = %+v, want none", events)
		}
	})
}

func TestWorkspaceDeleted_DeletesTheWorkspacesChangesAndLeavesOthers(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	ctx := context.Background()
	other := w.addWorkspace(t, "other")
	m1 := w.addFork(t, "demo--m1", "main", nil, nil)
	withSession := func(in *change.OpenInput) { in.SessionID = new(sessionOne) }
	head := w.branch(t, "feat", "Feat\n\nWhy.\n")
	w.openBranch(t, "feat", withSession)
	landed := w.branch(t, "done", "Done\n\nWhy.\n")
	w.openBranch(t, "done", nil)
	w.branch(t, "dropped", "Dropped\n\nWhy.\n")
	w.openBranch(t, "dropped", nil)
	commit(t, m1, "task", w.base, "Task\n\nWhy.\n", map[string]string{"t.txt": "t\n"})
	w.openBranch(t, "task", func(in *change.OpenInput) { in.SourceWorkspace = "demo--m1" })
	w.write(t, func(ctx context.Context, tx *sql.Tx) error {
		if err := w.changes.SetLanded(ctx, tx, "demo", 2, change.Landed{SHA: landed, At: time.Now(), By: change.Actor{ActorType: change.ActorSystem},
			Commits: []change.LandedCommit{{VersionSHA: landed, LandedSHA: landed}}}); err != nil {
			return err
		}
		return w.changes.SetAbandoned(ctx, tx, "demo", 3, "abandoned")
	})
	commit(t, other, "feat", "refs/heads/main", "Other\n\nWhy.\n", map[string]string{"o.txt": "o\n"})
	theirs, err := w.open(t, change.OpenInput{Workspace: "other", SourceBranch: "feat", Intent: new("why"), Caller: w.ownerCaller(),
		Kind: change.KindRef, SessionID: new(sessionOne)})
	if err != nil {
		t.Fatal(err)
	}
	theirs, _ = w.changes.ReadChange(ctx, nil, "other", theirs.Number)
	deleted := func(t *testing.T, slug string) {
		t.Helper()
		w.write(t, func(ctx context.Context, tx *sql.Tx) error { return w.changes.WorkspaceDeleted(ctx, tx, slug) })
	}

	t.Run("a fork's slug or a slug of nothing", func(t *testing.T) {
		before := rowsOf(t, w, "demo")
		deleted(t, "demo--m1")
		deleted(t, "nosuch")
		if after := rowsOf(t, w, "demo"); !reflect.DeepEqual(after, before) {
			t.Errorf("demo's rows = %v, had %v", after, before)
		}
	})
	t.Run("the workspace", func(t *testing.T) {
		deleted(t, "demo")
		if got := rowsOf(t, w, "demo"); !reflect.DeepEqual(got, map[string]int{"changes": 0, "change_versions": 0,
			"change_sessions": 0, "change_landed_commits": 0, "change_counters": 0}) {
			t.Errorf("demo's rows = %v, want none", got)
		}
		for _, sha := range []string{head, landed} {
			if got, found, err := w.changes.ChangeOfCommit(ctx, nil, "demo", sha); err != nil || found {
				t.Errorf("ChangeOfCommit(demo, %s) = %+v, %v, %v; want none", sha, got, found, err)
			}
		}
		if got, err := w.changes.ChangesOfSession(ctx, nil, sessionOne); err != nil ||
			!reflect.DeepEqual(got, []change.Key{{WorkspaceSlug: "other", Number: 1}}) {
			t.Errorf("ChangesOfSession = %+v, %v; want only other#1", got, err)
		}
		if _, err := w.changes.ReadChange(ctx, nil, "demo", 1); !errors.Is(err, change.ErrChangeNotFound) {
			t.Errorf("ReadChange(demo#1) = %v, want the change-not-found error", err)
		}
		if after, err := w.changes.ReadChange(ctx, nil, "other", 1); err != nil || !reflect.DeepEqual(after, theirs) {
			t.Errorf("other#1 reads %+v, %v; had %+v", after, err, theirs)
		}
	})
	t.Run("a workspace of the same slug numbers from 1", func(t *testing.T) {
		status, out := w.post(t, "demo", w.ownerKey, map[string]any{"source": map[string]any{"branch": "feat"}})
		if status != http.StatusCreated || out["number"] != float64(1) {
			t.Errorf("POST changes = %d %v, want 201 with number 1", status, out)
		}
	})
}

func TestSourceLost_AbandonsTheChangesWhoseBranchTheRecloneDidNotBringBack(t *testing.T) {
	t.Parallel()
	o := gitrepotest.SmartOrigin(t)
	w := newWorldWith(t, hubtest.Config{Env: map[string]string{"GIT_SSL_CAINFO": o.CAFile()}})
	w.branch(t, "kept", "Kept\n\nWhy.\n")
	w.branch(t, "lost", "Lost\n\nWhy.\n")
	w.branch(t, "sketch", "Sketch\n\nWhy.\n")
	gitrepotest.Git(t, w.demo, "push", "--quiet", o.Dir(), "main", "release", "kept")
	kept := w.openBranch(t, "kept", nil).Number
	lost := map[string]int64{
		"an open change": w.openBranch(t, "lost", nil).Number,
		"a draft":        w.openBranch(t, "sketch", func(in *change.OpenInput) { in.Draft = true }).Number,
	}
	w.write(t, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE workspaces SET git_url = ? WHERE slug = 'demo'`, o.URL())
		return err
	})
	// Origin takes none of the branches the reclone offers it first, so
	// that lost and sketch do not come back.
	o.Reject("refused")
	seq := w.seq(t)

	if status, out := w.send(t, http.MethodPost, "/workspaces/demo/reclone", w.ownerKey, nil); status != http.StatusOK {
		t.Fatalf("reclone = %d %v", status, out)
	}
	for deadline := time.Now().Add(20 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		rec, _, err := workspace.BySlug(context.Background(), w.Store.Main(), "demo")
		if err != nil {
			t.Fatal(err)
		}
		if rec.Status == workspace.StatusReady {
			break
		}
		if rec.Status == workspace.StatusFailed || time.Now().After(deadline) {
			t.Fatalf("the clone after the reclone left demo %s (%v)", rec.Status, deref(rec.CloneError))
		}
	}
	for name, n := range lost {
		if ch := w.read(t, n); ch.State != change.StateAbandoned {
			t.Errorf("%s reads %s once the workspace is ready, want abandoned", name, ch.State)
		}
		if got := abandonReason(t, w, n); got != change.ReasonSourceLost {
			t.Errorf("%s was abandoned for %q, want source_lost", name, got)
		}
		events := w.changeEvents(t, seq, change.EventAbandon, n)
		if len(events) != 1 || metadata(t, events[0])["reason"] != change.ReasonSourceLost {
			t.Errorf("%s: abandon events %v, want one with reason source_lost", name, events)
		}
		wantSystemEvents(t, events)
	}
	if ch := w.read(t, kept); ch.State != change.StateOpen {
		t.Errorf("the change whose branch came back reads %s, want open", ch.State)
	}
}

// abandonReason is the stored reason the change number of demo was
// abandoned for.
func abandonReason(t *testing.T, w *world, number int64) string {
	t.Helper()
	var reason sql.NullString
	if err := w.Store.Main().QueryRow(`SELECT abandon_reason FROM changes WHERE workspace_slug = 'demo' AND number = ?`, number).Scan(&reason); err != nil {
		t.Fatal(err)
	}
	return reason.String
}

// rowsOf counts the rows of each change table that belong to the workspace.
func rowsOf(t *testing.T, w *world, workspace string) map[string]int {
	t.Helper()
	out := map[string]int{}
	for _, table := range []string{"changes", "change_versions", "change_sessions", "change_landed_commits", "change_counters"} {
		var n int
		if err := w.Store.Main().QueryRow(`SELECT COUNT(*) FROM `+table+` WHERE workspace_slug = ?`, workspace).Scan(&n); err != nil {
			t.Fatal(err)
		}
		out[table] = n
	}
	return out
}
