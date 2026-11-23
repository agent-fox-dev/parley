package scm_test

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/agent-fox-dev/parley/internal/gitrepo/gitrepotest"
	"github.com/agent-fox-dev/parley/internal/gitserver/pushrule"
	"github.com/agent-fox-dev/parley/internal/identity"
	"github.com/agent-fox-dev/parley/internal/scm"
)

// scriptedRules are rules that record every request they decide, with what
// the repository at dir held while they decided it, and answer decision,
// when it is set, to every update, else what the hub's engine answers.
type scriptedRules struct {
	scm.Rules
	dir string

	mu       sync.Mutex
	decision *pushrule.Decision
	seen     []ruleCall
}

// ruleCall is one request the rules decided: its caller and updates, the
// commit each update's ref was at while they decided ("" for none), and
// whether each update's after commit was in the repository then.
type ruleCall struct {
	caller  pushrule.Caller
	updates []pushrule.Update
	at      []string
	exists  []bool
}

func (s *scriptedRules) Evaluate(ctx context.Context, req pushrule.Request) pushrule.Verdict {
	call := ruleCall{caller: req.Caller, updates: slices.Clone(req.Updates)}
	for _, u := range req.Updates {
		at, _ := gitrepotest.Run(s.dir, "", "rev-parse", "-q", "--verify", u.Ref)
		_, err := gitrepotest.Run(s.dir, "", "cat-file", "-e", u.After+"^{commit}")
		call.at, call.exists = append(call.at, strings.TrimSpace(at)), append(call.exists, err == nil)
	}
	s.mu.Lock()
	s.seen = append(s.seen, call)
	scripted := s.decision
	s.mu.Unlock()
	if scripted == nil {
		return s.Rules.Evaluate(ctx, req)
	}
	v := pushrule.Verdict{}
	for _, u := range req.Updates {
		d := *scripted
		d.Update = u
		v.Decisions = append(v.Decisions, d)
	}
	return v
}

// script makes the rules answer d to every update, the hub's engine when d
// is nil, and forgets the requests they decided.
func (s *scriptedRules) script(d *pushrule.Decision) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.decision, s.seen = d, nil
}

// calls forgets the requests the rules decided and returns them.
func (s *scriptedRules) calls() []ruleCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	seen := s.seen
	s.seen = nil
	return seen
}

// errorMember is the member name of the error object of an error envelope.
func errorMember(r reply, name string) (any, bool) {
	e, _ := r.json["error"].(map[string]any)
	v, ok := e[name]
	return v, ok
}

