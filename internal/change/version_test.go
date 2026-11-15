package change_test

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/change"
	"github.com/agent-fox-dev/parley/internal/gitrepo/gitrepotest"
	"github.com/agent-fox-dev/parley/internal/server"
)

// feat starts one commit ahead of main, adding x.txt; each version moves
// it on.
func TestRecordVersion_RecordsTheNewHeadAsTheNextVersion(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.hold(t)
	h1 := commit(t, w.demo, "feat", w.base, "One\n", map[string]string{"x.txt": "x\n"})
	w.openBranch(t, "feat", nil)
	v1 := w.versions(t, "demo", 1)[0]
	before := w.read(t, 1).UpdatedAt
	h2 := commit(t, w.demo, "feat", h1, "Two\n", map[string]string{"y.txt": "y\n"})

	v := w.mustRecord(t, change.VersionInput{Workspace: "demo", Branch: "feat", Head: h2, Kind: change.KindPush,
		Caller: w.ownerCaller(), SessionID: new(sessionTwo)})
	if v.Number != 2 || v.Kind != "push" || v.HeadSHA != h2 || deref(v.BaseSHA) != w.base || v.CreatedBy.ActorID == nil ||
		*v.CreatedBy.ActorID != w.owner || v.SessionID == nil || *v.SessionID != sessionTwo {
		t.Errorf("RecordVersion = %+v, want version 2 of kind push at %s by the owner in sessionTwo", v, h2)
	}
	stored, err := change.ReadVersions(context.Background(), w.Store.Main(), "demo", 1)
	if err != nil {
		t.Fatal(err)
	}
	if want := (change.Credential{Type: change.CredentialAPIKey, ID: "k1"}); stored[1].Credential != want {
		t.Errorf("the stored credential of version 2 = %+v, want %+v", stored[1].Credential, want)
	}
	versions := w.versions(t, "demo", 1)
	for member := range versions[1] {
		if strings.Contains(member, "credential") {
			t.Errorf("the version object shows %s", member)
		}
	}
	if len(versions) != 2 || !reflect.DeepEqual(versions[0], v1) || !reflect.DeepEqual(versions[1], asJSON(t, v)) {
		t.Errorf("versions = %v, want version 1 unchanged (%v) and %v", versions, v1, asJSON(t, v))
	}
	ch := w.read(t, 1)
	if ch.Version != 2 || ch.HeadSHA != h2 || deref(ch.BaseSHA) != w.base || ch.Stats.Commits != 2 || ch.Stats.FilesChanged != 2 ||
		ch.SessionID == nil || *ch.SessionID != sessionTwo || ch.Mergeable != change.MergeableUnknown || ch.UpdatedAt.Before(before) ||
		!reflect.DeepEqual(ch.Paths, []string{"x.txt", "y.txt"}) || !reflect.DeepEqual(ch.Sessions, []string{sessionTwo}) {
		t.Errorf("the change reads %+v, want version 2 at %s with two commits, x.txt and y.txt, sessionTwo and mergeable unknown", ch, h2)
	}
	if want := []string{sessionTwo + " demo 1/2"}; !reflect.DeepEqual(w.sessions.records, want) {
		t.Errorf("session links recorded = %v, want %v", w.sessions.records, want)
	}

	t.Run("a rewritten history", func(t *testing.T) {
		h3 := commit(t, w.demo, "feat", w.base, "Other\n", map[string]string{"z.txt": "z\n"})
		v := w.mustRecord(t, change.VersionInput{Workspace: "demo", Branch: "feat", Head: h3, Kind: change.KindPush, Caller: w.ownerCaller()})
		if ch := w.read(t, 1); v.Number != 3 || ch.Number != 1 || ch.Version != 3 || ch.HeadSHA != h3 || ch.Stats.Commits != 1 ||
			!reflect.DeepEqual(ch.Paths, []string{"z.txt"}) {
			t.Errorf("version %d; the change reads %+v, want version 3 of change 1 at %s touching z.txt", v.Number, ch, h3)
		}
	})
	t.Run("every kind", func(t *testing.T) {
		for _, kind := range []string{"push", "commit", "ref", "rebase", "restack", "revert", "sync"} {
			caller, actor := w.ownerCaller(), "api_key"
			if kind == "restack" {
				caller, actor = change.Caller{}, "system"
			}
			head := commit(t, w.demo, "feat", "feat", kind+"\n", map[string]string{kind + ".txt": kind + "\n"})
			v := w.mustRecord(t, change.VersionInput{Workspace: "demo", Branch: "feat", Head: head, Kind: kind, Caller: caller})
			got := w.versions(t, "demo", 1)
			last := got[len(got)-1]
			if by, _ := last["created_by"].(map[string]any); last["kind"] != kind || by["actor_type"] != actor || v.Kind != kind {
				t.Errorf("a version of kind %s is stored as %v by %v", kind, last["kind"], last["created_by"])
			}
		}
	})
}

