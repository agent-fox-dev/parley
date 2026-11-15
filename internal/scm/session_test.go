package scm_test

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/agent-fox-dev/parley/internal/gitrepo/gitrepotest"
	"github.com/agent-fox-dev/parley/internal/identity"
	"github.com/agent-fox-dev/parley/internal/scm"
)

// TestSessionCheck checks how the commit and rebase routes judge the
// session_id a request names, on one workspace: the branch work at main's
// head for the commits, behind one commit on top of main's parent for the
// rebases.
func TestSessionCheck(t *testing.T) {
	t.Parallel()
	w := newCommitWorld(t, "")
	pat, patID := w.IssuePATID(t, identity.PATSpec{UserID: w.owner, Permissions: []string{"git:write"}})
	behind := commit(t, w.dir, w.m1, "b.txt", "b\n")
	type route struct {
		name, path, branch string
		body               func(session string) map[string]any
	}
	routes := []route{
		{"a commit", "/commits", "work", func(session string) map[string]any {
			return map[string]any{"branch": "work", "expected_head_sha": w.m2, "message": "m",
				"operations": []any{put("s.txt", "s\n")}, "session_id": session}
		}},
		{"a rebase", "/rebase", "behind", func(session string) map[string]any {
			return map[string]any{"branch": "behind", "onto": "main", "expected_head_sha": behind, "session_id": session}
		}},
	}
	reset := func(t *testing.T) {
		t.Helper()
		gitrepotest.Git(t, w.dir, "update-ref", "refs/heads/work", w.m2)
		gitrepotest.Git(t, w.dir, "update-ref", "refs/heads/behind", behind)
	}
	send := func(t *testing.T, rt route, body map[string]any) reply {
		t.Helper()
		return w.sendJSON(t, http.MethodPost, scmPath("cm", rt.path), pat, body)
	}
	t.Cleanup(func() { w.sessions.refuse(nil); w.scm.RegisterSessionLinker(w.sessions) })

	t.Run("refuses a session the linker does not accept and writes nothing", func(t *testing.T) {
		for _, linker := range []struct {
			name      string
			err       error
			absent    bool
			status    int
			errorType string
		}{
			{name: "a linker that refuses it", err: scm.ErrUnknownSession, status: http.StatusBadRequest, errorType: "unknown_session"},
			{name: "a linker that refuses it, wrapped", err: errors.Join(errors.New("no such session"), scm.ErrUnknownSession),
				status: http.StatusBadRequest, errorType: "unknown_session"},
			{name: "no linker", absent: true, status: http.StatusBadRequest, errorType: "unknown_session"},
			{name: "a linker that cannot tell", err: errors.New("the session store is down"), status: http.StatusInternalServerError, errorType: "internal"},
		} {
			w.sessions.refuse(linker.err)
			if linker.absent {
				w.scm.RegisterSessionLinker(nil)
			}
			for _, rt := range routes {
				reset(t)
				seq := w.lastSeq(t)
				r := send(t, rt, rt.body(sessionID))
				if r.status != linker.status || r.errorType() != linker.errorType {
					t.Errorf("%s with %s: %d %s, want %d %s", rt.name, linker.name, r.status, r.body, linker.status, linker.errorType)
				}
				if got := w.head(t, rt.branch); got != map[string]string{"work": w.m2, "behind": behind}[rt.branch] {
					t.Errorf("%s with %s: %s moved to %s", rt.name, linker.name, rt.branch, got)
				}
				if types := eventTypes(w.recordsAfter(t, seq)); len(types) != 0 {
					t.Errorf("%s with %s: events %q, want none", rt.name, linker.name, types)
				}
				if seen := w.stage.reset(); len(seen) != 0 {
					t.Errorf("%s with %s: the ref writer wrote %d requests, want none", rt.name, linker.name, len(seen))
				}
			}
			w.scm.RegisterSessionLinker(w.sessions)
		}
	})

	t.Run("checks the session after the body and before the branch", func(t *testing.T) {
		w.sessions.refuse(scm.ErrUnknownSession)
		reset(t)
		malformed := routes[0].body(sessionID)
		delete(malformed, "message")
		if r := send(t, routes[0], malformed); r.status != http.StatusBadRequest || r.errorType() != "invalid_request" {
			t.Errorf("a malformed commit naming a session: %d %s, want 400 invalid_request", r.status, r.body)
		}
		for _, rt := range routes {
			stale := rt.body(sessionID)
			stale["expected_head_sha"] = w.m1
			if r := send(t, rt, stale); r.status != http.StatusBadRequest || r.errorType() != "unknown_session" {
				t.Errorf("%s on a stale head naming an unknown session: %d %s, want 400 unknown_session", rt.name, r.status, r.body)
			}
		}
	})

	t.Run("carries an accepted session onto the events, the answer and the ref writer", func(t *testing.T) {
		w.sessions.refuse(nil)
		for _, rt := range routes {
			reset(t)
			seq := w.lastSeq(t)
			r := send(t, rt, rt.body(strings.ToUpper(sessionID)))
			if r.status != http.StatusCreated && r.status != http.StatusOK {
				t.Fatalf("%s with an accepted session: %d %s, want it accepted", rt.name, r.status, r.body)
			}
			checks := w.sessions.checks()
			if len(checks) != 1 || checks[0].session != strings.ToUpper(sessionID) || checks[0].slug != "cm" || checks[0].caller.CredentialID != patID {
				t.Errorf("%s: the session linker was asked %+v, want %s on cm by %s", rt.name, checks, strings.ToUpper(sessionID), patID)
			}
			recs := w.recordsAfter(t, seq)
			if len(recs) == 0 {
				t.Errorf("%s: no event", rt.name)
			}
			for _, rec := range recs {
				if rec.SessionID == nil || *rec.SessionID != sessionID {
					t.Errorf("%s: %s has session %v, want %s", rt.name, rec.Type, rec.SessionID, sessionID)
				}
			}
			if seen := w.stage.reset(); len(seen) != 1 || seen[0].SessionID == nil || *seen[0].SessionID != sessionID {
				t.Errorf("%s: the ref writer was given %+v, want one request of session %s", rt.name, seen, sessionID)
			}
			if rt.path == "/commits" && answerCommit(r)["session_id"] != sessionID {
				t.Errorf("%s: commit.session_id %v, want %s", rt.name, answerCommit(r)["session_id"], sessionID)
			}
		}
		reset(t)
		body := routes[0].body("")
		delete(body, "session_id")
		if r := send(t, routes[0], body); r.status != http.StatusCreated {
			t.Fatalf("a commit naming no session: %d %s", r.status, r.body)
		}
		if checks := w.sessions.checks(); len(checks) != 0 {
			t.Errorf("a commit naming no session asked the session linker %+v", checks)
		}
	})
}