// TestPushRules checks how every write route meets the push rules, on one
// workspace whose rules are scripted: rw, its main m1 <- m2 (refsStream),
// its branch work one commit on top of m1.
func TestPushRules(t *testing.T) {
	t.Parallel()
	rules := &scriptedRules{}
	w := newWriteWorld(t, "", func(d *scm.Deps) { rules.Rules, d.Rules = d.Rules, rules })
	dir := w.addWorkspace(t, "rw", w.org, refsStream)
	rules.dir = dir
	ids := strings.Fields(gitrepotest.Git(t, dir, "rev-parse", "main~1", "main"))
	m1, m2 := ids[0], ids[1]
	wk := commit(t, dir, m1, "work.txt", "work\n")
	gitrepotest.Git(t, dir, "update-ref", "refs/heads/work", wk)
	pat, patID := w.IssuePATID(t, identity.PATSpec{UserID: w.owner, Permissions: []string{"git:write"}})

	type request struct {
		method, path string
		body         map[string]any
	}
	routes := []struct {
		name string
		request
	}{
		{"a commit", request{http.MethodPost, "/commits", map[string]any{"branch": "work", "expected_head_sha": wk,
			"message": "m", "operations": []any{put("docs/a.md", "a\n")}}}},
		{"a ref creation", request{http.MethodPost, "/refs", map[string]any{"name": "refs/tags/v1", "sha": m2}}},
		{"a ref move", request{http.MethodPatch, "/refs/work", map[string]any{"sha": m2, "expected_sha": wk}}},
		{"a ref delete", request{http.MethodDelete, "/refs/work?expected_sha=" + wk, nil}},
		{"a rebase", request{http.MethodPost, "/rebase", map[string]any{"branch": "work", "onto": "main", "expected_head_sha": wk}}},
	}
	send := func(t *testing.T, rq request, header ...string) reply {
		t.Helper()
		return w.sendJSON(t, rq.method, scmPath("rw", rq.path), pat, rq.body, header...)
	}
	// unchanged checks that the request rq, refused, left the refs as refs
	// says, emitted nothing since seq and opened no change; a delete looks
	// up the change of its branch before the rules decide.
	unchanged := func(t *testing.T, rq request, refs string, seq int64) {
		t.Helper()
		if after := gitrepotest.Git(t, dir, "for-each-ref"); after != refs {
			t.Errorf("the refs changed from\n%s\nto\n%s", refs, after)
		}
		if types, _ := w.eventsAfter(t, seq); len(types) != 0 {
			t.Errorf("events %q, want none", types)
		}
		asked, opened := w.linker.calls()
		if len(opened) != 0 {
			t.Errorf("the change linker was asked to open %q, want nothing", opened)
		}
		if want := []string(nil); rq.method == http.MethodDelete {
			want = []string{"rw work"}
			if !slices.Equal(asked, want) {
				t.Errorf("the change linker was asked about %q, want %q", asked, want)
			}
		} else if len(asked) != 0 {
			t.Errorf("the change linker was asked about %q, want nothing", asked)
		}
	}

	t.Run("answers each rule's refusal with its message and type and changes nothing", func(t *testing.T) {
		refusals := []struct {
			rule, errorType, message, path string
		}{
			{"hub-owned refs", pushrule.ErrorTypeHubOwnedRef, "refs/parley/ is maintained by the hub and cannot be pushed to", ""},
			{"protected branches", pushrule.ErrorTypeProtectedBranch, "main is protected and cannot be force-pushed or deleted", ""},
			{"default branch", pushrule.ErrorTypeDefaultBranch, "main is the default branch and cannot be deleted", ""},
			{"tracking branches", pushrule.ErrorTypeTrackingBranch, "work tracks the parent workspace and is read-only in this fork; push a new branch", ""},
			{"carry-patch on the integration branch", pushrule.ErrorTypeIntegrationBranch, "work is the integration branch; push a patch branch", ""},
			{"carry-patch on a patch branch", pushrule.ErrorTypePatchBranch, "patch/one is a patch branch of the integration branch", ""},
			{"path-scoped tokens", pushrule.ErrorTypePathNotAllowed, "path src/b.go is outside this token's allowed paths", "src/b.go"},
		}
		for i, rf := range refusals {
			rules.script(&pushrule.Decision{Message: rf.message, ErrorType: rf.errorType, Path: rf.path})
			for j, rt := range routes {
				refs, seq := gitrepotest.Git(t, dir, "for-each-ref"), w.lastSeq(t)
				key := fmt.Sprintf("refused-%d-%d", i, j)
				for attempt := range 2 {
					r := send(t, rt.request, "Idempotency-Key", key)
					if r.status != http.StatusForbidden || r.errorType() != rf.errorType {
						t.Fatalf("%s refused by %s (attempt %d): %d %s, want 403 %s", rt.name, rf.rule, attempt+1, r.status, r.body, rf.errorType)
					}
					if msg, _ := errorMember(r, "message"); msg != rf.message {
						t.Errorf("%s refused by %s: message %q, want the rule's %q", rt.name, rf.rule, msg, rf.message)
					}
					path, found := errorMember(r, "path")
					switch {
					case rf.path != "" && path != rf.path:
						t.Errorf("%s refused by %s: error.path %v, want %q", rt.name, rf.rule, path, rf.path)
					case rf.path == "" && found:
						t.Errorf("%s refused by %s: error.path %v, want none", rt.name, rf.rule, path)
					}
					unchanged(t, rt.request, refs, seq)
				}
				// The retry under the same key was decided again, since a
				// refusal is not recorded for a replay.
				if n := len(rules.calls()); n != 2 {
					t.Errorf("%s refused by %s, sent twice under one Idempotency-Key: %d evaluations, want 2", rt.name, rf.rule, n)
				}
			}
		}
	})

	t.Run("answers a rule that could not be evaluated as an internal failure", func(t *testing.T) {
		const failure = "read the settings of rw: open /var/lib/parley/hub.db: input/output error"
		rules.script(&pushrule.Decision{Message: failure, ErrorType: pushrule.ErrorTypeInternal})
		for _, rt := range routes {
			refs, seq := gitrepotest.Git(t, dir, "for-each-ref"), w.lastSeq(t)
			r := send(t, rt.request)
			if r.status != http.StatusInternalServerError || r.errorType() != "internal" {
				t.Fatalf("%s whose rules failed: %d %s, want 500 internal", rt.name, r.status, r.body)
			}
			if msg, _ := errorMember(r, "message"); strings.Contains(fmt.Sprint(msg), "hub.db") || msg == failure {
				t.Errorf("%s whose rules failed: message %q tells the failure", rt.name, msg)
			}
			unchanged(t, rt.request, refs, seq)
		}
	})

	t.Run("asks the rules once per write, with the caller, before the ref moves", func(t *testing.T) {
		rules.script(nil)
		w.linker.calls()
		var work, n string
		for _, step := range []struct {
			name string
			request
			status int
			// want is the update the rules are asked about, from the
			// answer r.
			want func(r reply) pushrule.Update
		}{
			{"a commit on an existing branch", routes[0].request, http.StatusCreated, func(r reply) pushrule.Update {
				work, _ = answerCommit(r)["sha"].(string)
				return pushrule.Update{Ref: "refs/heads/work", Before: wk, After: work}
			}},
			{"a commit creating a branch", request{http.MethodPost, "/commits", map[string]any{"branch": "n", "base_ref": "main",
				"message": "n", "operations": []any{put("docs/n.md", "n\n")}}}, http.StatusCreated, func(r reply) pushrule.Update {
				n, _ = answerCommit(r)["sha"].(string)
				return pushrule.Update{Ref: "refs/heads/n", Before: pushrule.ZeroID, After: n}
			}},
			{"a tag creation", routes[1].request, http.StatusCreated, func(reply) pushrule.Update {
				return pushrule.Update{Ref: "refs/tags/v1", Before: pushrule.ZeroID, After: m2}
			}},
			{"a ref move", request{http.MethodPatch, "/refs/n", nil}, http.StatusOK, func(reply) pushrule.Update {
				return pushrule.Update{Ref: "refs/heads/n", Before: n, After: m1}
			}},
			{"a ref delete", request{http.MethodDelete, "/refs/n", nil}, http.StatusNoContent, func(reply) pushrule.Update {
				return pushrule.Update{Ref: "refs/heads/n", Before: m1, After: pushrule.ZeroID}
			}},
			{"a rebase", request{http.MethodPost, "/rebase", map[string]any{"branch": "work", "onto": "main"}}, http.StatusOK, func(r reply) pushrule.Update {
				head, _ := r.json["new_head_sha"].(string)
				return pushrule.Update{Ref: "refs/heads/work", Before: work, After: head}
			}},
		} {
			switch step.name {
			case "a ref move":
				step.body = map[string]any{"sha": m1, "expected_sha": n}
			case "a ref delete":
				step.path += "?expected_sha=" + m1
			}
			r := send(t, step.request)
			if r.status != step.status {
				t.Fatalf("%s: %d %s, want %d", step.name, r.status, r.body, step.status)
			}
			want := step.want(r)
			calls := rules.calls()
			if len(calls) != 1 {
				t.Fatalf("%s: the rules were asked %d times, want once", step.name, len(calls))
			}
			c := calls[0]
			if c.caller.CredentialType != pushrule.CredentialPAT || c.caller.CredentialID != patID || c.caller.UserID != w.owner {
				t.Errorf("%s: the rules were asked for %+v, want the owner's PAT %s", step.name, c.caller, patID)
			}
			if len(c.updates) != 1 || c.updates[0].Ref != want.Ref || c.updates[0].Before != want.Before || c.updates[0].After != want.After {
				t.Errorf("%s: the rules were asked about %+v, want %+v", step.name, c.updates, want)
				continue
			}
			wantAt := want.Before
			if wantAt == pushrule.ZeroID {
				wantAt = ""
			}
			if c.at[0] != wantAt {
				t.Errorf("%s: %s was at %q while the rules decided, want its before value %q", step.name, want.Ref, c.at[0], wantAt)
			}
			if want.After != pushrule.ZeroID && !c.exists[0] {
				t.Errorf("%s: the after commit %s was not in the repository while the rules decided", step.name, want.After)
			}
		}
	})
}

