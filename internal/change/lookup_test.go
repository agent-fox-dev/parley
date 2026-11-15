package change_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/change"
	"github.com/agent-fox-dev/parley/internal/gitrepo"
)

func TestLookups_AnswerFromTheChangeRecord(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	f := w.changesOfDemo(t)
	ctx := context.Background()

	t.Run("ChangeOfCommit", func(t *testing.T) {
		for _, c := range []struct {
			name, workspace, sha string
			found                bool
			want                 change.CommitChange
		}{
			{"the highest version with the head", "demo", f.task1, true, change.CommitChange{WorkspaceSlug: "demo", Number: 1, Version: new(int64(3))}},
			{"an earlier version's head", "demo", f.task2, true, change.CommitChange{WorkspaceSlug: "demo", Number: 1, Version: new(int64(2))}},
			{"the lowest number of two changes with the head", "demo", f.b2, true, change.CommitChange{WorkspaceSlug: "demo", Number: 2, Version: new(int64(1))}},
			{"a landed commit before a version head of a lower number", "demo", f.landed, true, change.CommitChange{WorkspaceSlug: "demo", Number: 3, Version: new(int64(1)), Landed: true}},
			{"the head a change landed", "demo", f.b3, true, change.CommitChange{WorkspaceSlug: "demo", Number: 3, Version: new(int64(1))}},
			{"a commit of no change", "demo", w.base, false, change.CommitChange{}},
			{"a fork's commit asked of the fork", "demo--m1", f.task2, true, change.CommitChange{WorkspaceSlug: "demo", Number: 1, Version: new(int64(2))}},
			{"a commit of a change whose source is not the fork", "demo--m1", f.b2, false, change.CommitChange{}},
			{"another workspace", "other", f.task1, false, change.CommitChange{}},
		} {
			got, found, err := w.changes.ChangeOfCommit(ctx, nil, c.workspace, c.sha)
			if err != nil || found != c.found || !reflect.DeepEqual(got, c.want) {
				t.Errorf("%s: ChangeOfCommit(%s, %s) = %+v, %v, %v; want %+v, %v", c.name, c.workspace, c.sha, got, found, err, c.want, c.found)
			}
		}
	})
	t.Run("ChangeOfCommit of a landed commit no version had as head", func(t *testing.T) {
		landed := strings.Repeat("e", 40)
		err := w.Store.Tx(ctx, func(tx *sql.Tx) error {
			if err := w.changes.SetLanded(ctx, tx, "demo", 4, change.Landed{SHA: landed, At: time.Now(), LandID: "l2",
				By: change.Actor{ActorType: change.ActorSystem}, Commits: []change.LandedCommit{{VersionSHA: w.base, LandedSHA: landed}}}); err != nil {
				return err
			}
			got, found, err := w.changes.ChangeOfCommit(ctx, tx, "demo", landed)
			if want := (change.CommitChange{WorkspaceSlug: "demo", Number: 4, Landed: true}); err != nil || !found || !reflect.DeepEqual(got, want) {
				t.Errorf("ChangeOfCommit = %+v, %v, %v; want %+v without a version", got, found, err, want)
			}
			return errRollback
		})
		if !errors.Is(err, errRollback) {
			t.Fatal(err)
		}
	})
	t.Run("OpenChangeOfBranch", func(t *testing.T) {
		for _, c := range []struct {
			name, workspace, branch string
			found                   bool
			want                    change.BranchChange
		}{
			{"a fork's branch, under the parent", "demo--m1", "task", true, change.BranchChange{WorkspaceSlug: "demo", Number: 1, Version: 3, State: change.StateOpen}},
			{"a draft", "demo", "b2", true, change.BranchChange{WorkspaceSlug: "demo", Number: 2, Version: 2, State: change.StateDraft}},
			{"the source of a landed change", "demo", "b3", false, change.BranchChange{}},
			{"a branch of no change", "demo", "main", false, change.BranchChange{}},
			{"the right name in the wrong workspace", "demo", "task", false, change.BranchChange{}},
		} {
			got, found, err := w.changes.OpenChangeOfBranch(ctx, nil, c.workspace, c.branch)
			if err != nil || found != c.found || got != c.want {
				t.Errorf("%s: OpenChangeOfBranch(%s, %s) = %+v, %v, %v; want %+v, %v", c.name, c.workspace, c.branch, got, found, err, c.want, c.found)
			}
		}
	})
	t.Run("ReadChange", func(t *testing.T) {
		ch, err := w.changes.ReadChange(ctx, nil, "demo", 1)
		if err != nil {
			t.Fatal(err)
		}
		got, want := asJSON(t, ch), map[string]any{
			"state": "open", "target_branch": "main", "source": map[string]any{"workspace_slug": "demo--m1", "branch": "task"},
			"version": 3.0, "head_sha": f.task1, "base_sha": w.base, "depends_on": nil, "session_id": sessionOne,
			"sessions": []any{sessionOne}, "mergeable": "unknown", "auto_land": false, "landed": nil, "reverts": nil,
			"reverted_by": nil, "landing": map[string]any{"status": "ready", "reason": nil, "requirements": []any{}},
		}
		for name, v := range want {
			if !reflect.DeepEqual(got[name], v) {
				t.Errorf("%s = %v, want %v", name, got[name], v)
			}
		}
		landed, err := w.changes.ReadChange(ctx, nil, "demo", 3)
		if err != nil || landed.State != change.StateLanded || landed.Landed == nil ||
			!reflect.DeepEqual(landed.Landed.Commits, []change.LandedCommit{{VersionSHA: f.b3, LandedSHA: f.landed}}) {
			t.Errorf("ReadChange(demo, 3) = %+v, %v; want it landed with its landed commit", landed, err)
		}
		for _, k := range []struct {
			workspace string
			number    int64
		}{{"demo", 99}, {"other", 1}} {
			if _, err := w.changes.ReadChange(ctx, nil, k.workspace, k.number); !errors.Is(err, change.ErrChangeNotFound) {
				t.Errorf("ReadChange(%s, %d) = %v, want the change-not-found error", k.workspace, k.number, err)
			}
		}
	})
	t.Run("ReadChange in the caller's transaction", func(t *testing.T) {
		err := w.Store.Tx(ctx, func(tx *sql.Tx) error {
			if err := w.changes.SetLanding(ctx, tx, "demo", 1, change.Landing{Status: change.LandingPending}); err != nil {
				return err
			}
			if in, err := w.changes.ReadChange(ctx, tx, "demo", 1); err != nil || in.Landing.Status != change.LandingPending {
				t.Errorf("ReadChange with the transaction = landing %+v, %v; want pending", in.Landing, err)
			}
			if out, err := w.changes.ReadChange(ctx, nil, "demo", 1); err != nil || out.Landing.Status != change.LandingReady {
				t.Errorf("ReadChange without it = landing %+v, %v; want ready", out.Landing, err)
			}
			return errRollback
		})
		if !errors.Is(err, errRollback) {
			t.Fatal(err)
		}
	})
	t.Run("ReadChange gives the object the routes answer", func(t *testing.T) {
		w.branch(t, "b5", "B5\n\nWhy.\n")
		posted := w.mustPost(t, map[string]any{"source": map[string]any{"branch": "b5"}})
		ch, err := w.changes.ReadChange(ctx, nil, "demo", int64(posted["number"].(float64)))
		if err != nil {
			t.Fatal(err)
		}
		if got := asJSON(t, ch); !reflect.DeepEqual(got, posted) {
			t.Errorf("ReadChange = %v, want the answer %v", got, posted)
		}
	})
	t.Run("OpenCounts", func(t *testing.T) {
		got, err := w.changes.OpenCounts(ctx)
		if want := map[string]int{"demo": 4}; err != nil || !reflect.DeepEqual(got, want) {
			t.Errorf("OpenCounts = %v, %v; want %v", got, err, want)
		}
	})
}