func TestRecordVersion_RecordsNothingForTheSameHeadOrABranchWithoutAnActiveChange(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	other := w.addWorkspace(t, "other")
	feat := w.branch(t, "feat", "Feat\n")
	w.openBranch(t, "feat", nil)
	w.branch(t, "landed", "Landed\n")
	w.openBranch(t, "landed", nil)
	w.branch(t, "abandoned", "Abandoned\n")
	w.openBranch(t, "abandoned", nil)
	ctx := context.Background()
	if err := w.Store.Tx(ctx, func(tx *sql.Tx) error {
		if err := w.changes.SetLanded(ctx, tx, "demo", 2, change.Landed{SHA: w.base, At: time.Now(), By: change.Actor{ActorType: "system"}}); err != nil {
			return err
		}
		return w.changes.SetAbandoned(ctx, tx, "demo", 3, "abandoned")
	}); err != nil {
		t.Fatal(err)
	}
	unrelated := w.branch(t, "unrelated", "Unrelated\n")
	landed := commit(t, w.demo, "landed", "landed", "More\n", map[string]string{"m.txt": "m\n"})
	abandoned := commit(t, w.demo, "abandoned", "abandoned", "More\n", map[string]string{"m.txt": "m\n"})
	elsewhere := commit(t, other, "feat", "main", "Elsewhere\n", map[string]string{"e.txt": "e\n"})

	for _, c := range []struct {
		name                    string
		workspace, branch, head string
	}{
		{"the head the change has", "demo", "feat", feat},
		{"a branch without a change", "demo", "unrelated", unrelated},
		{"the branch of a landed change", "demo", "landed", landed},
		{"the branch of an abandoned change", "demo", "abandoned", abandoned},
		{"the same branch name in another workspace", "other", "feat", elsewhere},
	} {
		t.Run(c.name, func(t *testing.T) {
			before, seq := w.snapshot(t), w.seq(t)
			v, err := w.record(t, change.VersionInput{Workspace: c.workspace, Branch: c.branch, Head: c.head, Kind: change.KindPush,
				Caller: w.ownerCaller(), SessionID: new(sessionOne)})
			if v != nil || err != nil {
				t.Errorf("RecordVersion = %+v, %v; want no version and no error", v, err)
			}
			if after := w.snapshot(t); !reflect.DeepEqual(after, before) {
				t.Errorf("RecordVersion wrote %+v, had %+v", after, before)
			}
			if events := w.eventsAfter(t, seq); len(events) != 0 {
				t.Errorf("events = %+v, want none", events)
			}
		})
	}
}

