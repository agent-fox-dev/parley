package scm_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/event"
	"github.com/agent-fox-dev/parley/internal/gitrepo/gitrepotest"
	"github.com/agent-fox-dev/parley/internal/gitserver/pushrule"
	"github.com/agent-fox-dev/parley/internal/identity"
	"github.com/agent-fox-dev/parley/internal/scm"
	"github.com/agent-fox-dev/parley/internal/server"
)

// sessionID is the agent session the requests that name one name; the
// fake session linker accepts it.
const sessionID = "6f1c2b9e-3d4a-4b5c-8d7e-9f0a1b2c3d4e"

// eventSCMCommit is the type of the event of a commit made through the
// source control API.
const eventSCMCommit = "parley.scm.commit"

// scmCommitEvent is the metadata of a parley.scm.commit event.
type scmCommitEvent struct {
	Branch    string   `json:"branch"`
	SHA       string   `json:"sha"`
	ParentSHA string   `json:"parent_sha"`
	Files     []string `json:"files"`
	Truncated bool     `json:"truncated"`
}

// eventTypes are the types of recs.
func eventTypes(recs []event.Record) []string {
	types := []string{}
	for _, r := range recs {
		types = append(types, r.Type)
	}
	return types
}

// metadata decodes the metadata of rec into v.
func metadata(t *testing.T, rec event.Record, v any) {
	t.Helper()
	if err := json.Unmarshal(rec.Metadata, v); err != nil {
		t.Fatalf("metadata of %s: %v", rec.Type, err)
	}
}

// revParse is the object the revision rev of the repository at dir names,
// "" for none; it reports no failure, so that it may run where a hub
// collaborator runs.
func revParse(dir, rev string) string {
	out, _ := gitrepotest.Run(dir, "", "rev-parse", "-q", "--verify", rev)
	return strings.TrimSpace(out)
}

// answerChange is the change member of an answer, as a map, nil when null.
func answerChange(r reply) map[string]any {
	c, _ := r.json["change"].(map[string]any)
	return c
}