func TestChangesOfSession_ListsEveryChangeWithAVersionOfTheSession(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	other := w.addWorkspace(t, "other")
	ctx := context.Background()
	head := w.branch(t, "feat", "Feat\n\nWhy.\n")
	w.openBranch(t, "feat", nil)
	for i, c := range []struct {
		session  *string
		current  any
		sessions []any
	}{
		{new(sessionOne), sessionOne, []any{sessionOne}},
		{new(sessionTwo), sessionTwo, []any{sessionOne, sessionTwo}},
		{nil, nil, []any{sessionOne, sessionTwo}},
		{new(sessionOne), sessionOne, []any{sessionOne, sessionTwo}},
	} {
		head = commit(t, w.demo, "feat", head, "More\n", map[string]string{"feat.txt": strings.Repeat("x\n", i+1)})
		w.mustRecord(t, change.VersionInput{Workspace: "demo", Branch: "feat", Head: head, Kind: change.KindPush, Caller: w.ownerCaller(), SessionID: c.session})
		got := asJSON(t, w.read(t, 1))
		if got["session_id"] != c.current || !reflect.DeepEqual(got["sessions"], c.sessions) {
			t.Errorf("after version %d: session_id = %v and sessions = %v, want %v and %v", i+2, got["session_id"], got["sessions"], c.current, c.sessions)
		}
	}
	commit(t, other, "o", "refs/heads/main", "O\n\nWhy.\n", map[string]string{"o.txt": "o\n"})
	w.openBranch(t, "o", func(in *change.OpenInput) { in.Workspace, in.SessionID = "other", new(sessionTwo) })

	for _, c := range []struct {
		session string
		want    []change.Key
	}{
		{sessionTwo, []change.Key{{WorkspaceSlug: "demo", Number: 1}, {WorkspaceSlug: "other", Number: 1}}},
		{sessionOne, []change.Key{{WorkspaceSlug: "demo", Number: 1}}},
		{sessionOther, []change.Key{}},
	} {
		if got, err := w.changes.ChangesOfSession(ctx, nil, c.session); err != nil || !reflect.DeepEqual(got, c.want) {
			t.Errorf("ChangesOfSession(%s) = %v, %v; want %v", c.session, got, err, c.want)
		}
	}
}

