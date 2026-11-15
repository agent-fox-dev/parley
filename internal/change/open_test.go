package change_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/change"
	"github.com/agent-fox-dev/parley/internal/gitrepo/gitrepotest"
	"github.com/agent-fox-dev/parley/internal/server"
)

// Each row opens a change from a source of its own: a branch of demo, a
// branch of the fork demo--m1 (base release, a purpose and sessionOne) or
// of the fork demo--m2 (base release, sessionOther, which the linker
// refuses).
func TestOpen_FillsTheDefaults(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	m1 := w.addFork(t, "demo--m1", "release", new("fix the flake"), new(sessionOne))
	m2 := w.addFork(t, "demo--m2", "release", nil, new(sessionOther))
	long := strings.Repeat("y", 20000)
	commit(t, w.demo, "feat", w.base, "Add x\n\nBecause y\n", map[string]string{"x.txt": "x\n"})
	commit(t, m1, "subject", w.base, "Subject only\n", map[string]string{"s.txt": "s\n"})
	commit(t, m1, "body", w.base, "With body\n\nThe body.\n", map[string]string{"b.txt": "b\n"})
	commit(t, m1, "trigger", w.base, "Triggered\n", map[string]string{"t.txt": "t\n"})
	commit(t, m2, "refused", w.base, "Refused session\n", map[string]string{"r.txt": "r\n"})
	commit(t, w.demo, "long", w.base, "Long\n\n"+long+"\n", map[string]string{"l.txt": "l\n"})
	commit(t, w.demo, "explicit", w.base, "Ignored\n\nIgnored too\n", map[string]string{"e.txt": "e\n"})

	for _, c := range []struct {
		name string
		body map[string]any
		// in, when set, opens through the component instead of the route.
		in   *change.OpenInput
		want map[string]any
	}{
		{"a branch of the workspace", map[string]any{"source": map[string]any{"branch": "feat"}},
			nil, map[string]any{"title": "Add x", "intent": "Because y", "target_branch": "main", "session_id": nil,
				"auto_land": false, "labels": []any{}, "source": map[string]any{"workspace_slug": "demo", "branch": "feat"}}},
		{"a fork's commit without a body", map[string]any{"source": map[string]any{"workspace_slug": "demo--m1", "branch": "subject"}},
			nil, map[string]any{"title": "Subject only", "intent": "fix the flake", "target_branch": "release", "session_id": sessionOne}},
		{"a fork's commit with a body", map[string]any{"source": map[string]any{"workspace_slug": "demo--m1", "branch": "body"}},
			nil, map[string]any{"intent": "The body.", "target_branch": "release", "session_id": sessionOne}},
		{"the session of the trigger", nil,
			&change.OpenInput{Workspace: "demo", SourceWorkspace: "demo--m1", SourceBranch: "trigger", TriggerSession: new(sessionTwo),
				Caller: change.Caller{UserID: w.owner, Credential: change.Credential{Type: change.CredentialAPIKey, ID: "k1"}}, Kind: change.KindPush},
			map[string]any{"session_id": sessionTwo, "target_branch": "release"}},
		{"a fork session the linker refuses", map[string]any{"source": map[string]any{"workspace_slug": "demo--m2", "branch": "refused"}},
			nil, map[string]any{"session_id": nil, "sessions": []any{}}},
		{"explicit values", map[string]any{"source": map[string]any{"branch": "explicit"}, "title": "T", "intent": "I",
			"target_branch": "release", "auto_land": true, "labels": []any{"a"}},
			nil, map[string]any{"title": "T", "intent": "I", "target_branch": "release", "auto_land": true, "labels": []any{"a"}}},
	} {
		t.Run(c.name, func(t *testing.T) {
			var got map[string]any
			if c.in != nil {
				ch, err := w.open(t, *c.in)
				if err != nil {
					t.Fatalf("Open: %v", err)
				}
				got = asJSON(t, ch)
			} else {
				got = w.mustPost(t, c.body)
			}
			for member, want := range c.want {
				if !reflect.DeepEqual(got[member], want) {
					t.Errorf("%s = %#v, want %#v", member, got[member], want)
				}
			}
		})
	}
	t.Run("a long commit body", func(t *testing.T) {
		got := w.mustPost(t, map[string]any{"source": map[string]any{"branch": "long"}})
		if intent, _ := got["intent"].(string); intent != long[:change.MaxIntent] {
			t.Errorf("intent has %d characters, want the first %d of the body", len(intent), change.MaxIntent)
		}
	})
}

