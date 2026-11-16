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
	"github.com/agent-fox-dev/parley/internal/identity"
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
	commit(t, m2, "refused", w.base, "Refused session\n\nWhy.\n", map[string]string{"r.txt": "r\n"})
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
		got := w.mustPost(t, map[string]any{"source": map[string]any{"branch": "many"}, "intent": "many files"})
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
		w.branch(t, name, name+"\n\nWhy.\n")
	}
	commit(t, other, "o1", "refs/heads/main", "o1\n\nWhy.\n", map[string]string{"o1.txt": "o\n"})
	commit(t, other, "o2", "refs/heads/main", "o2\n\nWhy.\n", map[string]string{"o2.txt": "o\n"})
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
		return w.changes.SetAbandoned(context.Background(), tx, "demo", 2, "abandoned")
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
		// The mergeability check, which mustPost waits for, announces the
		// clean change after the open.
		events := w.eventsAfter(t, seq)
		var types []string
		for _, e := range events {
			types = append(types, e.Type)
		}
		if want := []string{"parley.change.version", "parley.change.open", change.EventReady}; !reflect.DeepEqual(types, want) {
			t.Fatalf("event types = %v, want %v", types, want)
		}
		for _, e := range events[:2] {
			if e.ResourceType != "change" || e.ResourceID != "1" || e.Workspace == nil || *e.Workspace != "demo" ||
				e.SessionID == nil || *e.SessionID != sessionOne || e.ActorType != "api_key" || e.ActorID == nil || *e.ActorID != w.owner {
				t.Errorf("%s: %+v, want resource change 1 of demo in sessionOne by the owner's key", e.Type, e)
			}
		}
		common := map[string]any{"number": 1.0, "ref": "demo#1", "version": 1.0, "head_sha": head}
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

// evaluator answers the landing it holds, blocked for the cause it is
// given with the same requirements, and records each cause ("" for none).
type evaluator struct {
	*steps
	landing change.Landing
	mu      sync.Mutex
	causes  []string
}

func (e *evaluator) EvaluateLanding(_ context.Context, _ *sql.Tx, _ change.Change, _ change.Version, cause *string) (change.Landing, error) {
	e.add("evaluator")
	e.mu.Lock()
	defer e.mu.Unlock()
	e.causes = append(e.causes, deref(cause))
	if cause != nil {
		return change.Landing{Status: change.LandingBlocked, Reason: cause, Requirements: e.landing.Requirements}, nil
	}
	return e.landing, nil
}