func TestSetters_StoreWhatOtherComponentsCompute(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	ctx := context.Background()
	b1 := w.branch(t, "b1", "B1\n\nWhy.\n")
	b2 := commit(t, w.demo, "b2", b1, "B2\n\nWhy.\n", map[string]string{"b2.txt": "b2\n"})
	w.branch(t, "b3", "B3\n\nWhy.\n")
	for _, b := range []string{"b1", "b2", "b3"} {
		w.openBranch(t, b, nil)
	}
	// set runs the setter in a transaction of its own and returns the
	// change number as it reads then, failing unless updated_at moved.
	set := func(t *testing.T, number int64, setter func(ctx context.Context, tx *sql.Tx) error) change.Change {
		t.Helper()
		before := time.Now().UTC()
		w.write(t, setter)
		ch := w.read(t, number)
		if ch.UpdatedAt.Before(before) {
			t.Errorf("updated_at = %v, want at least %v", ch.UpdatedAt, before)
		}
		return ch
	}

	t.Run("depends_on and dependants", func(t *testing.T) {
		for _, n := range []int64{3, 2} {
			ch := set(t, n, func(ctx context.Context, tx *sql.Tx) error {
				return w.changes.SetDependsOn(ctx, tx, "demo", n, new(int64(1)))
			})
			if ch.DependsOn == nil || *ch.DependsOn != 1 {
				t.Errorf("depends_on of %d = %v, want 1", n, ch.DependsOn)
			}
		}
		if got := w.read(t, 1).Dependants; !reflect.DeepEqual(got, []int64{2, 3}) {
			t.Errorf("dependants = %v, want [2 3]", got)
		}
		if ch := set(t, 3, func(ctx context.Context, tx *sql.Tx) error {
			return w.changes.SetAbandoned(ctx, tx, "demo", 3, "superseded")
		}); ch.State != change.StateAbandoned {
			t.Errorf("state = %s, want abandoned", ch.State)
		}
		if got := w.read(t, 1).Dependants; !reflect.DeepEqual(got, []int64{2}) {
			t.Errorf("dependants after 3 is abandoned = %v, want [2]", got)
		}
	})
	t.Run("the base of the current version", func(t *testing.T) {
		repo, err := gitrepo.Git{}.Open(ctx, w.demo)
		if err != nil {
			t.Fatal(err)
		}
		patchID, err := repo.PatchID(ctx, b1, b2)
		if err != nil {
			t.Fatal(err)
		}
		ch := set(t, 2, func(ctx context.Context, tx *sql.Tx) error { return w.changes.SetBase(ctx, tx, "demo", 2, b1) })
		stats := change.Stats{Commits: 1, FilesChanged: 1, Insertions: 1}
		if ch.BaseSHA != b1 || ch.Stats != stats || !reflect.DeepEqual(ch.Paths, []string{"b2.txt"}) {
			t.Errorf("base_sha, stats, paths = %s, %+v, %v; want %s, %+v, [b2.txt]", ch.BaseSHA, ch.Stats, ch.Paths, b1, stats)
		}
		vs, err := change.ReadVersions(ctx, w.Store.Main(), "demo", 2)
		if err != nil || len(vs) != 1 || vs[0].BaseSHA != b1 || vs[0].Stats != stats || vs[0].PatchID != patchID {
			t.Errorf("versions = %+v, %v; want version 1 based on %s with %+v and the patch id %s", vs, err, b1, stats, patchID)
		}
	})
	t.Run("mergeable and its conflict report", func(t *testing.T) {
		report := json.RawMessage(`{"files":["b2.txt"]}`)
		ch := set(t, 2, func(ctx context.Context, tx *sql.Tx) error {
			return w.changes.SetMergeable(ctx, tx, "demo", 2, change.MergeableConflict, report)
		})
		if ch.Mergeable != change.MergeableConflict || string(ch.Conflict) != string(report) {
			t.Errorf("mergeable, conflict = %s, %s; want conflict, %s", ch.Mergeable, ch.Conflict, report)
		}
		ch = set(t, 2, func(ctx context.Context, tx *sql.Tx) error {
			return w.changes.SetMergeable(ctx, tx, "demo", 2, change.MergeableClean, report)
		})
		if ch.Mergeable != change.MergeableClean || ch.Conflict != nil {
			t.Errorf("mergeable, conflict = %s, %s; want clean and none", ch.Mergeable, ch.Conflict)
		}
	})
	t.Run("needs_restack and its reason", func(t *testing.T) {
		ch := set(t, 2, func(ctx context.Context, tx *sql.Tx) error {
			return w.changes.SetNeedsRestack(ctx, tx, "demo", 2, true, new("conflict"))
		})
		if !ch.NeedsRestack || ch.RestackReason == nil || *ch.RestackReason != "conflict" {
			t.Errorf("needs_restack, restack_reason = %v, %v; want true, conflict", ch.NeedsRestack, ch.RestackReason)
		}
	})
	t.Run("landing", func(t *testing.T) {
		l := change.Landing{Status: change.LandingPending, Requirements: []json.RawMessage{json.RawMessage(`{"name":"x"}`)}}
		ch := set(t, 2, func(ctx context.Context, tx *sql.Tx) error { return w.changes.SetLanding(ctx, tx, "demo", 2, l) })
		if got := asJSON(t, ch)["landing"]; !reflect.DeepEqual(got, asJSON(t, l)) {
			t.Errorf("landing = %v, want %v", got, asJSON(t, l))
		}
	})
	t.Run("reverts and reverted_by", func(t *testing.T) {
		if ch := set(t, 2, func(ctx context.Context, tx *sql.Tx) error {
			return w.changes.SetReverts(ctx, tx, "demo", 2, new(int64(1)))
		}); ch.Reverts == nil || *ch.Reverts != 1 {
			t.Errorf("reverts = %v, want 1", ch.Reverts)
		}
		if ch := set(t, 1, func(ctx context.Context, tx *sql.Tx) error {
			return w.changes.SetRevertedBy(ctx, tx, "demo", 1, new(int64(2)))
		}); ch.RevertedBy == nil || *ch.RevertedBy != 2 {
			t.Errorf("reverted_by = %v, want 2", ch.RevertedBy)
		}
	})
	t.Run("landed", func(t *testing.T) {
		landed := commit(t, w.demo, "main", w.base, "B1 (landed)\n\nWhy.\n", map[string]string{"b1.txt": "b1\n"})
		l := change.Landed{SHA: landed, At: time.Now().UTC().Truncate(time.Second), LandID: "l1",
			By: change.Actor{ActorType: change.ActorSystem}, Commits: []change.LandedCommit{{VersionSHA: b1, LandedSHA: landed}}}
		ch := set(t, 1, func(ctx context.Context, tx *sql.Tx) error { return w.changes.SetLanded(ctx, tx, "demo", 1, l) })
		if ch.State != change.StateLanded || ch.Landed == nil || !reflect.DeepEqual(asJSON(t, ch.Landed), asJSON(t, l)) {
			t.Errorf("state, landed = %s, %+v; want landed, %+v", ch.State, ch.Landed, l)
		}
		if got, found, err := w.changes.ChangeOfCommit(ctx, nil, "demo", landed); err != nil || !found || got.Number != 1 || !got.Landed {
			t.Errorf("ChangeOfCommit(the landed commit) = %+v, %v, %v; want change 1, landed", got, found, err)
		}
	})
	t.Run("an unknown change", func(t *testing.T) {
		before := w.landedCommits(t)
		for name, setter := range map[string]func(ctx context.Context, tx *sql.Tx) error{
			"SetDependsOn": func(ctx context.Context, tx *sql.Tx) error {
				return w.changes.SetDependsOn(ctx, tx, "demo", 99, new(int64(1)))
			},
			"SetBase": func(ctx context.Context, tx *sql.Tx) error { return w.changes.SetBase(ctx, tx, "demo", 99, b1) },
			"SetMergeable": func(ctx context.Context, tx *sql.Tx) error {
				return w.changes.SetMergeable(ctx, tx, "demo", 99, change.MergeableClean, nil)
			},
			"SetNeedsRestack": func(ctx context.Context, tx *sql.Tx) error {
				return w.changes.SetNeedsRestack(ctx, tx, "demo", 99, false, nil)
			},
			"SetLanding": func(ctx context.Context, tx *sql.Tx) error {
				return w.changes.SetLanding(ctx, tx, "demo", 99, change.Landing{Status: "ready"})
			},
			"SetLanded": func(ctx context.Context, tx *sql.Tx) error {
				return w.changes.SetLanded(ctx, tx, "demo", 99, change.Landed{SHA: b1, Commits: []change.LandedCommit{{VersionSHA: b1, LandedSHA: b1}}})
			},
			"SetReverts":    func(ctx context.Context, tx *sql.Tx) error { return w.changes.SetReverts(ctx, tx, "demo", 99, nil) },
			"SetRevertedBy": func(ctx context.Context, tx *sql.Tx) error { return w.changes.SetRevertedBy(ctx, tx, "demo", 99, nil) },
			"SetAbandoned": func(ctx context.Context, tx *sql.Tx) error {
				return w.changes.SetAbandoned(ctx, tx, "demo", 99, "gone")
			},
			"SetAutoLand": func(ctx context.Context, tx *sql.Tx) error {
				return w.changes.SetAutoLand(ctx, tx, "demo", 99, true, change.Caller{})
			},
		} {
			if err := w.Store.Tx(ctx, func(tx *sql.Tx) error { return setter(ctx, tx) }); !errors.Is(err, change.ErrChangeNotFound) {
				t.Errorf("%s of change 99 = %v, want the change-not-found error", name, err)
			}
		}
		if after := w.landedCommits(t); after != before {
			t.Errorf("%d landed-commit records, had %d", after, before)
		}
	})
	t.Run("a transaction that rolls back", func(t *testing.T) {
		before := w.read(t, 2)
		err := w.Store.Tx(ctx, func(tx *sql.Tx) error {
			if err := w.changes.SetNeedsRestack(ctx, tx, "demo", 2, false, nil); err != nil {
				return err
			}
			if err := w.changes.SetMergeable(ctx, tx, "demo", 2, change.MergeableConflict, json.RawMessage(`{}`)); err != nil {
				return err
			}
			return errRollback
		})
		if !errors.Is(err, errRollback) {
			t.Fatal(err)
		}
		if after := w.read(t, 2); !reflect.DeepEqual(after, before) {
			t.Errorf("the change reads %+v, had %+v", after, before)
		}
	})
}