// feat is two commits ahead of main: the first changes the last line of
// a.txt and adds two lines to it, the second adds b.txt of two lines.
func TestOpen_RecordsVersionOneWithItsRef(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	first := commit(t, w.demo, "feat", w.base, "One\n", map[string]string{"a.txt": "l1\nl2\nm3\nn1\nn2\n"})
	head := commit(t, w.demo, "feat", first, "Two\n", map[string]string{"b.txt": "b1\nb2\n"})
	got := w.mustPost(t, map[string]any{"source": map[string]any{"branch": "feat"}, "intent": "two commits"})
	want := map[string]any{
		"number": 1.0, "ref": "demo#1", "version": 1.0, "head_sha": head, "base_sha": w.base,
		"stats":           map[string]any{"commits": 2.0, "files_changed": 2.0, "insertions": 5.0, "deletions": 1.0},
		"paths":           []any{"a.txt", "b.txt"},
		"paths_truncated": false,
	}
	for member, v := range want {
		if !reflect.DeepEqual(got[member], v) {
			t.Errorf("%s = %#v, want %#v", member, got[member], v)
		}
	}
	if ref := strings.TrimSpace(gitrepotest.Git(t, w.demo, "rev-parse", "refs/parley/changes/1/1")); ref != head {
		t.Errorf("refs/parley/changes/1/1 = %s, want the head %s", ref, head)
	}
	versions := w.versions(t, "demo", 1)
	if len(versions) != 1 {
		t.Fatalf("versions = %v, want one", versions)
	}
	v := versions[0]
	for member, want := range map[string]any{"number": 1.0, "kind": "ref", "patch_equivalent_to": nil, "head_sha": head,
		"base_sha": w.base, "session_id": nil, "stats": want["stats"]} {
		if !reflect.DeepEqual(v[member], want) {
			t.Errorf("version %s = %#v, want %#v", member, v[member], want)
		}
	}
	if by, _ := v["created_by"].(map[string]any); by["actor_type"] != "api_key" || by["actor_id"] != w.owner || by["username"] != "owner" {
		t.Errorf("created_by = %v, want the owner's API key", v["created_by"])
	}

	t.Run("more touched files than a change lists", func(t *testing.T) {
		files := map[string]string{}
		for i := range change.MaxPaths + 1 {
			files[fmt.Sprintf("many/f%04d.txt", i)] = "x\n"
		}
		commit(t, w.demo, "many", w.base, "Many\n", files)
		got := w.mustPost(t, map[string]any{"source": map[string]any{"branch": "many"}})
		if paths, _ := got["paths"].([]any); len(paths) != change.MaxPaths || got["paths_truncated"] != true {
			t.Errorf("paths has %d entries and paths_truncated is %v, want %d and true", len(paths), got["paths_truncated"], change.MaxPaths)
		}
	})
}

func TestOpen_NumbersEachWorkspacesChangesNeverGivingANumberTwice(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	other := w.addWorkspace(t, "other")
	for _, name := range []string{"b1", "b2"} {
		w.branch(t, name, name+"\n")
	}
	commit(t, other, "o1", "refs/heads/main", "o1\n", map[string]string{"o1.txt": "o\n"})
	commit(t, other, "o2", "refs/heads/main", "o2\n", map[string]string{"o2.txt": "o\n"})
	var demo, others []any
	refs := map[string]bool{}
	open := func(slug, branch string) {
		t.Helper()
		status, got := w.post(t, slug, w.ownerKey, map[string]any{"source": map[string]any{"branch": branch}})
		if status != http.StatusCreated {
			t.Fatalf("open %s:%s = %d %v", slug, branch, status, got)
		}
		if slug == "demo" {
			demo = append(demo, got["number"])
		} else {
			others = append(others, got["number"])
		}
		refs[fmt.Sprint(got["ref"])] = true
	}
	open("demo", "b1")
	open("other", "o1")
	open("demo", "b2")
	if status, got := w.post(t, "demo", w.ownerKey, map[string]any{"source": map[string]any{"branch": "nosuch"}}); status == http.StatusCreated {
		t.Fatalf("an open of a missing branch = 201 %v", got)
	}
	if err := w.Store.Tx(context.Background(), func(tx *sql.Tx) error {
		return change.AbandonChange(context.Background(), tx, "demo", 2, "abandoned", time.Now())
	}); err != nil {
		t.Fatal(err)
	}
	open("demo", "b2")
	open("other", "o2")
	if want := []any{1.0, 2.0, 3.0}; !reflect.DeepEqual(demo, want) {
		t.Errorf("demo's numbers = %v, want %v", demo, want)
	}
	if want := []any{1.0, 2.0}; !reflect.DeepEqual(others, want) {
		t.Errorf("other's numbers = %v, want %v", others, want)
	}
	for _, ref := range []string{"demo#1", "demo#2", "demo#3", "other#1", "other#2"} {
		if !refs[ref] {
			t.Errorf("no change answered the ref %s; refs = %v", ref, refs)
		}
	}
}