func TestRecordVersion_KeepsEveryVersionRefWhateverHappensToTheSource(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	heads := []string{commit(t, w.demo, "feat", w.base, "One\n", map[string]string{"x.txt": "x\n"})}
	w.openBranch(t, "feat", nil)
	heads = append(heads, commit(t, w.demo, "feat", w.base, "One, amended\n", map[string]string{"x.txt": "x2\n"}))
	w.mustRecord(t, change.VersionInput{Workspace: "demo", Branch: "feat", Head: heads[1], Kind: change.KindPush, Caller: w.ownerCaller()})
	heads = append(heads, commit(t, w.demo, "feat", w.base, "Other\n", map[string]string{"z.txt": "z\n"}))
	w.mustRecord(t, change.VersionInput{Workspace: "demo", Branch: "feat", Head: heads[2], Kind: change.KindPush, Caller: w.ownerCaller()})
	gitrepotest.Git(t, w.demo, "update-ref", "-d", "refs/heads/feat")
	gitrepotest.Git(t, w.demo, "gc", "--quiet", "--prune=now")

	versions := w.versions(t, "demo", 1)
	for i, head := range heads {
		ref := change.VersionRef(1, int64(i+1))
		if got := strings.TrimSpace(gitrepotest.Git(t, w.demo, "rev-parse", ref)); got != head || versions[i]["head_sha"] != head {
			t.Errorf("%s = %s and version %d's head = %v, want %s", ref, got, i+1, versions[i]["head_sha"], head)
		}
		if _, err := gitrepotest.Run(w.demo, "", "cat-file", "-e", head); err != nil {
			t.Errorf("the head of version %d is gone: %v", i+1, err)
		}
	}

	t.Run("a source branch in a fork", func(t *testing.T) {
		m1 := w.addFork(t, "demo--m1", "main", nil, nil)
		first := commit(t, m1, "task", w.base, "Task\n", map[string]string{"t.txt": "t\n"})
		ch := w.openBranch(t, "task", func(in *change.OpenInput) { in.SourceWorkspace = "demo--m1" })
		second := commit(t, m1, "task", first, "More\n", map[string]string{"t.txt": "t2\n"})
		v := w.mustRecord(t, change.VersionInput{Workspace: "demo--m1", Branch: "task", Head: second, Kind: change.KindPush, Caller: w.ownerCaller()})
		if v.Number != 2 || w.read(t, ch.Number).HeadSHA != second {
			t.Fatalf("RecordVersion = version %d; want version 2 of %s at %s", v.Number, ch.Ref, second)
		}
		for i, head := range []string{first, second} {
			ref := change.VersionRef(ch.Number, int64(i+1))
			if got := strings.TrimSpace(gitrepotest.Git(t, w.demo, "rev-parse", ref)); got != head {
				t.Errorf("%s in demo = %s, want %s", ref, got, head)
			}
			if _, err := gitrepotest.Run(w.demo, "", "cat-file", "-e", head+"^{commit}"); err != nil {
				t.Errorf("demo lacks the fork's commit %s: %v", head, err)
			}
		}
	})
}

// main moves twice; feat carries x.txt as "x" or "y" on top of it.
func TestRecordVersion_NamesTheLatestPatchEquivalentVersion(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	commit(t, w.demo, "feat", w.base, "X\n", map[string]string{"x.txt": "x\n"})
	w.openBranch(t, "feat", nil)
	m1 := commit(t, w.demo, "main", "main", "Main moves\n", map[string]string{"m.txt": "1\n"})
	m2 := commit(t, w.demo, "main2", m1, "Main moves again\n", map[string]string{"m.txt": "2\n"})
	for _, c := range []struct {
		name string
		// main, when set, is where main moves first.
		main, onto, content string
		want                *int64
	}{
		{"a rebase without a change of content", "", m1, "x\n", new(int64(1))},
		{"an amended content", "", m1, "y\n", nil},
		{"the amended content rebased", m2, m2, "y\n", new(int64(3))},
		{"the first content again", "", m2, "x\n", new(int64(2))},
		{"words apart", "", m2, "x y\n", nil},
		{"the words together", "", m2, "xy\n", nil},
	} {
		if c.main != "" {
			gitrepotest.Git(t, w.demo, "update-ref", "refs/heads/main", c.main)
		}
		head := commit(t, w.demo, "feat", c.onto, c.name+"\n", map[string]string{"x.txt": c.content})
		v := w.mustRecord(t, change.VersionInput{Workspace: "demo", Branch: "feat", Head: head, Kind: change.KindRebase, Caller: w.ownerCaller()})
		var want any
		if c.want != nil {
			want = float64(*c.want)
		}
		if got := w.versions(t, "demo", 1)[v.Number-1]["patch_equivalent_to"]; !reflect.DeepEqual(v.PatchEquivalentTo, c.want) || got != want {
			t.Errorf("%s: version %d is patch-equivalent to %v (stored %v), want %v", c.name, v.Number, v.PatchEquivalentTo, got, want)
		}
	}
	// The interdiff of two versions apart only by white space shows it.
	status, got, _ := w.get(t, "/workspaces/demo/changes/1/diff?version=7&against=6", w.ownerKey)
	if f := filesByPath(got)["x.txt"]; status != http.StatusOK || f["changed"] != true {
		t.Errorf("GET diff?version=7&against=6 = %d %v, want x.txt changed", status, got)
	}
}

