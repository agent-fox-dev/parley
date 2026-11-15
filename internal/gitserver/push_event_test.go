package gitserver_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/agent-fox-dev/parley/internal/gitrepo/gitrepotest"
	"github.com/agent-fox-dev/parley/internal/gitserver/pushrule"
	"github.com/agent-fox-dev/parley/internal/hubtest"
)

// pushEvent is a parley.git.push event on one line: the workspace, the
// actor, the ref, the session and the metadata, an absent value as null.
func pushEvent(ws, actorType, actorID, ref, session, meta string) string {
	return strings.Join([]string{"parley.git.push", ws, actorType, actorID, "ref", ref, "push", session, meta}, " ")
}

// pushMeta is the metadata of the event of c through via, with the flags
// in flags ("created", "deleted", "forced", "direct") set.
func pushMeta(c command, via string, flags ...string) string {
	id := func(s string) string {
		if s == zero {
			return "null"
		}
		return `"` + s + `"`
	}
	return fmt.Sprintf(`{"ref":"%s","before":%s,"after":%s,"created":%t,"deleted":%t,"forced":%t,"direct":%t,"via":"%s"}`,
		c.ref, id(c.old), id(c.new), slices.Contains(flags, "created"), slices.Contains(flags, "deleted"),
		slices.Contains(flags, "forced"), slices.Contains(flags, "direct"), via)
}

// pushEventLine is rec as pushEvent writes it.
func pushEventLine(typ string, ws, actorID *string, actorType, resType, ref, action string, session *string, meta []byte) string {
	null := func(s *string) string {
		if s == nil {
			return "null"
		}
		return *s
	}
	return strings.Join([]string{typ, null(ws), actorType, null(actorID), resType, ref, action, null(session), string(meta)}, " ")
}

// eventsAfter are the events of the hub's log after seq, each as pushEvent
// writes it.
func (w *world) eventsAfter(t *testing.T, seq int64) []string {
	t.Helper()
	recs, _, err := w.events.ReadAfter(context.Background(), seq, 1000)
	if err != nil {
		t.Fatal(err)
	}
	lines := []string{}
	for _, r := range recs {
		lines = append(lines, pushEventLine(r.Type, r.Workspace, r.ActorID, r.ActorType, r.ResourceType, r.ResourceID, r.Action, r.SessionID, r.Metadata))
	}
	return lines
}

func TestPushEvent_RecordsEveryAcceptedRefUpdate(t *testing.T) {
	t.Parallel()
	w, s := newStagedWorld(t)
	w.addFork(t)
	pat, _ := w.pat(t, w.owner, "", "git:write")
	session := "3a6f1d2c-5b4e-4f7a-8c9d-0e1f2a3b4c5d"
	c1 := w.commit(t, w.head, "c1")
	c2 := w.commit(t, c1, "c2")
	c3 := w.commit(t, w.head, "c3")
	create, ff, forced := command{zero, c1, "refs/heads/feat"}, command{c1, c2, "refs/heads/feat"}, command{c2, c3, "refs/heads/feat"}
	del, direct := command{c3, zero, "refs/heads/feat"}, command{w.head, w.commit(t, w.head, "next"), "refs/heads/main"}
	tag := command{zero, c1, "refs/tags/v1"}
	a, b, c := command{zero, w.commit(t, w.head, "a"), "refs/heads/a"}, command{zero, w.commit(t, w.head, "b"), "refs/heads/b"}, command{zero, w.commit(t, w.head, "c"), "refs/heads/c"}
	fk, sess := command{zero, w.commit(t, w.head, "fk"), "refs/heads/fk"}, command{zero, w.commit(t, w.head, "s"), "refs/heads/s"}
	for _, tc := range []struct {
		name, repo, credential string
		session                *string
		cmds                   []command
		want                   []string
	}{
		{"a PAT creates a branch", "acme/demo.git", pat, nil, []command{create},
			[]string{pushEvent("demo", "pat", w.owner, create.ref, "null", pushMeta(create, "git", "created"))}},
		{"a PAT fast-forwards it", "acme/demo.git", pat, nil, []command{ff},
			[]string{pushEvent("demo", "pat", w.owner, ff.ref, "null", pushMeta(ff, "git"))}},
		{"a PAT force-pushes it", "acme/demo.git", pat, nil, []command{forced},
			[]string{pushEvent("demo", "pat", w.owner, forced.ref, "null", pushMeta(forced, "git", "forced"))}},
		{"an API key deletes it", "acme/demo.git", w.ownerKey, nil, []command{del},
			[]string{pushEvent("demo", "api_key", w.owner, del.ref, "null", pushMeta(del, "git", "deleted"))}},
		{"the owner's API key fast-forwards protected main", "acme/demo.git", w.ownerKey, nil, []command{direct},
			[]string{pushEvent("demo", "api_key", w.owner, direct.ref, "null", pushMeta(direct, "git", "direct"))}},
		{"the admin token creates a tag", "acme/demo.git", w.Token, nil, []command{tag},
			[]string{pushEvent("demo", "admin_token", "null", tag.ref, "null", pushMeta(tag, "git", "created"))}},
		{"three commands in one request", "acme/demo.git", w.ownerKey, nil, []command{a, b, c}, []string{
			pushEvent("demo", "api_key", w.owner, a.ref, "null", pushMeta(a, "git", "created")),
			pushEvent("demo", "api_key", w.owner, b.ref, "null", pushMeta(b, "git", "created")),
			pushEvent("demo", "api_key", w.owner, c.ref, "null", pushMeta(c, "git", "created")),
		}},
		{"a push to a fork", "acme/demo--t1.git", w.memberKey, nil, []command{fk},
			[]string{pushEvent("demo--t1", "api_key", w.member, fk.ref, "null", pushMeta(fk, "git", "created"))}},
		{"a push the options stage set a session for", "acme/demo.git", w.ownerKey, &session, []command{sess},
			[]string{pushEvent("demo", "api_key", w.owner, sess.ref, session, pushMeta(sess, "git", "created"))}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s.mu.Lock()
			s.session = tc.session
			s.mu.Unlock()
			seq := w.lastSeq()
			report := parseReport(w.receive(t, tc.repo, tc.credential, receiveRequest(fullCaps, tc.cmds, nil, w.packFor(t, tc.cmds))).body).lines
			if report[0] != "unpack ok" || slices.ContainsFunc(report[1:], func(l string) bool { return !strings.HasPrefix(l, "ok ") }) {
				t.Fatalf("report = %q, want every ref accepted", report)
			}
			if got := w.eventsAfter(t, seq); !slices.Equal(got, tc.want) {
				t.Errorf("events\n%q\nwant\n%q", got, tc.want)
			}
		})
	}
}