func TestOpen_WritesTheVersionEventAndTheOpenEventUnlessDraft(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	m1 := w.addFork(t, "demo--m1", "main", nil, nil)
	head := commit(t, m1, "task", w.base, "Task\n\nWhy.\n", map[string]string{"t.txt": "t\n"})
	w.branch(t, "feat", "Feat without a body\n")

	t.Run("open", func(t *testing.T) {
		seq := w.seq(t)
		got := w.mustPost(t, map[string]any{"source": map[string]any{"workspace_slug": "demo--m1", "branch": "task"}, "session_id": sessionOne})
		events := w.eventsAfter(t, seq)
		var types []string
		for _, e := range events {
			types = append(types, e.Type)
		}
		if want := []string{"parley.change.version", "parley.change.open"}; !reflect.DeepEqual(types, want) {
			t.Fatalf("event types = %v, want %v", types, want)
		}
		for _, e := range events {
			if e.ResourceType != "change" || e.ResourceID != "1" || e.Workspace == nil || *e.Workspace != "demo" ||
				e.SessionID == nil || *e.SessionID != sessionOne || e.ActorType != "api_key" || e.ActorID == nil || *e.ActorID != w.owner {
				t.Errorf("%s: %+v, want resource change 1 of demo in sessionOne by the owner's key", e.Type, e)
			}
		}
		common := map[string]any{"ref": "demo#1", "version": 1.0, "head_sha": head}
		version := map[string]any{"kind": "ref", "base_sha": w.base, "patch_equivalent_to": nil, "draft": false}
		opened := map[string]any{"title": "Task", "target_branch": "main", "source_workspace": "demo--m1",
			"source_branch": "task", "depends_on": nil, "was_draft": false}
		for i, extra := range []map[string]any{version, opened} {
			want := map[string]any{}
			for k, v := range common {
				want[k] = v
			}
			for k, v := range extra {
				want[k] = v
			}
			if got := metadata(t, events[i]); !reflect.DeepEqual(got, want) {
				t.Errorf("%s metadata = %v, want %v", events[i].Type, got, want)
			}
		}
		if got["session_id"] != sessionOne || !reflect.DeepEqual(got["sessions"], []any{sessionOne}) {
			t.Errorf("session_id = %v and sessions = %v, want sessionOne", got["session_id"], got["sessions"])
		}
		if want := []string{sessionOne + " demo 1/1"}; !reflect.DeepEqual(w.sessions.records, want) {
			t.Errorf("session links recorded = %v, want %v", w.sessions.records, want)
		}
	})
	t.Run("draft", func(t *testing.T) {
		seq := w.seq(t)
		status, got := w.post(t, "demo", w.ownerKey, map[string]any{"source": map[string]any{"branch": "feat"}, "draft": true})
		if status != http.StatusCreated || got["state"] != "draft" || got["intent"] != "" {
			t.Fatalf("POST a draft = %d with state %v and intent %q, want 201, draft and an empty intent", status, got["state"], got["intent"])
		}
		if want := map[string]any{"status": "blocked", "reason": "draft", "requirements": []any{}}; !reflect.DeepEqual(got["landing"], want) {
			t.Errorf("landing = %v, want %v", got["landing"], want)
		}
		events := w.eventsAfter(t, seq)
		if len(events) != 1 || events[0].Type != "parley.change.version" || metadata(t, events[0])["draft"] != true {
			t.Errorf("events = %+v, want one parley.change.version with draft true", events)
		}
	})
}

// steps records the collaborators' calls in their order.
type steps struct {
	mu    sync.Mutex
	order []string
}

func (s *steps) add(step string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.order = append(s.order, step)
}

// stack is a stack linker that records its calls and, for a depends_on,
// sets it and the base X on the change; or fails with err.
type stack struct {
	*steps
	err     error
	depends []*int64
}