func TestRecordVersion_StoresTheStructuralLandingWithoutTheEvaluator(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	commit(t, w.demo, "feat", w.base, "Feat\n", map[string]string{"x.txt": "x\n"})
	w.openBranch(t, "feat", nil)
	w.branch(t, "drafty", "Drafty\n")
	w.openBranch(t, "drafty", func(in *change.OpenInput) { in.Draft = true })
	calls := &steps{}
	w.changes.RegisterLandingEvaluator(&evaluator{steps: calls, landing: change.Landing{Status: change.LandingReady}})
	main := commit(t, w.demo, "main", "main", "Main moves\n", map[string]string{"m.txt": "m\n"})
	side := commit(t, w.demo, "side", w.base, "Side\n", map[string]string{"s.txt": "s\n"})
	merge := func(t *testing.T) string {
		if _, err := gitrepotest.Run(w.demo, "commit refs/heads/feat\n"+committer+data("Merge\n")+"from "+main+"\nmerge "+side+"\n\n",
			"fast-import", "--quiet", "--force"); err != nil {
			t.Fatal(err)
		}
		return gitrepotest.Refs(t, w.demo, "refs/heads/")["feat"]
	}
	var previousBase string

	for _, c := range []struct {
		name   string
		number int64
		// move moves the change's branch and returns its new head.
		move func(t *testing.T) string
		// reason is the landing's blocked reason, "" for the evaluator's
		// answer.
		reason string
	}{
		{"the head of main", 1, func(*testing.T) string { return main }, "nothing_to_land"},
		{"an ancestor of main", 1, func(*testing.T) string { return w.base }, "nothing_to_land"},
		{"a merge commit ahead of main", 1, merge, "merge_commits"},
		{"a history with no common ancestor", 1, func(t *testing.T) string {
			return commit(t, w.demo, "feat", "", "Root\n", map[string]string{"r.txt": "r\n"})
		}, "unrelated_history"},
		{"a commit ahead of main", 1, func(t *testing.T) string {
			return commit(t, w.demo, "feat", main, "Ahead\n", map[string]string{"x.txt": "x\n"})
		}, ""},
		{"a draft", 2, func(t *testing.T) string {
			return commit(t, w.demo, "drafty", "drafty", "More\n", map[string]string{"d.txt": "d\n"})
		}, "draft"},
		{"a missing target branch", 1, func(t *testing.T) string {
			previousBase = deref(w.read(t, 1).BaseSHA)
			gitrepotest.Git(t, w.demo, "update-ref", "-d", "refs/heads/main")
			return commit(t, w.demo, "feat", "feat", "After\n", map[string]string{"y.txt": "y\n"})
		}, "target_not_found"},
	} {
		t.Run(c.name, func(t *testing.T) {
			before := w.read(t, c.number)
			called := len(calls.order)
			branch := map[int64]string{1: "feat", 2: "drafty"}[c.number]
			v := w.mustRecord(t, change.VersionInput{Workspace: "demo", Branch: branch, Head: c.move(t), Kind: change.KindPush, Caller: w.ownerCaller()})
			ch := w.read(t, c.number)
			if ch.Version != before.Version+1 || v.Number != ch.Version || ch.State != before.State {
				t.Errorf("version %d and state %s, want version %d and state %s", ch.Version, ch.State, before.Version+1, before.State)
			}
			wantCalls, want := called, change.LandingReady
			if c.reason == "" {
				wantCalls++
			} else {
				want = change.LandingBlocked
			}
			if reason := ch.Landing.Reason; ch.Landing.Status != want || (c.reason == "") != (reason == nil) || (reason != nil && *reason != c.reason) {
				t.Errorf("landing = %+v, want %s %s", ch.Landing, want, c.reason)
			}
			if len(calls.order) != wantCalls {
				t.Errorf("the evaluator was called %d times, want %d", len(calls.order)-called, wantCalls-called)
			}
			if c.reason == "unrelated_history" && (v.BaseSHA != nil || ch.BaseSHA != nil ||
				ch.Stats != (change.Stats{Commits: 1, FilesChanged: 1, Insertions: 1}) || !reflect.DeepEqual(ch.Paths, []string{"r.txt"})) {
				t.Errorf("base_sha %v, the change reads %+v; want no base and r.txt measured from the empty tree", v.BaseSHA, ch)
			}
			if c.reason == "target_not_found" && deref(v.BaseSHA) != previousBase {
				t.Errorf("base_sha = %s, want the previous version's %s", deref(v.BaseSHA), previousBase)
			}
		})
	}
}