// TestPushRules_WithTheHubsEngine checks the refusals of the hub's own
// push rule engine that carry more than a message: the path a path-scoped
// token may not change, and the base branch of a fork's tracking branch.
func TestPushRules_WithTheHubsEngine(t *testing.T) {
	t.Parallel()
	w := newWorld(t)

	t.Run("holds a path-scoped token to its paths and names the first outside them", func(t *testing.T) {
		dir := w.addWorkspace(t, "paths", w.org, refsStream)
		head := strings.TrimSpace(gitrepotest.Git(t, dir, "rev-parse", "main"))
		gitrepotest.Git(t, dir, "update-ref", "refs/heads/work", head)
		pat := w.IssuePAT(t, identity.PATSpec{UserID: w.owner, Permissions: []string{"git:write"}, Paths: []string{"docs/**"}})
		path := scmPath("paths", "/commits")
		commitOn := func(head string, ops ...any) reply {
			return w.sendJSON(t, http.MethodPost, path, pat, map[string]any{"branch": "work", "expected_head_sha": head,
				"message": "m", "operations": ops})
		}

		refs := gitrepotest.Git(t, dir, "for-each-ref")
		r := commitOn(head, put("docs/a.md", "a\n"), put("src/b.go", "package b\n"))
		if p, _ := errorMember(r, "path"); r.status != http.StatusForbidden || r.errorType() != "path_not_allowed" || p != "src/b.go" {
			t.Errorf("a commit changing docs/a.md and src/b.go: %d %s, want 403 path_not_allowed with error.path src/b.go", r.status, r.body)
		}
		if after := gitrepotest.Git(t, dir, "for-each-ref"); after != refs {
			t.Errorf("the refused commit changed the refs from\n%s\nto\n%s", refs, after)
		}

		r = commitOn(head, put("docs/a.md", "a\n"))
		if r.status != http.StatusCreated {
			t.Fatalf("a commit changing docs/a.md alone: %d %s, want 201", r.status, r.body)
		}
		head, _ = answerCommit(r)["sha"].(string)

		r = commitOn(head, map[string]any{"op": "move", "from": "docs/guide.md", "to": "guide.md"})
		if p, _ := errorMember(r, "path"); r.status != http.StatusForbidden || r.errorType() != "path_not_allowed" || p != "guide.md" {
			t.Errorf("a commit moving docs/guide.md to guide.md: %d %s, want 403 path_not_allowed with error.path guide.md", r.status, r.body)
		}

		// A declaration operation changes its file as a put would.
		r = w.sendJSON(t, http.MethodPost, path, w.ownerKey, map[string]any{"branch": "work", "expected_head_sha": head,
			"message": "m", "operations": []any{put("src/b.go", "package b\n\nfunc F() {}\n")}})
		if r.status != http.StatusCreated {
			t.Fatalf("the owner's commit of src/b.go: %d %s, want 201", r.status, r.body)
		}
		head, _ = answerCommit(r)["sha"].(string)
		replace := map[string]any{"op": "replace_declaration", "path": "src/b.go", "name": "F", "content": "func F() { _ = 1 }\n"}
		refs = gitrepotest.Git(t, dir, "for-each-ref")
		r = commitOn(head, replace)
		if p, _ := errorMember(r, "path"); r.status != http.StatusForbidden || r.errorType() != "path_not_allowed" || p != "src/b.go" {
			t.Errorf("a replacement in src/b.go with a token of docs/**: %d %s, want 403 path_not_allowed with error.path src/b.go", r.status, r.body)
		}
		if after := gitrepotest.Git(t, dir, "for-each-ref"); after != refs {
			t.Errorf("the refused replacement changed the refs")
		}
		src := w.IssuePAT(t, identity.PATSpec{UserID: w.owner, Permissions: []string{"git:write"}, Paths: []string{"src/**"}})
		r = w.sendJSON(t, http.MethodPost, path, src, map[string]any{"branch": "work", "expected_head_sha": head, "message": "m",
			"operations": []any{replace}})
		if r.status != http.StatusCreated {
			t.Errorf("the same replacement with a token of src/**: %d %s, want 201", r.status, r.body)
		}
	})

	t.Run("keeps a fork's tracking branches read-only and names its base", func(t *testing.T) {
		parent := w.addWorkspace(t, "base", w.org, refsStream+"reset refs/heads/dev\nfrom :1\n\n")
		w.addFork(t, "base", w.org, []string{"main", "dev"}, nil)
		forkDir := w.workspaces.RepoPath("base--t1")
		ids := strings.Fields(gitrepotest.Git(t, parent, "rev-parse", "main", "dev"))
		main, dev := ids[0], ids[1]
		next := commit(t, forkDir, main, "README.md", "next\n")
		refs := gitrepotest.Git(t, forkDir, "for-each-ref")

		for _, rq := range []struct {
			name, method, path string
			body               map[string]any
		}{
			{"a commit to main", http.MethodPost, "/commits", map[string]any{"branch": "main", "expected_head_sha": main,
				"message": "m", "operations": []any{put("docs/a.md", "a\n")}}},
			{"a move of main", http.MethodPatch, "/refs/refs/heads/main", map[string]any{"sha": next, "expected_sha": main}},
			{"a delete of dev", http.MethodDelete, "/refs/refs/heads/dev?expected_sha=" + dev, nil},
		} {
			r := w.sendJSON(t, rq.method, scmPath("base--t1", rq.path), w.memberKey, rq.body)
			if base := errorDetails(r)["base_ref"]; r.status != http.StatusForbidden || r.errorType() != "tracking_branch" || base != "main" {
				t.Errorf("%s in the fork: %d %s, want 403 tracking_branch with details.base_ref main", rq.name, r.status, r.body)
			}
		}
		// main is protected too, and the protected-branch rule decides
		// before the tracking-branch rule, as for a push.
		r := w.sendJSON(t, http.MethodDelete, scmPath("base--t1", "/refs/refs/heads/main?expected_sha="+main), w.memberKey, nil)
		if r.status != http.StatusForbidden || r.errorType() != "protected_branch" {
			t.Errorf("a delete of main in the fork: %d %s, want 403 protected_branch", r.status, r.body)
		}
		if after := gitrepotest.Git(t, forkDir, "for-each-ref"); after != refs {
			t.Errorf("the refused writes changed the fork's refs from\n%s\nto\n%s", refs, after)
		}

		r = w.sendJSON(t, http.MethodPost, scmPath("base--t1", "/commits"), w.memberKey, map[string]any{"branch": "feat",
			"base_ref": "main", "message": "m", "operations": []any{put("docs/a.md", "a\n")}})
		if r.status != http.StatusCreated {
			t.Errorf("a commit to a new branch of the fork on main: %d %s, want 201", r.status, r.body)
		}
	})
}