const baseX = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func (s *stack) LinkVersion(ctx context.Context, tx *sql.Tx, ch change.Change, _ change.Version, dependsOn *int64) error {
	s.add("linker")
	s.depends = append(s.depends, dependsOn)
	if s.err != nil || dependsOn == nil {
		return s.err
	}
	_, err := tx.ExecContext(ctx, `UPDATE changes SET depends_on = ?, base_sha = ? WHERE workspace_slug = ? AND number = ?`,
		*dependsOn, baseX, ch.WorkspaceSlug, ch.Number)
	return err
}

func (s *stack) SetDependsOn(context.Context, *sql.Tx, change.Change, *int64) error { return nil }

// evaluator answers the landing it holds.
type evaluator struct {
	*steps
	landing change.Landing
}

func (e *evaluator) EvaluateLanding(context.Context, *sql.Tx, change.Change, change.Version) (change.Landing, error) {
	e.add("evaluator")
	return e.landing, nil
}

// observer records each version it is told of, with the landing of the
// change it reads in the transaction it is given; or fails with err.
type observer struct {
	*steps
	changes  *change.Component
	err      error
	versions []int64
	creds    []change.Credential
	landings []string
}

func (o *observer) VersionRecorded(ctx context.Context, tx *sql.Tx, ch change.Change, v change.Version, cred change.Credential) error {
	if o.steps != nil {
		o.add("observer")
	}
	if o.err != nil {
		return o.err
	}
	read, err := o.changes.ReadChange(ctx, tx, ch.WorkspaceSlug, ch.Number)
	if err != nil {
		return err
	}
	o.versions, o.creds, o.landings = append(o.versions, v.Number), append(o.creds, cred), append(o.landings, read.Landing.Status)
	return nil
}

func TestOpen_RunsTheCollaboratorsInOrderInTheCallersTransaction(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.branch(t, "plain", "Plain\n\nWhy.\n")
	w.branch(t, "linked", "Linked\n\nWhy.\n")

	t.Run("without collaborators", func(t *testing.T) {
		got := w.mustPost(t, map[string]any{"source": map[string]any{"branch": "plain"}})
		want := map[string]any{"depends_on": nil, "dependants": []any{}, "mergeable": "unknown", "conflict": nil,
			"needs_restack": false, "restack_reason": nil, "landed": nil, "reverts": nil, "reverted_by": nil,
			"landing": map[string]any{"status": "ready", "reason": nil, "requirements": []any{}}}
		for member, v := range want {
			if !reflect.DeepEqual(got[member], v) {
				t.Errorf("%s = %#v, want %#v", member, got[member], v)
			}
		}
	})
	t.Run("with a stack linker, an evaluator and an observer", func(t *testing.T) {
		order := &steps{}
		s := &stack{steps: order}
		o := &observer{steps: order, changes: w.changes}
		w.changes.RegisterStackLinker(s)
		w.changes.RegisterLandingEvaluator(&evaluator{steps: order, landing: change.Landing{Status: change.LandingPending,
			Requirements: []json.RawMessage{json.RawMessage(`{"name":"r"}`)}}})
		w.changes.AddVersionObserver(o)
		pat, patID := w.patFor(t, []string{"changes:write"}, nil)
		status, got := w.post(t, "demo", pat, map[string]any{"source": map[string]any{"branch": "linked"}, "depends_on": 7})
		if status != http.StatusCreated {
			t.Fatalf("POST = %d %v", status, got)
		}
		if want := []string{"linker", "evaluator", "observer"}; !reflect.DeepEqual(order.order, want) {
			t.Errorf("calls = %v, want %v", order.order, want)
		}
		if len(s.depends) != 1 || s.depends[0] == nil || *s.depends[0] != 7 {
			t.Errorf("the linker was given %v, want depends_on 7 once", s.depends)
		}
		wantCred := []change.Credential{{Type: change.CredentialPAT, ID: patID}}
		if !reflect.DeepEqual(o.versions, []int64{1}) || !reflect.DeepEqual(o.creds, wantCred) || !reflect.DeepEqual(o.landings, []string{"pending"}) {
			t.Errorf("the observer saw versions %v by %v with landings %v, want [1] by %v with [pending]", o.versions, o.creds, o.landings, wantCred)
		}
		landing := map[string]any{"status": "pending", "reason": nil, "requirements": []any{map[string]any{"name": "r"}}}
		if got["depends_on"] != 7.0 || got["base_sha"] != baseX || !reflect.DeepEqual(got["landing"], landing) {
			t.Errorf("depends_on = %v, base_sha = %v, landing = %v; want 7, %s and %v", got["depends_on"], got["base_sha"], got["landing"], baseX, landing)
		}
	})
}