func TestForkSources_AreListedAndToldOfOpensAndAbandons(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	ctx := context.Background()
	rec := &activity{}
	w.changes.RegisterForkActivity(rec)
	t1 := w.addFork(t, "demo--t1", "main", nil, nil)
	commit(t, t1, "work", w.base, "Work\n\nWhy.\n", map[string]string{"w.txt": "w\n"})
	commit(t, t1, "other", w.base, "Other\n\nWhy.\n", map[string]string{"o.txt": "o\n"})
	w.branch(t, "feat", "Feat\n\nWhy.\n")
	fromFork := func(in *change.OpenInput) { in.SourceWorkspace = "demo--t1" }
	abandon := func(t *testing.T, number int64) {
		t.Helper()
		w.write(t, func(ctx context.Context, tx *sql.Tx) error {
			return w.changes.SetAbandoned(ctx, tx, "demo", number, "abandoned")
		})
	}
	told := func(t *testing.T, step string, want ...string) {
		t.Helper()
		if got := rec.told(); !reflect.DeepEqual(got, want) {
			t.Errorf("after %s the recorder was told %v, want %v", step, got, want)
		}
	}

	w.openBranch(t, "work", fromFork)
	told(t, "an open from the fork", "demo--t1")
	w.openBranch(t, "feat", nil)
	told(t, "an open from demo", "demo--t1")
	abandon(t, 1)
	told(t, "the move of the fork's change to abandoned", "demo--t1", "demo--t1")
	abandon(t, 2)
	told(t, "the move of demo's change to abandoned", "demo--t1", "demo--t1")
	w.openBranch(t, "other", fromFork)
	w.write(t, func(ctx context.Context, tx *sql.Tx) error {
		return w.changes.SetLanded(ctx, tx, "demo", 3, change.Landed{SHA: w.base, At: time.Now(), By: change.Actor{ActorType: change.ActorSystem}})
	})
	told(t, "an open from the fork and its landing", "demo--t1", "demo--t1", "demo--t1")

	t.Run("ChangeNumbersOfSource", func(t *testing.T) {
		for _, c := range []struct {
			workspace string
			want      []change.SourceChange
		}{
			{"demo--t1", []change.SourceChange{
				{WorkspaceSlug: "demo", Number: 1, Branch: "work", State: change.StateAbandoned},
				{WorkspaceSlug: "demo", Number: 3, Branch: "other", State: change.StateLanded},
			}},
			{"demo", []change.SourceChange{{WorkspaceSlug: "demo", Number: 2, Branch: "feat", State: change.StateAbandoned}}},
			{"nosuch", []change.SourceChange{}},
		} {
			if got, err := w.changes.ChangeNumbersOfSource(ctx, nil, c.workspace); err != nil || !reflect.DeepEqual(got, c.want) {
				t.Errorf("ChangeNumbersOfSource(%s) = %+v, %v; want %+v", c.workspace, got, err, c.want)
			}
		}
		if got, found, err := w.changes.OpenChangeOfBranch(ctx, nil, "demo--t1", "work"); err != nil || found {
			t.Errorf("OpenChangeOfBranch of an abandoned change's source = %+v, %v, %v; want none", got, found, err)
		}
	})
	t.Run("a recorder that fails", func(t *testing.T) {
		rec.mu.Lock()
		rec.fail = true
		rec.mu.Unlock()
		ch := w.openBranch(t, "work", fromFork)
		abandon(t, ch.Number)
		if got := w.read(t, ch.Number); got.State != change.StateAbandoned {
			t.Errorf("state = %s, want abandoned", got.State)
		}
		told(t, "an open and an abandon the recorder refused", "demo--t1", "demo--t1", "demo--t1", "demo--t1", "demo--t1")
	})
}
