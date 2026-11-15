package change_test

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"reflect"
	"slices"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/change"
	"github.com/agent-fox-dev/parley/internal/gitrepo"
	"github.com/agent-fox-dev/parley/internal/gitrepo/gitrepotest"
	"github.com/agent-fox-dev/parley/internal/hubtest"
	"github.com/agent-fox-dev/parley/internal/server"
)

func TestRestackRoute_RebasesTheChangeOntoWhatItMustSitOn(t *testing.T) {
	t.Parallel()
	m := newMiaWorldWith(t, hubtest.Config{TOML: zeroDebounce + "max_stack_depth = 3\n"})
	m.hold(t)
	m.holdRestacks(t)
	// Mia's changes come from her fork demo--t1, since she only reads demo.
	asMia := func(session *string) func(*change.OpenInput) {
		return func(in *change.OpenInput) {
			in.SourceWorkspace, in.Caller, in.SessionID = "demo--t1", m.caller(), session
		}
	}
	head := func(t *testing.T, ch change.Change) string {
		dir := m.demo
		if ch.Source.WorkspaceSlug == "demo--t1" {
			dir = m.t1
		}
		return gitrepotest.Refs(t, dir, "refs/heads/")[ch.Source.Branch]
	}
	for i, c := range []struct {
		name string
		// build builds the change, each branch named with the prefix p, and
		// returns it and the commit it must be restacked onto.
		build             func(t *testing.T, p string) (change.Change, string)
		credential        string
		actorType, author string
		body              any
		session           any
		reason            any
	}{
		{"behind the change below while automatic restacks were off, by its author", func(t *testing.T, p string) (change.Change, string) {
			m.set(t, "demo", "settings", `{"changes.auto_restack":false}`)
			defer m.set(t, "demo", "settings", `{}`)
			a := m.stacked(t, p+"a")[0]
			commit(t, m.t1, p+"b", a.HeadSHA, "B\n", map[string]string{p + "b.txt": "b\n"})
			b := m.openBranch(t, p+"b", asMia(new(sessionOne)))
			onto := m.amend(t, p+"a", map[string]string{p + "a2.txt": "a2\n"})
			if got := m.read(t, b.Number); deref(got.RestackReason) != change.RestackAutoOff {
				t.Fatalf("demo#%d before the restack: restack_reason %q, want auto_restack_off", b.Number, deref(got.RestackReason))
			}
			return b, onto
		}, m.miaKey, change.CredentialAPIKey, m.mia, nil, sessionOne, nil},
		{"on a target branch that moved ahead, by a writer", func(t *testing.T, p string) (change.Change, string) {
			commit(t, m.t1, p+"x", m.base, "X\n", map[string]string{p + "x.txt": "x\n"})
			x := m.openBranch(t, p+"x", asMia(nil))
			return x, commit(t, m.demo, "main", m.head(t, "main"), "main moved\n", map[string]string{p + "m.txt": "m\n"})
		}, m.ownerKey, change.CredentialAPIKey, m.owner, map[string]any{}, nil, nil},
		{"in conflict on the change below, which moved again so that it applies, with the admin token", func(t *testing.T, p string) (change.Change, string) {
			a := commit(t, m.demo, p+"a", m.base, "A\n", map[string]string{p + "f.txt": "a\n"})
			m.openBranch(t, p+"a", nil)
			commit(t, m.t1, p+"b", a, "B\n", map[string]string{p + "f.txt": "b\n"})
			b := m.openBranch(t, p+"b", asMia(new(sessionOne)))
			m.amend(t, p+"a", map[string]string{p + "f.txt": "a2\n"})
			m.runPending(t)
			onto := m.amend(t, p+"a", map[string]string{p + "f.txt": "a\n", p + "a3.txt": "a3\n"})
			if got := m.read(t, b.Number); deref(got.RestackReason) != change.RestackConflict || !slices.Contains(m.pending(t), b.Number) {
				t.Fatalf("demo#%d before the restack: restack_reason %q, pending %v; want conflict, pending", b.Number, deref(got.RestackReason), m.pending(t))
			}
			return b, onto
		}, m.Token, change.CredentialAdminToken, "", map[string]any{"session_id": sessionTwo}, sessionTwo, nil},
		{"too deep in its stack", func(t *testing.T, p string) (change.Change, string) {
			s := m.stacked(t, p+"a", p+"b", p+"c", p+"d")
			return s[3], m.amend(t, p+"c", map[string]string{p + "c2.txt": "c2\n"})
		}, m.ownerKey, change.CredentialAPIKey, m.owner, nil, nil, change.RestackTooDeep},
	} {
		t.Run(c.name, func(t *testing.T) {
			ch, onto := c.build(t, fmt.Sprintf("r%d", i))
			seq := m.seq(t)
			status, out := m.restack(t, ch.Number, c.credential, c.body)
			got, _ := out["change"].(map[string]any)
			if status != http.StatusOK || out["status"] != "restacked" || got["version"] != float64(ch.Version+1) || got["base_sha"] != onto ||
				got["needs_restack"] != (c.reason != nil) || got["restack_reason"] != c.reason {
				t.Fatalf("restack demo#%d = %d %v, want 200 restacked at version %d on %s, restack_reason %v", ch.Number, status, out, ch.Version+1, onto, c.reason)
			}
			vs := m.versions(t, "demo", ch.Number)
			v := vs[len(vs)-1]
			by, _ := v["created_by"].(map[string]any)
			if v["kind"] != change.KindRestack || by["actor_type"] != c.actorType || deref(stringOf(by["actor_id"])) != c.author ||
				v["session_id"] != c.session || head(t, ch) != v["head_sha"] || slices.Contains(m.pending(t), ch.Number) {
				t.Errorf("version %v by %v, branch at %s, pending %v; want a restack by %s %q in %v at the branch's head, none pending",
					v, by, head(t, ch), m.pending(t), c.actorType, c.author, c.session)
			}
			var types []string
			for _, e := range m.changeEventsOf(t, seq, ch.Number) {
				types = append(types, e.Type)
				if e.ActorType != c.actorType || deref(e.ActorID) != c.author {
					t.Errorf("%s by %s %q, want by %s %q", e.Type, e.ActorType, deref(e.ActorID), c.actorType, c.author)
				}
				if e.Type == change.EventRestack && metadata(t, e)["trigger"] != change.TriggerManual {
					t.Errorf("parley.change.restack with %v, want the trigger manual", metadata(t, e))
				}
			}
			if want := []string{change.EventVersion, change.EventRestack}; !slices.Equal(types, want) {
				t.Errorf("events of demo#%d %v, want %v", ch.Number, types, want)
			}
		})
	}
}