func TestOpen_LeavesNothingWhenACollaboratorFailsOrTheCallerRollsBack(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.branch(t, "feat", "Feat\n\nWhy.\n")
	empty := map[string]int{"changes": 0, "change_versions": 0, "change_sessions": 0}
	seq := w.seq(t)
	assertNothing := func(t *testing.T) {
		t.Helper()
		if got := w.rows(t); !reflect.DeepEqual(got, empty) {
			t.Errorf("rows = %v, want none", got)
		}
		if events := w.eventsAfter(t, seq); len(events) != 0 {
			t.Errorf("events = %+v, want none", events)
		}
		if _, err := w.changes.ReadChange(context.Background(), nil, "demo", 1); !errors.Is(err, change.ErrChangeNotFound) {
			t.Errorf("ReadChange(demo, 1) = %v, want the change-not-found error", err)
		}
	}

	t.Run("the stack linker refuses", func(t *testing.T) {
		w.changes.RegisterStackLinker(&stack{steps: &steps{}, err: server.NewError("invalid_dependency", http.StatusBadRequest, "no change 9")})
		defer w.changes.RegisterStackLinker(nil)
		status, got := w.post(t, "demo", w.ownerKey, map[string]any{"source": map[string]any{"branch": "feat"}, "depends_on": 9})
		if errObj, _ := got["error"].(map[string]any); status != http.StatusBadRequest || errObj["error_type"] != "invalid_dependency" {
			t.Errorf("POST = %d %v, want 400 invalid_dependency", status, got)
		}
		assertNothing(t)
	})
	t.Run("the caller rolls back", func(t *testing.T) {
		err := w.Store.Tx(context.Background(), func(tx *sql.Tx) error {
			ch, err := w.changes.Open(context.Background(), tx, change.OpenInput{Workspace: "demo", SourceBranch: "feat",
				Caller: change.Caller{UserID: w.owner, Credential: change.Credential{Type: change.CredentialAPIKey, ID: "k1"}}, Kind: change.KindRef})
			if err != nil {
				return err
			}
			if ch.Number != 1 {
				t.Errorf("Open answered the number %d, want 1", ch.Number)
			}
			return errRollback
		})
		if !errors.Is(err, errRollback) {
			t.Fatalf("the transaction = %v, want the rollback", err)
		}
		assertNothing(t)
	})
	t.Run("an observer fails", func(t *testing.T) {
		w.changes.AddVersionObserver(&observer{err: errors.New("observer down")})
		status, got := w.post(t, "demo", w.ownerKey, map[string]any{"source": map[string]any{"branch": "feat"}})
		if errObj, _ := got["error"].(map[string]any); status != http.StatusInternalServerError || errObj["error_type"] != "internal" {
			t.Errorf("POST = %d %v, want 500 internal", status, got)
		}
		assertNothing(t)
	})
}

// guard records its calls and refuses with err, or accepts.
type guard struct {
	err   error
	calls []guardCall
}

type guardCall struct {
	caller  change.Caller
	version int64
	head    string
	value   bool
}

func (g *guard) CheckAutoLand(_ context.Context, _ *sql.Tx, caller change.Caller, ch change.Change, value bool) error {
	g.calls = append(g.calls, guardCall{caller, ch.Version, ch.HeadSHA, value})
	return g.err
}