// asked are the causes the evaluator was asked with, from the n-th call on.
func (e *evaluator) asked(n int) []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.causes[min(n, len(e.causes)):]...)
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
	noRefs := func(t *testing.T) {
		t.Helper()
		if refs := gitrepotest.Refs(t, w.demo, "refs/parley/changes/"); len(refs) != 0 {
			t.Errorf("version refs = %v, want none", refs)
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
		noRefs(t)
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
		// The version ref may stay: it names no stored version.
		assertNothing(t)
	})
	t.Run("an observer fails", func(t *testing.T) {
		w.changes.AddVersionObserver(&observer{err: errors.New("observer down")})
		status, got := w.post(t, "demo", w.ownerKey, map[string]any{"source": map[string]any{"branch": "feat"}})
		if errObj, _ := got["error"].(map[string]any); status != http.StatusInternalServerError || errObj["error_type"] != "internal" {
			t.Errorf("POST = %d %v, want 500 internal", status, got)
		}
		assertNothing(t)
		noRefs(t)
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
		before := w.snapshot(t)
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
		if after := w.snapshot(t); !reflect.DeepEqual(after, before) {
			t.Errorf("a refused open wrote %+v, had %+v", after, before)
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
		return w.changes.SetLanded(ctx, tx, "demo", 1, change.Landed{SHA: w.base, At: time.Now(), By: change.Actor{ActorType: change.ActorSystem}, LandID: "l1"})
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

// src is a body naming the branch of the source workspace ws ("" for the
// target).
func src(ws, branch string) map[string]any {
	s := map[string]any{"branch": branch}
	if ws != "" {
		s["workspace_slug"] = ws
	}
	return map[string]any{"source": s}
}

// The member m reads demo and owns the fork demo--m1; the fork
// demo--other is the owner's, which m reads as a member.
func TestOpen_LetsOnlyACallerWhoWritesTheSourceOpen(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	m, mKey := w.member(t, "m")
	stranger := w.CreateUser(t, "stranger")
	strangerKey := w.IssueKey(t, stranger)
	m1 := w.addFork(t, "demo--m1", "main", nil, nil)
	w.set(t, "demo--m1", "owner_id", m)
	other := w.addFork(t, "demo--other", "main", nil, nil)
	w.branch(t, "feat", "Feat\n\nWhy.\n")
	commit(t, other, "feat", w.base, "Other\n\nWhy.\n", map[string]string{"o.txt": "o\n"})
	commit(t, m1, "task", w.base, "Task\n\nWhy.\n", map[string]string{"t.txt": "t\n"})
	commit(t, m1, "task2", w.base, "Task 2\n\nWhy.\n", map[string]string{"t2.txt": "t\n"})
	readPAT, _ := w.patFor(t, []string{"changes:read"}, nil)
	now := time.Now().UTC()
	forkBound := w.IssuePAT(t, identity.PATSpec{UserID: m, Permissions: []string{"changes:write"}, Workspace: "demo--m1", CreatedAt: now})
	demoBound := w.IssuePAT(t, identity.PATSpec{UserID: w.owner, Permissions: []string{"changes:write"}, Workspace: "demo", CreatedAt: now})

	for _, c := range []struct {
		name, credential string
		body             map[string]any
		status           int
		errorType        string
	}{
		{"a token without changes:write", readPAT, src("", "feat"), http.StatusForbidden, "insufficient_scope"},
		{"a user without access to the target", strangerKey, src("", "feat"), http.StatusNotFound, "not_found"},
		{"a member naming a fork they only read", mKey, src("demo--other", "feat"), http.StatusForbidden, "forbidden"},
		{"a member naming the target they only read", mKey, src("demo", "feat"), http.StatusForbidden, "forbidden"},
		{"a fork-bound token naming no source workspace", forkBound, src("", "task"), http.StatusForbidden, "token_bound"},
		{"a fork-bound token naming another fork", forkBound, src("demo--other", "feat"), http.StatusForbidden, "token_bound"},
		{"a repository-bound token naming a fork it did not create", demoBound, src("demo--other", "feat"), http.StatusBadRequest, "source_not_related"},
	} {
		t.Run(c.name, func(t *testing.T) {
			w.refused(t, "demo", c.credential, c.body, c.status, c.errorType)
		})
	}
	t.Run("the owner of the workspace", func(t *testing.T) {
		got := w.mustPost(t, src("", "feat"))
		want := map[string]any{"workspace_slug": "demo", "number": 1.0, "ref": "demo#1", "state": "open", "version": 1.0,
			"source": map[string]any{"workspace_slug": "demo", "branch": "feat"},
			"author": map[string]any{"actor_type": "api_key", "actor_id": w.owner, "username": "owner"}}
		for member, v := range want {
			if !reflect.DeepEqual(got[member], v) {
				t.Errorf("%s = %#v, want %#v", member, got[member], v)
			}
		}
	})
	for _, c := range []struct{ name, credential, branch string }{
		{"a member from their own fork", mKey, "task"},
		{"a fork-bound token from its fork", forkBound, "task2"},
	} {
		t.Run(c.name, func(t *testing.T) {
			status, got := w.post(t, "demo", c.credential, src("demo--m1", c.branch))
			source, _ := got["source"].(map[string]any)
			if status != http.StatusCreated || got["workspace_slug"] != "demo" || source["workspace_slug"] != "demo--m1" || got["target_branch"] != "main" {
				t.Errorf("POST = %d %v, want 201 in demo from demo--m1 onto main", status, got)
			}
		})
	}
}

func TestOpen_RefusesAWorkspaceThatIsNotReady(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	m1 := w.addFork(t, "demo--m1", "main", nil, nil)
	commit(t, m1, "task", w.base, "Task\n\nWhy.\n", map[string]string{"t.txt": "t\n"})
	for _, slug := range []string{"demo", "demo--m1"} {
		for _, c := range []struct {
			column, value, errorType string
		}{
			{"status", "pending", "workspace_busy"},
			{"status", "cloning", "workspace_busy"},
			{"maintenance", "reclone", "workspace_busy"},
			{"status", "failed", "workspace_not_ready"},
			{"status", "archived", "workspace_archived"},
		} {
			t.Run(slug+" "+c.value, func(t *testing.T) {
				w.set(t, slug, c.column, c.value)
				defer w.set(t, slug, "status", "ready")
				defer w.set(t, slug, "maintenance", "")
				w.refused(t, "demo", w.ownerKey, src("demo--m1", "task"), http.StatusConflict, c.errorType)
			})
		}
	}
	w.mustPost(t, src("demo--m1", "task"))
}

func TestOpen_RefusesAnInvalidBody(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	for _, name := range []string{"feat", "twenty", "long", "twice", "session"} {
		w.branch(t, name, name+"\n\nWhy.\n")
	}
	labels := func(n int) []any {
		out := []any{}
		for i := range n {
			out = append(out, fmt.Sprintf("l%d", i))
		}
		return out
	}
	with := func(member string, value any) map[string]any {
		body := src("", "feat")
		body[member] = value
		return body
	}
	for _, c := range []struct {
		name      string
		body      any
		errorType string
	}{
		{"an empty body", map[string]any{}, "invalid_request"},
		{"a source without a branch", map[string]any{"source": map[string]any{}}, "invalid_request"},
		{"an empty branch", src("", ""), "invalid_request"},
		{"a member of the wrong type", with("draft", "yes"), "invalid_request"},
		{"more than twenty labels", with("labels", labels(21)), "invalid_request"},
		{"an upper-case label", with("labels", []any{"Upper"}), "invalid_request"},
		{"a label starting with a dash", with("labels", []any{"-x"}), "invalid_request"},
		{"a label of 64 characters", with("labels", []any{strings.Repeat("a", 64)}), "invalid_request"},
		{"depends_on zero", with("depends_on", 0), "invalid_request"},
		{"depends_on negative", with("depends_on", -1), "invalid_request"},
		{"depends_on a string", with("depends_on", "1"), "invalid_request"},
		{"a session the linker refuses", with("session_id", sessionOther), "unknown_session"},
	} {
		t.Run(c.name, func(t *testing.T) {
			w.refused(t, "demo", w.ownerKey, c.body, http.StatusBadRequest, c.errorType)
		})
	}
	for _, c := range []struct {
		name, branch, member string
		value                any
		want                 map[string]any
	}{
		{"twenty labels", "twenty", "labels", labels(20), map[string]any{"labels": labels(20)}},
		{"a label of 63 characters", "long", "labels", []any{strings.Repeat("a", 63)}, map[string]any{"labels": []any{strings.Repeat("a", 63)}}},
		{"a repeated label", "twice", "labels", []any{"a", "a"}, map[string]any{"labels": []any{"a"}}},
		{"a session the linker accepts", "session", "session_id", sessionOne, map[string]any{"session_id": sessionOne, "sessions": []any{sessionOne}}},
	} {
		t.Run(c.name, func(t *testing.T) {
			body := src("", c.branch)
			body[c.member] = c.value
			got := w.mustPost(t, body)
			for member, v := range c.want {
				if !reflect.DeepEqual(got[member], v) {
					t.Errorf("%s = %#v, want %#v", member, got[member], v)
				}
			}
		})
	}
}

// demo has merged, whose history ahead of main holds a merge commit, and
// behind, at main's commit; elsewhere is the owner's unrelated workspace,
// hidden one the owner cannot see, and cp a carry-patch workspace whose
// integration branch is integration and whose release is protected.
func TestOpen_AnswersTheFirstFailingCheckOfTheSourceAndTarget(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.addWorkspace(t, "elsewhere")
	w.addWorkspace(t, "hidden")
	stranger := w.CreateUser(t, "stranger")
	w.set(t, "hidden", "owner_id", stranger)
	w.set(t, "hidden", "org_id", w.CreateOrg(t, "strangers"))
	cp := w.addWorkspace(t, "cp")
	w.set(t, "cp", "workspace_mode", "carry_patch")
	w.set(t, "cp", "integration_branch", "integration")
	w.set(t, "cp", "settings", `{"protected_branches":["release"]}`)
	commit(t, cp, "integration", "refs/heads/main", "Upstream plus patches\n", map[string]string{"i.txt": "i\n"})
	commit(t, cp, "patch", "refs/heads/main", "Patch\n\nWhy.\n", map[string]string{"p.txt": "p\n"})
	side := commit(t, w.demo, "side", w.base, "Side\n", map[string]string{"s.txt": "s\n"})
	first := commit(t, w.demo, "merged", w.base, "First\n", map[string]string{"f.txt": "f\n"})
	if _, err := gitrepotest.Run(w.demo, "commit refs/heads/merged\n"+committer+data("Merge side\n\nWhy.\n")+
		"from "+first+"\nmerge "+side+"\n\n", "fast-import", "--quiet"); err != nil {
		t.Fatal(err)
	}
	gitrepotest.Git(t, w.demo, "update-ref", "refs/heads/behind", w.base)
	commit(t, w.demo, "orphan", "", "Orphan\n\nWhy.\n", map[string]string{"o.txt": "o\n"})
	w.branch(t, "feat", "Feat\n\nWhy.\n")

	body := func(ws, branch, target string) map[string]any {
		b := src(ws, branch)
		if target != "" {
			b["target_branch"] = target
		}
		return b
	}
	for _, c := range []struct {
		name, slug string
		body       map[string]any
		status     int
		errorType  string
	}{
		{"the target branch is missing", "demo", body("", "feat", "nope"), http.StatusBadRequest, "target_not_found"},
		{"the target is the integration branch", "cp", body("", "patch", "integration"), http.StatusForbidden, "integration_branch"},
		{"the source workspace is unknown", "demo", body("ghost", "feat", ""), http.StatusBadRequest, "source_not_related"},
		{"the source workspace is invisible", "demo", body("hidden", "main", ""), http.StatusBadRequest, "source_not_related"},
		{"the source workspace is unrelated", "demo", body("elsewhere", "main", ""), http.StatusBadRequest, "source_not_related"},
		{"the source branch is missing", "demo", body("", "nope", ""), http.StatusBadRequest, "source_not_found"},
		{"source and target are one branch", "demo", body("", "main", "main"), http.StatusBadRequest, "invalid_request"},
		{"nothing to land", "demo", body("", "behind", ""), http.StatusBadRequest, "nothing_to_land"},
		{"no history in common with the target", "demo", body("", "orphan", ""), http.StatusBadRequest, "unrelated_history"},
		{"a merge commit ahead", "demo", body("", "merged", ""), http.StatusBadRequest, "merge_commits_unsupported"},
		{"no target branch and no source branch", "demo", body("", "nope", "nope"), http.StatusBadRequest, "target_not_found"},
		{"an unrelated source without the branch", "demo", body("elsewhere", "nope", ""), http.StatusBadRequest, "source_not_related"},
	} {
		t.Run(c.name, func(t *testing.T) {
			w.refused(t, c.slug, w.ownerKey, c.body, c.status, c.errorType)
		})
	}
	t.Run("a protected patch branch of a carry-patch workspace", func(t *testing.T) {
		if status, got := w.post(t, "cp", w.ownerKey, body("", "patch", "release")); status != http.StatusCreated {
			t.Errorf("POST = %d %v, want 201", status, got)
		}
	})
}

func TestOpen_ChecksTheTitleAndTheIntent(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	lax := w.addWorkspace(t, "lax")
	w.set(t, "lax", "settings", `{"changes.require_intent":false}`)
	commit(t, lax, "bare", "refs/heads/main", "Bare\n", map[string]string{"b.txt": "b\n"})
	w.branch(t, "feat", "Feat\n\nWhy.\n")
	w.branch(t, "bare", "Bare\n")
	w.branch(t, "draft", "Draft\n")
	w.branch(t, "full", "Full\n")
	commit(t, w.demo, "nosubject", w.base, "\nOnly a body.\n", map[string]string{"n.txt": "n\n"})
	subject := strings.Repeat("s", 250)
	commit(t, w.demo, "longsubject", w.base, subject+"\n\nWhy.\n", map[string]string{"l.txt": "l\n"})

	with := func(branch string, members ...any) map[string]any {
		body := src("", branch)
		for i := 0; i < len(members); i += 2 {
			body[members[i].(string)] = members[i+1]
		}
		return body
	}
	for _, c := range []struct {
		name      string
		body      map[string]any
		errorType string
	}{
		{"an empty title", with("feat", "title", ""), "invalid_request"},
		{"a title of 201 characters", with("feat", "title", strings.Repeat("t", 201)), "invalid_request"},
		{"a title with a line break", with("feat", "title", "a\nb"), "invalid_request"},
		{"a head commit without a subject", with("nosubject"), "invalid_request"},
		{"an intent of 16,001 characters", with("feat", "intent", strings.Repeat("i", 16001)), "invalid_request"},
		{"no intent and a commit without a body", with("bare"), "intent_required"},
		{"an intent of white space", with("feat", "intent", "  \n"), "intent_required"},
	} {
		t.Run(c.name, func(t *testing.T) {
			w.refused(t, "demo", w.ownerKey, c.body, http.StatusBadRequest, c.errorType)
		})
	}
	for _, c := range []struct {
		name, slug string
		body       map[string]any
		want       map[string]any
	}{
		{"the longest title and intent", "demo", with("full", "title", strings.Repeat("t", 200), "intent", strings.Repeat("i", 16000)),
			map[string]any{"title": strings.Repeat("t", 200)}},
		{"a draft without an intent", "demo", with("draft", "draft", true), map[string]any{"intent": "", "state": "draft"}},
		{"no intent where none is required", "lax", with("bare"), map[string]any{"intent": ""}},
		{"a long subject", "demo", with("longsubject"), map[string]any{"title": subject[:change.MaxTitle]}},
	} {
		t.Run(c.name, func(t *testing.T) {
			status, got := w.post(t, c.slug, w.ownerKey, c.body)
			if status != http.StatusCreated {
				t.Fatalf("POST = %d %v, want 201", status, got)
			}
			for member, v := range c.want {
				if got[member] != v {
					t.Errorf("%s = %.40q, want %.40q", member, got[member], v)
				}
			}
		})
	}
}

func TestOpen_RefusesASecondChangeOfTheSameSource(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	for _, name := range []string{"feat", "drafted", "race"} {
		w.branch(t, name, name+"\n\nWhy.\n")
	}
	w.mustPost(t, src("", "feat"))
	w.mustPost(t, map[string]any{"source": map[string]any{"branch": "drafted"}, "draft": true})
	for _, c := range []struct {
		branch string
		number float64
	}{{"feat", 1}, {"drafted", 2}} {
		t.Run(c.branch, func(t *testing.T) {
			errObj := w.refused(t, "demo", w.ownerKey, src("", c.branch), http.StatusConflict, "change_exists")
			want := map[string]any{"workspace_slug": "demo", "number": c.number, "ref": fmt.Sprintf("demo#%v", c.number)}
			if !reflect.DeepEqual(errObj["details"], want) {
				t.Errorf("details = %v, want %v", errObj["details"], want)
			}
			if msg, _ := errObj["message"].(string); !strings.HasSuffix(msg, " of "+want["ref"].(string)) {
				t.Errorf("message = %q, want it to name %v", msg, want["ref"])
			}
		})
	}
	t.Run("after the change is abandoned", func(t *testing.T) {
		if err := w.Store.Tx(context.Background(), func(tx *sql.Tx) error {
			return w.changes.SetAbandoned(context.Background(), tx, "demo", 1, "abandoned")
		}); err != nil {
			t.Fatal(err)
		}
		if got := w.mustPost(t, src("", "feat")); got["number"] != 3.0 {
			t.Errorf("number = %v, want 3", got["number"])
		}
	})
	t.Run("concurrent opens", func(t *testing.T) {
		var wg sync.WaitGroup
		statuses := make([]int, 8)
		types := make([]any, 8)
		for i := range statuses {
			wg.Go(func() {
				status, raw := w.Call(t, http.MethodPost, "/api/v1/workspaces/demo/changes", w.ownerKey, src("", "race"))
				var out struct{ Error map[string]any }
				_ = json.Unmarshal(raw, &out)
				statuses[i], types[i] = status, out.Error["error_type"]
			})
		}
		wg.Wait()
		created := 0
		for i, status := range statuses {
			switch {
			case status == http.StatusCreated:
				created++
			case status != http.StatusConflict || types[i] != "change_exists":
				t.Errorf("an open answered %d %v, want 201 or 409 change_exists", status, types[i])
			}
		}
		var changes, versions int
		if err := w.Store.Main().QueryRow(`SELECT COUNT(*), (SELECT COUNT(*) FROM change_versions v WHERE v.workspace_slug = c.workspace_slug AND v.number = c.number)
			FROM changes c WHERE source_branch = 'race'`).Scan(&changes, &versions); err != nil {
			t.Fatal(err)
		}
		if created != 1 || changes != 1 || versions != 1 {
			t.Errorf("%d opens created, %d changes and %d versions stored, want one of each", created, changes, versions)
		}
	})
}

// The token may write docs/**; earlier was committed by someone else.
func TestOpen_RefusesAFileOutsideAPathScopedToken(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	commit(t, w.demo, "docs", w.base, "Docs\n\nWhy.\n", map[string]string{"docs/a.md": "a\n"})
	commit(t, w.demo, "mixed", w.base, "Mixed\n\nWhy.\n", map[string]string{"docs/a.md": "a\n", "src/x.go": "x\n", "src/y.go": "y\n"})
	commit(t, w.demo, "earlier", w.base, "Earlier\n\nWhy.\n", map[string]string{"src/x.go": "x\n"})
	pat, _ := w.patFor(t, []string{"changes:write"}, []string{"docs/**"})
	for _, branch := range []string{"mixed", "earlier"} {
		t.Run(branch, func(t *testing.T) {
			errObj := w.refused(t, "demo", pat, src("", branch), http.StatusForbidden, "path_not_allowed")
			if errObj["path"] != "src/x.go" {
				t.Errorf("error.path = %v, want src/x.go", errObj["path"])
			}
		})
	}
	t.Run("docs only", func(t *testing.T) {
		if status, got := w.post(t, "demo", pat, src("", "docs")); status != http.StatusCreated {
			t.Errorf("POST = %d %v, want 201", status, got)
		}
	})
}