// stringOf is v as a *string, nil when it is not a string.
func stringOf(v any) *string {
	if s, ok := v.(string); ok {
		return &s
	}
	return nil
}

func TestRestackRoute_AnswersUpToDateForAChangeOnWhatItMustSitOn(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	for i, c := range []struct {
		name string
		// build builds the change, each branch named with the prefix p.
		build   func(t *testing.T, p string) change.Change
		flagged bool
	}{
		{"on the head of the change below", func(t *testing.T, p string) change.Change {
			return w.stacked(t, p+"a", p+"b")[1]
		}, false},
		{"with no change below, on the head of its target", func(t *testing.T, p string) change.Change {
			w.branch(t, p+"x", "X\n\nWhy.\n")
			return w.openBranch(t, p+"x", nil)
		}, false},
		{"on the head of the change below, flagged with automatic restacks off", func(t *testing.T, p string) change.Change {
			b := w.stacked(t, p+"a", p+"b")[1]
			w.write(t, func(ctx context.Context, tx *sql.Tx) error {
				return w.changes.SetNeedsRestack(ctx, tx, "demo", b.Number, true, new(change.RestackAutoOff))
			})
			return b
		}, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			ch := c.build(t, fmt.Sprintf("u%d", i))
			mergeable := w.settled(t, ch.Number).Mergeable
			release := w.hold(t)
			defer release()
			seq := w.seq(t)
			status, out := w.restack(t, ch.Number, w.ownerKey, nil)
			got, _ := out["change"].(map[string]any)
			if status != http.StatusOK || out["status"] != "up_to_date" || got["version"] != float64(ch.Version) || got["needs_restack"] != false {
				t.Errorf("restack demo#%d = %d %v, want 200 up_to_date at version %d, needs_restack false", ch.Number, status, out, ch.Version)
			}
			if events := w.changeEventsOf(t, seq, ch.Number); len(events) != 0 || w.head(t, ch.Source.Branch) != ch.HeadSHA {
				t.Errorf("%d events, branch at %s; want none, at %s", len(events), w.head(t, ch.Source.Branch), ch.HeadSHA)
			}
			// A cleared flag has the mergeability computed again.
			if c.flagged {
				mergeable = change.MergeableUnknown
			}
			if now := w.read(t, ch.Number).Mergeable; now != mergeable {
				t.Errorf("mergeable %s, want %s", now, mergeable)
			}
		})
	}
}