// TestCommitConsequences checks what an accepted commit leaves besides its
// commit, on one workspace and its fork: the events, the change it names
// or opens, the replay of an Idempotency-Key and the post-push listeners.
func TestCommitConsequences(t *testing.T) {
	t.Parallel()
	w := newCommitWorld(t, "")
	pat, patID := w.IssuePATID(t, identity.PATSpec{UserID: w.owner, Permissions: []string{"git:write"}})
	post := func(t *testing.T, cred, slug string, body map[string]any, header ...string) reply {
		t.Helper()
		return w.sendJSON(t, http.MethodPost, scmPath(slug, "/commits"), cred, body, header...)
	}
	created := func(t *testing.T, name string, r reply) string {
		t.Helper()
		sha, _ := answerCommit(r)["sha"].(string)
		if r.status != http.StatusCreated || len(sha) != 40 {
			t.Fatalf("%s: %d %s, want 201 with a commit", name, r.status, r.body)
		}
		return sha
	}

	t.Run("emits a push and a commit event for the moved branch", func(t *testing.T) {
		old := w.branch(t, "work")
		seq := w.lastSeq(t)
		sha := created(t, "a commit to work", post(t, w.ownerKey, "cm", map[string]any{"branch": "work", "expected_head_sha": old,
			"message": "m", "operations": []any{
				map[string]any{"op": "move", "from": "api/old.go", "to": "api/new.go"},
				put("README.md", "changed\n"),
				map[string]any{"op": "delete", "path": "docs/guide.md"},
			}}))
		recs := w.recordsAfter(t, seq)
		if types := eventTypes(recs); !slices.Equal(types, []string{pushrule.EventPush, eventSCMCommit}) {
			t.Fatalf("events %q, want [%s %s]", types, pushrule.EventPush, eventSCMCommit)
		}
		var push pushEvent
		metadata(t, recs[0], &push)
		if push.Ref != "refs/heads/work" || push.Before == nil || *push.Before != old || push.After == nil || *push.After != sha ||
			push.Created || push.Deleted || push.Forced || push.Direct || push.Via != pushrule.ViaAPI {
			t.Errorf("push metadata %+v, want work from %s to %s, no flag set, via api", push, old, sha)
		}
		var cm scmCommitEvent
		metadata(t, recs[1], &cm)
		want := scmCommitEvent{Branch: "work", SHA: sha, ParentSHA: old,
			Files: []string{"README.md", "api/new.go", "api/old.go", "docs/guide.md"}}
		if fmt.Sprint(cm) != fmt.Sprint(want) {
			t.Errorf("commit metadata %+v, want %+v", cm, want)
		}
		rec := recs[1]
		if rec.Workspace == nil || *rec.Workspace != "cm" || rec.ActorType != event.ActorAPIKey || rec.ActorID == nil ||
			*rec.ActorID != w.owner || rec.ResourceType != "commit" || rec.ResourceID != sha {
			t.Errorf("commit event %+v, want workspace cm, actor api_key %s, resource commit %s", rec, w.owner, sha)
		}
		for _, rec := range recs {
			if rec.SessionID != nil {
				t.Errorf("%s has session %q, want none", rec.Type, *rec.SessionID)
			}
		}

		seq = w.lastSeq(t)
		sha = created(t, "a commit creating n", post(t, w.ownerKey, "cm", map[string]any{"branch": "n", "base_ref": "main",
			"message": "m", "operations": []any{put("n.txt", "n\n")}}))
		recs = w.recordsAfter(t, seq)
		if types := eventTypes(recs); !slices.Equal(types, []string{pushrule.EventPush, eventSCMCommit}) {
			t.Fatalf("events of a created branch %q", types)
		}
		metadata(t, recs[0], &push)
		metadata(t, recs[1], &cm)
		if push.Before != nil || !push.Created || push.Ref != "refs/heads/n" {
			t.Errorf("push metadata of a created branch %+v, want before null and created", push)
		}
		if cm.Branch != "n" || cm.SHA != sha || cm.ParentSHA != w.m2 || !slices.Equal(cm.Files, []string{"n.txt"}) {
			t.Errorf("commit metadata of a created branch %+v, want branch n at %s on %s with [n.txt]", cm, sha, w.m2)
		}

		ops, paths := []any{}, []string{}
		for i := range 150 {
			p := "many/" + strconv.Itoa(i) + ".txt"
			ops, paths = append(ops, put(p, "x\n")), append(paths, p)
		}
		slices.Sort(paths)
		seq = w.lastSeq(t)
		created(t, "a commit of 150 files", post(t, w.ownerKey, "cm", map[string]any{"branch": "many", "base_ref": "main",
			"message": "m", "operations": ops}))
		recs = w.recordsAfter(t, seq)
		cm = scmCommitEvent{}
		metadata(t, recs[len(recs)-1], &cm)
		if !slices.Equal(cm.Files, paths[:100]) || !cm.Truncated {
			t.Errorf("a commit of 150 files lists %d files (truncated %v), want the first 100 in order and truncated", len(cm.Files), cm.Truncated)
		}
	})

	t.Run("names the version the ref writer reports for the moved branch", func(t *testing.T) {
		old := w.branch(t, "work")
		w.stage.script(map[string]*pushrule.Version{"refs/heads/work": {WorkspaceSlug: "w", Number: 12, Version: 4}},
			func(pushrule.StageInput) string { return revParse(w.dir, "refs/heads/work") })
		t.Cleanup(func() { w.stage.script(nil, nil) })
		r := post(t, pat, "cm", map[string]any{"branch": "work", "expected_head_sha": old, "message": "m",
			"operations": []any{put("v.txt", "v\n")}, "session_id": sessionID})
		sha := created(t, "a commit to a change's source", r)
		if c := answerChange(r); fmt.Sprint(c) != fmt.Sprint(map[string]any{"workspace_slug": "w", "number": 12.0, "version": 4.0}) {
			t.Errorf("change %v, want {w 12 4}", r.json["change"])
		}
		if v, ok := r.json["change_error"]; !ok || v != nil {
			t.Errorf("change_error %v, want null", v)
		}
		looked := w.stage.lookedAt()
		seen := w.stage.reset()
		if len(seen) != 1 {
			t.Fatalf("the stage ran %d times, want once", len(seen))
		}
		in := seen[0]
		if in.Via != pushrule.ViaAPI || in.VersionKind != pushrule.VersionKindCommit || len(in.Updates) != 1 ||
			in.Updates[0].Ref != "refs/heads/work" || in.Updates[0].Before != old || in.Updates[0].After != sha {
			t.Errorf("the stage was given %+v, want via api, kind commit and work from %s to %s", in, old, sha)
		}
		if in.SessionID == nil || *in.SessionID != sessionID || in.Caller.CredentialID != patID {
			t.Errorf("the stage was given session %v and credential %q, want %s and %s", in.SessionID, in.Caller.CredentialID, sessionID, patID)
		}
		if !slices.Equal(looked, []string{old}) {
			t.Errorf("work was at %q while the stage ran, want the old head %s", looked, old)
		}
		if ops := w.linker.asks(); len(ops) != 0 {
			t.Errorf("the change linker was asked %+v, want nothing", ops)
		}
	})

	t.Run("opens the change the request asks for once the branch has moved", func(t *testing.T) {
		old := w.branch(t, "work")
		w.linker.script(nil, &scm.LinkedChange{WorkspaceSlug: "w", Number: 13, Version: 1}, nil,
			func(scm.ChangeOpening) string { return revParse(w.dir, "refs/heads/work") })
		t.Cleanup(func() { w.linker.script(nil, nil, nil, nil) })
		r := post(t, w.ownerKey, "cm", map[string]any{"branch": "work", "expected_head_sha": old, "message": "m",
			"operations": []any{put("c.txt", "c\n")},
			"change":     map[string]any{"target_branch": "main", "title": "T", "intent": "I", "draft": true}})
		sha := created(t, "a commit with a change object", r)
		if c := answerChange(r); fmt.Sprint(c) != fmt.Sprint(map[string]any{"workspace_slug": "w", "number": 13.0, "version": 1.0}) {
			t.Errorf("change %v, want {w 13 1}", r.json["change"])
		}
		ops := w.linker.asks()
		if len(ops) != 2 || ops[0].kind != "check" || ops[1].kind != "open" {
			t.Fatalf("the change linker was asked %+v, want a check, then an opening", ops)
		}
		if ops[0].looked != old || ops[1].looked != sha {
			t.Errorf("work was at %s during the check and %s during the opening, want %s, then %s", ops[0].looked, ops[1].looked, old, sha)
		}
		for _, op := range ops {
			o := op.o
			if o.Workspace != "cm" || o.SourceWorkspace != "cm" || o.SourceBranch != "work" || o.Head != sha ||
				o.TargetBranch != "main" || o.Title != "T" || o.Intent != "I" || !o.Draft || o.Caller.User == nil || o.Caller.User.ID != w.owner {
				t.Errorf("%s of %+v, want work of cm at %s into main, T, I, draft, by the owner", op.kind, o, sha)
			}
		}
	})

	t.Run("refuses the whole commit when the change cannot be opened", func(t *testing.T) {
		t.Cleanup(func() { w.linker.script(nil, nil, nil, nil); w.scm.RegisterChangeLinker(w.linker) })
		for _, tc := range []struct {
			name   string
			err    *server.Error
			linked bool
		}{
			{"without changes:write", server.InsufficientScope("changes:write is required"), true},
			{"a target that does not exist", server.NewError("target_not_found", http.StatusBadRequest, "no branch nope"), true},
			{"nothing to land", server.NewError("nothing_to_land", http.StatusBadRequest, "main holds work"), true},
			{"merge commits", server.NewError("merge_commits_unsupported", http.StatusBadRequest, "work has a merge"), true},
			{"no intent", server.NewError("intent_required", http.StatusBadRequest, "intent is required"), true},
			{"a change on the branch", server.NewError("change_exists", http.StatusConflict, "change 3 uses work"), true},
			{"no change linker", server.InvalidRequest("no change component"), false},
		} {
			old := w.branch(t, "work")
			w.linker.script(tc.err, &scm.LinkedChange{WorkspaceSlug: "w", Number: 14, Version: 1}, nil, nil)
			if !tc.linked {
				w.scm.RegisterChangeLinker(nil)
			}
			seq := w.lastSeq(t)
			r := post(t, w.ownerKey, "cm", map[string]any{"branch": "work", "expected_head_sha": old, "message": "m",
				"operations": []any{put("r.txt", tc.name)}, "change": map[string]any{"target_branch": "main", "title": "T", "intent": "I"}})
			if r.status != tc.err.Status() || r.errorType() != tc.err.Type() {
				t.Errorf("%s: %d %s, want %d %s", tc.name, r.status, r.body, tc.err.Status(), tc.err.Type())
			}
			if got := w.head(t, "work"); got != old {
				t.Errorf("%s: work moved to %s", tc.name, got)
			}
			if types := eventTypes(w.recordsAfter(t, seq)); len(types) != 0 {
				t.Errorf("%s: events %q, want none", tc.name, types)
			}
			ops := w.linker.asks()
			if tc.linked && (len(ops) != 1 || ops[0].kind != "check") {
				t.Errorf("%s: the change linker was asked %+v, want one check and no opening", tc.name, ops)
			}
			w.scm.RegisterChangeLinker(w.linker)
		}
	})

	t.Run("opens a change in the parent for a commit to an auto_change fork", func(t *testing.T) {
		const fork = "demo--t1"
		dir := w.workspaces.RepoPath(fork)
		t.Cleanup(func() {
			w.linker.script(nil, nil, nil, nil)
			w.stage.script(nil, nil)
			w.set(t, fork, "auto_change", "0")
		})
		opened := &scm.LinkedChange{WorkspaceSlug: "demo", Number: 20, Version: 1}
		for i, tc := range []struct {
			name       string
			autoChange string
			open       *scm.LinkedChange
			openErr    error
			version    *pushrule.Version
			change     any
			changeErr  any
			opens      bool
		}{
			{name: "opened", autoChange: "1", open: opened, opens: true,
				change: map[string]any{"workspace_slug": "demo", "number": 20.0, "version": 1.0}},
			{name: "refused for its intent", autoChange: "1", opens: true,
				openErr:   server.NewError("intent_required", http.StatusBadRequest, "intent is required"),
				changeErr: map[string]any{"error_type": "intent_required", "message": "intent is required"}},
			{name: "refused for the token's scope", autoChange: "1", opens: true,
				openErr:   server.InsufficientScope("changes:write is required"),
				changeErr: map[string]any{"error_type": "insufficient_scope", "message": "changes:write is required"}},
			{name: "failed", autoChange: "1", opens: true, openErr: errors.New("the change store is down"),
				changeErr: map[string]any{"error_type": "internal", "message": "the change could not be opened"}},
			{name: "auto_change off", autoChange: "0"},
			{name: "a change on the branch already", autoChange: "1",
				version: &pushrule.Version{WorkspaceSlug: "demo", Number: 21, Version: 2},
				change:  map[string]any{"workspace_slug": "demo", "number": 21.0, "version": 2.0}},
		} {
			branch := "auto-" + strconv.Itoa(i)
			w.set(t, fork, "auto_change", tc.autoChange)
			w.linker.script(nil, tc.open, tc.openErr, nil)
			w.stage.script(nil, nil)
			if tc.version != nil {
				w.stage.script(map[string]*pushrule.Version{"refs/heads/" + branch: tc.version}, nil)
				w.linker.answer(branch, &scm.LinkedChange{WorkspaceSlug: "demo", Number: 21, Version: 2, State: scm.ChangeOpen})
			}
			seq := w.lastSeq(t)
			r := post(t, w.memberKey, fork, map[string]any{"branch": branch, "base_ref": "main", "message": "m",
				"operations": []any{put("auto.txt", tc.name)}})
			sha := created(t, tc.name, r)
			if got := revParse(dir, "refs/heads/"+branch); got != sha {
				t.Errorf("%s: %s is at %q, want %s", tc.name, branch, got, sha)
			}
			if types := eventTypes(w.recordsAfter(t, seq)); !slices.Equal(types, []string{pushrule.EventPush, eventSCMCommit}) {
				t.Errorf("%s: events %q, want a push and a commit", tc.name, types)
			}
			if fmt.Sprint(r.json["change"]) != fmt.Sprint(tc.change) || fmt.Sprint(r.json["change_error"]) != fmt.Sprint(tc.changeErr) {
				t.Errorf("%s: change %v and change_error %v, want %v and %v", tc.name, r.json["change"], r.json["change_error"], tc.change, tc.changeErr)
			}
			for _, k := range []string{"change", "change_error"} {
				if _, ok := r.json[k]; !ok {
					t.Errorf("%s: the answer has no %s", tc.name, k)
				}
			}
			ops := w.linker.asks()
			switch {
			case !tc.opens && len(ops) != 0:
				t.Errorf("%s: the change linker was asked %+v, want nothing", tc.name, ops)
			case tc.opens && (len(ops) != 1 || ops[0].kind != "open"):
				t.Errorf("%s: the change linker was asked %+v, want one opening", tc.name, ops)
			case tc.opens:
				o := ops[0].o
				if o.Workspace != "demo" || o.SourceWorkspace != fork || o.SourceBranch != branch || o.TargetBranch != "main" || o.Head != sha {
					t.Errorf("%s: opening %+v, want %s of %s at %s into main of demo", tc.name, o, branch, fork, sha)
				}
			}
		}
	})

	t.Run("answers a replay from the first answer and creates nothing", func(t *testing.T) {
		old := w.branch(t, "work")
		seq := w.lastSeq(t)
		body := map[string]any{"branch": "work", "expected_head_sha": old, "message": "m", "operations": []any{put("idem.txt", "i\n")}}
		r1 := post(t, w.ownerKey, "cm", body, "Idempotency-Key", "k1")
		created(t, "the first commit", r1)
		r2 := post(t, w.ownerKey, "cm", body, "Idempotency-Key", "k1")
		if r2.status != http.StatusOK || !bytes.Equal(r2.body, r1.body) {
			t.Errorf("the replay: %d %s, want 200 with %s", r2.status, r2.body, r1.body)
		}
		if n := strings.TrimSpace(gitrepotest.Git(t, w.dir, "rev-list", "--count", old+"..work")); n != "1" {
			t.Errorf("work is %s commits ahead of %s, want 1", n, old)
		}
		if types := eventTypes(w.recordsAfter(t, seq)); !slices.Equal(types, []string{pushrule.EventPush, eventSCMCommit}) {
			t.Errorf("events %q, want those of one commit", types)
		}
		body["message"] = "other"
		if r := post(t, w.ownerKey, "cm", body, "Idempotency-Key", "k1"); r.status != http.StatusConflict || r.errorType() != "conflict" {
			t.Errorf("another body under k1: %d %s, want 409 conflict", r.status, r.body)
		}
		stale := map[string]any{"branch": "work", "expected_head_sha": old, "message": "m", "operations": []any{put("idem.txt", "j\n")}}
		if r := post(t, w.ownerKey, "cm", stale, "Idempotency-Key", "k2"); r.status != http.StatusConflict || r.errorType() != pushrule.ErrorTypeRefMoved {
			t.Fatalf("a stale head under k2: %d %s, want 409 ref_moved", r.status, r.body)
		}
		stale["expected_head_sha"] = w.head(t, "work")
		created(t, "the retry of a refused request under k2", post(t, w.ownerKey, "cm", stale, "Idempotency-Key", "k2"))

		seq = w.lastSeq(t)
		ref := map[string]any{"name": "idem-ref", "sha": w.m2}
		r1 = w.sendJSON(t, http.MethodPost, scmPath("cm", "/refs"), w.ownerKey, ref, "Idempotency-Key", "k3")
		r2 = w.sendJSON(t, http.MethodPost, scmPath("cm", "/refs"), w.ownerKey, ref, "Idempotency-Key", "k3")
		if r1.status != http.StatusCreated || r2.status != http.StatusOK || !bytes.Equal(r1.body, r2.body) {
			t.Errorf("a ref creation and its replay: %d %s, then %d %s, want 201, then 200 with the same body", r1.status, r1.body, r2.status, r2.body)
		}
		if _, pushes := w.eventsAfter(t, seq); len(pushes) != 1 {
			t.Errorf("a ref creation and its replay pushed %d times, want once", len(pushes))
		}

		gitrepotest.Git(t, w.dir, "update-ref", "refs/heads/behind", commit(t, w.dir, w.m1, "b.txt", "b\n"))
		seq = w.lastSeq(t)
		rebase := map[string]any{"branch": "behind", "onto": "main"}
		r1 = w.sendJSON(t, http.MethodPost, scmPath("cm", "/rebase"), w.ownerKey, rebase, "Idempotency-Key", "k4")
		r2 = w.sendJSON(t, http.MethodPost, scmPath("cm", "/rebase"), w.ownerKey, rebase, "Idempotency-Key", "k4")
		if r1.status != http.StatusOK || r1.json["status"] != "rebased" || r2.status != http.StatusOK || !bytes.Equal(r1.body, r2.body) {
			t.Errorf("a rebase and its replay: %d %s, then %d %s, want 200 rebased twice with the same body", r1.status, r1.body, r2.status, r2.body)
		}
		if _, pushes := w.eventsAfter(t, seq); len(pushes) != 1 || w.head(t, "behind") != r1.json["new_head_sha"] {
			t.Errorf("a rebase and its replay pushed %d times and left behind at %s, want once, at %v", len(pushes), w.head(t, "behind"), r1.json["new_head_sha"])
		}
	})

	t.Run("leaves the post-push listeners to the ref writer", func(t *testing.T) {
		old := w.branch(t, "work")
		before := len(w.pushes.wait(0))
		rows := []struct {
			name, method, path string
			body               map[string]any
			ref                string
			session            bool
			prepare            func(t *testing.T)
		}{
			{"a commit", http.MethodPost, "/commits", map[string]any{"branch": "work", "expected_head_sha": old, "message": "m",
				"operations": []any{put("l.txt", "l\n")}, "session_id": sessionID}, "refs/heads/work", true, nil},
			{"a ref creation", http.MethodPost, "/refs", map[string]any{"name": "n2", "sha": w.m1}, "refs/heads/n2", false, nil},
			{"a ref move", http.MethodPatch, "/refs/n2", map[string]any{"sha": w.m2, "expected_sha": w.m1}, "refs/heads/n2", false, nil},
			{"a ref delete", http.MethodDelete, "/refs/n2?expected_sha=" + w.m2, nil, "refs/heads/n2", false, nil},
			{"a rebase", http.MethodPost, "/rebase", map[string]any{"branch": "work", "onto": "main", "session_id": sessionID}, "refs/heads/work", true,
				func(t *testing.T) {
					gitrepotest.Git(t, w.dir, "update-ref", "refs/heads/work", commit(t, w.dir, w.m1, "w.txt", "w\n"))
				}},
		}
		for i, row := range rows {
			if row.prepare != nil {
				row.prepare(t)
			}
			r := w.sendJSON(t, row.method, scmPath("cm", row.path), w.ownerKey, row.body)
			if r.status >= 300 {
				t.Fatalf("%s: %d %s, want it accepted", row.name, r.status, r.body)
			}
			seen := w.pushes.wait(before + i + 1)
			if len(seen) != before+i+1 {
				t.Fatalf("%s: the listener was told %d times in all, want %d", row.name, len(seen), before+i+1)
			}
			p := seen[len(seen)-1]
			var want *string
			if row.session {
				want = new(sessionID)
			}
			if p.Workspace != "cm" || len(p.Updates) != 1 || p.Updates[0].Ref != row.ref || p.Via != pushrule.ViaAPI ||
				fmt.Sprint(p.SessionID != nil) != fmt.Sprint(want != nil) || (want != nil && *p.SessionID != *want) {
				t.Errorf("%s: the listener was told %+v, want cm, one update of %s, via api, session %v", row.name, p, row.ref, want)
			}
		}
		r := w.sendJSON(t, http.MethodPost, scmPath("cm", "/commits"), w.ownerKey, map[string]any{"branch": "work",
			"expected_head_sha": old, "message": "m", "operations": []any{put("l.txt", "stale\n")}})
		if r.status != http.StatusConflict || r.errorType() != pushrule.ErrorTypeRefMoved {
			t.Fatalf("a commit on a stale head: %d %s, want 409 ref_moved", r.status, r.body)
		}
		time.Sleep(100 * time.Millisecond)
		if seen := w.pushes.wait(0); len(seen) != before+len(rows) {
			t.Errorf("after a refused commit the listener was told %d times in all, want %d", len(seen), before+len(rows))
		}
	})
}