// The events of one request are written together, so concurrent pushes to
// one workspace never interleave theirs.
func TestPushEvent_WritesTheEventsOfOneRequestInAdjacentPositions(t *testing.T) {
	t.Parallel()
	w := newGitWorld(t, hubtest.Config{})
	const n = 6
	bodies := make([]string, n)
	for i := range n {
		var cmds []command
		for _, name := range []string{"a", "b", "c"} {
			ref := fmt.Sprintf("refs/heads/g%d/%s", i, name)
			cmds = append(cmds, command{zero, w.commit(t, w.head, ref), ref})
		}
		bodies[i] = receiveRequest(fullCaps, cmds, nil, w.packFor(t, cmds))
	}
	seq := w.lastSeq()
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			<-start
			a, err := w.receiveErr("acme/demo.git", w.ownerKey, bodies[i])
			if lines := parseReport(a.body).lines; err != nil || len(lines) != 4 || lines[3] != fmt.Sprintf("ok refs/heads/g%d/c", i) {
				t.Errorf("push %d answered %q, %v", i, lines, err)
			}
		})
	}
	close(start)
	wg.Wait()
	recs, _, err := w.events.ReadAfter(context.Background(), seq, 1000)
	if err != nil {
		t.Fatal(err)
	}
	var refs []string
	for _, r := range recs {
		if r.Type == pushrule.EventPush && r.Workspace != nil && *r.Workspace == "demo" {
			refs = append(refs, r.ResourceID)
		}
	}
	if len(refs) != 3*n {
		t.Fatalf("events of %v, want %d", refs, 3*n)
	}
	for k := 0; k < len(refs); k += 3 {
		prefix, _ := strings.CutSuffix(refs[k], "/a")
		if want := []string{prefix + "/a", prefix + "/b", prefix + "/c"}; !slices.Equal(refs[k:k+3], want) {
			t.Errorf("events %d to %d are of %v, want %v", k, k+2, refs[k:k+3], want)
		}
	}
}

// A hub that stopped after the refs of a request moved and before its
// events were written writes them as it starts, before its first request.
func TestPushEvent_WritesTheEventsTheJournalOwesBeforeTheRestartedHubServes(t *testing.T) {
	t.Parallel()
	w := newGitWorld(t, hubtest.Config{})
	ctx := context.Background()
	owed := command{zero, w.head, "refs/heads/owed"}
	gitrepotest.Git(t, w.repo, "update-ref", owed.ref, owed.new)
	if err := w.Store.Tx(ctx, func(tx *sql.Tx) error {
		for _, ref := range []string{owed.ref, "refs/heads/never"} {
			e := pushrule.JournalEntry{Workspace: "demo", ActorType: "api_key", ActorID: w.owner, Via: "git",
				Updates: []pushrule.JournalUpdate{{Ref: ref, Before: zero, After: w.head, Created: true}}}
			if _, err := pushrule.InsertJournalEntry(ctx, tx, e); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	w.Restart(t)
	status, body := w.Call(t, http.MethodGet, "/api/v1/events/log", w.Token, nil)
	var page struct {
		Items []json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(body, &page); status != http.StatusOK || err != nil {
		t.Fatalf("GET /api/v1/events/log = %d %s, %v", status, body, err)
	}
	var got []string
	for _, raw := range page.Items {
		var r struct {
			Type         string          `json:"type"`
			Workspace    *string         `json:"workspace"`
			ActorType    string          `json:"actor_type"`
			ActorID      *string         `json:"actor_id"`
			ResourceType string          `json:"resource_type"`
			ResourceID   string          `json:"resource_id"`
			Action       string          `json:"action"`
			SessionID    *string         `json:"session_id"`
			Metadata     json.RawMessage `json:"metadata"`
		}
		if err := json.Unmarshal(raw, &r); err != nil {
			t.Fatal(err)
		}
		if r.Type == pushrule.EventPush {
			got = append(got, pushEventLine(r.Type, r.Workspace, r.ActorID, r.ActorType, r.ResourceType, r.ResourceID, r.Action, r.SessionID, r.Metadata))
		}
	}
	if want := []string{pushEvent("demo", "api_key", w.owner, owed.ref, "null", pushMeta(owed, "git", "created"))}; !slices.Equal(got, want) {
		t.Errorf("the first request read the push events\n%q\nwant\n%q", got, want)
	}
	if entries, err := pushrule.JournalEntries(ctx, w.Store.Main()); err != nil || len(entries) != 0 {
		t.Errorf("journal = %+v, %v, want it empty", entries, err)
	}
}