func TestRecordVersion_RunsTheCollaboratorsInOrderAndWritesTheVersionEvent(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	h1 := commit(t, w.demo, "feat", w.base, "One\n", map[string]string{"x.txt": "x\n"})
	w.openBranch(t, "feat", nil)
	order := &steps{}
	o := &observer{steps: order, changes: w.changes}
	w.changes.RegisterStackLinker(&stack{steps: order})
	w.changes.RegisterLandingEvaluator(&evaluator{steps: order, landing: change.Landing{Status: change.LandingPending}})
	w.changes.AddVersionObserver(o)
	seq := w.seq(t)
	h2 := commit(t, w.demo, "feat", h1, "Two\n", map[string]string{"y.txt": "y\n"})
	pat := change.Credential{Type: change.CredentialPAT, ID: "t1"}

	w.mustRecord(t, change.VersionInput{Workspace: "demo", Branch: "feat", Head: h2, Kind: change.KindCommit,
		Caller: change.Caller{UserID: w.owner, Credential: pat}})
	if want := []string{"linker", "evaluator", "observer"}; !reflect.DeepEqual(order.order, want) {
		t.Errorf("calls = %v, want %v", order.order, want)
	}
	if !reflect.DeepEqual(o.versions, []int64{2}) || !reflect.DeepEqual(o.creds, []change.Credential{pat}) || !reflect.DeepEqual(o.landings, []string{"pending"}) {
		t.Errorf("the observer saw versions %v by %v with landings %v, want [2] by the PAT t1 with [pending]", o.versions, o.creds, o.landings)
	}
	events := w.eventsAfter(t, seq)
	if len(events) != 1 || events[0].Type != change.EventVersion || events[0].ResourceID != "1" {
		t.Fatalf("events = %+v, want one parley.change.version of change 1", events)
	}
	want := map[string]any{"number": 1.0, "ref": "demo#1", "version": 2.0, "head_sha": h2, "kind": "commit", "base_sha": w.base,
		"patch_equivalent_to": nil, "draft": false}
	if got := metadata(t, events[0]); !reflect.DeepEqual(got, want) {
		t.Errorf("metadata = %v, want %v", got, want)
	}

	t.Run("a version the hub makes", func(t *testing.T) {
		h3 := commit(t, w.demo, "feat", h2, "Three\n", map[string]string{"z.txt": "z\n"})
		w.mustRecord(t, change.VersionInput{Workspace: "demo", Branch: "feat", Head: h3, Kind: change.KindRestack})
		if !reflect.DeepEqual(o.creds, []change.Credential{pat, {}}) {
			t.Errorf("the observer was given the credentials %v, want the PAT t1, then none", o.creds)
		}
	})
	t.Run("the caller rolls back", func(t *testing.T) {
		before, seq := w.read(t, 1), w.seq(t)
		head := commit(t, w.demo, "feat", "feat", "Undone\n", map[string]string{"u.txt": "u\n"})
		err := w.Store.Tx(context.Background(), func(tx *sql.Tx) error {
			v, err := w.changes.RecordVersion(context.Background(), tx, change.VersionInput{Workspace: "demo", Branch: "feat", Head: head,
				Kind: change.KindPush, Caller: w.ownerCaller()})
			if err != nil {
				return err
			}
			if v == nil || v.Number != before.Version+1 {
				t.Errorf("RecordVersion = %+v, want version %d", v, before.Version+1)
			}
			return errRollback
		})
		if !errors.Is(err, errRollback) {
			t.Fatalf("the transaction = %v, want the rollback", err)
		}
		if after := w.read(t, 1); !reflect.DeepEqual(after, before) {
			t.Errorf("the change reads %+v, had %+v", after, before)
		}
		if n := len(w.versions(t, "demo", 1)); n != int(before.Version) {
			t.Errorf("%d versions, want %d", n, before.Version)
		}
		if events := w.eventsAfter(t, seq); len(events) != 0 {
			t.Errorf("events = %+v, want none", events)
		}
	})
	t.Run("an observer fails", func(t *testing.T) {
		w.changes.AddVersionObserver(&observer{err: errors.New("observer down")})
		before := w.read(t, 1)
		head := commit(t, w.demo, "feat", "feat", "Observed\n", map[string]string{"o.txt": "o\n"})
		if v, err := w.record(t, change.VersionInput{Workspace: "demo", Branch: "feat", Head: head, Kind: change.KindPush,
			Caller: w.ownerCaller()}); v != nil || err == nil {
			t.Errorf("RecordVersion = %+v, %v; want the observer's error", v, err)
		}
		ref := change.VersionRef(1, before.Version+1)
		if got, found := gitrepotest.Refs(t, w.demo, "")[ref]; found {
			t.Errorf("%s = %s, want no ref for a version never recorded", ref, got)
		}
	})
}