func TestRestackRoute_AnswersAConflictWithItsReportAndMovesNothing(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.set(t, "demo", "settings", `{"changes.auto_restack":false}`)
	w.hold(t)
	guard := w.guard(t, nil)
	a := commit(t, w.demo, "a", w.base, "A\n", map[string]string{"f.txt": "a\n"})
	w.openBranch(t, "a", nil)
	commit(t, w.demo, "b", a, "B\n", map[string]string{"f.txt": "b\n"})
	b := w.openBranch(t, "b", nil)
	onto := w.amend(t, "a", map[string]string{"f.txt": "a2\n"})
	seq := w.seq(t)
	for range 2 {
		status, out := w.restack(t, b.Number, w.ownerKey, nil)
		e, _ := out["error"].(map[string]any)
		details, _ := e["details"].(map[string]any)
		files, _ := details["files"].([]any)
		if status != http.StatusConflict || e["error_type"] != "rebase_conflict" || e["code"] != 409.0 || details["target_sha"] != onto ||
			len(files) == 0 || files[0].(map[string]any)["path"] != "f.txt" {
			t.Errorf("restack demo#%d = %d %v, want 409 rebase_conflict with the report on %s of f.txt", b.Number, status, out, onto)
		}
		ch := w.read(t, b.Number)
		if deref(ch.RestackReason) != change.RestackConflict || ch.Mergeable != change.MergeableConflict || ch.Version != 1 || w.head(t, "b") != b.HeadSHA {
			t.Errorf("demo#%d: restack_reason %q, mergeable %s, version %d, branch at %s; want conflict, conflict, 1, %s",
				b.Number, deref(ch.RestackReason), ch.Mergeable, ch.Version, w.head(t, "b"), b.HeadSHA)
		}
	}
	if n := len(w.changeEvents(t, seq, change.EventConflict, b.Number)); n != 1 || len(guard.called()) != 0 {
		t.Errorf("%d parley.change.conflict events, %d guard calls; want 1, none", n, len(guard.called()))
	}
}