func TestOpen_AsksTheAutoLandGuardBeforeStoringTheFlag(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	for _, name := range []string{"refused", "accepted", "absent", "off", "unguarded"} {
		commit(t, w.demo, name, w.base, name+"\n\nWhy.\n", map[string]string{"docs/" + name + ".md": name + "\n"})
	}
	pat, patID := w.patFor(t, []string{"changes:write"}, []string{"docs/**"})
	g := &guard{}
	w.changes.RegisterAutoLandGuard(g)
	open := func(branch string, autoLand any) (int, map[string]any) {
		body := map[string]any{"source": map[string]any{"branch": branch}}
		if autoLand != nil {
			body["auto_land"] = autoLand
		}
		return w.post(t, "demo", pat, body)
	}

	t.Run("the guard refuses", func(t *testing.T) {
		seq := w.seq(t)
		g.err = server.PathNotAllowed("auto_land: src/x.go is outside the token's paths").With("path", "src/x.go")
		defer func() { g.err = nil }()
		status, got := open("refused", true)
		if errObj, _ := got["error"].(map[string]any); status != http.StatusForbidden || errObj["error_type"] != "path_not_allowed" || errObj["path"] != "src/x.go" {
			t.Errorf("POST = %d %v, want 403 path_not_allowed naming src/x.go", status, got)
		}
		head := gitrepotest.Refs(t, w.demo, "refs/heads/")["refused"]
		want := []guardCall{{change.Caller{UserID: w.owner, Username: "owner", Credential: change.Credential{Type: change.CredentialPAT, ID: patID},
			Paths: []string{"docs/**"}}, 1, head, true}}
		if !reflect.DeepEqual(g.calls, want) {
			t.Errorf("guard calls = %+v, want %+v", g.calls, want)
		}
		if got := w.rows(t); !reflect.DeepEqual(got, map[string]int{"changes": 0, "change_versions": 0, "change_sessions": 0}) {
			t.Errorf("rows = %v, want none", got)
		}
		if events := w.eventsAfter(t, seq); len(events) != 0 {
			t.Errorf("events = %+v, want none", events)
		}
	})
	for _, c := range []struct {
		name, branch string
		autoLand     any
		unguarded    bool
		want         bool
		calls        int
	}{
		{"the guard accepts", "accepted", true, false, true, 1},
		{"auto_land absent", "absent", nil, false, false, 0},
		{"auto_land false", "off", false, false, false, 0},
		{"no guard registered", "unguarded", true, true, true, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			g.calls = nil
			if c.unguarded {
				w.changes.RegisterAutoLandGuard(nil)
			}
			status, got := open(c.branch, c.autoLand)
			if status != http.StatusCreated || got["auto_land"] != c.want {
				t.Errorf("POST = %d with auto_land %v, want 201 with %v", status, got["auto_land"], c.want)
			}
			if len(g.calls) != c.calls {
				t.Errorf("guard calls = %d, want %d", len(g.calls), c.calls)
			}
		})
	}
}

func TestOpen_RecordsARevertWithItsKindAndTheRevertedChange(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.branch(t, "feat", "Feat\n\nWhy.\n")
	w.branch(t, "revert/1", "Revert feat\n\nIt broke.\n")
	w.branch(t, "other", "Other\n\nWhy.\n")
	w.mustPost(t, map[string]any{"source": map[string]any{"branch": "feat"}})
	ctx := context.Background()
	if err := w.Store.Tx(ctx, func(tx *sql.Tx) error {
		return change.LandChange(ctx, tx, "demo", 1, change.Landed{SHA: w.base, At: time.Now(), By: change.Actor{ActorType: change.ActorSystem}, LandID: "l1"}, time.Now())
	}); err != nil {
		t.Fatal(err)
	}
	seq := w.seq(t)
	ch, err := w.open(t, change.OpenInput{Workspace: "demo", SourceBranch: "revert/1", TargetBranch: new("main"), Reverts: new(int64(1)),
		Kind: change.KindRevert, Caller: change.Caller{UserID: w.owner, Credential: change.Credential{Type: change.CredentialAPIKey, ID: "k1"}}})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if ch.Reverts == nil || *ch.Reverts != 1 || ch.RevertedBy != nil {
		t.Errorf("reverts = %v and reverted_by = %v, want 1 and null", ch.Reverts, ch.RevertedBy)
	}
	if v := w.versions(t, "demo", ch.Number); len(v) != 1 || v[0]["kind"] != "revert" {
		t.Errorf("versions = %v, want one of kind revert", v)
	}
	events := w.eventsAfter(t, seq)
	if len(events) == 0 || events[0].Type != "parley.change.version" || metadata(t, events[0])["kind"] != "revert" {
		t.Errorf("events = %+v, want parley.change.version of kind revert first", events)
	}
	if reverted, err := w.changes.ReadChange(ctx, nil, "demo", 1); err != nil || reverted.RevertedBy != nil {
		t.Errorf("ReadChange(demo, 1) = reverted_by %v, %v; want null", reverted.RevertedBy, err)
	}
	got := w.mustPost(t, map[string]any{"source": map[string]any{"branch": "other"}})
	if got["reverts"] != nil {
		t.Errorf("a change opened through the route has reverts %v, want null", got["reverts"])
	}
	if v := w.versions(t, "demo", int64(got["number"].(float64))); len(v) != 1 || v[0]["kind"] != "ref" {
		t.Errorf("versions = %v, want one of kind ref", v)
	}
}