func TestRecordVersion_RefusesASessionTheLinkerRefuses(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	h1 := commit(t, w.demo, "feat", w.base, "One\n", map[string]string{"x.txt": "x\n"})
	w.openBranch(t, "feat", func(in *change.OpenInput) { in.SessionID = new(sessionOne) })
	h2 := commit(t, w.demo, "feat", h1, "Two\n", map[string]string{"y.txt": "y\n"})
	before, seq := w.snapshot(t), w.seq(t)

	v, err := w.record(t, change.VersionInput{Workspace: "demo", Branch: "feat", Head: h2, Kind: change.KindPush,
		Caller: w.ownerCaller(), SessionID: new(sessionOther)})
	var apiErr *server.Error
	if v != nil || !errors.As(err, &apiErr) || apiErr.Status() != http.StatusBadRequest || apiErr.Type() != "unknown_session" {
		t.Errorf("RecordVersion = %+v, %v; want no version and 400 unknown_session", v, err)
	}
	if after := w.snapshot(t); !reflect.DeepEqual(after, before) {
		t.Errorf("a refused version wrote %+v, had %+v", after, before)
	}
	if events := w.eventsAfter(t, seq); len(events) != 0 {
		t.Errorf("events = %+v, want none", events)
	}

	t.Run("without a session", func(t *testing.T) {
		v := w.mustRecord(t, change.VersionInput{Workspace: "demo", Branch: "feat", Head: h2, Kind: change.KindPush, Caller: w.ownerCaller()})
		ch := w.read(t, 1)
		if v.Number != 2 || v.SessionID != nil || ch.SessionID != nil || !reflect.DeepEqual(ch.Sessions, []string{sessionOne}) {
			t.Errorf("version %d in session %v; the change has session %v and sessions %v; want version 2 without a session and sessions [sessionOne]",
				v.Number, v.SessionID, ch.SessionID, ch.Sessions)
		}
	})
}