func TestRestackRoute_RefusesAChangeItCannotRestack(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	for i, c := range []struct {
		name string
		// build builds the change, each branch named with the prefix p.
		build     func(t *testing.T, p string) change.Change
		status    int
		errorType string
	}{
		{"landed", func(t *testing.T, p string) change.Change {
			ch := w.stacked(t, p+"l")[0]
			w.land(t, ch.Number, map[string]string{p + "l.txt": p + "l\n"})
			return ch
		}, http.StatusConflict, "change_closed"},
		{"abandoned", func(t *testing.T, p string) change.Change {
			ch := w.stacked(t, p+"x")[0]
			w.write(t, func(ctx context.Context, tx *sql.Tx) error {
				return w.changes.SetAbandoned(ctx, tx, "demo", ch.Number, "")
			})
			return ch
		}, http.StatusConflict, "change_closed"},
		{"with no change below and its target branch deleted", func(t *testing.T, p string) change.Change {
			w.branch(t, p+"r", "R\n\nWhy.\n")
			ch := w.openBranch(t, p+"r", func(in *change.OpenInput) { in.TargetBranch = new("release") })
			gitrepotest.Git(t, w.demo, "update-ref", "-d", "refs/heads/release")
			return ch
		}, http.StatusBadRequest, "target_not_found"},
		{"with a merge commit among its own commits", func(t *testing.T, p string) change.Change {
			x := commit(t, w.demo, p+"m", w.base, "X\n\nWhy.\n", map[string]string{p + "x.txt": "x\n"})
			ch := w.openBranch(t, p+"m", nil)
			y := commit(t, w.demo, p+"y", w.base, "Y\n", map[string]string{p + "y.txt": "y\n"})
			if _, err := gitrepotest.Run(w.demo, "commit refs/heads/"+p+"m\n"+committer+data("Merge\n")+"from "+x+"\nmerge "+y+"\n\n",
				"fast-import", "--quiet", "--force"); err != nil {
				t.Fatal(err)
			}
			w.mustRecord(t, change.VersionInput{Workspace: "demo", Branch: p + "m", Head: w.head(t, p+"m"), Kind: change.KindPush, Caller: w.ownerCaller()})
			commit(t, w.demo, "main", w.head(t, "main"), "main moved\n", map[string]string{p + "main.txt": "m\n"})
			return w.read(t, ch.Number)
		}, http.StatusBadRequest, "merge_commits_unsupported"},
	} {
		t.Run(c.name, func(t *testing.T) {
			ch := c.build(t, fmt.Sprintf("c%d", i))
			before, seq := w.read(t, ch.Number), w.seq(t)
			if status, out := w.restack(t, ch.Number, w.ownerKey, nil); status != c.status || errorType(out) != c.errorType {
				t.Errorf("restack demo#%d = %d %v, want %d %s", ch.Number, status, out, c.status, c.errorType)
			}
			after := w.read(t, ch.Number)
			if events := w.changeEventsOf(t, seq, ch.Number); after.Version != before.Version || w.head(t, ch.Source.Branch) != before.HeadSHA || len(events) != 0 {
				t.Errorf("demo#%d at version %d, branch at %s, %d events; want %d, %s, none", ch.Number, after.Version, w.head(t, ch.Source.Branch), len(events), before.Version, before.HeadSHA)
			}
		})
	}
}

func TestRestackRoute_RefusesASessionTheLinkerRefusesBeforeAnyReplay(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.set(t, "demo", "settings", `{"changes.auto_restack":false}`)
	b := w.stacked(t, "a", "b")[1]
	w.amend(t, "a", map[string]string{"a2.txt": "a2\n"})
	var replays atomic.Int32
	change.SetRestackSeams(w.changes, change.RestackSeams{
		Rebase: func(ctx context.Context, repo *gitrepo.Repo, base, head, onto string, at time.Time) (string, *gitrepo.ConflictReport, error) {
			replays.Add(1)
			return repo.RebaseFrom(ctx, base, head, onto, at)
		},
	})
	for _, session := range []string{"44444444-4444-4444-8444-444444444444", sessionOther} {
		status, out := w.restack(t, b.Number, w.ownerKey, map[string]any{"session_id": session})
		if status != http.StatusBadRequest || errorType(out) != "unknown_session" || replays.Load() != 0 || w.read(t, b.Number).Version != b.Version {
			t.Errorf("restack with session %s = %d %v after %d replays; want 400 unknown_session, no replay, version %d", session, status, out, replays.Load(), b.Version)
		}
	}
}

func TestRestackRoute_AnswersRefMovedWhenTheSourceBranchKeepsMoving(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.set(t, "demo", "settings", `{"changes.auto_restack":false}`)
	b := w.stacked(t, "a", "b")[1]
	w.amend(t, "a", map[string]string{"a2.txt": "a2\n"})
	// The commits the branch is moved to before each swap, made here since
	// the hook runs in the hub's goroutine.
	var moves []string
	for i := range 5 {
		moves = append(moves, commit(t, w.demo, "moves", w.head(t, "b"), "move\n", map[string]string{"b.txt": strconv.Itoa(i)}))
		gitrepotest.Git(t, w.demo, "update-ref", "refs/heads/b", moves[i])
	}
	gitrepotest.Git(t, w.demo, "update-ref", "refs/heads/b", b.HeadSHA)
	change.SetRestackSeams(w.changes, change.RestackSeams{
		BeforeSwap: func(_ context.Context, _ string, _ int64, attempt int) {
			_, _ = gitrepotest.Run(w.demo, "", "update-ref", "refs/heads/b", moves[attempt-1])
		},
	})
	status, out := w.restack(t, b.Number, w.ownerKey, nil)
	e, _ := out["error"].(map[string]any)
	details, _ := e["details"].(map[string]any)
	if status != http.StatusConflict || e["error_type"] != "ref_moved" || details["current_sha"] != w.head(t, "b") {
		t.Errorf("restack demo#%d = %d %v, want 409 ref_moved at %s", b.Number, status, out, w.head(t, "b"))
	}
	for _, v := range w.versions(t, "demo", b.Number) {
		if v["kind"] == change.KindRestack {
			t.Errorf("a restack version was recorded: %v", v)
		}
	}
}

func TestRestackRoute_AsksTheVersionGuardBeforeTheSourceBranchMoves(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.hold(t)
	w.holdRestacks(t)
	pat, patID := w.patFor(t, []string{"changes:read", "changes:write"}, []string{"docs/**"})
	refusal := server.PathNotAllowed("src/x.go is outside the paths of the token").With("path", "src/x.go")
	for i, c := range []struct {
		name                    string
		guarded, behind, worker bool
		refuse                  error
		status                  int
		answer                  string
		calls                   int
	}{
		{"the guard refuses", true, true, false, refusal, http.StatusForbidden, "", 1},
		{"the guard accepts", true, true, false, nil, http.StatusOK, "restacked", 1},
		{"up to date", true, false, false, nil, http.StatusOK, "up_to_date", 0},
		{"a restack of the hub", true, true, true, nil, 0, "", 0},
		{"no guard", false, true, false, nil, http.StatusOK, "restacked", 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			p := fmt.Sprintf("g%d", i)
			b := w.stacked(t, p+"a", p+"b")[1]
			onto := w.head(t, p+"a")
			if c.behind {
				onto = w.amend(t, p+"a", map[string]string{p + "a2.txt": "a2\n"})
			}
			guard := &versionGuard{}
			if c.guarded {
				guard = w.guard(t, c.refuse)
			}
			if c.worker {
				w.runPending(t)
				if v := w.read(t, b.Number); v.Version != 2 || len(guard.called()) != 0 {
					t.Errorf("the hub's restack left demo#%d at version %d after %d guard calls; want 2, none", b.Number, v.Version, len(guard.called()))
				}
				return
			}
			_, _, before := w.get(t, "/workspaces/demo/changes/"+strconv.FormatInt(b.Number, 10), w.ownerKey)
			seq := w.seq(t)
			status, out := w.restack(t, b.Number, pat, nil)
			calls := guard.called()
			if status != c.status || (c.answer != "" && out["status"] != c.answer) || len(calls) != c.calls {
				t.Fatalf("restack demo#%d = %d %v after %d guard calls; want %d %s after %d", b.Number, status, out, len(calls), c.status, c.answer, c.calls)
			}
			if c.calls > 0 {
				call := calls[0]
				want := change.Caller{UserID: w.owner, Username: "owner", Credential: change.Credential{Type: change.CredentialPAT, ID: patID}, Paths: []string{"docs/**"}}
				_, err := gitrepotest.Run(w.demo, "", "merge-base", "--is-ancestor", onto, call.head)
				if !reflect.DeepEqual(call.caller, want) || call.base != onto || err != nil || call.branchAt != b.HeadSHA {
					t.Errorf("the guard was called with %+v; want %+v, base %s, a head built on it, the branch at %s", call, want, onto, b.HeadSHA)
				}
			}
			if c.refuse == nil {
				return
			}
			e, _ := out["error"].(map[string]any)
			_, _, after := w.get(t, "/workspaces/demo/changes/"+strconv.FormatInt(b.Number, 10), w.ownerKey)
			if e["error_type"] != "path_not_allowed" || e["path"] != "src/x.go" || !bytes.Equal(after, before) ||
				w.head(t, p+"b") != b.HeadSHA || len(w.changeEventsOf(t, seq, b.Number)) != 0 {
				t.Errorf("the refused restack answered %v and left %s, the branch at %s; want path_not_allowed for src/x.go and %s at %s",
					e, after, w.head(t, p+"b"), before, b.HeadSHA)
			}
		})
	}
}
